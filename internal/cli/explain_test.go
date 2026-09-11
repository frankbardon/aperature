package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
)

// E5-S1: the CLI is a decision surface, and its Explain output must carry the
// evaluation notes rule evaluation records.
//
// The CLI renders a trace with exactly one call — Trace.String() — so the way to
// prove it is NON-LOSSY is to prove the printed bytes are that rendering, whole.
// A test that only looked for a notes section would pass just as happily against
// a CLI that hand-picked a few trace fields; this one fails the moment the
// command starts projecting the trace onto a subset, whatever the field.
func TestExplainCommandPrintsTheWholeTrace(t *testing.T) {
	ctx := context.Background()
	const (
		principal = "alice"
		action    = "read"
		object    = "account:acme/project:atlas/document:42"
	)

	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	if err := app.Run(ctx, []string{"aperture", "explain", principal, action, object}); err != nil {
		t.Fatalf("explain: %v", err)
	}

	// The same question, asked through the library the command wraps.
	//
	// This side used to build service.New(engine.New(store)) — the bare engine —
	// which was only equivalent to the command because the command was ALSO
	// unwired (issue #8). Now that every surface shares one decision stack, the
	// comparison has to be built the same way, or this test would quietly become a
	// wired-vs-unwired comparison that happens to agree on a rule-free fixture.
	store, err := buildStore(ctx, "", "")
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	stack, err := buildDecisionStack(unconfigured(), store, "")
	if err != nil {
		t.Fatalf("buildDecisionStack: %v", err)
	}
	tr, err := stack.newService().Explain(ctx, service.Query{
		Account: seed.ExampleAccount, Principal: principal, Action: action, Object: object,
	})
	if err != nil {
		t.Fatalf("service Explain: %v", err)
	}

	// Compared as a SET of lines: the grants a trace considers come back in
	// storage order, which the in-memory store does not stabilise, so the line
	// order is not something either side controls. Every line the library renders
	// must appear in the CLI's output and vice versa — which is the non-lossiness
	// claim, without importing a flake.
	if got, want := sortedLines(out.String()), sortedLines(tr.String()); !slices.Equal(got, want) {
		t.Fatalf("the CLI must print the trace verbatim:\n got:\n%s\nwant:\n%s", out.String(), tr.String())
	}
}

func sortedLines(s string) []string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	slices.Sort(lines)
	return lines
}

// TestExplainCommandRendersNotes pins the rendering itself: a trace carrying
// evaluation notes prints them, one per line, naming the grant, the rule and the
// diagnostic — and a trace with none prints no notes section at all.
func TestExplainCommandRendersNotes(t *testing.T) {
	tr := engine.Trace{
		Request: engine.Request{Account: "acme", Principal: "alice", Action: "read", Object: "account:acme/document:1"},
		Notes: []engine.EvaluationNote{{
			GrantID: "g-doc", Rule: "tagged", Kind: "shape_mismatch", Op: "has",
			Path: "object.tags", Expected: "collection", Actual: "string",
			Message: "object.tags: expected collection, got string",
		}},
	}
	report := tr.String()
	for _, want := range []string{
		"evaluation notes (1):",
		"g-doc",
		"rule tagged",
		"object.tags: expected collection, got string",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("rendered trace is missing %q:\n%s", want, report)
		}
	}

	tr.Notes = nil
	if strings.Contains(tr.String(), "evaluation notes") {
		t.Errorf("a trace with no notes must print no notes section:\n%s", tr.String())
	}
}
