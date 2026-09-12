# Contributing to Aperture

Thanks for your interest in contributing.

## Getting Started

1. Create a branch off `main`.
2. Make your changes.
3. Run checks: `make test && make vet && make lint`.
4. Commit and push.
5. Open a pull request.

## Development Setup

- Go 1.26.1
- `CGO_ENABLED=0` — Aperture is pure-Go end to end.
- `staticcheck` (optional locally; CI installs it). `make lint` degrades to
  `go vet` only when no USABLE static analyser is on PATH — absent, or present
  but unable to start (a `golangci-lint` built with an older Go than this module
  targets refuses to run). The degrade is loud and says static analysis did not
  run; it never swallows a real finding.

```bash
make build   # build bin/aperture
make test    # run tests
make vet     # go vet
make lint    # vet + staticcheck
make fmt     # gofmt
```

## Code Conventions

See [CLAUDE.md](CLAUDE.md) for the full catalog. Highlights:

- **Library-first.** Business logic lives in the public root packages; surfaces
  (`cmd/aperture/`, Twirp, MCP) are thin translators over one engine. No
  business logic in `cmd/aperture/`.
- **Errors.** Every failure is an `APERTURE_*` coded error from `errors/`. Codes
  are SCREAMING_SNAKE, namespaced `APERTURE_*`, each with a message + fixup
  entry in `errors.Registry`. Pulse's `*CodedError` passes through verbatim.
- **Update-Demand.** Any change to a registered surface updates its `skills/`
  doc in the **same PR**, gated by named CI tests (see `skills/update-demand.md`).
- **No predecessor references** in names (no `Aperture2` / `LegacyX`).
- **Pulse is published, not vendored** — `go get github.com/frankbardon/pulse`;
  never add a `replace` directive.

## Pull Request Guidelines

- Keep PRs focused — one feature or fix per PR.
- Include tests for new functionality; no follow-up-PR commitments for test gaps.
- Update the relevant `skills/` doc when your change touches a registered surface.
- Run `make test && make vet && make lint` before submitting.

## Reporting Bugs

Include your config (redact secrets), Go version, OS, and the full `APERTURE_*`
code of any surfaced error.
