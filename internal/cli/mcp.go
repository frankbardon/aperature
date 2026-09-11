package cli

import (
	"context"
	"fmt"
	"io"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/mcp/gosdk"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	ucli "github.com/urfave/cli/v3"
)

// mcpServerName is the MCP server identity reported during initialize.
const mcpServerName = "aperture"

// mcpCommand is `aperture mcp`: it hand-wires the dependency graph
// (storage -> engine -> service) and serves Aperture's READ-ONLY MCP surface over
// stdio — the transport an MCP client uses when it spawns Aperture as a
// subprocess. The surface is read/decide/simulate/inspect only; no mutating tool
// exists, so the command wires the facade with storage (for inspection + what-if
// reads) but NOT the gate / delegation / impersonation mutators.
//
// The CLI stays thin: it builds the graph, constructs the go-sdk server, mounts
// the SDK-free catalog through the single gosdk adapter, and runs the transport.
// All tool logic lives in the mcp/ core; the binary entrypoint (cmd/aperture)
// only assembles the command tree.
func mcpCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "mcp",
		Usage: "Serve the read-only Aperture MCP surface over stdio",
		Description: "Exposes Aperture's decision API (check/enumerate/explain, " +
			"single + bulk), a read-only what-if simulator, and model inspection " +
			"as MCP tools over stdio. No tool mutates. Intended to be spawned over " +
			"stdio by an MCP client.",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:  "seed",
				Usage: "path to a JSON/YAML seed model (defaults to the embedded example)",
			},
			&ucli.StringFlag{
				Name:  "store",
				Usage: "DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path",
			},
			// The agent-facing surface is bounded by the SAME configured ceiling as
			// `serve` and the one-shot commands. An MCP client that could enumerate
			// past the number the operator set would be the widest possible place for
			// the two to disagree.
			enumerateLimitFlag(),
		},
		Action: runMCP,
	}
}

// mcpService builds the read-only facade the MCP tools are driven through.
//
// It is the SAME decision stack `serve` and the one-shot commands build. This
// used to be a bare engine.New(store), which made the agent-facing surface
// answer differently from every other surface: no rule evaluator, no object
// lister, and — once the metadata filter landed — no metadata source, so an
// aperture_enumerate carrying `Fields` could only report
// APERTURE_PROVIDER_UNREGISTERED however well the seed declared its objects.
//
// cmd is the parsed `aperture mcp` command, read for the shared decision
// configuration the whole binary honours (today --enumerate-limit).
//
// warnings receives the seed's provider/objects collision report. stdout is the
// MCP transport, so the caller passes stderr.
//
// The stack is returned alongside the facade because it OWNS resources — the
// seed's database pools — and the facade does not: something has to outlive this
// call to close them. Callers defer stack.Close().
func mcpService(cmd *ucli.Command, store model.Storage, seedPath string, warnings io.Writer) (*service.Service, decisionStack, error) {
	stack, err := buildDecisionStack(cmd, store, seedPath)
	if err != nil {
		return nil, decisionStack{}, err
	}
	stack.reportCollisions(warnings)

	// Read-only facade: the stack for decisions + the store for inspection and the
	// what-if overlay base. No mutators are wired — the MCP surface never writes.
	return stack.newService(service.WithStorage(store)), stack, nil
}

func runMCP(ctx context.Context, cmd *ucli.Command) error {
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	svc, stack, err := mcpService(cmd, store, cmd.String("seed"), cmd.ErrWriter)
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    mcpServerName,
		Version: cmd.Root().Version,
	}, nil)
	if err := gosdk.Register(srv, svc, gosdk.Config{Version: cmd.Root().Version}); err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: registering the mcp surface failed", err)
	}

	fmt.Fprintln(cmd.ErrWriter, "aperture mcp: serving read-only MCP surface over stdio")
	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: mcp server failed", err)
	}
	return nil
}
