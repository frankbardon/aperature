package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// searchCommand is `aperture search <principal> <action> <pattern> <query>`: it
// ranks the objects a principal may act on by how well their metadata matches
// free text — the name→id lookup a surface that takes input in a person's own
// words has to do before it can ask anything else.
//
// It is the operator's way to see exactly what a host's search call would see,
// which is the diagnosis "did my rename land?" needs and the one an operator
// cannot otherwise get: the library answer and the CLI answer come off the same
// facade, so they cannot disagree.
func searchCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "search",
		Usage:     "Rank the objects a principal may act on by a free-text name",
		ArgsUsage: "<principal> <action> <pattern> <query>",
		Description: "Ranks every object id under <pattern> that <principal> may take <action> on AND\n" +
			"whose metadata matches <query>, best match first.\n\n" +
			"Every result is one `aperture check` would allow: candidates are DECIDED before they\n" +
			"are scored, by the same walk `aperture enumerate` uses, so the result is always a\n" +
			"subset of what that command returns. A score ranks; it authorizes nothing.\n\n" +
			"Matching is case- and punctuation-insensitive (\"Nike, Inc.\" matches \"nike inc\") and\n" +
			"tolerates a typo or a transposition on tokens long enough for one to be unambiguous.\n" +
			"Only TEXT is matched — a string field and the string elements of a list field. Match\n" +
			"a number, bool, or date with --field instead.\n\n" +
			"Aperture has no notion of a \"label\": a label is an ordinary metadata field whose name\n" +
			"your host chose. By default every field holding text is searched; --in names the ones\n" +
			"to search, and each result reports which field actually matched.\n\n" +
			"  aperture search alice read 'account:acme/brand:*' nike\n" +
			"  aperture search alice read 'account:acme/brand:*' nike --in label --limit 5\n\n" +
			"--field / --fields-json and --via mean exactly what they mean on `enumerate`, and they\n" +
			"COMPOSE with the query: the predicate and the reference edge narrow the candidate set,\n" +
			"the query ranks what is left. \"The brand called Nike in dataset X\" is one call.\n\n" +
			"--min-score drops weak matches (default " + strconv.FormatFloat(provider.DefaultMinScore, 'g', -1, 64) + "). Raising it narrows the shortlist; it\n" +
			"never widens what the principal may see.\n\n" +
			"--limit caps how many MATCHES come back — the top of a finished ranking, not a bound\n" +
			"on the scan, so the best N are returned rather than the first N found. The scan itself\n" +
			"runs to the deployment's --enumerate-limit ceiling.",
		Flags: append(append([]ucli.Flag{
			&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
			&ucli.StringFlag{Name: "store", Usage: "DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path"},
			&ucli.StringFlag{Name: "account", Usage: "active account the search is scoped to", Value: seed.ExampleAccount},
			&ucli.StringSliceFlag{Name: inFlagName, Usage: "restrict matching to this metadata field; repeatable (default: every field holding text)"},
			&ucli.FloatFlag{Name: "min-score", Usage: "drop matches scoring below this, 0 to 1 (<=0 means the default, " + strconv.FormatFloat(provider.DefaultMinScore, 'g', -1, 64) + ")"},
			&ucli.IntFlag{Name: "limit", Usage: "cap the number of returned MATCHES for THIS request, clamped down to the deployment's --enumerate-limit ceiling (<=0 means that ceiling, which is " + strconv.Itoa(engine.DefaultEnumerateLimit) + " unless configured)"},
			&ucli.BoolFlag{Name: "scores", Usage: "print the score and the matching field/value alongside each id"},
			enumerateLimitFlag(),
		}, metadataFilterFlags()...), referenceEdgeFlags()...),
		Action: runSearch,
	}
}

// inFlagName is the repeatable flag restricting which metadata fields the query
// is matched against. It is spelled --in rather than --field to keep it audibly
// distinct from the PREDICATE flags: --in says where to look, --field says what
// to find, and conflating the two would let an operator think a search had been
// narrowed when it had only been filtered.
const inFlagName = "in"

func runSearch(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 4 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"search takes exactly 4 arguments (<principal> <action> <pattern> <query>), got %d", args.Len())
	}
	// Parsed BEFORE the store is opened: a malformed predicate or edge is a usage
	// error and there is no reason to boot a decision stack to report one.
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
	res, err := svc.Search(ctx, service.SearchQuery{
		Account:     cmd.String("account"),
		Principal:   args.Get(0),
		Action:      args.Get(1),
		Pattern:     args.Get(2),
		Query:       args.Get(3),
		MatchFields: cmd.StringSlice(inFlagName),
		Fields:      fields,
		References:  edges,
		MinScore:    cmd.Float("min-score"),
		Limit:       cmd.Int("limit"),
	})
	if err != nil {
		return err
	}
	for _, m := range res {
		if cmd.Bool("scores") {
			// The score alone is not readable — an operator diagnosing a surprising
			// ranking needs to know WHICH field matched, since a hit on an alias and
			// a hit on a display name look identical from the id.
			fmt.Fprintf(cmd.Writer, "%s\t%.3f\t%s=%s\n", m.Object, m.Score, m.Field, m.Value)
			continue
		}
		fmt.Fprintln(cmd.Writer, m.Object)
	}
	return nil
}
