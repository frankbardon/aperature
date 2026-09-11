package provider

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/scope"
)

// ---------------------------------------------------------------------------
// The containment guarantee
// ---------------------------------------------------------------------------

// TestAttributeRegistryIsNotAScopeLister is the load-bearing test of this
// package. registry.go carries `var _ ObjectLister = (*Registry)(nil)` on
// purpose; the whole point of the attribute seam is that the same assertion must
// be FALSE for an *AttributeRegistry.
//
// Go's typing is structural, so a comment saying "this is not a lister" enforces
// nothing: an enumeration method that happened to be spelled
// List(ctx, string, identity.Pattern, int) ([]identity.Identity, error) would
// satisfy scope.ObjectLister whether or not anyone meant it to, and a resolver
// wired to it would compile, run, and enumerate the principal directory
// mid-decision under the grant's own scope, with no admin tier consulted.
//
// The assertion is made against the REAL scope.ObjectLister — not the restated
// copy in provider.go — so it cannot pass because the local copy drifted.
func TestAttributeRegistryIsNotAScopeLister(t *testing.T) {
	var reg any = NewAttributeRegistry()
	if lister, ok := reg.(scope.ObjectLister); ok {
		t.Fatalf("*AttributeRegistry satisfies scope.ObjectLister (%T); "+
			"attribute enumeration is a system-tier admin read and must never be a "+
			"scope-resolution source — give it a signature scope cannot assign", lister)
	}
	// The restated local copy too, since that is the one this package's own
	// wiring would reach for.
	if _, ok := reg.(ObjectLister); ok {
		t.Fatal("*AttributeRegistry satisfies provider.ObjectLister")
	}
	// It must not pass for an object source either: an AttributeRegistry handed
	// to engine.WithMetadata would answer object questions with principal bags.
	if _, ok := reg.(ObjectProvider); ok {
		t.Fatal("*AttributeRegistry satisfies provider.ObjectProvider")
	}
	// Positive control: the guarantee is only meaningful if the assertion would
	// actually fire. *Registry is the type that DOES satisfy it.
	if _, ok := any(NewRegistry()).(scope.ObjectLister); !ok {
		t.Fatal("*Registry no longer satisfies scope.ObjectLister; the negative " +
			"assertion above proves nothing if the interface itself moved")
	}
}

// TestAttributeRegistryHasNoListMethod pins the structural reason the assertion
// above holds: there is no method named List on *AttributeRegistry at all, so a
// future edit cannot re-introduce the collision by accident while leaving the
// interface check technically satisfied elsewhere.
func TestAttributeRegistryHasNoListMethod(t *testing.T) {
	rt := reflect.TypeOf(NewAttributeRegistry())
	if _, ok := rt.MethodByName("List"); ok {
		t.Fatal("*AttributeRegistry grew a List method; enumeration is called " +
			"Enumerate precisely so the name cannot collide with scope.ObjectLister")
	}
	m, ok := rt.MethodByName("Enumerate")
	if !ok {
		t.Fatal("*AttributeRegistry has no Enumerate method")
	}
	// Every parameter and result differs from scope.ObjectLister.List's, so the
	// signature is unassignable four times over.
	if m.Type.In(2) == reflect.TypeOf("") {
		t.Error("Enumerate's slot parameter is a bare string; it must be an AttributeSlot")
	}
	if m.Type.In(3) == reflect.TypeOf(identity.Pattern{}) {
		t.Error("Enumerate takes an identity.Pattern; an attribute enumeration has no scope to bound with")
	}
	if m.Type.Out(0) == reflect.TypeOf([]identity.Identity(nil)) {
		t.Error("Enumerate returns []identity.Identity; attribute keys are bare strings")
	}
}

// TestAttributeFilterHasNoPattern asserts the omission documented on
// AttributeFilter is real. A Pattern field is what makes a filter look like the
// seam a scope resolver wants, and shapes get wired to what they look like.
func TestAttributeFilterHasNoPattern(t *testing.T) {
	rt := reflect.TypeOf(AttributeFilter{})
	for i := range rt.NumField() {
		if name := rt.Field(i).Name; name == "Pattern" {
			t.Fatal("AttributeFilter has a Pattern field; see the type doc for why it must not")
		}
	}
	if rt.NumField() != 2 {
		t.Errorf("AttributeFilter has %d fields, want exactly Fields and Limit", rt.NumField())
	}
}

// TestProviderPackageImportsOnlyIdentityAndErrors walks every non-test file in
// this package and asserts the dependency floor holds: stdlib, identity, errors,
// and nothing else. It is why a principal KIND has to arrive as an argument
// rather than be resolved in here — provider cannot see model, scope, or engine,
// and the attribute seam must not be the change that breaks that.
func TestProviderPackageImportsOnlyIdentityAndErrors(t *testing.T) {
	const module = "github.com/frankbardon/aperture/"
	allowed := map[string]bool{
		module + "identity": true,
		module + "errors":   true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import literal %s", name, spec.Path.Value)
			}
			if !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
				continue // stdlib
			}
			if !allowed[path] {
				t.Errorf("%s imports %q; provider may import only identity and errors "+
					"(never scope, engine, or model)", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go files found; the firewall scanned nothing")
	}
}

// ---------------------------------------------------------------------------
// The slot set
// ---------------------------------------------------------------------------

// TestTheSlotSetIsClosedAtThree asserts the closed set is exactly user, machine,
// account. A fourth slot is a change to what a decision's PARTIES are, not a
// registry tweak, and it should break a test before it reaches a rule root.
func TestTheSlotSetIsClosedAtThree(t *testing.T) {
	got := AttributeSlots()
	want := []AttributeSlot{AttributeSlotUser, AttributeSlotMachine, AttributeSlotAccount}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttributeSlots() = %v, want %v", got, want)
	}
	for _, s := range want {
		if !s.Valid() {
			t.Errorf("%q is in AttributeSlots() but Valid() is false", s)
		}
		if s.String() != string(s) {
			t.Errorf("%q.String() = %q", s, s.String())
		}
	}
	if AttributeSlotUser.String() != "user" ||
		AttributeSlotMachine.String() != "machine" ||
		AttributeSlotAccount.String() != "account" {
		t.Error("the slot keys are the wire spelling; they must stay user/machine/account")
	}
}

func TestParseAttributeSlot(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    AttributeSlot
		wantErr bool
	}{
		{in: "user", want: AttributeSlotUser},
		{in: "machine", want: AttributeSlotMachine},
		{in: "account", want: AttributeSlotAccount},
		{in: "", wantErr: true},
		{in: "User", wantErr: true},
		{in: "principal", wantErr: true},
		{in: "group", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseAttributeSlot(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAttributeSlot(%q) = %q, want an error", tc.in, got)
				}
				if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN {
					t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_SLOT_UNKNOWN", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAttributeSlot(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestAttributeRegistration(t *testing.T) {
	p := mustStaticAttributes(t, []AttributeRecord{{ID: "u-1", Attributes: Metadata{"department": "eng"}}})

	t.Run("unknown slot is refused", func(t *testing.T) {
		reg := NewAttributeRegistry()
		err := reg.Register(AttributeSlot("robot"), p)
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_SLOT_UNKNOWN (err=%v)", code, err)
		}
	})

	t.Run("nil provider is refused", func(t *testing.T) {
		reg := NewAttributeRegistry()
		err := reg.Register(AttributeSlotUser, nil)
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID (err=%v)", code, err)
		}
	})

	t.Run("a duplicate is refused rather than replacing", func(t *testing.T) {
		reg := NewAttributeRegistry()
		if err := reg.Register(AttributeSlotUser, p); err != nil {
			t.Fatalf("first register: %v", err)
		}
		other := mustStaticAttributes(t, []AttributeRecord{{ID: "u-1", Attributes: Metadata{"department": "sales"}}})
		err := reg.Register(AttributeSlotUser, other)
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID (err=%v)", code, err)
		}
		md, err := reg.Fetch(context.Background(), AttributeSlotUser, "u-1")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if md["department"] != "eng" {
			t.Fatalf("the refused registration still took effect: %v", md)
		}
	})

	t.Run("slots register independently", func(t *testing.T) {
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotUser, p)
		reg.MustRegister(AttributeSlotAccount, mustStaticAttributes(t,
			[]AttributeRecord{{ID: "acme", Attributes: Metadata{"plan": "enterprise"}}}))
		if !reg.Has(AttributeSlotUser) || !reg.Has(AttributeSlotAccount) {
			t.Fatal("registered slots do not report Has")
		}
		if reg.Has(AttributeSlotMachine) {
			t.Fatal("the machine slot reports Has without a registration")
		}
		if reg.Has(AttributeSlot("robot")) {
			t.Fatal("an unknown slot reports Has")
		}
		got := reg.RegisteredSlots()
		want := []AttributeSlot{AttributeSlotUser, AttributeSlotAccount}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("RegisteredSlots() = %v, want %v (AttributeSlots order)", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

func TestAttributeFetch(t *testing.T) {
	ctx := context.Background()

	t.Run("an empty slot is a wiring diagnostic, not an empty bag", func(t *testing.T) {
		reg := NewAttributeRegistry()
		md, err := reg.Fetch(ctx, AttributeSlotMachine, "svc-1")
		if md != nil {
			t.Fatalf("got a bag %v for an unregistered slot; an empty bag reads as "+
				"'no attributes' and denies silently", md)
		}
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED", code)
		}
	})

	t.Run("an unknown slot is distinct from an unwired one", func(t *testing.T) {
		reg := NewAttributeRegistry()
		_, err := reg.Fetch(ctx, AttributeSlot("robot"), "x")
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_SLOT_UNKNOWN", code)
		}
	})

	t.Run("keys that name nobody are refused before the provider", func(t *testing.T) {
		counter := &countingAttributes{}
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotAccount, counter)
		for _, key := range []string{"", "*"} {
			_, err := reg.Fetch(ctx, AttributeSlotAccount, key)
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
				t.Fatalf("key %q: code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", key, code)
			}
		}
		if n := counter.fetches.Load(); n != 0 {
			t.Fatalf("the provider was consulted %d times for an unusable key", n)
		}
	})

	t.Run("a hit never calls the provider", func(t *testing.T) {
		counter := &countingAttributes{bags: map[string]Metadata{"u-1": {"department": "eng"}}}
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotUser, counter)
		for range 3 {
			md, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if md["department"] != "eng" {
				t.Fatalf("bag = %v", md)
			}
		}
		if n := counter.fetches.Load(); n != 1 {
			t.Fatalf("provider fetched %d times, want 1", n)
		}
		st, ok := reg.Stats(AttributeSlotUser)
		if !ok {
			t.Fatal("no stats for the user slot")
		}
		if st.Hits != 2 || st.Misses != 1 || st.Entries != 1 {
			t.Fatalf("stats = %+v, want 2 hits / 1 miss / 1 entry", st)
		}
	})

	t.Run("APERTURE_NOT_FOUND passes through with no re-stamp", func(t *testing.T) {
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotUser, mustStaticAttributes(t, nil))
		_, err := reg.Fetch(ctx, AttributeSlotUser, "nobody")
		if code := aerr.CodeOf(err); code != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("code = %s, want APERTURE_NOT_FOUND — an unknown subject must stay "+
				"distinguishable from an unreachable directory", code)
		}
		if depth := codedDepth(err); depth != 1 {
			t.Fatalf("%d Aperture-coded errors in the chain, want exactly 1: Wrap "+
				"re-stamps, so a same-code re-wrap is invisible to CodeOf", depth)
		}
	})

	t.Run("a plain provider error becomes APERTURE_ATTRIBUTE_PROVIDER_FETCH", func(t *testing.T) {
		boom := errors.New("directory unreachable")
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotUser, &countingAttributes{err: boom})
		_, err := reg.Fetch(ctx, AttributeSlotUser, "u-1")
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_FETCH", code)
		}
		if !errors.Is(err, boom) {
			t.Fatal("the cause was not wrapped verbatim")
		}
	})

	t.Run("a failed fetch is not cached", func(t *testing.T) {
		counter := &countingAttributes{err: errors.New("transient")}
		reg := NewAttributeRegistry()
		reg.MustRegister(AttributeSlotUser, counter)
		for range 2 {
			if _, err := reg.Fetch(ctx, AttributeSlotUser, "u-1"); err == nil {
				t.Fatal("want an error")
			}
		}
		if n := counter.fetches.Load(); n != 2 {
			t.Fatalf("provider fetched %d times, want 2 — a failure must not be cached", n)
		}
	})
}

// TestAttributeSlotsCacheIndependently asserts each slot carries its own TTL,
// size cap, and counters. The three have genuinely different change rates, and a
// single pooled cache would tune all of them to the worst one — and hide which
// slot is actually missing.
func TestAttributeSlotsCacheIndependently(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()

	users := &countingAttributes{bags: map[string]Metadata{"u-1": {"department": "eng"}}}
	accounts := &countingAttributes{bags: map[string]Metadata{"acme": {"plan": "enterprise"}}}

	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, users, WithTTL(time.Minute), WithMaxSize(2), WithClock(clock.Now))
	reg.MustRegister(AttributeSlotAccount, accounts, WithTTL(time.Hour), WithClock(clock.Now))

	userCfg, ok := reg.CacheConfigFor(AttributeSlotUser)
	if !ok || userCfg.TTL != time.Minute || userCfg.MaxSize != 2 {
		t.Fatalf("user cache config = %+v (ok=%v), want TTL 1m / MaxSize 2", userCfg, ok)
	}
	acctCfg, ok := reg.CacheConfigFor(AttributeSlotAccount)
	if !ok || acctCfg.TTL != time.Hour || acctCfg.MaxSize != DefaultMaxSize {
		t.Fatalf("account cache config = %+v (ok=%v), want TTL 1h / default MaxSize", acctCfg, ok)
	}
	if _, ok := reg.CacheConfigFor(AttributeSlotMachine); ok {
		t.Fatal("an unregistered slot reported a cache config")
	}

	for _, f := range []func() error{
		func() error { _, err := reg.Fetch(ctx, AttributeSlotUser, "u-1"); return err },
		func() error { _, err := reg.Fetch(ctx, AttributeSlotAccount, "acme"); return err },
	} {
		if err := f(); err != nil {
			t.Fatalf("warm: %v", err)
		}
	}

	// Half an hour on: the user slot's minute TTL has expired, the account
	// slot's hour has not. One clock, two independent freshness windows.
	clock.Advance(30 * time.Minute)
	if _, err := reg.Fetch(ctx, AttributeSlotUser, "u-1"); err != nil {
		t.Fatalf("user re-fetch: %v", err)
	}
	if _, err := reg.Fetch(ctx, AttributeSlotAccount, "acme"); err != nil {
		t.Fatalf("account re-fetch: %v", err)
	}
	if n := users.fetches.Load(); n != 2 {
		t.Fatalf("user provider fetched %d times, want 2 (its TTL expired)", n)
	}
	if n := accounts.fetches.Load(); n != 1 {
		t.Fatalf("account provider fetched %d times, want 1 (its TTL had not expired)", n)
	}

	userStats, _ := reg.Stats(AttributeSlotUser)
	acctStats, _ := reg.Stats(AttributeSlotAccount)
	if userStats.Expirations != 1 {
		t.Fatalf("user stats = %+v, want 1 expiration", userStats)
	}
	if acctStats.Expirations != 0 || acctStats.Hits != 1 {
		t.Fatalf("account stats = %+v, want 0 expirations / 1 hit — counters are per slot", acctStats)
	}
	if _, ok := reg.Stats(AttributeSlotMachine); ok {
		t.Fatal("an unregistered slot reported stats")
	}
}

// ---------------------------------------------------------------------------
// Enumerate
// ---------------------------------------------------------------------------

func TestAttributeEnumerate(t *testing.T) {
	ctx := context.Background()
	records := []AttributeRecord{
		{ID: "u-1", Attributes: Metadata{"department": "eng", "tags": []any{"premium", "beta"}, "seats": int64(5)}},
		{ID: "u-2", Attributes: Metadata{"department": "sales", "tags": []any{"beta"}, "seats": int64(3)}},
		{ID: "u-3", Attributes: Metadata{"department": "eng", "tags": []any{"premium"}, "seats": int64(5)}},
	}
	reg := NewAttributeRegistry()
	reg.MustRegister(AttributeSlotUser, mustStaticAttributes(t, records))

	for _, tc := range []struct {
		name   string
		filter AttributeFilter
		want   []string
	}{
		{name: "the zero filter selects everything", want: []string{"u-1", "u-2", "u-3"}},
		{name: "scalar equality", filter: AttributeFilter{Fields: map[string]any{"department": "eng"}}, want: []string{"u-1", "u-3"}},
		{name: "collection membership", filter: AttributeFilter{Fields: map[string]any{"tags": "premium"}}, want: []string{"u-1", "u-3"}},
		{name: "predicates AND", filter: AttributeFilter{Fields: map[string]any{"department": "eng", "tags": "beta"}}, want: []string{"u-1"}},
		{name: "numbers compare across Go types", filter: AttributeFilter{Fields: map[string]any{"seats": 5}}, want: []string{"u-1", "u-3"}},
		{name: "a number never equals its string spelling", filter: AttributeFilter{Fields: map[string]any{"seats": "5"}}, want: []string{}},
		{name: "an absent field never matches", filter: AttributeFilter{Fields: map[string]any{"clearance": nil}}, want: []string{}},
		{name: "limit bounds the page", filter: AttributeFilter{Limit: 2}, want: []string{"u-1", "u-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.Enumerate(ctx, AttributeSlotUser, tc.filter)
			if err != nil {
				t.Fatalf("enumerate: %v", err)
			}
			if ids := recordIDs(got); !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}

	t.Run("an unwired slot is reported, not answered empty", func(t *testing.T) {
		out, err := reg.Enumerate(ctx, AttributeSlotMachine, AttributeFilter{})
		if out != nil {
			t.Fatalf("got %v for an unregistered slot", out)
		}
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED", code)
		}
	})

	t.Run("the registry re-enforces Fields a provider ignored", func(t *testing.T) {
		// A provider that returns everything regardless of the filter is still
		// correct — only less efficient — because MatchFields is applied again
		// here. That is what makes the Fields contract one rule rather than a
		// per-provider convention.
		ignoring := &countingAttributes{records: records}
		r := NewAttributeRegistry()
		r.MustRegister(AttributeSlotUser, ignoring)
		got, err := r.Enumerate(ctx, AttributeSlotUser, AttributeFilter{Fields: map[string]any{"department": "eng"}})
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if ids := recordIDs(got); !reflect.DeepEqual(ids, []string{"u-1", "u-3"}) {
			t.Fatalf("ids = %v, want [u-1 u-3]", ids)
		}
	})

	t.Run("the registry honors a positive limit a provider ignored", func(t *testing.T) {
		// This is the system-tier admin read of a directory, so a caller asking
		// for more than DefaultListLimit gets more than DefaultListLimit: the
		// registry re-enforces the limit on what a provider returns without
		// clamping it down to a number of its own.
		const want = DefaultListLimit + 50
		many := make([]AttributeRecord, want+25)
		for i := range many {
			many[i] = AttributeRecord{ID: fmt.Sprintf("u-%d", i)}
		}
		ignoring := &countingAttributes{records: many}
		r := NewAttributeRegistry()
		r.MustRegister(AttributeSlotUser, ignoring)
		got, err := r.Enumerate(ctx, AttributeSlotUser, AttributeFilter{Limit: want})
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if len(got) != want {
			t.Fatalf("got %d records, want the caller's limit of %d", len(got), want)
		}
		// The provider must have SEEN the limit the caller asked for, not a
		// number the registry substituted and then truncated to afterwards.
		if ignoring.lastLimit != want {
			t.Fatalf("provider saw limit %d, want the caller's %d", ignoring.lastLimit, want)
		}
	})

	t.Run("a non-positive limit still means DefaultListLimit", func(t *testing.T) {
		many := make([]AttributeRecord, DefaultListLimit+50)
		for i := range many {
			many[i] = AttributeRecord{ID: fmt.Sprintf("u-%d", i)}
		}
		ignoring := &countingAttributes{records: many}
		r := NewAttributeRegistry()
		r.MustRegister(AttributeSlotUser, ignoring)
		got, err := r.Enumerate(ctx, AttributeSlotUser, AttributeFilter{Limit: 0})
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if len(got) != DefaultListLimit {
			t.Fatalf("got %d records, want the DefaultListLimit default of %d", len(got), DefaultListLimit)
		}
		if ignoring.lastLimit != DefaultListLimit {
			t.Fatalf("provider saw limit %d, want the substituted %d", ignoring.lastLimit, DefaultListLimit)
		}
	})

	t.Run("enumeration warms the slot cache", func(t *testing.T) {
		counter := &countingAttributes{records: records}
		r := NewAttributeRegistry()
		r.MustRegister(AttributeSlotUser, counter)
		if _, err := r.Enumerate(ctx, AttributeSlotUser, AttributeFilter{}); err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		if _, err := r.Fetch(ctx, AttributeSlotUser, "u-2"); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if n := counter.fetches.Load(); n != 0 {
			t.Fatalf("provider Fetch called %d times after an enumeration warmed the cache", n)
		}
	})

	t.Run("a plain provider error is coded", func(t *testing.T) {
		r := NewAttributeRegistry()
		r.MustRegister(AttributeSlotUser, &countingAttributes{err: errors.New("boom")})
		_, err := r.Enumerate(ctx, AttributeSlotUser, AttributeFilter{})
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_FETCH", code)
		}
	})
}

// ---------------------------------------------------------------------------
// The value model is reused, not reimplemented
// ---------------------------------------------------------------------------

// TestAttributeBagsUseTheSharedValueModel asserts a bag is a value in the ONE
// metadata value model: the same legal shapes, the same depth cap, the same size
// cap, and — crucially — the same coded error, so an operator reading
// APERTURE_METADATA_INVALID gets the same four fixups whether the offending
// value was an object's or a principal's.
func TestAttributeBagsUseTheSharedValueModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		ok    bool
	}{
		{name: "scalar", value: "eng", ok: true},
		{name: "scalar array", value: []any{"premium", "beta"}, ok: true},
		{name: "flat object", value: map[string]any{"dept": "eng"}, ok: true},
		{name: "one nested object level", value: map[string]any{"lead": map[string]any{"name": "x"}}, ok: true},
		{name: "a canonical date rides as a string", value: "2026-08-25", ok: true},
		{name: "array of objects", value: []any{map[string]any{"id": 1}}},
		{name: "past the depth cap", value: map[string]any{"a": map[string]any{"b": map[string]any{"c": 1}}}},
		{name: "past the size cap", value: strings.Repeat("x", DefaultMaxValueBytes+1)},
		{name: "an unsupported Go type", value: time.Now()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewStaticAttributes([]AttributeRecord{{ID: "u-1", Attributes: Metadata{"f": tc.value}}})
			if tc.ok {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want rejected at load")
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_METADATA_INVALID {
				t.Fatalf("code = %s, want APERTURE_METADATA_INVALID — the same code object "+
					"metadata would produce", code)
			}
			// The object seam must reject the identical value identically.
			objErr := ValidateMetadata(Metadata{"f": tc.value})
			if objErr == nil {
				t.Fatal("the object seam accepted a value the attribute seam rejected; " +
					"there must be exactly one value model")
			}
		})
	}
}

func TestStaticAttributes(t *testing.T) {
	ctx := context.Background()

	t.Run("a duplicate key is refused", func(t *testing.T) {
		_, err := NewStaticAttributes([]AttributeRecord{
			{ID: "u-1", Attributes: Metadata{"department": "eng"}},
			{ID: "u-1", Attributes: Metadata{"department": "sales"}},
		})
		if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
			t.Fatalf("code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", code)
		}
	})

	t.Run("keys that name nobody are refused at load", func(t *testing.T) {
		for _, key := range []string{"", "*"} {
			_, err := NewStaticAttributes([]AttributeRecord{{ID: key}})
			if code := aerr.CodeOf(err); code != aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID {
				t.Fatalf("key %q: code = %s, want APERTURE_ATTRIBUTE_PROVIDER_INVALID", key, code)
			}
		}
	})

	t.Run("List is the unfiltered enumeration, in declaration order", func(t *testing.T) {
		p := mustStaticAttributes(t, []AttributeRecord{
			{ID: "u-2", Attributes: Metadata{"department": "sales"}},
			{ID: "u-1", Attributes: Metadata{"department": "eng"}},
		})
		if p.Len() != 2 {
			t.Fatalf("Len() = %d, want 2", p.Len())
		}
		got, err := p.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if ids := recordIDs(got); !reflect.DeepEqual(ids, []string{"u-2", "u-1"}) {
			t.Fatalf("ids = %v, want declaration order [u-2 u-1]", ids)
		}
	})

	t.Run("an unknown key is APERTURE_NOT_FOUND", func(t *testing.T) {
		p := mustStaticAttributes(t, nil)
		_, err := p.Fetch(ctx, "nobody")
		if code := aerr.CodeOf(err); code != aerr.APERTURE_NOT_FOUND {
			t.Fatalf("code = %s, want APERTURE_NOT_FOUND", code)
		}
	})

	t.Run("the caller's maps are deep-copied in", func(t *testing.T) {
		tags := []any{"premium"}
		src := Metadata{"tags": tags, "owner": map[string]any{"dept": "eng"}}
		p := mustStaticAttributes(t, []AttributeRecord{{ID: "u-1", Attributes: src}})
		// Mutate everything the caller still holds, at every depth.
		tags[0] = "mutated"
		src["owner"].(map[string]any)["dept"] = "mutated"
		src["injected"] = true

		md, err := p.Fetch(ctx, "u-1")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if got := md["tags"].([]any)[0]; got != "premium" {
			t.Fatalf("tags[0] = %v; the caller reached through into a cached bag", got)
		}
		if got := md["owner"].(map[string]any)["dept"]; got != "eng" {
			t.Fatalf("owner.dept = %v; the caller reached through into a cached bag", got)
		}
		if _, injected := md["injected"]; injected {
			t.Fatal("the caller added a field to a bag the provider had already served")
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustStaticAttributes(t *testing.T, records []AttributeRecord) *StaticAttributes {
	t.Helper()
	p, err := NewStaticAttributes(records)
	if err != nil {
		t.Fatalf("NewStaticAttributes: %v", err)
	}
	return p
}

func recordIDs(records []AttributeRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.ID)
	}
	return out
}

// codedDepth counts the Aperture-coded errors in a chain. A same-code re-stamp
// is invisible to CodeOf, so depth is what proves a pass-through actually passed
// through rather than re-wrapping.
func codedDepth(err error) int {
	depth := 0
	for err != nil {
		var ce *aerr.CodedError
		if !errors.As(err, &ce) {
			break
		}
		depth++
		err = errors.Unwrap(ce)
	}
	return depth
}

// countingAttributes is a test AttributeProvider that counts Fetch calls, can be
// made to fail, and deliberately IGNORES the filter it is handed — so the
// registry's own re-enforcement of Fields and Limit is what is under test.
type countingAttributes struct {
	bags      map[string]Metadata
	records   []AttributeRecord
	err       error
	fetches   atomic.Int64
	lastLimit int
}

func (c *countingAttributes) Fetch(_ context.Context, id string) (Metadata, error) {
	c.fetches.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	md, ok := c.bags[id]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"test: no such key", map[string]any{"key": id})
	}
	return md, nil
}

func (c *countingAttributes) List(_ context.Context) ([]AttributeRecord, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.records, nil
}

func (c *countingAttributes) Query(_ context.Context, filter AttributeFilter) ([]AttributeRecord, error) {
	c.lastLimit = filter.Limit
	if c.err != nil {
		return nil, c.err
	}
	return c.records, nil
}
