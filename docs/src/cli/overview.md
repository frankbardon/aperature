# CLI overview

**Audience:** operators and integrators driving Aperture from a shell.

`aperture` is the command-line face of the same decision engine the
[library](../library/overview.md), the RPC/HTTP server, and the MCP surface all
call. Every subcommand is a thin adapter: it parses flags, hand-wires the
`storage → engine → service` graph, makes exactly one call into the library, and
maps the result to output plus an exit code. There is no CLI-only behaviour — a
`check` from the shell resolves through the identical code path a `Check` RPC
does.

This part of the book is the *narrative* CLI guide, grouped by what each command
family is for. It links into the generated
[Command-Line Reference](../reference/cli.md), which is produced from the live
`urfave/cli` command tree and holds the authoritative, per-command flag tables.
When you want the exact flags for a command, follow the link on that command's
name — this guide never re-tabulates them, so the two never drift.

## Command families

| Family | Commands | What it does |
|---|---|---|
| [Decisions](decisions.md) | `check`, `enumerate`, `search`, `explain`, `identifiers` | Ask and audit access-control questions (read-only). |
| [Mutations](mutations.md) | `put`, `get`, `list`, `delete`, `bestow`, `revoke`, `impersonate` | Read and change the model — entities, grants, delegation, impersonation. |
| [Provisioning](provisioning.md) | `template`, `bulk` | Apply parameterized templates and transactional bulk grant/revoke. |
| [Portability](portability.md) | `export`, `import` | Serialize the whole model to a state file and apply it back. |
| [Attributes](attributes.md) | `attributes` | Inspect the host directories a rule reads `principal.*` / `account.*` from, read one, and drop cached bags. |
| [`serve`](serve.md) | `serve` | Run the HTTP + Twirp server and admin UI. |
| [`mcp`](mcp.md) | `mcp` | Serve the read-only MCP surface over stdio. |

## The embedded example model

Every command runs against a model. When you pass neither `--seed` nor `--store`,
Aperture loads a committed **example fixture** — a self-contained model for the
account `acme` — so the read commands below work with no setup. The fixture,
its grants, and its principals are walked through in
[First decision (CLI)](../getting-started/first-decision-cli.md). The examples in
this guide use that fixture unless they say otherwise, and never reference data
from any other account.

## Two ways to select a model

The commonly shared options — `--seed`, `--store`, `--account`, and
`--principal` — are defined **per command**, not as root persistent flags, and
they mean the same thing everywhere they appear. They are documented once in
[Global options](global-options.md); each family page below links back to that
page rather than re-explaining them.

## Related

- [Global options](global-options.md) — `--seed` / `--store` / `--account` / `--principal`.
- [Command-Line Reference](../reference/cli.md) — the generated per-command flag tables.
- [First decision (CLI)](../getting-started/first-decision-cli.md) — the example model, end to end.
- [Library overview](../library/overview.md) — the same engine, embedded in Go.
