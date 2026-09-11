package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/mcp"
	"github.com/frankbardon/aperture/mcp/toolmeta"
)

// E3-S3 on the command line: `aperture enumerate --via <holder>.<field>`.
//
// The fixture is CSV-backed because a `references:` declaration lives on a
// `providers:` entry — that is the only place the seed spells one — so this also
// proves the whole path a real deployment uses: seed file -> registry ->
// declared reference -> engine dereference -> printed ids.
//
//	dataset:x       lists brand:1 and brand:2 (brand:3 exists, is not listed)
//	dataset:secret  lists brand:3, and alice may NOT read it
const referenceSeed = `
accounts:
  - {id: acme, name: Acme Corp, description: The tenant every grant is stamped to.}
memberships:
  - {principal: alice, account: acme}
object_types:
  - name: dataset
    description: A protected dataset.
    actions: [list]
  - name: brand
    description: A protected brand.
    actions: [list]
permissions:
  - id: perm-dataset-list
    object_type: dataset
    action: list
    scope_strategy: "implicit"
    description: List every dataset within the grant's pattern.
  - id: perm-brand-list
    object_type: brand
    action: list
    scope_strategy: "implicit"
    description: List every brand within the grant's pattern.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
roles:
  - {id: viewer, name: Viewer, description: May list datasets and brands., permissions: [perm-dataset-list, perm-brand-list]}
grants:
  - id: g-viewer-datasets
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-dataset-list
    object: "account:acme/**"
    effect: allow
  - id: g-viewer-brands
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-brand-list
    object: "account:acme/**"
    effect: allow
  - id: g-secret-deny
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-dataset-list
    object: "account:acme/dataset:secret"
    effect: deny
providers:
  - object_type: dataset
    kind: csv
    path: datasets.csv
    ttl: "0"
    references:
      current_brands: brand
  - {object_type: brand, kind: csv, path: brands.csv, ttl: "0"}
`

// writeReferenceSeed materialises the seed and its two CSVs in one directory
// (provider paths resolve against the seed's own directory) and returns the
// seed path.
func writeReferenceSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("brands.csv", "id,region\n"+
		"account:acme/brand:1,us\n"+
		"account:acme/brand:2,eu\n"+
		"account:acme/brand:3,us\n")
	write("datasets.csv", "id,current_brands:list\n"+
		"account:acme/dataset:x,account:acme/brand:1|account:acme/brand:2\n"+
		"account:acme/dataset:secret,account:acme/brand:3\n")
	path := filepath.Join(dir, "references.yaml")
	write("references.yaml", referenceSeed)
	return path
}

// runEnumerateBrands drives the real command tree over the reference fixture and
// returns the printed brand ids, sorted.
func runEnumerateBrands(t *testing.T, ctx context.Context, seedPath string, args ...string) ([]string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	argv := append([]string{
		"aperture", "enumerate", "--seed", seedPath, "--account", "acme",
	}, args...)
	argv = append(argv, "alice", "list", "account:acme/brand:*")
	if err := app.Run(ctx, argv); err != nil {
		return nil, err
	}
	ids := nonEmptyLines(out.String())
	slices.Sort(ids)
	return ids, nil
}

// TestEnumerateCommandRestrictsThroughAReference is the terminal half of the
// motivating question: "which brands does dataset x list?".
func TestEnumerateCommandRestrictsThroughAReference(t *testing.T) {
	ctx := context.Background()
	seedPath := writeReferenceSeed(t)

	all := []string{"account:acme/brand:1", "account:acme/brand:2", "account:acme/brand:3"}
	if got, err := runEnumerateBrands(t, ctx, seedPath); err != nil || !slices.Equal(got, all) {
		t.Fatalf("unrestricted enumerate = %v (err %v), want %v", got, err, all)
	}

	got, err := runEnumerateBrands(t, ctx, seedPath, "--via", "account:acme/dataset:x.current_brands")
	if err != nil {
		t.Fatalf("--via: %v", err)
	}
	if want := []string{"account:acme/brand:1", "account:acme/brand:2"}; !slices.Equal(got, want) {
		t.Fatalf("--via dataset:x.current_brands = %v, want %v", got, want)
	}

	// It composes with the metadata filter, and both precede --limit.
	got, err = runEnumerateBrands(t, ctx, seedPath,
		"--via", "account:acme/dataset:x.current_brands", "--field", "region=eu")
	if err != nil {
		t.Fatalf("--via with --field: %v", err)
	}
	if want := []string{"account:acme/brand:2"}; !slices.Equal(got, want) {
		t.Fatalf("--via + --field = %v, want %v", got, want)
	}
}

// The fail-closed rule on the terminal: a holder alice may not read prints
// NOTHING and exits cleanly. An operator must not be able to tell "you may not
// see dataset:secret" from "dataset:secret lists nothing you may see".
func TestEnumerateCommandUnreadableHolderPrintsNothing(t *testing.T) {
	ctx := context.Background()
	seedPath := writeReferenceSeed(t)

	got, err := runEnumerateBrands(t, ctx, seedPath, "--via", "account:acme/dataset:secret.current_brands")
	if err != nil {
		t.Fatalf("unreadable holder = error %v, want an empty result", err)
	}
	if len(got) != 0 {
		t.Fatalf("unreadable holder = %v, want no output", got)
	}
}

// ...while an ABSENT holder inside the account is reported. The two answers stay
// distinct on this surface exactly as they do on the wire.
func TestEnumerateCommandAbsentInAccountHolderIsReported(t *testing.T) {
	ctx := context.Background()
	seedPath := writeReferenceSeed(t)

	_, err := runEnumerateBrands(t, ctx, seedPath, "--via", "account:acme/dataset:nope.current_brands")
	if code := aerr.CodeOf(err); code != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("absent in-account holder = %v (code %s), want APERTURE_NOT_FOUND", err, code)
	}
}

// A holder in ANOTHER account is empty and never NOT_FOUND, whether or not it
// exists — the disclosure boundary, on the terminal.
func TestEnumerateCommandOutOfAccountHolderIsEmpty(t *testing.T) {
	ctx := context.Background()
	seedPath := writeReferenceSeed(t)

	got, err := runEnumerateBrands(t, ctx, seedPath, "--via", "account:other/dataset:z.current_brands")
	if err != nil {
		t.Fatalf("out-of-account holder = error %v, want an empty result", err)
	}
	if len(got) != 0 {
		t.Fatalf("out-of-account holder = %v, want no output", got)
	}
}

// Every malformed --via is an APERTURE_INVALID_INPUT naming the offending text —
// the same rejection a malformed --field gets, in the same words, never a silent
// skip. A dropped edge would WIDEN the result, and an edge that silently widens
// is an edge that authorizes.
func TestEnumerateCommandRejectsAMalformedVia(t *testing.T) {
	ctx := context.Background()
	seedPath := writeReferenceSeed(t)

	cases := []struct {
		name string
		via  string
	}{
		{name: "no '.' at all", via: "account:acme/dataset:x"},
		{name: "an empty holder", via: ".current_brands"},
		{name: "an empty field", via: "account:acme/dataset:x."},
		{name: "nothing at all", via: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runEnumerateBrands(t, ctx, seedPath, "--via", tc.via)
			if err == nil {
				t.Fatalf("want a rejection, got %v", got)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("code = %s, want %s (err: %v)", code, aerr.APERTURE_INVALID_INPUT, err)
			}
			if !strings.Contains(err.Error(), `"`+tc.via+`"`) {
				t.Fatalf("error %q does not name the offending input %q", err.Error(), tc.via)
			}
		})
	}
}

// TestParseReferenceEdges pins the one genuine design leaf: where the holder
// ends and the field begins. A '.' is legal inside an identity component, so the
// split is on the LAST one and nothing else.
func TestParseReferenceEdges(t *testing.T) {
	cases := []struct {
		name   string
		vias   []string
		holder []string
		field  []string
	}{
		{name: "no flag yields no edges"},
		{
			name:   "one edge",
			vias:   []string{"account:acme/dataset:x.current_brands"},
			holder: []string{"account:acme/dataset:x"},
			field:  []string{"current_brands"},
		},
		{
			name:   "repeatable, in order",
			vias:   []string{"dataset:x.current_brands", "campaign:spring.brands"},
			holder: []string{"dataset:x", "campaign:spring"},
			field:  []string{"current_brands", "brands"},
		},
		{
			name:   "a dotted identity splits on the LAST dot",
			vias:   []string{"account:acme/dataset:2026.q1.current_brands"},
			holder: []string{"account:acme/dataset:2026.q1"},
			field:  []string{"current_brands"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseReferenceEdges(tc.vias)
			if err != nil {
				t.Fatalf("parseReferenceEdges(%v): %v", tc.vias, err)
			}
			if len(got) != len(tc.holder) {
				t.Fatalf("edges = %#v, want %d", got, len(tc.holder))
			}
			for i, e := range got {
				if e.HolderID != tc.holder[i] || e.Field != tc.field[i] {
					t.Fatalf("edge %d = %#v, want holder %q field %q", i, e, tc.holder[i], tc.field[i])
				}
				// The CLI never states a holder type: the engine derives it from
				// the identity, so the two cannot disagree.
				if e.HolderType != "" {
					t.Fatalf("edge %d states a holder type %q", i, e.HolderType)
				}
			}
		})
	}

	// No flag is a nil slice — "no edges", indistinguishable from an
	// unrestricted enumeration all the way down.
	if got, err := parseReferenceEdges(nil); err != nil || got != nil {
		t.Fatalf("parseReferenceEdges(nil) = %#v, %v; want nil, nil", got, err)
	}
}

// TestEnumerateHelpStatesTheEdgeSpelling keeps the only place an operator learns
// the --via format, and why an unreadable holder looks like an empty result,
// from drifting out of the command.
func TestEnumerateHelpStatesTheEdgeSpelling(t *testing.T) {
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	if err := app.Run(context.Background(), []string{"aperture", "enumerate", "--help"}); err != nil {
		t.Fatalf("enumerate --help: %v", err)
	}
	help := out.String()
	for _, want := range []string{
		"--via",
		"<holder-identity>.<field>", // the spelling
		"LAST",                      // where the field begins
		"EMPTY",                     // what an unreadable holder looks like
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("`aperture enumerate --help` never mentions %q:\n%s", want, help)
		}
	}
}

// TestMCPSurfaceEnumeratesThroughAReference proves the agent surface shares the
// wiring: `aperture mcp` composes the same decision stack every other command
// does, and that stack now hands the registry to engine.WithReferences as well
// as to the lister and the metadata source. Without it a `--via`-equivalent
// tool call could only ever report APERTURE_PROVIDER_UNREGISTERED, however well
// the seed declared its references.
func TestMCPSurfaceEnumeratesThroughAReference(t *testing.T) {
	ctx := context.Background()
	seedPath := writeReferenceSeed(t)

	store, err := buildStore(ctx, "", seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	var warnings bytes.Buffer
	svc, stack, err := mcpService(unconfigured(), store, seedPath, &warnings)
	if err != nil {
		t.Fatalf("mcpService: %v", err)
	}
	defer func() { _ = stack.Close() }()

	var invoke mcp.InvokeFunc
	for _, d := range mcp.Tools(mcp.Config{Version: "test"}) {
		if d.Name == toolmeta.ToolEnumerate {
			invoke = d.Invoke
		}
	}
	if invoke == nil {
		t.Fatalf("no %s tool in the catalog", toolmeta.ToolEnumerate)
	}

	out, err := invoke(ctx, svc, json.RawMessage(
		`{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/brand:*",`+
			`"References":[{"HolderID":"account:acme/dataset:x","Field":"current_brands"}]}`))
	if err != nil {
		t.Fatalf("invoke %s: %v", toolmeta.ToolEnumerate, err)
	}
	res, ok := out.(mcp.EnumerateOut)
	if !ok {
		t.Fatalf("enumerate returned %T, want mcp.EnumerateOut", out)
	}
	got := slices.Clone(res.Objects)
	slices.Sort(got)
	if want := []string{"account:acme/brand:1", "account:acme/brand:2"}; !slices.Equal(got, want) {
		t.Fatalf("mcp enumerate via dataset:x = %v, want %v", got, want)
	}
}
