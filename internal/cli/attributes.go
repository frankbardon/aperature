package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/frankbardon/aperture/authz"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// `aperture attributes` — the operator's window onto the attribute directories.
//
// Three subcommands, and they are deliberately not equals:
//
//	slots       what this deployment WIRES: which slots have a source, what kind
//	            it is, and how each slot's cache is tuned. No key, no bag.
//	query       a page OF a directory. The system-tier admin read, gated.
//	invalidate  drop cached bags so the next decision re-reads them. Gated.
//
// The CLI is the only surface this effort adds — there is no RPC method and no
// MCP tool for any of it. That is a scope decision with a reason: MCP is the
// surface where an agent listing a host's entire user table is least defensible,
// and a directory read is exactly the shape of request that should require a
// human at a shell with the deployment's seed file in hand.
//
// # Why `slots` is ungated and the other two are not
//
// `query` and `invalidate` go through the facade, which runs
// service.requireAttributeAdmin: the surface must be wired, the caller
// authenticated, and the caller a system-admin in its active account. The gate
// runs BEFORE the slot name is parsed, so a refused caller cannot learn which
// slots this deployment wires by reading which error came back.
//
// `slots` takes no actor and asks nothing of the gate, because it discloses
// nothing a caller did not already supply. It reads the SEED FILE the operator
// named on the command line and the cache configuration this process built from
// it; the answer is a restatement of the operator's own input, and requiring
// system-admin authority to read back a file you just passed in would only mean
// nobody could diagnose "is the user slot even wired?" without also holding the
// authority the diagnosis exists to explain. It never touches a provider, never
// names a key, and never prints a bag.

// attributeStack builds the decision stack the attribute commands read through,
// plus the cleanup that releases it. It is the SAME builder every other command
// uses, so the slots a listing reports are the slots a decision resolves through
// — the CLI cannot describe a wiring it does not itself run.
func attributeStack(ctx context.Context, cmd *ucli.Command) (decisionStack, func(), error) {
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return decisionStack{}, nil, err
	}
	stack, err := buildDecisionStack(cmd, store, cmd.String("seed"))
	if err != nil {
		_ = store.Close()
		return decisionStack{}, nil, err
	}
	stack.reportCollisions(cmd.ErrWriter)
	return stack, func() {
		_ = stack.Close()
		_ = store.Close()
	}, nil
}

// attributeService adds the admin gate to that stack.
//
// It has to be built here rather than taken from decisionStack.newService: a
// one-shot command wires no gate, and service.ListAttributes refuses outright
// with APERTURE_UNIMPLEMENTED when it has a directory in hand and nothing to
// authorize against. That refusal is correct — a system-tier read must never
// degrade to "no gate, so serve it" — so the command supplies the gate, the same
// authz.NewGate(engine) `serve` mounts, over the same engine.
func attributeService(ctx context.Context, cmd *ucli.Command) (*service.Service, func(), error) {
	stack, done, err := attributeStack(ctx, cmd)
	if err != nil {
		return nil, nil, err
	}
	svc := stack.newService(service.WithGate(authz.NewGate(stack.eng)))
	return svc, done, nil
}

// attributesCommand is `aperture attributes`, the parent of the three
// subcommands. It carries the staleness explanation once, where both `slots`
// (which shows the window) and `invalidate` (which closes it) inherit it.
func attributesCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "attributes",
		Usage: "Inspect the attribute directories a seed wires, read one, or drop cached bags",
		Description: "An attribute slot is a HOST DIRECTORY — the user table, the service-account\n" +
			"registry, the tenant catalogue — that a rule reads `principal.*` and `account.*`\n" +
			"out of. There are exactly three slots (user, machine, account) and each caches\n" +
			"the bags it has fetched, per slot, with its own ttl: and max_size:.\n\n" +
			"THE CACHE WINDOW IS A SECURITY PROPERTY, not only a tuning knob. Object metadata\n" +
			"that goes stale for a TTL is usually tolerable. An attribute bag is the ASKER'S\n" +
			"STANDING — the clearance, the department, the plan — so until a cached bag\n" +
			"expires, every decision about that subject keeps evaluating against access the\n" +
			"host has ALREADY TAKEN AWAY. Principals are the classic revoke case, and a\n" +
			"revocation that takes effect `ttl:` later is a revocation that has not happened\n" +
			"yet. What a slot's ttl: buys in fetch traffic it pays for in that delay.\n\n" +
			"So: pick a slot's ttl: for how fast its revocations must land, read back what a\n" +
			"deployment is actually running with `aperture attributes slots`, and close the\n" +
			"window on a specific subject with `aperture attributes invalidate`.\n\n" +
			"Reading a directory in bulk (`query`) and dropping cached bags (`invalidate`)\n" +
			"are SYSTEM-TIER operations: both require --principal holding system-admin\n" +
			"authority in --account, and a refusal returns nothing at all — no partial page,\n" +
			"no count, and nothing that tells an unauthorized caller which slots exist.",
		Commands: []*ucli.Command{
			attributesSlotsCommand(),
			attributesQueryCommand(),
			attributesInvalidateCommand(),
		},
	}
}

// attributesSlotsCommand is `aperture attributes slots`: the wiring listing.
func attributesSlotsCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "slots",
		Usage: "List the three attribute slots, the source each is wired to, and its cache settings",
		Description: "Prints one row per slot — user, machine, account — with the source the seed\n" +
			"declares for it (csv, sql, or inline), the cache freshness window, the cached-bag\n" +
			"cap, and how many bags this process currently holds.\n\n" +
			"THE TTL COLUMN IS THE REVOCATION WINDOW. A slot's cached bag keeps authorizing\n" +
			"until it expires, so `ttl` is the longest a removed clearance can keep working.\n" +
			"`never` means a bag, once fetched, is only dropped by eviction or by an explicit\n" +
			"`aperture attributes invalidate` — correct for a fixed inline block, dangerous\n" +
			"for a live directory.\n\n" +
			"The `cached` column counts THIS process's cache. A one-shot invocation starts\n" +
			"cold, so it reads 0; it is the number that matters in a long-running\n" +
			"`aperture serve`.\n\n" +
			"No actor is required: this reports the wiring in the seed file you passed and\n" +
			"the configuration this process built from it. It contacts no provider and prints\n" +
			"no subject key and no attribute value.",
		Flags:  storeFlags(),
		Action: runAttributeSlots,
	}
}

func runAttributeSlots(ctx context.Context, cmd *ucli.Command) error {
	// The document is the only source for a slot's SOURCE: providers:, objects:,
	// attributes: and attribute_providers: are runtime wiring that Apply never
	// writes to storage, so the file is their source of truth. The precedence
	// between the two attribute sections is seed's own rule, asked of the
	// document rather than re-derived here (seed.Document.AttributeSlotSources).
	doc, err := seedDocument(cmd.String("seed"))
	if err != nil {
		return err
	}
	sources := doc.AttributeSlotSources()

	stack, done, err := attributeStack(ctx, cmd)
	if err != nil {
		return err
	}
	defer done()

	w := tabwriter.NewWriter(cmd.Writer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "slot\tsource\tttl\tmax-size\tcached")
	for _, slot := range provider.AttributeSlots() {
		cfg, wired := stack.attributes.CacheConfigFor(slot)
		if !wired {
			// Unwired is not an error and not an empty directory: it is a
			// deployment that declared no source for this party. Every fetch
			// against it is APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, which the
			// decision path treats leniently (the floor bag) and an enumeration
			// does not.
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", slot, "(unwired)", "-", "-", "-")
			continue
		}
		source := sources[slot.String()]
		if source == "" {
			// A registered slot the document does not account for: a host that
			// wired this registry in Go rather than from the seed. Reported as
			// unknown rather than guessed at.
			source = "(host)"
		}
		stats, _ := stack.attributes.Stats(slot)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n",
			slot, source, renderTTL(cfg.TTL), renderMaxSize(cfg.MaxSize), stats.Entries)
	}
	return w.Flush()
}

// renderTTL spells a slot's freshness window for the listing. Zero is rendered
// as the word rather than "0s" because the two read very differently to an
// operator scanning the column: "0s" invites "expires immediately", and the
// truth is the opposite — the entry never expires on its own.
func renderTTL(d time.Duration) string {
	if d <= 0 {
		return "never"
	}
	return d.String()
}

// renderMaxSize spells the cached-bag cap. Zero means no cap at all, which is
// worth a word for the same reason: "0" would read as "caches nothing".
func renderMaxSize(n int) string {
	if n <= 0 {
		return "unbounded"
	}
	return strconv.Itoa(n)
}

// attributeRecordOut is the JSON shape `query` prints. It exists so the output
// has lower-case, stable field names without putting json tags on
// provider.AttributeRecord, whose shape is a library contract and not a
// presentation format.
type attributeRecordOut struct {
	ID         string            `json:"id"`
	Attributes provider.Metadata `json:"attributes"`
}

// attributesQueryCommand is `aperture attributes query`: the gated bulk read.
func attributesQueryCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags, metadataFilterFlags()...)
	flags = append(flags,
		&ucli.IntFlag{Name: "limit", Usage: "cap the number of returned records (<=0 means the default; the registry clamps it regardless)"},
	)
	return &ucli.Command{
		Name:      "query",
		Usage:     "Read a page of one attribute slot's directory (system-admin tier)",
		ArgsUsage: "<slot>",
		Description: "Returns up to --limit records of <slot> — user, machine, or account — as a JSON\n" +
			"array of {id, attributes}, narrowed by attribute predicates.\n\n" +
			"THIS IS A SYSTEM-TIER READ. Unfiltered, it returns the head of the host's user\n" +
			"table, keys and bags together, so it requires --principal holding system-admin\n" +
			"authority in --account. A refusal returns NOTHING — no partial page, no count,\n" +
			"and no way to tell an empty slot from a full one or from an unwired one. Ask\n" +
			"`aperture explain` about your own authority if a refusal is unexpected.\n\n" +
			"--field and --fields-json narrow the result by ATTRIBUTE, on exactly the\n" +
			"predicate `aperture enumerate` applies to object metadata: predicates are ANDed,\n" +
			"a field the bag does not carry never matches, a list-valued field matches by\n" +
			"membership, and everything else matches by TYPED equality, so the string \"5\"\n" +
			"never matches the number 5. --field always sends a string; use --fields-json\n" +
			"when a number, bool, or list is genuinely meant:\n\n" +
			"  aperture attributes query user --principal alice --account acme \\\n" +
			"    --field department=eng --fields-json '{\"clearance\":3}'\n\n" +
			"Both may be given together: --fields-json is merged FIRST and --field entries\n" +
			"then override it by key.\n\n" +
			"A slot whose sql: entry declares no get_all: is FETCH-ONLY by design — it can\n" +
			"answer the decision path without exposing the whole table to an enumeration —\n" +
			"and this command reports that provider's coded refusal rather than an empty\n" +
			"page.",
		Flags:  flags,
		Action: runAttributeQuery,
	}
}

func runAttributeQuery(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() != 1 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"attributes query takes exactly 1 argument (<slot>, one of %s), got %d",
			slotList(), cmd.Args().Len())
	}
	// Parsed BEFORE anything is opened: a malformed predicate is a usage error,
	// and there is no reason to boot a decision stack to report one.
	fields, err := parseMetadataFilter(cmd.String(fieldsJSONFlagName), cmd.StringSlice(fieldFlagName))
	if err != nil {
		return err
	}
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	svc, done, err := attributeService(ctx, cmd)
	if err != nil {
		return err
	}
	defer done()

	// The slot string is handed over UNPARSED. service.ListAttributes runs the
	// gate first and parses second, on purpose; parsing here would move the
	// disclosure the ordering exists to prevent back into the CLI.
	recs, err := svc.ListAttributes(ctx, actor, cmd.Args().Get(0), provider.AttributeFilter{
		Fields: fields,
		Limit:  cmd.Int("limit"),
	})
	if err != nil {
		return err
	}
	out := make([]attributeRecordOut, 0, len(recs))
	for _, rec := range recs {
		out = append(out, attributeRecordOut{ID: rec.ID, Attributes: rec.Attributes})
	}
	return printJSON(cmd, out)
}

// attributesInvalidateCommand is `aperture attributes invalidate`: the gated
// cache drop.
func attributesInvalidateCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "id", Usage: "drop only this subject's cached bag (a bare principal or account id); omit to clear the whole slot"},
		&ucli.BoolFlag{Name: "all", Usage: "clear EVERY slot's cache; takes no <slot> argument and no --id"},
	)
	return &ucli.Command{
		Name:      "invalidate",
		Usage:     "Drop cached attribute bags so the next decision re-reads them (system-admin tier)",
		ArgsUsage: "<slot>",
		Description: "Drops cached bags, so the next decision about the affected subjects pulls fresh\n" +
			"ones from the host directory. Three forms:\n\n" +
			"  aperture attributes invalidate user --id alice   one subject, one slot\n" +
			"  aperture attributes invalidate user             every bag in one slot\n" +
			"  aperture attributes invalidate --all            every bag in every slot\n\n" +
			"INVALIDATION IS A SECURITY CONTROL, NOT A PERFORMANCE KNOB. A cached attribute\n" +
			"bag is the asker's standing, so a REVOKED CLEARANCE KEEPS AUTHORIZING until that\n" +
			"bag expires: for the length of the slot's ttl:, every decision about that\n" +
			"subject is made against access the host has already removed. Waiting the window\n" +
			"out is not a remedy, it is the exposure. An operator who has just removed\n" +
			"someone's access invalidates that subject here, and then the removal is true.\n\n" +
			"Scope: this drops the caches of THE PROCESS THAT RUNS IT. That makes it exact\n" +
			"for a host embedding Aperture (it is the operator's spelling of\n" +
			"provider.AttributeRegistry.Invalidate, which such a host calls the moment its\n" +
			"directory changes) and it makes a ONE-SHOT invocation self-contained: this\n" +
			"process starts with a cold cache and exits with it, so there is nothing here for\n" +
			"a stale bag to survive in. For a long-running `aperture serve`, the controls\n" +
			"that reach ITS cache are the slot's ttl: — set it to how fast that directory's\n" +
			"revocations must land — and a restart.\n\n" +
			"Requires --principal holding system-admin authority in --account: the result\n" +
			"reports whether a bag was cached, which is a fact about who has recently been\n" +
			"decided about, and clearing a large slot costs the next wave of decisions a\n" +
			"provider round-trip each.",
		Flags:  flags,
		Action: runAttributeInvalidate,
	}
}

func runAttributeInvalidate(ctx context.Context, cmd *ucli.Command) error {
	var (
		all  = cmd.Bool("all")
		id   = cmd.String("id")
		args = cmd.Args()
	)
	// The three forms are mutually exclusive, and a conflict is refused rather
	// than resolved by precedence: "--all plus a slot" has two plausible readings
	// (everything, or just that slot) and guessing at the broader one would clear
	// caches the operator did not ask to clear.
	switch {
	case all && args.Len() > 0:
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"attributes invalidate --all clears every slot and takes no <slot> argument; drop --all to clear one slot")
	case all && id != "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"attributes invalidate --all clears every slot and takes no --id; drop --all to invalidate one subject")
	case !all && args.Len() != 1:
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"attributes invalidate takes exactly 1 argument (<slot>, one of %s) or --all, got %d",
			slotList(), args.Len())
	}

	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	svc, done, err := attributeService(ctx, cmd)
	if err != nil {
		return err
	}
	defer done()

	if all {
		if err := svc.InvalidateAllAttributes(ctx, actor); err != nil {
			return err
		}
		fmt.Fprintln(cmd.Writer, "cleared every attribute slot's cache")
		return nil
	}

	// Unparsed, for the same reason as query: the facade gates first and parses
	// second so a refusal cannot report which slots exist.
	slot := args.Get(0)
	if id == "" {
		if err := svc.InvalidateAttributeSlot(ctx, actor, slot); err != nil {
			return err
		}
		fmt.Fprintf(cmd.Writer, "cleared the %s slot's cache\n", slot)
		return nil
	}
	dropped, err := svc.InvalidateAttribute(ctx, actor, slot, id)
	if err != nil {
		return err
	}
	if dropped {
		fmt.Fprintf(cmd.Writer, "dropped the cached %s bag for %q\n", slot, id)
		return nil
	}
	// Not an error: "nothing was cached" is the state the caller asked for. It is
	// reported rather than silently succeeding, because an operator invalidating
	// a subject they believe is cached wants to know their key did not match.
	fmt.Fprintf(cmd.Writer, "no cached %s bag for %q\n", slot, id)
	return nil
}

// slotList renders the closed slot set for a usage error. It reads the set from
// provider rather than restating it, so a CLI message cannot name a different
// three from the registry.
func slotList() string {
	slots := provider.AttributeSlots()
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.String())
	}
	return strings.Join(out, ", ")
}
