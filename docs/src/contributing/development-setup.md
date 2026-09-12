# Development setup

**Audience:** new contributors setting up a local Aperture checkout.

Aperture is **library-first** and **pure-Go end to end**. There is no code
generation on the critical build path, no CGO, and no external services required
to build, test, or run the binary. If you can build Go, you can build Aperture.

## Toolchain

| Tool | Version | Why |
|---|---|---|
| Go | **1.26.1** | The module targets this toolchain (`go.mod`). |
| `CGO_ENABLED` | **0** (hard requirement) | Aperture consumes only pure-Go dependencies — it uses `expr-lang/expr` for rules, `modernc.org/sqlite` for storage, and has **no Pulse dependency**. CGO stays off so builds are static and cross-compile cleanly. The `Makefile` exports `CGO_ENABLED=0` for every target. |
| mdBook | **v0.5.2** | Only needed to build the documentation site (this book). Not required for the Go build or tests. |

Optional, only if you want the full local gate:

| Tool | Purpose |
|---|---|
| `staticcheck` | Static analysis for `make lint`. CI installs it; locally `make lint` degrades to `go vet` when no usable analyser is on PATH — absent, or present but unable to start. Install it with `go install honnef.co/go/tools/cmd/staticcheck@latest`. |
| `protoc` + `protoc-gen-go` + `protoc-gen-twirp` | Only to regenerate the RPC layer with `make proto`. The generated code is committed, so you do not need these for a normal build. |
| `node` | Only to rebuild the vendored Rete.js bundle with `make vendor-rete`. Never required by `build`/`test`/CI. |

## Building

Clone, then build the binary:

```bash
git clone https://github.com/frankbardon/aperture
cd aperture
make build          # produces bin/aperture (CGO off, -ldflags="-s -w" -trimpath)
```

`make build` is the default goal, so a bare `make` does the same thing. Run the
freshly built binary with `make run`, or invoke `bin/aperture` directly.

```bash
make run            # build, then execute bin/aperture
make clean          # remove the bin/ directory
```

Because the build is pure-Go, `go build ./...` also works if you prefer the raw
toolchain — but `make build` is the supported entry point (it sets the release
flags and keeps `CGO_ENABLED=0`).

## Building the documentation

The book you are reading is built with mdBook. Install mdBook v0.5.2, then:

```bash
make docs           # mdbook build docs → docs/book/ (gitignored output)
make docs-serve     # live-reload preview, opens a browser
make docs-clean     # remove docs/book/
```

Mermaid diagrams render client-side from vendored JavaScript
(`docs/mermaid.min.js` + `docs/mermaid-init.js`) — there are **no mdBook
preprocessor plugins** to install. The built output under `docs/book/` is
gitignored; never commit it.

## Next steps

- [Build, test & lint gates](gates.md) — the commands and CI gates to run before
  a PR.
- [Style & testing conventions](conventions.md) — the house rules your code must
  follow.
- [Regenerating artifacts](regenerating-artifacts.md) — when and how to run
  `make docs-gen`, `make proto`, and `make vendor-rete`.
