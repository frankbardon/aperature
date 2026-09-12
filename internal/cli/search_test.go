package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"

	ucli "github.com/urfave/cli/v3"
)

// labelledSeed is a catalogue whose objects carry the thing a person actually
// types: a name. The three brands are the shapes that make ranking observable —
// a company and one of its product lines sharing a word, a competitor sharing
// none, and an object carrying a name only in an alias list.
const labelledSeed = `
accounts:
  - {id: acme, name: Acme Corp, description: The tenant every grant is stamped to.}
memberships:
  - {principal: alice, account: acme}
  - {principal: bob, account: acme}
object_types:
  - name: brand
    description: A brand in the catalogue.
    actions: [read]
permissions:
  - id: perm-brand-read
    object_type: brand
    action: read
    scope_strategy: "implicit"
    description: Read every brand within the grant's pattern.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
  - {id: bob, kind: user, identity: "user:bob", display_name: Bob, roles: []}
roles:
  - {id: viewer, name: Viewer, description: May read brands., permissions: [perm-brand-read]}
grants:
  - id: g-viewer-read
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-brand-read
    object: "account:acme/**"
    effect: allow
objects:
  - id: "account:acme/brand:1"
    metadata: {label: "Nike Air Max Collection", sector: "Footwear"}
  - id: "account:acme/brand:2"
    metadata: {label: "Adidas Originals", sector: "Footwear"}
  - id: "account:acme/brand:3"
    metadata: {label: "Nike, Inc.", sector: "Footwear", aliases: [Nike, NIKE]}
`

func writeLabelledSeed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "labelled.yaml")
	if err := os.WriteFile(path, []byte(labelledSeed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

// runSearchCommand drives the real command tree and returns its stdout lines.
func runSearchCommand(t *testing.T, seedPath string, args ...string) ([]string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &bytes.Buffer{}
	app.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	full := append([]string{"aperture", "search", "--seed", seedPath, "--account", "acme"}, args...)
	err := app.Run(context.Background(), full)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(out.String())
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

func TestSearchCommandRanksByName(t *testing.T) {
	seedPath := writeLabelledSeed(t)
	got, err := runSearchCommand(t, seedPath, "alice", "read", "account:acme/brand:*", "nike")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("search 'nike' = %v, want the two Nike brands", got)
	}
	if got[0] != "account:acme/brand:3" {
		t.Errorf("best match = %q, want brand:3 (Nike, Inc.); full order %v", got[0], got)
	}
	for _, line := range got {
		if line == "account:acme/brand:2" {
			t.Error("Adidas matched 'nike'")
		}
	}
}

func TestSearchCommandToleratesATypo(t *testing.T) {
	seedPath := writeLabelledSeed(t)
	got, err := runSearchCommand(t, seedPath, "alice", "read", "account:acme/brand:*", "nkie")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("a transposed query resolved nothing")
	}
}

func TestSearchCommandIsContainedByEnumerate(t *testing.T) {
	// bob holds no role and therefore no grant. A search must tell him exactly
	// what an enumeration would: nothing. If these ever diverge, the CLI has
	// grown a second answer to the entitlement question.
	seedPath := writeLabelledSeed(t)
	got, err := runSearchCommand(t, seedPath, "bob", "read", "account:acme/brand:*", "nike")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a principal with no grants saw %v", got)
	}
}

func TestSearchCommandRestrictsToNamedFields(t *testing.T) {
	seedPath := writeLabelledSeed(t)
	// "Footwear" lives in sector, never in label.
	got, err := runSearchCommand(t, seedPath,
		"alice", "read", "account:acme/brand:*", "footwear", "--in", "label")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("--in was ignored: %v", got)
	}
	got, err = runSearchCommand(t, seedPath,
		"alice", "read", "account:acme/brand:*", "footwear", "--in", "sector")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("--in sector = %v, want all three brands", got)
	}
}

func TestSearchCommandComposesWithAFieldPredicate(t *testing.T) {
	seedPath := writeLabelledSeed(t)
	got, err := runSearchCommand(t, seedPath,
		"alice", "read", "account:acme/brand:*", "nike", "--field", "sector=Apparel")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the predicate did not narrow the search: %v", got)
	}
}

func TestSearchCommandRanksBeforeItTruncates(t *testing.T) {
	// --limit takes the top of a finished ranking. brand:1 sorts first by id and
	// matches 'nike' too, so a scan bounded by --limit would return it instead.
	seedPath := writeLabelledSeed(t)
	got, err := runSearchCommand(t, seedPath,
		"alice", "read", "account:acme/brand:*", "nike", "--limit", "1")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0] != "account:acme/brand:3" {
		t.Errorf("--limit 1 = %v, want the best match brand:3 — not the first one found", got)
	}
}

func TestSearchCommandShowsWhyItMatched(t *testing.T) {
	// The id alone cannot explain a surprising ranking: a hit on an alias and a
	// hit on a display name look identical from it.
	seedPath := writeLabelledSeed(t)
	got, err := runSearchCommand(t, seedPath,
		"alice", "read", "account:acme/brand:*", "nike", "--scores")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no output")
	}
	parts := strings.Split(got[0], "\t")
	if len(parts) != 3 {
		t.Fatalf("--scores line = %q, want id, score and field=value", got[0])
	}
	if !strings.Contains(parts[2], "=") {
		t.Errorf("--scores did not name the matching field: %q", got[0])
	}
}

func TestSearchCommandRejectsTheWrongArgumentCount(t *testing.T) {
	seedPath := writeLabelledSeed(t)
	_, err := runSearchCommand(t, seedPath, "alice", "read", "account:acme/brand:*")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %q, want APERTURE_INVALID_INPUT (err: %v)", got, err)
	}
}

func TestSearchCommandWithoutAnObjectSourceSaysSo(t *testing.T) {
	// A seed declaring no object source has nothing to match. The answer is the
	// misconfiguration, not an empty list that reads as "you may see nothing".
	seedPath := writeRuleBackedSeedWithoutObjects(t)
	_, err := runSearchCommand(t, seedPath, "alice", "read", "account:acme/**", "nike")
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %q, want APERTURE_PROVIDER_UNREGISTERED (err: %v)", got, err)
	}
}

// writeRuleBackedSeedWithoutObjects is the labelled fixture with its `objects:`
// section removed — a model that declares no object metadata source at all.
func writeRuleBackedSeedWithoutObjects(t *testing.T) string {
	t.Helper()
	body := labelledSeed[:strings.Index(labelledSeed, "objects:")]
	path := filepath.Join(t.TempDir(), "no-objects.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}
