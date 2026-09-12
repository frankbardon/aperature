# Aperture

Fine-grained access control engine for the frankbardon/* family. Aperture is a
library-first authorization system: the Go packages at the module root are the
deliverable, and the CLI, Twirp/HTTP, and MCP surfaces are thin translators over
a single decision engine.

## Status

Pre-release, but the core is in place. The decision engine (`Check` /
`Enumerate` / `Explain`, single and bulk) and its surfaces are built: the CLI,
the Twirp/HTTP server with an admin UI shell, the MCP stdio surface, and sqlite
plus in-memory storage behind one `Storage` interface. Work continues story by
story, but these surfaces exist today rather than being future work.

## Quick start

```bash
make build   # produce bin/aperture
make test    # run unit tests
make vet     # go vet
make lint    # vet + staticcheck (degrades gracefully when no usable analyser is installed)
```

## Documentation

The full reference manual is an mdBook published at
<https://frankbardon.github.io/aperture/> (once GitHub Pages is enabled). Build
and preview it locally with `make docs-serve` (source under `docs/src/`).

## What lives where

Library-first layout — public packages at the module root (mirrors Pulse), not
all-under-`internal/` (Orbit's shape):

- `cmd/aperture/` — thin `urfave/cli/v3` entry point; zero business logic.
- `errors/` — `APERTURE_*` coded error taxonomy with per-code message + fixups.
- `skills/` — embedded markdown skill pack, governed by the Update-Demand rule.
- `engine/`, `model/`, `storage/`, `identity/`, `scope/`, `provider/`, `rules/`,
  `auth/`, `audit/`, `mcp/` — the decision engine, resource model, storage, and
  the auth/audit/MCP surfaces.
- `internal/server/`, `internal/wire/rpc/` — the Twirp/HTTP server, admin UI
  shell, and the RPC contract (`service.proto`).

## Conventions

See [CLAUDE.md](CLAUDE.md) for the full conventions catalog. Highlights: every
failure is an `APERTURE_*` coded error; the library is the product and surfaces
are translators over one engine; rules consume the published
[Pulse](https://github.com/frankbardon/pulse) expression evaluator; any surface
change ships its `skills/` doc in the same PR (Update-Demand).

## License

Proprietary. © frankbardon.
