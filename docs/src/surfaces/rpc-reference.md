# RPC reference

**Audience:** engineers wiring specific calls against `aperture serve`.

This is a hand-authored catalog of the `ApertureService` RPCs, grouped by area.
It summarises each method's purpose, its request/response messages, and its auth
requirement. The **canonical, exact** field lists live in
[`internal/wire/rpc/service.proto`](https://github.com/frankbardon/aperture/blob/main/internal/wire/rpc/service.proto)
— read it for message shapes; this page is maintained by hand and may lag the
proto (the proto wins on any discrepancy).

Every method is a `POST` to
`/twirp/aperture.ApertureService/<Method>` with a JSON body. See the
[overview](rpc-overview.md) for transport, auth, and error mapping. Auth
shorthand used below:

- **open** — no authenticated principal required.
- **auth** — requires an authenticated principal.
- **system** — requires system-admin authority (`system:*`).
- **account** — requires account-admin authority in the target account (system
  supersedes).
- **own rule** — not gated by the admin tiers; carries its own delegation /
  impersonation authorization.

## Wire-shape convention

Simple hot-path messages (`Check`, `Enumerate`) carry their fields directly.
Rich or recursive shapes — the model entities with their timestamps, the
`Explain` trace, the rule AST — ride as a **canonical JSON string** in a `*_json`
field rather than being modelled in proto. That JSON is identical to the
library's own encoding of the corresponding `model.*` struct. So an
`EntityRequest.entity_json` is just the JSON of a `model.ObjectType`,
`model.Grant`, etc., and an `EntityResponse.entity_json` is the same on the way
back.

Mutations carry an `Actor { principal, account }`. On the wire the `principal` is
**ignored** — the authenticated identity from the middleware is always used —
while `account` selects the active account. Reads that are account-scoped resolve
their authority from the authenticated principal directly.

## Decision RPCs (open)

The core decision API, single and bulk. These are open (no principal required)
and always answer fail-closed.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Check` | `CheckRequest` → `Decision` | Is `principal` allowed `action` on `object` in `account`? Returns `allow`, a `reason`, and the deciding grant ids. |
| `CheckBatch` | `CheckBatchRequest` → `CheckBatchResponse` | Many `Check`s in one call; results are index-aligned, each either a `Decision` or a per-item error code+message. |
| `Enumerate` | `EnumerateRequest` → `EnumerateResponse` | Which object ids matching `pattern` may `principal` take `action` on? Optional `fields` (an object-metadata filter, below), optional `references` (reference edges, below) and `limit`. |
| `EnumerateBatch` | `EnumerateBatchRequest` → `EnumerateBatchResponse` | Batched `Enumerate`; index-aligned results. Each query embeds an `EnumerateRequest`, so `fields` and `references` are per-query. |
| `Search` | `SearchRequest` → `SearchResponse` | Which of the objects `Enumerate` would return are the ones a person means by `query`? Ranked best first, each match carrying its score, the field and value that matched, and the object's full metadata. Optional `match_fields`, `fields`, `references`, `min_score`, `limit`. |
| `SearchBatch` | `SearchBatchRequest` → `SearchBatchResponse` | Batched `Search`; index-aligned results. Each query embeds a `SearchRequest`, so every option is per-query. |
| `Explain` | `CheckRequest` → `ExplainResponse` | The full decision derivation for a query, as `trace_json` (the recursive engine `Trace`, not modelled in proto). |
| `ExplainBatch` | `CheckBatchRequest` → `ExplainBatchResponse` | Batched `Explain`; index-aligned `trace_json` or per-item error. |

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/Enumerate \
  -H 'Content-Type: application/json' \
  -d '{"account":"acme","principal":"alice","action":"read","pattern":"doc:*","limit":50}'
```

```json
{ "object_ids": ["doc:42", "doc:77"] }
```

### `SearchRequest` — resolving a name to an id

`Search` is the call to make when a question arrives in a person's own words and
every step after it needs ids.

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/Search \
  -H 'Content-Type: application/json' \
  -d '{"account":"acme","principal":"alice","action":"read",
       "pattern":"account:acme/brand:*","query":"nike","limit":5}'
```

```json
{ "matches": [
    { "object": "account:acme/brand:42", "score": 0.875,
      "field": "label", "value": "Nike, Inc.",
      "metadata": { "label": "Nike, Inc.", "sector": "Footwear" } }
] }
```

**The result is already scoped — do not filter it client-side.** Candidates are
*decided before they are scored*, by the same walk `Enumerate` uses, so the
matches are always a subset of what `Enumerate` would return for the same
`account`, `principal`, `action` and `pattern`. The alternative shape — enumerate
a type over the wire, then match in your client — puts the *unscoped* set across
the boundary first and narrows it second, and any bug in that client-side filter
turns your surface into an enumeration oracle for every object in the system.

**A score ranks; it never authorizes.** Nothing in Aperture reads one. A client
that acts on a returned id still calls `Check`.

| Field | Meaning |
|---|---|
| `query` | The free text. **Required** — an empty query is `invalid_argument` (`APERTURE_INVALID_INPUT`), not "match everything". For the whole entitled set, call `Enumerate`. |
| `match_fields` | Metadata field names to restrict matching to. Omit to search every field holding text. Aperture has no notion of a "label" — a label is an ordinary field whose name *you* chose. |
| `fields` | The object-metadata filter, identical in meaning and encoding to `EnumerateRequest.fields` (below). It **composes**: the predicate narrows, the query ranks. |
| `references` | Reference edges, identical to `EnumerateRequest.references` (below), with the same fail-closed rules. |
| `min_score` | The score floor in `[0,1]`; `<= 0` means the engine default. Raising it narrows the shortlist and never widens what the subject may see. |
| `limit` | Caps the returned **matches** — the top of a finished ranking, not a bound on the scan, so you get the best N rather than the first N found. |

Matching is case- and punctuation-insensitive (`Nike, Inc.` matches `nike inc`)
and tolerates a typo or transposition. Only text is matched — a string field and
the string elements of a list field; match a number, bool or date through
`fields` instead.

Each match's `metadata` is the object's full bag, encoded exactly like
`EnumerateRequest.fields` and carrying the same caveat: a
`google.protobuf.Value` holds a number as a double, so an integer beyond 2^53
loses precision in transit. It rides along so labelling a result set costs no
further round-trip.

`Search` **requires** the deployment to have an object-metadata source wired.
Without one the answer is `APERTURE_PROVIDER_UNREGISTERED`, never an empty
`matches` list — an empty list reads as "you may see nothing" and would hide the
misconfiguration.

### `EnumerateRequest.fields` — the object-metadata filter

`EnumerateRequest` carries an **optional** `fields` map (field 6,
`map<string, google.protobuf.Value>`) that narrows the result by object
metadata. Omitting it filters nothing, so an existing client is unaffected.

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/Enumerate \
  -H 'Content-Type: application/json' \
  -d '{"account":"acme","principal":"alice","action":"read","pattern":"account:acme/**",
       "fields":{"tier":"premium","seats":5,"brands":"brand:Y"},"limit":50}'
```

**Semantics** — the same contract a provider's own
[`Filter.Fields`](../concepts/providers.md#the-filterfields-contract) obeys,
evaluated by the same code, so a filtered enumeration and a provider-side query
select the same objects:

| Rule | Meaning |
|---|---|
| **AND across keys** | Every predicate must hold. |
| **Membership on collections** | A list-valued *metadata* field matches when it **contains** the wanted value (`"brands": "brand:Y"`). A list-valued *want* is a container compared by equality. |
| **Absent never matches** | An object whose metadata lacks the key is excluded — not even a `null` want matches it. |
| **Typed comparison** | Numbers compare across numeric types by value (`5` matches an `int64` 5), but the string `"5"` never matches the number `5`, and vice versa. |
| **Filter before `limit`** | Candidates are decided, then filtered, then truncated. Truncating first would return the matches among the first `limit` candidates rather than the first `limit` matches — a silently wrong answer. |
| **Filter only subtracts** | The predicate runs on candidates that already survived deny-overrides, so it can never surface an object `Check` would deny. |

**Why `Value` and not a JSON string.** The predicate compares by *type*, so the
string/number/bool/list distinction has to be visible in the schema — a JSON blob
in a `string` field would be opaque to every generated client and unvalidatable
at the boundary. Each `Value` kind maps to exactly one shape, recursively, with
no coercion between kinds: `null`→absent-ish nil, `number`→float64,
`string`→string, `bool`→bool, `list`→array, `struct`→object.

> **Caveat: integers beyond 2^53.** `google.protobuf.Value` carries every number
> as a **double**, so an integer larger than 2^53 loses precision in transit.
> Send such a key as a **string** and store it as a string in metadata. Nothing
> in the conversion can repair the loss — it is a property of `Value` itself.
> (`internal/wire/rpc`'s `TestFieldsRoundTrip_LargeIntegerLosesPrecision` pins
> this, and fails loudly if it ever stops holding.)

**Rejections** are `APERTURE_INVALID_INPUT` / 400, never a dropped predicate — a
dropped predicate widens the result, and a filter that silently widens is a
filter that authorizes:

- a **non-finite** number (NaN, ±Inf). It would otherwise become the string
  `"NaN"`, an unsatisfiable predicate that reads as "no access". Note protojson
  cannot marshal a non-finite `Value` at all, so only the binary codec can reach
  this guard;
- a `Value` with no kind set (unreachable from a generated client, reachable from
  a hand-rolled one).

In `EnumerateBatch` a malformed predicate fails **only its own item** — that
item carries the error code/message and never an empty `object_ids`, and the rest
of the batch runs.

**Wiring**: the server must have an object-metadata source (the same provider
registry that backs object listing). Filtering a type with no registered
provider, or with no source configured at all, is
`APERTURE_PROVIDER_UNREGISTERED` / 404 rather than an empty list — an empty list
would read as "no access" and hide the misconfiguration. Because the predicate
runs per candidate, that error only appears when the enumeration has at least one
allowed candidate. An object the provider has no row for is simply **excluded**
(every field absent, and absent never matches).

### `EnumerateRequest.references` — the reference edges

`EnumerateRequest` also carries an **optional** `references` list (field 7,
`repeated ReferenceEdge`) that restricts the result to the identities a holder
object's **declared reference field** contains — "the brands in dataset X".
Omitting it restricts nothing, so an existing client is unaffected.
`EnumerateBatchRequest` embeds `EnumerateRequest`, so edges ride **per query**.

```protobuf
message ReferenceEdge {
  string holder_type = 1;  // OPTIONAL; empty means holder_id's terminal segment type
  string holder_id   = 2;  // required — "account:acme/dataset:7"
  string field       = 3;  // required — a DECLARED reference field
}
```

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/Enumerate \
  -H 'Content-Type: application/json' \
  -d '{"account":"acme","principal":"alice","action":"read","pattern":"account:acme/brand:*",
       "references":[{"holder_id":"account:acme/dataset:x","field":"current_brands"}]}'
```

It is a **dereference, not a filter**, and the two are not interchangeable.
`fields` answers "which datasets contain brand Y?" — the dataset holds the field,
so a predicate on dataset expresses it. `references` answers the mirror image,
"which brands belong to dataset X?" — a brand holds **no** field naming its
datasets (references are declared on the [holding side
only](../concepts/providers.md#declared-references)), so no predicate on brand can
express it at all.

| Rule | Meaning |
|---|---|
| **Several edges AND** | "the brands in dataset X *and* in campaign Z". |
| **Composes with `fields`** | Both apply; they are independent. |
| **Before `limit`** | Restriction → decision → filter → truncate. |
| **Only subtracts** | Edges apply to candidates that already survived deny-overrides, so no edge can surface an object `Check` would deny. |
| **Exactly one hop** | The identities an edge yields are never themselves dereferenced. |
| **`holder_type` is optional** | Empty means "whatever `holder_id`'s terminal segment type is". When given it must **agree** with `holder_id`. |

**The security semantics are the point, and they are asserted over a real JSON
round trip** — a boundary that turned an empty result into a 404, or a 404 into
an empty list, would silently change what the server discloses about objects it
never let the caller see:

| Situation | Response |
|---|---|
| the principal **may not read the holder** | **HTTP 200 with an empty `object_ids`** — never 403, never 404. "You may not see dataset X" and "dataset X contains nothing you may see" must be indistinguishable, or the edge is an oracle for objects the caller was never allowed to know about. |
| the holder is **absent**, **inside** `account`, caller is a **member** | `APERTURE_NOT_FOUND` / **404** — the ergonomics a typo deserves, confined to a caller already inside the account. |
| the holder is **outside** `account` | **empty, whether or not it exists.** This is the disclosure boundary: a caller in one account never learns what does or does not exist in another. |
| the caller is **not a member** | **empty, always** — membership is decided before the holder is looked up, so a non-member never sees a 404. |
| a referenced identity **no longer exists** | **skipped**, with an operator-side warning log. An application-level foreign key has no database constraint behind it, so one deleted brand must not fail every decision on the dataset that still lists it. |
| the `field` is **not a declared reference** | `APERTURE_PROVIDER_REFERENCE_INVALID` / **400** — loud, never an empty list. A wiring fault describes the deployment, not the data; an empty list would read as "no access" to a client that cannot see the deployment. |
| the `holder_type` has **no registered provider** | `APERTURE_PROVIDER_UNREGISTERED` / **404** — likewise loud. |
| a malformed edge (empty `holder_id` or `field`, an unparseable identity, a `holder_type` that disagrees) | `APERTURE_INVALID_INPUT` / **400**, before any storage or provider work. |

> `APERTURE_PROVIDER_REFERENCE_INVALID` is a **400, not the 500** the default
> mapping would give it: the only way a client reaches it is an edge naming an
> undeclared field — its own input, which no retry can fix — and 5xx is the one
> class a client is entitled to retry and an alert rule is entitled to page on.
> The Aperture code is in `meta["code"]` either way.

In `EnumerateBatch` each item keeps its own answer: one query's 404 never becomes
another's, and a fail-closed empty result is reported as an empty list rather
than an error.

See [Object references](../concepts/providers.md#declared-references) for the
declaration side, and the CLI's [`--via`](../cli/decisions.md#restricting-to-what-a-reference-names)
for the same query from a terminal.

## Entity CRUD

Full create/read/list/delete for each model entity. The write body is
`entity_json` (a `model.*` struct as JSON); reads return `entity_json` (single)
or `entities_json` (list). List RPCs accept an optional server-side `Filter`
(field predicates ANDed or ORed) applied before the response is returned.

**Writes are system-tier** (managing the global schema); **reads require auth**.

| Entity | Put (system) | Get (auth) | List (auth) | Delete (system) |
|---|---|---|---|---|
| Object type | `PutObjectType` | `GetObjectType` | `ListObjectTypes` | `DeleteObjectType` |
| Permission | `PutPermission` | `GetPermission` | `ListPermissions` | `DeletePermission` |
| Principal | `PutPrincipal` | `GetPrincipal` | `ListPrincipals`¹ | `DeletePrincipal` |
| Role | `PutRole` | `GetRole` | `ListRoles` | `DeleteRole` |
| Group | `PutGroup` | `GetGroup` | `ListGroups` | `DeleteGroup` |
| Account | `PutAccount` | `GetAccount` | `ListAccounts`¹ | `DeleteAccount` |

¹ `ListPrincipals` and `ListAccounts` resolve read visibility against the
caller's admin authority, so an account-admin sees only what their tier permits.

Requests: `PutX` uses `EntityRequest { actor, entity_json }`; `GetX`/`DeleteX`
use `GetRequest`/`DeleteRequest { actor, id }`; `ListX` uses
`ListRequest { filter }`. Responses: `EntityResponse` / `EntityListResponse` /
`Empty`.

`ObjectIdentifiers` (`ObjectIdentifiersRequest` → `ObjectIdentifiersResponse`,
**auth**) enumerates every instance id of an object type from its provider,
optionally minus an `exclude` list — an admin/config read over all objects of a
type.

```bash
curl -s -X POST http://localhost:8080/twirp/aperture.ApertureService/PutGrant \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer acme-admin' \
  -d '{"actor":{"account":"acme"},"entity_json":"{\"ID\":\"g-1\",\"Account\":\"acme\",\"Principal\":\"alice\",\"Action\":\"read\",\"Object\":\"doc:*\",\"Effect\":\"allow\"}"}'
```

## Grants and memberships (account-tier)

Grants and memberships are account-scoped: **writes require account-admin** in
the target account; reads resolve against the caller's authority.

| RPC | Request → Response | Auth | Purpose |
|---|---|---|---|
| `PutGrant` | `EntityRequest` → `Empty` | account | Create/replace one grant (`model.Grant` as `entity_json`). |
| `GetGrant` | `GetRequest` → `EntityResponse` | auth | Read one grant, visibility-scoped to the caller. |
| `ListGrants` | `ListGrantsRequest` → `EntityListResponse` | auth | Grants in `account_id`, with optional `Filter`. |
| `DeleteGrant` | `DeleteRequest` → `Empty` | account | Delete one grant by id. |
| `PutMembership` | `EntityRequest` → `Empty` | account | Add/replace a principal's membership in an account. |
| `DeleteMembership` | `MembershipKeyRequest` → `Empty` | account | Remove a membership by `(principal_id, account_id)`. |

## Rules (definition writes system; reads auth)

Rules are global schema: named, persisted rule-AST definitions the node editor
authors and the rule-backed scope strategies resolve. The AST rides as a
`model.Rule` JSON in `rule_json`.

| RPC | Request → Response | Auth | Purpose |
|---|---|---|---|
| `PutRule` | `RuleRequest` → `Empty` | system | Persist a rule definition. |
| `GetRule` | `GetRequest` → `RuleResponse` | auth | Read one rule as `rule_json`. |
| `ListRules` | `Empty` → `RuleListResponse` | auth | Every stored rule, each as JSON. |
| `DeleteRule` | `DeleteRequest` → `Empty` | system | Delete a rule by id. |
| `ValidateRule` | `RuleRequest` → `Empty` | auth | Compile/validate a rule AST **without persisting**. Returns `Empty` on success, an `APERTURE_RULE_*` coded error (for the canvas) on failure. Touches no storage. |

## What-if / simulation (auth)

Read-only previews. Nothing is written and nothing is audited.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Simulate` | `SimulateRequest` → `Decision` | The decision a query WOULD get under a hypothetical overlay (unsaved rules + synthetic grants/permissions/principals layered over the live model). Backs the rule editor's live preview. |
| `SimulateExplain` | `SimulateRequest` → `ExplainResponse` | Same overlay, returning the full `Explain` trace. |
| `EvaluateRule` | `EvaluateRuleRequest` → `EvaluateRuleResponse` | Run an UNSAVED rule AST directly against one object's provider metadata (no account/principal/grant); returns the boolean `result`, the object metadata snapshot the rule saw (`object_json`), the reference instant it resolved relative dates against (`now`), what each relative-date operand became at that instant (`bounds_json`), and the evaluation's deny-safe notes (`notes_json`). |

## Templates

Reusable grant bundles. **Definition writes are system-tier**; **apply is
account-tier** (it materialises grants into a target account).

| RPC | Request → Response | Auth | Purpose |
|---|---|---|---|
| `PutTemplate` | `EntityRequest` → `Empty` | system | Persist a template definition. |
| `GetTemplate` | `TemplateKeyRequest` → `EntityResponse` | auth | Read template `(name, version)`; `version <= 0` selects the latest. |
| `ListTemplates` | `ListRequest` → `EntityListResponse` | auth | List templates, with optional `Filter`. |
| `DeleteTemplate` | `TemplateKeyRequest` → `Empty` | system | Delete by name/version (`version <= 0` deletes all versions). |
| `ApplyTemplate` | `ApplyTemplateRequest` → `EntityListResponse` | account | Expand a template transactionally into `account`, filling `params` and optionally prefixing generated grant ids; returns the applied grants as JSON. |

## Bulk grant / revoke (account-tier)

Transactional multi-grant mutations, both account-tier.

| RPC | Request → Response | Purpose |
|---|---|---|
| `BulkPutGrants` | `BulkGrantsRequest` → `Empty` | Create/replace many grants (`grants_json`) in one transaction. |
| `BulkDeleteGrants` | `BulkDeleteGrantsRequest` → `Empty` | Delete many grants by id in one transaction. |

## Declarative state (system-tier)

Whole-model portability.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Export` | `ExportRequest` → `ExportResponse` | Serialize the entire model to one declarative state file as `document_json` (a system-tier read). |
| `Import` | `ImportRequest` → `Empty` | Apply a state file (`document_json`) as an idempotent, transactional upsert (the most privileged mutation; system-tier). |

## Audit query (gated read)

| RPC | Request → Response | Purpose |
|---|---|---|
| `QueryAudit` | `QueryAuditRequest` → `QueryAuditResponse` | The append-only audit events matching a filter, newest first. Records nothing. A **system-admin** may query the whole trail; an **account-admin** must set `account` to their own account (which also gates the read). Filters: `filter_actor`, `account`, `event_type` (`mutation`/`decision`/`impersonation`/`delegation`), `outcome` (`allow`/`deny`/`success`/`failure`), `since`/`until` (RFC3339), `limit`. |

## Delegation (own rule)

Not admin-gated; authorized by the delegation subset rule, with the actor = the
authenticated delegator.

| RPC | Request → Response | Purpose |
|---|---|---|
| `Bestow` | `BestowRequest` → `Empty` | Hand on a subset of the delegator's own grants (`grant_json`). |
| `Revoke` | `RevokeRequest` → `Empty` | Revoke a previously bestowed grant by id. |

## Impersonation (own rule)

Not admin-gated; authorized by the impersonation guardrails, with the actor = the
authenticated operator. Sessions are stateless, time-boxed values.

| RPC | Request → Response | Purpose |
|---|---|---|
| `ImpersonationStart` | `ImpersonationStartRequest` → `ImpersonationSession` | Begin impersonating `target` in `account` under `mode` (`augment` or `become`); returns the session with `started_at` / `expires_at` (RFC3339). |
| `ImpersonationStop` | `ImpersonationStopRequest` → `Empty` | Discard a session (client-side; echoed for symmetry/audit). |

## Related

- [RPC / HTTP overview](rpc-overview.md) — transport, auth model, error mapping.
- [`service.proto`](https://github.com/frankbardon/aperture/blob/main/internal/wire/rpc/service.proto) — the canonical contract.
- [The service facade](../library/service-facade.md) — the shared code path.
- [Error Codes](../reference/error-codes.md) — the `APERTURE_*` registry.
