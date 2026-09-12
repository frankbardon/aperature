package provider

import (
	"math"
	"strings"
	"testing"
)

// The scorer is a ranking function, so almost nothing about it is worth pinning
// to an exact number — a tuning change that improves ranking should not be a red
// build. What IS worth pinning is the ORDER it produces and the boundaries it
// refuses to cross, because those are the properties a caller reasons about: a
// correctly spelled name beats a misspelled one, a whole name beats a fragment,
// and an unrelated word scores nothing at all.

func TestNormalizeTextFoldsCasePunctuationAndSpacing(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Nike", "nike"},
		{"Nike, Inc.", "nike inc"},
		{"  Ben & Jerry's  ", "ben jerry s"},
		{"AT&T", "at t"},
		{"under_armour", "under armour"},
		{"Adidas\tOriginals\n", "adidas originals"},
		{"---", ""},
		{"", ""},
		{"CAFÉ", "café"},
	} {
		if got := NormalizeText(tc.in); got != tc.want {
			t.Errorf("NormalizeText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAnExactNameScoresOne(t *testing.T) {
	for _, tc := range [][2]string{
		{"Nike", "nike"},
		{"Nike", "NIKE"},
		{"Nike, Inc.", "nike inc"},
		{"AT&T", "at t"},
	} {
		if got := ScoreText(tc[0], tc[1]); got != 1 {
			t.Errorf("ScoreText(%q, %q) = %v, want exactly 1", tc[0], tc[1], got)
		}
	}
}

func TestAnUnrelatedWordScoresNothing(t *testing.T) {
	// Zero, not merely low: a search that returns every entitled object ranked is
	// the brand-enumeration result this surface exists to avoid handing out.
	for _, candidate := range []string{"Adidas", "Reebok", "Puma", "Sneakers", "Footwear Category"} {
		if got := ScoreText(candidate, "nike"); got != 0 {
			t.Errorf("ScoreText(%q, %q) = %v, want 0", candidate, "nike", got)
		}
	}
}

func TestATypoStillResolves(t *testing.T) {
	// The transposition is the whole reason editDistance is Damerau rather than
	// plain Levenshtein: to Levenshtein "nkie" is two edits from "nike", which no
	// threshold strict enough to exclude "puma" would ever admit.
	for _, query := range []string{"nkie", "nikke", "nik"} {
		got := ScoreText("Nike", query)
		if got < DefaultMinScore {
			t.Errorf("ScoreText(%q, %q) = %v, want >= DefaultMinScore (%v)",
				"Nike", query, got, DefaultMinScore)
		}
	}
	if got := ScoreText("Footwear", "footware"); got < DefaultMinScore {
		t.Errorf("ScoreText(Footwear, footware) = %v, want >= %v", got, DefaultMinScore)
	}
}

func TestTheDefaultFloorSitsUnderTheWeakestFuzzyMatch(t *testing.T) {
	// DefaultMinScore is documented as sitting just under the weakest score the
	// fuzzy tier can produce for a candidate the query fully explains. If a
	// tuning change inverted that, every approximate match would be filtered out
	// by the default floor and the typo tolerance above would be dead code.
	weakest := fuzzyFloor * fuzzyWeight // coverage 1 leaves the base untouched
	if DefaultMinScore >= weakest {
		t.Fatalf("DefaultMinScore (%v) must sit below the weakest admissible fuzzy score (%v)",
			DefaultMinScore, weakest)
	}
}

func TestAShortTokenMustBeSpelledRight(t *testing.T) {
	// One edit in three characters is a third of the word. Admitting it would let
	// "IBM" answer for "IBN", and a three-letter ticker or code is exactly where
	// a wrong answer is most plausible to a reader.
	if got := ScoreText("IBM", "ibn"); got != 0 {
		t.Errorf("ScoreText(IBM, ibn) = %v, want 0 — short tokens do not match approximately", got)
	}
	// A prefix of a short token is still fine: it is not a guess.
	if got := ScoreText("IBM", "ib"); got == 0 {
		t.Error("ScoreText(IBM, ib) = 0, want a prefix match")
	}
}

func TestSpellingRightBeatsSpellingClose(t *testing.T) {
	exact := ScoreText("Nike", "nike")
	prefix := ScoreText("Nike", "nik")
	fuzzy := ScoreText("Nike", "nkie")
	if !(exact > prefix && prefix > fuzzy) {
		t.Errorf("tiers out of order: exact=%v prefix=%v fuzzy=%v", exact, prefix, fuzzy)
	}
}

func TestTheWholeNameOutranksTheProductLine(t *testing.T) {
	// Coverage is what makes this hold. Without it every name containing "nike"
	// scores the same and a chat surface cannot tell the company from one of its
	// collections — which is the disambiguation the caller is asking for.
	company := ScoreText("Nike Inc", "nike")
	line := ScoreText("Nike Air Max Collection 2024", "nike")
	if company <= line {
		t.Errorf("Nike Inc (%v) should outrank Nike Air Max Collection 2024 (%v) for %q",
			company, line, "nike")
	}
}

func TestAQueryIsOnlyAsGoodAsItsWeakestTerm(t *testing.T) {
	both := ScoreText("Nike Footwear", "nike footwear")
	half := ScoreText("Nike Footwear", "nike unrelatedterm")
	if half >= both {
		t.Errorf("a query with an unmatched term (%v) must score below one where every term lands (%v)", half, both)
	}
	if half == 0 {
		t.Error("a partially matching query should still score above zero")
	}
}

func TestEveryScoreStaysWithinTheUnitInterval(t *testing.T) {
	candidates := []string{"Nike", "Nike, Inc.", "Adidas Originals", "", "a", strings.Repeat("x", 200)}
	queries := []string{"nike", "n", "", "adidas originals footwear", strings.Repeat("x", 200)}
	for _, c := range candidates {
		for _, q := range queries {
			got := ScoreText(c, q)
			if got < 0 || got > 1 || math.IsNaN(got) {
				t.Errorf("ScoreText(%q, %q) = %v, outside [0,1]", c, q, got)
			}
		}
	}
}

func TestAnEmptySideScoresNothing(t *testing.T) {
	if got := ScoreText("Nike", ""); got != 0 {
		t.Errorf("an empty query scored %v, want 0", got)
	}
	if got := ScoreText("", "nike"); got != 0 {
		t.Errorf("an empty candidate scored %v, want 0", got)
	}
	if got := ScoreText("---", "nike"); got != 0 {
		t.Errorf("a candidate that normalises to nothing scored %v, want 0", got)
	}
}

func TestEditDistanceCountsATranspositionAsOneSlip(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"nike", "nike", 0},
		{"nike", "nkie", 1}, // transposition
		{"nike", "nikke", 1},
		{"nike", "nik", 1},
		{"nike", "mike", 1},
		{"nike", "", 4},
		{"", "nike", 4},
		{"abc", "cba", 2},
	} {
		if got := editDistance([]rune(tc.a), []rune(tc.b)); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- MatchText over the metadata value model --------------------------------

func TestMatchTextFindsTheBestFieldAndNamesIt(t *testing.T) {
	md := Metadata{
		"label":    "Nike, Inc.",
		"aliases":  []any{"Nike", "NIKE Inc"},
		"sector":   "Footwear",
		"seats":    int64(42),
		"launched": "2001-04-12",
	}
	m, ok := MatchText(md, "nike", nil)
	if !ok {
		t.Fatal("MatchText found nothing for 'nike'")
	}
	if m.Field != "aliases" && m.Field != "label" {
		t.Errorf("matched field = %q, want one of the name-bearing fields", m.Field)
	}
	if m.Score == 0 {
		t.Error("a hit must carry a score above zero")
	}
	if m.Value == "" {
		t.Error("a hit must report the string it scored")
	}
}

func TestMatchTextSearchesArrayElementsIndividually(t *testing.T) {
	md := Metadata{"aliases": []any{"Adidas", "Nike", "Puma"}}
	m, ok := MatchText(md, "nike", nil)
	if !ok {
		t.Fatal("MatchText found nothing in an array field")
	}
	if m.Value != "Nike" {
		t.Errorf("scored value = %q, want the matching ELEMENT %q", m.Value, "Nike")
	}
	if m.Score != 1 {
		t.Errorf("an element equal to the query scored %v, want 1 — the array is not scored whole", m.Score)
	}
}

func TestMatchTextNeverScoresANonStringValue(t *testing.T) {
	// Filtering by a number, a bool or a date is what Fields is for. A text query
	// that matched 42 would make a ranked shortlist depend on the spelling of a
	// value nobody typed.
	md := Metadata{
		"seats":  int64(42),
		"active": true,
		"empty":  nil,
		"nested": map[string]any{"label": "Nike"},
	}
	if m, ok := MatchText(md, "42", nil); ok {
		t.Errorf("a number was text-matched: %+v", m)
	}
	if m, ok := MatchText(md, "true", nil); ok {
		t.Errorf("a bool was text-matched: %+v", m)
	}
	if m, ok := MatchText(md, "nike", nil); ok {
		t.Errorf("a nested object was descended into: %+v", m)
	}
}

func TestMatchTextRestrictsToNamedFields(t *testing.T) {
	md := Metadata{"label": "Adidas", "notes": "Nike is the competitor"}
	if m, ok := MatchText(md, "nike", []string{"label"}); ok {
		t.Errorf("the restriction was ignored: matched %+v", m)
	}
	if _, ok := MatchText(md, "nike", []string{"notes"}); !ok {
		t.Error("the named field should have matched")
	}
	// A named field the object does not carry is not an error and not a match —
	// the same restrictive direction MatchFields takes for an absent field.
	if _, ok := MatchText(md, "nike", []string{"nonexistent"}); ok {
		t.Error("a field the object does not carry must not match")
	}
}

func TestMatchTextIsStableAcrossRuns(t *testing.T) {
	// Map iteration order is random, so a tie between two equally good fields has
	// to be broken deterministically or the same search reports a different Field
	// on a second call.
	md := Metadata{"zulu": "Nike", "alpha": "Nike", "mike": "Nike"}
	first, ok := MatchText(md, "nike", nil)
	if !ok {
		t.Fatal("MatchText found nothing")
	}
	for i := 0; i < 50; i++ {
		got, _ := MatchText(md, "nike", nil)
		if got != first {
			t.Fatalf("unstable result: %+v then %+v", first, got)
		}
	}
	if first.Field != "alpha" {
		t.Errorf("tie broken to %q, want the lexically first field %q", first.Field, "alpha")
	}
}

func TestMatchTextOnAnEmptyBagFindsNothing(t *testing.T) {
	if _, ok := MatchText(Metadata{}, "nike", nil); ok {
		t.Error("an empty metadata bag matched")
	}
	if _, ok := MatchText(nil, "nike", nil); ok {
		t.Error("a nil metadata bag matched")
	}
	if _, ok := MatchText(Metadata{"label": "Nike"}, "", nil); ok {
		t.Error("an empty query matched")
	}
}

func BenchmarkScoreText(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = ScoreText("Nike Air Max Collection 2024", "nkie")
	}
}

func BenchmarkMatchText(b *testing.B) {
	md := Metadata{
		"label":   "Nike, Inc.",
		"aliases": []any{"Nike", "NIKE Inc", "Nike Incorporated"},
		"sector":  "Footwear",
		"region":  "Global",
		"seats":   int64(42),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = MatchText(md, "nike", nil)
	}
}
