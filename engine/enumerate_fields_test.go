package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/storage/memory"
)

// fieldsFixture wires an engine whose scope lister AND metadata source are the
// same *provider.Registry — the production wiring, where a candidate's metadata
// is served from the per-type cache the enumeration already warmed.
type fieldsFixture struct {
	t     *testing.T
	store *memory.Store
	reg   *provider.Registry
	eng   *Engine
}

func newFieldsFixture(t *testing.T, objects map[string]provider.Metadata) *fieldsFixture {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustSeed(t, store.PutObjectType(ctx, model.ObjectType{Name: "document", Actions: []string{"read"}}))
	mustSeed(t, store.PutAccount(ctx, model.Account{ID: acctAcme, Name: acctAcme}))

	// metaProvider (explain_test.go) already serves fixed metadata per identity;
	// the Fields predicate is the engine's job, so a plain provider is exactly
	// the right double here.
	reg := provider.NewRegistry()
	reg.MustRegister("document", metaProvider{md: objects})

	eng := New(store,
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: reg}),
		WithMetadata(reg),
	)
	return &fieldsFixture{t: t, store: store, reg: reg, eng: eng}
}

func (f *fieldsFixture) principal(id string) {
	f.t.Helper()
	mustSeed(f.t, f.store.PutPrincipal(context.Background(), model.Principal{
		ID: id, Kind: model.PrincipalUser, Identity: "user:" + id,
	}))
	mustSeed(f.t, f.store.PutMembership(context.Background(), model.Membership{
		PrincipalID: id, AccountID: acctAcme,
	}))
}

func (f *fieldsFixture) perm(id, strategy string) {
	f.t.Helper()
	mustSeed(f.t, f.store.PutPermission(context.Background(), model.Permission{
		ID: id, ObjectType: "document", Action: "read", ScopeStrategy: strategy,
	}))
}

func (f *fieldsFixture) grant(id, principal string, effect model.Effect, permID, object string) {
	f.t.Helper()
	mustSeed(f.t, f.store.PutGrant(context.Background(), model.Grant{
		ID: id, AccountID: acctAcme, Subject: model.Subject{Kind: model.SubjectPrincipal, ID: principal},
		PermissionID: permID, Object: object, Effect: effect,
	}))
}

// enumerate runs a Fields-filtered enumeration for alice over the whole account.
func (f *fieldsFixture) enumerate(fields map[string]any, limit int) []string {
	f.t.Helper()
	ids, err := f.eng.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Fields: fields, Limit: limit,
	})
	if err != nil {
		f.t.Fatalf("Enumerate(fields=%v): unexpected error: %v", fields, err)
	}
	return ids
}

// allowAllFixture is the common shape: alice may read every document in acme.
func allowAllFixture(t *testing.T, objects map[string]provider.Metadata) *fieldsFixture {
	t.Helper()
	f := newFieldsFixture(t, objects)
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.grant("g-all", "alice", model.EffectAllow, "p-impl", "account:acme/**")
	return f
}

// The catalogue the epic's motivating question is asked of: "which datasets
// contain brand Y?" — current_brands is an ordinary metadata array.
func brandCatalogue() map[string]provider.Metadata {
	return map[string]provider.Metadata{
		"account:acme/document:1": {
			"tier":           "premium",
			"seats":          int64(5),
			"current_brands": []any{"brand:X", "brand:Y"},
		},
		"account:acme/document:2": {
			"tier":           "basic",
			"seats":          int64(5),
			"current_brands": []any{"brand:Y"},
		},
		"account:acme/document:3": {
			"tier":           "premium",
			"seats":          int64(9),
			"current_brands": []any{"brand:Z"},
		},
		// No current_brands and no tier at all: the absent-field case.
		"account:acme/document:4": {"seats": int64(5)},
	}
}

// --- Acceptance: an absent/empty Fields map is exactly today's behaviour ---

func TestEnumerateFields_NilAndEmptyFilterNothing(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	all := []string{
		"account:acme/document:1", "account:acme/document:2",
		"account:acme/document:3", "account:acme/document:4",
	}
	if got := f.enumerate(nil, 0); !sameSet(got, all) {
		t.Fatalf("nil Fields = %v, want the unfiltered set %v", got, all)
	}
	if got := f.enumerate(map[string]any{}, 0); !sameSet(got, all) {
		t.Fatalf("empty Fields = %v, want the unfiltered set %v", got, all)
	}
}

// An unfiltered enumeration must not even need a metadata source, so an engine
// wired without WithMetadata keeps working exactly as before.
func TestEnumerateFields_UnfilteredNeedsNoMetadataSource(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	bare := New(f.store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: f.reg}))

	ids, err := bare.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**",
	})
	if err != nil {
		t.Fatalf("unfiltered enumerate without a metadata source: %v", err)
	}
	if len(ids) != 4 {
		t.Fatalf("unfiltered enumerate = %v, want all 4 documents", ids)
	}
}

// --- Acceptance: the provider.Filter Fields contract, verbatim ---

// A collection field matches by MEMBERSHIP: this is the "which datasets contain
// brand Y?" question the epic exists for.
func TestEnumerateFields_CollectionMatchesByMembership(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	got := f.enumerate(map[string]any{"current_brands": "brand:Y"}, 0)
	want := []string{"account:acme/document:1", "account:acme/document:2"}
	if !sameSet(got, want) {
		t.Fatalf("membership filter = %v, want %v", got, want)
	}
}

// Multiple keys AND together.
func TestEnumerateFields_MultipleKeysAND(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	got := f.enumerate(map[string]any{"current_brands": "brand:Y", "tier": "premium"}, 0)
	want := []string{"account:acme/document:1"}
	if !sameSet(got, want) {
		t.Fatalf("ANDed filter = %v, want %v (document:2 is brand:Y but basic)", got, want)
	}
}

// A field ABSENT from an object never matches — not even against a nil want.
func TestEnumerateFields_AbsentFieldNeverMatches(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())

	if got := f.enumerate(map[string]any{"tier": nil}, 0); len(got) != 0 {
		t.Fatalf("nil want = %v, want empty (document:4 has no tier, so it must not match)", got)
	}
	if got := f.enumerate(map[string]any{"unknown_field": "anything"}, 0); len(got) != 0 {
		t.Fatalf("unknown field = %v, want empty", got)
	}
	// Positive control: the objects that DO hold the field still match.
	got := f.enumerate(map[string]any{"tier": "premium"}, 0)
	want := []string{"account:acme/document:1", "account:acme/document:3"}
	if !sameSet(got, want) {
		t.Fatalf("present-field filter = %v, want %v", got, want)
	}
}

// Comparison is TYPED, not a string rendering: a float64 want matches an int64
// metadata value, and a string want never matches a number. This is the property
// that keeps Enumerate's answer and Check's rule answer the same.
func TestEnumerateFields_TypedComparison(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	numeric := []string{
		"account:acme/document:1", "account:acme/document:2", "account:acme/document:4",
	}
	if got := f.enumerate(map[string]any{"seats": float64(5)}, 0); !sameSet(got, numeric) {
		t.Fatalf("float64(5) want = %v, want %v (int64(5) is the same number)", got, numeric)
	}
	if got := f.enumerate(map[string]any{"seats": int(5)}, 0); !sameSet(got, numeric) {
		t.Fatalf("int(5) want = %v, want %v", got, numeric)
	}
	if got := f.enumerate(map[string]any{"seats": "5"}, 0); len(got) != 0 {
		t.Fatalf(`"5" want = %v, want empty (a string never equals a number)`, got)
	}
}

// --- Acceptance: filter BEFORE limit ---

// The ordering rule, proven the only way it can be: many non-matching candidates
// sorted AHEAD of more matching candidates than Limit admits. Filtering after
// truncation would return the empty set here.
func TestEnumerateFields_FilterAppliedBeforeLimit(t *testing.T) {
	objects := make(map[string]provider.Metadata)
	for i := 1; i <= 20; i++ {
		// "a.." sorts before "z..", so every non-match is decided first.
		objects[fmt.Sprintf("account:acme/document:a%02d", i)] = provider.Metadata{
			"current_brands": []any{"brand:X"},
		}
	}
	matching := make([]string, 0, 5)
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("account:acme/document:z%02d", i)
		objects[id] = provider.Metadata{"current_brands": []any{"brand:Y"}}
		matching = append(matching, id)
	}
	f := allowAllFixture(t, objects)

	got := f.enumerate(map[string]any{"current_brands": "brand:Y"}, 3)
	if len(got) != 3 {
		t.Fatalf("limited filtered enumerate = %v (%d ids), want 3 matching ids — "+
			"the filter must run before Limit truncates", got, len(got))
	}
	for _, id := range got {
		found := false
		for _, m := range matching {
			if m == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("limited filtered enumerate returned a non-matching id %q (%v)", id, got)
		}
	}

	// Unlimited, the same predicate yields every match — the limit truncated the
	// filtered set, not the other way round.
	if all := f.enumerate(map[string]any{"current_brands": "brand:Y"}, 0); !sameSet(all, matching) {
		t.Fatalf("unlimited filtered enumerate = %v, want %v", all, matching)
	}
}

// Limit <= 0 still means DefaultEnumerateLimit, filtered or not.
func TestEnumerateFields_DefaultLimitUnchanged(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	for _, limit := range []int{0, -1, DefaultEnumerateLimit + 1} {
		if got := f.eng.boundEnumerateLimit(limit); got != DefaultEnumerateLimit {
			t.Fatalf("boundEnumerateLimit(%d) = %d, want %d", limit, got, DefaultEnumerateLimit)
		}
	}
	if got := f.eng.boundEnumerateLimit(7); got != 7 {
		t.Fatalf("boundEnumerateLimit(7) = %d, want 7", got)
	}

	got := f.enumerate(map[string]any{"seats": int64(5)}, 0)
	want := []string{
		"account:acme/document:1", "account:acme/document:2", "account:acme/document:4",
	}
	if !sameSet(got, want) {
		t.Fatalf("Limit 0 filtered enumerate = %v, want every match %v", got, want)
	}
}

// --- Acceptance: the filter only ever SUBTRACTS from the allowed set ---

// A deny-carved object is never returned, whatever the predicate says: the
// filter runs on candidates that already survived deny-overrides.
func TestEnumerateFields_DeniedObjectNeverReturned(t *testing.T) {
	f := newFieldsFixture(t, brandCatalogue())
	f.principal("alice")
	f.perm("p-impl", scope.StrategyImplicit)
	f.perm("p-inc", "inclusive;ids=account:acme/document:1")
	f.grant("allow-all", "alice", model.EffectAllow, "p-impl", "account:acme/**")
	// Deny document:1 at equal specificity — deny wins.
	f.grant("deny-1", "alice", model.EffectDeny, "p-inc", "account:acme/**")

	got := f.enumerate(map[string]any{"current_brands": "brand:Y"}, 0)
	want := []string{"account:acme/document:2"}
	if !sameSet(got, want) {
		t.Fatalf("filtered enumerate = %v, want %v (document:1 matches the predicate but is denied)", got, want)
	}

	// And the Check that backs the guarantee agrees.
	dec, err := f.eng.Check(context.Background(), Request{
		Account: acctAcme, Principal: "alice", Action: "read", Object: "account:acme/document:1",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if dec.Allow {
		t.Fatal("Check allowed a document Enumerate refused to return; the two must agree")
	}
}

// --- Acceptance: no metadata source is a coded error, never an empty result ---

func TestEnumerateFields_UnregisteredProviderErrors(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())

	// A registry with no provider for "document" at all.
	empty := provider.NewRegistry()
	unregistered := New(f.store,
		WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: f.reg}),
		WithMetadata(empty),
	)
	_, err := unregistered.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Fields: map[string]any{"tier": "premium"},
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("unregistered type: code = %q, want APERTURE_PROVIDER_UNREGISTERED (never a silent empty result)", code)
	}

	// An engine with no metadata source wired at all fails the same way.
	unwired := New(f.store, WithScopeResolution(scope.DefaultRegistry(), ScopeDeps{Lister: f.reg}))
	_, err = unwired.Enumerate(context.Background(), EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read",
		Pattern: "account:acme/**", Fields: map[string]any{"tier": "premium"},
	})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("unwired engine: code = %q, want APERTURE_PROVIDER_UNREGISTERED", code)
	}
}

// --- Acceptance: the batch and impersonated variants carry Fields identically ---

func TestEnumerateFields_BatchCarriesFields(t *testing.T) {
	f := allowAllFixture(t, brandCatalogue())
	base := EnumerateRequest{Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**"}
	filtered := base
	filtered.Fields = map[string]any{"current_brands": "brand:Y"}

	results := f.eng.EnumerateBatch(context.Background(), []EnumerateRequest{base, filtered})
	if len(results) != 2 {
		t.Fatalf("batch returned %d items, want 2", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("batch item %d: %v", i, r.Err)
		}
	}
	if len(results[0].Result) != 4 {
		t.Fatalf("unfiltered batch item = %v, want all 4 documents", results[0].Result)
	}
	want := []string{"account:acme/document:1", "account:acme/document:2"}
	if !sameSet(results[1].Result, want) {
		t.Fatalf("filtered batch item = %v, want %v", results[1].Result, want)
	}
}

func TestEnumerateFields_EnumerateAsCarriesFields(t *testing.T) {
	f := newFieldsFixture(t, brandCatalogue())
	f.principal("alice") // the operator
	f.principal("bob")   // the target, who alone may read
	f.perm("p-impl", scope.StrategyImplicit)
	f.grant("g-bob", "bob", model.EffectAllow, "p-impl", "account:acme/**")

	ic := ImpersonationContext{
		RealActor:        "alice",
		EffectiveSubject: "bob",
		Mode:             ModeBecome,
		ExpiresAt:        time.Now().Add(time.Hour),
	}
	req := EnumerateRequest{
		Account: acctAcme, Principal: "alice", Action: "read", Pattern: "account:acme/**",
		Fields: map[string]any{"current_brands": "brand:Y"},
	}
	got, err := f.eng.EnumerateAs(context.Background(), req, ic)
	if err != nil {
		t.Fatalf("EnumerateAs: %v", err)
	}
	want := []string{"account:acme/document:1", "account:acme/document:2"}
	if !sameSet(got, want) {
		t.Fatalf("impersonated filtered enumerate = %v, want %v", got, want)
	}

	// Alice's own (unelevated) enumeration sees nothing, so the filtered result
	// really came through the impersonated subject set.
	if own := f.enumerate(map[string]any{"current_brands": "brand:Y"}, 0); len(own) != 0 {
		t.Fatalf("operator's own filtered enumerate = %v, want empty", own)
	}
}

// An object the provider has no row for is EXCLUDED (every field absent), not an
// error and never silently included.
func TestEnumerateFields_ObjectWithoutMetadataExcluded(t *testing.T) {
	f := newFieldsFixture(t, brandCatalogue())
	f.principal("alice")
	f.perm("p-inc", "inclusive;ids=account:acme/document:1,account:acme/document:99")
	f.grant("g-inc", "alice", model.EffectAllow, "p-inc", "account:acme/**")

	// Unfiltered, both ids enumerate — document:99 has no provider row.
	unfiltered := f.enumerate(nil, 0)
	if !sameSet(unfiltered, []string{"account:acme/document:1", "account:acme/document:99"}) {
		t.Fatalf("unfiltered enumerate = %v, want both inclusive ids", unfiltered)
	}
	got := f.enumerate(map[string]any{"current_brands": "brand:Y"}, 0)
	if !sameSet(got, []string{"account:acme/document:1"}) {
		t.Fatalf("filtered enumerate = %v, want only the object with matching metadata", got)
	}
}

// The engine treats cached metadata as READ-ONLY, transitively: filtering must
// not disturb the provider's own value at any depth.
func TestEnumerateFields_DoesNotMutateMetadata(t *testing.T) {
	objects := brandCatalogue()
	f := allowAllFixture(t, objects)

	if got := f.enumerate(map[string]any{"current_brands": "brand:Y"}, 0); len(got) != 2 {
		t.Fatalf("filtered enumerate = %v, want 2 ids", got)
	}
	md, err := f.reg.Fetch(context.Background(), mustParse(t, "account:acme/document:1"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	brands, ok := md["current_brands"].([]any)
	if !ok || len(brands) != 2 || brands[0] != "brand:X" || brands[1] != "brand:Y" {
		t.Fatalf("metadata mutated by the filter: current_brands = %#v", md["current_brands"])
	}
	if len(md) != 3 {
		t.Fatalf("metadata gained or lost keys: %#v", md)
	}
}

func mustParse(t *testing.T, s string) identity.Identity {
	t.Helper()
	id, err := identity.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return id
}
