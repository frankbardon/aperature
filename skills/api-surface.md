---
name: api-surface
description: The full Aperture API over Twirp + net/http + CLI — decisions and mutations behind one service facade, with auth required and admin tiers enforced on every mutation.
applies_to: [twirp, http, cli, mcp]
---

# API surface

Aperture exposes its whole API — queries AND mutations — over three coordinated
surfaces that all drive ONE facade (`service.Service`), so the auth policy, the
admin-tier enforcement, and the fail-closed decision semantics live in exactly
one place (FR-26, FR-28).

- **Twirp** (`internal/wire/rpc`, package `aperture`) — the generated
  `ApertureService` server, mounted on the net/http `ServeMux` under the path
  prefix `/twirp/aperture.ApertureService/` with `twirp.ServerHooks` request +
  error logging (the orbit pattern). The handler is `internal/server/twirp.go`.
- **net/http** — the minimal plain `POST /check` decision route is preserved
  (E1-S5) alongside the Twirp surface, plus `GET /healthz`.
- **CLI** (`urfave/cli/v3`) — `check`, `enumerate`, `explain`, `identifiers`
  (decisions + provider reads) and
  `put`, `get`, `list`, `delete`, `bestow`, `revoke`, `impersonate`, `template`
  (`put`/`get`/`list`/`delete`/`apply`), `bulk` (`grant`/`revoke`) (mutations).
  Each command builds the same fully-wired facade in-process; `cmd/aperture`
  stays a thin adapter.

## The facade (`service.Service`)

`service.New(eng, opts...)` returns the facade. With no options it is read-only
(the decision API); the mutation surface turns on with `WithStorage` +
`WithGate` (+ `WithDelegation` / `WithImpersonation`). One facade is the single
seam the read-subset MCP (E4-S3), audit-wrapping (E4-S2), provisioning (E5), and
the UI (E6) all build on.

Full surface:

- **Decisions** (read): `Check`, `Enumerate`, `Search`, `Explain` + `CheckBatch`,
  `EnumerateBatch`, `SearchBatch`, `ExplainBatch`. Fail-closed (an operational error folds to a
  deny; only input-validation is returned). `EnumerateQuery` additionally carries
  the OPTIONAL metadata filter — see [the enumerate metadata
  filter](#the-enumerate-metadata-filter) — and a `Limit` that **every** surface
  passes through unexamined: the process-wide enumeration bound
  (`--enumerate-limit` / `APERTURE_ENUMERATE_LIMIT`, default 1000) is applied by
  the engine, so a `limit` above it is clamped **down** with nothing in the
  response saying so. No surface owns or restates that policy; see
  `skills/decision-api.md`.
- **Object search** (read): `Search(SearchQuery) -> []SearchMatch` and
  `SearchBatch`. Resolves a NAME to an id: the objects `Enumerate` would return,
  ranked by how well their metadata matches free text. Candidates are DECIDED
  before they are scored (it walks the enumerate pipeline), so the result is
  always a SUBSET of `Enumerate`'s — a score can only subtract. `SearchQuery`
  carries `Query` (required), `MatchFields` (which metadata fields to match
  against), `MinScore`, and the same OPTIONAL `Fields` / `References` /
  `Limit` `EnumerateQuery` carries, with `Limit` capping the finished RANKING
  rather than the scan. Each match carries the object's metadata inline, so
  labelling a result set costs no further call. Requires `engine.WithMetadata`
  (else `APERTURE_PROVIDER_UNREGISTERED` — never an empty result). Search
  SELECTS; it never authorizes. See `skills/object-search.md`.
- **Object metadata** (read): `ObjectMetadata(objectID)` and
  `ObjectMetadataBatch(objectIDs)` — the single and bulk forms of one provider
  metadata read, the batch aligned by index with one bad id carrying its own
  error. Both require `WithProviders`. Neither authorizes or filters: they label
  ids a caller already holds.
- **Audit query** (read): `QueryAudit(AuditFilter)` returns the append-only audit
  events matching the filter (actor, account, event type, outcome, since/until,
  limit), newest-first, each as canonical JSON. It is a GATED read — a
  system-admin reads the whole trail; an account-admin reads only events scoped to
  their own account (the filter must name it). It records nothing (not itself an
  audited mutation) and backs the E6-S4 audit viewer.
- **Entity CRUD**: `Put/Get/List/Delete` for `ObjectType`, `Permission`,
  `Principal`, `Role`, `Group`, `Account`; `Put/Delete` for `Membership`;
  `Put/Get/List/Delete` for `Grant`.
- **Object identifiers (read)**: `ObjectIdentifiers(objectType, exclude...)`
  enumerates a type's INSTANCE ids from its provider (the `providers:` or
  `objects:` section a seed declares, wired with `WithProviders`; when both
  sections claim a type the file-backed `providers:` entry wins the type outright
  by default and every inline entry for it is discarded, so the ids enumerated are
  the file's alone — see `skills/metadata-values.md`) — the complete,
  unbounded set, minus
  any `exclude` ids. It materialises the positive allow-list an EXCLUSIVE
  allowance ("all objects of this type except these ids") expands to. An
  object-type with no declared provider → `APERTURE_PROVIDER_UNREGISTERED`; a
  facade built without `WithProviders` → `APERTURE_UNIMPLEMENTED`.
- **Attribute directories (SYSTEM-tier read)**: `ListAttributes(actor, slot,
  provider.AttributeFilter)` enumerates one attribute slot (`user` / `machine` /
  `account`) — the host directory the decision path resolves `principal` and
  `account` bags from — returning `[]provider.AttributeRecord` (key + bag). It is
  wired with `WithAttributes` and is **gated directly through
  `authz.Gate.RequireSystemAdmin`**, exactly like `Export`, not through the
  `Mutation` table: it writes nothing, so it is a read, and it is not audited.
  `ExplainAttributeAuthority(actor)` returns the engine `Trace` behind that
  authority decision so a refused operator can see why. See
  [the attribute directory read](#the-attribute-directory-read).
- **Rules (E7-S3)**: `Put/Get/List/Delete` for `Rule` (the named rule-AST
  definitions the node editor authors and rule-backed scope strategies resolve;
  the AST rides as `rule_json`/`rules_json`, the exact `rules.Node` serialization).
  `PutRule` DEEP-validates the AST (structure + compile pass) and rejects an
  invalid rule with its `APERTURE_RULE_*` code before persisting. `ValidateRule`
  runs that same validation WITHOUT persisting, so the editor can check before it
  saves. Rule DEFINITION is SYSTEM tier; reads require auth only.
- **What-if (read-only, E7-S3)**: `Simulate` / `SimulateExplain` render the
  decision (and full Explain trace) for a query under a hypothetical `Overlay`
  (rules / grants / permissions / principals) layered over the live model,
  persisting nothing. They back the rule editor's live preview of an UNSAVED rule:
  the overlay rule shadows the stored one of the same name, so a preview reflects
  the edit against grants that reference it. Requires an authenticated principal.
- **Rule what-if against an object (read-only)**: `EvaluateRule(ast, objectID)`
  compiles an UNSAVED rule AST and evaluates it directly against ONE object's
  provider metadata (`WithProviders`), returning the boolean result plus the
  metadata snapshot. No account/principal/grant is involved — the rule reads only
  `object.*` — so the rule builder can sample an object (via `ObjectIdentifiers`)
  and show whether the rule selects it. Requires an authenticated principal.
  `EvaluateRulePreview` is the same evaluation returning the diagnostics too —
  the reference instant, what each relative-date operand resolved to at it, and
  the evaluation's deny-safe notes (`RulePreview`); `EvaluateRule` is its narrow
  projection. **This path supplies the reference instant itself** (from the
  facade clock, `WithClock`) because it compiles the AST directly rather than
  going through the decision engine's `rules.WithDecisionInstant` scope: without
  it every relative date resolves to nothing and the preview denies a rule that
  a real `Check` allows.
- **Delegation**: `Bestow`, `Revoke`.
- **Impersonation**: `ImpersonationStart`, `ImpersonationStop`.
- **Provisioning (E5-S1)**: `Put/Get/List/Delete` for `Template` (named,
  versioned, parameterized grant bundles); `ApplyTemplate` (resolve params →
  expand → apply transactionally → one audit event); `BulkPutGrants` /
  `BulkDeleteGrants` (provision/deprovision many grants atomically). Template
  DEFINITION is SYSTEM tier; APPLY and BULK write grants, so they are ACCOUNT tier
  in the target account. All three are TRANSACTIONAL via `Storage.Atomic` — a
  partial failure rolls the WHOLE operation back, so no grant persists if any
  fails.

## The enumerate metadata filter

`Enumerate` (and `EnumerateBatch`, and the impersonated `EnumerateAs`) takes an
OPTIONAL set of object-metadata predicates that narrow the result. It is one
input crossing five layers, and every layer spells it differently:

| Layer | Spelling |
|---|---|
| `engine.EnumerateRequest` | `Fields map[string]any` |
| `service.EnumerateQuery` | `Fields map[string]any` — forwarded to the engine **unchanged** by `request()` |
| Twirp (`service.proto`) | `EnumerateRequest.fields`, `map<string, google.protobuf.Value>` (field 6), decoded by `rpc.FieldsFromWire` / encoded by `rpc.FieldsToWire` (`internal/wire/rpc/fields.go`, hand-written beside the generated code) |
| CLI | `aperture enumerate --field key=value` (repeatable) and `--fields-json '{…}'` |
| MCP | `aperture_enumerate` / `aperture_enumerate_batch` input `Fields`, reflected off `service.EnumerateQuery` |

Omitting it everywhere means the same thing: a nil Go map, "no predicate", and
the engine does not even consult a metadata source.

### Semantics a caller cannot guess

The meaning is `provider.Filter`'s `Fields` contract **verbatim** — one
definition, one implementation (`provider.MatchFields`), so an enumeration
filtered by the engine and one filtered inside a provider's `Query` select the
same objects:

- **AND across keys.** Every predicate must hold.
- **A collection field matches by MEMBERSHIP.** `{"brands": "brand:Y"}` selects
  objects whose `brands` array contains `brand:Y`. A *list-valued want* is a
  container compared by equality, not a membership set.
- **An absent field NEVER matches** — not even against a `null`/`nil` want. An
  object whose metadata lacks the key is excluded.
- **Comparison is TYPED.** Numbers compare across Go numeric types by value
  (`int64(5)` matches a `float64(5)` want), but `"5"` never matches `5`.

Two ordering/failure rules matter as much as the comparison rules:

- **The filter runs BEFORE `Limit`.** Candidates are decided, then predicated,
  then truncated. Filtering after truncation would answer a different question —
  the matches among the first `Limit` candidates rather than the first `Limit`
  matches — and return a silently wrong answer.
- **The filter can only SUBTRACT from the allowed set.** It is applied to
  candidates that already survived deny-overrides and specificity, so no
  predicate can surface an object `Check` would deny.

### Failure modes

- **No metadata source, or no provider for the candidate's object-type** →
  `APERTURE_PROVIDER_UNREGISTERED`, never a silently empty result (an empty list
  reads as "no access" and would hide the misconfiguration). The metadata source
  is `engine.WithMetadata(reg)`, wired to the same `*provider.Registry` that
  backs the scope lister; `internal/cli.buildDecisionStack` does this for
  `serve`, the one-shot commands, and `aperture mcp` alike.
  Because the predicate runs **per candidate**, this error only surfaces when the
  enumeration has at least one ALLOWED candidate. An enumeration whose allowed
  set is empty returns empty regardless of how metadata is wired.
- **The object has no metadata row** (the provider's `Fetch` returns
  `APERTURE_NOT_FOUND`) → every field is absent, absent never matches, so the
  object is EXCLUDED rather than erroring. Any other `Fetch` failure surfaces
  verbatim.
- **A malformed predicate** → `APERTURE_INVALID_INPUT`, never a dropped
  predicate. A dropped predicate WIDENS the result, and a filter that silently
  widens is a filter that authorizes. In `EnumerateBatch` the rejection rides in
  that item's error slot alone (`BatchResult`'s contract) — one bad query never
  fails the batch, and the item reports the error rather than an empty list.

### Wire encoding: `google.protobuf.Value`, and its one caveat

`Value` rather than a JSON string because the predicate compares by *type*: the
schema itself states the string/number/bool/list distinction, so a generated
client in any language builds a well-formed predicate and a malformed one is
rejected at the boundary. `FieldsFromWire` maps each kind onto exactly one Go
shape (`null`→`nil`, `number`→`float64`, `string`, `bool`, `list`→`[]any`,
`struct`→`map[string]any`), recursively, with **no coercion between kinds** — a
number is never parsed out of a string. `FieldsToWire` is its inverse.

- **`>2^53` loses precision.** `Value` carries every number as a double, so an
  integer beyond 2^53 does not survive the trip and must be sent (and stored) as
  a **string**. `FieldsToWire` does not error on it — the loss is a property of
  `Value`, not something the conversion can repair.
  `TestFieldsRoundTrip_LargeIntegerLosesPrecision` fails loudly if that stops
  holding.
- **Non-finite numbers are REJECTED.** NaN and ±Inf are
  `APERTURE_INVALID_INPUT`, not `structpb`'s strings `"NaN"` / `"Infinity"` — a
  numeric want silently turned into a string want would match nothing and read
  as "no access". protojson cannot marshal a non-finite `Value` at all, so only
  the binary codec can reach that guard.

### The CLI's two spellings

A shell flag only ever carries a string, but the predicate is typed, and
guessing (parsing `"5"` into a number in the adapter) would make an enumeration
return objects a `Check` then denies. So `aperture enumerate` offers both and
says which is which:

- `--field key=value` — repeatable; the value is **always a string**. Everything
  after the FIRST `=` is the value, so `--field expr=a=b` wants `"a=b"`.
- `--fields-json '{…}'` — a JSON object, for a value that genuinely is a number,
  bool, or list.

**Precedence: `--fields-json` is merged FIRST and `--field` entries then override
by key**, so a stored JSON body can be reused with one value swapped from the
shell. The rule is stated in both flag usages and the command description, and a
test asserts `--help` still says so. Parsing happens before the store is opened,
so a usage error never boots a decision stack.

### The MCP schema's `omitempty`

`service.EnumerateQuery.Fields` carries the first `json`/`jsonschema` struct tags
on any `service` type, because `mcp.EnumerateIn` **aliases** the struct and the
tool schema is reflected off it. The `omitempty` is load-bearing: without it the
reflected schema marks the predicate REQUIRED, making an unfiltered
`aperture_enumerate` — by far the common call — unrepresentable for a
schema-validating client. The JSON name stays PascalCase `Fields` so one object
does not mix two spellings. Do not drop either tag in a refactor.

## The enumerate reference edge

`Enumerate` (and `EnumerateBatch`, and `EnumerateAs`) also takes an OPTIONAL set
of **reference edges** that restrict the result to the identities a holder
object's *declared* reference field contains — "the brands in dataset X". It is a
**dereference, not a predicate**, which is why it is a separate input rather than
another entry in `Fields`: a brand carries no field naming its datasets, so no
predicate on brand can express the question. `Fields` answers the mirror image
("which datasets contain brand Y?") because *that* side holds the field. The full
model is `skills/object-references.md`.

| Layer | Spelling |
|---|---|
| `engine.EnumerateRequest` | `References []engine.ReferenceEdge` |
| `service.EnumerateQuery` | `References []service.ReferenceEdge` — converted field-for-field by `request()`, never parsed or validated |
| Twirp (`service.proto`) | `EnumerateRequest.references`, `repeated ReferenceEdge` (field 7). `EnumerateBatchRequest` embeds `EnumerateRequest`, so edges ride **per query** |
| CLI | `aperture enumerate --via <holder-identity>.<field>` (repeatable) |
| MCP | `aperture_enumerate` / `aperture_enumerate_batch` input `References`, reflected off `service.EnumerateQuery` |

An edge is **three plain strings** — `{HolderType, HolderID, Field}` — so unlike
the metadata filter it needs no `google.protobuf.Value` gymnastics and no value
model at all: an edge *names* a field, it never carries one. `HolderID` and
`Field` are mandatory; `HolderType` is optional and, when given, must **agree**
with `HolderID`'s terminal segment type. The CLI does not spell the type at all,
so the two cannot disagree. An omitted edge list is a nil Go slice at every
layer, so an existing client is unaffected.

**No surface validates an edge on the way in.** Whether the holder identity
parses, whether its type serves a provider, and whether the field is declared are
all decisions with a disclosure consequence, so they are made in exactly one
place (`engine/reference.go`); a surface that answered one separately would be a
second place for the answer to differ.

### Composition and ordering

Several edges **AND**; an edge composes with `Fields`; and both are applied
**before `Limit`**, for the same reason the filter is. Exactly **one hop** is
taken — the identities an edge yields are never themselves dereferenced. The
restriction only ever *subtracts*, and it is computed **once per enumeration**,
before candidates are gathered, against the same grants and subject set the
candidates are decided with (so `EnumerateAs` checks the holder with the
**impersonated** authority).

### The empty-vs-`NOT_FOUND` split is a per-surface contract

This is the part most likely to be lost in translation, and it is asserted per
surface rather than assumed — a boundary that turned an empty result into a 404,
or a 404 into an empty list, would silently change what the system discloses
about objects it never let the caller see, and no test in `engine/` can catch it:

| Situation | Every surface answers |
|---|---|
| the holder is **unreadable** by the principal | **empty result, HTTP 200** — never 403, never 404 |
| the holder is **absent, in-account**, caller is a **member** | `APERTURE_NOT_FOUND` / **404** |
| the holder is **out of account** | **empty**, whether or not it exists |
| the caller is a **non-member** | **empty**, always — membership is decided before the holder is looked up |
| the field is **not declared** (a wiring fault) | `APERTURE_PROVIDER_REFERENCE_INVALID` / **400** — loud, never empty |
| the holder type has **no provider** | `APERTURE_PROVIDER_UNREGISTERED` / **404** — loud, never empty |

`APERTURE_PROVIDER_REFERENCE_INVALID` is mapped to `twirp.InvalidArgument`
explicitly, out of `codeToTwirp`'s 500 default: the only way it reaches a client
is an edge naming an undeclared field, which is the caller's own input and can
never succeed on retry, so a 5xx would tell a retrying client and a paging alert
rule exactly the wrong thing.

### The CLI's `--via`, and the MCP `omitempty`

`--via` is split on the **LAST** `.`: a `.` is legal inside an identity component
(`dataset:2026.q1`) while a reference field is a single metadata key, so the final
dot is the only unambiguous boundary. It is repeatable, edges are ANDed, and a
malformed value is `APERTURE_INVALID_INPUT` naming the offending text — in the
same words a malformed `--field` gets — rejected before the store is opened. The
format, the AND, and the empty-not-error rule are in the help text and asserted
there.

On the MCP side `mcp.EnumerateIn` aliases the facade query, so the edges arrive
typed; the `omitempty` on `References` is what keeps them **optional** in the
reflected schema, exactly as it is for `Fields`. Without it an edge-less
`aperture_enumerate` — by far the common call — is unrepresentable for a
schema-validating client. `HolderID` and `Field` are required properties;
`HolderType` is not.

## The object search

`SearchQuery` is `EnumerateQuery` plus a name. Everything the enumerate filter
and the reference edge say about carrying values UNCHANGED across a surface
applies here verbatim, and one thing more: **no surface normalises the query
text**. Folding case, stripping punctuation and tokenising all happen in exactly
one place (`provider.NormalizeText`, reached through `provider.MatchText`), so a
name typed at the CLI, sent over MCP, and passed in Go all score identically.

Three properties every surface must preserve:

- **`Query` is REQUIRED.** An empty query is `APERTURE_INVALID_INPUT`, not
  "match everything" — what it would return is the subject's whole entitled set,
  unranked, which is the bulk read this surface replaces. In the MCP schema that
  means `Query` carries a `json:"Query"` tag WITHOUT `omitempty`, while
  `MatchFields` / `Fields` / `References` / `MinScore` / `Limit` all carry it, so
  the reflected schema marks exactly one of them required.
- **A score is not a permission.** The MCP tool description says so in those
  words, because an agent has no other source of truth and the failure mode —
  acting on a high-scoring id without checking it — is invisible.
- **The containment holds per surface.** `Search` ⊆ `Enumerate` is asserted in
  `engine`, `service` AND `mcp` rather than once, for the same reason the
  enumerate reference edge's empty-vs-`NOT_FOUND` split is: a relaxation in one
  place is a silent disclosure channel.

`MatchFields` is spelled as a separate field from `Fields` — and as `--in`
rather than `--field` on the CLI — because the two say different things.
`MatchFields` says WHERE to look; `Fields` says WHAT to find. Conflating them
would let a caller think a search had been narrowed when it had only been
filtered.

| Surface | Spelling |
|---|---|
| Twirp (`service.proto`) | `SearchRequest` → `SearchResponse` (`Search`), `SearchBatchRequest` → `SearchBatchResponse` (`SearchBatch`). `query` is field 5; `match_fields` 6; `fields` 7 and `references` 8 reuse the enumerate encodings verbatim, decoded by the same `rpc.FieldsFromWire` / `referenceEdges` converters so a predicate cannot mean one thing on `Enumerate` and another on `Search`. `SearchMatch.metadata` goes back out through `rpc.FieldsToWire` |
| CLI | `aperture search <principal> <action> <pattern> <query>`; `--in` for `MatchFields`, `--min-score`, `--limit`, `--scores` |
| MCP | `aperture_search` / `aperture_search_batch`, schema reflected off `service.SearchQuery` |

A match whose metadata fails to encode fails the CALL rather than coming back
with an empty `metadata` map: a client cannot distinguish an object with
genuinely empty metadata from one whose bag failed to encode, so the second must
not be able to masquerade as the first. In the batch form the same failure rides
in that item's error slot and clears its matches — a query that never ran must
never read as "nothing by that name".

## Auth + admin-tier policy

- **Decision RPCs are open** — `Check` / `Enumerate` / `Explain` require no
  credential, preserving the simple decision path. `POST /check` stays open too.
- **Entity reads require an authenticated principal**, and account-scoped reads
  are additionally SCOPED to the caller's visibility (a customer's admin must not
  enumerate another customer's data). `ListAccounts` / `ListPrincipals` /
  `ListGrants` / `GetGrant` resolve through `service.readScope`: a system-admin
  (`aperture.admin` on `system:schema`, resolved in the `"*"` account) sees
  everything; any other principal is scoped to the accounts it can SEE — the
  accounts it is a MEMBER of plus the accounts it ADMINISTERS — and within those,
  their grants and the principals who are members of them (plus itself). Platform
  (`"*"`) grants are system-admin-only. A read for an
  account the caller does not administer returns `APERTURE_AUTHZ_DENIED`. The
  shared-schema catalogs (`ObjectType`, `Permission`, `Role`, `Group`, `Rule`,
  `Template`) stay readable by any authenticated principal — they are the
  vocabulary an account-admin needs, not per-account data. `ObjectIdentifiers` is
  likewise an auth-required read (all of a type's instance ids, not a
  principal-scoped decision). Scoping only engages when a gate is wired and a
  principal is identified, so the local CLI/MCP facades stay unrestricted.
### `ListGrants` — all-accounts view + pagination

`ListGrants` (`ListGrantsRequest` → `ListGrantsResponse`) lists grants for a
scope, gated by `service.readScope` as above, and is paginated:

- **Scope via `account_id`.** A **non-empty** `account_id` lists that single
  account's grants — the original behaviour, byte-for-byte compatible. An
  **empty** `account_id` is the **all-accounts sentinel**: it lists grants across
  EVERY account and is **SYSTEM-ADMIN ONLY** (`sys == true` from `readScope`). An
  account-admin — however many accounts it administers — is denied the
  all-accounts path with `APERTURE_AUTHZ_DENIED` / 403 (the service gates it
  before the store is touched, and never leaks which accounts exist). The
  wildcard-stamped (`"*"`) platform grants are returned **inline** in an
  all-accounts page, not filtered out. The empty string is a query sentinel, NOT
  a real account id — it is distinct from `"*"` (the wildcard account), which is
  never overloaded to mean "all".
- **Pagination via `offset` + `limit`.** The request carries `offset` (leading
  rows to skip) and `limit` (page size); the response is `ListGrantsResponse`,
  which carries the page as `entities_json` (each grant canonical JSON, like
  `EntityListResponse`) plus `total` (the full pre-pagination match count for the
  scope) and echoes the effective `offset` / `limit`. The client renders next/prev
  from `total` (`next_offset = offset + limit` while `next_offset < total`). A
  non-positive `limit` falls back to `model.DefaultGrantPageSize` and a `limit`
  above `model.MaxGrantPageSize` is clamped down (`model.ClampGrantPage`), so a
  single call never returns an unbounded page. A **negative** `offset` or `limit`
  is a caller error → `APERTURE_INVALID_INPUT` / 400. Older clients that omit the
  page fields get the default first page, preserving the original single-account
  behaviour. The `filter` (if any) applies server-side to the returned page.

- **Mutations require an authenticated principal AND the admin tier their kind
  needs** (`authz.Gate`): schema entities are SYSTEM tier (`system:*`);
  membership + raw grants are ACCOUNT tier (`account:<acct>/admin:*` in the
  target account). Unauthenticated → `APERTURE_UNAUTHENTICATED` / 401; wrong tier
  → `APERTURE_AUTHZ_DENIED` / 403.
- **Delegation and impersonation are NOT admin-gated** — they carry their own
  finer-grained authorization (the delegation subset rule / the impersonation
  guardrails), where the actor is the delegator / operator, not an admin.

On the Twirp surface the actor's principal is ALWAYS the authenticated identity
from the auth middleware, never a value from the request body — a caller cannot
act as someone else. The wire's `Actor.account` selects the active account.

## The attribute directory read

`ListAttributes` is the ONE administrative door onto an attribute slot, and the
only place `provider.AttributeRegistry.Enumerate` is reachable from a surface.
Listing the `user` slot returns the host's whole user table, so it is a
system-tier read: `RequireSystemAdmin` directly, the shape `Export` uses, never a
`Mutation` row (it writes nothing).

- **The gate is above the registry, deliberately.** `provider` imports only
  `identity` + `errors` (`TestProviderPackageImportsOnlyIdentityAndErrors`), and
  `authz` imports `engine` — a gate inside the registry would invert the
  dependency. The facade is also where it belongs on the merits: one gate serves
  CLI, HTTP, Twirp and MCP at once.
- **The gate runs BEFORE the slot is resolved**, so a refused caller cannot
  probe which slots a deployment wires. A non-admin gets the identical
  `APERTURE_AUTHZ_DENIED` for a populated slot, an unregistered slot, and a slot
  name that does not exist — with a nil slice, no count, and nothing about the
  directory in the error. The gate's error passes through VERBATIM (`Wrap`
  re-stamps, and the code's fixups are the operator's remedy). A system-admin, by
  contrast, does get the real diagnostics: `APERTURE_ATTRIBUTE_SLOT_UNKNOWN` and
  `APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED`.
- **No gate wired → `APERTURE_UNIMPLEMENTED`**, not "unrestricted". This is the
  one read that does NOT take `readScope`'s local-context concession, because a
  bulk directory read has no narrower fallback to degrade to.
- **`Fetch` on the decision path is NOT gated, and must never be.** A decision
  resolves one bag for a subject it already named; gating it would deny every
  rule-backed decision in every deployment that wires a directory. The two paths
  reach the same registry through different seams —
  `rules.WithPrincipalResolver` / `rules.WithAccountResolver` for the decision,
  `WithAttributes` for the admin read.
- **The implicit path is impossible, not policed.** A scope resolver enumerating
  mid-decision through `scope.ObjectLister` has no actor, so it could never carry
  a gate. `provider.AttributeRegistry` is therefore built so it cannot satisfy
  that interface (`Enumerate`, not `List`; an `AttributeSlot`, not a type string;
  an `AttributeFilter` with no `identity.Pattern`; `[]AttributeRecord`, not
  `[]identity.Identity`), asserted by
  `provider.TestAttributeRegistryIsNotAScopeLister`.
- **It is not the only way a value is seen.** An `Explain` trace carries the
  bags a decision was evaluated against, values included (E5-S1) — a deliberate
  disclosure. The gate closes the bulk-read door, not every door.

## Wire encoding

Simple/hot-path messages are typed (`CheckRequest`, `Decision`,
`EnumerateRequest`). Rich or nested shapes ride as a canonical JSON string: model
entities as `entity_json` (the `encoding/json` form of the `model.*` struct), the
decision `Trace` as `trace_json`. This mirrors orbit's JSON-payload convention,
keeps the proto small, and sidesteps modelling the self-referential rule AST.

The one deliberate exception is `EnumerateRequest.fields`, which is
`map<string, google.protobuf.Value>` rather than a `*_json` string: the predicate
compares by TYPE, so the string/number/bool/list distinction has to live in the
schema where a generated client can see it, not inside an opaque blob. See [the
enumerate metadata filter](#the-enumerate-metadata-filter) for the conversion and
its `>2^53` caveat.

## Regenerating

`make proto` regenerates `service.pb.go` + `service.twirp.go` from
`service.proto` (needs `protoc` + `protoc-gen-go` + `protoc-gen-twirp`). The
generated files are COMMITTED; CI does not regenerate, so re-run `make proto` and
commit the result whenever the `.proto` changes.
