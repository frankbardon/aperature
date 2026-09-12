package engine

import (
	"context"
	"sort"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
)

// Search resolves a NAME to an id, which is the question a surface that takes
// input in a person's own words has to answer before it can ask any other one.
//
// # Why it is not a filtered Enumerate the caller runs itself
//
// Every ingredient looks available already: Enumerate returns the ids a subject
// may act on, and ObjectMetadata returns a bag per id. Composing them in the
// caller fails on the count that matters — the composition inverts the
// entitlement model. Enumerating a whole type and filtering afterwards means the
// unscoped set has already crossed the boundary, and any bug in the filter turns
// the calling surface into an oracle for every object in the system. It is also
// one round-trip per candidate with no ranking, but that is the cost argument,
// and the cost argument is not the one that decides this.
//
// So the requirement is stated positively and held structurally: the result is
// the intersection of "matches the query" and "this subject could have obtained
// it via Enumerate", computed as ONE operation. Search walks the same
// walkAllowed pipeline Enumerate walks — same candidate gather, same
// deny-overrides decision per candidate, same reference restriction, same Fields
// predicate — and only then scores what survived. Nothing is filtered after the
// fact and nothing is scored before the decision.
//
// # Search selects; it never authorizes
//
// A score is a ranking hint for a human (or an agent) choosing between
// candidates. No grant, rule, or verdict anywhere in Aperture reads one. A
// caller that acts on a selected id still Checks it — Search having proposed it
// is not authority, it is a shortlist.
type SearchRequest struct {
	// Account is the active account the search is scoped to. Mandatory.
	Account string
	// Principal is the id of the principal whose access bounds the search.
	// Mandatory. As with Enumerate this is the SUBJECT being searched as, which
	// a surface must not confuse with the caller: the result is what THIS
	// principal may see.
	Principal string
	// Action is the verb the candidates must be permitted for (e.g. "read").
	// Mandatory. A search is scoped to an action for the same reason an
	// enumeration is: "which brands can I see?" is meaningless without saying
	// see how.
	Action string
	// Pattern is the identity pattern bounding the search. Mandatory. An
	// object-type-scoped search is spelled as the pattern for that type —
	// "account:acme/brand:*" — because the candidate set is bounded by a pattern
	// at every other entry point and growing a second spelling for the same
	// bound would be a second place for the two to disagree.
	Pattern string
	// Query is the free text to match against object metadata. Mandatory: an
	// empty query is rejected rather than treated as "match everything", because
	// the thing it would match everything in is the subject's whole entitled set,
	// which is precisely the unranked bulk read Search exists to replace. A
	// caller that wants that set is asking for Enumerate.
	Query string
	// MatchFields OPTIONALLY restricts which metadata fields the query is
	// matched against. Empty (the default) searches every field carrying string
	// material.
	//
	// It is a []string of field NAMES, not a predicate — it says where to look,
	// never what to find — which is why it is a separate field from Fields
	// rather than a mode of it.
	//
	// Aperture has no notion of a "label": a label is an ordinary metadata field
	// whose name the host chose. A caller that means one specific field names it
	// here, and reads which field actually matched off SearchResult.Field.
	MatchFields []string
	// Fields are OPTIONAL object-metadata predicates, identical in meaning to
	// EnumerateRequest.Fields and evaluated by the same provider.MatchFields.
	// They compose with Query: the predicate narrows the candidate set, the
	// query ranks what is left. "The Nike in footwear" is one call.
	Fields map[string]any
	// References are OPTIONAL reference edges, identical in meaning to
	// EnumerateRequest.References. They compose with Query the same way — "the
	// brand called Nike in dataset 7" is one call — and they carry the same
	// fail-closed rules, including that a holder the principal may not see
	// yields an empty result rather than an error.
	References []ReferenceEdge
	// MinScore is the score a match must reach to be returned, in [0,1]. Zero or
	// negative means provider.DefaultMinScore.
	//
	// It exists because a fuzzy matcher with no floor ranks the subject's ENTIRE
	// entitled set and returns it — which is both useless as a shortlist and the
	// bulk disclosure a ranked search is supposed to make unnecessary. Raising it
	// narrows the shortlist; it never widens what the subject may see.
	MinScore float64
	// Limit caps the number of returned MATCHES. <= 0 means the engine's
	// configured bound; a Limit above it is clamped down to it, exactly as
	// Enumerate's does.
	//
	// It does NOT bound the scan. Ranking is meaningless if the candidates are
	// truncated before they are scored — asking for the best 10 would return the
	// first 10 allowed objects that happened to match at all — so the walk covers
	// the same candidate set Enumerate would cover at the engine's bound, and
	// Limit takes the top of the finished ranking. See Search's bound note.
	Limit int
}

// SearchResult is one ranked candidate: the object, how well it matched, the
// metadata field and value that produced the match, and the object's full
// metadata bag.
//
// Metadata rides along because the caller that asked "which object is called
// nike?" invariably needs to render what it found, and fetching it per id
// afterwards would restore the one-round-trip-per-object cost the search was
// asked to remove. It is the same bag ObjectMetadata returns for that id, from
// the same cached source, and it discloses nothing the caller could not already
// fetch — the object has already been decided allowed for this subject.
type SearchResult struct {
	// Object is the matched object's canonical identity string.
	Object string
	// Score is the match quality in [0,1] — 1 is an exact match once both sides
	// are normalised. It ranks; it decides nothing.
	Score float64
	// Field is the metadata field whose value produced the score.
	Field string
	// Value is the string that was scored: the field's value, or the matching
	// element when the field is an array.
	Value string
	// Metadata is the object's full metadata bag, as ObjectMetadata would return
	// it. Never nil for a returned match — an object with no metadata has nothing
	// to match and never becomes one.
	Metadata provider.Metadata
}

// Search returns the objects under Pattern that Principal may take Action on in
// Account AND whose metadata matches Query, ranked best first.
//
// Every returned object is one Check would allow: the candidates are decided
// before they are scored, by the same walk Enumerate uses (see walkAllowed), so
// the result set is a subset of what Enumerate would have returned for the same
// subject, action and pattern. A score can only ever subtract from that set.
//
// Matching is provider.MatchText: case- and punctuation-insensitive, tolerant of
// a single transposition or typo on tokens long enough for one to be
// unambiguous, and ranked by how completely the query accounts for the candidate
// and the candidate for the query. Only STRING material is matched — a top-level
// string field and the string elements of a top-level array — because filtering
// by a number, a bool or a date is what Fields is for.
//
// # The bound, and why it is not Limit
//
// The scan is bounded by the engine's configured enumeration bound
// (WithEnumerateLimit, default DefaultEnumerateLimit) — the same bound and the
// same candidate set Enumerate runs under. Limit then takes the top of the
// finished ranking. The two are separate numbers on purpose: bounding the scan
// by Limit would return the first Limit objects that matched at all rather than
// the Limit best, which is not a ranking. A scan that comes back holding exactly
// its bound is WARNED about through the engine's logger, because a better match
// may lie past it — the same hint, with the same caveat, that Enumerate raises.
//
// Ordering is deterministic: by score descending, then by canonical id
// ascending, so two equally good matches rank stably across calls.
//
// Search REQUIRES an object-metadata source (WithMetadata) — unlike Fields,
// which is only consulted when a request carries one. Without it the answer is
// an APERTURE_PROVIDER_UNREGISTERED error rather than an empty result, because
// an empty result reads as "you may see nothing" and would hide the
// misconfiguration behind a plausible answer.
func (e *Engine) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if err := validateSearchRequest(req); err != nil {
		return nil, err
	}
	if e.metadata == nil {
		return nil, aerr.WithContext(aerr.APERTURE_PROVIDER_UNREGISTERED,
			"engine: search needs an object-metadata source, none is configured",
			map[string]any{"pattern": req.Pattern})
	}
	enumReq := req.enumerateRequest()
	query, err := identity.ParsePattern(req.Pattern)
	if err != nil {
		return nil, err
	}
	member, err := e.requireMembership(ctx, req.Account, req.Principal)
	if err != nil {
		return nil, err
	}
	if !member {
		// Fail-closed, exactly as Enumerate: a non-member may act on nothing in
		// this account, so there is nothing here to name.
		return []SearchResult{}, nil
	}
	subjects, subject, err := e.subjectSet(ctx, req.Principal)
	if err != nil {
		return nil, err
	}
	return e.searchWithSubjects(ctx, req, enumReq, subject, query, subjects)
}

// SearchAs is the impersonation-aware sibling of Search: it ranks the objects the
// EFFECTIVE subject set may act on. Inert ic delegates to Search (an expired
// session searches only the operator's own access), and for an active session a
// cross-account boundary violation fails closed to the empty set.
//
// As in EnumerateAs, a rule-backed grant reads `principal.*` off the effective
// subject, so a search under become shortlists what the TARGET may see.
func (e *Engine) SearchAs(ctx context.Context, req SearchRequest, ic ImpersonationContext) ([]SearchResult, error) {
	if !ic.active(e.now()) {
		return e.Search(ctx, req)
	}
	if err := validateSearchRequest(req); err != nil {
		return nil, err
	}
	if e.metadata == nil {
		return nil, aerr.WithContext(aerr.APERTURE_PROVIDER_UNREGISTERED,
			"engine: search needs an object-metadata source, none is configured",
			map[string]any{"pattern": req.Pattern})
	}
	if err := validateImpersonation(req.Principal, ic); err != nil {
		return nil, err
	}
	enumReq := req.enumerateRequest()
	query, err := identity.ParsePattern(req.Pattern)
	if err != nil {
		return nil, err
	}
	subjects, subject, ok, err := e.elevatedSubjects(ctx, req.Account, ic)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []SearchResult{}, nil
	}
	ctx = WithImpersonation(ctx, ic)
	return e.searchWithSubjects(ctx, req, enumReq, subject, query, subjects)
}

// SearchBatch resolves many Search requests in one call, aligned with reqs
// (result[i] is the ranked list for reqs[i]). A request that errors yields an
// item with Err set and a nil list; the rest are unaffected.
//
// It is the shape a surface resolving several names from one sentence needs —
// "Nike's score in footwear" names a brand and a category — so the two lookups
// are one round-trip rather than two.
func (e *Engine) SearchBatch(ctx context.Context, reqs []SearchRequest) []BatchResult[[]SearchResult] {
	if reqs == nil {
		return nil
	}
	out := make([]BatchResult[[]SearchResult], len(reqs))
	for i, req := range reqs {
		res, err := e.Search(ctx, req)
		out[i] = BatchResult[[]SearchResult]{Result: res, Err: err}
	}
	return out
}

// searchWithSubjects runs the search over an already-resolved subject set, shared
// by Search and SearchAs the same way enumerateWithSubjects is shared by
// Enumerate and EnumerateAs.
func (e *Engine) searchWithSubjects(ctx context.Context, req SearchRequest, enumReq EnumerateRequest, subject effectivePrincipal, query identity.Pattern, subjects []model.Subject) ([]SearchResult, error) {
	scanBound := e.enumerateBound()
	minScore := req.MinScore
	if minScore <= 0 {
		minScore = provider.DefaultMinScore
	}

	scanned := 0
	matches := make([]SearchResult, 0, 16)
	err := e.walkAllowed(ctx, enumReq, subject, query, subjects, func(obj identity.Identity, md provider.Metadata) (bool, error) {
		scanned++
		// The walk hands over the bag it already fetched when a Fields predicate
		// made it pay for one; otherwise the fetch happens here, through the same
		// cached source, so a filtered and an unfiltered search cost the same per
		// candidate.
		if md == nil {
			fetched, err := e.metadata.Fetch(ctx, obj)
			if err != nil {
				// An allowed object whose metadata is simply absent has nothing to
				// match and is not a failure — the same restrictive direction the
				// Fields predicate takes for a missing bag.
				if aerr.CodeOf(err) == aerr.APERTURE_NOT_FOUND {
					return scanned < scanBound, nil
				}
				return false, err
			}
			md = fetched
		}
		if m, ok := provider.MatchText(md, req.Query, req.MatchFields); ok && m.Score >= minScore {
			matches = append(matches, SearchResult{
				Object:   obj.String(),
				Score:    m.Score,
				Field:    m.Field,
				Value:    m.Value,
				Metadata: md,
			})
		}
		// The scan covers the same candidate set Enumerate would at this bound,
		// NOT the caller's Limit: truncating before the ranking is complete would
		// return the first matches rather than the best ones.
		return scanned < scanBound, nil
	})
	if err != nil {
		return nil, err
	}

	// Score descending, then canonical id ascending. The id tiebreak is what makes
	// two equally good matches rank the same way on every call — sort.Slice is not
	// stable, and a shortlist that reshuffles between identical calls is a bug a
	// caller cannot diagnose.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].Object < matches[j].Object
	})

	e.warnAtSearchBound(ctx, req, scanBound, scanned)

	if limit := e.boundEnumerateLimit(req.Limit); len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// warnAtSearchBound reports, through the engine's logger, that a search scanned
// as many allowed candidates as it was allowed to scan.
//
// It matters more here than it does for Enumerate. A truncated enumeration
// returns a short list, which at least looks short; a truncated search returns a
// full, confidently ranked shortlist whose best entry may simply be the best
// among the candidates that fit. Nothing in the result can say so — the ranking
// is well-formed either way — so this is the only place an operator learns that
// the corpus was larger than the scan.
//
// As with Enumerate's warning it is a HINT, not an assertion: a candidate set of
// exactly the bound's size is indistinguishable from a truncated one.
func (e *Engine) warnAtSearchBound(ctx context.Context, req SearchRequest, bound, scanned int) {
	if scanned < bound {
		return
	}
	e.log().WarnContext(ctx,
		"engine: search scanned exactly its bound; a better match may lie beyond it",
		"bound", bound,
		"configured_bound", e.enumerateBound(),
		"requested_limit", req.Limit,
		"account", req.Account,
		"action", req.Action,
		"pattern", req.Pattern,
	)
}

// enumerateRequest projects the search onto the enumeration it is ranking. Limit
// is deliberately NOT carried across: the walk must cover the whole candidate
// set for the ranking to mean anything, and searchWithSubjects bounds the scan
// itself.
func (req SearchRequest) enumerateRequest() EnumerateRequest {
	return EnumerateRequest{
		Account:    req.Account,
		Principal:  req.Principal,
		Action:     req.Action,
		Pattern:    req.Pattern,
		Fields:     req.Fields,
		References: req.References,
	}
}

// validateSearchRequest rejects a request missing any required field, or one
// carrying an out-of-range score floor, before any storage or provider work
// happens.
func validateSearchRequest(req SearchRequest) error {
	switch {
	case req.Account == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: search account is empty")
	case req.Principal == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: search principal is empty")
	case req.Action == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: search action is empty")
	case req.Pattern == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "engine: search pattern is empty")
	case req.Query == "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"engine: search query is empty; enumerate the pattern instead of searching it for nothing")
	case req.MinScore > 1:
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"engine: search minimum score is above 1; no match can ever reach it",
			map[string]any{"min_score": req.MinScore})
	}
	return validateReferenceEdges(req.References)
}
