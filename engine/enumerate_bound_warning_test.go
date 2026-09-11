package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/scope"
)

// boundWarnFixture is the allow-all catalogue with a captured warn-level logger,
// so a test reads the engine's operational output the way an operator would.
type boundWarnFixture struct {
	*fieldsFixture
	logs *bytes.Buffer
}

func newBoundWarnFixture(t *testing.T, opts ...Option) *boundWarnFixture {
	t.Helper()
	f := allowAllFixture(t, brandCatalogue())
	logs := &bytes.Buffer{}
	base := []Option{
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: f.reg}),
		WithMetadata(f.reg),
		WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}
	f.eng = New(f.store, append(base, opts...)...)
	return &boundWarnFixture{fieldsFixture: f, logs: logs}
}

// read enumerates every document alice may read, at the given request limit.
func (f *boundWarnFixture) read(t *testing.T, limit int) []string {
	t.Helper()
	ids, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Limit: limit,
	})
	if err != nil {
		t.Fatalf("Enumerate(limit=%d): unexpected error: %v", limit, err)
	}
	return ids
}

const boundWarning = "enumeration returned exactly its bound"

// The motivating case: a catalogue of four documents enumerated by an engine
// bounded at two comes back with two, and the operator is told the result sat on
// its bound — the only signal that the set may be short, since nothing in
// ([]string, error) can say so.
func TestAResultSittingOnItsBoundIsWarnedAbout(t *testing.T) {
	f := newBoundWarnFixture(t, WithEnumerateLimit(2))

	if ids := f.read(t, 0); len(ids) != 2 {
		t.Fatalf("Enumerate with bound 2 returned %d ids (%v), want 2", len(ids), ids)
	}
	out := f.logs.String()
	if !strings.Contains(out, boundWarning) {
		t.Fatalf("no bound warning logged; logs = %q", out)
	}
	// The bound that was hit is NAMED, or the operator cannot tell which number to
	// raise.
	if !strings.Contains(out, "bound=2") {
		t.Fatalf("warning does not name the bound that was hit; logs = %q", out)
	}
	if !strings.Contains(out, "configured_bound=2") {
		t.Fatalf("warning does not name the engine's configured bound; logs = %q", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("bound warning was not logged at WARN; logs = %q", out)
	}
	// It is a hint, not an assertion: a complete set of exactly that size looks
	// identical from here, so the wording must not claim truncation as fact.
	if !strings.Contains(out, "may be truncated") {
		t.Fatalf("warning states truncation as fact rather than as a hint; logs = %q", out)
	}
}

// A result that came back UNDER its bound cannot have been truncated, so there is
// nothing to report and the engine says nothing.
func TestAResultBelowTheBoundIsSilent(t *testing.T) {
	f := newBoundWarnFixture(t, WithEnumerateLimit(DefaultEnumerateLimit))

	if ids := f.read(t, 0); len(ids) != 4 {
		t.Fatalf("Enumerate returned %d ids (%v), want the whole catalogue of 4", len(ids), ids)
	}
	if out := f.logs.String(); out != "" {
		t.Fatalf("an under-bound enumeration logged %q, want nothing", out)
	}
}

// A result sitting on a bound the CALLER chose is warned about too — the caller's
// limit is this enumeration's effective bound — and the two numbers are reported
// separately, because "I asked for 2" and "the deployment allows 1000" are
// different operational stories.
func TestTheEffectiveBoundIsTheCallersLimitWhenItIsSmaller(t *testing.T) {
	f := newBoundWarnFixture(t)

	if ids := f.read(t, 2); len(ids) != 2 {
		t.Fatalf("Enumerate(Limit=2) returned %d ids (%v), want 2", len(ids), ids)
	}
	out := f.logs.String()
	if !strings.Contains(out, boundWarning) {
		t.Fatalf("no bound warning logged for a caller-imposed bound; logs = %q", out)
	}
	if !strings.Contains(out, "bound=2") {
		t.Fatalf("warning does not name the caller's effective bound; logs = %q", out)
	}
	if !strings.Contains(out, "configured_bound=1000") {
		t.Fatalf("warning does not distinguish the engine's ceiling; logs = %q", out)
	}
	if !strings.Contains(out, "requested_limit=2") {
		t.Fatalf("warning does not report the limit the caller asked for; logs = %q", out)
	}
}

// An engine wired with no logger reports through slog.Default() rather than
// dereferencing a nil — and the result is byte-for-byte the one a logged engine
// returns, because the warning is an observation and never a decision.
func TestAnEngineWithNoLoggerNeitherPanicsNorChangesTheAnswer(t *testing.T) {
	// Bounded at exactly the catalogue's size, so the warning fires on both
	// engines and the result is still the whole deterministic set.
	quiet := allowAllFixture(t, brandCatalogue())
	quiet.eng = New(quiet.store,
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: quiet.reg}),
		WithMetadata(quiet.reg),
		WithEnumerateLimit(4),
	)
	silent, err := quiet.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**",
	})
	if err != nil {
		t.Fatalf("Enumerate on an unlogged engine: unexpected error: %v", err)
	}

	logged := newBoundWarnFixture(t, WithEnumerateLimit(4))
	want := logged.read(t, 0)
	if !strings.Contains(logged.logs.String(), boundWarning) {
		t.Fatalf("the logged engine did not warn, so this comparison proves nothing; logs = %q",
			logged.logs.String())
	}
	if !sameSet(silent, want) {
		t.Fatalf("unlogged engine returned %v, logged engine returned %v", silent, want)
	}
	if len(silent) != 4 {
		t.Fatalf("unlogged engine returned %d ids (%v), want 4", len(silent), silent)
	}
}

// Enumerate's return shape is unchanged: still ([]string, error), no third
// result and no truncation flag. Asserted against the method value, so widening
// it is a compile error here rather than a surprise at a surface.
func TestEnumerateStillReturnsIdsAndAnErrorOnly(t *testing.T) {
	var _ func(context.Context, EnumerateRequest) ([]string, error) = (&Engine{}).Enumerate
}

// A filtered enumeration whose SURVIVORS land on the bound is warned about, so
// the signal survives the Fields predicate rather than reporting on the raw
// candidate set.
func TestTheWarningCountsWhatWasReturnedNotWhatWasConsidered(t *testing.T) {
	f := newBoundWarnFixture(t, WithEnumerateLimit(4))

	// Three of the four documents carry seats=5; the bound is 4, so the filtered
	// result is under it and nothing is reported.
	ids, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**",
		Fields:  map[string]any{"seats": int64(5)},
	})
	if err != nil {
		t.Fatalf("Enumerate: unexpected error: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("filtered enumerate returned %d ids (%v), want 3", len(ids), ids)
	}
	if out := f.logs.String(); out != "" {
		t.Fatalf("a filtered result under its bound logged %q, want nothing "+
			"— the warning must count survivors, not candidates", out)
	}

	// Asked for three, the same three survivors now sit exactly on the effective
	// bound and the operator hears about it — even though all four candidates were
	// walked and the predicate, not the bound, is what removed the fourth.
	narrow := newBoundWarnFixture(t, WithEnumerateLimit(4))
	ids, err = narrow.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Limit: 3,
		Fields: map[string]any{"seats": int64(5)},
	})
	if err != nil {
		t.Fatalf("Enumerate: unexpected error: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("filtered enumerate returned %d ids (%v), want 3", len(ids), ids)
	}
	if out := narrow.logs.String(); !strings.Contains(out, boundWarning) {
		t.Fatalf("a filtered result sitting on its bound logged %q, want a warning", out)
	}
}
