package rules

import (
	"bytes"
	"context"
	"crypto/sha256"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// Rule is a named rule definition: a reference label plus the AST that decides
// it. Rule is the unit a RuleSource resolves and the editor/state-file persist,
// so it serializes to stable JSON alongside its AST.
type Rule struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	AST         *Node  `json:"ast"`
}

// RuleSource resolves a scope strategy's opaque rule reference to its definition.
// A host backs it with whatever store holds rules (a config map, a database, the
// state file); MapSource is the in-memory default. A reference with no matching
// rule yields APERTURE_RULE_NOT_FOUND.
type RuleSource interface {
	Lookup(ctx context.Context, ref string) (*Rule, error)
}

// MapSource is an in-memory RuleSource keyed by rule reference. It is the simple
// default for tests and static configurations.
type MapSource map[string]*Rule

// Lookup resolves ref, returning APERTURE_RULE_NOT_FOUND when it is absent.
func (m MapSource) Lookup(_ context.Context, ref string) (*Rule, error) {
	r, ok := m[ref]
	if !ok || r == nil {
		return nil, aerr.WithContext(aerr.APERTURE_RULE_NOT_FOUND,
			"rule: no rule registered for reference", map[string]any{"rule": ref})
	}
	return r, nil
}

// MetadataFetcher supplies an object's metadata for the evaluation context. Its
// signature matches *provider.Registry.Fetch (provider.Metadata is map[string]any),
// so a *provider.Registry is wired directly as the fetcher without this package
// importing provider. The returned map is treated as read-only.
type MetadataFetcher interface {
	Fetch(ctx context.Context, id identity.Identity) (map[string]any, error)
}

// PrincipalResolver supplies a principal's attribute bag for the evaluation
// context, keyed by principal kind and id. A host wires one (a directory, a
// *provider.AttributeRegistry) when its rules need principal attributes; without
// one, `principal` is exactly the floor bag. The returned map is treated as
// read-only, transitively — the engine copies it rather than writing into it,
// and no other holder writes to it at any depth.
//
// A resolver returns ONLY what the host knows. It does not need to publish `id`
// or `kind`: the engine stamps the floor over whatever comes back (see
// principalBag), so a resolver that returns nil is not an error and not an empty
// `principal` — it is the floor.
//
// kind is model.PrincipalKind's spelling — "user" or "machine" — passed as a
// string because rules imports no model. It is what lets one resolver dispatch to
// a different attribute source per kind (a human directory and a service-account
// registry are rarely the same store) without re-deriving the kind from the id.
// An empty kind means the caller did not have the principal's record in hand: a
// resolver treats that as unknown, and must not substitute a default kind, or a
// machine would silently be answered for out of the human directory.
type PrincipalResolver interface {
	Attributes(ctx context.Context, kind, principal string) (map[string]any, error)
}

// AccountResolver supplies the ACTIVE account's attribute bag for the evaluation
// context — the tenant's plan, region, feature flags a rule reads off `account`.
// A host wires one (a *provider.AttributeRegistry) when its rules need account
// attributes; without one, `account` is exactly the floor bag. The returned map
// is treated as read-only, transitively — the engine copies it rather than
// writing into it, and no other holder writes to it at any depth.
//
// Like PrincipalResolver it returns ONLY what the host knows: the `id` a rule
// reads off `account` is the engine's floor, stamped over whatever comes back
// (see accountBag), so a resolver that returns nil is not an error and not an
// empty `account` — it is the floor.
//
// # Why the method is not called Attributes
//
// So that ONE type can satisfy both seams. A *provider.AttributeRegistry serves
// the principal slots and the account slot, and Go has no overloading: reusing
// the name would force a host to wire two objects, or to wrap one, where the
// registry already holds both directories and both caches. The distinct name is
// what keeps `rules.WithPrincipalResolver(reg)` and
// `rules.WithAccountResolver(reg)` the same reg.
//
// # The wildcard is not an account
//
// account is a concrete account id. The account wildcard "*" never reaches a
// resolver — the engine resolves it to the floor without consulting one (see
// Engine.accountAttributes) — so an implementation never has to decide what
// "the attributes of every account" would mean.
type AccountResolver interface {
	AccountAttributes(ctx context.Context, account string) (map[string]any, error)
}

// The floor keys. `principal` always carries these two, whatever resolver is
// wired and whether or not it answered.
const (
	principalKeyID   = "id"
	principalKeyKind = "kind"
)

// The two attribute roots, spelled once for the diagnostic channel: a floor-only
// note names the root it is about (see NoteAttributesFloorOnly), and the root is
// the whole of what that note discloses. They are the same words allowedRoots
// (ast.go) admits, which is frozen — `principal` and `account` are two of the
// four things a rule may name.
const (
	principalRoot = "principal"
	accountRoot   = "account"
)

// accountKeyID is the `account` root's single floor key. It is spelled
// separately from principalKeyID rather than shared: the two floors are
// independent contracts that happen to agree on one word today, and a shared
// constant would make renaming one silently rename the other.
const accountKeyID = "id"

// accountWildcard is model.AccountWildcard ("*") spelled as a literal, because
// rules imports no model — the same reason provider spells it out in
// attribute.go.
//
// It is a grant/membership SENTINEL meaning "every account", never an account
// that exists: model.ValidateAccount refuses to store a row under it. A decision
// can still be made with it as the active account — that is how platform-tier
// authority is anchored — and what `account` means there is handled in
// Engine.accountAttributes.
const accountWildcard = "*"

// accountBag builds the `account` root: a FRESH map holding the resolver's
// attributes with the floor — the account id — stamped over them.
//
// # Why the floor is {id} and not {id, kind}
//
// `principal` publishes a kind because attribute providers are registered per
// principal kind, so which directory answered is a fact a rule author needs in
// order to state a rule's dependence on it. An account has no such fact: there
// is one account slot, it answers for every account, and a `kind` key would be
// the same constant in every bag in every deployment — a value that can never
// discriminate is not information, it is noise a rule author would eventually
// write a comparison against.
//
// What remains is `id`, and it earns its place for the reason the floor exists
// at all: it is the part of `account` that does not depend on what the host
// happened to configure, so `account.id == "acme"` is writable against ANY
// deployment, wired or not.
//
// # Why the floor wins on collision
//
// The floor is stamped LAST, exactly as principalBag does it and for the same
// reason. A host's account table with its own internal `id` column is an
// ordinary accident, not an attack; if it could shadow the floor then
// `account.id == object.account` would silently start comparing a surrogate key
// against an account id and the rule would answer a different question with no
// error anywhere. A floor that can be shadowed is not a floor.
//
// # Why a fresh map
//
// bag may be a cached attribute bag shared across every object in this decision
// and every concurrent decision in the same account — which, for the account
// slot, is every decision the tenant is making at once. Stamping into it would
// be a write through a read-only value at the widest blast radius Aperture has.
func accountBag(bag map[string]any, account string) map[string]any {
	out := make(map[string]any, len(bag)+1)
	maps.Copy(out, bag)
	out[accountKeyID] = account
	return out
}

// principalBag builds the `principal` root: a FRESH map holding the resolver's
// attributes with the floor — id and kind — stamped over them.
//
// # Why the floor is the engine's job, not the resolver's
//
// Every resolver gets it, including a host's own. A rule author can therefore
// write principal.id and principal.kind against ANY deployment, wired or not,
// which is the whole point of a floor: it is the part of `principal` that does
// not depend on what the host happened to configure.
//
// # Why `kind` is published at all
//
// Attribute providers are registered PER KIND, so a rule that reads a user
// directory's field is silently kind-dependent: for a machine principal that
// field is simply absent and the comparison is false. In an inclusive grant that
// is deny-safe; in an EXCLUSIVE one — where selection means "excluded" — a rule
// that quietly stops selecting WIDENS access. principal.kind is the tool an
// author needs to say which kinds a rule is about, so
// `principal.kind == "user" && principal.tier == "gold"` states the dependence
// instead of hiding it.
//
// # Why the floor wins on collision
//
// The floor is stamped LAST, so a host bag carrying its own "id" or "kind" key
// cannot shadow it. Not primarily as a defence — a directory is host-trusted
// infrastructure — but because the realistic collision is innocent: a directory
// with its own internal `id` column would silently redefine what
// `principal.id == object.owner` compares, and an ownership rule would start
// answering a different question with no error anywhere. A floor that can be
// shadowed is not a floor.
//
// # Why a fresh map
//
// bag may be a cached attribute bag, shared across every object in this decision
// and every concurrent decision for the same principal. Stamping into it would
// be a write through a read-only value at the widest blast radius Aperture has.
func principalBag(bag map[string]any, kind, principal string) map[string]any {
	out := make(map[string]any, len(bag)+2)
	maps.Copy(out, bag)
	out[principalKeyID] = principal
	// Published even when empty. An unknown kind is a value a rule can compare
	// against ("" matches neither "user" nor "machine"); an ABSENT key would make
	// "unknown" and "not published by this build" the same observation.
	out[principalKeyKind] = kind
	return out
}

// recordFloorOnly notes each attribute root that THIS rule reads a host-defined
// field off and that resolved to NOTHING but the engine's floor — so a rule
// written against `principal.tier` says the tier was never there, instead of
// merely failing to select.
//
// The bags are the RESOLVERS' answers, before principalBag/accountBag stamp the
// floor into them, which is what makes "floor-only" a plain emptiness check: an
// empty resolver bag is exactly a root that carries the floor alone.
//
// # Why the rule's own paths gate it
//
// A note is only worth recording where the hazard exists. A rule that reads
// nothing but `object.*`, or nothing but `principal.id`, is unaffected by an
// unwired directory: the floor is always there and it always says the same
// thing. Recording a floor-only note for those rules would put two lines on
// every trace of every rule-backed grant in the very common deployment that
// wires no attribute provider at all, and the traces where the note MATTERS
// would be the ones nobody could find.
//
// The gate is on what the rule NAMES, not on what a comparison did, and that is
// the deliberate half. `principal.tier == "contractor"` earns the note whether
// the comparison was reached, short-circuited away by an && to its left, or
// evaluated and found false — because in every one of those cases the operator
// reading the trace is looking at a rule whose author expected a field the
// deployment did not supply.
//
// It is called only from the collector branch of Selected, so the AST walk — and
// the note — cost Check and Enumerate nothing.
//
// THE ACCOUNT WILDCARD LANDS HERE TOO, and truthfully. A decision made at
// platform scope resolves no account bag at all (see Engine.accountAttributes:
// "*" is not an account and never reaches a resolver), so a rule reading
// `account.plan` there really is comparing against nothing, which is worth
// saying out loud for the same reason an unwired slot is.
func recordFloorOnly(sink *NoteCollector, ast *Node, principal, account map[string]any) {
	if len(principal) == 0 && readsBeyondFloor(ast, principalRoot, principalKeyID, principalKeyKind) {
		sink.Add(Note{Kind: NoteAttributesFloorOnly, Path: principalRoot})
	}
	if len(account) == 0 && readsBeyondFloor(ast, accountRoot, accountKeyID) {
		sink.Add(Note{Kind: NoteAttributesFloorOnly, Path: accountRoot})
	}
}

// readsBeyondFloor reports whether n names any variable path rooted at root
// whose first segment past the root is outside floor — that is, whether the rule
// reads something only a HOST provider could have supplied.
//
// A bare reference to the root itself (`principal`, with no path under it)
// counts: a rule handed the whole bag reads whatever is in it, so an empty bag
// is exactly the surprise this note exists for.
//
// The walk covers every field a Node can hang a child off — Left, Right,
// Children, Items — rather than switching on Type, so a node type that gains a
// child position cannot silently stop being scanned. It is a diagnostic-path
// walk over a rule AST (tens of nodes, not thousands), performed once per
// evaluation only when a collector is installed.
func readsBeyondFloor(n *Node, root string, floor ...string) bool {
	if n == nil {
		return false
	}
	if n.Type == NodeVar && n.Name != "" {
		if path, ok := strings.CutPrefix(n.Name, root); ok {
			switch {
			case path == "":
				// The whole bag.
				return true
			case path[0] == '.':
				field := path[1:]
				if i := indexByte(field, '.'); i >= 0 {
					field = field[:i]
				}
				if !slices.Contains(floor, field) {
					return true
				}
			}
			// Anything else shares a prefix without sharing a root
			// (`principality.x`) and is not this root at all.
		}
	}
	if readsBeyondFloor(n.Left, root, floor...) || readsBeyondFloor(n.Right, root, floor...) {
		return true
	}
	for _, c := range n.Children {
		if readsBeyondFloor(c, root, floor...) {
			return true
		}
	}
	for _, it := range n.Items {
		if readsBeyondFloor(it, root, floor...) {
			return true
		}
	}
	return false
}

// Engine is the rules engine: it resolves a rule reference, compiles-and-caches
// it, builds the evaluation context from object metadata and principal/action,
// and evaluates. It satisfies scope.RuleEvaluator (see scope.go), so the
// inclusive/exclusive scope resolvers get their rule-backed variant by wiring an
// Engine as scope.Deps{Rules: engine}.
//
// Engine is safe for concurrent use: the compiler options are read-only and the
// compiled-rule cache is concurrency-safe.
type Engine struct {
	source    RuleSource
	fetcher   MetadataFetcher
	principal PrincipalResolver
	account   AccountResolver
	compiler  *Compiler
	cache     *compiledCache
	// clock is the engine's single time source: the compiled-rule cache expires
	// entries against it AND every decision takes its reference instant from it.
	// See Clock and WithClock.
	clock Clock
}

// Option configures an Engine.
type Option func(*engineConfig)

type engineConfig struct {
	compilerOpts []CompilerOption
	ttl          time.Duration
	clock        Clock
	principal    PrincipalResolver
	account      AccountResolver
}

// WithFunction registers a pure host function callable from rules (expr-lang's
// Function seam). See Function.
func WithFunction(name string, fn func(args ...any) (any, error)) Option {
	return func(c *engineConfig) { c.compilerOpts = append(c.compilerOpts, Function(name, fn)) }
}

// WithCacheTTL bounds how long a compiled rule stays cached. TTL <= 0 (the
// default) keeps compiled rules until explicitly invalidated.
func WithCacheTTL(ttl time.Duration) Option {
	return func(c *engineConfig) { c.ttl = ttl }
}

// WithClock injects the engine's SINGLE time source. Defaults to the real clock.
//
// It drives BOTH of the engine's time-dependent behaviours:
//
//   - compiled-rule cache TTL expiry (WithCacheTTL), and
//   - the per-decision reference instant rule date comparisons resolve against
//     (now.go), snapshotted once per decision and always converted to UTC.
//
// The second of those is the wider promise: an injected clock now decides what
// "now" MEANS to a rule, not merely when a cached program is dropped. Pinning the
// clock is therefore how a date rule is tested reproducibly — and, as the flip
// side of one time source, a pinned clock also FREEZES cache expiry, so a TTL'd
// entry under a clock that never advances never expires. That coupling is
// intended: two independent clocks inside one engine can disagree, which is worse
// than the coupling (see Clock).
func WithClock(clk Clock) Option {
	return func(c *engineConfig) { c.clock = clk }
}

// WithPrincipalResolver supplies principal attributes to the evaluation context.
// Without it, `principal` is the floor bag alone: its id and its kind. A
// *provider.AttributeRegistry is wired straight in here — it resolves the
// principal's kind to an attribute slot and fetches through that slot's provider.
func WithPrincipalResolver(r PrincipalResolver) Option {
	return func(c *engineConfig) { c.principal = r }
}

// WithAccountResolver supplies the active account's attributes to the evaluation
// context. Without it, `account` is the floor bag alone: the account id. A
// *provider.AttributeRegistry is wired straight in here — the same registry that
// serves WithPrincipalResolver — and it reads the account slot.
func WithAccountResolver(r AccountResolver) Option {
	return func(c *engineConfig) { c.account = r }
}

// NewEngine builds an Engine over a rule source and a metadata fetcher. A nil
// fetcher means object metadata is empty (for rules that read only the
// principal/action context). Pass a *provider.Registry as the fetcher to read
// real object metadata.
func NewEngine(source RuleSource, fetcher MetadataFetcher, opts ...Option) *Engine {
	cfg := &engineConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	var principal PrincipalResolver = floorPrincipal{}
	if cfg.principal != nil {
		principal = cfg.principal
	}
	var account AccountResolver = floorAccount{}
	if cfg.account != nil {
		account = cfg.account
	}
	// One clock, resolved once here and shared: the cache and the decision
	// instant must never be able to read different sources.
	var clock Clock = realClock{}
	if cfg.clock != nil {
		clock = cfg.clock
	}
	return &Engine{
		source:    source,
		fetcher:   fetcher,
		principal: principal,
		account:   account,
		compiler:  NewCompiler(cfg.compilerOpts...),
		cache:     newCompiledCache(cfg.ttl, clock),
		clock:     clock,
	}
}

// Selected reports whether object is selected by the named rule for the given
// account/principal/action context. It is the scope.RuleEvaluator implementation
// the rule-backed inclusive/exclusive scope resolvers consult: an inclusive grant
// covers an object Selected reports true for, an exclusive grant excludes one.
//
// The flow is: resolve the rule reference, compile-and-cache its AST, fetch the
// object's metadata, build the context (including the decision's reference
// instant), and evaluate. Any failure is an APERTURE_* coded error and the
// resolver treats it as a non-decision — there is no select-on-error.
//
// account is the ACTIVE account the decision is being made in, and it is what
// `account` resolves against: the bag describes the tenancy the decision is
// happening in, never the account a grant happens to be stamped to. A
// wildcard-stamped grant ("*", the all-accounts sentinel) is evaluated inside
// whatever account is active, so reading its attributes from the stamp would give
// one tenant's decision another tenant's plan.
func (e *Engine) Selected(ctx context.Context, rule string, object identity.Identity, account, principalKind, principal, action string) (bool, error) {
	r, err := e.source.Lookup(ctx, rule)
	if err != nil {
		return false, err
	}
	if r == nil || r.AST == nil {
		return false, aerr.WithContext(aerr.APERTURE_RULE_NOT_FOUND,
			"rule: reference resolved to an empty rule", map[string]any{"rule": rule})
	}
	compiled, err := e.compile(r.AST)
	if err != nil {
		return false, err
	}
	metadata, err := e.metadata(ctx, object)
	if err != nil {
		return false, err
	}
	// The decision's attribute memo, or nil when this evaluation is unscoped.
	// Both lookups below go through it, so under a decision scope the principal
	// and the account are resolved ONCE for the whole decision however many
	// objects and rules it evaluates, and every evaluation sees the same two
	// bags. Unscoped, each resolves its own. See attributes.go.
	attrs := decisionAttributesFrom(ctx)
	principalAttrs, err := attrs.principalAttributes(ctx, e.principal, principalKind, principal)
	if err != nil {
		return false, err
	}
	accountAttrs, err := e.accountAttributes(ctx, attrs, account)
	if err != nil {
		return false, err
	}
	in := Input{
		Object: metadata,
		// The resolver's answer with the floor stamped over it, in a fresh map —
		// the resolver's bag may be cached and shared, and is read-only.
		Principal: principalBag(principalAttrs, principalKind, principal),
		// Same contract on the account side: the account resolver's bag is cached
		// and shared across the whole tenancy, and the floor is stamped into a
		// copy of it, never into it.
		Account: accountBag(accountAttrs, account),
		Action:  action,
		// The reference instant, read from the engine's one clock. Under a
		// decision scope (the decision engine opens one per Check / Enumerate /
		// Explain) every evaluation in that decision shares the first instant
		// taken; unscoped, this evaluation takes its own. Either way it is read
		// ONCE here and threaded as data, so a rule referring to it twice cannot
		// straddle a tick. See now.go.
		Now: decisionInstantFrom(ctx).snapshot(e.clock),
	}

	// Fast path: no collector installed (Check, Enumerate), so evaluation records
	// nothing and allocates nothing extra.
	sink := NoteCollectorFrom(ctx)
	if sink == nil {
		return compiled.Eval(ctx, in)
	}
	// Explain installed a collector. Evaluate into a local sink so each note can
	// be stamped with the rule reference that produced it before it is published
	// — the collector spans every grant in the trace, so the reference is what
	// tells two rules' notes apart.
	local := &NoteCollector{}
	// The floor-only observation is made HERE, past the fast-path return, and
	// that placement is the whole of its cost model: Check and Enumerate install
	// no collector, so they never reach this line and pay nothing for a
	// diagnostic nobody asked for. It is recorded BEFORE evaluation because it
	// describes the input the evaluation is about to be handed, and it goes
	// through the same local sink so it is stamped with this rule's reference
	// like every other note.
	recordFloorOnly(local, r.AST, principalAttrs, accountAttrs)
	selected, err := compiled.eval(in, local)
	notes := local.Notes()
	for i := range notes {
		notes[i].Rule = rule
	}
	sink.Add(notes...)
	return selected, err
}

// Compile validates, compiles, and caches an AST, returning the reusable program.
// A second call with an AST that renders to the same expression returns the
// cached program without recompiling. It is exported so a host can warm the cache
// or validate a rule (e.g. the node editor's save path) ahead of evaluation.
func (e *Engine) Compile(n *Node) (*Compiled, error) {
	return e.compile(n)
}

func (e *Engine) compile(n *Node) (*Compiled, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	// The cache is CONTENT-ADDRESSED: the key is the hash of what this AST
	// renders to right now, so an AST a host mutated in place renders
	// differently, misses, and recompiles. That is a correctness property, not
	// an accident — rules.Node is an exported struct of exported fields and
	// nothing forbids mutating one, and a rule edit that silently kept
	// authorizing under its old program is the worst bug this package could
	// have. So the render is NOT memoised against the node.
	//
	// It is made cheap instead. The buffer is pooled and reused, the digest is
	// taken over its bytes, and the canonical source is materialised as a string
	// only on a MISS — a hit happens on every evaluation, a miss once per
	// distinct rule.
	b := exprBufPool.Get().(*bytes.Buffer)
	defer func() {
		b.Reset()
		exprBufPool.Put(b)
	}()
	if err := n.render(b); err != nil {
		return nil, err
	}
	key := sha256.Sum256(b.Bytes())
	if c, ok := e.cache.get(key); ok {
		return c, nil
	}
	compiled, err := e.compiler.compileSource(b.String())
	if err != nil {
		return nil, err
	}
	e.cache.put(compiled)
	return compiled, nil
}

// exprBufPool reuses the render buffers Engine.compile fills. Every evaluation
// renders its rule to take the cache key, so this is one of the hottest
// allocations in the package; the buffers are small, uniform and very
// short-lived, which is exactly the shape a pool suits.
var exprBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// metadata fetches object's metadata, or returns an empty map when no fetcher is
// configured (rules that read only principal/action still evaluate).
func (e *Engine) metadata(ctx context.Context, object identity.Identity) (map[string]any, error) {
	if e.fetcher == nil {
		return map[string]any{}, nil
	}
	md, err := e.fetcher.Fetch(ctx, object)
	if err != nil {
		return nil, err
	}
	if md == nil {
		md = map[string]any{}
	}
	return md, nil
}

// accountAttributes resolves the active account's bag, and is the one place that
// decides an account is not something to ask a directory about.
//
// # The wildcard never reaches a resolver
//
// "*" is the all-accounts grant sentinel, not an account: there is no row for it
// (model.ValidateAccount refuses to store one) and nothing could answer "the
// attributes of every account" except one tenant's data served as every other's.
// It is nonetheless a live value here — platform-tier authority is anchored at
// the wildcard account, so authz.Gate.RequireSystemAdmin really does run a Check
// with it active — so the question is what a decision made at platform scope sees
// on `account`, and the answer is: the floor, and nothing else.
//
// Short-circuiting rather than erroring is deliberate. The bag is built for EVERY
// evaluation, whatever the rule reads, so refusing here would make every
// rule-backed grant undecidable at platform scope — including the overwhelming
// majority that never mention `account` — and turn one invariant about attribute
// keys into a restriction on which strategies may guard the system anchor. The
// floor is the honest answer instead: `account.id` is "*", which truthfully says
// "this decision is not scoped to a tenant", and every host-defined field is
// absent exactly as it is for an unwired slot.
//
// The refusal still exists, one layer down: presenting "*" to
// provider.AttributeRegistry (AccountAttributes, or Fetch on any slot) is
// APERTURE_ATTRIBUTE_PROVIDER_INVALID, so a caller that DOES reach a fetch with
// it gets a diagnostic rather than a bag. This short-circuit is what keeps that
// refusal a backstop instead of the mechanism.
// The short-circuit is also why the memo (attrs, nil when unscoped) is consulted
// only past it: a wildcard decision resolves nothing, so there is nothing to
// memoize and DecisionAttributes.Account reports that no bag was taken.
func (e *Engine) accountAttributes(ctx context.Context, attrs *DecisionAttributes, account string) (map[string]any, error) {
	if account == accountWildcard {
		return nil, nil
	}
	return attrs.accountAttributes(ctx, e.account, account)
}

// CacheStats exposes the compiled-rule cache counters for observability and the
// latency benchmark (E4-S4).
func (e *Engine) CacheStats() CacheStats { return e.cache.stats() }

// InvalidateAll clears the compiled-rule cache. A host calls it after a rule's
// definition changes underneath a cached compilation.
func (e *Engine) InvalidateAll() { e.cache.clear() }

// Invalidate drops the cached compilation of a single rule AST, reporting whether
// one was cached. A host calls it when exactly one rule's definition changed, so
// the next evaluation recompiles only that rule.
func (e *Engine) Invalidate(n *Node) (bool, error) {
	if err := n.Validate(); err != nil {
		return false, err
	}
	src, err := n.Expr()
	if err != nil {
		return false, err
	}
	return e.cache.invalidate(hashSource(src)), nil
}

// floorPrincipal is the default PrincipalResolver: it contributes NOTHING, so an
// unwired engine's `principal` is exactly the floor the engine stamps — id and
// kind (see principalBag).
//
// It returns nil rather than the floor map itself so there is exactly one
// definition of what the floor is. A default that built its own copy would be a
// second definition, free to drift from the one every host resolver gets.
type floorPrincipal struct{}

func (floorPrincipal) Attributes(context.Context, string, string) (map[string]any, error) {
	return nil, nil
}

// floorAccount is the default AccountResolver, and it is floorPrincipal's
// counterpart in every respect: it contributes NOTHING, so an unwired engine's
// `account` is exactly the floor the engine stamps — the account id (see
// accountBag) — and it returns nil rather than the floor map itself so there is
// only ever one definition of what that floor is.
type floorAccount struct{}

func (floorAccount) AccountAttributes(context.Context, string) (map[string]any, error) {
	return nil, nil
}
