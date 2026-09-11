# Build, test & lint gates

**Audience:** contributors preparing a change for review.

Run the local gates before every PR. They are fast, pure-Go, and require no
services. CI runs the same commands plus the non-skippable gate tests described
below.

## The `make` targets

| Target | Runs | Notes |
|---|---|---|
| `make build` | `go build` → `bin/aperture` | Default goal. `CGO_ENABLED=0`, `-ldflags="-s -w" -trimpath`. |
| `make test` | `go test ./...` | The full unit/integration suite. Does **not** include the NFR benchmark gate (see below). |
| `make fmt` | `go fmt ./...` | Formats the tree. |
| `make vet` | `go vet ./...` | Standard vet checks. |
| `make lint` | `go vet` + a static analyser | Runs `staticcheck` if present, else `golangci-lint`, else prints a notice and runs vet only. CI installs `staticcheck` explicitly, so lint is real in CI even though it degrades locally. |

A minimal pre-PR loop:

```bash
make fmt
make test
make vet
make lint
```

### The benchmark / NFR gate is separate

`make test` deliberately excludes the hard performance assertion so a loaded CI
machine never flakes the build. The informational benchmark suite and the gated
NFR test live under `bench/`:

```bash
make bench                                              # informational: ns/op, p99, checks/sec
APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/   # the hard NFR assertion
```

`TestCheckNFR` asserts p99 cached `Check` < 1ms **and** ≥ 10k checks/sec/instance.
See [Performance & NFR](../operations/performance.md) and the committed numbers in
`docs/benchmarks.md` for methodology.

### So is the real-Postgres integration test

`make test` passes with **no database present** — CI runs with no service
containers, so the [SQL provider](../concepts/providers.md#worked-example-sqlprovider)
is proved against a hand-rolled fake driver returning canned values. One test
talks to a real Postgres, and it is gated the same way the NFR assertion is:

```bash
APERTURE_PG_INTEGRATION=1 \
APERTURE_PG_DSN='postgres://user:pass@localhost:5432/db?sslmode=disable' \
go test -run TestPostgresIntegration ./seed/
```

Ungated it **skips**. Gated with a missing or empty `APERTURE_PG_DSN` it
**fails** rather than skipping — asking for the integration run and silently not
getting one is the outcome a gate must never produce. It creates and drops its
own table, so point it at a scratch database, and **never put a DSN in a file**.

It exists because a fake cannot prove the two things only the real driver can:
that pgx is linked into a `CGO_ENABLED=0` binary and actually connects, and that
a real Postgres result set lands in the value model the way the mapping table
claims.

## The non-skippable CI gates

Five gate tests protect Aperture's error taxonomy and its Update-Demand rule.
They are ordinary Go tests, so `make test` already runs all of them — you do not
need a special command. To run them in isolation:

```bash
go test ./errors/ -run 'TestCodesHaveFixups|TestRegistryHasNoOrphans|TestCodesAreScreamingSnakeNamespaced'
go test ./skills/ -run 'TestUpdateDemandDocPresent|TestEverySkillHasFrontmatter'
```

| Gate | Enforces | Trips when you… |
|---|---|---|
| **`TestCodesHaveFixups`** | Every `APERTURE_*` code has a `Registry` entry with a `Message` and at least one `Fixup` (or `FixupNotApplicable: true`). | Add an error code without its remediation metadata. |
| **`TestRegistryHasNoOrphans`** | The `Registry` contains nothing absent from `AllCodes`. | Add a `Registry` entry but forget to list the code in `AllCodes` (or vice versa). |
| **`TestCodesAreScreamingSnakeNamespaced`** | Every code is `SCREAMING_SNAKE` and `APERTURE_`-prefixed. | Name a code `apertureFoo` or drop the `APERTURE_` prefix. |
| **`TestUpdateDemandDocPresent`** | The Update-Demand seed doc `skills/update-demand.md` exists with frontmatter. | Delete or de-frontmatter the rule's own documentation. |
| **`TestEverySkillHasFrontmatter`** | Every `skills/*.md` has a `name` (matching its file stem) and a `description`. | Add or edit a `skills/` doc without valid YAML frontmatter. |

The first three live in `errors/codes_test.go`; the last two enforce the
[Update-Demand rule](../internals/update-demand.md) over the `skills/` surface
docs. None of them can be skipped — a red gate blocks the PR.

Alongside them run the **registry-parity** gates, which diff a table in Go
against its mirror somewhere else and **fail rather than skip** when the mirror
is missing: the rule-editor contract tests in `rules/` (Go AST ↔ the served
`rules-serializer.js`) and `TestDriverValueMappingTableMatchesTheTypeSwitch` in
`sqlprovider/`, which parses `values.go` with `go/ast` and fails if the
driver-value type switch and `mappedDriverTypes` disagree. Adding a case to one
half and not the other is build-red on purpose. `CLAUDE.md` carries the full
change → required-update → enforcing-test table.

### The book's own links are gated

Two more ordinary Go tests, in `internal/docsgate`, keep this book honest. They
need no network and no mdBook, so `make test` already runs them:

```bash
go test ./internal/docsgate/
```

| Gate | Enforces | Trips when you… |
|---|---|---|
| **`TestEveryBookLinkResolves`** | Every relative link in `docs/src` names a file that exists **and** a heading anchor that page actually emits. | Link to a page that moved, or to a `#section` mdBook does not generate. |
| **`TestEveryBookPageIsInTheSummary`** | `SUMMARY.md` and the `docs/src` tree agree in **both** directions. | Add a page without listing it (mdBook would not build it), or list a chapter with no file behind it. |

The anchor half is the one worth knowing about, because a wrong anchor is
**silent**: the file resolves, the browser finds no such id, and the reader lands
at the top of the page as if nothing happened. That is not hypothetical — two
links in this book pointed at `#the-acting-principal-principal` for an anchor
mdBook never emitted, and they were found by accident.

mdBook derives an anchor by keeping alphanumerics, `_` and `-`, lowercasing
them, turning whitespace into hyphens, and **dropping** everything else. Dropping
rather than replacing is the part that surprises people, and it never collapses
a run of hyphens:

| Heading | Anchor |
|---|---|
| ``## The acting principal: `--principal` `` | `#the-acting-principal---principal` |
| `# Build, test & lint gates` | `#build-test--lint-gates` |
| ``## The `Filter.Fields` contract`` | `#the-filterfields-contract` |

So a heading naming a CLI flag keeps the flag's dashes and gains one more from
the space in front of it. Derive the anchor rather than guessing it, or copy it
out of the built page — `make docs` writes `docs/book`, whose `<h2 id="…">`
attributes are the ground truth the gate's derivation was validated against.

The generated pages are covered like any other. If the gate fires on
[`reference/cli.md`](../reference/cli.md) or
[`reference/error-codes.md`](../reference/error-codes.md), fix the **generator**
under `internal/docsgen` and re-run `make docs-gen` — both files say `DO NOT
EDIT` and mean it. And if the gate fires on a link you believe is fine, fix the
link or fix the heading: there is no exception list, deliberately.

## What CI does not gate

The generated reference pages —
[`docs/src/reference/error-codes.md`](../reference/error-codes.md) and
[`docs/src/reference/cli.md`](../reference/cli.md) — have **no CI drift gate**.
Nothing fails if they go stale. Regenerating them is a manual step you own; see
[Regenerating artifacts](regenerating-artifacts.md).
