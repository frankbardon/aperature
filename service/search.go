package service

import (
	"context"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// SearchQuery is a name-resolution question in surface-neutral form: which
// objects under Pattern, that Principal may take Action on in Account, are the
// ones a person means when they type Query.
//
// It mirrors engine.SearchRequest so the engine type stays an engine-internal
// concern, and it is the type every surface marshals to and from.
//
// The facade passes every field to the engine UNCHANGED — it does not normalise
// the query text, coerce a predicate value, or parse a holder identity. That is
// the same rule EnumerateQuery follows and for the same reason: each of those is
// a decision with a disclosure or correctness consequence, and a surface that
// made it separately would be a second place for the answer to differ.
type SearchQuery struct {
	// Account is the active account the search is scoped to.
	Account string
	// Principal is the id of the principal whose access bounds the search. It is
	// the SUBJECT being searched as — the result is what THIS principal may see,
	// which a surface must not confuse with whoever is calling.
	Principal string
	// Action is the verb the candidates must be permitted for.
	Action string
	// Pattern is the identity pattern bounding the search. A type-scoped search
	// is spelled as that type's pattern, e.g. "account:acme/brand:*".
	Pattern string
	// Query is the free text matched against object metadata. Mandatory — an
	// empty query is refused rather than read as "everything", because what it
	// would return is the subject's whole entitled set, unranked. That question
	// is Enumerate's.
	Query string `json:"Query" jsonschema:"Free text to match against object metadata, e.g. a brand name as a person would type it. Matching is case- and punctuation-insensitive and tolerates a typo. Required."`
	// MatchFields OPTIONALLY restricts which metadata fields the query is matched
	// against. Empty (the default) searches every field carrying string material.
	//
	// Aperture has no notion of a "label": a label is an ordinary metadata field
	// whose name the host chose. Name the field here, and read which one actually
	// matched off SearchMatch.Field.
	MatchFields []string `json:"MatchFields,omitempty" jsonschema:"Optional metadata field names to restrict matching to, e.g. ['label']. Omit to search every field holding text."`
	// Fields are OPTIONAL object-metadata predicates, identical in meaning to
	// EnumerateQuery.Fields. They compose with Query: the predicate narrows, the
	// query ranks what is left.
	Fields map[string]any `json:"Fields,omitempty" jsonschema:"Optional object-metadata predicates narrowing the candidate set before ranking. ANDed; a field the object does not carry never matches; a list-valued field matches by membership; everything else by typed equality, so \"5\" never matches 5. Omit to filter nothing."`
	// References are OPTIONAL reference edges, identical in meaning to
	// EnumerateQuery.References — "the brand called Nike in dataset 7" is one
	// call.
	References []ReferenceEdge `json:"References,omitempty" jsonschema:"Optional reference edges restricting the candidate set to the identities a holder object's declared reference field contains, e.g. the brands in dataset X. A holder the principal may not see yields an empty result, not an error. Omit to restrict nothing."`
	// MinScore is the score a match must reach to be returned, in [0,1]. Zero or
	// negative means provider.DefaultMinScore. Raising it narrows the shortlist;
	// it never widens what the subject may see.
	MinScore float64 `json:"MinScore,omitempty" jsonschema:"Optional score floor in [0,1]; a match below it is dropped. Omit for the default, which admits a single typo but not an unrelated word."`
	// Limit caps the number of returned MATCHES — the top of a finished ranking,
	// not a bound on the scan. <= 0 means the engine's configured bound; a Limit
	// above it is clamped down to it.
	Limit int `json:"Limit,omitempty" jsonschema:"Optional cap on how many ranked matches come back. The scan is unaffected: the best N are returned, not the first N found."`
}

// SearchMatch is one ranked candidate in surface-neutral form. It mirrors
// engine.SearchResult.
type SearchMatch struct {
	// Object is the matched object's canonical identity string.
	Object string `json:"Object" jsonschema:"Canonical identity of the matched object, e.g. 'account:acme/brand:42'."`
	// Score is the match quality in [0,1], 1 being an exact match once both sides
	// are normalised. It RANKS; nothing in Aperture authorizes on it.
	Score float64 `json:"Score" jsonschema:"Match quality from 0 to 1, 1 being exact. A ranking hint for choosing between candidates — never a permission."`
	// Field is the metadata field whose value produced the score.
	Field string `json:"Field" jsonschema:"The metadata field whose value produced this score."`
	// Value is the string that was scored — the field's value, or the matching
	// element when the field is an array.
	Value string `json:"Value" jsonschema:"The string that was scored: the field's value, or the matching element of a list-valued field."`
	// Metadata is the object's full metadata bag, exactly as ObjectMetadata would
	// return it for this id. It rides along so labelling a result set costs no
	// further call; it discloses nothing new, since the object has already been
	// decided allowed for this subject.
	Metadata map[string]any `json:"Metadata" jsonschema:"The object's full metadata, as ObjectMetadata would return it — so a caller never fetches labels one id at a time."`
}

// request adapts a SearchQuery to the engine's SearchRequest.
func (q SearchQuery) request() engine.SearchRequest {
	return engine.SearchRequest{
		Account:     q.Account,
		Principal:   q.Principal,
		Action:      q.Action,
		Pattern:     q.Pattern,
		Query:       q.Query,
		MatchFields: q.MatchFields,
		Fields:      q.Fields,
		References:  q.references(),
		MinScore:    q.MinScore,
		Limit:       q.Limit,
	}
}

// references converts the surface-neutral edges to the engine's, field for
// field. A nil or empty slice stays nil, so an omitted edge list is
// indistinguishable from none at every layer.
func (q SearchQuery) references() []engine.ReferenceEdge {
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

// matches adapts engine results to the surface-neutral form.
func matches(res []engine.SearchResult) []SearchMatch {
	out := make([]SearchMatch, len(res))
	for i, r := range res {
		out[i] = SearchMatch{
			Object:   r.Object,
			Score:    r.Score,
			Field:    r.Field,
			Value:    r.Value,
			Metadata: r.Metadata,
		}
	}
	return out
}

// Search resolves a NAME to an id: the objects under q.Pattern that q.Principal
// may take q.Action on AND whose metadata matches q.Query, ranked best first.
//
// It is the surface a caller reaches for when a question arrives in a person's
// own words and every downstream step needs ids. Composing the pieces that
// already exist — enumerate a whole type, fetch each id's metadata, match in the
// caller — is not the same thing and must not be treated as a substitute: that
// shape puts the UNSCOPED set across the boundary first and filters afterwards,
// so any bug in the filter turns the calling surface into an enumeration oracle
// for every object in the system. Here the entitlement decision happens BEFORE
// the match, inside the engine, over the same walk Enumerate uses, and a score
// can only ever subtract from what Enumerate would have returned.
//
// Search SELECTS a candidate; it never authorizes one. A caller that acts on a
// returned id still Checks it — that this call proposed it is a shortlist, not
// an authority, and no grant or rule anywhere reads a score.
//
// Engine errors are returned verbatim, exactly as Enumerate's are: Search cannot
// fail open by construction, so an operational failure is a returned error and
// never a silently short list.
func (s *Service) Search(ctx context.Context, q SearchQuery) ([]SearchMatch, error) {
	res, err := s.eng.Search(ctx, q.request())
	if err != nil {
		return nil, err
	}
	out := matches(res)
	s.recordDecision(ctx, "Search", q.Account, q.Principal, q.Pattern, true,
		"ranked accessible objects by name", map[string]any{"count": len(out), "query": q.Query})
	return out, nil
}

// SearchBatch answers many search queries in one call, returning results ALIGNED
// with qs (result[i] is the ranked list for qs[i]). A query that errors carries
// its error in the item's Err; the rest are unaffected.
//
// It is the shape a surface resolving several names out of one sentence needs —
// "Nike's score in footwear" names a brand and a category — so two lookups cost
// one round-trip.
func (s *Service) SearchBatch(ctx context.Context, qs []SearchQuery) []engine.BatchResult[[]SearchMatch] {
	if qs == nil {
		return nil
	}
	out := make([]engine.BatchResult[[]SearchMatch], len(qs))
	for i, q := range qs {
		res, err := s.Search(ctx, q)
		out[i] = engine.BatchResult[[]SearchMatch]{Result: res, Err: err}
	}
	return out
}

// ObjectMetadataBatch returns the metadata for many object ids in one call,
// ALIGNED with objectIDs (result[i] is the bag for objectIDs[i]). An id that
// fails — a malformed identity, an unregistered type, an absent object — carries
// its own error in that item; the rest are unaffected, so one bad id never fails
// the batch. It is the bulk form of ObjectMetadata and follows the same
// convention as CheckBatch / EnumerateBatch / ExplainBatch.
//
// # What it is for, and what it is not
//
// Any result set a caller renders needs labels, and a label is metadata: the ids
// behind a stored analysis scope, a cohort's own coverage list, the entitled set
// behind a question asked about "our brands". Without this, labelling N ids is N
// round-trips.
//
// It is NOT a way to discover ids. It authorizes nothing and filters nothing —
// it is exactly ObjectMetadata, N times, with the same posture — so a caller
// passes ids it already holds legitimately. The path that PRODUCES ids a subject
// may see is Enumerate, or Search when the input is a name.
//
// # Why the batch stops at this boundary
//
// The saving is the round-trip, and the round-trip is here. Below this, the
// provider registry already serves repeats from its per-type cache, and
// ObjectProvider.Fetch cannot grow a batch form without breaking every host
// implementation of the interface. A provider that can genuinely push a batch
// down into its own storage is a separate, optional seam and not one any caller
// needs to wait for.
//
// Requires WithProviders (else APERTURE_UNIMPLEMENTED, once, for the whole
// call — a missing dependency is the deployment's error, not per-id data).
// A nil objectIDs yields a nil result.
func (s *Service) ObjectMetadataBatch(ctx context.Context, objectIDs []string) ([]engine.BatchResult[map[string]any], error) {
	if s.providers == nil {
		return nil, aerr.New(aerr.APERTURE_UNIMPLEMENTED, "service: object providers are not wired")
	}
	if objectIDs == nil {
		return nil, nil
	}
	out := make([]engine.BatchResult[map[string]any], len(objectIDs))
	for i, raw := range objectIDs {
		id, err := identity.Parse(raw)
		if err != nil {
			out[i] = engine.BatchResult[map[string]any]{Err: err} // APERTURE_IDENTITY_INVALID
			continue
		}
		md, err := s.providers.Fetch(ctx, id)
		out[i] = engine.BatchResult[map[string]any]{Result: md, Err: err}
	}
	return out, nil
}
