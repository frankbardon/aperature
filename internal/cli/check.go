package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// denyExitCode is the process exit code for a clean DENY decision. A check that
// resolves to allow exits 0; a deny exits non-zero so the command composes in
// shell pipelines (e.g. `aperture check ... && deploy`).
const denyExitCode = 1

// checkCommand is `aperture check <principal> <action> <object>`: it builds a
// store from --seed/--store, asks the decision service the single question, and
// prints the verdict + reason. The exit code reflects the decision.
func checkCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "check",
		Usage:     "Decide whether a principal may take an action on an object",
		ArgsUsage: "<principal> <action> <object>",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:  "seed",
				Usage: "path to a JSON/YAML seed model (defaults to the embedded example)",
			},
			&ucli.StringFlag{
				Name:  "store",
				Usage: "DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path",
			},
			&ucli.StringFlag{
				Name:  "account",
				Usage: "active account the decision is scoped to",
				Value: seed.ExampleAccount,
			},
			// The process-wide enumeration bound, carried by every command that
			// decides so the CLI and `serve` cannot be configured apart. `check`
			// decides about one object and never enumerates, but it builds the same
			// stack through the same builder, and a flag missing from one decision
			// command is exactly how the two surfaces drifted in the first place.
			enumerateLimitFlag(),
		},
		Action: runCheck,
	}
}

func runCheck(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 3 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"check takes exactly 3 arguments (<principal> <action> <object>), got %d", args.Len())
	}
	principal, action, object := args.Get(0), args.Get(1), args.Get(2)

	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	stack, err := buildDecisionStack(cmd, store, cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()
	stack.reportCollisions(cmd.ErrWriter)

	svc := stack.newService()
	res, err := svc.Check(ctx, service.Query{
		Account:   cmd.String("account"),
		Principal: principal,
		Action:    action,
		Object:    object,
	})
	if err != nil {
		// Genuine input-validation error — surface it as a usage error (exit 1).
		return err
	}

	verdict := "deny"
	if res.Allow {
		verdict = "allow"
	}
	fmt.Fprintf(cmd.Writer, "%s\nreason: %s\n", verdict, res.Reason)

	if !res.Allow {
		// A clean deny: print the decision (above) then exit non-zero with no
		// extra error noise.
		return ucli.Exit("", denyExitCode)
	}
	return nil
}

// enumerateCommand is `aperture enumerate <principal> <action> <pattern>`: it
// lists the object ids the principal may act on under the pattern, optionally
// narrowed by object-metadata predicates (--field / --fields-json) and
// restricted to what a declared reference names (--via).
func enumerateCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "enumerate",
		Usage:     "List the objects a principal may act on",
		ArgsUsage: "<principal> <action> <pattern>",
		Description: "Lists every object id under <pattern> that <principal> may take <action> on.\n\n" +
			"--field and --fields-json narrow that list by OBJECT METADATA. The predicate is typed:\n" +
			"a field matches only when its value equals the wanted value AND is of the same kind, so\n" +
			"the string \"5\" never matches the number 5. --field always sends a STRING; use\n" +
			"--fields-json when a number, bool, or list is genuinely meant:\n\n" +
			"  --field tier=premium --field current_brands=brand:Y\n" +
			"  --fields-json '{\"seats\":5,\"active\":true,\"tags\":[\"public\"]}'\n\n" +
			"Both may be given together: --fields-json is merged FIRST and --field entries then\n" +
			"OVERRIDE it by key. Predicates are ANDed; a field the object does not carry never\n" +
			"matches; a list-valued field matches by membership. Filtering happens before --limit.\n\n" +
			"--via restricts the list to what a DECLARED REFERENCE names — the other direction:\n" +
			"--field asks \"which datasets contain brand Y?\", --via asks \"which brands does\n" +
			"dataset X list?\". It is spelled <holder-identity>.<field>, where the field is\n" +
			"everything after the LAST '.', and it is repeatable (edges are ANDed):\n\n" +
			"  --via account:acme/dataset:x.current_brands\n\n" +
			"A holder you may not read yields an EMPTY list and no error, which is deliberate:\n" +
			"\"you may not see dataset X\" and \"dataset X lists nothing you may see\" must not be\n" +
			"tellable apart. Restriction, like filtering, happens before --limit.\n\n" +
			"--limit and --enumerate-limit are two different bounds. --limit is THIS REQUEST's\n" +
			"cap; --enumerate-limit is the DEPLOYMENT's ceiling, the same value `aperture serve`\n" +
			"honours, and it is what a --limit larger than it is clamped down to. A --limit of\n" +
			"zero or less asks for the ceiling.",
		Flags: append(append([]ucli.Flag{
			&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
			&ucli.StringFlag{Name: "store", Usage: "DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path"},
			&ucli.StringFlag{Name: "account", Usage: "active account the enumeration is scoped to", Value: seed.ExampleAccount},
			&ucli.IntFlag{Name: "limit", Usage: "cap the number of returned object ids for THIS request, clamped down to the deployment's --enumerate-limit ceiling (<=0 means that ceiling, which is " + strconv.Itoa(engine.DefaultEnumerateLimit) + " unless configured)"},
			enumerateLimitFlag(),
		}, metadataFilterFlags()...), referenceEdgeFlags()...),
		Action: runEnumerate,
	}
}

func runEnumerate(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 3 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"enumerate takes exactly 3 arguments (<principal> <action> <pattern>), got %d", args.Len())
	}
	// Parsed BEFORE the store is opened: a malformed predicate is a usage error
	// and there is no reason to boot a decision stack to report one.
	fields, err := parseMetadataFilter(cmd.String(fieldsJSONFlagName), cmd.StringSlice(fieldFlagName))
	if err != nil {
		return err
	}
	edges, err := parseReferenceEdges(cmd.StringSlice(viaFlagName))
	if err != nil {
		return err
	}
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	stack, err := buildDecisionStack(cmd, store, cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()
	stack.reportCollisions(cmd.ErrWriter)

	svc := stack.newService()
	ids, err := svc.Enumerate(ctx, service.EnumerateQuery{
		Account:    cmd.String("account"),
		Principal:  args.Get(0),
		Action:     args.Get(1),
		Pattern:    args.Get(2),
		Fields:     fields,
		References: edges,
		Limit:      cmd.Int("limit"),
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Fprintln(cmd.Writer, id)
	}
	return nil
}

// identifiersCommand is `aperture identifiers <object_type>`: it lists every
// valid instance id of an object type, enumerated from the object sources the
// seed declares (`providers:` and `objects:`). --exclude drops ids, yielding the
// positive allow-list an exclusive ("all except these") allowance expands to.
func identifiersCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "identifiers",
		Usage:     "List all valid instance ids of an object type from its provider",
		ArgsUsage: "<object_type>",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
			&ucli.StringFlag{Name: "store", Usage: "DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path"},
			&ucli.StringSliceFlag{Name: "exclude", Usage: "id to omit from the result (repeatable); expands an exclusive allowance"},
			enumerateLimitFlag(),
		},
		Action: runIdentifiers,
	}
}

func runIdentifiers(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 1 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"identifiers takes exactly 1 argument (<object_type>), got %d", args.Len())
	}
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	stack, err := buildDecisionStack(cmd, store, cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()
	stack.reportCollisions(cmd.ErrWriter)

	svc := stack.newService()
	ids, err := svc.ObjectIdentifiers(ctx, args.Get(0), cmd.StringSlice("exclude")...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Fprintln(cmd.Writer, id)
	}
	return nil
}

// explainCommand is `aperture explain <principal> <action> <object>`: it prints
// the full decision trace.
func explainCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "explain",
		Usage:     "Explain why a decision resolved the way it did",
		ArgsUsage: "<principal> <action> <object>",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
			&ucli.StringFlag{Name: "store", Usage: "DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path"},
			&ucli.StringFlag{Name: "account", Usage: "active account the decision is scoped to", Value: seed.ExampleAccount},
			enumerateLimitFlag(),
		},
		Action: runExplain,
	}
}

func runExplain(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 3 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"explain takes exactly 3 arguments (<principal> <action> <object>), got %d", args.Len())
	}
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	stack, err := buildDecisionStack(cmd, store, cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()
	stack.reportCollisions(cmd.ErrWriter)

	svc := stack.newService()
	tr, err := svc.Explain(ctx, service.Query{
		Account:   cmd.String("account"),
		Principal: args.Get(0),
		Action:    args.Get(1),
		Object:    args.Get(2),
	})
	if err != nil {
		return err
	}
	fmt.Fprint(cmd.Writer, tr.String())
	return nil
}
