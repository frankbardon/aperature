package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// resolveManaged runs the real `serve` flag set over args and returns what
// managedEntities makes of it, without booting a server.
func resolveManaged(t *testing.T, args ...string) (service.ManagedEntities, error) {
	t.Helper()
	var (
		got service.ManagedEntities
		err error
	)
	cmd := &ucli.Command{
		Name:  "serve",
		Flags: serveCommand().Flags,
		Action: func(_ context.Context, cmd *ucli.Command) error {
			got, err = managedEntities(cmd)
			return nil
		},
	}
	if runErr := cmd.Run(context.Background(), append([]string{"serve"}, args...)); runErr != nil {
		t.Fatalf("parsing %v: %v", args, runErr)
	}
	return got, err
}

// TestManagedEntities_DefaultsToManaged asserts an operator who passes nothing
// and sets nothing gets Aperture's historical behaviour.
func TestManagedEntities_DefaultsToManaged(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	got, err := resolveManaged(t)
	if err != nil {
		t.Fatalf("managedEntities: %v", err)
	}
	if !got.Accounts.Enabled() || !got.Principals.Enabled() || !got.Memberships.Enabled() {
		t.Fatalf("unconfigured serve is not fully managed: %+v", got)
	}
}

// TestManagedEntities_FlagsAreBooleansOfPositivePolarity asserts --manage-X=false
// disables exactly X. Positive polarity: the flag names the ENABLED state.
func TestManagedEntities_FlagsAreBooleansOfPositivePolarity(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	for _, tc := range []struct {
		flag string
		read func(service.ManagedEntities) service.Managed
	}{
		{"--manage-accounts=false", func(m service.ManagedEntities) service.Managed { return m.Accounts }},
		{"--manage-principals=false", func(m service.ManagedEntities) service.Managed { return m.Principals }},
		{"--manage-memberships=false", func(m service.ManagedEntities) service.Managed { return m.Memberships }},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got, err := resolveManaged(t, tc.flag)
			if err != nil {
				t.Fatalf("managedEntities: %v", err)
			}
			if tc.read(got).Enabled() {
				t.Fatalf("%s left the kind managed: %+v", tc.flag, got)
			}
		})
	}
}

// TestManagedEntities_EnvIsRead asserts the env var alone (no flag) is enough.
func TestManagedEntities_EnvIsRead(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "false")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	got, err := resolveManaged(t)
	if err != nil {
		t.Fatalf("managedEntities: %v", err)
	}
	if got.Accounts.Enabled() {
		t.Fatalf("%s=false was not read: %+v", service.EnvManageAccounts, got)
	}
	if !got.Principals.Enabled() || !got.Memberships.Enabled() {
		t.Fatalf("one env var disabled more than its own kind: %+v", got)
	}
}

// TestManagedEntities_FlagBeatsEnv pins the precedence in both directions, so
// the flag is a real override and not merely a second way to say the same thing.
func TestManagedEntities_FlagBeatsEnv(t *testing.T) {
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	t.Run("flag disables what env enabled", func(t *testing.T) {
		t.Setenv(service.EnvManageAccounts, "true")
		got, err := resolveManaged(t, "--manage-accounts=false")
		if err != nil {
			t.Fatalf("managedEntities: %v", err)
		}
		if got.Accounts.Enabled() {
			t.Fatalf("the flag did not override the env var: %+v", got)
		}
	})

	t.Run("flag enables what env disabled", func(t *testing.T) {
		t.Setenv(service.EnvManageAccounts, "false")
		got, err := resolveManaged(t, "--manage-accounts=true")
		if err != nil {
			t.Fatalf("managedEntities: %v", err)
		}
		if !got.Accounts.Enabled() {
			t.Fatalf("the flag did not override the env var: %+v", got)
		}
	})
}

// TestManagedEntities_MalformedEnvIsConfigError asserts the coded error survives
// the CLI layer. This is the reason the flags carry no ucli.EnvVars source:
// urfave would fail the command with its own uncoded parse error first.
func TestManagedEntities_MalformedEnvIsConfigError(t *testing.T) {
	t.Setenv(service.EnvManageAccounts, "yes-please")
	t.Setenv(service.EnvManagePrincipals, "")
	t.Setenv(service.EnvManageMemberships, "")

	_, err := resolveManaged(t)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
}

// resolveEngineOptions runs the real `serve` flag set over args and returns the
// engine options serveEngineOptions makes of it, without booting a server. It is
// the SAME function runServe calls, so a test driving it cannot pass against a
// serve command that never wired the flag.
func resolveEngineOptions(t *testing.T, args ...string) ([]engine.Option, error) {
	t.Helper()
	var (
		opts []engine.Option
		err  error
	)
	cmd := &ucli.Command{
		Name:  "serve",
		Flags: serveCommand().Flags,
		Action: func(_ context.Context, cmd *ucli.Command) error {
			opts, err = serveEngineOptions(cmd)
			return nil
		},
	}
	if runErr := cmd.Run(context.Background(), append([]string{"serve"}, args...)); runErr != nil {
		t.Fatalf("parsing %v: %v", args, runErr)
	}
	return opts, err
}

// enumerateUnderServeFlags is the enumeration `serve` would answer, built from the engine
// options the given serve arguments resolve to. The fixture holds exactly three
// documents and alice may list all of them, so the returned count IS the bound
// whenever the bound is below three — which is what makes the configured value
// observable rather than merely stored.
func enumerateUnderServeFlags(t *testing.T, args ...string) []string {
	t.Helper()
	opts, err := resolveEngineOptions(t, args...)
	if err != nil {
		t.Fatalf("serveEngineOptions: %v", err)
	}

	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)
	store, err := buildStore(ctx, "", seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stack, err := buildDecisionStack(store, seedPath, opts...)
	if err != nil {
		t.Fatalf("buildDecisionStack: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close() })

	ids, err := stack.eng.Enumerate(ctx, engine.EnumerateRequest{
		Account:   "acme",
		Principal: "alice",
		Action:    "list",
		Pattern:   "account:acme/**",
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// TestEnumerateLimit_UnconfiguredIsTheEngineDefault asserts an operator who
// passes nothing and sets nothing adds NO option, which is what leaves the
// engine on DefaultEnumerateLimit and serve answering exactly as it did before
// the flag existed. Asserting the absence of the option is the precise claim:
// with a three-object fixture, a bound of 1000 and a bound of 1_000_000 look
// identical.
func TestEnumerateLimit_UnconfiguredIsTheEngineDefault(t *testing.T) {
	t.Setenv(envEnumerateLimit, "")

	opts, err := resolveEngineOptions(t)
	if err != nil {
		t.Fatalf("serveEngineOptions: %v", err)
	}
	if len(opts) != 0 {
		t.Fatalf("an unconfigured serve carries %d engine options, want none", len(opts))
	}
	if got := enumerateUnderServeFlags(t); len(got) != 3 {
		t.Fatalf("unconfigured enumerate returned %v, want all three documents", got)
	}
}

// TestEnumerateLimit_EnvIsRead asserts the env var alone (no flag) reaches the
// engine — the whole point of carrying a ucli.EnvVars source.
func TestEnumerateLimit_EnvIsRead(t *testing.T) {
	t.Setenv(envEnumerateLimit, "2")

	if got := enumerateUnderServeFlags(t); len(got) != 2 {
		t.Fatalf("%s=2 returned %v, want two ids", envEnumerateLimit, got)
	}
}

// TestEnumerateLimit_FlagBeatsEnv pins urfave's native flag > env > default
// precedence in BOTH directions, so the flag is a real override rather than a
// second way to say the same number.
func TestEnumerateLimit_FlagBeatsEnv(t *testing.T) {
	t.Run("flag raises what env lowered", func(t *testing.T) {
		t.Setenv(envEnumerateLimit, "1")
		if got := enumerateUnderServeFlags(t, "--enumerate-limit=2"); len(got) != 2 {
			t.Fatalf("the flag did not override the env var: %v", got)
		}
	})

	t.Run("flag lowers what env raised", func(t *testing.T) {
		t.Setenv(envEnumerateLimit, "2")
		if got := enumerateUnderServeFlags(t, "--enumerate-limit=1"); len(got) != 1 {
			t.Fatalf("the flag did not override the env var: %v", got)
		}
	})
}

// TestEnumerateLimit_MalformedIsConfigError asserts a value that is not a number
// is an Aperture-coded error from BOTH sources. This is what the StringFlag
// buys: a ucli.IntFlag carrying the same EnvVars source would have failed the
// command with urfave's own uncoded parse error before the action ran.
func TestEnumerateLimit_MalformedIsConfigError(t *testing.T) {
	t.Run("from the environment", func(t *testing.T) {
		t.Setenv(envEnumerateLimit, "banana")
		_, err := resolveEngineOptions(t)
		if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
			t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
		}
		if !strings.Contains(err.Error(), "banana") || !strings.Contains(err.Error(), envEnumerateLimit) {
			t.Fatalf("the error names neither the setting nor the value: %v", err)
		}
	})

	t.Run("from the flag", func(t *testing.T) {
		t.Setenv(envEnumerateLimit, "")
		_, err := resolveEngineOptions(t, "--enumerate-limit=banana")
		if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
			t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
		}
	})
}

// TestEnumerateLimit_NonPositiveIsConfigError asserts a value that parses but
// cannot be a ceiling — zero, or any negative — is refused at the boundary from
// BOTH sources, rather than reaching engine.WithEnumerateLimit and being
// normalised away.
//
// This is the half a type check cannot catch: -5 is a perfectly good int, so the
// StringFlag/IntFlag argument buys nothing here. The hazard is the silent
// fallback — engine.WithEnumerateLimit deliberately normalises a non-positive
// bound to DefaultEnumerateLimit (correct for a Go embedder passing a computed
// number; see engine.TestEnumerateLimit* for the library side), which would
// leave an operator who typed -5 being served 1000 while believing otherwise.
// Lenient normalisation in the library, input validation at the boundary. Do not
// "fix" this by changing the engine.
func TestEnumerateLimit_NonPositiveIsConfigError(t *testing.T) {
	for _, raw := range []string{"0", "-1", "-5", "-1500"} {
		t.Run("from the environment/"+raw, func(t *testing.T) {
			t.Setenv(envEnumerateLimit, raw)
			opts, err := resolveEngineOptions(t)
			assertRejectedEnumerateLimit(t, opts, err, raw)
		})

		t.Run("from the flag/"+raw, func(t *testing.T) {
			// Empty, not unset: the flag must be what fails, not a leftover variable.
			t.Setenv(envEnumerateLimit, "")
			opts, err := resolveEngineOptions(t, "--enumerate-limit="+raw)
			assertRejectedEnumerateLimit(t, opts, err, raw)
		})
	}
}

// assertRejectedEnumerateLimit is the shared claim behind every refusal of a
// configured bound: the boot fails with one APERTURE_CONFIG_INVALID naming both
// spellings of the setting and the value it rejected, and NO engine option is
// produced — an option carrying a value the engine would normalise is exactly
// the silent fallback these tests exist to forbid.
func assertRejectedEnumerateLimit(t *testing.T, opts []engine.Option, err error, raw string) {
	t.Helper()
	if err == nil {
		t.Fatalf("--enumerate-limit=%s was accepted; it must fail the boot", raw)
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s, want %s (err=%v)", got, aerr.APERTURE_CONFIG_INVALID, err)
	}
	if d := codedDepth(err); d != 1 {
		t.Errorf("coded chain depth = %d, want exactly 1 (err=%v)", d, err)
	}
	msg := err.Error()
	for _, want := range []string{raw, "--enumerate-limit", envEnumerateLimit} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	if len(opts) != 0 {
		t.Errorf("a refused bound still produced %d engine option(s); the value must never reach the engine", len(opts))
	}
}

// codedDepth counts the Aperture-coded errors in a chain. A same-code re-stamp
// is invisible to CodeOf, so depth is what proves the refusal is constructed
// once rather than wrapped on the way out.
func codedDepth(err error) int {
	depth := 0
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		depth++
		err = errors.Unwrap(ce)
	}
	return depth
}

// TestServeFlagsNameTheirEnvVars asserts each --manage-* flag's usage text names
// the environment variable it pairs with. The usage string is the only shipped
// explanation of the flag, so an operator reading `aperture serve --help` must
// be able to find the env var from it.
func TestServeFlagsNameTheirEnvVars(t *testing.T) {
	usage := map[string]string{}
	for _, f := range serveCommand().Flags {
		bf, ok := f.(*ucli.BoolFlag)
		if !ok {
			continue
		}
		usage[bf.Name] = bf.Usage
		if bf.Name == "manage-accounts" || bf.Name == "manage-principals" || bf.Name == "manage-memberships" {
			if !bf.Value {
				t.Errorf("--%s defaults to false; all three manage flags default to true", bf.Name)
			}
		}
	}
	for flag, env := range map[string]string{
		"manage-accounts":    service.EnvManageAccounts,
		"manage-principals":  service.EnvManagePrincipals,
		"manage-memberships": service.EnvManageMemberships,
	} {
		u, ok := usage[flag]
		if !ok {
			t.Fatalf("serve has no --%s bool flag", flag)
		}
		if !strings.Contains(u, env) {
			t.Errorf("--%s usage does not name %s: %q", flag, env, u)
		}
	}

	// The convention is about the usage TEXT, not about the flag's Go type, so it
	// covers the string flags that pair with a variable too. --enumerate-limit
	// carries a real ucli.EnvVars source, which makes its usage text the only
	// place `aperture serve --help` says the variable exists at all.
	strUsage := map[string]string{}
	for _, f := range serveCommand().Flags {
		if sf, ok := f.(*ucli.StringFlag); ok {
			strUsage[sf.Name] = sf.Usage
		}
	}
	for flag, env := range map[string]string{
		"auth":            auth.EnvMode,
		"enumerate-limit": envEnumerateLimit,
	} {
		u, ok := strUsage[flag]
		if !ok {
			t.Fatalf("serve has no --%s string flag", flag)
		}
		if !strings.Contains(u, env) {
			t.Errorf("--%s usage does not name %s: %q", flag, env, u)
		}
	}
}

// TestEnumerateLimitIsNotAnIntFlag pins the shape choice, not just the
// behaviour. A ucli.IntFlag carrying the same EnvVars source parses the
// environment inside urfave and fails the command with its own UNCODED parse
// error before the action ever runs, so APERTURE_ENUMERATE_LIMIT=banana would
// report something other than APERTURE_CONFIG_INVALID. The StringFlag is what
// keeps the parse — and therefore the coded error — on this side of the
// boundary, while still leaving precedence to urfave's native flag > env >
// default. Changing the type is a contract change, not a tidy-up.
func TestEnumerateLimitIsNotAnIntFlag(t *testing.T) {
	for _, f := range serveCommand().Flags {
		if !slices.Contains(f.Names(), "enumerate-limit") {
			continue
		}
		if _, ok := f.(*ucli.StringFlag); !ok {
			t.Fatalf("--enumerate-limit is a %T; it must stay a *ucli.StringFlag so a malformed value is APERTURE_CONFIG_INVALID", f)
		}
		return
	}
	t.Fatal("serve has no --enumerate-limit flag")
}
