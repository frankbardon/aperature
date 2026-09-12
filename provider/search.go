package provider

import (
	"sort"
	"strings"
	"unicode"
)

// The shared TEXT-MATCH model, implemented once for every surface that resolves
// a NAME to an id.
//
// Fields (match.go) answers "which objects hold exactly this value?". It is
// exact, typed, and deliberately coercion-free, because an enumeration that
// selected an object a Check then denies is a silent disagreement between the
// two halves of the decision API. That property is why Fields cannot answer the
// other question a caller has — "which objects are people NAMING when they type
// `nike`?" — where "Nike", "nike", "Nike, Inc." and a transposed typo are all
// the same intent and none of them is an exact value.
//
// This file is that second question, and it is separated from the first on
// purpose: a text match RANKS candidates for a human to choose between, it never
// decides anything. Nothing in Aperture authorizes on a score.
//
// The rule, in one sentence: a query scores against a candidate string by how
// completely its tokens are accounted for, and a candidate is only as good as
// the share of ITSELF the query explains.
//
// # Why the scorer lives in provider
//
// The same reason MatchFields does. A host that can push a search down into its
// own storage (a SQL `LIKE`, a trigram index, an external search engine) must be
// able to rank the way Aperture would, or the ids a provider proposes and the
// ids Aperture would have proposed are two different answers to one question.
// ScoreText and MatchText are exported for exactly that.
//
// # What is searched, and what is not
//
// MatchText reads the metadata value model (metadata.go) and scores only the
// STRING material in it: a top-level string scalar, and the string elements of a
// top-level array. Numbers, bools and nil are never text-matched — filtering by
// one of those is what Fields is for — and nested objects are not descended
// into, because a match NAMES the field it came from and a nested path is not a
// field name a caller could restrict the search to.
//
// Aperture has no notion of a "label". A label is an ordinary metadata field
// whose name the HOST chose, so no field is privileged here: the score is the
// best over every searched field, and TextMatch.Field reports which one won. A
// caller that means one specific field says so, by name, through the fields
// argument.

const (
	// DefaultMinScore is the score a match must reach to be returned when the
	// caller sets no floor of its own. It sits just under the weakest score the
	// fuzzy tier can produce for a fully-explained candidate, so a single typo
	// still resolves while an unrelated word does not.
	DefaultMinScore = 0.4

	// fuzzyFloor is the per-token similarity an approximate match must reach
	// before it counts at all: 2/3, i.e. at most one edit in three characters.
	// Below it the two tokens are simply different words, and admitting them
	// would turn a ranked shortlist back into the whole entitled set.
	fuzzyFloor = 2.0 / 3.0

	// fuzzyWeight caps what an approximate token match can contribute, keeping
	// every fuzzy hit strictly below the weakest SUBSTRING hit. Being spelled
	// right matters more than being close.
	fuzzyWeight = 0.8

	// minFuzzyLen is the shortest token that may match approximately. On a three
	// letter token one edit is a third of the word — "IBM" would reach "IBN" —
	// so short tokens must be spelled exactly or be a prefix.
	minFuzzyLen = 4

	// maxFuzzyLen bounds the edit-distance matrix. Two tokens this long that are
	// not already equal, a prefix, or a substring are not the same word, and the
	// comparison is O(n*m) on the enumeration's hot path.
	maxFuzzyLen = 64

	// coverageFloor is the share of a candidate's score that survives when the
	// query explains almost none of it. A query that accounts for the whole
	// candidate keeps the full score; one that accounts for a fifth of a long
	// name keeps coverageFloor plus its fifth. It is what ranks "Nike Inc" above
	// "Nike Air Max Collection" for the query "nike" without excluding either.
	coverageFloor = 0.75
)

// TextMatch is one scored hit: which metadata field produced it, the string
// value that was scored, and the score itself. It is the unit MatchText returns
// and the unit a ranked result is built from.
type TextMatch struct {
	// Field is the metadata field name the matching value came from.
	Field string
	// Value is the string that was scored — the field's value, or the matching
	// element when the field is an array.
	Value string
	// Score is the match quality in [0,1]: 1 is an exact match after
	// normalisation, and anything above zero is some partial or approximate one.
	Score float64
}

// MatchText scores query against md and returns the single best hit, reporting
// false when nothing scored above zero.
//
// When fields is non-empty the search is restricted to those metadata field
// names; a named field the object does not carry is simply not searched (it is
// not an error, and it is not a match — the same restrictive direction an absent
// field takes in MatchFields). When fields is empty every field is searched.
//
// Ties are broken by field name in lexical order, so one object's best field is
// stable across calls and two runs of the same search rank identically.
func MatchText(md Metadata, query string, fields []string) (TextMatch, bool) {
	q := NormalizeText(query)
	if q == "" || len(md) == 0 {
		return TextMatch{}, false
	}
	var restrict map[string]struct{}
	if len(fields) > 0 {
		restrict = make(map[string]struct{}, len(fields))
		for _, f := range fields {
			restrict[f] = struct{}{}
		}
	}

	// Field order is not map order: a tie has to resolve the same way every time
	// or the same search returns a different Field on a second call.
	names := make([]string, 0, len(md))
	for name := range md {
		if restrict != nil {
			if _, want := restrict[name]; !want {
				continue
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)

	best := TextMatch{}
	for _, name := range names {
		for _, candidate := range searchableStrings(md[name]) {
			score := scoreNormalized(NormalizeText(candidate), q)
			if score > best.Score {
				best = TextMatch{Field: name, Value: candidate, Score: score}
			}
		}
	}
	return best, best.Score > 0
}

// searchableStrings returns the string material a metadata value contributes to
// a text search: the value itself when it is a string, every string element when
// it is an array, and nothing at all otherwise. See the file doc for why numbers
// and nested objects contribute nothing.
func searchableStrings(value any) []string {
	switch v := value.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, elem := range v {
			if s, ok := elem.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ScoreText reports how well query names candidate, on a [0,1] scale where 1 is
// an exact match once both sides are normalised. It is the canonical scorer: a
// provider that ranks in its own storage should reproduce this, and a host that
// wants to pre-sort its own results can call it directly.
func ScoreText(candidate, query string) float64 {
	return scoreNormalized(NormalizeText(candidate), NormalizeText(query))
}

// scoreNormalized is ScoreText over already-normalised strings, so a scan over
// many candidates normalises the query once rather than once per candidate.
func scoreNormalized(candidate, query string) float64 {
	if candidate == "" || query == "" {
		return 0
	}
	if candidate == query {
		return 1
	}
	ct := strings.Fields(candidate)
	qt := strings.Fields(query)
	if len(ct) == 0 || len(qt) == 0 {
		return 0
	}

	// Every query token scores its best match against any candidate token, and
	// the base score is their mean: a query is only as good as its WEAKEST term,
	// so "nike shoes" against "Nike" is a partial answer, not a perfect one.
	total := 0.0
	for _, q := range qt {
		best := 0.0
		for _, c := range ct {
			if s := tokenScore(c, q); s > best {
				best = s
				if best == 1 {
					break
				}
			}
		}
		total += best
	}
	base := total / float64(len(qt))
	if base == 0 {
		return 0
	}

	// Coverage: the share of the CANDIDATE the query accounts for. Without it
	// every name containing "nike" scores identically and a chat surface cannot
	// tell the company from one of its product lines.
	coverage := float64(len(qt)) / float64(len(ct))
	if coverage > 1 {
		coverage = 1
	}
	return base * (coverageFloor + (1-coverageFloor)*coverage)
}

// tokenScore scores one query token against one candidate token, in descending
// tiers: exact, prefix, substring, then approximate. Each tier is bounded below
// the one above it, so a correctly spelled partial always outranks a misspelled
// whole.
func tokenScore(candidate, query string) float64 {
	if candidate == query {
		return 1
	}
	// Ratio of the shared material to the candidate token: "nik" explains more of
	// "nike" than of "nikefootwear", and the tiers scale by it so a prefix of a
	// much longer word does not masquerade as a near-exact hit.
	ratio := float64(len(query)) / float64(len(candidate))
	if ratio > 1 {
		ratio = 1
	}
	switch {
	case strings.HasPrefix(candidate, query):
		return 0.8 + 0.2*ratio
	case strings.Contains(candidate, query):
		return 0.6 + 0.2*ratio
	}

	cr, qr := []rune(candidate), []rune(query)
	if len(cr) < minFuzzyLen || len(qr) < minFuzzyLen {
		return 0
	}
	if len(cr) > maxFuzzyLen || len(qr) > maxFuzzyLen {
		return 0
	}
	longest := len(cr)
	if len(qr) > longest {
		longest = len(qr)
	}
	similarity := 1 - float64(editDistance(cr, qr))/float64(longest)
	if similarity < fuzzyFloor {
		return 0
	}
	return similarity * fuzzyWeight
}

// NormalizeText folds a string to the form both sides of a comparison are scored
// in: lower case, with every non-alphanumeric rune becoming a separator and runs
// of separators collapsing to one space.
//
// It is what makes "Nike, Inc." and "nike inc" the same string, and it is
// exported so a host indexing its own data can store the same form Aperture will
// compare against. Case folding is Unicode-aware; the separator rule is
// deliberately blunt, because punctuation inside a brand name ("AT&T", "Ben &
// Jerry's") carries no information a search should insist on.
func NormalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true // leading separators are dropped
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// editDistance is the Damerau-Levenshtein distance (optimal string alignment)
// between two rune slices: insertions, deletions, substitutions, and the
// TRANSPOSITION of two adjacent runes, each costing one.
//
// The transposition is the reason this is not plain Levenshtein. "nkie" for
// "nike" is the single commonest way a name is mistyped, and to Levenshtein it
// is two edits — far enough from a four-letter word to fall under any threshold
// that also excludes unrelated words. Counting it as the one slip it actually is
// is what lets the fuzzy floor stay strict.
//
// It runs in O(n*m) time and O(m) space, over inputs bounded by maxFuzzyLen.
func editDistance(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	// prev2/prev/curr are the three rows optimal string alignment needs: the
	// transposition case reaches back two rows and two columns.
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			best := min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				if t := prev2[j-2] + 1; t < best {
					best = t
				}
			}
			curr[j] = best
		}
		prev2, prev, curr = prev, curr, prev2
	}
	return prev[len(b)]
}
