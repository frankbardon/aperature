package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// ruleBackedSeed is a model whose ONLY read permission is rule-backed:
// `inclusive;rule=public-documents` means the grant covers exactly the objects
// the rule selects, not everything its pattern matches. Object metadata is
// declared inline (`objects:`), so no file provider is needed.
//
// The three documents are the three interesting shapes:
//
//	document:42  tags ["public"]              -> selected      -> ALLOW
//	document:99  tags ["internal"]            -> not selected  -> DENY
//	document:7   tags "public" (a STRING)     -> shape mismatch -> DENY + a note
//
// Without a rule evaluator wired, the scope resolver reports
// APERTURE_SCOPE_RULE_UNCONFIGURED for all three and the facade folds that into a
// fail-closed deny — so an unwired surface answers DENY everywhere and disagrees
// with a wired one on document:42.
const ruleBackedSeed = `
accounts:
  - {id: acme, name: Acme Corp, description: The tenant every grant is stamped to.}
memberships:
  - {principal: alice, account: acme}
object_types:
  - name: document
    description: A protected document.
    actions: [read, list]
permissions:
  - id: perm-doc-read
    object_type: document
    action: read
    scope_strategy: "inclusive;rule=public-documents"
    description: Read a document the public-documents rule selects.
  # An implicit scope covers every object of the type within the grant's pattern,
  # which the resolver can only enumerate through the object LISTER — the other
  # half of the registry wiring, and what "aperture enumerate" exercises.
  - id: perm-doc-list
    object_type: document
    action: list
    scope_strategy: "implicit"
    description: List every document within the grant's pattern.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
roles:
  - {id: viewer, name: Viewer, description: May read public documents., permissions: [perm-doc-read, perm-doc-list]}
grants:
  - id: g-viewer-read
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-read
    object: "account:acme/**"
    effect: allow
  - id: g-viewer-list
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-list
    object: "account:acme/**"
    effect: allow
rules:
  - name: public-documents
    description: A document is public when its tags carry "public".
    ast:
      type: compare
      op: has
      left: {type: var, name: object.tags}
      right: {type: literal, value: "public"}
objects:
  - id: "account:acme/document:42"
    metadata: {tags: [public]}
  - id: "account:acme/document:99"
    metadata: {tags: [internal]}
  - id: "account:acme/document:7"
    metadata: {tags: "public"}
`

// unconfigured is the command a test passes buildDecisionStack when the shared
// configuration is not what it is asserting about: a command declaring no flags,
// which resolves every shared setting to "unset" and therefore to the library's
// own defaults. A test that DOES care drives the real flag set instead — see
// enumerate_limit_test.go.
func unconfigured() *ucli.Command { return &ucli.Command{} }

// writeRuleBackedSeed materialises the fixture and returns its path.
func writeRuleBackedSeed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rule-backed.yaml")
	if err := os.WriteFile(path, []byte(ruleBackedSeed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

// TestCLIAndServerAgreeOnRuleBackedDecisions is the invariant behind issue #8:
// the one-shot CLI commands and the server must return the SAME decision for the
// same model. They did not — `serve` wired the rules engine and scope
// resolution, `check` / `enumerate` / `explain` wired neither, so every
// rule-backed permission fail-closed denied on the CLI while the server answered
// from the rule.
//
// The test is deliberately two-sided. Equality alone would pass against a build
// where BOTH surfaces are unwired (deny == deny), so each case also pins the
// verdict the rule actually implies: document:42 must ALLOW, and that allow is
// only reachable if the rule was evaluated.
func TestCLIAndServerAgreeOnRuleBackedDecisions(t *testing.T) {
	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)

	cases := []struct {
		name      string
		object    string
		wantAllow bool
	}{
		{
			// Only reachable through rule evaluation: the grant's pattern matches
			// every object under the account, but the inclusive strategy hands
			// membership to the rule.
			name:      "rule selects the object",
			object:    "account:acme/document:42",
			wantAllow: true,
		},
		{
			name:      "rule does not select the object",
			object:    "account:acme/document:99",
			wantAllow: false,
		},
		{
			// Deny-safe: a collection operator over a string is false, not an error.
			name:      "metadata is the wrong shape",
			object:    "account:acme/document:7",
			wantAllow: false,
		},
	}

	srv := httptest.NewServer(server.New(serverService(t, ctx, seedPath)))
	defer srv.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cliAllow := runCheckCommand(t, ctx, seedPath, "alice", "read", tc.object)
			httpAllow := postCheck(t, srv.URL, "alice", "read", tc.object)

			if cliAllow != httpAllow {
				t.Fatalf("the CLI and the server disagree on %s: cli allow=%v, server allow=%v",
					tc.object, cliAllow, httpAllow)
			}
			if cliAllow != tc.wantAllow {
				t.Fatalf("want allow=%v for %s, got %v (both surfaces agree, so the rule is not being evaluated at all)",
					tc.wantAllow, tc.object, cliAllow)
			}
		})
	}
}

// TestEnumerateAgreesWithTheServer covers the other decision verb. Enumeration
// over an IMPLICIT scope has to walk the object lister to answer at all, so it is
// the case that proves the provider registry reaches the scope resolver and not
// only the rules fetcher: unwired, `aperture enumerate` could only report
// APERTURE_SCOPE_LISTER_UNCONFIGURED.
//
// The rule-backed row covers the seam the implicit one cannot. It could not be
// asserted at all until the inclusive resolver learned to enumerate its rule half
// — enumerating a rule-backed inclusive grant used to report
// APERTURE_SCOPE_RULE_UNCONFIGURED no matter how completely the stack was wired,
// so the surface-equivalence test could only reach the object lister through a
// permission that needed no rule. Both verbs now go through both seams.
func TestEnumerateAgreesWithTheServer(t *testing.T) {
	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)

	cases := []struct {
		name   string
		action string
		// want pins the content as well as the agreement, so an unwired build
		// cannot pass by returning nothing on both sides.
		want []string
	}{
		{
			name:   "implicit scope walks the object lister",
			action: "list",
			want: []string{
				"account:acme/document:42",
				"account:acme/document:7",
				"account:acme/document:99",
			},
		},
		{
			// Only document:42 carries a proper ["public"] tag list. document:99 is
			// tagged internal, and document:7's tags are a STRING, which the
			// collection operator treats as a deny-safe false rather than an error.
			name:   "rule-backed inclusive scope filters the listed objects",
			action: "read",
			want:   []string{"account:acme/document:42"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			app := NewApp("test")
			app.Writer = &out
			if err := app.Run(ctx, []string{
				"aperture", "enumerate", "--seed", seedPath, "--account", "acme",
				"alice", tc.action, "account:acme/**",
			}); err != nil {
				t.Fatalf("enumerate: %v", err)
			}
			got := nonEmptyLines(out.String())

			server, err := serverService(t, ctx, seedPath).Enumerate(ctx, service.EnumerateQuery{
				Account: "acme", Principal: "alice", Action: tc.action, Pattern: "account:acme/**",
			})
			if err != nil {
				t.Fatalf("server Enumerate: %v", err)
			}

			slices.Sort(got)
			slices.Sort(server)
			if !slices.Equal(got, server) {
				t.Fatalf("the CLI and the server disagree on enumeration:\n cli:    %v\n server: %v", got, server)
			}
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("enumeration = %v, want %v", got, want)
			}
		})
	}
}

// TestExplainSurfacesRuleNotesEndToEnd closes the loop E5-S1 left open. That
// story added shape-mismatch notes to the trace and proved the CLI prints the
// trace WHOLE (see explain_test.go), but no note could ever reach the CLI because
// `explain` had no rule evaluator to produce one. With the shared stack wired,
// asking about the mistyped document must surface the diagnostic.
func TestExplainSurfacesRuleNotesEndToEnd(t *testing.T) {
	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)

	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	if err := app.Run(ctx, []string{
		"aperture", "explain", "--seed", seedPath, "--account", "acme",
		"alice", "read", "account:acme/document:7",
	}); err != nil {
		t.Fatalf("explain: %v", err)
	}

	report := out.String()
	for _, want := range []string{
		"evaluation notes (1):",
		"g-viewer-read",
		"[rule public-documents]",
		"object.tags: expected collection, got string",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("explain output is missing %q:\n%s", want, report)
		}
	}
}

// TestServeAndOneShotCommandsShareOneStack pins the structural half of the fix:
// serve's extra dependencies are COMPOSED onto the shared stack, not a second
// copy of the wiring. A serve-only option (membership enforcement) must reach the
// engine without the one-shot commands inheriting it.
func TestServeAndOneShotCommandsShareOneStack(t *testing.T) {
	ctx := context.Background()
	seedPath := writeRuleBackedSeed(t)

	store, err := buildStore(ctx, "", seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	stack, err := buildDecisionStack(unconfigured(), store, seedPath)
	if err != nil {
		t.Fatalf("buildDecisionStack: %v", err)
	}
	if stack.registry == nil {
		t.Fatal("the provider registry must always be wired — it backs ObjectIdentifiers")
	}
	if stack.ruleSource == nil {
		t.Fatal("the storage rule source must be wired — it is what resolves rule references")
	}
	if stack.fetcher == nil {
		t.Fatal("a seed declaring an objects: section must get a metadata fetcher")
	}

	// The read-only facade a one-shot command gets still enumerates identifiers,
	// because the registry is wired unconditionally.
	ids, err := stack.newService().ObjectIdentifiers(ctx, "document")
	if err != nil {
		t.Fatalf("ObjectIdentifiers: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("want the three seeded documents, got %v", ids)
	}
}

// serverService builds the facade the server is handed, exactly as runServe does:
// the shared stack plus storage. Decisions depend only on the engine inside the
// stack, so this is the server's decision path.
func serverService(t *testing.T, ctx context.Context, seedPath string) *service.Service {
	t.Helper()
	store, err := buildStore(ctx, "", seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stack, err := buildDecisionStack(unconfigured(), store, seedPath)
	if err != nil {
		t.Fatalf("buildDecisionStack: %v", err)
	}
	return stack.newService(service.WithStorage(store))
}

// runCheckCommand runs `aperture check` through the real command tree and reports
// the printed verdict. A deny is signalled with a non-zero ExitCoder, which
// urfave/cli would otherwise turn into os.Exit — the no-op ExitErrHandler keeps
// the exit code observable inside the test process.
func runCheckCommand(t *testing.T, ctx context.Context, seedPath, principal, action, object string) bool {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	err := app.Run(ctx, []string{
		"aperture", "check", "--seed", seedPath, "--account", "acme",
		principal, action, object,
	})

	switch {
	case strings.HasPrefix(out.String(), "allow\n"):
		if err != nil {
			t.Fatalf("an allow must exit cleanly, got %v", err)
		}
		return true
	case strings.HasPrefix(out.String(), "deny\n"):
		var coder ucli.ExitCoder
		if !errors.As(err, &coder) || coder.ExitCode() == 0 {
			t.Fatalf("a deny must exit non-zero, got %v", err)
		}
		return false
	default:
		t.Fatalf("check printed no verdict (err %v):\n%s", err, out.String())
		return false
	}
}

// postCheck asks the HTTP surface the same question.
func postCheck(t *testing.T, baseURL, principal, action, object string) bool {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"account": "acme", "principal": principal, "action": action, "object": object,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(baseURL+"/check", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /check: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 from /check, got %d", resp.StatusCode)
	}
	var got struct {
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode /check response: %v", err)
	}
	return got.Allow
}

// nonEmptyLines splits command output into its non-blank lines.
func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
