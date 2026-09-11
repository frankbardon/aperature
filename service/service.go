// Package service is the thin decision facade that every surface — the CLI
// check command, the HTTP /check endpoint, and (in E4-S1) the Twirp service —
// calls instead of touching the engine directly. It exists so the surfaces
// share ONE code path with ONE fail-closed policy: the rule for turning an
// engine error into a rendered decision lives here, not duplicated per surface.
//
// Fail-closed rendering (per the engine's error contract):
//
//   - A genuine input-validation error (APERTURE_INVALID_INPUT /
//     APERTURE_IDENTITY_INVALID) is returned to the caller verbatim, so the CLI
//     renders a usage error and HTTP returns 400. The caller asked an
//     ill-formed question; that is not a deny.
//   - Every other engine error (an unknown principal surfaces as
//     APERTURE_NOT_FOUND, a storage fault as APERTURE_STORAGE, ...) is rendered
//     fail-closed as a DENY, with the underlying error folded into the reason.
//     A decision point must never fail open.
//   - A clean engine result passes through unchanged.
//
// E4-S1 note: the Twirp handler should call exactly this facade —
// service.New(eng).Check(ctx, service.Query{...}) returning service.Result — so
// the gRPC/Twirp surface inherits the same fail-closed semantics for free.
//
// Beyond Check the facade exposes the rest of the decision API (FR-10):
// Enumerate (which objects a principal may act on), Explain (the full decision
// trace), and a bulk-batched form of all three. The single ops keep the
// fail-closed contract; the batch ops return per-item results ALIGNED with their
// queries (result[i] for query[i]) so one bad query never fails the batch — the
// shape E4-S1 (Twirp), E4-S3 (MCP), and E6-S4 (what-if) all build on.
//
// Rendering per op:
//
//   - Check / CheckBatch keep the fail-closed contract above (operational error
//     folds to a deny Result; an input-validation error is returned).
//   - Enumerate / Explain return engine errors verbatim. Enumerate cannot fail
//     open by construction — every id it returns is one Check allows, denied
//     objects are excluded inside the engine — so an operational failure is a
//     returned error, not a silent partial set. Explain is a diagnostic.
//   - The batch ops carry each item's error in its BatchResult, so a partial
//     failure is per-item, never whole-batch.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/frankbardon/aperture/audit"
	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/delegation"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/impersonation"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// Query is a single authorization question in surface-neutral form. It mirrors
// engine.Request but is the type the CLI and HTTP layers marshal to/from, so the
// engine's Request type stays an engine-internal concern.
type Query struct {
	// Account is the active account the decision is scoped to.
	Account string
	// Principal is the id of the principal asking.
	Principal string
	// Action is the verb being attempted.
	Action string
	// Object is the canonical object-identity string.
	Object string
}

// Result is a rendered decision: the verdict, a human-readable reason, and the
// ids of the deciding grant(s). It is the value each surface serializes.
type Result struct {
	// Allow is the verdict: true permits, false denies.
	Allow bool
	// Reason explains the verdict (names the deciding grants, or the fail-closed
	// cause on an operational error).
	Reason string
	// DecidingGrantIDs are the grant ids that produced an allow/deny verdict;
	// empty on a default-deny or a fail-closed deny.
	DecidingGrantIDs []string
}

// Service is the single facade every surface — CLI, HTTP, Twirp, and the MCP
// read subset — calls instead of touching the engine, storage, or the
// delegation / impersonation / authz packages directly. It carries the read
// path (Check / Enumerate / Explain + batch) always, and the mutation path
// (entity CRUD, grants, delegation, impersonation) when constructed with the
// backing dependencies (WithStorage / WithGate / WithDelegation /
// WithImpersonation). A read-only Service (service.New(eng)) returns
// APERTURE_UNIMPLEMENTED from any mutation, so the decision surfaces that do not
// need write access stay minimal.
type Service struct {
	eng     *engine.Engine
	store   model.Storage
	gate    *authz.Gate
	deleg   *delegation.Service
	imperso *impersonation.Service
	// audit, when non-nil, records the append-only audit trail (E4-S2): mutations
	// synchronously and always, decision checks sampled + async. Nil = no audit
	// (the existing no-audit construction). Wired with WithAudit.
	audit *audit.Recorder
	// now is the facade clock for stamping entity timestamps on writes. It is
	// time.Now by default; WithClock overrides it for deterministic tests.
	now func() time.Time
	// ruleSource / ruleFetcher back the READ-ONLY what-if preview of an UNSAVED
	// rule (E7-S3): Simulate overlays an Overlay.Rules edit over ruleSource and
	// evaluates through a transient rules engine built with ruleFetcher. Nil (the
	// default) leaves Overlay.Rules inert — the sim engine uses the live engine's
	// own rule evaluator. Wired with WithRuleSource.
	ruleSource  rules.RuleSource
	ruleFetcher rules.MetadataFetcher
	// providers, when non-nil, is the object-metadata provider registry the
	// ObjectIdentifiers read enumerates a type's instances through. Nil (the
	// default) makes ObjectIdentifiers report APERTURE_UNIMPLEMENTED. Wired with
	// WithProviders.
	providers *provider.Registry
	// attrs, when non-nil, is the ATTRIBUTE registry (principal + account
	// directories) the gated ListAttributes read enumerates a slot through. It is
	// a separate field from providers because the two are separate registries
	// answering separate questions — what the host knows about an OBJECT versus
	// what it knows about the party ASKING — and only this one is behind a
	// system-admin gate. Nil (the default) makes ListAttributes report
	// APERTURE_UNIMPLEMENTED. Wired with WithAttributes.
	attrs *provider.AttributeRegistry
	// managed is the deployment's boot-time entity-management posture: which of
	// accounts / principals / memberships this Aperture owns the lifecycle of.
	// Read once at construction and never mutated. Its zero value means every
	// kind is managed, so a facade built without WithManagedEntities behaves
	// exactly as before the option existed. Wired with WithManagedEntities.
	managed ManagedEntities
}

// Option configures a Service at construction. Options compose; the mutation
// methods report APERTURE_UNIMPLEMENTED until the dependency they need is wired.
type Option func(*Service)

// WithStorage gives the facade the persistence handle the entity-CRUD mutations
// (and their reads) operate over.
func WithStorage(store model.Storage) Option {
	return func(s *Service) { s.store = store }
}

// WithGate gives the facade the admin-authority gate (E3-S4) it calls before
// every system/account-tier mutation.
func WithGate(gate *authz.Gate) Option {
	return func(s *Service) { s.gate = gate }
}

// WithDelegation gives the facade the delegation service (E3-S2) backing Bestow
// and Revoke.
func WithDelegation(d *delegation.Service) Option {
	return func(s *Service) { s.deleg = d }
}

// WithImpersonation gives the facade the impersonation service (E3-S3) backing
// ImpersonationStart.
func WithImpersonation(i *impersonation.Service) Option {
	return func(s *Service) { s.imperso = i }
}

// WithRuleSource gives the facade the base rule source and object-metadata
// fetcher its Simulate path overlays an UNSAVED rule onto, so a what-if can
// preview the rule being edited (E7-S3) without persisting it. The base source is
// the same storage-backed source the live decision engine resolves against, so a
// preview shadows exactly the stored rule the edit will replace. A nil fetcher
// means object metadata is empty (fine for rules over the principal/action
// context). Without this option Simulate's Overlay.Rules field is inert.
func WithRuleSource(base rules.RuleSource, fetcher rules.MetadataFetcher) Option {
	return func(s *Service) {
		s.ruleSource = base
		s.ruleFetcher = fetcher
	}
}

// WithProviders gives the facade the object-metadata provider registry its
// ObjectIdentifiers read enumerates a type's instances through. Without it,
// ObjectIdentifiers reports APERTURE_UNIMPLEMENTED.
func WithProviders(reg *provider.Registry) Option {
	return func(s *Service) { s.providers = reg }
}

// WithAttributes gives the facade the attribute registry its gated
// ListAttributes read enumerates a slot through — the same registry the decision
// path resolves `principal` and `account` bags from, so an operator lists
// exactly the directory a rule reads. Without it, ListAttributes reports
// APERTURE_UNIMPLEMENTED.
//
// Wiring it grants nobody anything: enumeration is refused to any actor without
// system-admin authority, and the decision path's per-bag Fetch never goes
// through this option at all (it goes through rules.WithPrincipalResolver /
// rules.WithAccountResolver, wired straight onto the rules engine). See
// service/attributes.go for why that separation is the whole design.
func WithAttributes(reg *provider.AttributeRegistry) Option {
	return func(s *Service) { s.attrs = reg }
}

// WithManagedEntities declares which entity kinds this deployment manages —
// which of accounts, principals, and memberships Aperture owns the lifecycle of.
// It is boot-time posture, not a permission: it is fixed for the process's
// lifetime, and it is orthogonal to the authority gate WithGate installs.
//
// Omitting the option leaves every kind managed, which is Aperture's historical
// behaviour, so existing callers are unaffected.
func WithManagedEntities(m ManagedEntities) Option {
	return func(s *Service) { s.managed = m }
}

// ManagedEntities reports the deployment posture the facade was constructed
// with. It is a read of immutable boot-time configuration, which is what lets a
// surface tell a caller which entity kinds are writable without attempting a
// write.
func (s *Service) ManagedEntities() ManagedEntities { return s.managed }

// WithClock overrides the facade clock used to stamp entity timestamps on
// writes. It exists for deterministic tests; production uses time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New returns a Service backed by eng. With no options it is read-only (the
// decision API); pass WithStorage / WithGate / WithDelegation / WithImpersonation
// to enable the mutation surface. The serve command builds the fully-wired
// facade so HTTP, Twirp, and CLI all drive one mutation path.
func New(eng *engine.Engine, opts ...Option) *Service {
	s := &Service{eng: eng, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Check answers q. It returns an error ONLY for genuine input-validation
// failures (which surfaces render as 400 / usage errors); every other engine
// failure is folded into a fail-closed DENY Result with a nil error, so a
// decision point never fails open.
func (s *Service) Check(ctx context.Context, q Query) (Result, error) {
	dec, err := s.eng.Check(ctx, q.request())
	res, rerr := renderCheck(dec, err)
	// Decision audit is sampled + asynchronous (off the Check hot path). An
	// input-validation error is a caller bug, not a decision, so skip it; an
	// operational failure has already folded into a fail-closed deny Result.
	if rerr == nil {
		s.recordDecision(ctx, "Check", q.Account, q.Principal, q.Object, res.Allow, res.Reason, nil)
	}
	return res, rerr
}

// renderCheck applies the fail-closed contract to one engine Check outcome: an
// input-validation error is returned; any other engine error folds into a deny
// Result; a clean decision passes through. It is shared by Check and CheckBatch
// so every Check surface renders identically.
func renderCheck(dec engine.Decision, err error) (Result, error) {
	if err != nil {
		switch aerr.CodeOf(err) {
		case aerr.APERTURE_INVALID_INPUT, aerr.APERTURE_IDENTITY_INVALID:
			// The caller asked an ill-formed question — surface it as an error.
			return Result{}, err
		default:
			// Operational failure (unknown principal, storage fault, ...): deny.
			return Result{
				Allow:  false,
				Reason: fmt.Sprintf("fail-closed deny: %v", err),
			}, nil
		}
	}
	return Result{
		Allow:            dec.Allow,
		Reason:           dec.Reason,
		DecidingGrantIDs: dec.DecidingGrantIDs,
	}, nil
}

// request adapts a Query to the engine's Request type.
func (q Query) request() engine.Request {
	return engine.Request{
		Account:   q.Account,
		Principal: q.Principal,
		Action:    q.Action,
		Object:    q.Object,
	}
}

// EnumerateQuery is an enumeration question in surface-neutral form: which
// objects under Pattern may Principal take Action on, in Account. Limit caps the
// result (<= 0 means the engine's default bound). It mirrors
// engine.EnumerateRequest so the engine type stays an engine-internal concern.
type EnumerateQuery struct {
	// Account is the active account the enumeration is scoped to.
	Account string
	// Principal is the id of the principal whose access is enumerated.
	Principal string
	// Action is the verb being enumerated.
	Action string
	// Pattern is the identity pattern bounding the search.
	Pattern string
	// Fields are OPTIONAL object-metadata predicates narrowing the result: an
	// allowed object is returned only when its metadata satisfies every one of
	// them. Nil or empty (the default) filters nothing.
	//
	// The facade passes the map to the engine UNCHANGED — it does not parse,
	// coerce, or normalise a value. That is deliberate: the predicate's meaning
	// is provider.Filter's Fields contract (AND across keys, absent never
	// matches, membership for a collection field, typed equality otherwise) and
	// it is evaluated in exactly one place, provider.MatchFields. A surface that
	// re-interpreted a value on the way in — parsing "5" into a number, say —
	// would make an enumeration select objects a Check then denies.
	//
	// The struct tags are the MCP surface's, which reflects its tool schema
	// straight off this type (mcp.EnumerateIn is an alias for it). `omitempty`
	// is load-bearing there: without it the reflected schema marks the predicate
	// REQUIRED and an agent could not ask an unfiltered question at all. The
	// name is kept in the struct's existing PascalCase so one object does not
	// mix two spellings.
	Fields map[string]any `json:"Fields,omitempty" jsonschema:"Optional object-metadata predicates narrowing the result. ANDed; a field the object does not carry never matches; a list-valued field matches by membership; everything else by typed equality, so \"5\" never matches 5. Omit to filter nothing."`
	// References are OPTIONAL reference edges restricting the result to the
	// identities a holder object's DECLARED reference field contains — "the
	// brands in dataset X". Nil or empty (the default) restricts nothing.
	//
	// It is a DEREFERENCE, not a predicate, which is why it is not spelled
	// through Fields: a brand carries no field naming its datasets, so no
	// predicate on brand can express the question. Several edges AND, an edge
	// composes with Fields, and both precede Limit.
	//
	// The facade carries the edges to the engine UNCHANGED — it does not parse a
	// holder identity, infer a holder type, or check that a field is declared.
	// Every one of those is a decision with a disclosure consequence (see
	// engine/reference.go), and a surface that made it separately would be a
	// second place for the answer to differ.
	//
	// The struct tags are the MCP surface's, which reflects its tool schema
	// straight off this type. `omitempty` is load-bearing there: without it the
	// reflected schema marks the edges REQUIRED and an agent could not ask an
	// unrestricted question at all.
	References []ReferenceEdge `json:"References,omitempty" jsonschema:"Optional reference edges restricting the result to the identities a holder object's declared reference field contains, e.g. the brands in dataset X. Several edges are ANDed and they compose with Fields. A holder the principal may not see yields an empty result, not an error. Omit to restrict nothing."`
	// Limit caps the number of returned object ids. <= 0 means the engine's
	// configured bound; a Limit above it is clamped down to it. That bound is
	// engine.DefaultEnumerateLimit only when nothing configured one.
	Limit int
}

// ReferenceEdge is one reference edge in surface-neutral form: which holder
// object, and which of its declared reference fields to dereference. It mirrors
// engine.ReferenceEdge so the engine type stays an engine-internal concern, and
// it is three plain strings — an edge NAMES a field, it never carries a value,
// so none of the metadata value model applies to it.
type ReferenceEdge struct {
	// HolderType is the object-type that DECLARES the reference ("dataset").
	// OPTIONAL: empty means "whatever HolderID's terminal segment type is". When
	// it IS given it must agree with HolderID; the engine, not this type, is
	// where that is enforced.
	HolderType string `json:"HolderType,omitempty" jsonschema:"Object-type that declares the reference, e.g. 'dataset'. Optional: omit it and the type is taken from HolderID's last segment. When given it must agree with HolderID."`
	// HolderID is the holder object's canonical identity
	// ("account:acme/dataset:7"). Mandatory.
	HolderID string `json:"HolderID" jsonschema:"Canonical identity of the holder object, e.g. 'account:acme/dataset:7'."`
	// Field is the metadata field on HolderType that a references: declaration
	// binds to a target object-type ("current_brands"). Mandatory, and it must
	// be DECLARED — an undeclared field is a coded error, never an empty edge.
	Field string `json:"Field" jsonschema:"Declared reference field on the holder to dereference, e.g. 'current_brands'."`
}

// String renders an edge as "account:acme/dataset:7.current_brands" — the same
// spelling the CLI's --via flag accepts.
func (r ReferenceEdge) String() string { return r.HolderID + "." + r.Field }

// references converts the surface-neutral edges to the engine's, field for
// field. A nil or empty slice stays nil, so an omitted edge list is
// indistinguishable from none at every layer.
func (q EnumerateQuery) references() []engine.ReferenceEdge {
	if len(q.References) == 0 {
		return nil
	}
	out := make([]engine.ReferenceEdge, len(q.References))
	for i, e := range q.References {
		out[i] = engine.ReferenceEdge{
			HolderType: e.HolderType,
			HolderID:   e.HolderID,
			Field:      e.Field,
		}
	}
	return out
}

func (q EnumerateQuery) request() engine.EnumerateRequest {
	return engine.EnumerateRequest{
		Account:    q.Account,
		Principal:  q.Principal,
		Action:     q.Action,
		Pattern:    q.Pattern,
		Fields:     q.Fields,
		References: q.references(),
		Limit:      q.Limit,
	}
}

// Enumerate returns the object ids under q.Pattern that q.Principal may take
// q.Action on. Every id is one Check would allow — denied objects are excluded
// inside the engine — so the result never fails open. Engine errors (including
// input validation) are returned verbatim for the surface to map to a status.
func (s *Service) Enumerate(ctx context.Context, q EnumerateQuery) ([]string, error) {
	ids, err := s.eng.Enumerate(ctx, q.request())
	if err == nil {
		s.recordDecision(ctx, "Enumerate", q.Account, q.Principal, q.Pattern, true,
			"enumerated accessible objects", map[string]any{"count": len(ids)})
	}
	return ids, err
}

// Explain returns the full decision trace for q. The engine.Trace it returns is
// the public contract surfaces serialize. Engine errors are returned verbatim;
// Explain is a diagnostic, not an enforcement gate.
func (s *Service) Explain(ctx context.Context, q Query) (engine.Trace, error) {
	tr, err := s.eng.Explain(ctx, q.request())
	if err == nil {
		s.recordDecision(ctx, "Explain", q.Account, q.Principal, q.Object,
			tr.Decision.Allow, tr.Decision.Reason, nil)
	}
	return tr, err
}

// CheckBatch answers many queries in one call, returning results ALIGNED with qs
// (result[i] is the rendered decision for qs[i]). Each item is rendered exactly
// as Check: an operational failure folds into a deny Result (Err nil); an
// input-validation failure sets the item's Err. One bad query never fails the
// batch.
func (s *Service) CheckBatch(ctx context.Context, qs []Query) []engine.BatchResult[Result] {
	if qs == nil {
		return nil
	}
	out := make([]engine.BatchResult[Result], len(qs))
	for i, q := range qs {
		res, err := s.Check(ctx, q)
		out[i] = engine.BatchResult[Result]{Result: res, Err: err}
	}
	return out
}

// EnumerateBatch answers many enumeration queries in one call, aligned with qs.
// A query that errors carries its error in the item's Err; the rest are
// unaffected.
func (s *Service) EnumerateBatch(ctx context.Context, qs []EnumerateQuery) []engine.BatchResult[[]string] {
	if qs == nil {
		return nil
	}
	reqs := make([]engine.EnumerateRequest, len(qs))
	for i, q := range qs {
		reqs[i] = q.request()
	}
	return s.eng.EnumerateBatch(ctx, reqs)
}

// ObjectIdentifiers returns every valid identifier of objectType, enumerated
// from the type's provider (the complete, unbounded set). When exclude is
// non-empty the excluded ids are removed, so the result is the positive
// allow-list an EXCLUSIVE allowance ("all objects of this type except these
// ids") materialises to. An object-type with no declared provider yields
// APERTURE_PROVIDER_UNREGISTERED; a facade built without WithProviders yields
// APERTURE_UNIMPLEMENTED. The ids are returned as canonical strings, sorted.
func (s *Service) ObjectIdentifiers(ctx context.Context, objectType string, exclude ...string) ([]string, error) {
	if s.providers == nil {
		return nil, aerr.New(aerr.APERTURE_UNIMPLEMENTED, "service: object providers are not wired")
	}
	excluded := make([]identity.Identity, 0, len(exclude))
	for _, raw := range exclude {
		id, err := identity.Parse(raw)
		if err != nil {
			return nil, err // APERTURE_IDENTITY_INVALID
		}
		excluded = append(excluded, id)
	}
	ids, err := s.providers.IdentifiersExcept(ctx, objectType, excluded...)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out, nil
}

// ExplainBatch answers many explain queries in one call, aligned with qs. A
// query that errors carries its error in the item's Err; the rest are
// unaffected.
func (s *Service) ExplainBatch(ctx context.Context, qs []Query) []engine.BatchResult[engine.Trace] {
	if qs == nil {
		return nil
	}
	reqs := make([]engine.Request, len(qs))
	for i, q := range qs {
		reqs[i] = q.request()
	}
	return s.eng.ExplainBatch(ctx, reqs)
}
