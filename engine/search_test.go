package engine

import (
	"context"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// The catalogue every search case below is asked of. The labels are deliberately
// the awkward ones: a company and one of its product lines that share a word, a
// competitor that shares none, and an object carrying no label at all.
func searchCatalogue() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		"account:acme/document:01": {"label": "Nike Air Max Collection 2024", "sector": "Footwear"},
		"account:acme/document:02": {"label": "Adidas Originals", "sector": "Footwear"},
		"account:acme/document:03": {"label": "Nike, Inc.", "sector": "Footwear", "aliases": []any{"Nike", "NIKE"}},
		"account:acme/document:04": {"label": "Reebok", "sector": "Apparel"},
		"account:acme/document:05": {"sector": "Footwear"}, // no label at all
	}
}

func (f *fieldsFixture) search(t *testing.T, req SearchRequest) []SearchResult {
	t.Helper()
	if req.Account == "" {
		req.Account = acctAcme
	}
	if req.Principal == "" {
		req.Principal = "alice"
	}
	if req.Action == "" {
		req.Action = "read"
	}
	if req.Pattern == "" {
		req.Pattern = "account:acme/**"
	}
	res, err := f.eng.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("Search(%q): unexpected error: %v", req.Query, err)
	}
	return res
}

func objectsOf(res []SearchResult) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Object
	}
	return out
}

// THE load-bearing property. Search is a ranking over what Enumerate would have
// returned, not a second answer to the same question: an object carved out by a
// deny must not reappear because its name matched well. If this ever fails, the
// ranked shortlist a chat surface renders has become a disclosure channel.
func TestSearchNeverProposesWhatEnumerateWouldWithhold(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	// Carve out the best possible match for "nike" with an equal-specificity deny.
	f.perm("p-lit", scope.StrategyLiteral)
	f.grant("g-deny", "alice", model.EffectDeny, "p-lit", "account:acme/document:03")

	allowed, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**",
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	allowedSet := map[string]struct{}{}
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}
	if _, leaked := allowedSet["account:acme/document:03"]; leaked {
		t.Fatal("fixture is wrong: the deny did not take effect")
	}

	for _, obj := range objectsOf(f.search(t, SearchRequest{Query: "nike"})) {
		if _, ok := allowedSet[obj]; !ok {
			t.Errorf("search returned %q, which Enumerate withholds", obj)
		}
	}
	for _, r := range f.search(t, SearchRequest{Query: "nike"}) {
		if r.Object == "account:acme/document:03" {
			t.Fatal("a denied object was returned because its name matched")
		}
	}
}

func TestSearchRanksTheWholeNameAboveTheProductLine(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	res := f.search(t, SearchRequest{Query: "nike"})
	if len(res) < 2 {
		t.Fatalf("expected at least two hits for 'nike', got %v", objectsOf(res))
	}
	if res[0].Object != "account:acme/document:03" {
		t.Errorf("best match = %q, want the company (document:03); full order %v",
			res[0].Object, objectsOf(res))
	}
	if res[0].Score < res[1].Score {
		t.Error("results are not sorted by score descending")
	}
}

func TestSearchExcludesWhatDoesNotMatch(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	for _, obj := range objectsOf(f.search(t, SearchRequest{Query: "nike"})) {
		switch obj {
		case "account:acme/document:02", "account:acme/document:04", "account:acme/document:05":
			t.Errorf("%q matched 'nike' but shares nothing with it", obj)
		}
	}
}

func TestSearchToleratesATypo(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	res := f.search(t, SearchRequest{Query: "nkie"})
	if len(res) == 0 {
		t.Fatal("a transposed query resolved nothing; the whole point is that a person's typing is not exact")
	}
}

func TestSearchCarriesTheMetadataAndNamesWhatMatched(t *testing.T) {
	// Metadata inline is what keeps a result set from costing one round-trip per
	// id to label, and Field/Value are how a caller explains the match it renders.
	f := allowAllFixture(t, searchCatalogue())
	res := f.search(t, SearchRequest{Query: "nike inc"})
	if len(res) == 0 {
		t.Fatal("no hits")
	}
	top := res[0]
	if top.Metadata == nil {
		t.Fatal("a match carried no metadata")
	}
	if top.Metadata["sector"] != "Footwear" {
		t.Errorf("metadata = %v, want the object's full bag", top.Metadata)
	}
	if top.Field == "" || top.Value == "" {
		t.Errorf("match reported no field/value: %+v", top)
	}
	if top.Field != "label" {
		t.Errorf("matched field = %q, want %q", top.Field, "label")
	}
}

func TestSearchRestrictsToNamedFields(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	// "Footwear" lives in sector, never in label.
	if res := f.search(t, SearchRequest{Query: "footwear", MatchFields: []string{"label"}}); len(res) != 0 {
		t.Errorf("the field restriction was ignored: %v", objectsOf(res))
	}
	if res := f.search(t, SearchRequest{Query: "footwear", MatchFields: []string{"sector"}}); len(res) == 0 {
		t.Error("the named field should have matched")
	}
}

func TestSearchMatchesAnArrayElement(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	res := f.search(t, SearchRequest{Query: "nike", MatchFields: []string{"aliases"}})
	if len(res) != 1 || res[0].Object != "account:acme/document:03" {
		t.Fatalf("aliases search = %v, want just document:03", objectsOf(res))
	}
	if res[0].Score != 1 {
		t.Errorf("score = %v, want 1 — an element equal to the query is an exact match", res[0].Score)
	}
}

func TestSearchComposesWithAFieldPredicate(t *testing.T) {
	// "The Nike in footwear" is one call: the predicate narrows, the query ranks.
	f := allowAllFixture(t, searchCatalogue())
	res := f.search(t, SearchRequest{Query: "nike", Fields: map[string]any{"sector": "Apparel"}})
	if len(res) != 0 {
		t.Errorf("the predicate did not narrow the search: %v", objectsOf(res))
	}
	res = f.search(t, SearchRequest{Query: "nike", Fields: map[string]any{"sector": "Footwear"}})
	if len(res) == 0 {
		t.Error("the predicate excluded objects it should have kept")
	}
}

func TestSearchRanksBeforeItTruncates(t *testing.T) {
	// Limit takes the top of a FINISHED ranking. If the scan stopped at Limit
	// instead, this would return document:01 — the first allowed object that
	// matched at all — rather than the best match, and the ranking would be
	// decoration over an arbitrary prefix.
	f := allowAllFixture(t, searchCatalogue())
	res := f.search(t, SearchRequest{Query: "nike", Limit: 1})
	if len(res) != 1 {
		t.Fatalf("Limit 1 returned %d results", len(res))
	}
	if res[0].Object != "account:acme/document:03" {
		t.Errorf("Limit 1 returned %q, want the best match document:03 — not the first one found",
			res[0].Object)
	}
}

func TestSearchIsStableAcrossCalls(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	first := objectsOf(f.search(t, SearchRequest{Query: "nike"}))
	for i := 0; i < 20; i++ {
		got := objectsOf(f.search(t, SearchRequest{Query: "nike"}))
		if len(got) != len(first) {
			t.Fatalf("unstable length: %v then %v", first, got)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("unstable order: %v then %v", first, got)
			}
		}
	}
}

func TestSearchHonoursTheScoreFloor(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	loose := f.search(t, SearchRequest{Query: "nkie"})
	strict := f.search(t, SearchRequest{Query: "nkie", MinScore: 0.99})
	if len(loose) == 0 {
		t.Fatal("fixture is wrong: the fuzzy query matched nothing at the default floor")
	}
	if len(strict) != 0 {
		t.Errorf("a floor of 0.99 admitted approximate matches: %v", objectsOf(strict))
	}
}

func TestSearchRejectsAQuestionItCannotAnswer(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	for name, req := range map[string]SearchRequest{
		"no account":   {Principal: "alice", Action: "read", Pattern: "account:acme/**", Query: "nike"},
		"no principal": {Account: acctAcme, Action: "read", Pattern: "account:acme/**", Query: "nike"},
		"no action":    {Account: acctAcme, Principal: "alice", Pattern: "account:acme/**", Query: "nike"},
		"no pattern":   {Account: acctAcme, Principal: "alice", Action: "read", Query: "nike"},
		// An empty query is rejected rather than read as "everything": the set it
		// would return unranked is the bulk disclosure this surface replaces.
		"no query": {Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**"},
		"impossible floor": {Account: acctAcme, Principal: "alice", Action: "read",
			Pattern: "account:acme/**", Query: "nike", MinScore: 1.5},
	} {
		_, err := f.eng.Search(context.Background(), req)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
			t.Errorf("%s: code = %q, want APERTURE_INVALID_INPUT", name, got)
		}
	}
}

func TestSearchWithoutAMetadataSourceIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	// An empty result reads as "you may see nothing" and would hide the
	// misconfiguration behind a plausible answer — the same asymmetry the Fields
	// predicate is built on.
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))
	reg := provider.NewRegistry()
	reg.MustRegister("document", metaProvider{md: searchCatalogue()})
	eng := New(store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg}))

	_, err := eng.Search(ctx, SearchRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Query: "nike",
	})
	if got := aerr.CodeOf(err); got != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %q, want APERTURE_PROVIDER_UNREGISTERED (err: %v)", got, err)
	}
}

func TestSearchByANonMemberFindsNothingAndSaysNothing(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	mustSeed(t, f.store.PutPrincipal(context.Background(), model.Principal{
		ID: "mallory", Kind: model.PrincipalUser, Identity: "user:mallory",
	}))
	res, err := f.eng.Search(context.Background(), SearchRequest{
		Account: acctAcme, Principal: "mallory", Action: "read",
		Pattern: "account:acme/**", Query: "nike",
	})
	if err != nil {
		t.Fatalf("a non-member search errored: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("a non-member saw %v", objectsOf(res))
	}
}

func TestSearchSkipsAnObjectWithNoMetadata(t *testing.T) {
	// document:05 carries a bag with no label; an object absent from the provider
	// entirely carries none at all. Neither is a failure — there is simply
	// nothing there to name.
	f := allowAllFixture(t, searchCatalogue())
	res, err := f.eng.Search(context.Background(), SearchRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Query: "nike",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.Object == "account:acme/document:05" {
			t.Error("an object with no matching metadata was returned")
		}
	}
}

func TestSearchBatchAlignsWithItsQueries(t *testing.T) {
	f := allowAllFixture(t, searchCatalogue())
	reqs := []SearchRequest{
		{Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**", Query: "nike"},
		{Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**"}, // invalid: no query
		{Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**", Query: "adidas"},
	}
	res := f.eng.SearchBatch(context.Background(), reqs)
	if len(res) != len(reqs) {
		t.Fatalf("batch returned %d items for %d queries", len(res), len(reqs))
	}
	if res[0].Err != nil || len(res[0].Result) == 0 {
		t.Errorf("item 0: %+v", res[0])
	}
	if res[1].Err == nil {
		t.Error("item 1: a malformed query should carry its own error")
	}
	if res[2].Err != nil || len(res[2].Result) == 0 {
		t.Errorf("item 2: one bad query must not fail its siblings: %+v", res[2])
	}
	if f.eng.SearchBatch(context.Background(), nil) != nil {
		t.Error("a nil batch should stay nil")
	}
}

// --- Composition with the reference restriction ------------------------------

// searchBrands ranks the brands alice may read, restricted by edges — the search
// twin of refFixture.brands.
func (f *refFixture) searchBrands(ctx context.Context, query string, edges ...ReferenceEdge) ([]SearchResult, error) {
	f.t.Helper()
	return f.eng.Search(ctx, SearchRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/brand:*", Query: query, References: edges,
	})
}

func TestSearchComposesWithAReferenceEdge(t *testing.T) {
	// "The brand in dataset x whose region is eu" is one call: the edge restricts
	// the candidate set, the query ranks what is left. brand:3 is also `us` but
	// dataset x never names it.
	f := newRefFixture(t)
	ctx := context.Background()

	res, err := f.searchBrands(ctx, "us", inDatasetX())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.Object == "account:acme/brand:3" {
			t.Error("the reference restriction was ignored: brand:3 is not in dataset x")
		}
	}
	if len(res) != 1 || res[0].Object != "account:acme/brand:1" {
		t.Fatalf("results = %v, want just brand:1", objectsOf(res))
	}
}

func TestSearchThroughAnInvisibleHolderIsEmptyNotAnError(t *testing.T) {
	// The same fail-closed rule Enumerate carries: a holder the principal may not
	// see is indistinguishable from one that contains nothing visible. Relaxing it
	// here would make the search surface the place a holder's existence leaks.
	f := newRefFixture(t)
	ctx := context.Background()
	res, err := f.eng.Search(ctx, SearchRequest{
		Account: acctAcme, Principal: "bob", Action: "read",
		Pattern: "account:acme/brand:*", Query: "us", References: []ReferenceEdge{inDatasetX()},
	})
	if err != nil {
		t.Fatalf("expected an empty result, got error: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("a principal with no grants saw %v", objectsOf(res))
	}
}

func TestSearchAsDelegatesWhenImpersonationIsInert(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	direct, err := f.searchBrands(ctx, "us")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	via, err := f.eng.SearchAs(ctx, SearchRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/brand:*", Query: "us",
	}, ImpersonationContext{})
	if err != nil {
		t.Fatalf("SearchAs: %v", err)
	}
	if len(via) != len(direct) {
		t.Fatalf("SearchAs under an inert context = %v, want Search's %v",
			objectsOf(via), objectsOf(direct))
	}
	for i := range direct {
		if via[i].Object != direct[i].Object {
			t.Fatalf("SearchAs = %v, want %v", objectsOf(via), objectsOf(direct))
		}
	}
}
