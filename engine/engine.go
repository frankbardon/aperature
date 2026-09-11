// Package engine is Aperture's Policy Decision Point (PDP). It answers the
// single authorization question — "may this principal perform this action on
// this object, in this account?" — by resolving the grants that bind to the
// principal's effective subject set with deny-overrides plus a specificity
// tiebreak.
//
// Resolution rule (FR-6, FR-10, FR-11):
//
//   - A grant is a candidate when its permission's action matches the requested
//     action AND its object pattern covers the requested object.
//   - The decision is the effect of the most specific candidate. Specificity is
//     scored by the identity package (more fixed components, fewer/shallower
//     wildcards rank higher).
//   - A strictly more-specific allow carves an exception out of a broader deny
//     (e.g. deny account:acme/** + allow account:acme/project:atlas/** ⇒ atlas
//     is allowed, the rest of acme denied).
//   - At equal top specificity deny wins. This is both the deny-overrides rule
//     and the deterministic final tiebreak, so the result never depends on grant
//     insertion order.
//   - With no candidate the decision is DENY. Default-deny is the floor.
//
// Account isolation: the engine only ever asks storage for grants stamped to the
// request's active account (model.Storage.GrantsForSubjects is account-scoped),
// so a grant bestowed in another account can never influence a decision here.
// The one deliberate exception is a grant stamped to model.AccountWildcard
// ("*"), which GrantsForSubjects returns for every active account — a single
// grant that spans all tenancies (e.g. read documents everywhere). Only a
// system-admin can mint one, and it is the sole crack in the isolation floor.
//
// The "does this grant cover this object?" test is isolated behind the coverer
// seam. In E1 it is a literal identity-pattern match; in E2 a scope resolver
// will produce a grant's object set and swap in behind the same seam without
// touching the resolution logic.
//
// The hot path is allocation-conscious (NFR p99 < 1ms) but deliberately holds no
// cache yet — caching lands in E4-S4. Nothing here precludes it: Check is a pure
// function of storage state plus the request.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
)

// Request is the input to Check: the principal asking, the action and object in
// question, and the active account the decision is scoped to. It is a value type
// so the hot path passes it without indirection.
//
// Principal is a principal id (the key storage and the subject set are keyed on),
// not the principal's identity string. Object is an object-identity string in
// canonical form (e.g. "account:acme/project:atlas/document:42"). Account is the
// active account id; grants stamped to any other account are never consulted.
type Request struct {
	// Account is the active account the decision is scoped to. Mandatory.
	Account string
	// Principal is the id of the principal requesting access. Mandatory.
	Principal string
	// Action is the verb being attempted (e.g. "read"). Mandatory.
	Action string
	// Object is the canonical object-identity string. Mandatory.
	Object string
}

// Decision is the PDP's answer. Allow is the verdict; Reason is a human-readable
// explanation that names the deciding grant(s) (the seed Explain builds on in
// E2-S4); DecidingGrantIDs are the ids of the grant(s) that produced the verdict,
// in deterministic order (empty on a default-deny).
type Decision struct {
	// Allow is the verdict: true permits, false denies.
	Allow bool
	// Reason explains the verdict and identifies the deciding grant(s).
	Reason string
	// DecidingGrantIDs are the grant ids that produced the verdict, sorted for
	// determinism. Empty when the decision is a default-deny.
	DecidingGrantIDs []string
	// Impersonation, when non-nil, records that this decision was resolved under
	// an ACTIVE impersonation session: the real operator, the effective subject
	// whose authority was used, the mode, and the session expiry. It is the audit
	// linkage (E4-S2) — every decision made on behalf of another identity carries
	// the real actor here. Nil on the ordinary, non-impersonated path.
	Impersonation *ImpersonationContext
}

// effectivePrincipal is WHOSE authority a decision is resolved over, as the rule
// layer sees it: the principal id a rule reads as `principal.id`, and the kind
// that picks which attribute slot `principal.*` is fetched from. The pair travels
// together because it is meaningless apart — an attribute bag is only an answer
// once you know both whose it is and which directory to ask — and it travels as
// an argument rather than on Request because both halves are derived from storage
// during the decision instead of being supplied by the caller.
//
// On the ordinary path it is simply the requesting principal. Under an ACTIVE
// impersonation it is the effective subject: the target under become, the
// operator under augment (see elevatedSubjects). Request.Principal is untouched
// either way — the request records who asked, this records whose authority
// answers, and under become those are deliberately two different principals.
type effectivePrincipal struct {
	// ID is the principal id a rule reads as `principal.id`.
	ID string
	// Kind is model.PrincipalKind's spelling — "user" or "machine" — carried as a
	// string because that is the form scope.GrantContext takes (scope imports no
	// model). Empty means the engine did not have the principal's record in hand;
	// a consumer treats it as unknown and never as a default.
	Kind string
}

// coverer answers "does this grant cover this object, and at what specificity?".
// It is the seam scope strategies plug behind in E2: today a grant's object set
// is a single literal pattern, tomorrow it is whatever a scope resolver yields,
// and the resolution logic in Check is unaffected either way. The grant's
// permission is threaded through because a scope strategy is selected by the
// permission's scope-strategy reference and bounded by its object type; the
// literal default ignores it. The request supplies the account/action context a
// rule-backed strategy needs; the effective principal rides alongside it because
// it is derived from storage during the decision rather than supplied by the
// caller, so it has no home on Request — and because under impersonation it is
// deliberately not req.Principal.
type coverer interface {
	// cover reports whether g applies to object and, when it does, the
	// specificity at which it applies (higher = more specific). perm is the
	// grant's resolved permission (never nil when a candidate reached here). It
	// returns an error on a malformed grant pattern or scope reference (an
	// internal inconsistency, since writes validate them) or an unresolvable
	// strategy.
	cover(ctx context.Context, req Request, subject effectivePrincipal, g model.Grant, perm *model.Permission, object identity.Identity) (covered bool, specificity int, err error)

	// members enumerates the concrete object identities g covers that also match
	// query, bounded by the resolver's own limit. It is the Enumerate seam: the
	// literal default yields the grant's pattern only when it is a concrete
	// identity (a wildcard literal grant is not concretely enumerable without a
	// lister, so it contributes nothing); a scope strategy delegates to its
	// resolver's Members, which may consult the ObjectLister for "all of type"
	// strategies. perm selects the strategy exactly as cover does.
	members(ctx context.Context, req Request, subject effectivePrincipal, g model.Grant, perm *model.Permission, query identity.Pattern) ([]identity.Identity, error)
}

// literalCoverer is the E1 coverer: it treats Grant.Object as a literal identity
// pattern and matches it against the requested object with the identity matcher.
// It is the default, so an engine constructed without scope options keeps exact
// E1 behaviour.
type literalCoverer struct{ cache *patternCache }

func (c literalCoverer) cover(_ context.Context, _ Request, _ effectivePrincipal, g model.Grant, _ *model.Permission, object identity.Identity) (bool, int, error) {
	pat, err := c.cache.orParse(g.Object)
	if err != nil {
		// PutGrant validates the pattern, so reaching here means the stored grant
		// is corrupt. Surface it rather than silently dropping the grant.
		return false, 0, aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"grant %s has an unparseable object pattern %q", g.ID, g.Object)
	}
	if !pat.Matches(object) {
		return false, 0, nil
	}
	return true, identity.Specificity(pat), nil
}

func (literalCoverer) members(_ context.Context, _ Request, _ effectivePrincipal, g model.Grant, _ *model.Permission, query identity.Pattern) ([]identity.Identity, error) {
	return literalMembers(g, query)
}

// pattern resolves a grant's object pattern through the cache when one is wired,
// falling back to a direct parse for a zero-value coverer (defensive — New always
// wires a cache). It centralises the cached-parse used by both coverers.
func (c *patternCache) orParse(s string) (identity.Pattern, error) {
	if c == nil {
		return identity.ParsePattern(s)
	}
	return c.pattern(s)
}

// literalMembers yields a grant's concrete object for Enumerate. A literal grant
// is concretely enumerable only when its pattern is a fixed identity (no
// wildcards): identity.Parse succeeds exactly in that case. A wildcard literal
// grant cannot be enumerated without a provider lister, so it contributes
// nothing — Enumerate stays bounded rather than materialising an unbounded set.
// A pattern that is neither a valid identity nor a valid pattern is corrupt
// storage and surfaces an error, mirroring literalCoverer.cover.
func literalMembers(g model.Grant, query identity.Pattern) ([]identity.Identity, error) {
	id, err := identity.Parse(g.Object)
	if err != nil {
		pat, perr := identity.ParsePattern(g.Object)
		if perr != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_STORAGE, perr,
				"grant %s has an unparseable object pattern %q", g.ID, g.Object)
		}
		// A finitely-enumerable pattern (explicit "{a,b,c}" id sets, no wildcards)
		// expands to its concrete identities so a set-scoped grant lists its
		// objects; a wildcard pattern is not enumerable without a provider lister
		// and contributes nothing.
		expanded, ok := pat.Expand()
		if !ok {
			return nil, nil
		}
		out := make([]identity.Identity, 0, len(expanded))
		for _, e := range expanded {
			if query.Matches(e) {
				out = append(out, e)
			}
		}
		return out, nil
	}
	if !query.Matches(id) {
		return nil, nil
	}
	return []identity.Identity{id}, nil
}

// scopeCoverer consults a grant's pluggable scope resolver for object membership
// instead of only literal pattern matching. It preserves E1 behaviour for grants
// whose permission declares no strategy or the literal strategy: those still
// resolve by pure pattern match, so the literal default is never penalised. For
// any other strategy it parses the permission's scope reference, builds the
// resolver from the registry, and asks it whether the object is a member. The
// grant's pattern still supplies specificity, so the deny-overrides tiebreak is
// unchanged regardless of strategy.
type scopeCoverer struct {
	registry *scope.Registry
	deps     scope.Deps
	cache    *patternCache
}

func (c scopeCoverer) cover(ctx context.Context, req Request, subject effectivePrincipal, g model.Grant, perm *model.Permission, object identity.Identity) (bool, int, error) {
	ref := ""
	objectType := ""
	if perm != nil {
		ref = perm.ScopeStrategy
		objectType = perm.ObjectType
	}
	spec, err := scope.ParseSpec(ref)
	if err != nil {
		return false, 0, err
	}

	pat, err := c.cache.orParse(g.Object)
	if err != nil {
		return false, 0, aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"grant %s has an unparseable object pattern %q", g.ID, g.Object)
	}
	specificity := identity.Specificity(pat)

	// Literal (or unset) strategy is the E1 path: pure pattern match, no resolver.
	if spec.Strategy == scope.StrategyLiteral {
		if !pat.Matches(object) {
			return false, 0, nil
		}
		return true, specificity, nil
	}

	resolver, err := c.registry.Resolve(scope.GrantContext{
		Pattern:       pat,
		ObjectType:    objectType,
		Spec:          spec,
		Account:       req.Account,
		PrincipalKind: subject.Kind,
		Principal:     subject.ID,
		Action:        req.Action,
	}, c.deps)
	if err != nil {
		return false, 0, err
	}
	covered, err := resolver.Contains(ctx, object)
	if err != nil {
		return false, 0, err
	}
	if !covered {
		return false, 0, nil
	}
	return true, specificity, nil
}

func (c scopeCoverer) members(ctx context.Context, req Request, subject effectivePrincipal, g model.Grant, perm *model.Permission, query identity.Pattern) ([]identity.Identity, error) {
	ref := ""
	objectType := ""
	if perm != nil {
		ref = perm.ScopeStrategy
		objectType = perm.ObjectType
	}
	spec, err := scope.ParseSpec(ref)
	if err != nil {
		return nil, err
	}
	pat, err := c.cache.orParse(g.Object)
	if err != nil {
		return nil, aerr.Wrapf(aerr.APERTURE_STORAGE, err,
			"grant %s has an unparseable object pattern %q", g.ID, g.Object)
	}

	// Literal (or unset) strategy keeps the E1 enumeration: a concrete pattern is
	// its own sole member, a wildcard literal grant is not concretely enumerable.
	if spec.Strategy == scope.StrategyLiteral {
		return literalMembers(g, query)
	}

	resolver, err := c.registry.Resolve(scope.GrantContext{
		Pattern:       pat,
		ObjectType:    objectType,
		Spec:          spec,
		Account:       req.Account,
		PrincipalKind: subject.Kind,
		Principal:     subject.ID,
		Action:        req.Action,
	}, c.deps)
	if err != nil {
		return nil, err
	}
	return resolver.Members(ctx, query)
}

// Engine is the Policy Decision Point. It is stateless beyond its storage handle
// and coverer, and safe for concurrent use to whatever degree the underlying
// Storage is.
type Engine struct {
	store             model.Storage
	coverer           coverer
	enforceMembership bool
	// enumerateLimit is the configured ceiling on an enumeration — the single
	// number the whole decision graph inherits. Zero means DefaultEnumerateLimit;
	// New normalises it, and WithEnumerateLimit never stores a non-positive value,
	// so a zero here only ever means "built without New".
	enumerateLimit int
	// metadata is the object-metadata source Enumerate's Fields predicate reads
	// through (WithMetadata). Nil by default and consulted ONLY by a request that
	// carries Fields, so the unfiltered decision path never touches it.
	metadata MetadataFetcher
	// references is the declared-reference source Enumerate's reference edges are
	// dereferenced through (WithReferences). Nil by default and consulted ONLY by
	// a request that carries References.
	references ReferenceSource
	// logger receives the operational warnings a decision can raise without
	// failing — today only a dangling object reference. Nil means slog.Default();
	// it is never consulted by a decision that has nothing to report.
	logger *slog.Logger
	// now is the engine's clock, injected so impersonation time-box expiry is
	// deterministic in tests. It defaults to time.Now and is only consulted on the
	// impersonated decision path (CheckAs/EnumerateAs/ExplainAs); the
	// non-impersonated hot path never reads it.
	now func() time.Time
}

// Option configures an Engine at construction. Options compose; the last one to
// set a given facet wins.
type Option func(*Engine)

// New returns an Engine backed by store. With no options the coverer is the E1
// literal identity-pattern matcher, preserving exact E1 behaviour. Pass
// WithScopeResolution to consult a grant's pluggable scope resolver for object
// membership.
func New(store model.Storage, opts ...Option) *Engine {
	e := &Engine{store: store, coverer: literalCoverer{cache: newPatternCache()}, now: time.Now}
	for _, opt := range opts {
		opt(e)
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.enumerateLimit <= 0 {
		e.enumerateLimit = DefaultEnumerateLimit
	}
	// Stamped AFTER every option has run, so the bound the engine clamps with and
	// the bound the scope layer gathers with are the same number no matter which
	// order WithEnumerateLimit and WithScopeResolution were passed in.
	e.stampScopeBound()
	return e
}

// enumerateBound is the engine's effective enumeration ceiling. It tolerates a
// zero field so an Engine obtained other than through New (a shallow copy, a
// zero value in a test) still reports a positive bound rather than clamping
// every enumeration to nothing.
func (e *Engine) enumerateBound() int {
	if e.enumerateLimit <= 0 {
		return DefaultEnumerateLimit
	}
	return e.enumerateLimit
}

// stampScopeBound writes the engine's effective enumeration bound into the
// ScopeDeps its scope coverer holds.
//
// The stamp is UNCONDITIONAL and overwrites whatever MaxMembers a caller-built
// ScopeDeps literal carried — internal/cli builds exactly such a literal. One
// enumeration must not be governed by two different numbers: a scope layer
// gathering to a smaller bound than the engine clamps to would silently truncate
// the member set before the engine ever saw it, and nothing in the result would
// say so. Configure the bound on the engine (WithEnumerateLimit); the deps
// inherit it.
//
// It is a no-op on an engine using the literal coverer, which holds no deps.
func (e *Engine) stampScopeBound() {
	sc, ok := e.coverer.(scopeCoverer)
	if !ok {
		return
	}
	sc.deps.MaxMembers = e.enumerateBound()
	e.coverer = sc
}

// WithEnumerateLimit sets the ceiling an enumeration is bounded by: the value an
// EnumerateRequest with a non-positive Limit receives, and the value a larger
// request Limit is clamped down to. It is also stamped into the ScopeDeps the
// engine wires (WithScopeResolution), so the member gather and the result cap
// agree.
//
// A non-positive n is NORMALISED to DefaultEnumerateLimit rather than stored.
// An Option cannot report an error, and a silently zero bound would turn every
// enumeration into an empty result that reads exactly like "no access" — so a
// misconfiguration degrades to the documented default instead of fabricating a
// denial. Validate and report a bad configured value at the surface that parses
// it, where an APERTURE_CONFIG_INVALID can still reach the operator.
func WithEnumerateLimit(n int) Option {
	return func(e *Engine) {
		if n <= 0 {
			n = DefaultEnumerateLimit
		}
		e.enumerateLimit = n
		e.stampScopeBound()
	}
}

// WithClock overrides the engine's clock. It governs impersonation time-box
// expiry on the CheckAs/EnumerateAs/ExplainAs path, so tests can advance time
// deterministically instead of sleeping. Production uses the default time.Now.
// The non-impersonated decision path does not consult the clock.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithScopeResolution makes the engine consult each grant's scope resolver — as
// selected by its permission's scope-strategy reference and built from registry
// — for object membership, instead of only literal pattern matching. Grants with
// no strategy (or the literal strategy) still resolve by pattern match, so E1
// behaviour is preserved. The optional ScopeDeps supply the object lister
// (E2-S2) and rule evaluator (E2-S3) seams; omit them and the resolvers fall
// back to inert defaults that report those dependencies are unconfigured.
//
// A nil registry is treated as scope.DefaultRegistry() so the three built-in
// strategies are available out of the box.
//
// The engine's configured enumeration bound (WithEnumerateLimit) is stamped into
// the deps it keeps, so a caller who hands in a bare ScopeDeps literal — as
// internal/cli does — still gathers members against the same number the engine
// clamps the result to. See stampScopeBound.
func WithScopeResolution(registry *scope.Registry, deps ...ScopeDeps) Option {
	return func(e *Engine) {
		reg := registry
		if reg == nil {
			reg = scope.DefaultRegistry()
		}
		var d scope.Deps
		if len(deps) > 0 {
			d = scope.Deps(deps[0])
		}
		e.coverer = scopeCoverer{registry: reg, deps: d, cache: newPatternCache()}
		e.stampScopeBound()
	}
}

// WithMembershipEnforcement makes the engine require that a decision request's
// principal is actually a member of the active account before any grant is
// consulted. A non-member is denied at the door — a clean, fail-closed
// default-deny (Check), an empty result (Enumerate), or a deny Trace (Explain) —
// rather than an error, so the PDP never fails open and the rendering surfaces
// treat it like any other deny.
//
// It is OPT-IN. The (principal, active-account) isolation invariant is ALREADY
// guaranteed without it — grant queries are account-scoped, so a non-member with
// no grants in the account already default-denies. Enforcement is the
// defence-in-depth layer that makes membership a hard precondition: it closes
// the theoretical gap where a grant is mistakenly stamped to an account a
// principal was never admitted to, and gives surfaces an explicit, uniform
// "not in this tenancy" verdict. Off by default so a deployment that models
// membership purely through grants is unaffected.
func WithMembershipEnforcement() Option {
	return func(e *Engine) { e.enforceMembership = true }
}

// WithStore returns a shallow copy of e that reads from store instead of e's own
// storage handle, preserving the coverer, membership policy, and clock. It is the
// READ-ONLY what-if seam (E4-S3 MCP Simulate / E6-S4 simulator): a caller layers
// a hypothetical overlay over the live storage and evaluates Check / Explain /
// Enumerate against the returned copy without ever writing — the engine performs
// no writes, and a read-only overlay's mutators are inert. e itself is unchanged,
// so the live engine and the transient what-if engine never interfere.
func (e *Engine) WithStore(store model.Storage) *Engine {
	clone := *e
	clone.store = store
	return &clone
}

// WithRuleEvaluator returns a shallow copy of e whose rule-backed scope
// strategies (inclusive/exclusive with a rule reference) consult re instead of
// e's own rule evaluator. It is the READ-ONLY what-if seam for previewing an
// UNSAVED rule (E7-S3): the simulate path layers a hypothetical rule over the
// live rule source and evaluates Check / Explain against the returned copy
// without ever persisting the edit. e itself is unchanged, so the live engine and
// the transient preview engine never interfere.
//
// It only affects an engine that resolves scopes (WithScopeResolution); on an
// engine using the literal coverer there are no rule-backed strategies to
// redirect, so the copy is returned unchanged.
func (e *Engine) WithRuleEvaluator(re scope.RuleEvaluator) *Engine {
	clone := *e
	if sc, ok := clone.coverer.(scopeCoverer); ok {
		sc.deps.Rules = re
		clone.coverer = sc
	}
	return &clone
}

// requireMembership reports whether the request may proceed under the active
// membership policy. With enforcement off it always returns true. With
// enforcement on it returns whether principal is a member of account; a storage
// failure surfaces as an APERTURE_STORAGE error (a non-decision), never a silent
// allow.
func (e *Engine) requireMembership(ctx context.Context, account, principal string) (bool, error) {
	if !e.enforceMembership {
		return true, nil
	}
	ok, err := e.store.IsMember(ctx, principal, account)
	if err != nil {
		return false, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to check active-account membership", err)
	}
	if ok {
		return true, nil
	}
	// Wildcard membership mirrors the wildcard-account grant: a principal enrolled
	// in the "*" account is treated as a member of EVERY account, so a cross-account
	// super-admin (whose authority is carried by "*"-stamped grants) is not fenced
	// out by enforcement it is meant to transcend.
	wild, err := e.store.IsMember(ctx, principal, model.AccountWildcard)
	if err != nil {
		return false, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to check wildcard-account membership", err)
	}
	return wild, nil
}

// nonMemberDeny is the fail-closed verdict for a principal that is not a member
// of the active account. It names no deciding grant — the denial precedes grant
// evaluation entirely.
func nonMemberDeny(req Request) Decision {
	return Decision{
		Allow: false,
		Reason: fmt.Sprintf(
			"default deny: principal %q is not a member of active account %q",
			req.Principal, req.Account),
	}
}

// ScopeDeps are the runtime dependencies the scope resolvers may consult: the
// object lister (E2-S2) and the rule evaluator (E2-S3). It mirrors scope.Deps so
// callers do not import the scope package's seam types directly when they only
// need to wire the engine.
type ScopeDeps = scope.Deps

// candidate is a grant that matched the request, tagged with the specificity at
// which it applies.
type candidate struct {
	grant       model.Grant
	specificity int
}

// Check resolves a single authorization decision. It never returns an
// allow-on-error: any operational failure (bad request, storage error, missing
// principal) is returned as an APERTURE_* coded error and the caller treats it as
// a non-decision, while a well-formed request with no matching grant yields a
// clean default-deny.
func (e *Engine) Check(ctx context.Context, req Request) (Decision, error) {
	if err := validateRequest(req); err != nil {
		return Decision{}, err
	}

	object, err := identity.Parse(req.Object)
	if err != nil {
		return Decision{}, err
	}

	member, err := e.requireMembership(ctx, req.Account, req.Principal)
	if err != nil {
		return Decision{}, err
	}
	if !member {
		return nonMemberDeny(req), nil
	}

	subjects, subject, err := e.subjectSet(ctx, req.Principal)
	if err != nil {
		return Decision{}, err
	}

	grants, err := e.store.GrantsForSubjects(ctx, req.Account, subjects)
	if err != nil {
		return Decision{}, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load grants for subjects", err)
	}

	// A single permission cache is threaded through the evaluation so a subject
	// with many grants on the same permission pays one lookup.
	permCache := make(map[string]*model.Permission, len(grants))
	return e.evaluate(ctx, req, subject, object, grants, permCache)
}

// evaluate runs the deny-overrides/specificity decision for one concrete object
// against an already-loaded grant set, reusing permCache across grants. It is
// the shared core of Check and of Enumerate's per-candidate verdict, so the
// "never returns a denied object" guarantee in Enumerate is the exact same
// decision the hot path makes.
func (e *Engine) evaluate(ctx context.Context, req Request, subject effectivePrincipal, object identity.Identity, grants []model.Grant, permCache map[string]*model.Permission) (Decision, error) {
	// One reference instant for the whole decision, whatever a rule-backed scope
	// resolver asks for later. The call is idempotent, so an Enumerate that
	// already opened a scope for the enumeration keeps ITS instant across every
	// per-candidate decision rather than taking a fresh one each time.
	ctx, _ = rules.WithDecisionInstant(ctx)
	// One principal bag and one account bag for the whole decision, for the same
	// reason and with the same idempotence: the principal asking and the tenancy
	// it is asking in do not change between two grants, and a bag re-resolved
	// mid-decision could come back different (an attribute cache expires on a
	// TTL) and decide two grants against two views of the same principal.
	ctx, _ = rules.WithDecisionAttributes(ctx)

	candidates := make([]candidate, 0, len(grants))
	for _, g := range grants {
		ok, err := e.actionMatches(ctx, g, req.Action, permCache)
		if err != nil {
			return Decision{}, err
		}
		if !ok {
			continue
		}
		// actionMatches has populated permCache with a non-nil permission for a
		// matched grant; the scope coverer needs it to select the strategy.
		perm := permCache[g.PermissionID]
		covered, spec, err := e.coverer.cover(ctx, req, subject, g, perm, object)
		if err != nil {
			return Decision{}, err
		}
		if !covered {
			continue
		}
		candidates = append(candidates, candidate{grant: g, specificity: spec})
	}

	return decide(req, candidates), nil
}

// EffectiveGrants returns every grant that binds to principal within account:
// the account-scoped grants for the principal's full subject set
// ({principal} ∪ roles ∪ groups), exactly the set Check resolves over. It is the
// delegation subsystem's (E3-S2) view of "what may this principal do here" — the
// basis for the bestow subset rule — and is account-scoped, so a principal's
// authority in one account never bleeds into a bestow stamped to another.
//
// It returns the grants verbatim (no action/object filtering); the caller pairs
// each with its permission to apply the subset rule. An unknown principal
// surfaces as APERTURE_NOT_FOUND; a storage fault as APERTURE_STORAGE.
func (e *Engine) EffectiveGrants(ctx context.Context, account, principal string) ([]model.Grant, error) {
	switch {
	case account == "":
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: account is empty")
	case principal == "":
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: principal is empty")
	}
	// The kind is irrelevant here: EffectiveGrants returns grants verbatim and
	// evaluates no scope strategy, so nothing downstream asks who is asking.
	subjects, _, err := e.subjectSet(ctx, principal)
	if err != nil {
		return nil, err
	}
	grants, err := e.store.GrantsForSubjects(ctx, account, subjects)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load effective grants for principal", err)
	}
	return grants, nil
}

// subjectSet expands a principal into the set of subjects whose grants apply to
// it: the principal itself, every role it is assigned, and every group it belongs
// to. The expansion is the union {principal} ∪ roles ∪ groups.
//
// It also returns the principal as an effectivePrincipal — its id paired with its
// kind. The kind is free here — GetPrincipal is already on the decision's critical
// path to read RoleIDs — and returning it is what keeps a rule-backed scope
// strategy from having to buy a second read of the same row later. Pairing it with
// the id is what lets an impersonated decision hand the rule layer the TARGET's
// (id, kind) without a second read either: the row this call already fetched is
// the target's (see elevatedSubjects).
func (e *Engine) subjectSet(ctx context.Context, principalID string) ([]model.Subject, effectivePrincipal, error) {
	p, err := e.store.GetPrincipal(ctx, principalID)
	if err != nil {
		// A missing principal surfaces as APERTURE_NOT_FOUND verbatim; the caller
		// decides whether to render it as a hard error or a fail-closed deny.
		return nil, effectivePrincipal{}, err
	}

	subjects := make([]model.Subject, 0, 1+len(p.RoleIDs)+2)
	subjects = append(subjects, model.Subject{Kind: model.SubjectPrincipal, ID: principalID})
	for _, roleID := range p.RoleIDs {
		subjects = append(subjects, model.Subject{Kind: model.SubjectRole, ID: roleID})
	}

	groups, err := e.store.GroupsForPrincipal(ctx, principalID)
	if err != nil {
		return nil, effectivePrincipal{}, aerr.Wrap(aerr.APERTURE_STORAGE,
			"engine: failed to load groups for principal", err)
	}
	for _, g := range groups {
		subjects = append(subjects, model.Subject{Kind: model.SubjectGroup, ID: g.ID})
	}
	return subjects, effectivePrincipal{ID: principalID, Kind: string(p.Kind)}, nil
}

// actionMatches reports whether the grant's permission names the requested
// action. A grant whose permission no longer exists (a dangling reference left by
// a deleted permission) is inert and reported as a non-match rather than an
// error, so a stale grant cannot crash the hot path. permCache memoises lookups
// across a single Check.
func (e *Engine) actionMatches(ctx context.Context, g model.Grant, action string, permCache map[string]*model.Permission) (bool, error) {
	perm, ok := permCache[g.PermissionID]
	if !ok {
		p, err := e.store.GetPermission(ctx, g.PermissionID)
		if err != nil {
			if aerr.CodeOf(err) == aerr.APERTURE_NOT_FOUND {
				permCache[g.PermissionID] = nil
				return false, nil
			}
			return false, aerr.Wrap(aerr.APERTURE_STORAGE,
				"engine: failed to load permission for grant", err)
		}
		perm = &p
		permCache[g.PermissionID] = perm
	}
	if perm == nil {
		return false, nil
	}
	return perm.Action == action, nil
}

// decide applies deny-overrides with specificity tiebreak to the matched
// candidates. The verdict is the effect of the most specific candidate; at equal
// top specificity deny wins. The result is independent of candidate order.
func decide(req Request, candidates []candidate) Decision {
	if len(candidates) == 0 {
		return Decision{
			Allow: false,
			Reason: fmt.Sprintf(
				"default deny: no grant matched action %q on %q for principal %q in account %q",
				req.Action, req.Object, req.Principal, req.Account),
		}
	}

	// Find the top specificity, and whether any candidate at that level denies.
	maxSpec := candidates[0].specificity
	for _, c := range candidates[1:] {
		if c.specificity > maxSpec {
			maxSpec = c.specificity
		}
	}
	denyWins := false
	for _, c := range candidates {
		if c.specificity == maxSpec && c.grant.Effect == model.EffectDeny {
			denyWins = true
			break
		}
	}

	winning := model.EffectAllow
	if denyWins {
		winning = model.EffectDeny
	}

	// Collect the deciding grants: those at the top specificity carrying the
	// winning effect. Sort by id for a deterministic reason and id list.
	deciding := make([]model.Grant, 0, len(candidates))
	for _, c := range candidates {
		if c.specificity == maxSpec && c.grant.Effect == winning {
			deciding = append(deciding, c.grant)
		}
	}
	sort.Slice(deciding, func(i, j int) bool { return deciding[i].ID < deciding[j].ID })

	ids := make([]string, len(deciding))
	for i, g := range deciding {
		ids[i] = g.ID
	}

	return Decision{
		Allow:            winning == model.EffectAllow,
		Reason:           reasonFor(winning, deciding, len(candidates), maxSpec),
		DecidingGrantIDs: ids,
	}
}

// reasonFor renders a decision explanation that names the deciding grant(s) and
// records the specificity and the number of candidates considered — the seed
// E2-S4's Explain builds on.
func reasonFor(effect model.Effect, deciding []model.Grant, totalMatched, spec int) string {
	verb := "allowed"
	if effect == model.EffectDeny {
		verb = "denied"
	}
	parts := make([]string, len(deciding))
	for i, g := range deciding {
		parts[i] = fmt.Sprintf("%s (%s %s)", g.ID, g.Effect, g.Object)
	}
	return fmt.Sprintf(
		"%s by grant %s at specificity %d; %d matching grant(s) considered",
		verb, strings.Join(parts, ", "), spec, totalMatched)
}

// validateRequest rejects a Request missing any required field before any
// storage work happens.
func validateRequest(req Request) error {
	switch {
	case req.Account == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request account is empty")
	case req.Principal == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request principal is empty")
	case req.Action == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request action is empty")
	case req.Object == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: request object is empty")
	}
	return nil
}
