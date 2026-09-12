# MCP surface

**Audience:** authors integrating Aperture into an MCP client (an AI assistant or
agent runtime) as a tool provider.

Aperture exposes its decision API and model inspection as
[Model Context Protocol](https://modelcontextprotocol.io) tools. An MCP client
spawns `aperture mcp` as a subprocess, speaks MCP over stdio, and calls the
Aperture tools the same way it calls any other tool server.

## Read-only by construction

The MCP surface is **read-only**. Every tool calls a *read* or *decision* method
on the single `service.Service` facade — `Check` / `Enumerate` / `Explain`,
the what-if `Simulate`, and the `Get*` / `List*` inspectors. **No tool mutates.**
No tool name carries a mutating verb (`put` / `create` / `add` / `set` /
`delete` / `remove` / `update` / `write` / `bestow` / `revoke` / `grant` / …),
and a test (`mcp.TestNoMutatingTool`, mirrored at the wire level in the adapter)
fails if one ever does. The `aperture mcp` command deliberately wires the facade
with storage for inspection and what-if reads but **not** the gate, delegation,
or impersonation mutators, so the surface cannot write even by accident.

Everything on the surface is a thin translation onto the same facade the CLI and
the HTTP/Twirp surface drive — one decision engine, one code path. If a
behaviour is not described here it is governed by the facade and documented under
[The service facade](../library/service-facade.md).

## The tool catalog

The catalog is stable and ordered. Tool names are `aperture_`-prefixed,
snake_cased, and defined once in `mcp/toolmeta` (the single source of truth for
tool identity), so the SDK-free core and the SDK adapter never drift.

### Decision API (single + bulk)

| Tool | Purpose | Maps to |
|---|---|---|
| `aperture_check` | Decide whether a principal may take an action on an object, scoped to an account. Returns the verdict (allow/deny), a human-readable reason, and the deciding grant ids. **Fail-closed:** an operational failure renders as a *deny*, not an error; only an ill-formed question is an error. | `service.Check` |
| `aperture_check_batch` | Decide many `(account, principal, action, object)` questions in one round-trip; `results[i]` answers `queries[i]`. A single ill-formed query carries its error in that item without failing the batch. | `service.CheckBatch` |
| `aperture_enumerate` | List the object ids under a pattern a principal may act on — the inverse of `aperture_check`. Deny-overrides and specificity are honoured, so a denied object is never returned. Takes an optional `Fields` metadata filter and optional `References` edges (both below). | `service.Enumerate` |
| `aperture_enumerate_batch` | Enumerate accessible objects for many queries in one round-trip, aligned with the input queries. Each query carries its own `Fields` and `References`. | `service.EnumerateBatch` |
| `aperture_search` | Resolve a **name** to an object id: rank the objects a principal may act on by how well their metadata matches free text. Candidates are *decided before they are scored*, so the result is always a subset of `aperture_enumerate`'s for the same subject, action and pattern. Matching tolerates case, punctuation and a typo. A score **ranks; it never authorizes.** | `service.Search` |
| `aperture_search_batch` | Resolve many names in one round-trip, aligned with the input queries — for a sentence that names several things. | `service.SearchBatch` |
| `aperture_explain` | Return the full structured decision trace for one question: the expanded subject set, every grant considered with its per-grant outcome, which grants decided, and the final verdict. Use to understand *why*. | `service.Explain` |
| `aperture_explain_batch` | Return decision traces for many questions in one round-trip, aligned with the input queries. | `service.ExplainBatch` |

#### What a trace discloses about attributes

`ExplainOut` — and `SimulateOut` — are **type aliases** for `engine.Trace`, so
every one of these tools serializes the trace verbatim. That includes its
`Attributes` field: the `principal` and `account` bags the decision's rules were
evaluated against, **values included**.

That is a deliberate disclosure, and it reached this surface with no `mcp/` code
change at all — which is exactly why it is written down here. The two bags are
the subjects of the very request being explained (the principal and account named
in the query), so a trace tells the asker about their own decision and nothing
else; see [the attribute bags](../library/decision-api.md#the-attribute-bags).
Gate the MCP surface accordingly: an agent that may call `aperture_explain` on an
arbitrary `(account, principal)` pair can read that principal's attribute bag for
that account.

There is deliberately **no attribute tool**. Listing a slot returns the host's
whole user table, and MCP is where an agent doing that is least defensible, so
the directory read stays a system-tier CLI operation
([`aperture attributes`](../cli/attributes.md)) and never becomes a tool.

#### `aperture_enumerate`'s `Fields` filter

`Fields` is an **optional** object of metadata predicates that narrows the
listing — "which of the datasets alice may list carry brand Y?". Omit it to
filter nothing; it is not a required property, so an unfiltered enumerate is a
valid call for a schema-validating client.

```json
{
  "Account": "acme", "Principal": "alice", "Action": "list",
  "Pattern": "account:acme/**",
  "Fields": { "tier": "premium", "seats": 5, "brands": "brand:Y" }
}
```

The predicates are **ANDed**; a field the object does not carry **never
matches**; a **list-valued field matches by membership**; everything else is
**typed equality**, so `"5"` never matches `5`. The filter runs on objects the
principal is already allowed — it can only remove ids, never add one — and it is
applied **before** `Limit`, so `Limit: 10` returns the first ten *matches*.

The schema is reflected straight off `service.EnumerateQuery`, so the tool input
and the Go facade query are the same type; the semantics above are carried in the
property description an agent reads.

#### `aperture_enumerate`'s `References` edges

`References` is an **optional** list of reference edges that restricts the
listing to the identities a holder object's **declared reference field**
contains — "which brands belong to dataset X?". Omit it to restrict nothing; like
`Fields`, it is not a required property.

```json
{
  "Account": "acme", "Principal": "alice", "Action": "read",
  "Pattern": "account:acme/brand:*",
  "References": [{ "HolderID": "account:acme/dataset:x", "Field": "current_brands" }]
}
```

`HolderID` and `Field` are required properties of an edge; `HolderType` is
optional and, when given, must agree with `HolderID`'s last segment type.

**This is not the same thing as `Fields`, and an agent that treats them as
interchangeable will get the wrong answer.** `Fields` is a *filter* — "which
datasets contain brand Y?" — and works because the dataset holds the field.
`References` is a *dereference* — "which brands belong to dataset X?" — and
exists because a brand holds **no** field naming its datasets, so no filter can
express that question at all.

Several edges are **ANDed**, an edge composes with `Fields`, both apply **before
`Limit`**, and exactly **one hop** is taken.

The answers an agent must not over-read:

- A holder the principal **may not see** returns an **empty list, not an error**.
  Emptiness here means "nothing you may see", and it is deliberately
  indistinguishable from "you may not see the holder" — do not report it as a
  permission failure.
- An **absent** holder is `APERTURE_NOT_FOUND` only when it is inside the
  request's account and the principal is a member of it. Out of account, or for a
  non-member, the answer is empty.
- An edge naming a field that is **not a declared reference** is a loud
  `APERTURE_PROVIDER_REFERENCE_INVALID`, never an empty list — it means the
  deployment is not wired for that question, not that access was denied.

### What-if simulation (read-only, never persisted)

| Tool | Purpose | Maps to |
|---|---|---|
| `aperture_simulate` | Render the full decision trace for a question as it *would* be under a hypothetical overlay of principals, groups, permissions, grants, and memberships — **without writing anything**. The overlay is additive; an overlay entity with the same id as a stored one shadows it. Nothing is persisted and nothing is audited. | `service.SimulateExplain` |

### Model inspection

| Tool | Purpose | Maps to |
|---|---|---|
| `aperture_list_object_types` | List every object type with its declared action verb set. | `service.ListObjectTypes` |
| `aperture_get_object_type` | Fetch one object type by `name`. | `service.GetObjectType` |
| `aperture_list_permissions` | List every permission (object-type, action, scope-strategy, delegatable flag). | `service.ListPermissions` |
| `aperture_get_permission` | Fetch one permission by `id`. | `service.GetPermission` |
| `aperture_list_roles` | List every role (named permission bundles). | `service.ListRoles` |
| `aperture_get_role` | Fetch one role by `id`, including its permission bundle. | `service.GetRole` |
| `aperture_list_groups` | List every group (collections of principals usable as grant subjects). | `service.ListGroups` |
| `aperture_get_group` | Fetch one group by `id`, including member principal ids. | `service.GetGroup` |
| `aperture_list_principals` | List every principal (user or machine) with assigned role ids and identity strings. | `service.ListPrincipals` |
| `aperture_get_principal` | Fetch one principal by `id`, including assigned roles. | `service.GetPrincipal` |
| `aperture_list_grants` | List every grant stamped to an `account`. **Account-scoped:** a grant in another account is never returned. | `service.ListGrants` |
| `aperture_get_grant` | Fetch one grant by `id` (subject, permission, object pattern, effect, account). | `service.GetGrant` |

### Surface documentation

| Tool | Purpose | Maps to |
|---|---|---|
| `aperture_skills_list` | List the embedded skill docs describing how the decision, simulate, and inspection tools fit together. | `mcp/skills.List` |
| `aperture_skills_get` | Fetch the markdown body of a named skill doc (e.g. `mcp-surface`), the authoritative reference for driving the surface. | `mcp/skills.Get` |

Grants are account-scoped by design: `aperture_list_grants` requires an
`account` argument and never returns another account's grants, so the surface
cannot leak cross-account data.

### Name resolution, and the two things an agent must not assume

`aperture_search` exists because an agent receives questions in a person's words
and needs ids. Two properties are stated in the tool's own description, because
an agent has no other source of truth and both failure modes are silent:

1. **A score is not a permission.** It ranks candidates so the agent can pick one
   or ask which was meant. An id still goes through `aperture_check` before it is
   acted on.
2. **The result is already scoped.** There is nothing to filter afterwards, and
   an object missing from the result is one this principal may not see — not a
   matching failure to work around by enumerating the type and filtering locally.
   That workaround is precisely the bare-enumeration shape the surface exists to
   remove.

`Query` is required; there is no "match everything". For the whole entitled set
the tool is `aperture_enumerate`. Each match carries the object's full metadata
inline, so an agent should not follow up with per-id metadata reads to label what
it found.

## Inputs, outputs, and errors

Each tool carries an **input** and **output** JSON Schema (draft 2020-12),
reflected at package-init time from the typed Go contract in `mcp/schema.go` and
`mcp/contract.go`. The In/Out types alias the facade's own surface-neutral types
(for example `aperture_check`'s input is `service.Query` and its output is
`service.Result`), so the schema advertised to the client is exactly the shape
the facade decides on. A client discovers these schemas through the standard MCP
`tools/list` call — no Aperture-specific schema fetch is needed.

Argument handling and error surfacing:

- **Parameterless tools** (the `list_*` inspectors, `aperture_skills_list`)
  accept an empty argument blob.
- A **missing required argument** (for example `id` on a `get_*` tool) returns a
  plain validation tool-error, surfaced to the model so it can self-correct — it
  is intentionally *not* a coded error.
- A **coded facade error** (an `APERTURE_*` error) is returned verbatim; the
  adapter renders it as a structured `{code, message, details}` envelope rather
  than a flattened string.
- In the **batch** tools, a per-item failure is folded into that item's `error`
  field (a string); the rest of the batch is unaffected.

## The `mcp/` core and the SDK firewall

The core lives in the root `mcp/` package and is **SDK-free**: it imports no MCP
protocol SDK. Its files divide the surface cleanly:

| File | Role |
|---|---|
| `mcp/toolmeta/` | Leaf, pure-data package: the canonical `(name, description)` table for every tool. Imported by both the core and the adapter so the two never drift. |
| `contract.go` | The typed In/Out structs for every tool (aliasing the facade's `service` / `engine` / `model` types). |
| `schema.go` | Reflects an input + output JSON Schema for each contract type at init, carried as `json.RawMessage`. |
| `handlers.go` | One typed `func(ctx, *service.Service, In) (Out, error)` per tool; each calls exactly one facade read/decision method. |
| `tools.go` | Type-erases handlers into a `ToolDescriptor` catalog (`Tools(cfg)`), pairing each name with its reflected schemas and an `Invoke` closure. |

The core depends only on the decision facade (`service`), the domain types it
returns (`engine`, `model`), the coded-errors package (`errors`), the embedded
skill docs (`mcp/skills`, stdlib-only), and the schema reflector
(`google/jsonschema-go`). It carries the schema as `json.RawMessage`
specifically so a consumer can import the contract without pulling in any MCP
SDK.

### The adapter is the only SDK importer

The one package permitted to import the protocol SDK
(`github.com/modelcontextprotocol/go-sdk`) is the thin **`mcp/gosdk`** adapter.
`gosdk.Register(server, svc, cfg)` mounts the core's `ToolDescriptor` catalog
onto a caller-supplied go-sdk server via the low-level `Server.AddTool` path —
which accepts any value that marshals to a valid 2020-12 schema, exactly the
`json.RawMessage` the core emits. `Register` mounts onto the server it is given;
it never constructs, serves, or owns the server lifecycle. An embedder that
already runs its own go-sdk server can mount the full Aperture surface the same
way.

### `firewall_test.go` makes the guarantee load-bearing

`mcp/firewall_test.go` enforces the boundary so it is a fact, not an aspiration:

- **`TestMCPCore_NoSDKImport`** runs `go list -deps` over the core packages
  (`mcp`, `mcp/toolmeta`, `mcp/skills`) and fails if the MCP SDK module appears
  anywhere in their transitive dependency graph. Moving an SDK import into any
  core package makes this test fail with the exact offending dependency line.
- **`TestMCPCore_AllowedDepsReachable`** asserts the documented allow-list
  (`service`, `engine`, `model`, `errors`, `mcp/skills`, `mcp/toolmeta`,
  `google/jsonschema-go`) *is* reachable from the core, proving the firewall is
  inspecting a real, populated graph rather than passing vacuously.

The adapter (`mcp/gosdk`) is deliberately excluded from the firewall's package
list — the SDK is allowed there and nowhere else. This keeps the promise a client
author relies on: importing the Aperture MCP contract never drags in a protocol
SDK.

## Running it

`aperture mcp` hand-wires the read-only dependency graph
(storage → engine → service), constructs the go-sdk server, mounts the SDK-free
catalog through the `gosdk` adapter, and serves over **stdio** — the transport an
MCP client uses when it spawns Aperture as a subprocess:

```text
aperture mcp [--seed <path>] [--store <dsn>]
```

- `--seed` — path to a JSON/YAML seed model (defaults to the embedded example).
- `--store` — sqlite DSN for the backing store (defaults to in-memory).

With neither flag it serves the embedded example model over an in-memory store.
There are no other flags: the surface is read-only by construction, so it needs
no acting principal, account, or auth adapter.

On start it prints one line to **stderr** — stdout is reserved for the MCP
protocol:

```text
aperture mcp: serving read-only MCP surface over stdio
```

### Connecting a client

You normally don't launch `aperture mcp` interactively; an MCP client spawns it
over stdio. A minimal client configuration points at the binary and the model to
serve:

```json
{
  "mcpServers": {
    "aperture": {
      "command": "bin/aperture",
      "args": ["mcp", "--store", "./aperture.db"]
    }
  }
}
```

The server advertises its identity as `aperture` with the binary's build version
during the MCP `initialize` handshake, then answers `tools/list` with the full
catalog above and `tools/call` for each tool.

## Related

- [`aperture mcp`](../cli/mcp.md) — the command-level reference and flags.
- [The service facade](../library/service-facade.md) — the one code path behind every tool.
- [Decision API](../library/decision-api.md) — the same check/enumerate/explain questions from Go.
- [RPC / HTTP overview](rpc-overview.md) — the read/write Twirp surface served by `aperture serve`.
