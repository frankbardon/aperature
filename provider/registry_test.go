package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/scope"
)

// Registry is the concrete type E2-S4 wires as engine.ScopeDeps{Lister: reg};
// assert it satisfies the scope seam left by E2-S1 so the wiring cannot rot.
var _ scope.ObjectLister = (*Registry)(nil)

// fakeProvider is a host ObjectProvider backed by an in-memory table. It counts
// Fetch/Query calls so tests can prove a cache hit avoids the provider.
type fakeProvider struct {
	mu        sync.Mutex
	objects   map[string]Metadata // identity string -> metadata
	fetches   int64               // atomic
	queries   int64               // atomic
	lastLimit int                 // the Filter.Limit the last Query actually saw
	failNext  error
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{objects: make(map[string]Metadata)}
}

func (p *fakeProvider) put(id string, md Metadata) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.objects[id] = md
}

func (p *fakeProvider) Fetch(_ context.Context, id identity.Identity) (Metadata, error) {
	atomic.AddInt64(&p.fetches, 1)
	if p.failNext != nil {
		err := p.failNext
		p.failNext = nil
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	md, ok := p.objects[id.String()]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND, "no such object",
			map[string]any{"id": id.String()})
	}
	return md, nil
}

func (p *fakeProvider) List(ctx context.Context) ([]Object, error) {
	return p.Query(ctx, Filter{})
}

func (p *fakeProvider) Query(_ context.Context, f Filter) ([]Object, error) {
	atomic.AddInt64(&p.queries, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastLimit = f.Limit
	out := make([]Object, 0, len(p.objects))
	for s, md := range p.objects {
		out = append(out, Object{ID: identity.MustParse(s), Metadata: md})
	}
	return out, nil
}

func (p *fakeProvider) fetchCount() int64 { return atomic.LoadInt64(&p.fetches) }
func (p *fakeProvider) queryCount() int64 { return atomic.LoadInt64(&p.queries) }

func TestRegistryFetchThrough(t *testing.T) {
	p := newFakeProvider()
	p.put("account:acme/document:42", Metadata{"title": "Q3 plan"})
	reg := NewRegistry()
	reg.MustRegister("document", p)

	id := identity.MustParse("account:acme/document:42")

	md, err := reg.Fetch(context.Background(), id)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if md["title"] != "Q3 plan" {
		t.Fatalf("metadata = %v", md)
	}
	if p.fetchCount() != 1 {
		t.Fatalf("provider fetches = %d, want 1 after first Fetch", p.fetchCount())
	}

	// Second Fetch is a cache hit: the provider must not be called again.
	if _, err := reg.Fetch(context.Background(), id); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if p.fetchCount() != 1 {
		t.Fatalf("provider fetches = %d, want 1 (second Fetch should hit cache)", p.fetchCount())
	}
	s, _ := reg.Stats("document")
	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats = %+v, want Hits=1 Misses=1", s)
	}
}

func TestRegistryFetchUnregistered(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/widget:1"))
	if aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %s, want APERTURE_PROVIDER_UNREGISTERED", aerr.CodeOf(err))
	}
}

func TestRegistryFetchNotFoundPassesThrough(t *testing.T) {
	p := newFakeProvider()
	reg := NewRegistry()
	reg.MustRegister("document", p)
	_, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/document:missing"))
	if aerr.CodeOf(err) != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s, want APERTURE_NOT_FOUND (coded provider error passes through)", aerr.CodeOf(err))
	}
}

func TestRegistryFetchPlainErrorWrapped(t *testing.T) {
	p := newFakeProvider()
	sentinel := errors.New("upstream down")
	p.failNext = sentinel
	reg := NewRegistry()
	reg.MustRegister("document", p)

	_, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/document:1"))
	if aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_FETCH {
		t.Fatalf("code = %s, want APERTURE_PROVIDER_FETCH", aerr.CodeOf(err))
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("wrapped cause is not reachable via errors.Is")
	}
}

func TestRegistryInvalidate(t *testing.T) {
	p := newFakeProvider()
	p.put("account:acme/document:42", Metadata{"v": 1})
	reg := NewRegistry()
	reg.MustRegister("document", p)
	id := identity.MustParse("account:acme/document:42")

	if _, err := reg.Fetch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	removed, err := reg.Invalidate(id)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("Invalidate reported nothing removed")
	}
	// After invalidation the next Fetch re-pulls from the provider.
	if _, err := reg.Fetch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if p.fetchCount() != 2 {
		t.Fatalf("fetches = %d, want 2 (invalidation should force a re-pull)", p.fetchCount())
	}
}

func TestRegistryInvalidateType(t *testing.T) {
	p := newFakeProvider()
	p.put("account:acme/document:1", Metadata{})
	p.put("account:acme/document:2", Metadata{})
	reg := NewRegistry()
	reg.MustRegister("document", p)
	ctx := context.Background()
	_, _ = reg.Fetch(ctx, identity.MustParse("account:acme/document:1"))
	_, _ = reg.Fetch(ctx, identity.MustParse("account:acme/document:2"))

	if err := reg.InvalidateType("document"); err != nil {
		t.Fatal(err)
	}
	if s, _ := reg.Stats("document"); s.Entries != 0 {
		t.Fatalf("type cache not cleared: entries = %d", s.Entries)
	}
	if err := reg.InvalidateType("missing"); aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("InvalidateType(missing) code = %s, want UNREGISTERED", aerr.CodeOf(err))
	}
}

func TestRegistryTTLForcesRefetch(t *testing.T) {
	clk := newFakeClock()
	p := newFakeProvider()
	p.put("account:acme/document:1", Metadata{"v": 1})
	reg := NewRegistry()
	reg.MustRegister("document", p, WithTTL(time.Minute), WithClock(clk.Now))
	id := identity.MustParse("account:acme/document:1")

	if _, err := reg.Fetch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute) // entry now stale
	if _, err := reg.Fetch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if p.fetchCount() != 2 {
		t.Fatalf("fetches = %d, want 2 (stale entry should re-pull)", p.fetchCount())
	}
}

// TestRegistryAsScopeLister drives the Registry through the scope.ObjectLister
// contract exactly as the implicit/exclusive resolvers (E2-S1) will, and proves
// enumeration warms the metadata cache so a follow-up Fetch is a hit.
func TestRegistryAsScopeLister(t *testing.T) {
	p := newFakeProvider()
	for i := 1; i <= 3; i++ {
		p.put(fmt.Sprintf("account:acme/document:%d", i), Metadata{"n": i})
	}
	p.put("account:other/document:9", Metadata{"n": 9})

	reg := NewRegistry()
	reg.MustRegister("document", p)

	var lister scope.ObjectLister = reg
	pat := identity.MustParsePattern("account:acme/document:*")
	ids, err := lister.List(context.Background(), "document", pat, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("listed %d identities, want 3 (pattern must exclude account:other)", len(ids))
	}
	if p.queryCount() != 1 {
		t.Fatalf("provider queries = %d, want 1 (one enumeration)", p.queryCount())
	}
	for _, id := range ids {
		if !pat.Matches(id) {
			t.Fatalf("listed identity %s does not match the bounding pattern", id)
		}
	}
	// Enumeration warmed the cache: fetching a listed object is a hit (no Fetch call).
	before := p.fetchCount()
	if _, err := reg.Fetch(context.Background(), identity.MustParse("account:acme/document:1")); err != nil {
		t.Fatal(err)
	}
	if p.fetchCount() != before {
		t.Fatalf("List did not warm the cache: Fetch triggered a provider call")
	}
}

// List honours a positive caller limit rather than clamping it back to
// DefaultListLimit. The caller is the authority: the ceiling on a decision path
// is the engine's configured enumerate limit, one layer above this package.
func TestRegistryListHonorsAPositiveLimit(t *testing.T) {
	const want = DefaultListLimit + 50

	p := newFakeProvider()
	for i := 1; i <= want+25; i++ {
		p.put(fmt.Sprintf("account:acme/document:%d", i), Metadata{"n": i})
	}
	// One object outside the pattern, so the re-check is still exercised at a
	// limit the old clamp would never have reached.
	p.put("account:other/document:9", Metadata{"n": 9})

	reg := NewRegistry()
	reg.MustRegister("document", p)

	pat := identity.MustParsePattern("account:acme/document:*")
	ids, err := reg.List(context.Background(), "document", pat, want)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != want {
		t.Fatalf("listed %d identities, want the caller's limit of %d", len(ids), want)
	}
	for _, id := range ids {
		if !pat.Matches(id) {
			t.Fatalf("listed identity %s does not match the bounding pattern", id)
		}
	}
	// The provider SAW the caller's limit — the value propagates rather than
	// being substituted here and truncated afterwards.
	p.mu.Lock()
	saw := p.lastLimit
	p.mu.Unlock()
	if saw != want {
		t.Fatalf("provider saw limit %d, want the caller's %d", saw, want)
	}
	// Cache warming still holds at the raised bound.
	before := p.fetchCount()
	if _, err := reg.Fetch(context.Background(), ids[0]); err != nil {
		t.Fatal(err)
	}
	if p.fetchCount() != before {
		t.Fatalf("List did not warm the cache: Fetch triggered a provider call")
	}
}

// A non-positive limit still means DefaultListLimit — that half of the contract
// is unchanged, and it is what stops an unasked-for enumeration being unbounded.
func TestRegistryListDefaultsANonPositiveLimit(t *testing.T) {
	p := newFakeProvider()
	for i := 1; i <= DefaultListLimit+50; i++ {
		p.put(fmt.Sprintf("account:acme/document:%d", i), Metadata{"n": i})
	}
	reg := NewRegistry()
	reg.MustRegister("document", p)

	pat := identity.MustParsePattern("account:acme/document:*")
	ids, err := reg.List(context.Background(), "document", pat, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != DefaultListLimit {
		t.Fatalf("listed %d identities, want the DefaultListLimit default of %d", len(ids), DefaultListLimit)
	}
	p.mu.Lock()
	saw := p.lastLimit
	p.mu.Unlock()
	if saw != DefaultListLimit {
		t.Fatalf("provider saw limit %d, want the substituted %d", saw, DefaultListLimit)
	}
}

func TestRegistryListUnregistered(t *testing.T) {
	reg := NewRegistry()
	pat := identity.MustParsePattern("account:acme/widget:*")
	_, err := reg.List(context.Background(), "widget", pat, 0)
	if aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %s, want APERTURE_PROVIDER_UNREGISTERED", aerr.CodeOf(err))
	}
}

func TestRegistryRegisterValidation(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register("", newFakeProvider()); aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_INVALID {
		t.Errorf("empty type code = %s, want INVALID", aerr.CodeOf(err))
	}
	if err := reg.Register("document", nil); aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_INVALID {
		t.Errorf("nil provider code = %s, want INVALID", aerr.CodeOf(err))
	}
	if err := reg.Register("document", newFakeProvider()); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := reg.Register("document", newFakeProvider()); aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_INVALID {
		t.Errorf("duplicate code = %s, want INVALID", aerr.CodeOf(err))
	}
	if !reg.Has("document") {
		t.Error("Has(document) = false after register")
	}
	if got := reg.Keys(); len(got) != 1 || got[0] != "document" {
		t.Errorf("Keys() = %v, want [document]", got)
	}
}

func TestRegistryCustomCacheFactory(t *testing.T) {
	var built int
	reg := NewRegistry(WithCacheFactory(func(cfg CacheConfig) CacheBackend {
		built++
		return NewMemoryCache(cfg)
	}))
	reg.MustRegister("document", newFakeProvider())
	if built != 1 {
		t.Fatalf("custom cache factory invoked %d times, want 1", built)
	}
}

func TestRegistryConcurrentFetch(t *testing.T) {
	p := newFakeProvider()
	for i := 0; i < 50; i++ {
		p.put(fmt.Sprintf("account:acme/document:%d", i), Metadata{"n": i})
	}
	reg := NewRegistry()
	reg.MustRegister("document", p, WithTTL(-1))

	const workers = 24
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 200; i++ {
				id := identity.MustParse(fmt.Sprintf("account:acme/document:%d", i%50))
				if _, err := reg.Fetch(ctx, id); err != nil {
					t.Errorf("Fetch: %v", err)
					return
				}
				if i%40 == 0 {
					_, _ = reg.Invalidate(id)
				}
				_, _ = reg.Stats("document")
			}
		}()
	}
	wg.Wait()
}

func TestRegistryIdentifiers(t *testing.T) {
	p := newFakeProvider()
	p.put("brand:1", Metadata{"category_id": "category:1"})
	p.put("brand:2", Metadata{"category_id": "category:1"})
	p.put("brand:3", Metadata{"category_id": "category:2"})
	reg := NewRegistry()
	reg.MustRegister("brand", p)

	ids, err := reg.Identifiers(context.Background(), "brand")
	if err != nil {
		t.Fatalf("Identifiers: %v", err)
	}
	// Uncapped and sorted by canonical id.
	got := make([]string, len(ids))
	for i, id := range ids {
		got[i] = id.String()
	}
	want := []string{"brand:1", "brand:2", "brand:3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}

	// Enumeration warms the cache, so a following Fetch is a hit (no new fetch).
	if _, err := reg.Fetch(context.Background(), identity.MustParse("brand:2")); err != nil {
		t.Fatalf("Fetch after Identifiers: %v", err)
	}
	if p.fetchCount() != 0 {
		t.Errorf("provider fetches = %d, want 0 (served from warmed cache)", p.fetchCount())
	}

	// Unregistered type is a coded error.
	if _, err := reg.Identifiers(context.Background(), "nope"); aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Errorf("code = %s, want APERTURE_PROVIDER_UNREGISTERED", aerr.CodeOf(err))
	}
}

func TestRegistryIdentifiersExcept(t *testing.T) {
	p := newFakeProvider()
	for _, id := range []string{"brand:1", "brand:2", "brand:3", "brand:4"} {
		p.put(id, Metadata{})
	}
	reg := NewRegistry()
	reg.MustRegister("brand", p)

	// Exclusive allowance: all brands EXCEPT brand:2 and brand:4.
	allowed, err := reg.IdentifiersExcept(context.Background(), "brand",
		identity.MustParse("brand:2"),
		identity.MustParse("brand:4"),
		identity.MustParse("brand:99"), // not a real id: ignored, not an error
	)
	if err != nil {
		t.Fatalf("IdentifiersExcept: %v", err)
	}
	got := make([]string, len(allowed))
	for i, id := range allowed {
		got[i] = id.String()
	}
	want := []string{"brand:1", "brand:3"}
	if len(got) != len(want) {
		t.Fatalf("allowed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allowed = %v, want %v", got, want)
		}
	}

	// No exclusions returns the full set.
	full, err := reg.IdentifiersExcept(context.Background(), "brand")
	if err != nil {
		t.Fatalf("IdentifiersExcept (none): %v", err)
	}
	if len(full) != 4 {
		t.Errorf("full = %d, want 4", len(full))
	}
}
