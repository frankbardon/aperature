package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// The bound was raised in three separate layers — the engine's clamp
// (WithEnumerateLimit), the scope member gather (scope.Deps.MaxMembers) and the
// provider list (provider.boundLimit). Each is tested in its own package against
// its own seam, and each of those tests passes whether or not the OTHER two
// cooperate. This file is the only place the three are asked to compose: a real
// *provider.Registry behind a real scope resolver behind a real Engine, over a
// catalogue deliberately LARGER than the raised bound.
//
// The catalogue is twice the default so every interesting number is distinct: an
// unconfigured engine must stop at 1000, a raised one at 1500, and a bound above
// the catalogue must return all 2000 — three different answers from one fixture,
// which no single-layer regression can produce by accident.
const (
	// e2ePopulation is the number of documents the host catalogue holds.
	e2ePopulation = 2 * DefaultEnumerateLimit
	// e2eRaised is the configured bound: above the default, below the catalogue,
	// so a result of exactly this many ids is attributable to the bound and to
	// nothing else.
	e2eRaised = 1500
)

// e2eDocID spells the n-th document in the house example domain. The counter is
// ZERO-PADDED on purpose: canonical-id order is lexical, so padding makes the
// sorted order and the numeric order the same one, and "the first 1500 by
// canonical id" is a set the test can write down exactly rather than merely
// count.
func e2eDocID(n int) string {
	return fmt.Sprintf("account:acme/project:atlas/document:%04d", n)
}

// e2eDocRange is the canonical-id-ordered window [from, to] of the catalogue,
// the exact set a gather bounded at `to` is expected to yield.
func e2eDocRange(from, to int) []string {
	out := make([]string, 0, to-from+1)
	for n := from; n <= to; n++ {
		out = append(out, e2eDocID(n))
	}
	return out
}

// catalogueProvider is a host ObjectProvider over e2ePopulation documents in
// canonical-id order, which HONOURS Filter.Limit and remembers every limit it was
// asked for.
//
// Both halves are load-bearing. Honouring the limit is what makes the recorded
// number causal rather than decorative: had the engine's bound failed to reach
// here, the provider would have returned 1000 rows and no amount of downstream
// truncation could recover the missing 500. Recording it is what tells the two
// indistinguishable failures apart — "the bound never propagated" and "the bound
// propagated but something clamped it back down" both surface as 1000 ids, and
// only the limit the provider SAW says which.
type catalogueProvider struct {
	ids []identity.Identity

	mu     sync.Mutex
	limits []int
}

func newCatalogueProvider(t *testing.T) *catalogueProvider {
	t.Helper()
	ids := make([]identity.Identity, 0, e2ePopulation)
	for n := 1; n <= e2ePopulation; n++ {
		id, err := identity.Parse(e2eDocID(n))
		if err != nil {
			t.Fatalf("catalogue id %d: %v", n, err)
		}
		ids = append(ids, id)
	}
	return &catalogueProvider{ids: ids}
}

func (p *catalogueProvider) Fetch(_ context.Context, id identity.Identity) (provider.Metadata, error) {
	for _, o := range p.ids {
		if o.String() == id.String() {
			return provider.Metadata{"id": id.String()}, nil
		}
	}
	return nil, aerr.New(aerr.APERTURE_NOT_FOUND, "catalogue: absent object")
}

func (p *catalogueProvider) List(_ context.Context) ([]provider.Object, error) {
	out := make([]provider.Object, len(p.ids))
	for i, o := range p.ids {
		out[i] = provider.Object{ID: o, Metadata: provider.Metadata{"id": o.String()}}
	}
	return out, nil
}

func (p *catalogueProvider) Query(_ context.Context, f provider.Filter) ([]provider.Object, error) {
	p.mu.Lock()
	p.limits = append(p.limits, f.Limit)
	p.mu.Unlock()

	out := make([]provider.Object, 0, len(p.ids))
	for _, o := range p.ids {
		if f.Pattern != nil && !f.Pattern.Matches(o) {
			continue
		}
		out = append(out, provider.Object{ID: o, Metadata: provider.Metadata{"id": o.String()}})
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

// reset forgets the recorded limits so each enumeration is measured on its own.
func (p *catalogueProvider) reset() {
	p.mu.Lock()
	p.limits = nil
	p.mu.Unlock()
}

// observedLimit returns the single limit the provider was asked for, failing if
// it was asked a different number of times — an enumeration that listed twice
// would make "the limit that arrived" ambiguous.
func (p *catalogueProvider) observedLimit(t *testing.T) int {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.limits) != 1 {
		t.Fatalf("provider was queried %d times (%v), want exactly 1", len(p.limits), p.limits)
	}
	return p.limits[0]
}

// e2eFixture is the production wiring in miniature: alice holds one allow grant
// over the whole account, resolved by a real scope strategy, whose ObjectLister
// is a real *provider.Registry fronting the catalogue.
type e2eFixture struct {
	t     *testing.T
	store *memory.Store
	reg   *provider.Registry
	prov  *catalogueProvider
}

// newE2EFixture seeds the catalogue and one allow grant using the named scope
// strategy. The strategy matters: only implicit and exclusive reach the scope
// gather and the provider lister, which are two of the three layers under test.
// A literal or plain id-list grant would pass this file's assertions while
// touching neither.
func newE2EFixture(t *testing.T, strategy string) *e2eFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutPrincipal(ctx, model.Principal{
		ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice",
	}))
	mustSeed(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acctAcme}))
	mustSeed(t, store.PutPermission(ctx, model.Permission{
		ID: "p-read", ObjectType: "document", Action: "read", ScopeStrategy: strategy,
	}))
	mustSeed(t, store.PutGrant(ctx, model.Grant{
		ID: "g-all", AccountID: acctAcme,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	prov := newCatalogueProvider(t)
	reg := provider.NewRegistry()
	reg.MustRegister("document", prov)
	return &e2eFixture{t: t, store: store, reg: reg, prov: prov}
}

// engine builds the decision graph the way a host does: the same registry is the
// scope lister, and the bound options are whatever the case configures.
func (f *e2eFixture) engine(opts ...Option) *Engine {
	f.t.Helper()
	all := append([]Option{
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: f.reg}),
	}, opts...)
	return New(f.store, all...)
}

// enumerate runs one enumeration over the whole account and returns the ids
// together with the limit the host provider was actually asked for.
func (f *e2eFixture) enumerate(eng *Engine, limit int) ([]string, int) {
	f.t.Helper()
	f.prov.reset()
	ids, err := eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Limit: limit,
	})
	if err != nil {
		f.t.Fatalf("Enumerate(limit=%d): unexpected error: %v", limit, err)
	}
	return ids, f.prov.observedLimit(f.t)
}

// equalIDs reports whether got is want, in order. Enumerate's contract is an
// ordered result, so this is deliberately not a set comparison.
func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestARaisedBoundReturnsMoreThanADefaultEnumerationEndToEnd is the epic's
// composition proof on the implicit strategy, which reaches every layer the epic
// raised.
//
// Each row asserts three things rather than one: how many ids came back, WHICH
// ids they were (the first N in canonical order — so a regression that returned
// the right count from the wrong window still fails), and the limit the host
// provider was asked for (so a bound that never left the engine fails even if
// the count happens to come out right).
func TestARaisedBoundReturnsMoreThanADefaultEnumerationEndToEnd(t *testing.T) {
	cases := []struct {
		name string
		// opts configures the engine; empty means an engine with no bound option,
		// which is every embedder that has not opted in.
		opts []Option
		// reqLimit is the Limit on the request itself.
		reqLimit int
		// want is the exact id window expected back.
		want []string
		// wantAsked is the limit the provider must have been handed.
		wantAsked int
	}{{
		// The acceptance criterion: 2000 matching objects, a bound of 1500,
		// exactly 1500 ids. The catalogue is larger than the bound, so the number
		// is the bound's doing and not the fixture's size.
		name:      "a configured bound of 1500 returns exactly 1500 of 2000 objects",
		opts:      []Option{WithEnumerateLimit(e2eRaised)},
		reqLimit:  0,
		want:      e2eDocRange(1, e2eRaised),
		wantAsked: e2eRaised,
	}, {
		// The regression guard for every existing embedder: the SAME fixture,
		// no bound option, still stops at the historical 1000.
		name:      "the same fixture with no configured bound still returns exactly 1000",
		opts:      nil,
		reqLimit:  0,
		want:      e2eDocRange(1, DefaultEnumerateLimit),
		wantAsked: DefaultEnumerateLimit,
	}, {
		// A request may not talk its way past the deployment's ceiling.
		name:      "a request limit above the configured bound is clamped down to it",
		opts:      []Option{WithEnumerateLimit(e2eRaised)},
		reqLimit:  e2ePopulation,
		want:      e2eDocRange(1, e2eRaised),
		wantAsked: e2eRaised,
	}, {
		// ...but a request limit UNDER the bound is still the caller's to choose.
		// Note what the provider is asked for: the gather is bounded by the
		// engine's ceiling, and the request limit trims the decided result. The
		// two numbers are allowed to differ, and this row pins that they do.
		name:      "a request limit under the configured bound is honoured",
		opts:      []Option{WithEnumerateLimit(e2eRaised)},
		reqLimit:  1200,
		want:      e2eDocRange(1, 1200),
		wantAsked: e2eRaised,
	}, {
		// The fixture's own control. A bound ABOVE the catalogue returns the whole
		// catalogue, which is what proves 1500 and 1000 were truncations of a
		// genuinely larger candidate set rather than the most the fixture had.
		name:      "a bound above the catalogue returns every one of the 2000 objects",
		opts:      []Option{WithEnumerateLimit(e2ePopulation + 500)},
		reqLimit:  0,
		want:      e2eDocRange(1, e2ePopulation),
		wantAsked: e2ePopulation + 500,
	}, {
		// Lowering still works end to end, so the option is a bound and not a
		// "raise only" switch.
		name:      "a bound below the default truncates end to end too",
		opts:      []Option{WithEnumerateLimit(25)},
		reqLimit:  0,
		want:      e2eDocRange(1, 25),
		wantAsked: 25,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newE2EFixture(t, scope.StrategyImplicit)
			eng := f.engine(tc.opts...)

			ids, asked := f.enumerate(eng, tc.reqLimit)
			if len(ids) != len(tc.want) {
				t.Fatalf("Enumerate returned %d ids, want %d", len(ids), len(tc.want))
			}
			if !equalIDs(ids, tc.want) {
				t.Fatalf("Enumerate returned the wrong window: first=%q last=%q, want first=%q last=%q",
					ids[0], ids[len(ids)-1], tc.want[0], tc.want[len(tc.want)-1])
			}
			// The reason the count holds: the bound travelled engine -> scope
			// gather -> provider list, and the provider was asked for it.
			if asked != tc.wantAsked {
				t.Fatalf("host provider was asked for limit %d, want %d "+
					"— the configured bound did not reach the lister", asked, tc.wantAsked)
			}
		})
	}
}

// TestTheBoundedResultStaysSortedAndStableAcrossRuns covers the ordering half of
// the acceptance criteria. Truncation is only safe if the set truncated is
// deterministic: a bound that returned a different 1500 of 2000 on each call
// would page an operator round in circles and would make the count above the
// only thing any test could assert.
func TestTheBoundedResultStaysSortedAndStableAcrossRuns(t *testing.T) {
	f := newE2EFixture(t, scope.StrategyImplicit)
	eng := f.engine(WithEnumerateLimit(e2eRaised))

	first, _ := f.enumerate(eng, 0)
	if len(first) != e2eRaised {
		t.Fatalf("Enumerate returned %d ids, want %d", len(first), e2eRaised)
	}
	if !sort.SliceIsSorted(first, func(i, j int) bool { return first[i] < first[j] }) {
		t.Fatalf("Enumerate result is not sorted by canonical id")
	}
	// A fresh engine over the same store and registry, so nothing but the
	// algorithm decides the window.
	for run := 2; run <= 4; run++ {
		again, _ := f.enumerate(f.engine(WithEnumerateLimit(e2eRaised)), 0)
		if !equalIDs(again, first) {
			t.Fatalf("run %d returned a different truncated set than run 1 "+
				"(first=%q/%q, last=%q/%q)", run, again[0], first[0],
				again[len(again)-1], first[len(first)-1])
		}
	}
	// And the window really is the head of the catalogue, not an arbitrary slice
	// of it: the 1500th document is in and the 1501st is out.
	if first[len(first)-1] != e2eDocID(e2eRaised) {
		t.Fatalf("last returned id = %q, want %q", first[len(first)-1], e2eDocID(e2eRaised))
	}
	for _, id := range first {
		if id == e2eDocID(e2eRaised+1) {
			t.Fatalf("id %q is past the bound and must not be returned", id)
		}
	}
}

// TestTheExclusiveGatherComposesWithTheRaisedBound covers the second gather site.
// implicit and exclusive reach enumerateOfType by different routes — exclusive
// hands it a keep predicate — and a bound wired into one and not the other would
// leave half the deployments capped at 1000 with nothing in the result to say so.
//
// The count here is deliberately not a round number. The gather asks the lister
// for the bound, gets documents 1..1500, and only THEN drops the carved-out one:
// it does not reach forward to document 1501 to make the number whole again.
// 1499 is what a correct pipeline produces, and 1500 would mean the exclusion ran
// somewhere it should not have.
func TestTheExclusiveGatherComposesWithTheRaisedBound(t *testing.T) {
	const carved = 1 // account:acme/project:atlas/document:0001

	cases := []struct {
		name  string
		opts  []Option
		upTo  int // the window the gather is bounded to
		asked int
	}{{
		name:  "a raised bound gathers 1500 and the carve-out leaves 1499",
		opts:  []Option{WithEnumerateLimit(e2eRaised)},
		upTo:  e2eRaised,
		asked: e2eRaised,
	}, {
		name:  "with no configured bound the same carve-out leaves 999",
		opts:  nil,
		upTo:  DefaultEnumerateLimit,
		asked: DefaultEnumerateLimit,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newE2EFixture(t, "exclusive;ids="+e2eDocID(carved))
			ids, asked := f.enumerate(f.engine(tc.opts...), 0)

			want := e2eDocRange(carved+1, tc.upTo)
			if len(ids) != len(want) {
				t.Fatalf("exclusive enumerate returned %d ids, want %d", len(ids), len(want))
			}
			if !equalIDs(ids, want) {
				t.Fatalf("exclusive enumerate returned the wrong window: first=%q last=%q, want first=%q last=%q",
					ids[0], ids[len(ids)-1], want[0], want[len(want)-1])
			}
			if asked != tc.asked {
				t.Fatalf("host provider was asked for limit %d, want %d", asked, tc.asked)
			}
			for _, id := range ids {
				if id == e2eDocID(carved) {
					t.Fatalf("the carved-out object %q was returned", id)
				}
			}
		})
	}
}
