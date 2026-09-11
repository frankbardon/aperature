package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/delegation"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/impersonation"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// shutdownTimeout bounds how long serve waits for in-flight requests to drain on
// SIGINT/SIGTERM before forcing the listener closed.
const shutdownTimeout = 10 * time.Second

// serveCommand is `aperture serve`: it hand-wires the dependency graph
// (storage -> engine -> service -> HTTP server), boots a net/http server, and
// shuts it down gracefully on SIGINT/SIGTERM. This is the manual constructor-DI
// pattern Aperture mirrors from orbit's serve command — no DI framework.
func serveCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "serve",
		Usage: "Run the Aperture HTTP server",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:  "addr",
				Usage: "TCP address to listen on",
				Value: ":8080",
			},
			&ucli.StringFlag{
				Name:  "seed",
				Usage: "path to a JSON/YAML seed model (defaults to the embedded example)",
			},
			&ucli.StringFlag{
				Name:  "store",
				Usage: "DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path",
			},
			&ucli.StringFlag{
				Name:    "auth",
				Usage:   "authenticator adapter: dev|oidc|parsec (overrides APERTURE_AUTH_MODE; defaults to dev — bearer is the principal id, no external IdP)",
				Sources: ucli.EnvVars(auth.EnvMode),
			},
			&ucli.BoolFlag{
				Name:    "enforce-membership",
				Usage:   "deny any decision whose principal is not a member of the active account, before grants are consulted (defence-in-depth; lets shared roles be reused across accounts safely)",
				Sources: ucli.EnvVars("APERTURE_ENFORCE_MEMBERSHIP"),
			},
			// Not a serve-only knob, and deliberately not declared here: the bound
			// governs the process, so it is the SAME flag `check` / `enumerate` /
			// `identifiers` / `explain` / `mcp` carry, and buildDecisionStack — not
			// serveEngineOptions — is what applies it. See enumerate_limit.go.
			enumerateLimitFlag(),
			&ucli.BoolFlag{
				Name:  "manage-accounts",
				Value: true,
				Usage: "manage the lifecycle of account records — allow account create/update/delete through the API (default true; overrides " + service.EnvManageAccounts + "). Pass --manage-accounts=false when accounts are mastered by an upstream system: Aperture then refuses every account write regardless of the caller's authority, while account reads and every decision stay unaffected. Read once at startup; a restart is required to change it",
			},
			&ucli.BoolFlag{
				Name:  "manage-principals",
				Value: true,
				Usage: "manage the lifecycle of principal records — allow principal create/update/delete through the API (default true; overrides " + service.EnvManagePrincipals + "). Pass --manage-principals=false when principals are mastered by an upstream directory or IdP: Aperture then refuses every principal write regardless of the caller's authority, while principal reads and every decision stay unaffected. Read once at startup; a restart is required to change it",
			},
			&ucli.BoolFlag{
				Name:  "manage-memberships",
				Value: true,
				Usage: "manage the lifecycle of principal-to-account memberships — allow membership create/update/delete through the API (default true; overrides " + service.EnvManageMemberships + "). Independent of the other two, so a deployment can master accounts and principals upstream and still decide who belongs to what, or the reverse. Read once at startup; a restart is required to change it",
			},
		},
		Action: runServe,
	}
}

// managedEntities resolves which entity kinds this process manages, in
// precedence order: the documented default (every kind managed), overridden by
// the APERTURE_MANAGE_* environment variables, overridden by a --manage-* flag
// the operator actually passed.
//
// The flags deliberately do NOT carry ucli.EnvVars sources even though their
// usage text names the matching variable. urfave/cli parses a boolean source
// itself and fails the command with its own uncoded "parse error" before the
// action ever runs, which would make a typo'd APERTURE_MANAGE_ACCOUNTS report
// something other than APERTURE_CONFIG_INVALID. Reading the environment through
// service.ManagedEntitiesFromEnv keeps one coded error for a bad value, and
// keeps cmd.IsSet meaning "the operator typed this flag" so flag-over-env
// precedence is unambiguous. Both paths accept strconv.ParseBool's spellings, so
// the two can never disagree about a value they both accept.
//
// --enumerate-limit makes the opposite trade against the same hazard: it KEEPS
// its EnvVars source, because a string source has no parse for urfave to fail,
// and does the number parsing itself. See enumerateLimit in enumerate_limit.go.
func managedEntities(cmd *ucli.Command) (service.ManagedEntities, error) {
	managed, err := service.ManagedEntitiesFromEnv()
	if err != nil {
		return service.ManagedEntities{}, err
	}
	for _, f := range []struct {
		flag  string
		field *service.Managed
	}{
		{"manage-accounts", &managed.Accounts},
		{"manage-principals", &managed.Principals},
		{"manage-memberships", &managed.Memberships},
	} {
		if cmd.IsSet(f.flag) {
			*f.field = service.ManagedFrom(cmd.Bool(f.flag))
		}
	}
	return managed, nil
}

// serveEngineOptions turns the SERVE-ONLY flags that configure the decision
// engine into the options buildDecisionStack layers on top of the shared wiring.
// It is pure flag reading with no I/O, which is why runServe calls it before
// anything is opened, and it is a function rather than inline code so a test can
// drive the real flag set and assert against the engine the options actually
// produce.
//
// "Serve-only" is the whole test for belonging here. Membership enforcement is a
// posture the server takes and the one-shot commands deliberately do not; the
// enumeration bound is the opposite — it describes the deployment, so it lives
// in sharedEngineOptions where every command inherits it, and wiring it here
// would have left `aperture enumerate` answering 1000 while the server answered
// 1500.
func serveEngineOptions(cmd *ucli.Command) ([]engine.Option, error) {
	var opts []engine.Option
	if cmd.Bool("enforce-membership") {
		// Defence-in-depth, and serve-specific: a non-member of the active account
		// is denied before any grant is read, which is what lets a single shared
		// role (manager, analyst, ...) be reused across customer accounts without
		// one customer's account-scoped grants leaking to another customer's
		// members.
		opts = append(opts, engine.WithMembershipEnforcement())
	}
	return opts, nil
}

func runServe(ctx context.Context, cmd *ucli.Command) error {
	// Resolve the deployment's entity-management posture FIRST, before anything is
	// opened or created: a malformed APERTURE_MANAGE_* value must fail the boot
	// outright rather than after a store file has been written. It is read exactly
	// once, here — which entities Aperture owns is a property of the deployment,
	// not of a request, so nothing downstream re-reads or mutates it.
	managed, err := managedEntities(cmd)
	if err != nil {
		return err
	}

	// The engine's own configuration is read in the same breath and for the same
	// reason: a malformed value must fail the boot before a store is opened, not
	// once requests are already answered.
	engOpts, err := serveEngineOptions(cmd)
	if err != nil {
		return err
	}

	// The SHARED configuration is read here too, and the result is thrown away.
	// buildDecisionStack is what applies it — the enumeration bound governs every
	// command that decides, so it cannot be wired on serve — but that runs after
	// the store has been opened and seeded. A malformed --enumerate-limit /
	// APERTURE_ENUMERATE_LIMIT must fail the boot before a store file is written,
	// so serve pays for one extra parse of a string it already holds rather than
	// leaving a database behind on a refused configuration.
	if _, err := sharedEngineOptions(cmd); err != nil {
		return err
	}

	// Construct the dependency graph by hand: storage -> engine -> service ->
	// HTTP handler. Each layer is a plain constructor; there is no container.
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Construct the authenticator from configuration (env + the --auth flag), then
	// apply it as request middleware so HTTP requests resolve to an Aperture
	// principal. The default adapter is dev/static (bearer == principal id), so
	// Aperture runs with NO external IdP; oidc and parsec are opt-in via config.
	authCfg := auth.ConfigFromEnv()
	if mode := cmd.String("auth"); mode != "" {
		authCfg.Mode = auth.Mode(mode)
	}
	authn, err := authCfg.Build(ctx)
	if err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: building the authenticator failed", err)
	}

	// Build the shared decision stack — object providers, the rules engine over a
	// storage-backed rule source, and scope resolution — through the SAME builder
	// `check` / `enumerate` / `identifiers` / `explain` use, so no surface can
	// answer a question differently from another (see decision.go). The
	// serve-specific engine options resolved above are layered on last.
	stack, err := buildDecisionStack(cmd, store, cmd.String("seed"), engOpts...)
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()
	stack.reportCollisions(cmd.ErrWriter)

	// Wire the append-only audit trail (E4-S2) through the same store so the
	// mutation/impersonation/delegation record is durable and the E6-S4 audit
	// viewer has data to query. Mutations are always recorded; decisions are
	// sampled — sample every decision here so the demo trail is legible. The
	// recorder owns a background writer that Close flushes on shutdown.
	rec := audit.New(store, audit.WithSampleRate(1))
	defer func() { _ = rec.Close() }()

	// Build the fully-wired facade so HTTP, Twirp, and CLI drive ONE mutation
	// path: the engine for decisions + authority, the admin gate for tier checks,
	// and the delegation / impersonation services for their own gated mutations.
	// These are the serve-only extras layered on top of the shared stack; the
	// rule source is handed over as well so the editor's live what-if can preview
	// an UNSAVED rule read-only (E7-S3).
	eng := stack.eng
	svc := stack.newService(
		service.WithStorage(store),
		service.WithGate(authz.NewGate(eng)),
		service.WithDelegation(delegation.New(store, eng)),
		service.WithImpersonation(impersonation.New(store, eng)),
		service.WithAudit(rec),
		service.WithRuleSource(stack.ruleSource, stack.fetcher),
		service.WithManagedEntities(managed),
	)

	handler := server.Authenticate(authn, server.New(svc))

	addr := cmd.String("addr")
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Trip ctx on the first SIGINT/SIGTERM so the select below can begin a
	// graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(cmd.Writer, "aperture serving on %s\n", addr)
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return aerr.Wrap(aerr.APERTURE_BOOT, "cli: http server failed", err)
		}
		return nil
	case <-ctx.Done():
		fmt.Fprintln(cmd.Writer, "shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return aerr.Wrap(aerr.APERTURE_BOOT, "cli: graceful shutdown failed", err)
		}
		return nil
	}
}
