package bench

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/scope"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// Rule-backed Enumerate: what a bounded enumeration through a rule actually
// costs.
//
// Everything else in bench/ measures a single Check, where a rule-backed grant
// is evaluated exactly once. Enumerate is a different shape entirely. An
// inclusive grant whose membership is decided by a rule is not invertible, so
// the resolver LISTS the object type and filters each candidate through the
// rule — and then the engine runs its ordinary deny-overrides decision over
// every surviving candidate, which consults the same grant again. So one
// Enumerate is up to 2 x candidates rule evaluations:
//
//	scope.inclusiveResolver.Members -> enumerateOfType(keep: Contains) -> 1 eval per LISTED candidate
//	engine.enumerateWithSubjects    -> evaluate -> scopeCoverer.cover -> 1 eval per SELECTED candidate
//
// Both halves are bounded by the SAME single number — the engine's configured
// enumeration bound, which it also stamps into the scope member gather, so the
// member set and the result cap can never disagree. Unconfigured that number is
// engine.DefaultEnumerateLimit (1000, matching scope.DefaultMaxMembers);
// engine.WithEnumerateLimit replaces it. Either way the worst case is fixed by
// the bound rather than proportional to the object population.
// TestEnumerateRuleBackedStaysBounded below pins that at both the default and a
// raised bound; no second limit exists and none is introduced here.
//
// That is also why the bound is a MEASURED axis of this sweep and not just an
// asserted one. It is the operator's only knob on the worst case, and raising it
// raises the fan-out proportionally (2 x candidates rule evaluations), so a host
// about to raise it in production should be able to read the cost off a
// benchmark rather than estimate it.
//
// These benchmarks are INFORMATIONAL: they publish the absolute per-candidate
// cost, so a future change that regresses it has something to regress against.
// They are not held to the cached-Check thresholds (p99 < 1 ms, >= 10k
// checks/sec) — a bound-sized rule-backed enumeration is milliseconds by design.
//
// What IS asserted, and gated, is the SHAPE: TestCheckNFREnumerateBound at the
// bottom of this file measures an enumeration at a raised bound against the same
// enumeration at the default bound and requires the per-candidate cost to stay
// flat, which is the invariant these rows demonstrate and the one a raised bound
// can break. The threshold is a ratio derived from the table above, and the
// reasoning is written where it is set.
//
// MEASURED (Apple M1 Max, 10 cores, go1.26.5 darwin/arm64,
// `go test -bench BenchmarkEnumerateRuleBacked -benchmem -benchtime=2s -count=3`,
// medians of 3; audit off, one scalar-comparison rule over one metadata field,
// every candidate selected). "bound" is the value configured through
// engine.WithEnumerateLimit; "—" is an engine configured with none, running on
// engine.DefaultEnumerateLimit:
//
//	candidates | bound |    ns/op   | ids | ns/candidate | allocs/op |   B/op
//	-----------+-------+------------+-----+--------------+-----------+----------
//	        10 |   —   |     54 724 |  10 |        5 472 |       596 |   45 060
//	       100 |   —   |    567 891 | 100 |        5 679 |     5 679 |  442 772
//	     1 000 |   —   |  6 317 880 |1 000|        6 318 |    56 133 |4 444 912
//	     2 000 |   —   |  5 929 466 |1 000|        5 929 |    56 136 |4 478 722
//	     1 000 | 2 000 |  5 743 803 |1 000|        5 744 |    56 127 |4 444 822
//	     2 000 | 2 000 | 11 638 062 |2 000|        5 819 |   112 176 |8 964 388
//	     4 000 | 2 000 | 12 005 617 |2 000|        6 003 |   112 162 |9 025 695
//
// And the rule half in isolation (BenchmarkEnumerateRuleBackedRuleEval, 1 000
// rules.Engine.Selected calls with no engine around them), same run:
//
//	2 348 ns/eval, 17 allocs/eval, ~1 297 B/eval
//
// Those absolutes are one machine on one day, and that day was loaded: the same
// RuleEval benchmark re-run on the same machine while quiet measured 1 500
// ns/eval, against 2 348 here and 1 235 when these benchmarks first landed. Read
// the table's INTERNAL ratios, which is what it is for.
//
// The allocation counters do not move with load, and those did change: 14 -> 17
// allocs/eval, ~976 -> ~1 296 B/eval. All of it is the principal and account
// FLOOR BAGS, added in attribute-providers (232e123, eba23be) and built once per
// evaluation in rules/engine.go. The per-evaluation copy is the security
// property — a resolver bag may be cached and shared and is read-only, so the
// floor is stamped into a copy, never into it. Three allocations is the price.
// There is no unexplained drift here to chase.
//
// Read five things off it:
//
//  1. **~5.7–6.3 µs and ~56 allocations per RETURNED id**, flat across every row
//     at every bound. The cost is LINEAR in the result size, with no super-linear
//     term hiding in the resolver at the raised bound either.
//  2. **Doubling the bound doubles the cost, and no more than doubles it.**
//     1 000 ids -> 2 000 ids is 6.32 ms -> 11.64 ms (1.84x), 56 133 -> 112 176
//     allocations (2.00x), 4.44 MB -> 8.96 MB (2.02x). Budget a raise as
//     proportional: roughly **4.5 KB and 56 allocations of transient garbage per
//     id the bound allows**, so a bound of 10 000 is a ~45 MB, ~58 ms
//     enumeration.
//  3. **Raising the bound is free until the population reaches it.** The
//     1 000-candidate row at bound 2 000 costs what the same row costs
//     unconfigured — 5.74 ms vs 6.32 ms, 56 127 vs 56 133 allocations, the same
//     B/op to four digits. Configuring headroom a deployment does not use is not
//     paid for.
//  4. **The raised bound clamps exactly as the default one does.** 4 000
//     candidates at bound 2 000 costs what 2 000 candidates at bound 2 000 costs
//     (12.01 vs 11.64 ms, 112 162 vs 112 176 allocations); 2 000 candidates
//     unconfigured costs what 1 000 do. Past the bound the extra objects are
//     never visited, so the worst case stays a constant a host can budget for —
//     it is just a constant the operator now chooses.
//  5. **The rule is ~55–60% of it.** Two evaluations per candidate at 2 348 ns is
//     ~4.7 µs of the ~5.8–6.3 µs, and 34 of the ~56 allocations. The rest is the
//     decision engine's own per-candidate work. So halving the rule cost is worth
//     about a third of the total, and the per-decision AST re-walk
//     (BenchmarkRuleCompileCached) is the largest single line item inside the
//     rule half.
//
// One asymmetry the table does not show, worth knowing before reading a caller's
// EnumerateRequest.Limit as a cost knob: a smaller Limit shortens only the
// SECOND half. The engine bounds its result loop by the caller's limit, but the
// member set is gathered first and is bounded by the engine's CONFIGURED bound
// regardless — so Limit=10 against a bound of 1 000 still pays ~1 000 rule
// evaluations to build the member set, then ~10 decisions. That is a property of
// where the bound sits, not something these benchmarks change: the knob that
// moves the first half is the configured bound, not the request's Limit.

// The rule-backed enumeration fixture's identifiers. It is a SEPARATE store,
// registry and rules engine from buildModel's: the existing benchmarks are the
// committed baseline for the cached Check, and adding thousands of objects and
// grants to their model would move numbers this story is not about.
const (
	enumAccount    = "acctenum"
	enumRole       = "roleenum"
	enumUser       = "userenum"
	enumAction     = "enumread"
	enumPermission = "perm-enumread"
	enumRule       = "rule-enum-public"
	enumProject    = "projenum"
)

// enumerateRaisedBound is the configured ceiling the raised-bound rows run at:
// twice the default, so those rows measure a result set the default rows
// literally cannot produce. It is a value handed to engine.WithEnumerateLimit,
// never a constant the fixture reads back — the point of the sweep is to
// measure the real configured path, so the bound must travel engine clamp ->
// scope member gather -> provider list exactly as a deployment's would.
const enumerateRaisedBound = 2 * scope.DefaultMaxMembers

// enumerateCase is one row of the sweep: a candidate population and the bound
// the engine is configured with, if any.
type enumerateCase struct {
	// candidates is how many objects the provider holds.
	candidates int
	// bound is the ceiling to configure through engine.WithEnumerateLimit.
	// ZERO means configure none, so the engine's own default applies — those are
	// the committed baseline rows, and they are kept so a single `make bench` run
	// carries both halves of the before/after comparison.
	bound int
}

// name keeps the unconfigured rows spelled exactly as they always were
// (candidates-N), so a benchstat against the committed baseline still lines the
// default rows up; a configured row carries its bound in the name.
func (c enumerateCase) name() string {
	if c.bound <= 0 {
		return fmt.Sprintf("candidates-%d", c.candidates)
	}
	return fmt.Sprintf("candidates-%d/bound-%d", c.candidates, c.bound)
}

// enumerateCases is the sweep. It has two halves.
//
// The unconfigured half brackets the DEFAULT bound rather than stopping at it:
// scope.DefaultMaxMembers is where the member set clamps, and the 2x row shows
// that going past it costs nothing more.
//
// The configured half raises the bound through engine.WithEnumerateLimit and
// brackets THAT: below it (the bound is never reached, so raising it is free),
// at it, and past it (the raised bound clamps, exactly as the default one did).
// Without those rows the raised bound would be a number nothing ever ran at —
// the fails-by-passing shape, where the feature is asserted but never exercised.
func enumerateCases() []enumerateCase {
	return []enumerateCase{
		{candidates: 10},
		{candidates: 100},
		{candidates: scope.DefaultMaxMembers},
		{candidates: 2 * scope.DefaultMaxMembers},
		{candidates: scope.DefaultMaxMembers, bound: enumerateRaisedBound},
		{candidates: enumerateRaisedBound, bound: enumerateRaisedBound},
		{candidates: 2 * enumerateRaisedBound, bound: enumerateRaisedBound},
	}
}

// enumerateModel is the self-contained rule-backed enumeration fixture.
type enumerateModel struct {
	svc *service.Service
	// query enumerates every document of the fixture's project.
	query service.EnumerateQuery
	// candidates is how many objects the provider holds.
	candidates int
	// bound is the ceiling the engine was configured with, or zero when it was
	// configured with none.
	bound int
	// effective is the ceiling this enumeration actually runs under: the
	// configured bound, or the engine's default when none was configured.
	effective int
	// want is how many ids Enumerate must return: every candidate is selected by
	// the rule, so it is the candidate count clamped by the effective bound.
	want int
}

// buildEnumerateModel seeds a store with one rule-backed inclusive grant over
// `candidates` documents, all of which the rule selects, and returns the facade
// plus the enumeration query.
//
// A POSITIVE bound is configured on the engine through
// engine.WithEnumerateLimit; a non-positive one configures nothing at all, so
// the engine runs on its own default. The two are not the same wiring even when
// the numbers coincide, which is why the unconfigured rows pass zero rather than
// passing engine.DefaultEnumerateLimit: the baseline must keep measuring the
// path a deployment that never set the flag takes.
//
// Every candidate is selected on purpose. A rule that filtered some of them out
// would measure a mixture of the selected and rejected paths and make the
// per-candidate number depend on the selectivity rather than on the machinery;
// selecting all of them is also the worst case, because a rejected candidate
// skips the second (per-decision) evaluation.
func buildEnumerateModel(tb testing.TB, candidates, bound int) enumerateModel {
	tb.Helper()
	ctx := context.Background()
	store := memory.New()

	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatalf("seed: %v", err)
		}
	}

	must(store.PutObjectType(ctx, model.ObjectType{
		Name: "document", Actions: []string{enumAction},
	}))
	must(store.PutPermission(ctx, model.Permission{
		ID: enumPermission, ObjectType: "document", Action: enumAction,
		ScopeStrategy: "inclusive;rule=" + enumRule,
	}))
	must(store.PutAccount(ctx, model.Account{ID: enumAccount, Name: enumAccount}))
	must(store.PutRole(ctx, model.Role{ID: enumRole, Name: enumRole}))
	must(store.PutPrincipal(ctx, model.Principal{
		ID: enumUser, Kind: model.PrincipalUser, Identity: "user:" + enumUser,
		RoleIDs: []string{enumRole},
	}))
	// ONE grant, account-wide, whose membership is decided entirely by the rule —
	// so the whole measured cost is the rule-backed resolution path and not a
	// large grant set being walked.
	must(store.PutGrant(ctx, model.Grant{
		ID: "g-" + enumRule, AccountID: enumAccount,
		Subject:      model.Subject{Kind: model.SubjectRole, ID: enumRole},
		PermissionID: enumPermission,
		Object:       "account:" + enumAccount + "/**",
		Effect:       model.EffectAllow,
	}))

	objects := make([]provider.Object, 0, candidates)
	for i := 0; i < candidates; i++ {
		id, err := identity.Parse(enumObjectID(i))
		if err != nil {
			tb.Fatalf("parse candidate %d: %v", i, err)
		}
		objects = append(objects, provider.Object{
			ID: id, Metadata: provider.Metadata{"classification": "public"},
		})
	}
	static, err := provider.NewStatic(objects)
	if err != nil {
		tb.Fatalf("build static provider: %v", err)
	}
	// TTL 0 (never expire) for the same reason buildRuleLayer disables it: a long
	// benchmark run must not silently start re-fetching partway through and
	// measure a mix of the cached and uncached paths.
	reg := provider.NewRegistry()
	reg.MustRegister("document", static, provider.WithTTL(0))

	ruleEng := rules.NewEngine(rules.MapSource{
		enumRule: {
			Name: enumRule,
			AST:  rules.Compare(rules.OpEq, rules.Var("object.classification"), rules.Lit("public")),
		},
	}, reg)

	opts := []engine.Option{
		engine.WithScopeResolution(
			scope.DefaultRegistry(),
			engine.ScopeDeps{Lister: reg, Rules: ruleEng},
		),
		// An enumeration that lands exactly on its bound WARNs through the
		// engine's logger, and every row of this sweep is designed to land exactly
		// on its bound. With no logger wired that warning goes to slog.Default(),
		// which means b.N stderr writes per case: unreadable output, and a
		// formatting cost charged to the enumeration being measured. Discarding it
		// measures the enumeration rather than the log handler. The warning itself
		// is asserted where it belongs, in engine/enumerate_bound_warning_test.go.
		engine.WithLogger(slog.New(slog.DiscardHandler)),
	}
	effective := engine.DefaultEnumerateLimit
	if bound > 0 {
		// The configured path, exercised as a deployment configures it: the option,
		// never a constant read back out. This one value is what the engine clamps
		// the result to AND what it stamps into the scope member gather.
		opts = append(opts, engine.WithEnumerateLimit(bound))
		effective = bound
	}
	eng := engine.New(store, opts...)

	want := candidates
	if want > effective {
		want = effective
	}
	return enumerateModel{
		svc: service.New(eng),
		query: service.EnumerateQuery{
			Account:   enumAccount,
			Principal: enumUser,
			Action:    enumAction,
			Pattern:   "account:" + enumAccount + "/project:" + enumProject + "/document:*",
		},
		candidates: candidates,
		bound:      bound,
		effective:  effective,
		want:       want,
	}
}

// enumObjectID renders the i-th candidate's canonical identity. Fixed-width so
// the canonical sort order Enumerate returns matches the declaration order.
func enumObjectID(i int) string {
	return fmt.Sprintf("account:%s/project:%s/document:doc%05d", enumAccount, enumProject, i)
}

// warmEnumerate runs the query once, asserting it returns exactly the expected
// number of ids, so a benchmark never silently measures an empty enumeration.
// It also primes the parsed-pattern cache, the compiled-rule cache and the
// provider metadata cache, so the measured loop is the steady state.
func warmEnumerate(tb testing.TB, m enumerateModel) {
	tb.Helper()
	ids, err := m.svc.Enumerate(context.Background(), m.query)
	if err != nil {
		tb.Fatalf("warm Enumerate: %v", err)
	}
	if len(ids) != m.want {
		tb.Fatalf("warm Enumerate returned %d ids over %d candidates at effective bound %d "+
			"(configured %d), want %d", len(ids), m.candidates, m.effective, m.bound, m.want)
	}
}

// BenchmarkEnumerateRuleBacked sweeps a rule-backed Enumerate across candidate-
// set sizes AND configured bounds, reporting ns/op, allocs/op, and the derived
// per-candidate cost.
//
// The unconfigured rows and the raised-bound rows run in the SAME invocation, so
// "what does raising the bound cost?" is answered by one `make bench` output
// rather than by comparing two runs on two machines.
//
// It reports and asserts nothing about a threshold — see the file comment.
func BenchmarkEnumerateRuleBacked(b *testing.B) {
	ctx := context.Background()
	for _, c := range enumerateCases() {
		b.Run(c.name(), func(b *testing.B) {
			m := buildEnumerateModel(b, c.candidates, c.bound)
			warmEnumerate(b, m)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ids, err := m.svc.Enumerate(ctx, m.query)
				if err != nil {
					b.Fatalf("Enumerate: %v", err)
				}
				if len(ids) != m.want {
					b.Fatalf("Enumerate returned %d ids, want %d", len(ids), m.want)
				}
			}
			b.StopTimer()
			// The number this benchmark exists to publish. Dividing by the
			// RETURNED id count (not the candidate count) keeps the past-the-bound
			// rows comparable with the at-bound rows: past the bound the extra
			// objects are never visited, so charging them would understate the real
			// cost. It is also what makes rows at DIFFERENT bounds comparable —
			// a flat ns/candidate across bounds is what "raising it is linear"
			// means, and a rising one is what a super-linear term would look like.
			if m.want > 0 && b.N > 0 {
				b.ReportMetric(
					float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(m.want),
					"ns/candidate")
			}
			b.ReportMetric(float64(m.want), "ids")
		})
	}
}

// BenchmarkEnumerateRuleBackedRuleEval isolates the rule half of the cost: the
// same number of rules.Engine.Selected calls the enumeration above makes, with
// no engine, no store and no grant walk around them.
//
// Reporting it beside BenchmarkEnumerateRuleBacked is what separates "the rule
// is expensive" from "enumeration is expensive": the difference between the two
// is everything the decision engine adds per candidate.
func BenchmarkEnumerateRuleBackedRuleEval(b *testing.B) {
	ctx := context.Background()
	reg := provider.NewRegistry()
	objects := make([]provider.Object, 0, scope.DefaultMaxMembers)
	ids := make([]identity.Identity, 0, scope.DefaultMaxMembers)
	for i := 0; i < scope.DefaultMaxMembers; i++ {
		id, err := identity.Parse(enumObjectID(i))
		if err != nil {
			b.Fatalf("parse candidate %d: %v", i, err)
		}
		ids = append(ids, id)
		objects = append(objects, provider.Object{
			ID: id, Metadata: provider.Metadata{"classification": "public"},
		})
	}
	static, err := provider.NewStatic(objects)
	if err != nil {
		b.Fatalf("build static provider: %v", err)
	}
	reg.MustRegister("document", static, provider.WithTTL(0))
	eng := rules.NewEngine(rules.MapSource{
		enumRule: {
			Name: enumRule,
			AST:  rules.Compare(rules.OpEq, rules.Var("object.classification"), rules.Lit("public")),
		},
	}, reg)

	for _, id := range ids { // warm the metadata cache and the compiled-rule cache
		if ok, err := eng.Selected(ctx, enumRule, id, enumAccount, string(model.PrincipalUser), enumUser, enumAction); err != nil || !ok {
			b.Fatalf("warm Selected(%s): ok=%v err=%v", id, ok, err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range ids {
			if _, err := eng.Selected(ctx, enumRule, id, enumAccount, string(model.PrincipalUser), enumUser, enumAction); err != nil {
				b.Fatalf("Selected: %v", err)
			}
		}
	}
	b.StopTimer()
	if b.N > 0 {
		b.ReportMetric(
			float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(ids)),
			"ns/eval")
	}
}

// TestEnumerateRuleBackedStaysBounded is the ungated, always-on guard that the
// enumeration is bounded by the limit it was configured with and by nothing
// else.
//
// It seeds twice as many objects as the bound and asserts the result is exactly
// the bound: not more (the resolver would be materialising an unbounded member
// set) and not less (a second, tighter limit would have crept in, and a silently
// truncated Enumerate is a wrong access-control answer, not a performance
// tradeoff). It is structural arithmetic over the fixture rather than a timing
// assertion, so it cannot flake and runs in the default make test.
func TestEnumerateRuleBackedStaysBounded(t *testing.T) {
	// Unconfigured: the original assertion, unchanged. An engine handed no bound
	// gathers and clamps at the default, and the 2x population proves the clamp is
	// real rather than a population that simply ran out.
	t.Run("unconfigured", func(t *testing.T) {
		m := buildEnumerateModel(t, 2*scope.DefaultMaxMembers, 0)
		ids := enumerateIDs(t, m)
		if len(ids) != scope.DefaultMaxMembers {
			t.Fatalf("Enumerate over %d rule-selected candidates returned %d ids, want exactly "+
				"the existing bound %d", m.candidates, len(ids), scope.DefaultMaxMembers)
		}
		if len(ids) > engine.DefaultEnumerateLimit {
			t.Fatalf("Enumerate returned %d ids, exceeding engine.DefaultEnumerateLimit %d",
				len(ids), engine.DefaultEnumerateLimit)
		}
	})

	// Configured above the default, population above the configured bound: the
	// clamp is still a real clamp, but now AT the raised number. The extra
	// assertion is what stops this becoming a tautology — the result has to exceed
	// the default, so a bound that silently fell back to 1000 anywhere between the
	// engine's clamp, the scope member gather and the provider list fails here.
	t.Run("configured-above-the-default", func(t *testing.T) {
		m := buildEnumerateModel(t, 2*enumerateRaisedBound, enumerateRaisedBound)
		ids := enumerateIDs(t, m)
		if len(ids) != enumerateRaisedBound {
			t.Fatalf("Enumerate over %d rule-selected candidates at a configured bound of %d "+
				"returned %d ids, want exactly the configured bound",
				m.candidates, enumerateRaisedBound, len(ids))
		}
		if len(ids) <= engine.DefaultEnumerateLimit {
			t.Fatalf("Enumerate at a configured bound of %d returned %d ids, which does not "+
				"exceed engine.DefaultEnumerateLimit %d — the raised bound never took effect",
				enumerateRaisedBound, len(ids), engine.DefaultEnumerateLimit)
		}
	})

	// Configured above the POPULATION: nothing clamps, and the answer is every
	// candidate — more than the default would ever have returned. This is the row
	// that proves the raised bound widens the result rather than merely raising a
	// ceiling nothing reaches.
	t.Run("configured-above-the-population", func(t *testing.T) {
		population := engine.DefaultEnumerateLimit + 500
		m := buildEnumerateModel(t, population, 2*population)
		ids := enumerateIDs(t, m)
		if len(ids) != population {
			t.Fatalf("Enumerate over %d rule-selected candidates at a configured bound of %d "+
				"returned %d ids, want all %d — no bound should have applied",
				population, 2*population, len(ids), population)
		}
		if len(ids) <= engine.DefaultEnumerateLimit {
			t.Fatalf("Enumerate returned %d ids, which does not exceed "+
				"engine.DefaultEnumerateLimit %d — the raised bound never took effect",
				len(ids), engine.DefaultEnumerateLimit)
		}
	})
}

// enumerateIDs runs the fixture's query once and asserts every returned id is a
// real candidate in canonical order, so a bound assertion can never be satisfied
// by the right NUMBER of the wrong ids.
func enumerateIDs(t *testing.T, m enumerateModel) []string {
	t.Helper()
	ids, err := m.svc.Enumerate(context.Background(), m.query)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	for i, id := range ids {
		if want := enumObjectID(i); id != want {
			t.Fatalf("id[%d] = %q, want %q (Enumerate must return candidates in canonical order)",
				i, id, want)
		}
	}
	return ids
}

// enumerateBoundRatioCeiling is this gate's one threshold, and it is a RATIO
// rather than a wall clock: the per-candidate cost of an enumeration at a
// RAISED bound, divided by the per-candidate cost of the same enumeration at the
// engine's default bound, must not exceed it.
//
// WHY A RATIO AT ALL. The suite's existing thresholds (p99Ceiling = 1 ms,
// throughputMin = 10 000/sec) are about a cached single Check. Neither transfers
// here: a 2 000-id rule-backed enumeration is ~11.6 ms BY DESIGN — it is ~4 000
// rule evaluations plus 2 000 decisions — so a 1 ms ceiling could never pass, and
// any absolute ceiling large enough to pass would have to be pinned to one
// machine on one day. E3-S1 measured exactly that hazard: the
// candidates-4000/bound-2000 row alone ranged 11.21–16.31 ms (1.45x) across a
// single -count=3 run, and RuleEval measured 2 348 ns/eval loaded against 1 500
// quiet on the same machine. An absolute ceiling on an
// 11.6 ms operation measured on a machine that swings 1.45x is the design most
// likely to flake, and re-tuning it would become a recurring chore that
// eventually gets loosened into a tautology.
//
// A ratio divides machine speed and baseline drift out entirely, because both
// arms are measured on the same machine in the same second (see the interleaving
// below). What is left is the thing the raised bound can actually break.
//
// WHY THIS RATIO. ns/candidate flat across bounds is the strongest invariant in
// E3-S1's data — it is what "the cost is linear in the result size" means. From
// that table (medians of 3, Apple M1 Max, go1.26.5 darwin/arm64), with the
// 1 000-candidate unconfigured row at 6 318 ns/candidate as the denominator:
//
//	1 000 candidates @ bound 2 000 (headroom)  5 744 / 6 318 = 0.91
//	2 000 candidates @ bound 2 000 (at bound)  5 819 / 6 318 = 0.92
//	4 000 candidates @ bound 2 000 (past it)   6 003 / 6 318 = 0.95
//
// and the widest spread of ns/candidate between ANY two rows of the sweep, across
// three orders of magnitude of population and both bounds, is 5 472..6 318 =
// 1.15x. So 1.5 sits 1.63x above the worst ratio actually measured and 1.3x above
// the widest spread the sweep has ever produced.
//
// WHAT IT CATCHES. A super-linear term in the enumeration fan-out — the one
// regression raising the bound can introduce that nothing else in the suite would
// see. A quadratic member gather doubles the per-candidate cost when the bound
// doubles (ratio 2.0) and fails here; anything worse fails harder. So does a
// bound-sized pre-allocation, a per-candidate rescan of the member set, or a
// cache keyed such that it stops hitting once the member set grows — all of which
// present as ns/candidate rising with the bound. That was verified rather than
// argued: a linear rescan of the accumulated member set injected into
// scope.enumerateOfType (a textbook quadratic gather) measures 1.86x here and
// fails, while the headroom arm stays at 0.99x through the same injection — so the
// ratio really does track the bound and not the fixture. The headroom arm
// catches the shape specific to THIS effort: configuring a bound a deployment
// never reaches must cost nothing, and a ceiling-sized allocation on the
// configured path would show up there and nowhere else.
//
// WHAT IT DELIBERATELY DOES NOT CATCH: a uniform slowdown that hits both arms
// equally — by construction, that divides out. A machine-wide slowdown is
// precisely that shape, and so is a uniform per-evaluation cost like the floor
// bags above; both are BenchmarkEnumerateRuleBacked questions, not gate
// questions. Asserting them here would make the gate a machine detector. The
// decisions/sec floor below is what keeps a catastrophic uniform regression from
// passing unnoticed, and it reuses the committed throughputMin rather than
// inventing a number.
//
// WHICH RISK THIS FAVOURS: **not flaking on a loaded machine**, explicitly and by
// a wide margin. 1.5 is 63% above the worst ratio in E3-S1's table, and the
// measurement (interleaved arms, minimum over rounds) is built to suppress
// one-sided load noise rather than average it in. Measured: across 17 runs on an
// Apple M1 Max — idle, and under 12 then 16 competing CPU burners at load
// averages up to 24 — both ratios stayed within **0.94x..1.10x**. The absolute
// per-id cost moved ~15% under that load; the ratio did not. For contrast, the
// same two populations read straight off BenchmarkEnumerateRuleBacked during that
// load (no interleaving, no min-of-rounds) differed by 1.72x — which is what the
// measurement design, not the ceiling, is buying. The cost of that choice, stated
// plainly: a regression that makes the raised bound between 1.0x and 1.5x dearer
// PER CANDIDATE than the default bound passes here, and has to be caught by
// reading BenchmarkEnumerateRuleBacked's committed table instead. A gate that
// cried wolf would be
// disabled within a month, and a disabled gate catches nothing at any threshold.
const enumerateBoundRatioCeiling = 1.5

// enumerateNFRRounds is how many interleaved measurement rounds the gate takes
// per arm, and enumerateNFRRunsPerRound how many enumerations each round times.
//
// The arms are measured ROUND-ROBIN, not one after the other, so a load spike
// part-way through the test lands on every arm rather than on whichever one
// happened to be running — an ordering artefact is the easiest way to turn a
// ratio assertion into a coin flip. Each arm then keeps its MINIMUM round, not
// its mean: contention on a shared runner is one-sided (it only ever makes a run
// slower), so the fastest round is the closest estimate of the machinery's real
// cost and averaging would fold the noise straight into the ratio.
//
// Three enumerations per round is already ~6 000 rule evaluations at the default
// bound and ~12 000 at the raised one, so a round is a large enough unit to time;
// seven rounds over the three arms is ~0.5 s end to end, which keeps this a gate
// rather than a second benchmark suite.
const (
	enumerateNFRRounds       = 7
	enumerateNFRRunsPerRound = 3
)

// enumerateNFRArm is one measured leg of the ratio: a fixture, and the best
// per-candidate cost observed for it.
type enumerateNFRArm struct {
	label string
	m     enumerateModel
	// best is the lowest per-returned-id cost seen across the rounds; zero until
	// the first round has run.
	best time.Duration
}

// TestCheckNFREnumerateBound is the raised-bound half of the hard NFR gate: it
// asserts that raising the enumeration bound scales the cost LINEARLY, by
// measuring an enumeration at a bound above the default and comparing its
// per-candidate cost against the same enumeration at the default bound.
//
// It is gated identically to the rest (APERTURE_BENCH_ASSERT=1, skipped under
// -short), and its NAME deliberately contains "TestCheckNFR" so the one
// documented invocation
//
//	APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
//
// — whose -run pattern is an unanchored regexp — covers it with no command
// change. A gate case reachable only by a second, undocumented command is a gate
// case that will not be run.
//
// Three arms, and the third is the one the story exists for:
//
//	default-bound   1 000 candidates, engine configured with NO bound
//	headroom        1 000 candidates at a configured bound of 2 000 (never reached)
//	at-the-bound    2 000 candidates at a configured bound of 2 000 (reached)
//
// The at-the-bound arm returns MORE ids than engine.DefaultEnumerateLimit, and
// that is asserted before any timing runs. Without it this would be the
// fails-by-passing shape CLAUDE.md names for stampedEntities(): a gate that
// measures the raised bound only in its variable names, and would keep passing if
// the bound silently fell back to the default anywhere between the engine clamp,
// the scope member gather and the provider list.
func TestCheckNFREnumerateBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR wall-clock assertion under -short")
	}
	if os.Getenv("APERTURE_BENCH_ASSERT") != "1" {
		t.Skip("set APERTURE_BENCH_ASSERT=1 to run the hard NFR latency/throughput gate")
	}

	base := &enumerateNFRArm{
		label: "default-bound",
		m:     buildEnumerateModel(t, scope.DefaultMaxMembers, 0),
	}
	headroom := &enumerateNFRArm{
		label: "raised-bound/headroom",
		m:     buildEnumerateModel(t, scope.DefaultMaxMembers, enumerateRaisedBound),
	}
	atBound := &enumerateNFRArm{
		label: "raised-bound/at-the-bound",
		m:     buildEnumerateModel(t, enumerateRaisedBound, enumerateRaisedBound),
	}
	arms := []*enumerateNFRArm{base, headroom, atBound}

	// Warm the parsed-pattern, compiled-rule and provider-metadata caches so the
	// measured window is the steady state, and assert each arm returns the id
	// count its fixture predicts before anything is timed.
	for _, a := range arms {
		warmEnumerate(t, a.m)
	}

	// The gate is only about the raised bound if the raised bound actually
	// produced a result the default one could not. Structural, so it cannot flake.
	if atBound.m.want <= engine.DefaultEnumerateLimit {
		t.Fatalf("%s: fixture returns %d ids at a configured bound of %d, which does not exceed "+
			"engine.DefaultEnumerateLimit %d — this gate would be measuring the default bound "+
			"under a raised-bound name", atBound.label, atBound.m.want, enumerateRaisedBound,
			engine.DefaultEnumerateLimit)
	}

	ctx := context.Background()
	for round := 0; round < enumerateNFRRounds; round++ {
		for _, a := range arms {
			a.observe(t, ctx)
		}
	}

	t.Logf("%s: %v per returned id (%d ids/enumeration, best of %d rounds x %d enumerations)",
		base.label, base.best, base.m.want, enumerateNFRRounds, enumerateNFRRunsPerRound)

	for _, a := range []*enumerateNFRArm{headroom, atBound} {
		ratio := float64(a.best) / float64(base.best)
		t.Logf("%s: %v per returned id over %d ids = %.2fx the default bound's per-id cost "+
			"(ceiling %.2fx)", a.label, a.best, a.m.want, ratio, enumerateBoundRatioCeiling)
		if ratio > enumerateBoundRatioCeiling {
			t.Errorf("%s: %v per returned id is %.2fx the default bound's %v, exceeding the %.2fx "+
				"ceiling. Raising the enumeration bound is supposed to cost PROPORTIONALLY more, "+
				"not more per candidate: a rising per-candidate cost is a super-linear term in the "+
				"fan-out (a quadratic member gather, a bound-sized allocation, or a cache that "+
				"stops hitting once the member set grows). See the ns/candidate column of "+
				"BenchmarkEnumerateRuleBacked, which is flat across both bounds",
				a.label, a.best, ratio, base.best, enumerateBoundRatioCeiling)
		}

		// The ratio's blind spot, covered with a threshold this suite already
		// committed to rather than a new guess. An Enumerate makes one authorization
		// decision per returned id, so decisions/sec is the same unit as
		// throughputMin, and a uniform slowdown — which divides out of the ratio by
		// construction — cannot get past both. The denominator undercounts the real
		// work (the member gather evaluates the rule once more per LISTED candidate
		// and is not charged), so the measured rate is conservative in the direction
		// that makes the assertion honest.
		rate := float64(time.Second) / float64(a.best)
		t.Logf("%s: %.0f decisions/sec (floor %.0f)", a.label, rate, throughputMin)
		if rate < throughputMin {
			t.Errorf("%s: %.0f decisions/sec is below the NFR floor %.0f", a.label, rate, throughputMin)
		}
	}
}

// observe times one round of enumerations for the arm and keeps it if it is the
// cheapest per returned id seen so far. Every round re-asserts the id count, so
// an arm that quietly stopped returning its bound's worth of ids fails here
// rather than reporting a flattering rate over a shorter result.
func (a *enumerateNFRArm) observe(t *testing.T, ctx context.Context) {
	t.Helper()
	start := time.Now()
	for i := 0; i < enumerateNFRRunsPerRound; i++ {
		ids, err := a.m.svc.Enumerate(ctx, a.m.query)
		if err != nil {
			t.Fatalf("%s: Enumerate: %v", a.label, err)
		}
		if len(ids) != a.m.want {
			t.Fatalf("%s: Enumerate returned %d ids, want %d", a.label, len(ids), a.m.want)
		}
	}
	per := time.Since(start) / time.Duration(enumerateNFRRunsPerRound*a.m.want)
	if a.best == 0 || per < a.best {
		a.best = per
	}
}
