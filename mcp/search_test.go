package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// An agent is the caller that most needs name→id resolution — it receives a
// question in a person's words — and the one where an over-broad result set does
// the most damage. These tests cover both halves: the reflected schema an MCP
// client reads to decide what it may send, and the handler's behaviour.

// searchService wires the same graph filterService does, with datasets that
// carry NAMES: a company and one of its product lines sharing a word, and a
// competitor sharing none.
//
//	dataset:a  "Nike Air Max Collection"  sector Footwear
//	dataset:b  "Adidas Originals"         sector Footwear
//	dataset:c  "Nike, Inc."               sector Footwear
func searchService(t *testing.T) *service.Service {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(store.Setup(ctx))
	must(store.PutAccount(ctx, model.Account{ID: "acme", Name: "Acme"}))
	must(store.PutPrincipal(ctx, model.Principal{ID: "alice", Kind: model.PrincipalUser, Identity: "user:alice"}))
	must(store.PutMembership(ctx, model.Membership{PrincipalID: "alice", AccountID: "acme"}))
	must(store.PutObjectType(ctx, model.ObjectType{Name: "dataset", Actions: []string{"list"}}))
	must(store.PutPermission(ctx, model.Permission{
		ID: "p-list", ObjectType: "dataset", Action: "list", ScopeStrategy: "implicit",
	}))
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-list", AccountID: "acme", Subject: model.Subject{Kind: model.SubjectPrincipal, ID: "alice"},
		PermissionID: "p-list", Object: "account:acme/**", Effect: model.EffectAllow,
	}))

	static, err := provider.NewStatic([]provider.Object{
		{ID: identity.MustParse("account:acme/dataset:a"), Metadata: provider.Metadata{
			"label": "Nike Air Max Collection", "sector": "Footwear"}},
		{ID: identity.MustParse("account:acme/dataset:b"), Metadata: provider.Metadata{
			"label": "Adidas Originals", "sector": "Footwear"}},
		{ID: identity.MustParse("account:acme/dataset:c"), Metadata: provider.Metadata{
			"label": "Nike, Inc.", "sector": "Footwear"}},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("dataset", static, provider.WithTTL(0))

	eng := engine.New(store,
		engine.WithScopeResolution(nil, engine.ScopeDeps{Lister: reg}),
		engine.WithMetadata(reg),
	)
	return service.New(eng, service.WithProviders(reg))
}

// searchSchema digs out the properties of the search-query object, which the
// batch tool nests one level down inside its queries array.
func searchSchema(t *testing.T, name string) (map[string]any, []string) {
	t.Helper()
	ts, ok := SchemaFor(name)
	if !ok {
		t.Fatalf("no schema registered for %s", name)
	}
	var schema map[string]any
	if err := json.Unmarshal(ts.InputSchema, &schema); err != nil {
		t.Fatalf("%s input schema is not JSON: %v", name, err)
	}
	obj := schema
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s input schema has no properties", name)
	}
	if queries, ok := props["queries"].(map[string]any); ok {
		items, ok := queries["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s queries has no items schema", name)
		}
		obj = items
		props, ok = items["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s queries items have no properties", name)
		}
	}
	required := []string{}
	if raw, ok := obj["required"].([]any); ok {
		for _, r := range raw {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	return props, required
}

// The query text is REQUIRED and the narrowing arguments are OPTIONAL. Both
// halves are load-bearing. An optional query would let an agent ask for a
// subject's whole entitled set through a tool whose result shape invites
// rendering all of it; a required MatchFields or Fields would make the common
// call — "find me anything called nike" — unrepresentable.
func TestSearchToolSchemaRequiresTheQueryAndNothingElseNew(t *testing.T) {
	for _, name := range []string{toolmeta.ToolSearch, toolmeta.ToolSearchBatch} {
		props, required := searchSchema(t, name)

		if !slices.Contains(required, "Query") {
			t.Errorf("%s: Query is not required; required = %v", name, required)
		}
		for _, optional := range []string{"MatchFields", "Fields", "References", "MinScore", "Limit"} {
			if _, ok := props[optional]; !ok {
				t.Errorf("%s: input schema has no %s property", name, optional)
			}
			if slices.Contains(required, optional) {
				t.Errorf("%s: %s is marked required; an agent could not ask without it", name, optional)
			}
		}
	}
}

// An agent has no source of truth but the description, so the two properties it
// could most plausibly get wrong have to be stated there: that a score is not a
// permission, and that the result is bounded by what the subject may see.
func TestSearchToolDescriptionStatesWhatAScoreIsNot(t *testing.T) {
	desc := toolmeta.DescSearch
	for _, want := range []string{"never a permission", "aperture_check would allow", "subset"} {
		if !strings.Contains(desc, want) {
			t.Errorf("aperture_search description does not mention %q:\n%s", want, desc)
		}
	}
}

func TestSearchToolRanksAndCarriesMetadata(t *testing.T) {
	svc := searchService(t)
	invoke := invokerFor(t, toolmeta.ToolSearch)

	raw, err := json.Marshal(service.SearchQuery{
		Account: "acme", Principal: "alice", Action: "list",
		Pattern: "account:acme/**", Query: "nike",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := invoke(context.Background(), svc, raw)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	res, ok := out.(SearchOut)
	if !ok {
		t.Fatalf("tool returned %T, want SearchOut", out)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("matches = %+v, want the two Nike datasets", res.Matches)
	}
	if res.Matches[0].Object != "account:acme/dataset:c" {
		t.Errorf("best match = %q, want dataset:c (Nike, Inc.)", res.Matches[0].Object)
	}
	if res.Matches[0].Metadata == nil {
		t.Error("a match carried no metadata; an agent would have to fetch labels one id at a time")
	}
	if res.Matches[0].Field == "" {
		t.Error("a match did not name the field it came from")
	}
}

func TestSearchToolNeverProposesWhatEnumerateWithholds(t *testing.T) {
	svc := searchService(t)
	ctx := context.Background()

	allowed, err := svc.Enumerate(ctx, service.EnumerateQuery{
		Account: "acme", Principal: "alice", Action: "list", Pattern: "account:acme/**",
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	invoke := invokerFor(t, toolmeta.ToolSearch)
	raw, _ := json.Marshal(service.SearchQuery{
		Account: "acme", Principal: "alice", Action: "list",
		Pattern: "account:acme/**", Query: "nike",
	})
	out, err := invoke(ctx, svc, raw)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	for _, m := range out.(SearchOut).Matches {
		if !slices.Contains(allowed, m.Object) {
			t.Errorf("the tool proposed %q, which Enumerate withholds", m.Object)
		}
	}
}

func TestSearchBatchToolAlignsWithItsQueries(t *testing.T) {
	svc := searchService(t)
	invoke := invokerFor(t, toolmeta.ToolSearchBatch)
	raw, _ := json.Marshal(SearchBatchIn{Queries: []service.SearchQuery{
		{Account: "acme", Principal: "alice", Action: "list", Pattern: "account:acme/**", Query: "nike"},
		{Account: "acme", Principal: "alice", Action: "list", Pattern: "account:acme/**"}, // no query
	}})
	out, err := invoke(context.Background(), svc, raw)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	res := out.(SearchBatchOut)
	if len(res.Results) != 2 {
		t.Fatalf("batch returned %d items for 2 queries", len(res.Results))
	}
	if res.Results[0].Error != "" || len(res.Results[0].Result) == 0 {
		t.Errorf("item 0: %+v", res.Results[0])
	}
	if res.Results[1].Error == "" {
		t.Error("item 1: a malformed query should carry its own error string")
	}
	// A failed item still marshals an empty list rather than null, so an agent
	// reading results[i].result never has to special-case a missing field.
	if res.Results[1].Result == nil {
		t.Error("item 1: a failed item should carry an empty list, not nil")
	}
}
