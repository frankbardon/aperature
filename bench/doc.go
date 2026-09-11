// Package bench holds Aperture's performance benchmark suite and the gated NFR
// assertion that proves the Check Success Metric (FR-31): p99 cached Check
// < 1ms AND >= 10k checks/sec/instance, measured on a sizable seeded model with
// decision audit both ON and OFF.
//
// Two layers, deliberately separated so the default unit-test path stays fast
// and never flakes on a loaded CI machine:
//
//   - The Benchmark* functions are INFORMATIONAL. `make bench` runs them with
//     -benchmem and prints ns/op + allocs/op; the throughput / latency
//     benchmarks additionally report a computed checks/sec and p99 via
//     b.ReportMetric, so a human (or a perf dashboard) reads the real numbers.
//
//   - TestCheckNFR is the HARD assertion: it measures p99 over many cached
//     Checks and asserts the targets with comfortable headroom. It is GATED —
//     it self-skips unless APERTURE_BENCH_ASSERT=1 is set, and also skips under
//     `go test -short` — so a wall-clock assertion never gates the default
//     `make test`. See docs/benchmarks.md for the methodology.
//
//     Its siblings (TestCheckNFRCollections, TestCheckNFRAttributes,
//     TestCheckNFREnumerateBound) are gated identically and are NAMED so that
//     the one documented invocation — `APERTURE_BENCH_ASSERT=1 go test -run
//     TestCheckNFR ./bench/`, whose -run pattern is an unanchored regexp —
//     catches every one of them with no command change. Adding a gate case
//     reachable only by a second, undocumented command adds a case that will
//     not be run.
//
//     TestCheckNFREnumerateBound is the one that does NOT assert a wall clock:
//     a bound-sized rule-backed Enumerate is milliseconds by design, so it
//     asserts a RATIO — per-candidate cost at a raised bound against the same
//     enumeration at the default bound — which divides machine speed and
//     baseline drift out of the measurement.
//
// The fixture (buildModel) is intentionally non-trivial: many accounts,
// principals, roles, groups, and grants, with overlapping wildcard scopes and
// deny-overrides carve-outs, so grant resolution exercises a real candidate set
// rather than a three-grant toy.
package bench
