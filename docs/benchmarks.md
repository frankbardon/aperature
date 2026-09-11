# Benchmarks & the performance NFR (FR-31)

Aperture's decision hot path carries a hard Success Metric:

> **p99 cached `Check` < 1 ms** and **≥ 10 000 checks/sec/instance.**

This document describes how that NFR is measured, how it is asserted without
flaking CI, and the committed numbers from the optimization pass (E4-S4).

## The suite

Everything lives in [`bench/`](../bench). It seeds a **sizable** authorization
model (not a three-grant toy) and exercises the full decision facade
(`service.Service.Check`), so the numbers reflect what a surface actually pays.

### Fixture (`bench/fixture_test.go`)

`buildModel` seeds an in-memory store with the generic org → project → document
domain at scale:

- 8 accounts, 60 roles, 60 groups, 480 principals;
- per account, every role carries wildcard allow-read / allow-write on its own
  project plus a more-specific deny-read carve-out, every group a
  document-scoped read, and `group0` an account-wide broad read plus a broad
  deny — overlapping candidates of differing specificity;
- 60 concrete (wildcard-free) document grants per account so `Enumerate` has a
  real, bounded candidate set to materialise.

The representative cached `Check` (`user0` reading a document under `proj0`)
resolves a subject set of six subjects to **~73 applicable grants**, of which
several match at different specificities — so deny-overrides + the specificity
tiebreak genuinely run, rather than short-circuiting on a single grant.

#### The rule-backed extension (E5-S2)

Everything above resolves through **literal** scope, which never reaches the
provider registry or the rules engine. The metadata-collections effort put array
iteration, map traversal, and E5-S1's guarded dispatcher on the same hot path, so
`buildModel` additionally seeds a rule-backed half: one `inclusive;rule=…`
permission and grant per **rule variant**, all bounded to a single object whose
metadata is served by a one-object `ObjectProvider` registered with expiry
disabled (`provider.WithTTL(0)` — the NFR is about the *cached* `Check`, and the
30 s default would silently start re-fetching partway through a long run).

The rule grants hang off a **dedicated principal and role** (`user-rules` /
`role-rules`) that `user0` does not hold, so the literal-scope baseline above is
measured on exactly the subject set and candidate grants it was measured on
before. (Attaching them to `role0` instead moved that baseline by +5 allocs/op
and +2 KB/op — a fixture that changes the number it is the control for.)

Every variant checks the **same object**, so the provider fetch and its cache
hit are byte-identical across them and the measured delta is purely the rule:

| Variant | Rule | Renders to |
|---|---|---|
| `rule-scalar` | `object.classification == "public"` | native infix — **the control** |
| `rule-literal-in` | `object.region in ["us-east", …]` | native infix; the collection operand is a **list literal**, so E5-S1 leaves it unguarded |
| `rule-nested` | `object.owner.dept == "platform"` | E2-S2 **optional chaining**: `object?.owner?.dept` |
| `rule-tags-typical` | `object.tags hasAny [...]` over 12 tags | E5-S1 **guarded dispatcher** `$op("hasAny", …)` — the collection operand is a *variable* |
| `rule-tags-at-cap` | the same over **7 281** tags | the same guarded dispatcher |

#### The date extension

Date comparisons add a second kind of per-evaluation work: both operands are
canonical **strings** and both are **parsed to instants on every evaluation**,
because comparing the text is wrong across granularities. A relative bound adds
calendar arithmetic on top, and then formats a canonical string the comparison
immediately re-parses. Eight more variants cover it, all against the same object
and all inside the asserted gate:

| Variant | Rule | What it is here for |
|---|---|---|
| `rule-date-compare` | `object.hired_at before "2026-12-31T23:59:59Z"` | the binary literal comparison — 2 parses |
| `rule-date-between` | `object.reviewed_at between ["2026-01-01", …]` | the ternary form — **3** parses over the same dispatcher arity |
| `rule-date-same-year` | `object.hired_at sameYear "2026-07-15"` | the calendar-**bucket** branch, which reads components rather than comparing instants |
| `rule-date-relative` | `… before $rel(NOW, −1 quarters, startOfQuarter)` | the bound **resolved per evaluation** instead of read from a literal |
| `rule-date-rel-bounds` | a `between` whose **both** bounds are relative | the most expensive shape the AST can express |
| `rule-date-clamped` | `… after $rel(NOW, −6 months, endOfMonth)` | the **clamping** branch: a 31-day month end offset into a 28-day month |
| `rule-date-multi` | five date comparisons under one `and` | a date-heavy policy, and where `onOrAfter`, `onOrBefore`, `sameDay` and `sameMonth` meet the floor |
| `rule-date-deny` | `object.malformed_at before …` | the **deny** branch, and the one allocating parse failure |

Three properties of that set are deliberate and are asserted rather than assumed:

- **The reference instant is pinned** (`benchNow` = `2026-08-31T12:00:00Z`,
  injected with `rules.WithClock`). A relative bound moves with the clock, so a
  fixture built on `time.Now()` decides a different comparison every day, and the
  clamping variant would only reach the branch it exists to measure during part of
  the year. `TestDateFixtureActuallyClamps` asserts the clamp genuinely runs at
  that instant — a clamping fixture that has quietly stopped clamping still passes
  every latency and allocation assertion here, it simply measures the other
  branch. Note the documented coupling: the engine has **one** time source, so
  pinning it also freezes compiled-rule cache expiry. That is inert for this
  fixture (the rules engine takes the default TTL, under which entries live until
  invalidated) and, if anything, keeps the measured window in the cached steady
  state the NFR is about.
- **One variant denies**, so the expected verdict is declared per variant and
  asserted on every warm-up rather than assumed to be `allow`. A deny is a
  different branch through the comparison, not an excuse to leave it unmeasured.
- **All eight date operators are inside the asserted set**, which
  `TestDateBenchCoverage` enforces by reading the operator registry out of
  `rules/ast.go` — see [the coverage guard](#the-coverage-guard).

##### Why 7 281 tags

The array sizes are **derived from the value model's caps**, not picked for
roundness. `provider.ValueBytes` measures a `[]any` of strings as the sum of
their lengths, and `provider.DefaultMaxValueBytes` is 64 KiB **per field value**,
so at the fixture's fixed 9-byte tag width (`tag-00042`) the cap fixes an exact
element count:

```text
floor(65536 / 9) = 7281 tags = 65 529 bytes;  7282 tags = 65 538 — over the cap
```

7 281 is therefore the **largest array a host can legally load under the
defaults** — the adversarial-but-permitted worst case the NFR has to survive.
`TestCollectionFixtureSitsAtTheValueCap` asserts that derivation rather than
leaving it in a comment, so a change to the caps fails there instead of quietly
benchmarking a different array. `rule-tags-typical`'s 12 tags (108 bytes, 0.16%
of the cap) is the realistic other end of the same axis; the pair is what makes
the scaling behaviour visible.

Both `hasAny` cases match on the **last** element on purpose: the operator
short-circuits on the first hit, so an early match would report a best case a
real tag list does not guarantee.

### Benchmarks (informational — `make bench`)

| Benchmark | What it reports |
|---|---|
| `BenchmarkCheckCachedAuditOff` / `...AuditOn` | single cached `Check` ns/op, allocs/op, and a computed `p99-ns` |
| `BenchmarkCheckThroughputAuditOff` / `...AuditOn` | sustained parallel throughput as a `checks/sec` metric |
| `BenchmarkEnumerateBounded` | bounded `Enumerate`; asserts the result never exceeds `engine.DefaultEnumerateLimit` |
| `BenchmarkCheckRule/<variant>` | cached `Check` through a rule-backed grant, per variant (E5-S2) |
| `BenchmarkCheckRuleThroughput/<variant>` | the same, as sustained parallel `checks/sec` |
| `BenchmarkRuleEval/<variant>` | the rule **in isolation** — compiled once, evaluated over the same metadata, so the fixture's ~60 µs of grant resolution cannot mask it |
| `BenchmarkCollectionScaling/tags-N` | the guarded collection operator swept across array sizes, with the field's `ValueBytes` reported alongside |
| `BenchmarkRuleCompileCached/<variant>` | the AST work a `Check` pays on **every** decision even when the compiled program is cached, with the AST's node count alongside |
| `BenchmarkEnumerateRuleBacked/candidates-N` | a **rule-backed** `Enumerate` swept across candidate-set sizes at the engine's default bound, reporting a derived `ns/candidate` and the returned `ids` count |
| `BenchmarkEnumerateRuleBacked/candidates-N/bound-M` | the same sweep at a bound **raised** through `engine.WithEnumerateLimit`, so "what does raising the bound cost?" is answered from one run |
| `BenchmarkEnumerateRuleBackedRuleEval` | the same number of `rules.Engine.Selected` calls with no engine around them, so the rule half of the enumeration cost is separable from the decision half |
| `BenchmarkDateOpEval/<op>` | each of the **eight** date operators in isolation, so the post-parse cost of an ordering operator and a calendar-bucket one come apart |
| `BenchmarkRelativeDateComplexity/<stage>` | the relative-date cost **ladder** — no `$rel`, anchor, offset, snap, snap + offset, snap + clamping offset, two relative bounds — where adjacent rows differ by exactly one stage |
| `BenchmarkRelativeDateVocabulary/<member>` | every anchor, unit and snap, so no vocabulary member's arithmetic goes unmeasured |

`make bench` runs them all with `-benchmem`. The **audit toggle** is the
benchmark axis: audit-off is the `s.audit == nil` path; audit-on wires a sampled
(1%), asynchronous `audit.Recorder` — the production shape where decision audit
is off the critical path (E4-S2).

### The hard NFR assertion (gated — `TestCheckNFR`)

Wall-clock assertions are environment-sensitive, so the **hard** gate is a test
that is **off by default**:

- it `t.Skip`s under `go test -short`, **and**
- it `t.Skip`s unless `APERTURE_BENCH_ASSERT=1` is set.

The default `make test` therefore never runs a wall-clock assertion and stays
fast and deterministic. Run the gate explicitly:

```sh
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
```

Methodology inside the gate:

- **p99:** time 100 000 cached `Check`s on a warm engine, sort the per-op
  latencies, take the 99th percentile, assert `< 1 ms`.
- **throughput:** run 200 000 cached `Check`s, divide by wall time, assert
  `≥ 10 000 checks/sec` (a conservative single-goroutine floor; a real instance
  parallelises well above it — see the throughput benchmark).
- both are run with audit **on** and **off**.

`TestCheckNFRCollections` applies the **same two thresholds** to each rule
variant, audit on and off — the date variants included, so a date comparison is
held to exactly the floor every other cached `Check` is. Its name deliberately
contains `TestCheckNFR` so the invocation above — whose `-run` pattern is an
unanchored regexp — picks it up with no command change. It measures over 20 000
samples rather than 100 000/200 000 because the gate multiplies out over every
variant × the audit axis (26 combinations today, ~105 s end to end); the
thresholds are *rates*, so the smaller sample does not weaken them.

`TestCheckNFREnumerateBound` is the third case that invocation picks up, and the
one threshold in the suite that is **not** a wall clock: it holds a rule-backed
`Enumerate` at a **raised** bound to a *ratio* against the same enumeration at the
default bound. See [the raised-bound gate](#the-raised-bound-gate-testchecknfrenumeratebound).

### The allocation guard (E5-S2)

`provider` hands a cached `Metadata` out **by reference** and documents the
read-only contract as **transitive** over nested values. A defensive deep copy
introduced on that read path would be invisible to every correctness test in the
repo — the decisions would stay identical — and fatal to the NFR, because it
would put an O(metadata) copy on the per-`Check` hot path. Two tests cover it,
deliberately at different costs:

| Test | Gated? | What it does |
|---|---|---|
| `TestMetadataIsSharedByReference` | **no** — runs in `make test` | structural: the map, its nested object, and its nested arrays returned by two successive `Fetch`es must be the **same objects by address**. Cannot flake, costs nothing, fails the instant someone clones on read. |
| `TestCollectionCheckAllocations` | yes (`APERTURE_BENCH_ASSERT=1`) | measured: the per-`Check` allocation profile of every variant, with a **per-element** copy detector |

The measured detector differences the two collection cases **against each other**
rather than against the scalar control. That cancels the guarded dispatcher's
fixed per-evaluation overhead exactly and leaves **bytes per array element** —
which is the actual signature of a copy. Sharing by reference reads as ~0 B per
element; materialising the array into a fresh `[]any` costs 16 B per element for
the backing store alone, so the budget sits at 8 B, well clear of both.

`TestDateCheckAllocations` is the same idea for the date path, and it is budgeted
per **mechanism** rather than per absolute number, so a regression fails with the
cause rather than with a figure. A date rule has three plausible places for a
copy-shaped mistake to hide — a constant bound re-parsed per evaluation, an
intermediate `time.Time` per calendar step, and an error value built on a failed
parse — and each has a budget naming it:

| Budget | Claim | Measured |
|---|---|---:|
| a date comparison over the scalar control ≤ 8 allocs/op | the cost is the `$date` dispatcher's fixed **eight-argument call boxing**, and nothing else | +7.0 |
| `between` over `before` ≤ 1 alloc/op | **parsing allocates nothing** — `between` parses three operands to `before`'s two | +0.0 |
| clamping over non-clamping ≤ 1 alloc/op | the month-end clamp is a comparison and a substitution; no intermediate instants | +0.0 |
| the deny path over the matching allow ≤ 6 allocs/op | the one allocating step is the `*time.ParseError` the standard library builds and `provider` discards | **−2.0** |
| a second relative bound ≤ 6 allocs/op | a second relative operand pays a second *resolution*, not a second per-decision setup | +3.0 |
| each date comparison after the first ≤ 7 allocs/op | a rule's date cost is **linear** in comparison count | +4.2 |

The deny row is negative because a rule that denies also leaves the grant
unselected, so the decision does less afterwards than an allow — more than paying
for the four extra allocations. The isolated figure is `BenchmarkRuleEval`, where
`rule-date-deny` is **+4 allocs / +120 B** over `rule-date-compare` with no
decision around it. The budget here is a ceiling on the whole branch, and a coded
error constructed on that path — exactly what `provider.DateValueOf` exists to
avoid — would clear it easily.

### The coverage guard

`TestDateBenchCoverage` is ungated, cheap, and about the fixture that **does not**
exist. It reads the date-operator registry out of `rules/ast.go` (the `opSpecs`
entries whose kind is `renderDate`) and the three relative-date vocabularies out
of `rules/relative.go`, and fails if any member has no bench fixture. Nothing
about adding a ninth date operator or a twelfth snap makes anyone open `bench/`,
which is precisely why a hand-maintained list would fall behind — and the repo
already gates the Go↔JS editor contract, the collection operator tables and the
scope strategy matrix the same way.

The two tiers are held to **different** standards on purpose:

- an **operator** must appear in the *asserted* set (`ruleVariants()`), because an
  operator is a comparison on the `Check` hot path and throughput is the only
  thing that can see a regression there — a fixture outside the assertion proves
  nothing;
- a **vocabulary member** must appear somewhere in the corpus, asserted or
  informational. Holding 20 anchor/unit/snap combinations to a wall-clock floor
  would multiply the gate's runtime for arithmetic that is bounded by construction
  (it touches no metadata, allocates nothing per element, and scales with
  nothing), while the two shapes that *do* reach the hot path — one resolution and
  two resolutions in one comparison — are both already asserted.

Like `rules/editor_js_contract_test.go` it scans **source text**, so restructuring
a table declaration can break the scanner even when the tables agree. It therefore
fails loudly with the file and the pattern it could not find, and never skips.

## Committed results

Measured on an Apple M1 Max (`go test -benchtime=2s`); absolute numbers are
hardware-dependent, but the **headroom** and the **allocation profile** are the
durable signal.

| Metric (cached `Check`) | audit off | audit on |
|---|---|---|
| mean latency | ~66 µs/op | ~70 µs/op |
| allocations | 34 allocs/op | 34 allocs/op |
| p99 (gated `TestCheckNFR`, 100k samples) | ~0.275 ms | ~0.265 ms |
| throughput (single goroutine) | ~15 100 checks/sec | ~14 700 checks/sec |
| throughput (parallel benchmark) | ~20 000 checks/sec | ~30 000 checks/sec |

Both targets are met with comfortable headroom — p99 sits ~3.6× under the 1 ms
ceiling, and even the single-goroutine throughput clears the 10 k/s floor by
~1.5×, before parallelism. **Audit-on does not regress the target:** sampling is
a single `Sampler.Sample()` call on the un-kept path and the kept event is built
lazily and written asynchronously, so the decision never blocks on audit.

### Collection & nested-object rules (E5-S2)

Re-measured on the same Apple M1 Max, default `-benchtime`, **after both E5-S2
findings were fixed** (the `[]any` fast path in `rules/shape.go` and the literal
fast path in `rules/ast.go` — see [Findings](#findings) for the before/after).
The literal-scope baseline above is **unchanged** by this fixture (61 890 B/op,
34 allocs/op, both before and after), which is what makes the rule numbers
readable as deltas.

Cached `Check` through a rule-backed grant, audit off (`BenchmarkCheckRule`):

| Variant | ns/op | B/op | allocs/op | `p99-ns` | parallel checks/sec |
|---|---:|---:|---:|---:|---:|
| `rule-scalar` (control) | 54 061 | 10 779 | 47 | 220 708 | 54 557 |
| `rule-literal-in` | 54 047 | 10 812 | 47 | 211 834 | 55 782 |
| `rule-nested` | 56 716 | 10 795 | 48 | 216 625 | 59 501 |
| `rule-tags-typical` (12) | 56 003 | 11 285 | 52 | 221 625 | 55 441 |
| `rule-tags-at-cap` (7 281) | 85 925 | 11 286 | 52 | 273 625 | 45 245 |

The gated `TestCheckNFRCollections`, single-goroutine over 20 000 samples:

| Variant | p99 (audit off / on) | checks/sec (audit off / on) | verdict |
|---|---|---|---|
| `rule-scalar` | 205 µs / 224 µs | 17 593 / 18 586 | pass |
| `rule-literal-in` | 197 µs / 199 µs | 18 039 / 18 133 | pass |
| `rule-nested` | 214 µs / 178 µs | 18 413 / 17 656 | pass |
| `rule-tags-typical` | 239 µs / 188 µs | 18 216 / 17 020 | pass |
| `rule-tags-at-cap` | 260 µs / 251 µs | 11 462 / 11 747 | pass |

The rule in isolation (`BenchmarkRuleEval`), which strips the ~55 µs of grant
resolution out of the numbers above:

| Variant | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `rule-scalar` | 157.7 | 96 | 3 |
| `rule-literal-in` | 176.6 | 96 | 3 |
| `rule-nested` | 209.3 | 112 | 4 |
| `rule-tags-typical` | 426.5 | 384 | 6 |
| `rule-tags-at-cap` | 27 141 | 384 | 6 |

**What passes cleanly.** Optional chaining (`rule-nested`) costs +1 alloc/op and
+16 B/op over a flat field — it is a render-time concern and stays one at
runtime. The list-literal render E5-S1 deliberately left native evaluates
identically to a scalar comparison (176.6 ns vs 157.7 ns, same 3 allocs) **and now
costs the same per `Check` too** (47 allocs/op against the control's 47) — so the
common `object.region in [...]` shape pays nothing for the operator anywhere. A
realistic 12-tag collection adds ~270 ns and ~500 B per `Check` — noise against a
54 µs decision. Note the collection variants' B/op no longer grows with array
length: `rule-tags-at-cap` allocates 11 286 B against `rule-tags-typical`'s
11 285, over 600× the elements.

### Date comparisons

Re-measured on the same Apple M1 Max, default `-benchtime`, against the pinned
reference instant. **The gate passes on every date variant, with the smallest
margin at 1.57× the throughput floor** — the slowest case in the whole suite
remains `rule-tags-at-cap`, not a date rule.

`TestCheckNFRCollections`, single-goroutine over 20 000 samples:

| Variant | p99 (audit off / on) | checks/sec (audit off / on) | verdict |
|---|---|---|---|
| `rule-date-compare` | 227 µs / 212 µs | 17 192 / 17 484 | pass |
| `rule-date-between` | 219 µs / 202 µs | 17 038 / 17 186 | pass |
| `rule-date-same-year` | 233 µs / 209 µs | 17 241 / 17 702 | pass |
| `rule-date-relative` | 230 µs / 210 µs | 16 960 / 17 477 | pass |
| `rule-date-rel-bounds` | 235 µs / 214 µs | 16 677 / 17 326 | pass |
| `rule-date-clamped` | 237 µs / 195 µs | 16 956 / 17 552 | pass |
| `rule-date-multi` | 250 µs / 221 µs | **15 664** / 16 274 | pass |
| `rule-date-deny` | 225 µs / 191 µs | 16 947 / 17 640 | pass |

For scale, on the same run the literal-scope baseline was 296 µs / 14 150 and the
collection variants 212–254 µs / 11 934–17 624. Absolute numbers have varied ~12 %
run to run on this machine under load; the *ordering* has not.

The rule in isolation (`BenchmarkRuleEval`), which strips the ~55 µs of grant
resolution out of the numbers above:

| Variant | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `rule-scalar` (control) | 156.3 | 128 | 3 |
| `rule-date-same-year` | 526.4 | 704 | 7 |
| `rule-date-compare` | 590.6 | 704 | 7 |
| `rule-date-deny` | 661.5 | 824 | 11 |
| `rule-date-between` | 751.5 | 704 | 7 |
| `rule-date-clamped` | 897.0 | 872 | 10 |
| `rule-date-relative` | 898.3 | 872 | 10 |
| `rule-date-rel-bounds` | 1 334 | 976 | 12 |
| `rule-date-multi` | 2 112 | 1 344 | 14 |

Four things are readable off that, and each is a claim a future regression has to
break:

1. **The parse itself allocates nothing.** `rule-date-between` parses *three*
   operands to `rule-date-compare`'s two and is byte-identical at 704 B / 7
   allocs. The ~576 B a date comparison costs over the scalar control is the
   `$date` dispatcher's fixed **eight-argument call boxing**, not parsing.
2. **Clamping is free.** `rule-date-clamped` genuinely clamps (asserted, not
   assumed) and measures 897.0 ns / 872 B / 10 allocs against the non-clamping
   `rule-date-relative`'s 898.3 / 872 / 10.
3. **A second relative operand costs a second full resolution.** The per-decision
   `NOW` snapshot is shared — that is what makes a decision reproducible — but
   only the *instant* is shared, not the arithmetic. `rule-date-rel-bounds` is
   +583 ns and +2 allocs over `rule-date-between`: two resolutions at ~290 ns
   each, not one amortised. That follows from the design (`$rel` is an ordinary
   call per operand) rather than being a defect, but it means a rule's cost scales
   with its **relative-operand count**.
4. **The deny path allocates, and is bounded** — +4 allocs / +120 B, the
   `*time.ParseError` described above.

`rule-date-multi` is the slowest date case (15 664 checks/sec, still 1.57× the
floor) and only part of that is its five comparisons: at 16 AST nodes it also pays
the largest per-decision **AST re-walk** in the suite — 4 112 ns / 18 allocs in
`BenchmarkRuleCompileCached`, against 913 ns / 8 allocs for the single-comparison
`rule-date-compare`. That is the known walk cost documented under
[Findings](#findings), scaling with node count exactly as described there, and it
is not specific to dates: any rule with five comparisons of any kind pays it. It
is worth knowing before reading the multi-comparison row as a date number.

#### Where the cost of a relative date actually is

`BenchmarkRelativeDateComplexity` is a ladder whose adjacent rows differ by
exactly one stage, so each stage's contribution is a difference rather than part
of an aggregate:

| Stage | ns/op | B/op | allocs/op | delta |
|---|---:|---:|---:|---|
| `literal-bound` (no `$rel`) | 420.4 | 688 | 6 | — |
| `anchor` | 769.9 | 856 | 9 | **+349 ns, +168 B, +3 allocs** |
| `offset` | 789.0 | 856 | 9 | +19 ns, +0 B, +0 |
| `snap` | 792.8 | 856 | 9 | +23 ns, +0 B, +0 |
| `snap-offset` | 846.2 | 856 | 9 | +76 ns, +0 B, +0 |
| `snap-offset-clamped` | 859.7 | 856 | 9 | +13 ns, +0 B, +0 |
| `two-relative-bounds` | 1 300 | 960 | 11 | a second full resolution |

**The calendar arithmetic is not the expensive part.** Snapping, offsetting and
clamping together are ~90 ns and *zero* allocations on top of the bare anchor. The
+349 ns and all three allocations arrive with the very first `$rel`, and they are
its call boxing plus the canonical string it formats for `$date` to immediately
re-parse.

That round trip is the one date optimization with real headroom behind it, and it
is **deliberately not done**: removing it means `$rel` returning something `$date`
accepts without re-parsing, which changes the dispatcher's operand contract. The
same is true of `$date`'s fixed eight-argument arity. Both are recorded here as
measured, accepted costs — the floors hold comfortably with them in, and this
suite measures rather than tunes.

`BenchmarkRelativeDateVocabulary` reports every anchor, unit and snap at a flat
~1.1 µs / 960 B / 11 allocs per evaluation (two resolutions each — a
self-comparison is true exactly when the node resolved, which is the only way to
prove from outside the package that the arithmetic ran). No vocabulary member is
materially dearer than another, ISO-week and quarter boundaries included, so there
is no member-specific cost for a rule author to route around.

`BenchmarkDateOpEval` reports all eight operators at an identical 704 B / 7 allocs
and 496–659 ns, `between` dearest (three parses) and the inclusive ordering
operators cheapest.

### Rule-backed `Enumerate`

`Enumerate` is a different shape from `Check`, and until now nothing measured it
through a rule. A rule is not invertible, so an `inclusive;rule=…` grant's
members are found by **listing the object type and filtering each candidate
through the rule** — and the engine then runs its ordinary deny-overrides
decision over every survivor, which consults the same grant again. One
`Enumerate` is therefore up to **2 × candidates** rule evaluations.

`bench/enumerate_test.go` builds its own store, registry and rules engine (so the
cached-`Check` baseline above is untouched) with one account-wide
`inclusive;rule=…` grant over N documents, all of which the rule selects — the
worst case, since a rejected candidate skips the second evaluation.

The sweep has two halves, run in the **same** invocation so the before/after
comparison never spans two machines: rows with no bound run on the engine's
default, and rows with one configure it through `engine.WithEnumerateLimit` —
the option, never a constant read back out, so the number really travels engine
clamp → scope member gather → provider list.

`BenchmarkEnumerateRuleBacked`, Apple M1 Max (10 cores), go1.26.5 darwin/arm64,
`-benchtime=2s -count=3`, medians of 3, audit off:

| candidates | bound | ns/op | ns/candidate | allocs/op | B/op | ids returned |
|---:|---:|---:|---:|---:|---:|---:|
| 10 | — | 54 724 | 5 472 | 596 | 45 060 | 10 |
| 100 | — | 567 891 | 5 679 | 5 679 | 442 772 | 100 |
| 1 000 | — | 6 317 880 | 6 318 | 56 133 | 4 444 912 | 1 000 |
| 2 000 | — | 5 929 466 | 5 929 | 56 136 | 4 478 722 | 1 000 |
| 1 000 | 2 000 | 5 743 803 | 5 744 | 56 127 | 4 444 822 | 1 000 |
| 2 000 | 2 000 | 11 638 062 | 5 819 | 112 176 | 8 964 388 | 2 000 |
| 4 000 | 2 000 | 12 005 617 | 6 003 | 112 162 | 9 025 695 | 2 000 |

And the rule half in isolation (`BenchmarkEnumerateRuleBackedRuleEval`, 1 000
`Selected` calls), same run: **2 348 ns/eval, 17 allocs/eval, ~1 297 B/eval**.

The whole table — the untouched `RuleEval` row included — sits roughly 40% above
the figures first committed here (4 195 ns/candidate, 1 235 ns/eval). Every row
moved together, so read the **internal ratios**; the absolute numbers are one
machine on one day.

- **~5.7–6.3 µs and ~56 allocations per returned id**, flat across three orders
  of magnitude *and* across both bounds — linear in the result size, with no
  super-linear term in the resolver at the raised bound either.
- **Doubling the bound doubles the cost, and no more than doubles it.** 1 000
  ids → 2 000 ids is 6.32 ms → 11.64 ms (1.84×), 56 133 → 112 176 allocations
  (2.00×), 4.44 MB → 8.96 MB (2.02×). Budget a raise as proportional: roughly
  **4.5 KB and 56 allocations of transient garbage per id the bound allows**, so
  a bound of 10 000 is a ~45 MB, ~58 ms enumeration.
- **Raising the bound is free until the population reaches it.** The
  1 000-candidate row at bound 2 000 costs what it costs unconfigured — 5.74 ms
  vs 6.32 ms, 56 127 vs 56 133 allocations, the same `B/op` to four digits.
  Headroom a deployment does not use is not paid for.
- **The raised bound clamps exactly as the default one does.** 4 000 candidates
  at bound 2 000 costs what 2 000 at bound 2 000 costs (12.01 vs 11.64 ms,
  112 162 vs 112 176 allocations), and 2 000 candidates unconfigured costs what
  1 000 do. Past the bound the extra objects are never visited, so the worst case
  stays a constant a host can budget for — it is just a constant the operator now
  chooses. `TestEnumerateRuleBackedStaysBounded` pins that ungated, at the
  default bound and at a raised one.
- **The rule is ~55–60% of it.** Two evaluations at 2 348 ns is ~4.7 µs of the
  ~5.8–6.3 µs and 34 of the ~56 allocations; the rest is the decision engine's
  own per-candidate work. Inside the rule half the largest line item is the
  per-decision AST re-walk (`BenchmarkRuleCompileCached`).
- **A bound-limit enumeration is ~6.3 ms at the default bound**, roughly 1 000× a
  cached `Check`. That is arithmetic (it *is* ~2 000 evaluations plus 1 000
  decisions), but it makes rule-backed `Enumerate` an interactive operation, not
  a hot-path one — and raising the bound scales that interactive latency with it.
- One asymmetry: a smaller `EnumerateRequest.Limit` shortens only the **second**
  half. The member set is gathered first and bounded by the engine's *configured*
  bound regardless, so `Limit=10` against a bound of 1 000 still pays ~1 000
  evaluations to build it, then ~10 decisions. The knob that moves the first half
  is the configured bound, not the request's `Limit`.

The fixtures wire a discarding `slog` logger. Every row of the sweep is designed
to land exactly on its bound, which is precisely when the engine emits its
"enumeration returned exactly its bound" WARN — unwired that goes to
`slog.Default()`, once per iteration, and the benchmark would be measuring the
log handler. The warning itself is asserted in
`engine/enumerate_bound_warning_test.go`, where it belongs.

The absolute figures above are **informational** — they exist so a future change
that regresses them has something to regress against. What is *asserted* is the
**shape**, in `TestCheckNFREnumerateBound`.

#### The raised-bound gate (`TestCheckNFREnumerateBound`)

Raising the bound is an operator-facing knob on the worst case, so a regression
in the enumeration fan-out should be caught by the suite rather than discovered
in production. The gate rides the same invocation as everything else — its name
contains `TestCheckNFR`, and `-run` is an unanchored regexp:

```sh
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
```

Three arms, measured **round-robin** (so a load spike lands on all three rather
than on whichever happened to be running), each keeping its **minimum** round
(contention on a shared runner is one-sided, so the fastest round is the closest
estimate of the machinery's real cost):

| Arm | Candidates | Configured bound | ids |
|---|---:|---:|---:|
| `default-bound` | 1 000 | none | 1 000 |
| `raised-bound/headroom` | 1 000 | 2 000 | 1 000 |
| `raised-bound/at-the-bound` | 2 000 | 2 000 | **2 000** |

The at-the-bound arm returns **more ids than `engine.DefaultEnumerateLimit`**, and
that is asserted structurally *before* anything is timed — otherwise this would be
the fails-by-passing shape `CLAUDE.md` names for `stampedEntities()`: a gate that
measures the raised bound only in its variable names.

**The threshold is a ratio, not a wall clock**, and that is the whole design.
A 2 000-id rule-backed enumeration is ~11.6 ms *by design*, so neither `p99Ceiling`
(1 ms) nor a re-tuned absolute number transfers: the table above already sits ~40 %
above its first-committed figures, and the `candidates-4000/bound-2000` row alone
ranged 11.21–16.31 ms (1.45×) inside a single `-count=3` run. A ratio divides
machine speed and baseline drift out entirely, because both arms are measured on
the same machine in the same second.

The ratio asserted is **per-candidate cost at the raised bound ÷ per-candidate cost
at the default bound ≤ 1.5×**, derived from the table above (denominator: the
1 000-candidate unconfigured row at 6 318 ns/candidate):

| Row | ns/candidate | ratio |
|---|---:|---:|
| 1 000 @ bound 2 000 (headroom) | 5 744 | 0.91 |
| 2 000 @ bound 2 000 (at the bound) | 5 819 | 0.92 |
| 4 000 @ bound 2 000 (past it) | 6 003 | 0.95 |

and the widest spread of `ns/candidate` between *any* two rows of the whole sweep
— three orders of magnitude of population, both bounds — is 5 472…6 318 = **1.15×**.
So 1.5 sits 1.63× above the worst ratio ever measured and 1.3× above the widest
spread the sweep has produced.

**What it catches:** a super-linear term in the fan-out — a quadratic member
gather, a bound-sized pre-allocation, a per-candidate rescan, or a cache that
stops hitting once the member set grows. All present as `ns/candidate` rising with
the bound. A quadratic member gather injected into `scope.enumerateOfType`
measures **1.86×** and fails, while the headroom arm correctly stays at 0.99× —
the ratio really is measuring the bound and not the fixture. The headroom arm
catches the shape specific to the configurable bound: configuring a ceiling a
deployment never reaches must cost nothing.

**What it deliberately does not catch:** a uniform slowdown hitting both arms
equally — by construction, that divides out. The ~40 % drift above is exactly that
shape, and it is a benchmark question, not a gate question. The blind spot is
covered by a second assertion that introduces **no new constant**: an `Enumerate`
makes one authorization decision per returned id, so the arms are additionally
held to the committed `throughputMin` as decisions/sec (they measure ~180–230 k,
18–23× the floor).

**Which risk the threshold favours: not flaking on a loaded machine**, explicitly
and by a wide margin. Observed on an Apple M1 Max across **17 runs** — idle, and
under 12 then 16 competing CPU-burner processes at load averages up to 24 — both
ratios stayed within **0.94×–1.10×** against the 1.5× ceiling. The absolute per-id
cost moved ~15 % under that load; the ratio did not. For contrast, the same two
populations read straight off `BenchmarkEnumerateRuleBacked` during that load (no
interleaving, no min-of-rounds) differed by **1.72×** — which is what the
*measurement design*, rather than the ceiling, is buying.

The cost of the choice is stated plainly: a regression making the raised
bound 1.0–1.5× dearer *per candidate* than the default passes here and must be
caught by reading the table above. A gate that cried wolf would be disabled within
a month, and a disabled gate catches nothing at any threshold.

The whole gate runs in **~0.5 s**.

The counters in the compiled-rule cache are `sync/atomic` for this reason. The
hit path used to take the cache's **write** lock purely to bump `hits`, which
serialised concurrent evaluations on a number none of them read; at one rule
evaluation per decision that was a rounding error, but rule-backed enumeration
multiplies it by the candidate count. `TestCacheHitCountIsExactUnderConcurrency`
(in `rules/`) asserts the counter stays exact — 32 goroutines × 100 evaluations
must report exactly 3 200 hits — so the change is correct, not merely faster.

## Findings

Two costs the collection fixtures surfaced. Both were **reported, not tuned
around** — the benchmarks were left asserting the honest budget and failing —
and both have since been fixed. The before/after is kept here because the
mechanisms are the interesting part, and because the budgets that caught them are
now the ratchets holding the fixes in place.

### 1. The guarded dispatcher copied the collection operand on every evaluation

**Status: fixed** (`rules/shape.go`). `BenchmarkCollectionScaling` originally
showed a cost **linear in array length** with no fixed component worth speaking
of — and, critically, allocated bytes growing at the same rate:

| Elements | field bytes | ns/op (before → after) | B/op (before → after) | allocs/op |
|---:|---:|---:|---:|---:|
| 12 | 108 | 519.3 → 410.2 | 592 → 384 | 8 → 6 |
| 128 | 1 152 | 1 721 → 892.5 | 2 704 → 384 | 8 → 6 |
| 1 024 | 9 216 | 11 431 → 4 170 | 18 832 → 384 | 8 → 6 |
| 4 096 | 36 864 | 42 589 → 15 669 | 65 936 → 384 | 8 → 6 |
| 7 281 | 65 529 | 88 344 → 27 169 | 123 280 → 384 | 8 → 6 |

The old cost was **~12 ns and ~16.9 bytes per element, per `Check`** — and
16 B/element is exactly the cost of a fresh `[]any` backing store. The mechanism
was `rules/shape.go`, `classify`:

```go
o.members = make([]any, rv.Len())
for i := range o.members {
    o.members[i] = rv.Index(i).Interface()
}
```

Every guarded collection comparison materialised a fresh `[]any` copy of the
operand, filled element-by-element through `reflect`, before the operator ran. The
reflect branch exists so that any slice kind works, but the validated metadata
model only ever admits `[]any` (see the `metadata-values` skill), so for the shape
that actually reaches it the copy bought nothing. `classify` now aliases an
`[]any` operand directly — safe because `members` is read-only after
construction, which is what preserves `provider`'s transitive by-reference
contract — and falls back to the reflect copy for anything else.

Traversal is now **~3.7 ns and 0 bytes per element**: allocated bytes are flat at
384 B from 12 elements to 7 281.

This was the exact failure mode E5-S2 was written to catch: `provider` goes to
trouble to hand metadata out by reference and to make that contract transitive,
and the value was copied anyway one layer up. It is invisible to every
correctness test — the decision is identical either way — which is why the guard
is a measured allocation budget rather than an assertion about behaviour.

**Consequence for the 64 KiB cap.** The interview's flagged risk was real before
the fix: at ~12 ns/element the budget above the 10 000 checks/sec floor
(100 µs/`Check`, of which the fixture's own resolution spent ~62 µs) was roughly
**3 000 elements, about 40 % of what the default cap permits**, and the
cap-maximal 7 281-element array measured 6 195–6 710 checks/sec. At ~3.7
ns/element the same arithmetic gives **~12 500 elements**, comfortably past the
7 281 the cap admits, and that array now measures 11 462–11 747 checks/sec. So
`DefaultMaxValueBytes` does not need lowering: the cap is inside the budget on
this hardware. It stays configurable through `provider.ValueLimits`, and a host
whose per-`Check` resolution is heavier than this fixture's should re-run the
arithmetic with its own baseline rather than inherit the conclusion.

### 2. The compiled-rule cache did not remove the AST walk

**Status: fixed** (`rules/ast.go`, [issue #9]). `rules.Engine.Selected` calls
`Engine.Compile` on **every** evaluation, and `Compile` re-validates the AST,
re-renders it to expression source, and re-hashes that source *before* it can
probe the compiled-program cache. The cache removes the expr-lang compile; it does
not remove the walk. That walk's cost scaled with node count
(`BenchmarkRuleCompileCached`):

| Variant | AST nodes | ns/op (before → after) | B/op (before → after) | allocs/op (before → after) |
|---|---:|---:|---:|---:|
| `rule-scalar` | 3 | 2 246 → 684 | 5 203 → 288 | 19 → 5 |
| `rule-nested` | 3 | 2 178 → 644 | 5 203 → 288 | 19 → 5 |
| `rule-tags-typical` | 4 | 2 509 → 834 | 5 419 → 488 | 21 → 7 |
| `rule-tags-at-cap` | 4 | 2 591 → 875 | 5 419 → 488 | 21 → 7 |
| `rule-literal-in` | 6 | 5 503 → 673 | 15 066 → 320 | 47 → 5 |

The dominant term was **per literal node**: ~14 allocs and ~4.9 KB each, because
`Validate` → `decodeScalar` built a `json.NewDecoder` per literal, on every walk,
twice per decision — the decoder's internal read buffer dwarfing the value it was
parsing. That fully accounted for `rule-literal-in`'s +28 allocs/op and
+9.9 KB/op at the `Check` level (+28.1 and +9 876 measured) even though
`BenchmarkRuleEval` shows the two rules evaluating identically.

The fix is `rules.classifyScalar`: it identifies the common literal forms —
`null`, `true`, `false`, any JSON number, and any string of unescaped printable
ASCII — straight from the raw bytes with **no allocation**, and both `Validate`
and `render` short-circuit on it. It is deliberately **one-sided**: it answers
only where the bytes are provably that form under the JSON grammar, and defers to
`decodeScalar` for escaped or non-ASCII strings, composites, and malformed input.
So it cannot widen what `Validate` accepts and cannot change a rendered byte —
properties pinned by `rules/scalar_test.go`, which runs every input through both
paths and asserts they agree, plus a fuzz target stating the same invariant.

What remains in the walk is flat in literal count: the render buffer, the rendered
string, and the sha256 + hex of it. `rule-literal-in` and `rule-scalar` now cost
the same 47 allocs per `Check` (measured delta **0.0**, down from 28.1), and even
the minimal 3-node rule dropped from 2.2 µs / 19 allocs / 5.2 KB to 0.68 µs / 5
allocs / 288 B. This was never a collections problem; the collection fixtures
merely made it legible.

**Not done: skipping the walk entirely on a cache hit.** Both designs issue #9
sketched — keying the cache on the rule reference, or memoising the rendered
source on the `Node` — trade the current key's best property for speed the fix
above already delivered. The source hash is *content-addressed*, so an edited
rule cannot keep deciding with a stale program by construction; a
reference-keyed cache needs correct invalidation on every mutation path to get
the same guarantee, and a memo on `Node` — a public, mutable struct the Rete
editor edits in place — cannot enforce its own invalidation at all. With the walk
at 0.68 µs against a ~54 µs `Check`, neither risk is worth taking.

[issue #9]: https://github.com/frankbardon/aperture/issues/9

## The optimization pass

`go test -bench -benchmem` identified the dominant per-`Check` allocator: the
literal/scope coverer re-parsed each grant's object **pattern on every candidate
of every Check** (`identity.ParsePattern(g.Object)`), so a principal resolving
~73 grants paid ~73 fresh pattern parses — each a string split plus a new
segment slice.

The fix is a parsed-pattern cache in the engine
([`engine/patterncache.go`](../engine/patterncache.go)): a concurrency-safe
`sync.Map` keyed by the object string, shared by the literal and scope coverers.
A parsed `Pattern` is immutable and a pure function of its source, so a cache hit
returns the identical pattern a fresh parse would — **decision semantics are
unchanged** and the full existing test suite stays green.

Effect on the cached `Check`: **172 → 34 allocs/op** (~5× fewer), with the
re-parse churn and its GC pressure removed from the hot path.

Measure-first discipline: the literal decision hot path does not exercise the
compiled-rule cache (`rules`, E2-S3) or the provider metadata cache (`provider`,
E2-S2) — those already bound their own costs behind hash-keyed / TTL+LRU caches —
so no change was made there absent a benchmark showing a win. E5-S2's rule-backed
fixture is what finally put both of those caches under measurement, and
[Findings](#findings) is what came back.

## Regression guard

`TestCheckNFR` and `TestCheckNFRCollections` are the threshold guards: they fail
if p99 ever crosses 1 ms or throughput drops below 10 k/s, on the literal path and
on each rule variant respectively. They are gated (see above) so they never flake
the default build, but they are wired and runnable on demand and in a dedicated CI
job/cron where the runner is known to be unloaded.
`TestMetadataIsSharedByReference` is the one guard that is **not** gated: it is
structural rather than timed, so it runs in every `make test`.
`TestDateBenchCoverage` and `TestDateFixtureActuallyClamps` are ungated on the
same principle — the first is a source scan, the second is calendar arithmetic,
and neither has a wall clock in it. They cover the two ways a date fixture fails
silently: a new operator or vocabulary member that no fixture exercises, and a
clamping fixture that has stopped clamping.
`TestEnumerateRuleBackedStaysBounded` is ungated for the same reason: it asserts
that a rule-backed `Enumerate` over twice the bound's worth of candidates returns
**exactly** the bound — not more (an unbounded member set) and not less (a
second, tighter limit having crept in, which would be a wrong access-control
answer rather than a performance tradeoff). It does so three ways: unconfigured
(clamping at `scope.DefaultMaxMembers`), at a bound raised through
`engine.WithEnumerateLimit` over a population above it (clamping at the raised
number), and at a raised bound above the population (no clamp at all, returning
more ids than the default ever could). The last two additionally assert the
result **exceeds** `engine.DefaultEnumerateLimit`, so a raised bound that
silently fell back to 1 000 anywhere along engine clamp → scope gather → provider
list fails here rather than passing as a tautology.

**All gates now pass.** Three were failing by design as of E5-S2, left asserting
the honest budget rather than relaxed to green — the numbers were the
deliverable, and the budgets were what a fix had to satisfy. Each is now a
ratchet rather than an open finding — see [Findings](#findings):

| Gate | Case | Budget | Was | Now |
|---|---|---|---|---|
| `TestCheckNFRCollections` | `rule-tags-at-cap` | ≥ 10 000 checks/sec at the largest array the default 64 KiB cap permits | 6 195–6 710 | 11 462–11 747 |
| `TestCollectionCheckAllocations` | per-element copy detector | ≤ 8 B/element | 16.9 B | ~0 B |
| `TestCollectionCheckAllocations` | `rule-literal-in` alloc budget | ≤ 2 allocs/op over the scalar control (was held at 30) | +28.1 | +0.0 |

The `rule-literal-in` budget in particular is now tight on purpose: any
per-literal decode reintroduced into the AST walk scales with literal count and
blows straight past 2.

The date budgets are newer and were **met on first measurement** — unlike the
three above, nothing had to be fixed to make them green. They are set at the
measured value plus a small margin rather than at a round number, so they function
as ratchets from the outset:

| Gate | Case | Budget | Measured |
|---|---|---|---:|
| `TestCheckNFRCollections` | every date variant | ≥ 10 000 checks/sec, p99 < 1 ms | 15 664–17 702 / 191–250 µs |
| `TestDateCheckAllocations` | parsing allocates nothing | ≤ 1 alloc/op for a third operand | +0.0 |
| `TestDateCheckAllocations` | clamping allocates nothing | ≤ 1 alloc/op over a non-clamping offset | +0.0 |
| `TestDateCheckAllocations` | date cost is linear in comparisons | ≤ 7 allocs/op per comparison after the first | +4.2 |
| `TestDateBenchCoverage` | no operator or vocabulary member is unmeasured | every member has a fixture; operators must be in the **asserted** set | 8/8 operators, 2 anchors, 7 units, 11 snaps |
