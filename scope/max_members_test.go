package scope

import (
	"context"
	"fmt"
	"testing"

	"github.com/frankbardon/aperture/identity"
)

// recordingLister is an ObjectLister over a synthetic document catalogue that
// remembers every limit it was asked for.
//
// The recorded limit is the load-bearing half of these tests. A gather that
// asked the lister for DefaultMaxMembers and then returned 1500 members is
// impossible, but a gather that asked for 1500 and truncated to 1000 afterwards
// looks identical from the outside to one that never raised the bound at all —
// only what the lister SAW distinguishes "the ceiling propagated" from "the
// ceiling was applied twice, the lower one winning".
type recordingLister struct {
	population int
	limits     *[]int
}

func (l recordingLister) List(_ context.Context, _ string, pattern identity.Pattern, limit int) ([]identity.Identity, error) {
	*l.limits = append(*l.limits, limit)
	out := make([]identity.Identity, 0, l.population)
	for i := 0; i < l.population; i++ {
		id := identity.MustParse(fmt.Sprintf("account:acme/project:atlas/document:%d", i))
		if !pattern.Matches(id) {
			continue
		}
		out = append(out, id)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// allRules selects every object, so a rule-backed gather is bounded by the
// ceiling and the listing alone.
type allRules struct{}

func (allRules) Selected(context.Context, string, identity.Identity, string, string, string, string) (bool, error) {
	return true, nil
}

// TestMembersGatherAgainstTheConfiguredCeiling is the story's proof in both
// directions: a wiring that configures a ceiling gathers against THAT number,
// and a wiring that configures none still gathers against DefaultMaxMembers —
// the zero value of Deps stays usable exactly as its doc comment promises.
//
// Every strategy whose Members path enumerates is covered, because the ceiling
// reaches the lister through enumerateOfType and a strategy that bypassed it
// would starve silently.
func TestMembersGatherAgainstTheConfiguredCeiling(t *testing.T) {
	const population = 2 * DefaultMaxMembers

	strategies := []struct {
		name string
		gc   GrantContext
		deps Deps // Lister is filled in per case; Rules is not.
	}{{
		name: "implicit",
		gc:   gc(StrategyImplicit, "account:acme/**", "document", nil, ""),
	}, {
		// A minus-list naming an object the catalogue never produces, so the
		// exclusion removes nothing and the ceiling is the only thing bounding it.
		name: "exclusive",
		gc:   gc(StrategyExclusive, "account:acme/**", "document", []string{"account:acme/project:atlas/document:-1"}, ""),
	}, {
		name: "inclusive rule-backed",
		gc:   gc(StrategyInclusive, "account:acme/**", "document", nil, "everything"),
		deps: Deps{Rules: allRules{}},
	}}

	ceilings := []struct {
		name       string
		maxMembers int
		want       int
	}{{
		name:       "a zero Deps gathers up to the default",
		maxMembers: 0,
		want:       DefaultMaxMembers,
	}, {
		name:       "a negative ceiling is not a ceiling of nothing",
		maxMembers: -1,
		want:       DefaultMaxMembers,
	}, {
		name:       "a raised ceiling is honoured rather than clamped back to the default",
		maxMembers: 1500,
		want:       1500,
	}, {
		name:       "a lowered ceiling is honoured too",
		maxMembers: 7,
		want:       7,
	}}

	for _, st := range strategies {
		for _, c := range ceilings {
			t.Run(st.name+"/"+c.name, func(t *testing.T) {
				var limits []int
				deps := st.deps
				deps.Lister = recordingLister{population: population, limits: &limits}
				deps.MaxMembers = c.maxMembers

				res := mustResolve(t, DefaultRegistry(), st.gc, deps)
				got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
				if err != nil {
					t.Fatalf("Members: %v", err)
				}
				if len(got) != c.want {
					t.Errorf("gathered %d members from %d candidates, want %d",
						len(got), population, c.want)
				}
				if len(limits) != 1 {
					t.Fatalf("lister was asked %d times, want exactly 1", len(limits))
				}
				// The lister was asked for the configured ceiling, not the
				// constant: the value rides the limit List already carries.
				if limits[0] != c.want {
					t.Errorf("lister was asked for limit %d, want %d", limits[0], c.want)
				}
			})
		}
	}
}

// TestTheInclusiveIDListHalfStopsAtTheConfiguredCeiling covers the one Members
// path that never touches a lister. It gathers straight out of the grant's
// id-list, so if it kept reading the package constant a lowered ceiling would be
// ignored on exactly the strategy an operator is most likely to cap.
func TestTheInclusiveIDListHalfStopsAtTheConfiguredCeiling(t *testing.T) {
	ids := []string{
		"account:acme/project:atlas/document:1",
		"account:acme/project:atlas/document:2",
		"account:acme/project:atlas/document:3",
		"account:acme/project:atlas/document:4",
		"account:acme/project:atlas/document:5",
	}
	for _, tc := range []struct {
		name       string
		maxMembers int
		want       int
	}{
		{"a zero Deps leaves a short list untouched", 0, len(ids)},
		{"a ceiling below the list truncates it", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := mustResolve(t, DefaultRegistry(),
				gc(StrategyInclusive, "account:acme/**", "document", ids, ""),
				Deps{MaxMembers: tc.maxMembers})
			got, err := res.Members(context.Background(), identity.MustParsePattern("**"))
			if err != nil {
				t.Fatalf("Members: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("gathered %d of %d listed ids, want %d", len(got), len(ids), tc.want)
			}
		})
	}
}

// TestBoundLimitNormalisesTheCeiling pins the normaliser itself: zero and
// negative mean DefaultMaxMembers, and a positive ceiling is returned as given.
// The last row is the regression this story exists for — boundLimit used to cap
// its answer at DefaultMaxMembers, so a raised bound could not survive the trip.
func TestBoundLimitNormalisesTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{0, DefaultMaxMembers},
		{-1, DefaultMaxMembers},
		{1, 1},
		{DefaultMaxMembers, DefaultMaxMembers},
		{DefaultMaxMembers + 500, DefaultMaxMembers + 500},
	} {
		if got := boundLimit(tc.in); got != tc.want {
			t.Errorf("boundLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
