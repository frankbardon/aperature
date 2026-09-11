package engine

import (
	"context"
	"testing"

	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// An engine built with no bound option behaves exactly as it always has: the
// effective ceiling is the package default, and the clamp is the old arithmetic.
func TestUnconfiguredEngineClampsToTheDefault(t *testing.T) {
	eng := New(memory.New())

	if got := eng.enumerateBound(); got != DefaultEnumerateLimit {
		t.Fatalf("enumerateBound() = %d, want %d", got, DefaultEnumerateLimit)
	}
	cases := []struct {
		limit int
		want  int
	}{
		{0, DefaultEnumerateLimit},
		{-1, DefaultEnumerateLimit},
		{DefaultEnumerateLimit, DefaultEnumerateLimit},
		{DefaultEnumerateLimit + 1, DefaultEnumerateLimit},
		{7, 7},
	}
	for _, tc := range cases {
		if got := eng.boundEnumerateLimit(tc.limit); got != tc.want {
			t.Fatalf("boundEnumerateLimit(%d) = %d, want %d", tc.limit, got, tc.want)
		}
	}
}

// The clamp is against the CONFIGURED value, not the constant — in both
// directions. A bound above the default really does admit a request limit the
// constant would have rejected, and a bound below it really does cut one the
// constant would have allowed.
func TestTheConfiguredBoundGovernsTheClamp(t *testing.T) {
	cases := []struct {
		name      string
		configure int
		limit     int
		want      int
	}{
		{"raised: no request limit yields the raised bound", 5000, 0, 5000},
		{"raised: a request limit under it survives", 5000, 2500, 2500},
		{"raised: a limit the constant would have cut survives", 5000, DefaultEnumerateLimit + 1, DefaultEnumerateLimit + 1},
		{"raised: a limit above it is still clamped down", 5000, 5001, 5000},
		{"lowered: no request limit yields the lowered bound", 10, 0, 10},
		{"lowered: a limit the constant would have allowed is cut", 10, 900, 10},
		{"lowered: a limit under it survives", 10, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := New(memory.New(), WithEnumerateLimit(tc.configure))
			if got := eng.boundEnumerateLimit(tc.limit); got != tc.want {
				t.Fatalf("boundEnumerateLimit(%d) with bound %d = %d, want %d",
					tc.limit, tc.configure, got, tc.want)
			}
		})
	}
}

// A non-positive bound is normalised to the default, never stored. A zero bound
// would make every enumeration return nothing, which is indistinguishable from
// "no access" — the one failure mode this option must not be able to produce.
func TestANonPositiveBoundNormalisesToTheDefault(t *testing.T) {
	for _, n := range []int{0, -1, -1000} {
		eng := New(memory.New(), WithEnumerateLimit(n))
		if eng.enumerateLimit != DefaultEnumerateLimit {
			t.Fatalf("WithEnumerateLimit(%d) stored %d, want %d",
				n, eng.enumerateLimit, DefaultEnumerateLimit)
		}
		if got := eng.boundEnumerateLimit(0); got != DefaultEnumerateLimit {
			t.Fatalf("WithEnumerateLimit(%d): boundEnumerateLimit(0) = %d, want %d",
				n, got, DefaultEnumerateLimit)
		}
	}
}

// scopeDeps reads back the deps the engine's scope coverer actually holds — the
// ones a resolver will be handed — rather than the literal the caller passed in.
func scopeDeps(t *testing.T, eng *Engine) scope.Deps {
	t.Helper()
	sc, ok := eng.coverer.(scopeCoverer)
	if !ok {
		t.Fatalf("engine coverer is %T, want scopeCoverer", eng.coverer)
	}
	return sc.deps
}

// internal/cli hands WithScopeResolution a ScopeDeps LITERAL it built itself. The
// engine's bound must still govern the member gather; if the literal won, the CLI
// would keep the default gather while the engine clamped to something else, and
// nothing in the result would say so.
func TestACallerBuiltScopeDepsInheritsTheEngineBound(t *testing.T) {
	reg := provider.NewRegistry()

	// Both option orders, because options are applied in sequence and a stamp done
	// only inside WithScopeResolution would miss a bound configured after it.
	orders := []struct {
		name string
		opts []Option
	}{
		{"bound before scope resolution", []Option{
			WithEnumerateLimit(5000),
			WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg}),
		}},
		{"bound after scope resolution", []Option{
			WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg}),
			WithEnumerateLimit(5000),
		}},
	}
	for _, o := range orders {
		t.Run(o.name, func(t *testing.T) {
			eng := New(memory.New(), o.opts...)
			deps := scopeDeps(t, eng)
			if deps.MaxMembers != 5000 {
				t.Fatalf("deps.MaxMembers = %d, want 5000", deps.MaxMembers)
			}
			// The stamp must not clobber what the caller DID wire.
			if deps.Lister != scope.ObjectLister(reg) {
				t.Fatalf("stamping the bound replaced the caller's Lister: got %v", deps.Lister)
			}
		})
	}
}

// An engine wired with scope resolution and no bound option stamps the default,
// so the deps are never left with a zero MaxMembers to interpret.
func TestScopeDepsGetTheDefaultBoundWhenNoneIsConfigured(t *testing.T) {
	eng := New(memory.New(), WithScopeResolution(nil, ScopeDeps{}))
	if got := scopeDeps(t, eng).MaxMembers; got != DefaultEnumerateLimit {
		t.Fatalf("deps.MaxMembers = %d, want %d", got, DefaultEnumerateLimit)
	}
}

// A MaxMembers a caller set on its own literal is OVERWRITTEN. One enumeration is
// governed by one number: a gather bounded lower than the engine's clamp would
// truncate the member set before the engine ever saw it.
func TestTheEngineBoundOverwritesACallerSuppliedMaxMembers(t *testing.T) {
	eng := New(memory.New(),
		WithScopeResolution(nil, ScopeDeps{MaxMembers: 7}),
		WithEnumerateLimit(5000),
	)
	if got := scopeDeps(t, eng).MaxMembers; got != 5000 {
		t.Fatalf("deps.MaxMembers = %d, want 5000 (the engine's bound)", got)
	}
}

// WithRuleEvaluator returns a shallow copy; the stamped bound must ride along, or
// the what-if engine would gather against a different number than the live one.
func TestAShallowCopyKeepsTheStampedBound(t *testing.T) {
	eng := New(memory.New(),
		WithScopeResolution(nil, ScopeDeps{}),
		WithEnumerateLimit(5000),
	)
	clone := eng.WithRuleEvaluator(nil)
	if got := scopeDeps(t, clone).MaxMembers; got != 5000 {
		t.Fatalf("clone deps.MaxMembers = %d, want 5000", got)
	}
	if got := clone.boundEnumerateLimit(0); got != 5000 {
		t.Fatalf("clone boundEnumerateLimit(0) = %d, want 5000", got)
	}
	if got := eng.WithStore(memory.New()).boundEnumerateLimit(0); got != 5000 {
		t.Fatalf("WithStore copy boundEnumerateLimit(0) = %d, want 5000", got)
	}
}

// The bound is exercised at its configured value, not merely asserted as a
// constant: a real enumeration over four documents, bounded at two, returns two —
// and the same engine unbounded by the request still returns two.
func TestAConfiguredBoundTruncatesARealEnumeration(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	f.eng = New(f.store,
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: f.reg}),
		WithMetadata(f.reg),
		WithEnumerateLimit(2),
	)

	ids, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Limit: 0,
	})
	if err != nil {
		t.Fatalf("Enumerate: unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Enumerate with bound 2 returned %d ids (%v), want 2", len(ids), ids)
	}

	// A request limit ABOVE the configured bound is clamped down to it — the
	// default constant is not what is governing here.
	ids, err = f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Limit: DefaultEnumerateLimit,
	})
	if err != nil {
		t.Fatalf("Enumerate: unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Enumerate(Limit=%d) with bound 2 returned %d ids (%v), want 2",
			DefaultEnumerateLimit, len(ids), ids)
	}

	// And the truncation really is the bound's doing: the unbounded engine over the
	// same fixture sees more.
	f.eng = New(f.store,
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: f.reg}),
		WithMetadata(f.reg),
	)
	all, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Limit: 0,
	})
	if err != nil {
		t.Fatalf("Enumerate: unexpected error: %v", err)
	}
	if len(all) <= 2 {
		t.Fatalf("unbounded enumerate returned %d ids (%v), want more than 2 "+
			"— the fixture cannot prove the bound truncated anything", len(all), all)
	}
}
