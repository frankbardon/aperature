# Performance & the NFR

Aperture's decision hot path carries a hard success metric (FR-31):

> **p99 cached `Check` < 1 ms** *and* **≥ 10 000 checks/sec/instance.**

This chapter summarizes how that budget is measured and asserted. The full
methodology, the optimization pass, and the committed hardware numbers live in
the repository at **`docs/benchmarks.md`** (repo root, alongside the book — it is
not part of the mdBook source tree, so read it directly in the repo or on GitHub).

## The benchmark suite (`make bench`)

The suite lives in the `bench/` package. It seeds a **sizable** authorization
model — not a three-grant toy — and drives the full decision facade
(`service.Service.Check`), so the numbers reflect what a real surface pays.

The fixture seeds 8 accounts, 60 roles, 60 groups, and 480 principals, with
overlapping wildcard allows, more-specific deny carve-outs, and 60 concrete
document grants per account. The representative cached `Check` resolves a
six-subject subject set to roughly 73 applicable grants at differing
specificities, so deny-overrides and the specificity tiebreak genuinely run
rather than short-circuiting.

```bash
make bench     # go test -run '^$' -bench=. -benchmem ./bench/
```

`make bench` is **informational** — it prints, but never asserts:

| Benchmark | Reports |
|---|---|
| `BenchmarkCheckCachedAuditOff` / `…AuditOn` | single cached `Check` ns/op, allocs/op, and a computed `p99-ns` |
| `BenchmarkCheckThroughputAuditOff` / `…AuditOn` | sustained parallel throughput as `checks/sec` |
| `BenchmarkEnumerateBounded` | an `Enumerate` on an engine that configures no bound; asserts the result never exceeds `engine.DefaultEnumerateLimit` |
| `BenchmarkEnumerateRuleBacked/candidates-N` | a **rule-backed** `Enumerate` swept across candidate-set sizes on the default bound, reporting a derived `ns/candidate` and the returned `ids` count |
| `BenchmarkEnumerateRuleBacked/candidates-N/bound-M` | the same sweep at a bound **raised** through `engine.WithEnumerateLimit` — the rows that answer ["what does raising the bound cost?"](#the-enumeration-bound), from the same invocation as the default ones |
| `BenchmarkEnumerateRuleBackedRuleEval` | the same count of `rules.Engine.Selected` calls with no engine around them, separating the rule half of the enumeration cost from the decision half |

The **audit toggle** is the axis: audit-off is the `s.audit == nil` path;
audit-on wires a sampled (1 %), asynchronous `audit.Recorder` — the production
shape where decision audit sits off the critical path.

## The hard NFR gate (`TestCheckNFR`)

Wall-clock assertions are environment-sensitive, so the **hard** gate is a test
that is **off by default** and never runs in the routine `make test`. It
self-skips under `go test -short` **and** skips unless `APERTURE_BENCH_ASSERT=1`
is set. Run it explicitly on a known-unloaded machine:

```bash
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/
```

Inside the gate:

- **p99** — time 100 000 cached `Check`s on a warm engine, split into 10 rounds
  of 10 000; sort each round's per-op latencies and take its 99th percentile;
  assert the **lowest** round's p99 is `< 1 ms`.
- **throughput** — run 200 000 cached `Check`s, split into 50 rounds of 4 000;
  time each round; assert the **fastest** round's rate is `≥ 10 000 checks/sec`
  (a conservative single-goroutine floor; a real instance parallelises well
  above it).
- both are run with audit **on** and **off**.

The rounds **partition** the sample budget rather than multiplying it — the gate
performs the same total number of `Check`s a single contiguous measurement did —
and taking the best round is what widens an *absolute* wall-clock threshold's
margin on a machine that is doing other things. It widens the margin rather than
conferring immunity: measured A/B with the same competing load held across both
arms, the contiguous measurement cleared the 10,000 floor by 1.10× where
best-of-rounds cleared it by 1.31×, and past roughly 10x core oversubscription
both fail, because no window is uncontended and there is no clean round to take
the best of. Contention is one-sided: it only ever makes
a window slower, never faster. Measured as one contiguous window, pressure
anywhere in that window drags the whole average under the floor, and the gate
then reports a fact about the machine rather than about Aperture. Measured as
rounds, a contended window costs its own round and the best round still reports
what a decision costs when it has a core — which is the quantity the NFR is
about. Two round counts rather than one because throughput is a *rate* (many
short windows, so one is likely to land uncontended) and p99 is a *percentile*
(fewer, larger windows, or the estimator itself goes noisy).

The trade is explicit: a regression that is **intermittent** in exactly the shape
load noise has — slow in most windows, fine in one — can now pass. A **uniform**
one still fails, because it misses the target in every round. The failure
messages say which case you are looking at ("that is the BEST of *N* rounds, so
it is not one noisy window"), so a red gate is never dismissible as a bad second.

`TestCheckNFR` is the regression guard: it fails if p99 ever crosses 1 ms or
throughput drops below the floor. Because it is gated it never flakes the default
build, but it is wired and runnable on demand and in a dedicated CI job/cron
where the runner is known to be idle.

`-run` is an unanchored regexp, so that one invocation also picks up
`TestCheckNFRCollections`, `TestCheckNFRAttributes` and
`TestCheckNFREnumerateBound` — the last of which guards the
[enumeration bound](#the-enumeration-bound) and is the one threshold in the suite
that is **not** a wall clock. It holds a rule-backed `Enumerate` at a *raised*
bound to a **ratio** against the same enumeration at the default bound
(per-candidate cost within 1.5×), measuring both arms on the same machine in the
same second. An 11.6 ms enumeration is correct by design at a bound of 2 000, so
no absolute number could be asserted there; what must not change is that the cost
stays **linear in the bound**.

## Committed numbers

Measured on an Apple M1 Max (`go test -benchtime=2s`). Absolute numbers are
hardware-dependent; the durable signal is the **headroom** and the **allocation
profile**.

| Metric (cached `Check`) | audit off | audit on |
|---|---|---|
| mean latency | ~66 µs/op | ~70 µs/op |
| allocations | 34 allocs/op | 34 allocs/op |
| p99 (gated, 100k samples) | ~0.275 ms | ~0.265 ms |
| throughput (single goroutine) | ~15 100 checks/sec | ~14 700 checks/sec |
| throughput (parallel benchmark) | ~20 000 checks/sec | ~30 000 checks/sec |

Both targets are met with comfortable headroom — p99 sits ~3.6× under the 1 ms
ceiling, and even the single-goroutine throughput clears the 10 k/s floor by
~1.5× before any parallelism. **Audit-on does not regress the target:** sampling
is a single call on the un-kept path and the kept event is built lazily and
written asynchronously, so the decision never blocks on audit.

## The enumeration bound

The numbers above are `Check`, which asks about **one** object. `Enumerate` asks
about a population, and every candidate on the way to the result costs a full
deny-overrides evaluation — a `Check`'s worth of work each. The **enumeration
bound** is what caps that. It governs how much work a single decision may do, not
merely how long a list it may print, and it is the one performance number an
operator sets.

### Setting it

| | |
|---|---|
| Flag | `--enumerate-limit` |
| Environment | `APERTURE_ENUMERATE_LIMIT` |
| Default | `1000` |
| Precedence | **flag > env > default** |

```bash
bin/aperture serve --enumerate-limit 2000
APERTURE_ENUMERATE_LIMIT=2000 bin/aperture serve
```

It configures the **process, not the server**. `check`, `enumerate`,
`identifiers`, `explain` and `mcp` carry the same flag and read the same
variable, so one binary can never answer `2000` over HTTP and `1000` at the
shell. (`aperture attributes` is the deliberate exception: it builds the same
decision stack, but everything it prints is paged by the attribute registry's own
cap, which this bound never governs.)

A value that is not a whole number **greater than zero** — `banana`, `0`, `-5` —
fails the command with `APERTURE_CONFIG_INVALID` naming the setting and the value
it rejected, rather than quietly serving `1000`. Under `serve` that refusal
happens **before the store is opened**, so a rejected configuration leaves no
database file behind. To get the default, omit the setting.

What the number means at request time: a request limit above it is clamped
**down** to it, a request limit at or below it is honoured as asked, and a
non-positive request limit *receives* it. The same value also bounds the scope
member gather, so the gather and the result cap are one number rather than two
that could disagree. See [Deployment](deployment.md#configuration-precedence),
[Global options](../cli/global-options.md) and
[Decisions](../cli/decisions.md#--limit-and-the-deployments-ceiling).

### What raising it costs

Measured with `BenchmarkEnumerateRuleBacked` on the **worst case on purpose**:
one account-wide `inclusive;rule=…` grant, a scalar comparison over one metadata
field, every candidate selected (a rejected candidate is cheaper, because it
skips the second evaluation), audit off. Apple M1 Max (10 cores), go1.26.5
darwin/arm64, `-benchtime=1s -count=3`, medians. `bound` is the value passed to
`engine.WithEnumerateLimit`; `—` means unconfigured.

| candidates | bound | ns/op | ids | ns/candidate | allocs/op | B/op |
|---:|---:|---:|---:|---:|---:|---:|
| 10 | — | 44 359 | 10 | 4 436 | 496 | 39 311 |
| 100 | — | 436 078 | 100 | 4 361 | 4 679 | 385 291 |
| 1 000 | — | 5 058 189 | 1 000 | 5 058 | 46 137 | 3 870 603 |
| 2 000 | — | 4 733 331 | 1 000 | 4 733 | 46 141 | 3 904 836 |
| 1 000 | 2 000 | 4 543 591 | 1 000 | 4 544 | 46 131 | 3 870 590 |
| 2 000 | 2 000 | 11 565 568 | 2 000 | 5 783 | 92 188 | 7 816 610 |
| 4 000 | 2 000 | 9 170 151 | 2 000 | 4 585 | 92 168 | 7 876 370 |

Four things to take from it:

- **The cost is linear in the bound, not worse.** 1 000 → 2 000 returned ids is
  2.00× the allocations and 2.02× the bytes, and `ns/candidate` stays flat
  (4 361–5 783) across three orders of magnitude of population *and* across both
  bounds. Doubling the bound doubles the worst case and no more.
- **Budget roughly 4.6 µs, 3.9 KB and 46 allocations of transient garbage per id
  the bound allows.** A bound of 2 000 is a ~9 ms, ~7.8 MB enumeration; a bound
  of 10 000 is a ~46 ms, ~39 MB one. Even at the default, a rule-backed
  `Enumerate` is ~1 000× a cached `Check` — it is an *interactive* operation, not
  a hot-path one, and raising the bound scales that latency with it.
- **Headroom a deployment does not use costs nothing.** 1 000 candidates at a
  bound of 2 000 is indistinguishable from the same population unconfigured —
  46 131 vs 46 137 allocations, the same `B/op` to four
  digits. Configuring room you have not grown into is not paid for.
- **A raised bound clamps exactly as the default one does.** 4 000 candidates at
  a bound of 2 000 costs what 2 000 at that bound costs; past the bound the extra
  objects are never visited. The worst case stays a constant a host can budget
  for — it is simply a constant the operator now chooses.

**Read the ratios, not the absolutes.** Wall-clock figures here move with
whatever else the measuring machine is doing — the same benchmark measured 2 348
ns/eval on a loaded machine and 1 500 ns/eval on a quiet one, before any code
changed. (The one durable
change since these were first recorded is ~3 extra allocations per rule
evaluation, added deliberately in 2026-08 for the attribute floor bags; the
repository's `docs/benchmarks.md` accounts for it.) Treat the
per-id figures as a shape to size a deployment with, not as a performance promise
for your hardware — and measure your own with `make bench` before committing to a
number. The full methodology and the rest of the sweep are in the repository file
`docs/benchmarks.md`.

### Truncation is a log line, not a result field

When an enumeration comes back holding **exactly** its effective bound, the
engine emits a WARN naming the bound that was hit:

```
WARN engine: enumeration returned exactly its bound; the result may be truncated
  bound=2000 configured_bound=2000 requested_limit=0
  account=acme action=read pattern=account:acme/**
```

Read it literally: it is a **hint, not an assertion**. A complete set that
happens to be exactly that size is indistinguishable from a truncated one, so the
line never claims anything was dropped. `Enumerate` returns `([]string, error)`
and grows no truncation flag, which means a **caller cannot tell a truncated
result from a complete one** — the signal is the operator's, and the response is
to raise the bound and ask again. A result below the bound logs nothing.

### Two things the bound deliberately does not cap

Both are accepted consequences, not gaps:

1. **A direct Go embedder gets the limit it asks for.**
   `provider.Registry.List` honours a *positive* limit verbatim, however large —
   `provider.DefaultListLimit` (1000) is only what a non-positive limit means. A
   host calling `reg.List(ctx, t, pat, 1_000_000)` is asking deliberately and is
   answered. This is not a hole: every *network* surface reaches enumeration
   through the engine's clamp, and the library is sharp-edged on purpose.
2. **`provider.AttributeRegistry.Enumerate` is uncapped.** It is a system-tier
   directory read — pulling a whole directory is the point — and it is protected
   by the **administrator authority it demands**, not by a number. That is why
   `aperture attributes` carries no `--enumerate-limit`: this bound never
   governed what it prints.

The full statement of both lives in the
[decision API](../library/decision-api.md#enumerate) and in
`skills/decision-api.md`; this page restates them because they are the two places
where "the bound caps enumeration" is not the whole truth.

### The honest caveat: batching against SQLite

Raising the bound costs more under `EnumerateBatch` against a file-backed SQLite
store than the table above suggests. The SQLite pool is capped at a **single
connection** (writes serialize cleanly under SQLite's single-writer model, and
reads pay for it), and `EnumerateBatch` is today a batch in call shape only — it
runs each request in turn, re-resolving membership, the subject set and the grant
query per item. Store round trips, not the candidate walk, dominate: the same
enumeration costs ~0.026 ms/pattern against the in-memory store and ~0.504
ms/pattern against file-backed SQLite, and end-to-end latency is linear in
concurrent callers.

That is **not fixed by this bound's configurability** and is out of scope here;
it is tracked as issue #13. Until it moves, size a raised bound against your
batch fan-out and your concurrency, not against a single enumeration in
isolation — and note the raised-bound gate uses the in-memory store, so it does
not exercise this amplifier at all.

## Where the headroom came from

The optimization pass (recorded in `docs/benchmarks.md`) found the dominant
per-`Check` allocator: the coverer re-parsed each grant's object pattern on every
candidate of every `Check`, so a principal resolving ~73 grants paid ~73 fresh
pattern parses. A concurrency-safe parsed-pattern cache in the engine
(`engine/patterncache.go`) removed the churn — a parsed pattern is immutable and a
pure function of its source, so a cache hit returns exactly what a fresh parse
would and **decision semantics are unchanged**. Effect: **172 → 34 allocs/op**
(~5× fewer), with the re-parse GC pressure gone from the hot path.

The change was measure-first: caches that already bound their own cost (the
compiled-rule cache, the provider metadata cache) were left untouched absent a
benchmark showing a win.

## Related

- Repository file `docs/benchmarks.md` — the authoritative methodology, the
  optimization write-up, and the latest committed numbers.
- [Deployment](deployment.md) — running the instance whose throughput these
  numbers describe, and where `--enumerate-limit` sits among the other settings.
- [Global options](../cli/global-options.md) and
  [Decisions](../cli/decisions.md) — the bound at the command line, and how a
  request's `--limit` interacts with it.
- [Decision API](../library/decision-api.md) — `WithEnumerateLimit`, the
  on-the-bound warning, and the two uncapped reads in full.
- [Rules engine](../concepts/rules.md), [Providers](../concepts/providers.md) —
  the caches referenced by the measure-first note.
