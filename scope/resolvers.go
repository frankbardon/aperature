package scope

import (
	"context"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// implicitResolver covers every object of the permission's type that falls
// within the grant's pattern scope — "all objects of the type (unfettered)".
// Membership is the conjunction of the pattern match and a terminal-type check,
// so Contains never enumerates. Members must list the type and so requires an
// ObjectLister.
type implicitResolver struct {
	gc   GrantContext
	deps Deps
}

// newImplicitResolver is the implicit Factory. implicit takes no configuration;
// an id-list or rule on an implicit permission is a misconfiguration and is
// rejected so it cannot silently mask intent.
func newImplicitResolver(gc GrantContext, deps Deps) (ScopeResolver, error) {
	if len(gc.Spec.IDs) > 0 || gc.Spec.Rule != "" {
		return nil, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
			"implicit scope takes no ids or rule",
			map[string]any{"object_type": gc.ObjectType})
	}
	return implicitResolver{gc: gc, deps: deps}, nil
}

func (r implicitResolver) Contains(_ context.Context, object identity.Identity) (bool, error) {
	if !r.gc.Pattern.Matches(object) {
		return false, nil
	}
	return terminalOfType(object, r.gc.ObjectType), nil
}

func (r implicitResolver) Members(ctx context.Context, pattern identity.Pattern) ([]identity.Identity, error) {
	return enumerateOfType(ctx, r.deps, r.gc, pattern, nil)
}

// inclusiveResolver covers only the objects named by an explicit id-list (the
// list path) or selected by a rule (the rule path, which consults the
// RuleEvaluator). The grant's pattern still bounds membership, so a listed
// object outside the pattern is not covered.
type inclusiveResolver struct {
	gc   GrantContext
	deps Deps
}

// newInclusiveResolver is the inclusive Factory. inclusive must opt in via an
// id-list or a rule; declaring neither is a misconfiguration.
func newInclusiveResolver(gc GrantContext, deps Deps) (ScopeResolver, error) {
	if len(gc.Spec.IDs) == 0 && gc.Spec.Rule == "" {
		return nil, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
			"inclusive scope requires an ids list or a rule",
			map[string]any{"object_type": gc.ObjectType})
	}
	return inclusiveResolver{gc: gc, deps: deps}, nil
}

func (r inclusiveResolver) Contains(ctx context.Context, object identity.Identity) (bool, error) {
	if !r.gc.Pattern.Matches(object) {
		return false, nil
	}
	// List path: membership is exact identity-string equality against the list.
	if idInList(r.gc.Spec.IDs, object) {
		return true, nil
	}
	// Rule path: consult the rule evaluator only when a rule is declared, so a
	// pure list-backed grant never touches the rule dependency.
	if r.gc.Spec.Rule != "" {
		return r.deps.rules().Selected(ctx, r.gc.Spec.Rule, object,
			r.gc.Account, r.gc.PrincipalKind, r.gc.Principal, r.gc.Action)
	}
	return false, nil
}

// Members is the union of the two membership paths, which is exactly what
// Contains answers object-by-object — the two must not disagree, or Check and
// Enumerate answer differently about the same grant.
//
// The list half needs no lister: the members are the listed identities that fall
// within both the grant pattern and the query pattern. The rule half is
// list-then-filter, exactly as exclusive does it — a rule is an arbitrary
// expression over object metadata and is not invertible in general, so there is
// nothing to invert and no reverse index to consult. It therefore requires an
// ObjectLister, and reports APERTURE_SCOPE_LISTER_UNCONFIGURED when none is
// wired rather than returning an empty set: an empty member list is an answer,
// and a missing dependency must not be able to impersonate one.
func (r inclusiveResolver) Members(ctx context.Context, pattern identity.Pattern) ([]identity.Identity, error) {
	limit := r.deps.maxMembers()
	out := make([]identity.Identity, 0, len(r.gc.Spec.IDs))
	// seen deduplicates across the two halves: an id that is both listed and
	// rule-selected is one member, not two. It is built only for the id-list —
	// Contains stays map-free on the hot path.
	seen := make(map[string]struct{}, len(r.gc.Spec.IDs))
	for _, raw := range r.gc.Spec.IDs {
		id, err := identity.Parse(raw)
		if err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_SCOPE_INVALID, err,
				"inclusive scope id %q is not a valid identity", raw)
		}
		if !r.gc.Pattern.Matches(id) || !pattern.Matches(id) {
			continue
		}
		key := id.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
		if len(out) >= limit {
			return out, nil
		}
	}
	if r.gc.Spec.Rule == "" {
		return out, nil
	}
	// Contains applies the grant pattern, the id-list, and the rule, so a
	// candidate the list already contributed is simply filtered out below.
	selected, err := enumerateOfType(ctx, r.deps, r.gc, pattern, r.Contains)
	if err != nil {
		return nil, err
	}
	for _, id := range selected {
		key := id.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// exclusiveResolver covers every object of the type within the pattern scope
// EXCEPT those named by the minus id-list or excluded by a rule (which consults
// the RuleEvaluator). Contains needs no enumeration; Members must enumerate the
// type and so requires an ObjectLister.
type exclusiveResolver struct {
	gc   GrantContext
	deps Deps
}

// newExclusiveResolver is the exclusive Factory. exclusive is an opt-out, so it
// requires a minus list or a rule; declaring neither would make it identical to
// implicit and is rejected as a misconfiguration.
func newExclusiveResolver(gc GrantContext, deps Deps) (ScopeResolver, error) {
	if len(gc.Spec.IDs) == 0 && gc.Spec.Rule == "" {
		return nil, aerr.WithContext(aerr.APERTURE_SCOPE_INVALID,
			"exclusive scope requires a minus ids list or a rule",
			map[string]any{"object_type": gc.ObjectType})
	}
	return exclusiveResolver{gc: gc, deps: deps}, nil
}

func (r exclusiveResolver) Contains(ctx context.Context, object identity.Identity) (bool, error) {
	if !r.gc.Pattern.Matches(object) {
		return false, nil
	}
	if !terminalOfType(object, r.gc.ObjectType) {
		return false, nil
	}
	// List path: excluded iff the object is named by the minus list.
	if idInList(r.gc.Spec.IDs, object) {
		return false, nil
	}
	// Rule path: excluded iff the rule selects the object.
	if r.gc.Spec.Rule != "" {
		excluded, err := r.deps.rules().Selected(ctx, r.gc.Spec.Rule, object,
			r.gc.Account, r.gc.PrincipalKind, r.gc.Principal, r.gc.Action)
		if err != nil {
			return false, err
		}
		if excluded {
			return false, nil
		}
	}
	return true, nil
}

func (r exclusiveResolver) Members(ctx context.Context, pattern identity.Pattern) ([]identity.Identity, error) {
	// Enumerate all-of-type within scope, then drop the excluded ones via
	// Contains (which applies both the minus list and any rule exclusion).
	return enumerateOfType(ctx, r.deps, r.gc, pattern, r.Contains)
}

// idInList reports whether object's canonical identity string is in ids. The
// list is typically small, so a linear scan keeps Contains allocation-free on
// the hot path (no map is built per evaluation).
func idInList(ids []string, object identity.Identity) bool {
	if len(ids) == 0 {
		return false
	}
	s := object.String()
	for _, id := range ids {
		if id == s {
			return true
		}
	}
	return false
}

// enumerateOfType lists every object of the grant's type bounded by both the
// grant pattern and the query pattern, optionally filtering each candidate
// through keep (used by exclusive to drop excluded objects, and by inclusive to
// keep rule-selected ones). It requires an ObjectLister and surfaces
// APERTURE_SCOPE_LISTER_UNCONFIGURED when none is wired.
//
// The gather is bounded by deps.maxMembers(), and that same number is the limit
// handed to ObjectLister.List — the seam already carries one, so the configured
// ceiling reaches the provider through the existing parameter rather than a new
// one. Asking the lister for exactly what this gather can keep is what makes a
// raised bound produce more objects instead of more discarded candidates.
func enumerateOfType(
	ctx context.Context,
	deps Deps,
	gc GrantContext,
	query identity.Pattern,
	keep func(context.Context, identity.Identity) (bool, error),
) ([]identity.Identity, error) {
	limit := deps.maxMembers()
	candidates, err := deps.lister().List(ctx, gc.ObjectType, gc.Pattern, limit)
	if err != nil {
		return nil, err
	}
	out := make([]identity.Identity, 0, len(candidates))
	for _, obj := range candidates {
		if !gc.Pattern.Matches(obj) || !query.Matches(obj) {
			continue
		}
		if keep != nil {
			ok, err := keep(ctx, obj)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, obj)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
