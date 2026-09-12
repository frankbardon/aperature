package service

import (
	"context"
	"reflect"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// The facade's job for a search is to carry the query and adapt the result, and
// nothing else — no normalising the text, no re-ranking, no second opinion about
// what the subject may see. These tests pin that, and pin the one property the
// whole surface rests on: what Search returns is a subset of what Enumerate
// would.

// newSearchSvc returns a facade and the engine underneath it, sharing one
// registry as scope lister, metadata source and provider registry — the
// production wiring.
func newSearchSvc(t *testing.T) (*Service, *engine.Engine) {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustPut(t, store.PutAccount(ctx, model.Account{ID: acct, Name: acct}))
	mustPut(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustPut(t, store.PutPermission(ctx, model.Permission{
		ID: "p-impl", ObjectType: "document", Action: "read", ScopeStrategy: scope.StrategyImplicit,
	}))
	mustPut(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	mustPut(t, store.PutGrant(ctx, allowGrant("g-all", "p-impl", "account:acme/**")))

	sp, err := provider.NewStatic([]provider.Object{
		{ID: identity.MustParse("account:acme/document:1"),
			Metadata: provider.Metadata{"label": "Nike Air Max Collection", "sector": "Footwear"}},
		{ID: identity.MustParse("account:acme/document:2"),
			Metadata: provider.Metadata{"label": "Adidas Originals", "sector": "Footwear"}},
		{ID: identity.MustParse("account:acme/document:3"),
			Metadata: provider.Metadata{"label": "Nike, Inc.", "sector": "Footwear"}},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", sp)

	eng := engine.New(store,
		engine.WithScopeResolution(scope.DefaultRegistry(), engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
	)
	return New(eng, WithProviders(reg)), eng
}

func searchQuery(text string) SearchQuery {
	return SearchQuery{
		Account: acct, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Query: text,
	}
}

func matchObjects(in []SearchMatch) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.Object
	}
	return out
}

// The facade and the engine answer the same question the same way, for every
// shape of query. Anything the facade did to the text or the predicate on the
// way through would show up here as a divergence.
func TestService_SearchPassesTheQueryThrough(t *testing.T) {
	svc, eng := newSearchSvc(t)
	ctx := context.Background()

	cases := []SearchQuery{
		searchQuery("nike"),
		searchQuery("NIKE"),
		searchQuery("nike inc"),
		searchQuery("nkie"),
		func() SearchQuery { q := searchQuery("nike"); q.MatchFields = []string{"sector"}; return q }(),
		func() SearchQuery {
			q := searchQuery("nike")
			q.Fields = map[string]any{"sector": "Footwear"}
			return q
		}(),
		func() SearchQuery { q := searchQuery("nike"); q.MinScore = 0.99; return q }(),
		func() SearchQuery { q := searchQuery("nike"); q.Limit = 1; return q }(),
	}
	for _, q := range cases {
		got, err := svc.Search(ctx, q)
		if err != nil {
			t.Fatalf("facade Search(%+v): %v", q, err)
		}
		want, err := eng.Search(ctx, q.request())
		if err != nil {
			t.Fatalf("engine Search(%+v): %v", q, err)
		}
		if len(got) != len(want) {
			t.Fatalf("query %q: facade returned %v, engine %d results", q.Query, matchObjects(got), len(want))
		}
		for i := range want {
			if got[i].Object != want[i].Object || got[i].Score != want[i].Score ||
				got[i].Field != want[i].Field || got[i].Value != want[i].Value {
				t.Errorf("query %q item %d: facade %+v, engine %+v", q.Query, i, got[i], want[i])
			}
			if !reflect.DeepEqual(map[string]any(want[i].Metadata), got[i].Metadata) {
				t.Errorf("query %q item %d: metadata diverged", q.Query, i)
			}
		}
	}
}

// The containment property, at the facade. A surface that renders search results
// is rendering a subset of the subject's entitled set, always.
func TestService_SearchIsContainedByEnumerate(t *testing.T) {
	svc, _ := newSearchSvc(t)
	ctx := context.Background()

	allowed, err := svc.Enumerate(ctx, EnumerateQuery{
		Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	allowedSet := map[string]struct{}{}
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}
	for _, text := range []string{"nike", "adidas", "footwear", "collection", "a"} {
		res, err := svc.Search(ctx, searchQuery(text))
		if err != nil {
			t.Fatalf("Search(%q): %v", text, err)
		}
		for _, m := range res {
			if _, ok := allowedSet[m.Object]; !ok {
				t.Errorf("Search(%q) proposed %q, which Enumerate withholds", text, m.Object)
			}
		}
	}
}

func TestService_SearchRanksAndCarriesMetadata(t *testing.T) {
	svc, _ := newSearchSvc(t)
	res, err := svc.Search(context.Background(), searchQuery("nike"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) < 2 {
		t.Fatalf("expected two hits, got %v", matchObjects(res))
	}
	if res[0].Object != "account:acme/document:3" {
		t.Errorf("best match = %q, want document:3 (Nike, Inc.)", res[0].Object)
	}
	if res[0].Metadata["sector"] != "Footwear" {
		t.Errorf("metadata not carried inline: %v", res[0].Metadata)
	}
	if res[0].Field != "label" || res[0].Value != "Nike, Inc." {
		t.Errorf("match provenance = %q/%q, want label/%q", res[0].Field, res[0].Value, "Nike, Inc.")
	}
}

func TestService_SearchReturnsEngineErrorsVerbatim(t *testing.T) {
	// Search cannot fail open by construction, so an error is an error — never a
	// silently short list rendered as "nothing matched".
	svc, _ := newSearchSvc(t)
	q := searchQuery("")
	_, err := svc.Search(context.Background(), q)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %q, want APERTURE_INVALID_INPUT (err: %v)", got, err)
	}
}

func TestService_SearchBatchAlignsWithItsQueries(t *testing.T) {
	svc, _ := newSearchSvc(t)
	qs := []SearchQuery{searchQuery("nike"), searchQuery(""), searchQuery("adidas")}
	res := svc.SearchBatch(context.Background(), qs)
	if len(res) != len(qs) {
		t.Fatalf("batch returned %d items for %d queries", len(res), len(qs))
	}
	if res[0].Err != nil || len(res[0].Result) == 0 {
		t.Errorf("item 0: %+v", res[0])
	}
	if res[1].Err == nil {
		t.Error("item 1: the malformed query should carry its own error")
	}
	if res[2].Err != nil || len(res[2].Result) == 0 {
		t.Errorf("item 2: one bad query must not fail its siblings: %+v", res[2])
	}
	if svc.SearchBatch(context.Background(), nil) != nil {
		t.Error("a nil batch should stay nil")
	}
}

// --- AP2: the batch metadata read -------------------------------------------

func TestService_ObjectMetadataBatchAlignsAndMatchesTheSingleRead(t *testing.T) {
	svc, _ := newSearchSvc(t)
	ctx := context.Background()
	ids := []string{
		"account:acme/document:1",
		"account:acme/document:404", // absent
		"not a valid identity",
		"account:acme/document:3",
	}
	res, err := svc.ObjectMetadataBatch(ctx, ids)
	if err != nil {
		t.Fatalf("ObjectMetadataBatch: %v", err)
	}
	if len(res) != len(ids) {
		t.Fatalf("batch returned %d items for %d ids", len(res), len(ids))
	}

	// Every item that succeeds is byte-for-byte what the single read gives, so
	// the batch is a round-trip saving and never a different answer.
	for _, i := range []int{0, 3} {
		if res[i].Err != nil {
			t.Fatalf("item %d errored: %v", i, res[i].Err)
		}
		single, err := svc.ObjectMetadata(ctx, ids[i])
		if err != nil {
			t.Fatalf("ObjectMetadata(%q): %v", ids[i], err)
		}
		if !reflect.DeepEqual(single, res[i].Result) {
			t.Errorf("item %d: batch %v, single %v", i, res[i].Result, single)
		}
	}
	// One bad id never fails its siblings.
	if got := aerr.CodeOf(res[1].Err); got != aerr.APERTURE_NOT_FOUND {
		t.Errorf("absent object: code = %q, want APERTURE_NOT_FOUND", got)
	}
	if got := aerr.CodeOf(res[2].Err); got != aerr.APERTURE_IDENTITY_INVALID {
		t.Errorf("malformed id: code = %q, want APERTURE_IDENTITY_INVALID", got)
	}
	if res[2].Result != nil {
		t.Error("a failed item must carry the zero result")
	}
}

func TestService_ObjectMetadataBatchNeedsProviders(t *testing.T) {
	// A missing dependency is the deployment's error, reported once — not N
	// per-id failures a caller would have to read as data.
	svc := New(engine.New(memory.New()))
	_, err := svc.ObjectMetadataBatch(context.Background(), []string{"account:acme/document:1"})
	if got := aerr.CodeOf(err); got != aerr.APERTURE_UNIMPLEMENTED {
		t.Fatalf("code = %q, want APERTURE_UNIMPLEMENTED", got)
	}
}

func TestService_ObjectMetadataBatchOnNilStaysNil(t *testing.T) {
	svc, _ := newSearchSvc(t)
	res, err := svc.ObjectMetadataBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("ObjectMetadataBatch(nil): %v", err)
	}
	if res != nil {
		t.Errorf("a nil id list should stay nil, got %v", res)
	}
}
