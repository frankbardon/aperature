---
name: mcp-surface
description: Aperture's read-only MCP surface — the decide, search, simulate, and inspect tools an agent drives over stdio, including name-to-id resolution whose results are already entitlement-scoped and whose scores rank but never authorize, what an explain/simulate trace discloses about a principal's and an account's attribute bags (values included, via the ExplainOut = engine.Trace alias) and why there is no attribute-directory tool, with the SDK-free-core + gosdk-adapter + import-firewall house pattern that keeps the protocol SDK out of the core.
applies_to: [mcp, service, engine, what-if]
---

# Aperture MCP surface

Aperture exposes a **read-only** Model Context Protocol surface: an agent may
**decide**, **simulate**, and **inspect**, but **never mutate**. Every tool calls
the single `service.Service` facade's read and decision methods; the facade's
mutators (Put*/Delete*, Bestow/Revoke, impersonation) are not surfaced, and a
test enumerates the registered tools to assert none carries a mutating verb.

Serve it with `aperture mcp` (stdio) — the transport an MCP client uses when it
spawns Aperture as a subprocess. It builds the SAME decision stack `serve` and
the one-shot CLI commands build (`internal/cli.buildDecisionStack`): the rules
engine, the object lister, and the object-metadata source. An agent therefore
gets the same verdict as every other surface — and a filtered
`aperture_enumerate` works at all.

## Tools

### Decide (single + bulk)
- `aperture_check` — verdict + reason + deciding grant ids for one
  (account, principal, action, object). Fail-closed: an operational failure
  renders as DENY, never an allow-on-error.
- `aperture_check_batch` — many questions in one round-trip, results aligned with
  the input queries; one bad query never fails the batch.
- `aperture_enumerate` — the object ids under a pattern a principal may act on
  (the inverse of check); every id is one check would allow. Takes an OPTIONAL
  `Fields` object of metadata predicates: ANDed, a field the object does not
  carry never matches, a list-valued field matches by membership, everything else
  is typed equality (`"5"` never matches `5`). It only ever removes ids from the
  allowed set, and it is applied BEFORE `Limit`, so `Limit` counts matches. The
  schema is reflected off `service.EnumerateQuery`, where `Fields` carries
  `omitempty` so an unfiltered enumerate stays representable. Also takes an
  OPTIONAL `References` list of reference edges (`HolderID` + `Field`, and an
  optional `HolderType`) restricting the result to what a holder object's
  DECLARED reference field names — "which brands belong to dataset X?". That is a
  DEREFERENCE, not a filter: `Fields` answers the mirror image ("which datasets
  contain brand Y?") because that side holds the field. Edges are ANDed, compose
  with `Fields`, apply before `Limit`, and take exactly one hop. A holder the
  principal may not see is an EMPTY list and NOT an error — do not report it as a
  permission failure; an undeclared field is a loud
  `APERTURE_PROVIDER_REFERENCE_INVALID`, which means the deployment is not wired
  for that question rather than that access was denied.
- `aperture_enumerate_batch` — bulk enumerate, aligned with queries; each query
  carries its own `Fields` and `References`.
- `aperture_search` — resolve a NAME to an object id. Ranks the objects a
  principal may act on by how well their metadata matches free text, best first.
  This is the tool for a question that arrives in a person's words ("what is
  Nike's score?") where every step after it needs an id.

  Three things to hold onto:

  1. **A score is not a permission.** It ranks candidates so you can pick one or
     ask the user which they meant. Still `aperture_check` an id before acting on
     it, and never present a high score as authorization.
  2. **The result is already scoped.** Candidates are DECIDED before they are
     scored, by the same walk `aperture_enumerate` uses, so what comes back is
     always a SUBSET of what `aperture_enumerate` would return for the same
     subject, action and pattern. You never need to filter it afterwards, and an
     object missing from it is an object this principal may not see — not a
     matching failure to work around by enumerating and filtering yourself.
  3. **`Query` is required.** There is no "match everything" — for the whole
     entitled set, use `aperture_enumerate`.

  Matching is case- and punctuation-insensitive ("Nike, Inc." matches "nike inc")
  and tolerates a typo or a transposition. Only TEXT is matched; match a number,
  bool or date through `Fields` instead. Aperture has no notion of a "label" —
  every field holding text is searched unless `MatchFields` names some, and each
  match reports the `Field` and `Value` that produced it, so you can tell a hit
  on an alias from a hit on a display name. `Fields` and `References` mean what
  they mean on `aperture_enumerate` and COMPOSE with the query: the predicate and
  the edge narrow, the query ranks. `MinScore` drops weak matches; `Limit` caps
  the returned MATCHES (the best N, not the first N found). Each match carries
  the object's full `Metadata` inline, so do NOT follow up with a per-id metadata
  read to label what you found.
- `aperture_search_batch` — bulk search, aligned with queries; use it when one
  sentence names several things (a brand and a category).
- `aperture_explain` — the full decision trace: subject set, every grant
  considered with its per-grant outcome, the deciding grants, the verdict.
- `aperture_explain_batch` — bulk explain, aligned with queries.

**What a trace discloses about attributes.** `ExplainOut` and `SimulateOut` are
type ALIASES for `engine.Trace`, so these tools serialize it verbatim — including
its `Attributes` field, which carries the `principal` and `account` bags the
decision's rules were evaluated against, VALUES INCLUDED. That disclosure reached
this surface with no `mcp/` code change at all, which is why it is stated here.
It is deliberate: the two bags are the subjects of the very request being
explained, so a trace tells the asker about their own decision and nothing else.
Gate accordingly — an agent free to explain any (account, principal) pair can
read that principal's bag for that account.

There is deliberately NO attribute tool. Listing a slot returns the host's whole
user table, and MCP is where an agent doing that is least defensible, so the
directory read stays a system-tier CLI operation (`aperture attributes`) and
never becomes a tool. `aperture_simulate` cannot inject a bag either: the overlay
has no field for one, and a previewed unsaved rule evaluates against the floor
bags alone.

### Simulate (what-if, never persisted)
- `aperture_simulate` — render the decision (full trace) for a question under a
  hypothetical overlay of principals, groups, permissions, grants, and
  memberships, **without writing anything**. The overlay is additive; an overlay
  entity with the same id as a stored one shadows it. Use it to preview
  "what if I bestowed this grant" before doing it. Nothing is audited.

### Inspect (read the model)
- `aperture_list_object_types` / `aperture_get_object_type`
- `aperture_list_permissions` / `aperture_get_permission`
- `aperture_list_roles` / `aperture_get_role`
- `aperture_list_groups` / `aperture_get_group`
- `aperture_list_principals` / `aperture_get_principal`
- `aperture_list_grants` (account-scoped) / `aperture_get_grant`

### Docs
- `aperture_skills_list` / `aperture_skills_get` — this skill pack.

## House pattern: SDK-free core + adapter + firewall

The MCP layer is split so that importing the tool contract never drags in the
protocol SDK:

- **`mcp/` core** is SDK-free. It defines the typed In/Out contract per tool,
  reflects a JSON Schema for each via `github.com/google/jsonschema-go`, and holds
  the handlers that call the `service.Service` facade. It imports the facade (and
  the domain types the facade returns) plus the schema reflector — **never** the
  MCP SDK.
- **`mcp/gosdk`** is the single adapter and the ONLY package allowed to import
  `github.com/modelcontextprotocol/go-sdk`. It mounts the SDK-free catalog onto a
  caller-supplied server via the low-level `Server.AddTool` path, adapting raw
  tool arguments into the core's type-erased `Invoke` and rendering coded errors
  as a structured `{code, message, details}` envelope.
- **`mcp/firewall_test.go`** runs `go list -deps` over the core packages and fails
  if the protocol SDK appears anywhere in their transitive dependency set — making
  the SDK-free guarantee load-bearing rather than aspirational.

Deeply-nested or self-referential schema fields are typed `any` so the schema
reflector never recurses (it errors on a Go-level type cycle); the current
contract types are all non-cyclic, and the schema test asserts reflection
produced no errors.
