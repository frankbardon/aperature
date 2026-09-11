package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/aperture/service"
)

// The kind: csv attribute slice, driven through the real command tree.
//
// attributeSeed (attributes_test.go) proves the same shape for the INLINE
// section. This one changes exactly one thing — where the bags come from — and
// keeps the rest identical, because that is the claim: an attribute_providers:
// entry is another source for the same seam, not another seam.
//
// Two files, because the slot dispatch is part of what is being proven: a human
// principal is answered out of users.csv and a machine out of machines.csv. A
// build that served both from one directory, or that ignored kind entirely,
// fails on ci-runner.
const csvAttributeSeed = `
accounts:
  - {id: acme, name: Acme Corp}
memberships:
  - {principal: alice, account: acme}
  - {principal: bob, account: acme}
  - {principal: carol, account: acme}
  - {principal: dave, account: acme}
  - {principal: ci-runner, account: acme}
  - {principal: mallory, account: acme}
object_types:
  - {name: document, description: A protected document., actions: [read]}
permissions:
  - id: perm-doc-read
    object_type: document
    action: read
    scope_strategy: "inclusive;rule=engineering"
    description: Read a document when the asker is a cleared engineer.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
  - {id: bob, kind: user, identity: "user:bob", display_name: Bob, roles: [viewer]}
  - {id: carol, kind: user, identity: "user:carol", display_name: Carol, roles: [viewer]}
  - {id: dave, kind: user, identity: "user:dave", display_name: Dave, roles: [viewer]}
  - {id: ci-runner, kind: machine, identity: "machine:ci-runner", display_name: CI, roles: [viewer]}
  - {id: mallory, kind: user, identity: "user:mallory", display_name: Mallory, roles: [viewer]}
roles:
  - {id: viewer, name: Viewer, description: May read engineering documents., permissions: [perm-doc-read]}
grants:
  - id: g-viewer-read
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-read
    object: "account:acme/**"
    effect: allow
rules:
  - name: engineering
    description: The asker is a cleared engineer on the platform team.
    ast:
      type: and
      children:
        - type: compare
          op: eq
          left: {type: var, name: principal.department}
          right: {type: literal, value: "eng"}
        - type: compare
          op: ge
          left: {type: var, name: principal.clearance}
          right: {type: literal, value: 3}
        - type: compare
          op: has
          left: {type: var, name: principal.teams}
          right: {type: literal, value: "platform"}
attribute_providers:
  - {subject: user, kind: csv, path: users.csv}
  - {subject: machine, kind: csv, path: machines.csv}
`

// The id column holds BARE subject ids — "alice", not "user:alice". A file
// written the other way loads, caches and enumerates perfectly happily, and
// matches no principal id a decision ever presents.
const csvAttributeUsers = `id,department,clearance:int,teams:list
alice,eng,3,platform|oncall
bob,sales,5,platform
carol,eng,1,platform
dave,eng,5,crm
`

const csvAttributeMachines = `id,department,clearance:int,teams:list
ci-runner,eng,5,platform
`

// writeCSVAttributeSeed materialises the seed and the two files it names, side
// by side, and returns the seed's path. The relative path: values resolve
// against the seed's own directory, which is what a deployment looks like.
func writeCSVAttributeSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"attributes-csv.yaml": csvAttributeSeed,
		"users.csv":           csvAttributeUsers,
		"machines.csv":        csvAttributeMachines,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "attributes-csv.yaml")
}

// TestCSVAttributesDecideThroughCheck is the story's end-to-end criterion: a CSV
// of users, a rule over its columns, and the verdict `aperture check` returns.
//
// The rule reads NOTHING off the object. It asks three questions of the asker,
// and each one is a different half of the value model:
//
//	principal.department == "eng"    the plain scalar column
//	principal.clearance  >= 3        the :int column, compared NUMERICALLY
//	principal.teams      has "x"     the :list column, matched by MEMBERSHIP
//
// The typed ones are why alice's ALLOW is worth asserting: an untyped loader
// would hand the rule the string "3", and "3" >= 3 is not a comparison the
// evaluator performs — alice would deny. Likewise a list column parsed as a
// delimited blob would fail the membership test. Every deny below names the one
// question its principal fails, so no single wrong answer can pass the table.
func TestCSVAttributesDecideThroughCheck(t *testing.T) {
	ctx := context.Background()
	seedPath := writeCSVAttributeSeed(t)

	cases := []struct {
		name      string
		principal string
		wantAllow bool
	}{
		{
			// Only reachable by reading users.csv AND by reading it with the
			// column types the header declares.
			name:      "the user slot is served from its file, typed",
			principal: "alice",
			wantAllow: true,
		},
		{
			name:      "the scalar column decided against them",
			principal: "bob",
			wantAllow: false,
		},
		{
			// clearance 1: the :int column is compared as a number.
			name:      "the int column decided against them",
			principal: "carol",
			wantAllow: false,
		},
		{
			// teams is [crm]: the :list column is matched by membership, so a
			// blob-matching loader (which would find "platform" nowhere) and a
			// correct one agree here, while a loader that matched substrings of a
			// joined blob would wrongly allow a "platform-trial" row.
			name:      "the list column decided against them",
			principal: "dave",
			wantAllow: false,
		},
		{
			// Declared kind: machine, so the MACHINE file answers. Serving it out
			// of users.csv, or not at all, denies.
			name:      "the machine slot is served from its own file",
			principal: "ci-runner",
			wantAllow: true,
		},
		{
			// In no file at all. A missing SUBJECT is not a failed decision:
			// `principal` is the floor bag, whose only keys are id and kind, so the
			// comparison is simply false.
			name:      "a subject the file does not carry evaluates against the floor",
			principal: "mallory",
			wantAllow: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCheckCommand(t, ctx, seedPath, tc.principal, "read", "account:acme/document:42")
			if got != tc.wantAllow {
				t.Fatalf("check %s = allow:%v, want allow:%v", tc.principal, got, tc.wantAllow)
			}

			// The stack is shared, so `serve` must answer identically — the
			// attribute wiring lives in buildDecisionStack, not in one command's
			// handler, and this is what keeps it there.
			res, err := serverService(t, ctx, seedPath).Check(ctx, service.Query{
				Account: "acme", Principal: tc.principal, Action: "read",
				Object: "account:acme/document:42",
			})
			if err != nil {
				t.Fatalf("server Check: %v", err)
			}
			if res.Allow != got {
				t.Fatalf("the CLI and the server disagree on %s: cli allow=%v, server allow=%v",
					tc.principal, got, res.Allow)
			}
		})
	}
}

// TestACSVAttributeFileThatWillNotParseFailsTheBoot is the other half of "a
// malformed file is a coded error at build": the CLI must refuse to start rather
// than boot into a deployment whose every decision for that slot is a
// non-decision discovered in production.
func TestACSVAttributeFileThatWillNotParseFailsTheBoot(t *testing.T) {
	ctx := context.Background()
	seedPath := writeCSVAttributeSeed(t)
	broken := filepath.Join(filepath.Dir(seedPath), "users.csv")
	if err := os.WriteFile(broken, []byte("id,clearance:int\nalice,three\n"), 0o600); err != nil {
		t.Fatalf("rewrite users.csv: %v", err)
	}

	store, err := buildStore(ctx, "", seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := buildDecisionStack(unconfigured(), store, seedPath); err == nil {
		t.Fatal("the stack booted with an attribute file that does not parse")
	}
}
