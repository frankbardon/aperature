package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"

	"google.golang.org/protobuf/types/known/structpb"
)

// Search, end to end over Twirp/HTTP.
//
// These go through the JSON client rather than the handler in-process for the
// same reason the filter tests do: a match carries the object's METADATA back,
// encoded as map<string, google.protobuf.Value>, and only a real round trip
// proves the string/number/bool distinction survives serialization on the way
// out as well as on the way in.

// searchFixture is a live Twirp server over the production wiring: one registry
// as scope lister, metadata source and provider registry.
type searchFixture struct {
	t   *testing.T
	cli rpc.ApertureService
}

// searchCatalogue carries the names a person would actually type: a company and
// one of its product lines sharing a word, a competitor sharing none, an object
// whose name lives only in an alias list, and one with a numeric field that must
// never be text-matched.
func searchCatalogue() []provider.Object {
	return []provider.Object{
		{ID: identity.MustParse("account:acme/document:1"), Metadata: provider.Metadata{
			"label": "Nike Air Max Collection", "sector": "Footwear", "seats": int64(5)}},
		{ID: identity.MustParse("account:acme/document:2"), Metadata: provider.Metadata{
			"label": "Adidas Originals", "sector": "Footwear", "seats": int64(9)}},
		{ID: identity.MustParse("account:acme/document:3"), Metadata: provider.Metadata{
			"label": "Nike, Inc.", "sector": "Footwear", "seats": int64(5),
			"aliases": []any{"Nike", "NIKE Inc"}}},
	}
}

func newSearchFixture(t *testing.T) *searchFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must(t, store.Setup(ctx))
	must(t, store.PutAccount(ctx, model.Account{ID: acct, Name: "Acme"}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: acct}))
	must(t, store.PutPrincipal(ctx, model.Principal{ID: "bob", Kind: model.PrincipalUser, Identity: "user:bob"}))
	must(t, store.PutMembership(ctx, model.Membership{PrincipalID: "bob", AccountID: acct}))
	must(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	must(t, store.PutPermission(ctx, model.Permission{
		ID: "p-read", ObjectType: "document", Action: "read", ScopeStrategy: scope.StrategyImplicit,
	}))
	must(t, store.PutGrant(ctx, model.Grant{
		ID: "g-alice", AccountID: acct,
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-read", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	sp, err := provider.NewStatic(searchCatalogue())
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("document", sp)

	eng := engine.New(store,
		engine.WithScopeResolution(scope.DefaultRegistry(), engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
	)
	svc := service.New(eng, service.WithProviders(reg), service.WithStorage(store))
	srv := httptest.NewServer(server.New(svc))
	t.Cleanup(srv.Close)

	return &searchFixture{t: t, cli: rpc.NewApertureServiceJSONClient(srv.URL, http.DefaultClient)}
}

func (f *searchFixture) search(req *rpc.SearchRequest) ([]*rpc.SearchMatch, error) {
	f.t.Helper()
	if req.Account == "" {
		req.Account = acct
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
	res, err := f.cli.Search(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return res.GetMatches(), nil
}

// mustValues builds the Value map a non-Go client would send, so the predicate
// reaches the server through the same encoding a real caller uses.
func mustValues(t *testing.T, in map[string]any) map[string]*structpb.Value {
	t.Helper()
	out := make(map[string]*structpb.Value, len(in))
	for k, v := range in {
		val, err := structpb.NewValue(v)
		if err != nil {
			t.Fatalf("structpb.NewValue(%v): %v", v, err)
		}
		out[k] = val
	}
	return out
}

func wireObjects(in []*rpc.SearchMatch) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.GetObject()
	}
	return out
}

func TestWireSearchRanksBestFirst(t *testing.T) {
	f := newSearchFixture(t)
	got, err := f.search(&rpc.SearchRequest{Query: "nike"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("search 'nike' = %v, want the two Nike documents", wireObjects(got))
	}
	// The ranking IS the payload — a surface that reordered it would discard the
	// whole answer while still returning the right set.
	if got[0].GetObject() != "account:acme/document:3" {
		t.Errorf("best match = %q, want document:3; order %v", got[0].GetObject(), wireObjects(got))
	}
	if got[0].GetScore() < got[1].GetScore() {
		t.Errorf("scores are not descending: %v then %v", got[0].GetScore(), got[1].GetScore())
	}
}

// The property this surface must hold as much as every other one: a search can
// only ever propose what an enumeration would. Asserted here, over the wire,
// rather than trusted from the layer below — a relaxation in one surface is a
// silent disclosure channel.
func TestWireSearchNeverProposesWhatEnumerateWithholds(t *testing.T) {
	f := newSearchFixture(t)
	res, err := f.cli.Enumerate(context.Background(), &rpc.EnumerateRequest{
		Account: acct, Principal: "alice", Action: "read", Pattern: "account:acme/**",
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	allowed := map[string]struct{}{}
	for _, id := range res.GetObjectIds() {
		allowed[id] = struct{}{}
	}
	for _, q := range []string{"nike", "adidas", "footwear", "a"} {
		got, err := f.search(&rpc.SearchRequest{Query: q})
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		for _, m := range got {
			if _, ok := allowed[m.GetObject()]; !ok {
				t.Errorf("Search(%q) proposed %q, which Enumerate withholds", q, m.GetObject())
			}
		}
	}
}

func TestWireSearchByAPrincipalWithNoGrantsIsEmptyNotAnError(t *testing.T) {
	f := newSearchFixture(t)
	got, err := f.search(&rpc.SearchRequest{Principal: "bob", Query: "nike"})
	if err != nil {
		t.Fatalf("expected an empty result, got error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a principal with no grants saw %v", wireObjects(got))
	}
}

// The whole reason metadata rides back on a match: labelling a result set must
// not cost a round-trip per id. And it must survive the Value encoding with its
// types intact, which is why this asserts a number as well as a string.
func TestWireSearchCarriesMetadataWithItsTypesIntact(t *testing.T) {
	// document:2 carries no aliases, so the matching field is unambiguous — the
	// point here is the ENCODING, not the tiebreak (see below for that).
	f := newSearchFixture(t)
	got, err := f.search(&rpc.SearchRequest{Query: "adidas originals"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no hits")
	}
	top := got[0]
	md := top.GetMetadata()
	if md == nil {
		t.Fatal("a match came back with no metadata")
	}
	if md["label"].GetStringValue() != "Adidas Originals" {
		t.Errorf("label = %v, want the object's own label", md["label"])
	}
	if md["seats"].GetNumberValue() != 9 {
		t.Errorf("seats = %v, want the number 9 — a number must not arrive as a string", md["seats"])
	}
	// Field/Value are how a client explains the match it renders: a hit on an
	// alias and a hit on a display name are indistinguishable from the id alone.
	if top.GetField() != "label" || top.GetValue() != "Adidas Originals" {
		t.Errorf("match provenance = %q/%q, want label/%q", top.GetField(), top.GetValue(), "Adidas Originals")
	}
}

// Two fields of one object can match a query equally well — "Nike, Inc." and the
// alias "NIKE Inc" both normalise to exactly the query — and the winner has to
// be the same on every call. Map iteration order is random, so without the
// lexical tiebreak a client would see the reported Field flip between identical
// requests and have no way to diagnose it.
func TestWireSearchBreaksAFieldTieTheSameWayEveryTime(t *testing.T) {
	f := newSearchFixture(t)
	first, err := f.search(&rpc.SearchRequest{Query: "nike inc"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("no hits")
	}
	if first[0].GetScore() != 1 {
		t.Errorf("score = %v, want an exact 1 — both fields normalise to the query", first[0].GetScore())
	}
	if first[0].GetField() != "aliases" {
		t.Errorf("tie broken to %q, want the lexically first field %q", first[0].GetField(), "aliases")
	}
	for i := 0; i < 10; i++ {
		got, err := f.search(&rpc.SearchRequest{Query: "nike inc"})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if got[0].GetField() != first[0].GetField() || got[0].GetObject() != first[0].GetObject() {
			t.Fatalf("unstable result: %q/%q then %q/%q",
				first[0].GetObject(), first[0].GetField(), got[0].GetObject(), got[0].GetField())
		}
	}
}

func TestWireSearchMatchesAnAliasAndSaysSo(t *testing.T) {
	f := newSearchFixture(t)
	got, err := f.search(&rpc.SearchRequest{Query: "nike", MatchFields: []string{"aliases"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].GetObject() != "account:acme/document:3" {
		t.Fatalf("alias search = %v, want just document:3", wireObjects(got))
	}
	if got[0].GetField() != "aliases" {
		t.Errorf("matched field = %q, want aliases", got[0].GetField())
	}
}

func TestWireSearchComposesWithAPredicateAndRanksBeforeTruncating(t *testing.T) {
	f := newSearchFixture(t)
	// The predicate narrows; the query ranks what is left.
	got, err := f.search(&rpc.SearchRequest{
		Query:  "nike",
		Fields: mustValues(t, map[string]any{"seats": float64(9)}),
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the predicate did not narrow the search: %v", wireObjects(got))
	}
	// Limit takes the top of a FINISHED ranking. document:1 sorts first by id and
	// matches too, so a scan bounded by limit would return it instead.
	got, err = f.search(&rpc.SearchRequest{Query: "nike", Limit: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].GetObject() != "account:acme/document:3" {
		t.Errorf("limit 1 = %v, want the best match document:3 — not the first found", wireObjects(got))
	}
}

// A wire failure must carry the Aperture code, so a client can tell a malformed
// question from a misconfigured deployment rather than reading an empty result
// as "nothing by that name".
func TestWireSearchFailuresCarryTheirCode(t *testing.T) {
	f := newSearchFixture(t)
	if _, err := f.search(&rpc.SearchRequest{}); err == nil {
		t.Error("an empty query should be refused, not read as 'match everything'")
	} else {
		assertTwirpCode(t, err, aerr.APERTURE_INVALID_INPUT)
	}
	if _, err := f.search(&rpc.SearchRequest{Query: "nike", MinScore: 1.5}); err == nil {
		t.Error("an unreachable score floor should be refused")
	} else {
		assertTwirpCode(t, err, aerr.APERTURE_INVALID_INPUT)
	}
}

func TestWireSearchBatchAlignsWithItsQueries(t *testing.T) {
	f := newSearchFixture(t)
	base := func(q string) *rpc.SearchRequest {
		return &rpc.SearchRequest{
			Account: acct, Principal: "alice", Action: "read",
			Pattern: "account:acme/**", Query: q,
		}
	}
	res, err := f.cli.SearchBatch(context.Background(), &rpc.SearchBatchRequest{
		Queries: []*rpc.SearchRequest{base("nike"), base(""), base("adidas")},
	})
	if err != nil {
		t.Fatalf("SearchBatch: %v", err)
	}
	items := res.GetResults()
	if len(items) != 3 {
		t.Fatalf("batch returned %d items for 3 queries", len(items))
	}
	if items[0].GetErrorCode() != "" || len(items[0].GetMatches()) == 0 {
		t.Errorf("item 0: %+v", items[0])
	}
	// A query that never ran must not come back as "no matches" — on a search
	// that reads as "nothing by that name", which is plausible and wrong.
	if items[1].GetErrorCode() != string(aerr.APERTURE_INVALID_INPUT) {
		t.Errorf("item 1: error code = %q, want APERTURE_INVALID_INPUT", items[1].GetErrorCode())
	}
	if len(items[1].GetMatches()) != 0 {
		t.Error("item 1: a failed item must carry no matches")
	}
	if items[2].GetErrorCode() != "" || len(items[2].GetMatches()) == 0 {
		t.Errorf("item 2: one bad query must not fail its siblings: %+v", items[2])
	}
}
