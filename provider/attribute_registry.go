package provider

import (
	"context"
	"sync"

	aerr "github.com/frankbardon/aperture/errors"
)

// attributeSlotEntry binds one slot's provider to its own cache and the resolved
// config that built it. Each slot's cache is independent — its own TTL, its own
// size cap, its own counters — because the three slots have genuinely different
// change rates and cardinalities: an account's plan changes rarely and there are
// few accounts, while a user directory is large and its bags churn.
type attributeSlotEntry struct {
	provider AttributeProvider
	cache    CacheBackend
	config   CacheConfig
}

// AttributeRegistry maps each of the three attribute slots to its
// AttributeProvider plus a per-slot bag cache. It is the seam the engine and
// rules layers resolve a decision's principal and account attributes through.
// It is safe for concurrent use: providers are registered at startup and read on
// the hot path under an RWMutex, and each per-slot cache is independently
// concurrency-safe.
//
// # It is NOT an object lister, and that is a property of the type
//
// A *Registry deliberately satisfies the scope.ObjectLister contract, so it can
// be handed straight to a scope resolver as the thing that enumerates "all
// objects of a type". An *AttributeRegistry deliberately does NOT, and there is
// no compile-time assertion here to match registry.go's precisely because the
// assertion is the thing being refused.
//
// The reason is not tidiness. A scope resolver enumerating through an
// ObjectLister is answering "which objects does this grant reach?" mid-decision.
// If the principal directory were reachable through that seam, the principal
// table would become an enumerable object set inside a decision — every
// principal in the deployment listable by anything holding a lister, with the
// grant's own scope as the only bound, and no admin tier consulted. Attribute
// enumeration is a system-tier admin read; it is not a scope-resolution source.
//
// Go's typing is STRUCTURAL, so intending that is worth nothing. A method that
// happens to be spelled List(ctx, string, identity.Pattern, int)
// ([]identity.Identity, error) satisfies scope.ObjectLister whether or not
// anybody meant it to, and the wiring mistake it enables is silent — the
// resolver compiles, runs, and enumerates. Containment is therefore structural
// too:
//
//   - enumeration is called Enumerate, not List;
//   - it is keyed by an AttributeSlot, not a bare object-type string;
//   - it takes an AttributeFilter, which has no identity.Pattern to bound with
//     (see AttributeFilter);
//   - it returns []AttributeRecord — bare string keys — not
//     []identity.Identity.
//
// Any one of those makes the signature unassignable; all four make it
// unassignable by accident. TestAttributeRegistryIsNotAScopeLister asserts the
// negative against the real scope.ObjectLister interface, so the guarantee
// cannot rot into a comment.
type AttributeRegistry struct {
	mu       sync.RWMutex
	slots    map[AttributeSlot]*attributeSlotEntry
	defaults CacheConfig
	newCache func(CacheConfig) CacheBackend
}

// AttributeRegistryOption configures an AttributeRegistry at construction.
type AttributeRegistryOption func(*AttributeRegistry)

// WithAttributeDefaultCacheConfig sets the cache config a slot inherits when it
// is registered with no per-slot overrides. Unset fields still fall back to the
// package defaults (DefaultTTL / DefaultMaxSize) at cache construction.
func WithAttributeDefaultCacheConfig(cfg CacheConfig) AttributeRegistryOption {
	return func(r *AttributeRegistry) { r.defaults = cfg }
}

// WithAttributeCacheFactory swaps the cache backend constructor every per-slot
// cache is built from. The default builds a MemoryCache; a host supplies this to
// plug a custom CacheBackend. A networked backend (e.g. Redis) is out of scope.
func WithAttributeCacheFactory(f func(CacheConfig) CacheBackend) AttributeRegistryOption {
	return func(r *AttributeRegistry) {
		if f != nil {
			r.newCache = f
		}
	}
}

// NewAttributeRegistry returns a registry with no slot filled. Each slot is
// registered separately and gets its own in-memory LRU cache tuned by the
// registry defaults plus that slot's options.
//
// A slot left unregistered is not an error at construction: a deployment with no
// machine principals wires no machine provider. It becomes
// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED at the first fetch against that slot,
// which is a configuration diagnostic rather than an empty bag silently reading
// as "this principal has no attributes".
func NewAttributeRegistry(opts ...AttributeRegistryOption) *AttributeRegistry {
	r := &AttributeRegistry{
		slots:    make(map[AttributeSlot]*attributeSlotEntry, len(AttributeSlots())),
		newCache: func(cfg CacheConfig) CacheBackend { return NewMemoryCache(cfg) },
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register binds provider to slot with a per-slot cache configured from the
// registry defaults plus opts. A slot outside the closed set is
// APERTURE_ATTRIBUTE_SLOT_UNKNOWN; a nil provider or a second registration for a
// slot that already has one is APERTURE_ATTRIBUTE_PROVIDER_INVALID.
//
// A duplicate is refused rather than replaced because "last writer wins" over a
// slot is how one deployment's directory quietly shadows another's during
// wiring, and the failure surfaces as attributes that are merely wrong rather
// than absent.
func (r *AttributeRegistry) Register(slot AttributeSlot, provider AttributeProvider, opts ...CacheOption) error {
	if !slot.Valid() {
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			"provider: cannot register an attribute provider under an unknown slot",
			map[string]any{"slot": string(slot), "slots": slotNames()})
	}
	if provider == nil {
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"provider: cannot register a nil attribute provider",
			map[string]any{"slot": string(slot)})
	}
	cfg := r.defaults
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg = cfg.withDefaults()

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.slots[slot]; dup {
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"provider: attribute slot already has a registered provider",
			map[string]any{"slot": string(slot)})
	}
	r.slots[slot] = &attributeSlotEntry{
		provider: provider,
		cache:    r.newCache(cfg),
		config:   cfg,
	}
	return nil
}

// MustRegister is Register that panics on error; for host startup wiring where a
// registration failure is a programming error.
func (r *AttributeRegistry) MustRegister(slot AttributeSlot, provider AttributeProvider, opts ...CacheOption) {
	if err := r.Register(slot, provider, opts...); err != nil {
		panic(err)
	}
}

// Has reports whether slot has a registered provider. An unknown slot is simply
// false — Has is a question, not an assertion.
func (r *AttributeRegistry) Has(slot AttributeSlot) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.slots[slot]
	return ok
}

// RegisteredSlots returns the slots that have a provider, in AttributeSlots()
// order so the result is stable and diffable. It is deliberately not called
// Keys: the KEY SET of this registry is fixed at three, and what varies is which
// of them are filled.
func (r *AttributeRegistry) RegisteredSlots() []AttributeSlot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AttributeSlot, 0, len(r.slots))
	for _, slot := range AttributeSlots() {
		if _, ok := r.slots[slot]; ok {
			out = append(out, slot)
		}
	}
	return out
}

// entry resolves slot's entry, or the coded error that says why it cannot: an
// unknown slot is APERTURE_ATTRIBUTE_SLOT_UNKNOWN (a programming error at the
// call site), an empty one is APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED (a wiring
// gap). The two are distinct because their fixups are.
func (r *AttributeRegistry) entry(slot AttributeSlot) (*attributeSlotEntry, error) {
	if !slot.Valid() {
		return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			"provider: not an attribute slot",
			map[string]any{"slot": string(slot), "slots": slotNames()})
	}
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED,
			"provider: no attribute provider registered for slot",
			map[string]any{"slot": string(slot)})
	}
	return e, nil
}

// Fetch returns the attribute bag for id in slot, serving it from that slot's
// cache when fresh and otherwise pulling it through the provider and caching the
// result. A cache hit never calls the provider.
//
// id is a BARE KEY — a principal id for the user and machine slots, an account
// id for the account slot — and Aperture never parses it. Two keys are refused
// outright, before any provider is consulted: the empty string, which names
// nobody, and "*", the account wildcard, which would ask for the attributes of
// every account and could only be answered with one account's data served as
// another's.
//
// The returned bag is READ-ONLY, transitively. It is the whole decision's view
// of this subject, shared across every object being checked and every concurrent
// decision for the same key, so a write through it is not one bad read — see the
// blast-radius note in attribute.go.
func (r *AttributeRegistry) Fetch(ctx context.Context, slot AttributeSlot, id string) (Metadata, error) {
	e, err := r.entry(slot)
	if err != nil {
		return nil, err
	}
	if err := attributeKeyError(slot, id); err != nil {
		return nil, err
	}
	if md, ok := e.cache.Get(id); ok {
		return md, nil
	}
	md, err := e.provider.Fetch(ctx, id)
	if err != nil {
		return nil, attributeError(err)
	}
	e.cache.Set(id, md)
	return md, nil
}

// Enumerate returns up to filter.Limit records of slot that satisfy
// filter.Fields, by querying the slot's provider and re-enforcing both bounds on
// what comes back. It opportunistically warms the slot's cache with each
// returned bag, since the provider call already paid to produce it. A positive
// limit is honoured as given; a non-positive one means DefaultListLimit.
//
// This read is UNCAPPED on purpose. It is the SYSTEM-TIER ADMIN READ of a
// directory, and an operator answering "who is in the user slot?" may legitimately
// need the whole of it — a page size chosen here would only make the honest
// answer arrive in pieces. It is not a scope-resolution source, and its signature
// is built so it cannot be mistaken for one — see the type doc on
// AttributeRegistry for why each part of it differs from scope.ObjectLister.List.
// What keeps it safe is the authority required to reach it (service tier), not a
// number in this package.
//
// Fields is re-enforced through MatchFields rather than trusted to the provider.
// The object Registry leaves Fields entirely to its provider because there
// Query's answer bounds an authorization; here the registry is the only shared
// place the predicate can be applied identically for every provider, and the
// cost is one map walk over an already-bounded page. A provider that pushes the
// predicate into its own storage still passes, because MatchFields is the rule
// it was pushing down.
func (r *AttributeRegistry) Enumerate(ctx context.Context, slot AttributeSlot, filter AttributeFilter) ([]AttributeRecord, error) {
	e, err := r.entry(slot)
	if err != nil {
		return nil, err
	}
	limit := boundLimit(filter.Limit)
	filter.Limit = limit
	records, err := e.provider.Query(ctx, filter)
	if err != nil {
		return nil, attributeError(err)
	}
	out := make([]AttributeRecord, 0, len(records))
	for _, rec := range records {
		if !MatchFields(rec.Attributes, filter.Fields) {
			continue
		}
		if rec.Attributes != nil {
			e.cache.Set(rec.ID, rec.Attributes)
		}
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// principalSlot maps a PRINCIPAL KIND to the slot that serves it. It is a
// deliberately narrower door than ParseAttributeSlot: only user and machine are
// principal kinds, and "account" — a real slot — is not one.
//
// Routing a principal fetch through ParseAttributeSlot would let the string
// "account" resolve the ACCOUNT directory and serve a tenant's bag as a
// principal's. There is no caller that wants that, so the mapping that makes it
// expressible does not exist. An unrecognised or empty kind reports false rather
// than defaulting: a machine answered for out of the human directory is exactly
// the substitution PrincipalResolver's contract forbids.
func principalSlot(kind string) (AttributeSlot, bool) {
	switch AttributeSlot(kind) {
	case AttributeSlotUser:
		return AttributeSlotUser, true
	case AttributeSlotMachine:
		return AttributeSlotMachine, true
	default:
		return "", false
	}
}

// Attributes resolves a principal's attribute bag by KIND, so a user principal
// is answered from the user slot and a machine principal from the machine slot.
//
// Its signature is rules.PrincipalResolver's, so an *AttributeRegistry is handed
// straight to rules.WithPrincipalResolver — structurally, without this package
// importing rules (provider imports only identity and errors). That direction is
// the same one *Registry already satisfies rules.MetadataFetcher in.
//
// It returns the host's bag alone. The `id` and `kind` a rule reads off
// `principal` are the rules engine's floor, stamped over whatever comes back —
// this method never invents them, and a nil return is the correct, complete
// answer for "the host knows nothing about this principal".
//
// # Leniency, and its one deliberate limit
//
// A missing SOURCE is not a failed decision. Two cases yield a nil bag and no
// error:
//
//   - the kind names no principal slot (an empty kind is the live case — a
//     decision path that never had the principal's record in hand), and
//   - the slot has no registered provider (APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED),
//     or a registered one has no record for this key (APERTURE_NOT_FOUND).
//
// A deployment that wires a user directory and no machine directory must keep
// deciding, and it does: its machine principals evaluate against the floor. This
// mirrors internal/cli's lenientFetcher, which collapses the same two codes for
// object metadata and for the same reason — an absent source must not become a
// non-decision.
//
// Everything else — a directory that is unreachable, a bag the value model
// rejects — surfaces VERBATIM, keeping its code and its registry fixups, and the
// caller treats it as a non-decision. An outage must not read as "this principal
// has no attributes", because that is an authorization change wearing an
// infrastructure failure's clothes.
//
// A key that can never name one subject is a third thing, and it is refused
// rather than collapsed: an empty key, or the account wildcard, is a CALLER that
// has not resolved what it is asking about, not a deployment that chose not to
// wire a directory (see attributeKeyError).
//
// # The asymmetry leniency leaves, which is accepted rather than solved
//
// An absent attribute makes every comparison against it false. That is deny-safe
// in an INCLUSIVE grant, where a rule that fails to select covers nothing — and
// access-WIDENING in an EXCLUSIVE one, where selection means "excluded", so a
// rule that stops selecting stops excluding and the object the exclusion was
// written to withhold becomes covered.
//
// The alternative is worse in the direction that matters more: erroring on an
// absent bag makes a deployment with an unwired slot undecidable for every
// principal of that kind, which is the outage the leniency exists to prevent.
// The mitigation is therefore VISIBILITY, not refusal — a decision trace says
// when a bag came back floor-only — plus `principal.kind`, which is what lets a
// rule author state a rule's dependence on a directory instead of hiding it (see
// rules.principalBag). rules.TestAMissingBagWidensAnExclusiveGrant keeps the
// hazard executable, so it cannot quietly stop being true.
func (r *AttributeRegistry) Attributes(ctx context.Context, kind, principal string) (map[string]any, error) {
	slot, ok := principalSlot(kind)
	if !ok {
		return nil, nil
	}
	md, err := r.Fetch(ctx, slot, principal)
	if err != nil {
		switch aerr.CodeOf(err) {
		case aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, aerr.APERTURE_NOT_FOUND:
			return nil, nil
		default:
			return nil, err
		}
	}
	return md, nil
}

// AccountAttributes resolves the ACTIVE account's attribute bag from the account
// slot — the tenant's plan, region, feature flags a rule reads off `account`.
//
// Its signature is rules.AccountResolver's, so an *AttributeRegistry is handed
// straight to rules.WithAccountResolver, structurally, without this package
// importing rules. It is spelled AccountAttributes rather than Attributes so that
// ONE registry can satisfy both resolver seams: Go has no overloading, and
// Attributes is already the principal seam's method. A host wires the same reg
// into WithPrincipalResolver and WithAccountResolver and gets both directories
// and both caches from it.
//
// It returns the host's bag alone. The `id` a rule reads off `account` is the
// rules engine's floor, stamped over whatever comes back, and a nil return is the
// correct, complete answer for "the host knows nothing about this account".
//
// # The wildcard is a call-site bug, not a wiring gap
//
// account is a concrete account id. "*" — the all-accounts grant sentinel — is
// refused with APERTURE_ATTRIBUTE_PROVIDER_INVALID (by Fetch's shared key guard,
// so the refusal has exactly one definition), and it is refused rather than
// answered leniently because the two failures are not the same kind of thing. An
// unregistered slot is a deployment that has no account directory and must keep
// deciding; a wildcard key is a caller that has not resolved the sentinel to the
// account the decision is actually in, and the only bag that could satisfy it is
// one account's data served as another's. The rules engine resolves the sentinel
// to the floor before it ever gets here, so this is the backstop.
//
// # Leniency, otherwise identical to the principal seam
//
// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED (no account directory wired) and
// APERTURE_NOT_FOUND (a directory that has no record for this account) both yield
// a nil bag and no error: the decision proceeds against the floor. Everything
// else — an unreachable directory, a bag the value model rejects — surfaces
// VERBATIM with its code and its registry fixups, and the caller treats it as a
// non-decision. An outage must not read as "this account has no attributes".
func (r *AttributeRegistry) AccountAttributes(ctx context.Context, account string) (Metadata, error) {
	md, err := r.Fetch(ctx, AttributeSlotAccount, account)
	if err != nil {
		switch aerr.CodeOf(err) {
		case aerr.APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, aerr.APERTURE_NOT_FOUND:
			return nil, nil
		default:
			return nil, err
		}
	}
	return md, nil
}

// Invalidate drops the cached bag for id in slot so the next Fetch re-pulls it
// from the provider. It reports whether an entry was present. An unknown slot is
// APERTURE_ATTRIBUTE_SLOT_UNKNOWN and an unfilled one is
// APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, exactly as a Fetch would report
// them.
//
// # This is a security control, not a performance knob
//
// The object Registry's Invalidate (registry.go) exists so a host whose source
// of truth changed can stop serving a stale row. The three methods here have the
// same shape and a different reason, and the difference is worth stating because
// it decides how a deployment tunes ttl:.
//
// An object's metadata going stale for a TTL is usually tolerable: a document's
// category is a fact about a thing, and a decision made against yesterday's
// category is wrong in the way a cache is always wrong. An attribute bag is the
// ASKER'S standing — the clearance, the department, the plan — and a revoked
// clearance that stays cached keeps AUTHORIZING for the rest of the window.
// Principals are the classic revoke case, so the freshness window a slot's ttl:
// buys in fetch traffic is paid for in the time a revocation takes to become
// true. An operator who has just removed someone's access needs a way to make
// that true now; this is that way.
//
// The key is validated through the SAME guard Fetch uses, so the empty string
// and the account wildcard are refused here as well. Neither can ever have been
// cached — Fetch refuses them before a provider is consulted — so the refusal
// costs a caller nothing it could otherwise have had, and it answers "why did
// invalidating '*' report nothing?" with the reason instead of a false.
func (r *AttributeRegistry) Invalidate(slot AttributeSlot, id string) (bool, error) {
	e, err := r.entry(slot)
	if err != nil {
		return false, err
	}
	if err := attributeKeyError(slot, id); err != nil {
		return false, err
	}
	return e.cache.Delete(id), nil
}

// InvalidateSlot clears every cached bag for slot. Use it when a whole directory
// changed underneath the cache — a bulk role sync, a department reorg, a
// restored backup — rather than invalidating each key the change touched.
//
// It is spelled InvalidateSlot, not InvalidateType: the object registry's unit
// is an open-ended object TYPE and this one's is a closed SLOT, and the two are
// deliberately different words everywhere they appear (see the type doc).
func (r *AttributeRegistry) InvalidateSlot(slot AttributeSlot) error {
	e, err := r.entry(slot)
	if err != nil {
		return err
	}
	e.cache.Clear()
	return nil
}

// InvalidateAll clears every registered slot's cache. It does not unregister
// providers, and it is not an error on a registry with no slot filled — "drop
// everything" is satisfiable by a registry that is holding nothing.
//
// It is the blunt instrument, and the right one exactly once: an operator who
// knows a directory changed but not which subjects it changed. Every decision
// that follows re-reads its bags, so a large slot pays a burst of provider
// traffic; that is the price of the guarantee, and it is cheaper than the window
// it closes.
func (r *AttributeRegistry) InvalidateAll() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.slots {
		e.cache.Clear()
	}
}

// Stats returns the cache counters for slot, or false when the slot has no
// registered provider. The counters are per slot and never pooled: a user
// directory's hit rate says nothing about an account cache's, and one number
// covering both would hide the slot that is actually missing.
func (r *AttributeRegistry) Stats(slot AttributeSlot) (Stats, bool) {
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return Stats{}, false
	}
	return e.cache.Stats(), true
}

// CacheConfigFor returns the resolved cache configuration a slot was registered
// with, or false when the slot is empty. It is how a caller confirms that a
// per-slot override actually took (the defaults are filled in at registration,
// so the returned config is what the cache is really running, not what was
// passed).
func (r *AttributeRegistry) CacheConfigFor(slot AttributeSlot) (CacheConfig, bool) {
	r.mu.RLock()
	e, ok := r.slots[slot]
	r.mu.RUnlock()
	if !ok {
		return CacheConfig{}, false
	}
	return e.config, true
}
