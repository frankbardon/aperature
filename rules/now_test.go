package rules

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/expr-lang/expr"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// --- clocks -----------------------------------------------------------------

// advancingClock moves forward on EVERY read. It is the instrument that makes
// "the instant was read once" observable: any code path that reads it twice sees
// two different values, so a test asserting one value proves one read. A clock
// that merely returned a fixed time would pass whether the snapshot happened
// once or a thousand times.
type advancingClock struct {
	mu    sync.Mutex
	at    time.Time
	step  time.Duration
	reads int
}

func newAdvancingClock() *advancingClock {
	return &advancingClock{at: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC), step: time.Second}
}

func (c *advancingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	now := c.at
	c.at = c.at.Add(c.step)
	return now
}

func (c *advancingClock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// zonedClock answers in a non-UTC zone. An injected clock is host code and may be
// implemented any way at all, so the engine — not the clock — is responsible for
// the UTC guarantee.
type zonedClock struct{ t time.Time }

func (c zonedClock) Now() time.Time { return c.t }

// --- a program that reads the decision instant -------------------------------

// No operator renders __now yet — the relative-date node is the one that will —
// so these tests compile the probe expression directly, which is what that node's
// render will produce in shape: the instant arrives as an ARGUMENT to a
// $-prefixed dispatcher, never as a variable a rule could name.
//
// probeFunc registers such a dispatcher with an explicit signature, so the
// type-checker knows it returns a bool and two calls can be conjoined.
func probeFunc(name string, record func(time.Time)) CompilerOption {
	return func(c *Compiler) {
		c.options = append(c.options, expr.Function(name, func(args ...any) (any, error) {
			t, _ := args[0].(time.Time)
			record(t)
			return true, nil
		}, new(func(time.Time) bool)))
	}
}

// probeSource reads the decision instant TWICE in one expression. If anything
// re-read the clock between the two references, they would disagree.
const probeSource = `$probe(__now) && $probe(__now)`

// TestNowReachesEvaluationAsOneInstant proves the plumbing: Input.Now arrives at
// the compiled program under the __now identifier, and two references inside one
// evaluation see the SAME value. There is only one instant to see because the
// instant is data, fixed before the program runs, rather than a function the
// expression could call twice.
func TestNowReachesEvaluationAsOneInstant(t *testing.T) {
	var seen []time.Time
	comp := NewCompiler(probeFunc("$probe", func(at time.Time) { seen = append(seen, at) }))
	prog, err := comp.compileSource(probeSource)
	if err != nil {
		t.Fatalf("compile probe: %v", err)
	}

	want := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	if _, err := prog.eval(Input{Now: want}, nil); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("the probe ran %d times, want 2 (the expression reads __now twice)", len(seen))
	}
	if !seen[0].Equal(seen[1]) {
		t.Fatalf("two references to the decision instant disagreed: %s vs %s", seen[0], seen[1])
	}
	if !seen[0].Equal(want) {
		t.Fatalf("__now = %s, want the Input's instant %s", seen[0], want)
	}
}

// TestDecisionNowIsUTC pins the zone guarantee at both boundaries a non-UTC
// instant could enter through: an injected clock that answers in a local zone,
// and a hand-built Input.
func TestDecisionNowIsUTC(t *testing.T) {
	zone := time.FixedZone("plus5", 5*60*60)
	local := time.Date(2026, 3, 4, 17, 0, 0, 0, zone)
	wantUTC := local.UTC()

	t.Run("an injected clock is converted at the snapshot", func(t *testing.T) {
		var d *DecisionInstant
		got := d.snapshot(zonedClock{t: local})
		if got.Location() != time.UTC {
			t.Fatalf("snapshot location = %v, want UTC", got.Location())
		}
		if !got.Equal(wantUTC) {
			t.Fatalf("snapshot = %s, want %s", got, wantUTC)
		}

		ctx, scoped := WithDecisionInstant(context.Background())
		_ = scoped.snapshot(zonedClock{t: local})
		at, ok := decisionInstantFrom(ctx).At()
		if !ok {
			t.Fatal("the scope recorded no instant")
		}
		if at.Location() != time.UTC {
			t.Fatalf("scoped instant location = %v, want UTC", at.Location())
		}
	})

	t.Run("a hand-built Input is converted before evaluation", func(t *testing.T) {
		var seen []time.Time
		comp := NewCompiler(probeFunc("$probe", func(at time.Time) { seen = append(seen, at) }))
		prog, err := comp.compileSource(probeSource)
		if err != nil {
			t.Fatalf("compile probe: %v", err)
		}
		if _, err := prog.eval(Input{Now: local}, nil); err != nil {
			t.Fatalf("eval: %v", err)
		}
		if len(seen) == 0 {
			t.Fatal("the probe never ran")
		}
		if seen[0].Location() != time.UTC {
			t.Fatalf("__now location = %v, want UTC — no zone may reach evaluation", seen[0].Location())
		}
		if !seen[0].Equal(wantUTC) {
			t.Fatalf("__now = %s, want %s", seen[0], wantUTC)
		}
	})
}

// nowProbeEngine builds an engine whose one rule evaluates the probe expression.
//
// The surgery is deliberate and load-bearing: no operator renders __now yet, so
// the only way to drive the ENGINE's threading end to end today is to seat a
// probe program in the compiled-rule cache under the hash the rule's AST renders
// to. Engine.compile probes the cache by that hash and returns the entry without
// looking at it, so Selected then evaluates the probe against the Input the
// engine built — clock snapshot included. When the relative-date node lands, its
// own render replaces this stand-in and the assertions stay as they are.
func nowProbeEngine(t *testing.T, record func(time.Time), opts ...Option) (*Engine, identity.Identity) {
	t.Helper()

	ast := Compare(OpEq, Var("object.classification"), Lit("public"))
	src, err := ast.Expr()
	if err != nil {
		t.Fatalf("render rule: %v", err)
	}
	probe, err := NewCompiler(probeFunc("$probe", record)).compileSource(probeSource)
	if err != nil {
		t.Fatalf("compile probe: %v", err)
	}

	fetcher := fakeFetcher{"account:acme/document:1": {"classification": "public"}}
	eng := NewEngine(MapSource{"probe": {Name: "probe", AST: ast}}, fetcher, opts...)
	probeKey := hashSource(src)
	eng.cache.put(&Compiled{program: probe.program, source: src, key: probeKey, hash: hexHash(probeKey)})
	return eng, identity.MustParse("account:acme/document:1")
}

// TestEngineSnapshotsNowOncePerEvaluation is the straddle test the story asks
// for: a clock that moves on every read, and a rule that reads the decision
// instant twice. Both reads must agree — the engine reads the clock once and
// threads the value, so there is no window between the two references for the
// clock to move through.
func TestEngineSnapshotsNowOncePerEvaluation(t *testing.T) {
	var seen []time.Time
	clk := newAdvancingClock()
	eng, obj := nowProbeEngine(t, func(at time.Time) { seen = append(seen, at) }, WithClock(clk))

	if _, err := eng.Selected(context.Background(), "probe", obj, "acme", "user", "alice", "read"); err != nil {
		t.Fatalf("Selected: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("the rule read the instant %d times, want 2", len(seen))
	}
	if !seen[0].Equal(seen[1]) {
		t.Fatalf("one evaluation straddled a tick: %s then %s", seen[0], seen[1])
	}
	if got := clk.count(); got != 1 {
		t.Fatalf("the clock was read %d times for one evaluation, want exactly 1", got)
	}
}

// TestDecisionScopeSharesOneInstant widens that guarantee from one evaluation to
// one DECISION. A decision evaluates a rule per candidate grant, so without the
// scope each of those would take its own snapshot and a wide decision could
// resolve its first grant against one instant and its last against another.
func TestDecisionScopeSharesOneInstant(t *testing.T) {
	var seen []time.Time
	clk := newAdvancingClock()
	eng, obj := nowProbeEngine(t, func(at time.Time) { seen = append(seen, at) }, WithClock(clk))

	ctx, instant := WithDecisionInstant(context.Background())
	if _, ok := instant.At(); ok {
		t.Fatal("a fresh scope must hold no instant until an evaluation needs one")
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.Selected(ctx, "probe", obj, "acme", "user", "alice", "read"); err != nil {
			t.Fatalf("Selected(%d): %v", i, err)
		}
	}

	if got := clk.count(); got != 1 {
		t.Fatalf("the clock was read %d times across one decision, want exactly 1", got)
	}
	at, ok := instant.At()
	if !ok {
		t.Fatal("the scope recorded no instant after three evaluations")
	}
	for i, s := range seen {
		if !s.Equal(at) {
			t.Fatalf("read %d saw %s, want the decision's instant %s", i, s, at)
		}
	}

	// Nesting is idempotent: an inner scope must not shadow the outer one, or a
	// per-candidate decision inside an enumeration would take a fresh instant.
	inner, same := WithDecisionInstant(ctx)
	if same != instant {
		t.Fatal("a nested WithDecisionInstant must reuse the enclosing scope")
	}
	if _, err := eng.Selected(inner, "probe", obj, "acme", "user", "alice", "read"); err != nil {
		t.Fatalf("Selected(nested): %v", err)
	}
	if got := clk.count(); got != 1 {
		t.Fatalf("a nested scope re-read the clock; reads = %d, want 1", got)
	}
}

// TestUnscopedEvaluationSnapshotsPerEvaluation documents the other half of the
// contract: without a decision scope, each evaluation takes its own instant from
// the same clock. That is the direct-library case, and it is still one read per
// evaluation — never one per reference.
func TestUnscopedEvaluationSnapshotsPerEvaluation(t *testing.T) {
	clk := newAdvancingClock()
	eng, obj := nowProbeEngine(t, func(time.Time) {}, WithClock(clk))

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := eng.Selected(ctx, "probe", obj, "acme", "user", "alice", "read"); err != nil {
			t.Fatalf("Selected(%d): %v", i, err)
		}
	}
	if got := clk.count(); got != 3 {
		t.Fatalf("unscoped reads = %d, want one per evaluation (3)", got)
	}
}

// TestDecisionInstantIsNotAnInjectionPoint pins that a caller can open a scope
// but cannot choose what "now" means inside it: the value always comes from the
// engine's clock. Two callers disagreeing about now is the defect class the
// single-clock decision exists to prevent, so DecisionInstant deliberately has no
// setter.
func TestDecisionInstantIsNotAnInjectionPoint(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)}
	eng, obj := nowProbeEngine(t, func(time.Time) {}, WithClock(clk))

	ctx, instant := WithDecisionInstant(context.Background())
	if _, err := eng.Selected(ctx, "probe", obj, "acme", "user", "alice", "read"); err != nil {
		t.Fatalf("Selected: %v", err)
	}
	at, ok := instant.At()
	if !ok {
		t.Fatal("no instant recorded")
	}
	if !at.Equal(clk.t) {
		t.Fatalf("decision instant = %s, want the clock's %s", at, clk.t)
	}
}

// TestNowIsNotReachableFromARule pins the boundary that keeps the decision
// instant out of the variable namespace. Reflective METHOD calls survive
// expr.DisableAllBuiltins, so an exposed root would make `__now.AddDate(0,-3,0)`
// and `__now.Unix()` well-formed var paths — an unclamped calendar walk reachable
// from any rule. It fails if the identifier is ever added to allowedRoots.
func TestNowIsNotReachableFromARule(t *testing.T) {
	for _, name := range []string{nowVar, "NOW", "now", "TODAY", "today"} {
		if _, ok := allowedRoots[name]; ok {
			t.Fatalf("%q is an exposed variable root; the decision instant must never be one", name)
		}
	}
	for _, path := range []string{nowVar, nowVar + ".AddDate", nowVar + ".Unix", "NOW.AddDate"} {
		err := Var(path).Validate()
		if err == nil {
			t.Fatalf("Validate accepted the variable %q", path)
		}
		if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNKNOWN_VARIABLE {
			t.Fatalf("Validate(%q) code = %q, want APERTURE_RULE_UNKNOWN_VARIABLE", path, got)
		}
	}
}

// TestCachedProgramCarriesNoInstant proves the compiled-rule cache and the clock
// are independent: the rendered source names the instant's IDENTIFIER, never a
// resolved timestamp, so a cached program can never answer against a stale one.
// Compile, move the clock, evaluate again — the cache HITS and the evaluation
// still sees the new instant.
func TestCachedProgramCarriesNoInstant(t *testing.T) {
	var seen []time.Time
	clk := &fakeClock{t: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)}
	eng, obj := nowProbeEngine(t, func(at time.Time) { seen = append(seen, at) }, WithClock(clk))

	ctx := context.Background()
	if _, err := eng.Selected(ctx, "probe", obj, "acme", "user", "alice", "read"); err != nil {
		t.Fatalf("Selected(first): %v", err)
	}
	before := eng.CacheStats()

	clk.advance(48 * time.Hour)
	if _, err := eng.Selected(ctx, "probe", obj, "acme", "user", "alice", "read"); err != nil {
		t.Fatalf("Selected(second): %v", err)
	}
	after := eng.CacheStats()

	if after.Misses != before.Misses || after.Hits <= before.Hits {
		t.Fatalf("the second evaluation must be a cache hit; before=%+v after=%+v", before, after)
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 instant reads across two evaluations, got %d", len(seen))
	}
	if !seen[2].Equal(clk.t) {
		t.Fatalf("a cache hit answered against %s, want the advanced clock's %s", seen[2], clk.t)
	}
	if seen[0].Equal(seen[2]) {
		t.Fatal("the clock advanced but the second evaluation reused the first instant")
	}
	for _, c := range eng.cache.entries {
		if got := c.compiled.Source(); got != c.compiled.source {
			t.Fatalf("unexpected source drift: %q", got)
		}
	}
}

// TestPinnedClockAlsoFreezesCacheExpiry is the documented CONSEQUENCE of one time
// source. Pinning the clock so a date fixture is reproducible also stops TTL
// expiry, because they are the same clock — a test that pins for one gets the
// other whether it wanted it or not. This is the coupling WithClock warns about,
// asserted rather than left as prose.
func TestPinnedClockAlsoFreezesCacheExpiry(t *testing.T) {
	pinned := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)
	clk := &fakeClock{t: pinned}
	eng, obj := nowProbeEngine(t, func(time.Time) {},
		WithClock(clk), WithCacheTTL(time.Millisecond))

	ctx, instant := WithDecisionInstant(context.Background())
	for i := 0; i < 5; i++ {
		if _, err := eng.Selected(ctx, "probe", obj, "acme", "user", "alice", "read"); err != nil {
			t.Fatalf("Selected(%d): %v", i, err)
		}
	}

	// The date half: every decision resolves against the pinned month end.
	at, ok := instant.At()
	if !ok || !at.Equal(pinned) {
		t.Fatalf("decision instant = %s (recorded=%v), want the pinned %s", at, ok, pinned)
	}
	// The cache half, unasked for and unavoidable: a 1ms TTL under a frozen clock
	// never elapses, so nothing was evicted.
	if st := eng.CacheStats(); st.Evictions != 0 {
		t.Fatalf("a pinned clock must also freeze TTL expiry; evictions = %d", st.Evictions)
	}

	// Advancing the clock moves BOTH: the entry expires and the instant a fresh
	// decision resolves against moves with it.
	clk.advance(time.Second)
	if _, err := eng.Selected(context.Background(), "probe", obj, "acme", "user", "alice", "read"); err != nil {
		t.Fatalf("Selected(after advance): %v", err)
	}
	if st := eng.CacheStats(); st.Evictions == 0 {
		t.Fatalf("advancing the clock must expire the TTL'd entry; stats = %+v", st)
	}
}
