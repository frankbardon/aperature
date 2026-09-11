package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
)

// Enumerating THROUGH a declared object reference.
//
// EnumerateRequest.Fields answers "which datasets contain brand Y?" — a FILTER,
// applied to each candidate's own metadata. This file answers the mirror-image
// question, "which brands belong to dataset X?", which is not a filter at all: a
// brand holds no field pointing back at its datasets (E3-S1 declares references
// on the HOLDING side only), so there is no predicate on brand that expresses it.
// It is a DEREFERENCE — read dataset X's declared reference field, and restrict
// the enumeration to the identities it names — and that is why it is a separate
// input rather than another entry in Fields.
//
// The security semantics are the whole point of doing it here rather than making
// the caller fetch the dataset and enumerate the ids itself:
//
//   - THE HOLDER IS CHECKED, FAIL-CLOSED. The principal must pass the same
//     decision against the holder object, for the same action, that Check would
//     make. It does not, the result is EMPTY and there is no error: "you may not
//     see dataset X" and "dataset X contains nothing you may see" have to be
//     indistinguishable, or the edge becomes an oracle for objects the caller was
//     never allowed to know about.
//   - THE EXISTENCE ORACLE IS BOUNDED. An absent holder is APERTURE_NOT_FOUND
//     only when it is INSIDE the request's account, and only once account
//     membership has passed. Outside the account, or for a non-member, the answer
//     is empty and never NOT_FOUND. That keeps the ergonomics a typo deserves
//     while confining the disclosure to people already inside the account, which
//     is the house rule against leaking cross-account data through an error
//     message applied to the one place a reference could break it.
//   - DANGLING REFERENCES ARE TOLERATED. An application-level foreign key has no
//     database constraint behind it, so a referenced brand that no longer exists
//     is SKIPPED, logged at warning, and recorded as a rules.NoteDanglingReference
//     — never a failed decision. One deleted brand must not take down every
//     decision on the dataset that still lists it.
//
// EXACTLY ONE HOP. An edge dereferences the holder's field and stops; the
// identities it yields are not themselves dereferenced, however many references
// their own type declares. Transitive traversal is a different feature with a
// different cost model, and it is not this one.

// accountSegmentType is the segment type an account-scoped identity leads with
// ("account:acme/dataset:7"). It is how an identity says which account it belongs
// to, and therefore how the NOT_FOUND-vs-empty boundary is drawn.
const accountSegmentType = "account"

// ReferenceEdge restricts an enumeration to the identities ONE holder object's
// declared reference field contains — "the brands in dataset X".
//
// The declared target object-type is not stated here: it comes from the
// declaration itself (provider.Registry.ReferenceTarget), so an edge cannot
// disagree with the wiring about what a field points at.
type ReferenceEdge struct {
	// HolderType is the object-type that DECLARES the reference ("dataset") — the
	// type the references: block was written on. OPTIONAL: empty means "whatever
	// HolderID's terminal segment type is". When it IS given it must agree with
	// HolderID, so the three-field {holder_type, holder_id, field} spelling a
	// non-Go surface uses can never drift from the identity it carries.
	HolderType string
	// HolderID is the holder object's CANONICAL IDENTITY —
	// "account:acme/dataset:7", or "dataset:7" in a deployment whose identities
	// are not account-scoped. Mandatory.
	HolderID string
	// Field is the metadata field on HolderType that a references: declaration
	// binds to a target object-type ("current_brands"). Mandatory, and it must be
	// DECLARED: an undeclared field is a coded error, never an empty edge.
	Field string
}

// String renders an edge as "account:acme/dataset:7.current_brands".
func (r ReferenceEdge) String() string { return r.HolderID + "." + r.Field }

// ReferenceSource is the declared-reference seam Enumerate dereferences a
// ReferenceEdge through. It is *provider.Registry's shape — the same registry
// that already backs the scope lister and the Fields predicate — so a deployment
// wires ONE object source for all three and every fetch is served from the
// per-type cache it already warmed.
//
// It embeds MetadataFetcher because dereferencing needs metadata twice: once for
// the holder (which also decides whether the holder EXISTS) and once per
// referenced identity (which decides whether it is DANGLING).
type ReferenceSource interface {
	MetadataFetcher
	// Has reports whether objectType has a registered provider, so "this type
	// serves nothing" and "this field declares nothing" stay distinguishable.
	Has(objectType string) bool
	// ReferenceTarget answers "what does dataset.current_brands point at?".
	ReferenceTarget(objectType, field string) (string, bool)
	// ResolveReferenceValue turns a reference field's already-fetched value into
	// the identities it names, rejecting anything that does not point at the
	// declared target.
	ResolveReferenceValue(objectType, field string, value any) ([]identity.Identity, error)
}

// compile-time assertion: the registry a host already wires is a usable
// reference source, with no adapter in between.
var _ ReferenceSource = (*provider.Registry)(nil)

// WithReferences gives the engine the declared-reference source Enumerate's
// reference edges are dereferenced through — normally the *provider.Registry
// that already backs the scope lister and the Fields predicate.
//
// It is only consulted when a request actually carries References; an engine
// wired without it keeps exactly today's behaviour for every enumeration that
// carries none, and fails closed (a coded error, never an empty result that
// reads as "no access") for one that does.
func WithReferences(r ReferenceSource) Option {
	return func(e *Engine) { e.references = r }
}

// WithLogger sets the logger the engine reports non-fatal operational findings
// to — a dangling object reference, which is skipped rather than raised, and an
// enumeration that came back holding exactly its bound. Both would otherwise be
// invisible to an operator: neither changes the result's shape. A nil logger, or
// none at all, means slog.Default().
//
// It is NOT a decision log: the engine logs nothing on the Check hot path.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.logger = l
		}
	}
}

// log returns the engine's logger, defaulting to the process logger. It is only
// reached from a path that has something to report.
func (e *Engine) log() *slog.Logger {
	if e.logger != nil {
		return e.logger
	}
	return slog.Default()
}

// referenceRestriction dereferences every edge in req and returns the set of
// canonical object ids an enumeration is restricted to, keyed by id string.
//
// The three results are distinct on purpose:
//
//   - (nil, true, nil)   — the request carries no edges; nothing is restricted.
//   - (set, true, nil)   — restrict to set. It may be EMPTY, which restricts to
//     nothing; that is a real answer, not "unrestricted".
//   - (nil, false, nil)  — FAIL CLOSED: the caller may not see a holder, or a
//     holder outside the account is absent. The enumeration returns an empty
//     result and no error.
//   - (nil, false, err)  — a wiring fault, an unreadable source, or the bounded
//     NOT_FOUND. A non-result the caller must not read as "no access".
//
// Several edges intersect (AND): "the brands in dataset X and in campaign Z".
// grants and permCache are the ones the enumeration already loaded, so the
// holder's check runs against the SAME subject set the enumeration does — which
// is what makes EnumerateAs check the holder with the impersonated authority
// rather than the operator's.
func (e *Engine) referenceRestriction(
	ctx context.Context,
	req EnumerateRequest,
	decReq Request,
	subject effectivePrincipal,
	grants []model.Grant,
	permCache map[string]*model.Permission,
) (map[string]struct{}, bool, error) {
	if len(req.References) == 0 {
		return nil, true, nil
	}
	if e.references == nil {
		return nil, false, aerr.New(aerr.APERTURE_PROVIDER_UNREGISTERED,
			"engine: enumerate reference edges need a declared-reference source, none is configured")
	}

	var restricted map[string]struct{}
	for _, edge := range req.References {
		holder, holderType, err := edge.holder()
		if err != nil {
			return nil, false, err
		}
		// Wiring faults are raised BEFORE anything about the holder is read: they
		// describe the deployment, not the data, so they carry no disclosure and a
		// caller deserves to hear about them loudly rather than as an empty page.
		if !e.references.Has(holderType) {
			return nil, false, aerr.WithContext(aerr.APERTURE_PROVIDER_UNREGISTERED,
				fmt.Sprintf("engine: cannot dereference %s.%s: no provider is registered for object type %q",
					holderType, edge.Field, holderType),
				map[string]any{"object_type": holderType, "field": edge.Field})
		}
		target, declared := e.references.ReferenceTarget(holderType, edge.Field)
		if !declared {
			return nil, false, aerr.WithContext(aerr.APERTURE_PROVIDER_REFERENCE_INVALID,
				fmt.Sprintf("engine: field %s.%s is not a declared reference", holderType, edge.Field),
				map[string]any{"object_type": holderType, "field": edge.Field})
		}

		// Existence is resolved BEFORE the holder's check, because the two answers
		// are different disclosures with different boundaries: absence is reported
		// to any member of the account the holder lives in, permission is not
		// reported at all. Deciding the check first would collapse "no such
		// dataset" into "no access" for everyone and throw the ergonomics away.
		md, err := e.references.Fetch(ctx, holder)
		if err != nil {
			if aerr.CodeOf(err) == aerr.APERTURE_NOT_FOUND {
				if identityInAccount(holder, req.Account) {
					return nil, false, err
				}
				// Outside the account: empty, never NOT_FOUND. This is the
				// disclosure boundary.
				return nil, false, nil
			}
			return nil, false, err
		}

		holderReq := decReq
		holderReq.Object = holder.String()
		dec, err := e.evaluate(ctx, holderReq, subject, holder, grants, permCache)
		if err != nil {
			return nil, false, err
		}
		if !dec.Allow {
			return nil, false, nil
		}

		// One hop: the value already in hand is resolved to identities and the walk
		// stops there. Nothing consults the target type's own references.
		ids, err := e.references.ResolveReferenceValue(holderType, edge.Field, md[edge.Field])
		if err != nil {
			return nil, false, err
		}
		edgeSet := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			live, err := e.referencedObjectExists(ctx, id)
			if err != nil {
				return nil, false, err
			}
			if !live {
				e.noteDanglingReference(ctx, holderType, edge.Field, target, id)
				continue
			}
			edgeSet[id.String()] = struct{}{}
		}

		if restricted == nil {
			restricted = edgeSet
		} else {
			restricted = intersectIDs(restricted, edgeSet)
		}
		// Deliberately NO short-circuit on an empty intersection. Stopping early
		// would make whether a later edge's wiring fault, absent holder, or
		// dangling entry is reported depend on how much DATA the earlier edges
		// happened to hold — an error that appears and disappears with the
		// contents of a table is worse than the work of one more lookup.
	}
	return restricted, true, nil
}

// referencedObjectExists reports whether the target type's provider still serves
// id. An APERTURE_NOT_FOUND is the DANGLING case and is not an error — that is
// the whole tolerance policy; any other failure is a real non-result and is
// returned, because silently dropping ids on a provider outage would read as a
// narrower answer rather than a broken one.
func (e *Engine) referencedObjectExists(ctx context.Context, id identity.Identity) (bool, error) {
	if _, err := e.references.Fetch(ctx, id); err != nil {
		if aerr.CodeOf(err) == aerr.APERTURE_NOT_FOUND {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// noteDanglingReference records a skipped reference twice, for two audiences.
//
// The OPERATOR gets a warning log naming the missing identity, because fixing it
// means knowing which row to fix and a log stays inside the deployment.
//
// The CALLER gets a rules.Note on the evaluation-notes channel — the same
// context-borne collector Explain installs and renders — which names the
// DECLARATION and the target type but never the missing identity, because a note
// travels out through Explain across account boundaries exactly as an error
// message does. Enumerate installs no collector of its own (it returns []string
// and grows no diagnostics channel), so this costs nothing unless something is
// already listening.
func (e *Engine) noteDanglingReference(ctx context.Context, holderType, field, target string, missing identity.Identity) {
	rules.NoteCollectorFrom(ctx).Add(rules.Note{
		Kind:     rules.NoteDanglingReference,
		Path:     holderType + "." + field,
		Expected: target,
		Actual:   "absent",
	})
	e.log().WarnContext(ctx, "engine: skipped a dangling object reference",
		"reference", holderType+"."+field,
		"target", target,
		"missing", missing.String())
}

// holder resolves the edge's holder identity and the object-type its reference
// declaration is looked up under, rejecting a malformed or self-contradicting
// edge with APERTURE_INVALID_INPUT before any storage or provider work happens.
func (r ReferenceEdge) holder() (identity.Identity, string, error) {
	if strings.TrimSpace(r.HolderID) == "" {
		return identity.Identity{}, "", aerr.New(aerr.APERTURE_INVALID_INPUT,
			"engine: enumerate reference edge is missing its holder id")
	}
	if strings.TrimSpace(r.Field) == "" {
		return identity.Identity{}, "", aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"engine: enumerate reference edge is missing its field",
			map[string]any{"holder": r.HolderID})
	}
	holder, err := identity.Parse(r.HolderID)
	if err != nil {
		return identity.Identity{}, "", err
	}
	got := terminalTypeOf(holder)
	if r.HolderType != "" && r.HolderType != got {
		return identity.Identity{}, "", aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			fmt.Sprintf("engine: enumerate reference edge declares holder type %q but %q is a %q",
				r.HolderType, r.HolderID, got),
			map[string]any{"holder_type": r.HolderType, "holder": r.HolderID, "actual_type": got})
	}
	return holder, got, nil
}

// terminalTypeOf returns the object-type an identity belongs to: the type of its
// LAST segment, so "account:acme/dataset:7" is a dataset.
func terminalTypeOf(id identity.Identity) string {
	segs := id.Segments()
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1].Type
}

// identityInAccount reports whether id belongs to account, which is what bounds
// the NOT_FOUND existence oracle.
//
// An identity says which account it belongs to by LEADING with an account
// segment. One that does not lead with an account segment ("dataset:7", in a
// deployment that does not scope identities by account) is not OUTSIDE the
// account either — the account boundary simply is not expressed in the identity,
// so there is no cross-account inference to protect and the ergonomic answer
// applies.
func identityInAccount(id identity.Identity, account string) bool {
	segs := id.Segments()
	if len(segs) == 0 || segs[0].Type != accountSegmentType {
		return true
	}
	return segs[0].ID == account
}

// intersectIDs returns the ids present in both sets — the AND of two reference
// edges. It allocates a fresh set rather than pruning either input, so a caller's
// per-edge set is never mutated behind its back.
func intersectIDs(a, b map[string]struct{}) map[string]struct{} {
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	out := make(map[string]struct{}, len(small))
	for id := range small {
		if _, ok := large[id]; ok {
			out[id] = struct{}{}
		}
	}
	return out
}
