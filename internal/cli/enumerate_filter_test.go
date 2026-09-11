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

// typedMetadataSeed is the CLI filter fixture. Its objects are declared through
// the seed's object source, so `aperture enumerate` reaches them the same way it
// reaches any provider-backed type: the registry the decision stack wires feeds
// both the scope resolver's lister and the engine's metadata fetcher.
//
// The metadata is deliberately mixed-typed, because that is the whole reason the
// command has two flags:
//
//	dataset:a  tier "premium" seats 5   active true  brands ["brand:Y","brand:Z"]
//	dataset:b  tier "basic"   seats 5   active false brands ["brand:Y"]
//	dataset:c  tier "premium" seats 12  active true  (no brands field at all)
const typedMetadataSeed = `
accounts:
  - {id: acme, name: Acme Corp, description: The tenant every grant is stamped to.}
memberships:
  - {principal: alice, account: acme}
object_types:
  - name: dataset
    description: A protected dataset.
    actions: [list]
permissions:
  - id: perm-dataset-list
    object_type: dataset
    action: list
    scope_strategy: "implicit"
    description: List every dataset within the grant's pattern.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
roles:
  - {id: viewer, name: Viewer, description: May list datasets., permissions: [perm-dataset-list]}
grants:
  - id: g-viewer-list
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-dataset-list
    object: "account:acme/**"
    effect: allow
objects:
  - id: "account:acme/dataset:a"
    metadata: {tier: premium, seats: 5, active: true, brands: ["brand:Y", "brand:Z"]}
  - id: "account:acme/dataset:b"
    metadata: {tier: basic, seats: 5, active: false, brands: ["brand:Y"]}
  - id: "account:acme/dataset:c"
    metadata: {tier: premium, seats: 12, active: true}
`

// writeTypedMetadataSeed materialises the fixture and returns its path.
func writeTypedMetadataSeed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "typed-metadata.yaml")
	if err := os.WriteFile(path, []byte(typedMetadataSeed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

// runEnumerateCommand drives the real command tree and returns the printed ids,
// sorted so the assertion does not depend on candidate order.
func runEnumerateCommand(t *testing.T, ctx context.Context, seedPath string, args ...string) ([]string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	argv := append([]string{
		"aperture", "enumerate", "--seed", seedPath, "--account", "acme",
	}, args...)
	argv = append(argv, "alice", "list", "account:acme/**")
	if err := app.Run(ctx, argv); err != nil {
		return nil, err
	}
	ids := nonEmptyLines(out.String())
	slices.Sort(ids)
	return ids, nil
}

// TestEnumerateCommandFiltersByMetadata is the end-to-end proof that both
// spellings reach provider.MatchFields with the values the operator meant, and
// that they compose with the documented precedence.
func TestEnumerateCommandFiltersByMetadata(t *testing.T) {
	ctx := context.Background()
	seedPath := writeTypedMetadataSeed(t)

	all := []string{"account:acme/dataset:a", "account:acme/dataset:b", "account:acme/dataset:c"}
	if got, err := runEnumerateCommand(t, ctx, seedPath); err != nil || !slices.Equal(got, all) {
		t.Fatalf("unfiltered enumerate = %v (err %v), want %v", got, err, all)
	}

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "--field matches a string field",
			args: []string{"--field", "tier=premium"},
			want: []string{"account:acme/dataset:a", "account:acme/dataset:c"},
		},
		{
			name: "--field is repeatable and the predicates AND",
			args: []string{"--field", "tier=premium", "--field", "brands=brand:Z"},
			want: []string{"account:acme/dataset:a"},
		},
		{
			// A list-valued field matches by MEMBERSHIP, so the string flag is
			// the right tool for "contains this id".
			name: "--field matches a list field by membership",
			args: []string{"--field", "brands=brand:Y"},
			want: []string{"account:acme/dataset:a", "account:acme/dataset:b"},
		},
		{
			// dataset:c carries no brands field at all, and an absent field never
			// matches — not even a nil want.
			name: "an absent field never matches",
			args: []string{"--field", "brands=brand:Q"},
			want: nil,
		},
		{
			// The lesson the help text exists to teach: seats is a NUMBER, so the
			// string "5" matches nothing.
			name: "--field seats=5 is a string and matches no numeric field",
			args: []string{"--field", "seats=5"},
			want: nil,
		},
		{
			name: "--fields-json seats=5 is a number and does match",
			args: []string{"--fields-json", `{"seats":5}`},
			want: []string{"account:acme/dataset:a", "account:acme/dataset:b"},
		},
		{
			name: "--fields-json carries a bool",
			args: []string{"--fields-json", `{"active":true}`},
			want: []string{"account:acme/dataset:a", "account:acme/dataset:c"},
		},
		{
			name: "--fields-json carries a whole list, compared by equality",
			args: []string{"--fields-json", `{"brands":["brand:Y","brand:Z"]}`},
			want: []string{"account:acme/dataset:a"},
		},
		{
			name: "both flags merge",
			args: []string{"--fields-json", `{"seats":5}`, "--field", "tier=premium"},
			want: []string{"account:acme/dataset:a"},
		},
		{
			// --fields-json says tier=basic, --field says premium: --field wins,
			// so the result is the premium rows narrowed by seats.
			name: "--field overrides --fields-json on a key collision",
			args: []string{"--fields-json", `{"seats":5,"tier":"basic"}`, "--field", "tier=premium"},
			want: []string{"account:acme/dataset:a"},
		},
		{
			// The inverse of the case above, proving the override is real rather
			// than the two agreeing by accident.
			name: "the losing --fields-json value would have selected something else",
			args: []string{"--fields-json", `{"seats":5,"tier":"basic"}`},
			want: []string{"account:acme/dataset:b"},
		},
		{
			// Filtering happens BEFORE the limit, so a limit of 1 over a
			// two-object result returns one of the filtered rows.
			name: "the filter runs before --limit",
			args: []string{"--field", "tier=premium", "--limit", "1"},
			want: []string{"account:acme/dataset:a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runEnumerateCommand(t, ctx, seedPath, tc.args...)
			if err != nil {
				t.Fatalf("enumerate %v: %v", tc.args, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("enumerate %v = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestEnumerateCommandRejectsAMalformedFilter asserts the command surfaces the
// coded rejection instead of enumerating unfiltered — the failure mode that
// would hand a caller MORE objects than they asked to see.
func TestEnumerateCommandRejectsAMalformedFilter(t *testing.T) {
	ctx := context.Background()
	seedPath := writeTypedMetadataSeed(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "--field without an '='", args: []string{"--field", "tier"}, want: `"tier"`},
		{name: "--fields-json that is not JSON", args: []string{"--fields-json", "{tier}"}, want: `"{tier}"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runEnumerateCommand(t, ctx, seedPath, tc.args...)
			if err == nil {
				t.Fatalf("want a rejection, got %v", got)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("code = %s, want %s (err: %v)", code, aerr.APERTURE_INVALID_INPUT, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the offending input %s", err.Error(), tc.want)
			}
		})
	}
}

// TestEnumerateHelpStatesThePrecedence keeps the one place an operator learns
// why --field seats=5 is a STRING, and which flag wins a collision, from
// drifting out of the command. The story makes the help text part of the
// contract, so it is asserted rather than assumed.
func TestEnumerateHelpStatesThePrecedence(t *testing.T) {
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	if err := app.Run(context.Background(), []string{"aperture", "enumerate", "--help"}); err != nil {
		t.Fatalf("enumerate --help: %v", err)
	}
	help := out.String()
	for _, want := range []string{
		"--field",
		"--fields-json",
		"STRING",   // --field's value type, stated plainly
		"OVERRIDE", // the precedence rule
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("`aperture enumerate --help` never mentions %q:\n%s", want, help)
		}
	}
}

// TestMCPSurfaceFiltersByMetadata closes the wiring gap E1-S2 left open. The mcp
// command built a bare engine.New(store) — no rule evaluator, no object lister,
// no metadata source — so the agent-facing surface could not answer a filtered
// enumerate at all, whatever the seed declared. It now composes the same
// decision stack every other surface does, and this drives the real tool
// invocation an MCP client makes through the facade that command builds.
func TestMCPSurfaceFiltersByMetadata(t *testing.T) {
	ctx := context.Background()
	seedPath := writeTypedMetadataSeed(t)

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
		`{"Account":"acme","Principal":"alice","Action":"list","Pattern":"account:acme/**","Fields":{"tier":"premium","seats":5}}`))
	if err != nil {
		t.Fatalf("invoke %s: %v", toolmeta.ToolEnumerate, err)
	}
	res, ok := out.(mcp.EnumerateOut)
	if !ok {
		t.Fatalf("enumerate returned %T, want mcp.EnumerateOut", out)
	}
	got := slices.Clone(res.Objects)
	slices.Sort(got)
	if want := []string{"account:acme/dataset:a"}; !slices.Equal(got, want) {
		t.Fatalf("mcp enumerate = %v, want %v", got, want)
	}
}
