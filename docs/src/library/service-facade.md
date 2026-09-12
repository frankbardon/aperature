# The service facade

The `service` package is the thin decision **facade** every surface calls instead
of touching the engine directly — the CLI `check` command, the HTTP `/check`
endpoint, the Twirp service, and the MCP read subset. It exists so those surfaces
share **one** code path with **one** fail-closed policy: the rule for turning an
engine error into a rendered decision lives here, not duplicated per surface.

If you are embedding Aperture to build your own surface, call the facade rather
than the raw [engine](decision-api.md) — you inherit its fail-closed contract,
decision auditing, and the what-if `Simulate` path for free.

## Constructing the facade

```go
func New(eng *engine.Engine, opts ...Option) *Service
```

With no options a `Service` is **read-only**: it carries the decision API (`Check`
/ `Enumerate` / `Explain` and their batch forms) always, and returns
`APERTURE_UNIMPLEMENTED` from any mutation. Options wire the additional
dependencies:

| Option | Enables |
|---|---|
| `WithStorage(store model.Storage)` | Entity-CRUD mutations and their reads; also the base store the `Simulate` overlay layers onto. |
| `WithGate(gate *authz.Gate)` | The admin-authority gate consulted before every system/account-tier mutation. |
| `WithDelegation(d *delegation.Service)` | `Bestow` / `Revoke`. |
| `WithImpersonation(i *impersonation.Service)` | `ImpersonationStart` / session issuance. |
| `WithAudit(r *audit.Recorder)` | The append-only audit trail: mutations synchronously, decision checks sampled + async. |
| `WithProviders(reg *provider.Registry)` | `ObjectIdentifiers` and `ObjectMetadata` — object enumeration and metadata reads. |
| `WithAttributes(reg *provider.AttributeRegistry)` | `ListAttributes` and the three attribute-cache invalidations — the gated, system-tier directory reads. It grants nobody anything: the decision path never resolves a bag through this option. |
| `WithRuleSource(base rules.RuleSource, fetcher rules.MetadataFetcher)` | The what-if preview of an **unsaved** rule via `Simulate`'s `Overlay.Rules`. |
| `WithClock(now func() time.Time)` | Override the facade clock used to stamp entity timestamps on writes (for deterministic tests). |

The `serve` command builds the fully-wired facade so HTTP, Twirp, and the CLI all
drive one mutation path. A decision-only surface can stay minimal with
`service.New(eng)`.

## Surface-neutral query types

The facade takes `Query` / `EnumerateQuery` — surface-neutral mirrors of the
engine's request types — so the CLI and HTTP layers marshal to and from these and
the engine's `Request` stays an engine-internal concern.

```go
type Query struct {
	Account   string
	Principal string
	Action    string
	Object    string
}

type Result struct {
	Allow            bool     // the verdict
	Reason           string   // names the deciding grants, or the fail-closed cause
	DecidingGrantIDs []string // empty on a default-deny or a fail-closed deny
}

type EnumerateQuery struct {
	Account    string
	Principal  string
	Action     string
	Pattern    string
	Fields     map[string]any // optional object-metadata predicates; nil filters nothing
	References []ReferenceEdge // optional reference edges; nil restricts nothing
	Limit      int
}

type ReferenceEdge struct {
	HolderType string // optional; empty means HolderID's terminal segment type
	HolderID   string // "account:acme/dataset:7"
	Field      string // a DECLARED reference field on the holder
}
```

### `EnumerateQuery.Fields` — the object-metadata filter

`Fields` narrows an enumeration by object metadata: an object the principal is
allowed to act on is returned only when its metadata satisfies **every**
predicate. Nil or empty — the default — filters nothing and does not consult a
metadata source at all, so existing callers are unaffected.

The facade passes the map to the engine **unchanged**. It parses nothing, coerces
nothing, and normalises nothing, and it never rewrites the caller's map. That is
deliberate: the predicate's meaning is
[`provider.Filter`'s `Fields` contract](../concepts/providers.md#the-filterfields-contract),
evaluated in exactly one place (`provider.MatchFields`). A surface that
re-interpreted a value on the way in — parsing `"5"` into a number, say — would
make an enumeration select objects a `Check` then denies.

```go
ids, err := svc.Enumerate(ctx, service.EnumerateQuery{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Pattern:   "account:acme/**",
	Fields:    map[string]any{"tier": "premium", "brands": "brand:Y"},
	Limit:     10,
})
```

The semantics a caller must know:

- **AND across keys.** Every predicate must hold.
- **Collections match by membership.** A list-valued *metadata* field matches
  when it contains the wanted value; a list-valued *want* is a container compared
  by equality.
- **An absent field never matches** — not even against a `nil` want.
- **Comparison is typed.** `int(5)` and `float64(5)` are one value; `"5"` is not
  `5`.
- **The filter runs before `Limit`.** Candidates are decided, then filtered, then
  truncated — so "the first 10 objects tagged `brand:Y`" searches every
  candidate. Truncating first would return a silently wrong answer.
- **The filter only subtracts.** It applies to candidates that already survived
  deny-overrides, so no predicate can surface an object `Check` would deny.

Failure is asymmetric on purpose, because a short answer reads as "no access":

| Situation | Result |
|---|---|
| No metadata source wired, or no provider for the candidate's object-type | `APERTURE_PROVIDER_UNREGISTERED` — never a silently empty list. Because the predicate runs per candidate, this only surfaces when the enumeration has at least one **allowed** candidate; an empty allowed set returns empty regardless of wiring. |
| The provider has no row for the object (`APERTURE_NOT_FOUND` from `Fetch`) | The object is **excluded** — every field is absent, and absent never matches. |
| Any other provider failure | Surfaced verbatim. |

Wire the source alongside the scope lister — it is the same registry:

```go
eng := engine.New(store,
	engine.WithScopeResolution(nil, engine.ScopeDeps{Lister: reg, Rules: rulesEngine}),
	engine.WithMetadata(reg))
```

Two notes for anyone refactoring this field:

- **Over Twirp**, `Fields` rides as `map<string, google.protobuf.Value>`, which
  carries every number as a **double**. An integer beyond **2^53** loses
  precision in transit and must be sent (and stored) as a string; a non-finite
  number (NaN, ±Inf) is rejected as `APERTURE_INVALID_INPUT`. See the
  [RPC reference](../surfaces/rpc-reference.md#enumeraterequestfields--the-object-metadata-filter).
- **`Fields` carries `json` and `jsonschema` struct tags** — the only tagged
  field on any `service` type. The MCP surface aliases this struct
  (`mcp.EnumerateIn`) and reflects its tool schema off it, so the `omitempty` is
  load-bearing: without it the reflected schema marks the predicate *required*
  and an agent cannot ask an unfiltered question at all.

### `EnumerateQuery.References` — the reference edges

`References` restricts an enumeration to the identities a holder object's
**declared reference field** contains — "the brands in dataset X". Nil or empty
— the default — restricts nothing.

```go
ids, err := svc.Enumerate(ctx, service.EnumerateQuery{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Pattern:   "account:acme/brand:*",
	References: []service.ReferenceEdge{{
		HolderID: "account:acme/dataset:x",
		Field:    "current_brands",
	}},
})
```

It is a **dereference, not a predicate**, which is why it is not spelled through
`Fields`: a brand carries no field naming its datasets, so no predicate on brand
can express the question. `Fields` answers the mirror image ("which datasets
contain brand Y?") because *that* side holds the field. The declaration side is
[Declared references](../concepts/providers.md#declared-references).

The facade carries the edges to the engine **unchanged** — it does not parse a
holder identity, infer a holder type, or check that a field is declared. Every
one of those is a decision with a disclosure consequence, and a surface that made
it separately would be a second place for the answer to differ.

- **`HolderType` is optional.** Empty means "whatever `HolderID`'s terminal
  segment type is"; when given it must agree with `HolderID`, and the engine —
  not this type — enforces that.
- **Several edges AND**, an edge **composes with `Fields`**, and both precede
  `Limit`.
- **Exactly one hop.** The identities an edge yields are never themselves
  dereferenced.
- The restriction is computed **once per enumeration**, against the same grants
  and subject set the candidates are decided with — so `EnumerateAs` checks the
  holder with the **impersonated** authority, not the operator's.

The failure modes are asymmetric on purpose, and the asymmetry *is* the security
model:

| Situation | Result |
|---|---|
| The principal may not read the holder | **Empty result, no error.** "You may not see dataset X" and "dataset X contains nothing you may see" must be indistinguishable, or the edge is an oracle for objects the caller was never allowed to know about. |
| The holder is absent, **inside** `Account`, and the caller is a member | `APERTURE_NOT_FOUND` — the ergonomics a typo deserves, confined to a caller already inside the account. |
| The holder is **outside** `Account` | **Empty**, whether or not it exists. This is the disclosure boundary. |
| The caller is not a member of `Account` | **Empty**, always — membership is decided before the holder is looked up. |
| A referenced identity no longer exists | **Skipped**, with a warning log and a `dangling_reference` note; never a failed decision. |
| The field is not a declared reference | `APERTURE_PROVIDER_REFERENCE_INVALID` — loud, never an empty list that would read as "no access". |
| The holder's type has no provider, or the engine has no reference source | `APERTURE_PROVIDER_UNREGISTERED` — likewise loud. |

Wire the source alongside the metadata source — it is the same registry again:

```go
eng := engine.New(store,
	engine.WithScopeResolution(nil, engine.ScopeDeps{Lister: reg, Rules: rulesEngine}),
	engine.WithMetadata(reg),
	engine.WithReferences(reg),
	engine.WithLogger(logger)) // where a dangling reference is reported
```

`References` carries the same `json` / `jsonschema` tags `Fields` does, for the
same MCP-reflection reason: `omitempty` on the slice keeps the edges *optional*,
while `HolderID` and `Field` are required properties of an edge and `HolderType`
is not.

### `SearchQuery` — resolving a name to an id

`SearchQuery` is `EnumerateQuery` plus a name, and `Search` returns
`[]SearchMatch`:

```go
matches, err := svc.Search(ctx, service.SearchQuery{
	Account: "acme", Principal: "alice", Action: "read",
	Pattern: "account:acme/brand:*",
	Query:   "nike",
	Limit:   5,
})
```

| Field | Meaning |
|---|---|
| `Query` | The free text. **Required**; an empty query is `APERTURE_INVALID_INPUT`, never "match everything". |
| `MatchFields` | Metadata field names to restrict matching to. Empty searches every field holding text. |
| `Fields` / `References` / `Limit` | Exactly what they mean on `EnumerateQuery`, except that `Limit` caps the finished **ranking** rather than the scan. |
| `MinScore` | The score floor, `[0,1]`; `<= 0` means `provider.DefaultMinScore`. |

Each `SearchMatch` carries `Object`, `Score`, the `Field` and `Value` that
produced the score, and the object's full `Metadata` inline — so labelling a
result set costs no further call.

The facade passes every field to the engine **unchanged**. In particular no
surface normalises the query text: case folding, punctuation stripping and
tokenising happen in exactly one place, so a name typed at the CLI, sent over
MCP, and passed in Go score identically.

Two properties are worth restating because they are what make the surface safe:
candidates are **decided before they are scored**, so a result is always a subset
of `Enumerate`'s for the same subject; and a score **ranks but never
authorizes**, so a caller acting on a selected id still `Check`s it. `Search`
requires `engine.WithMetadata` — without it the answer is
`APERTURE_PROVIDER_UNREGISTERED`, never an empty list. See
[the decision API](decision-api.md#search).

### `ObjectMetadata` and `ObjectMetadataBatch`

`ObjectMetadata(objectID)` returns one object's provider metadata;
`ObjectMetadataBatch(objectIDs)` returns many, aligned by index as
`[]engine.BatchResult[map[string]any]`, so a malformed or absent id carries its
own error without failing its siblings. Both require `WithProviders`.

Neither authorizes or filters — they label ids a caller already holds. The path
that *produces* ids a subject may see is `Enumerate`, or `Search` when the input
is a name; and since `Search` already returns metadata inline, most callers never
need the batch at all.

## Fail-closed rendering

The facade's reason for existing is one shared policy for turning an engine
outcome into a rendered decision:

| Engine outcome | Facade renders it as |
|---|---|
| A clean decision | Passes through unchanged. |
| An input-validation error (`APERTURE_INVALID_INPUT` / `APERTURE_IDENTITY_INVALID`) | Returned to the caller **verbatim** — the caller asked an ill-formed question, so the CLI renders a usage error and HTTP returns 400. Not a deny. |
| Any other engine error (unknown principal, storage fault, ...) | Folded **fail-closed into a deny** `Result` (`Allow: false`, cause in `Reason`, `Err` nil). A decision point must never fail open. |

This rule is applied per operation as follows.

### Check (fail-closed)

```go
func (s *Service) Check(ctx context.Context, q Query) (Result, error)
```

`Check` returns an `error` **only** for a genuine input-validation failure; every
other engine failure folds into a fail-closed deny `Result` with a `nil` error.
On a clean render the decision is audited (sampled, asynchronous, off the hot
path).

```go
res, err := svc.Check(ctx, service.Query{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Object:    "account:acme/project:atlas/document:42",
})
if err != nil {
	// only a malformed query reaches here (APERTURE_INVALID_INPUT / _IDENTITY_INVALID)
	return err
}
fmt.Println(res.Allow, res.Reason) // an operational failure is res.Allow == false, err == nil
```

### Enumerate and Explain (verbatim errors)

```go
func (s *Service) Enumerate(ctx context.Context, q EnumerateQuery) ([]string, error)
func (s *Service) Explain(ctx context.Context, q Query) (engine.Trace, error)
```

`Enumerate` and `Explain` return engine errors **verbatim** for the surface to map
to a status. `Enumerate` cannot fail open by construction — every id it returns is
one `Check` allows — so an operational failure is a returned error, not a silent
partial set. `Explain` is a diagnostic, not an enforcement gate; its
`engine.Trace` is the public contract surfaces serialize.

That trace carries `Notes` — six kinds today: `shape_mismatch`, `absent_field`,
`date_invalid`, `date_bounds_inverted`, `dangling_reference`, and
`attributes_floor_only` — and it carries
[`Attributes`](decision-api.md#the-attribute-bags), the `principal` and `account`
bags the rules were evaluated against, **values included**. That is a deliberate
disclosure and the one place a trace carries values: the two bags are the
subjects of the very request being explained. Callers gate `Explain` accordingly.

### Batch forms

`CheckBatch`, `EnumerateBatch`, and `ExplainBatch` return per-item
`engine.BatchResult[T]` aligned with their queries. `CheckBatch` renders each item
exactly as `Check` (operational error → deny `Result`; input-validation error →
item `Err`); the other two carry engine errors verbatim per item. See
[Batch operations](batch.md).

## Simulate — what-if

The facade adds a **read-only** what-if surface: `Simulate` and `SimulateExplain`
answer *"what would the decision be if these hypothetical entities existed?"*
without ever persisting them. It is the seam the MCP Simulate tool and the what-if
simulator UI drive.

```go
func (s *Service) Simulate(ctx context.Context, ov Overlay, q Query) (Result, error)
func (s *Service) SimulateExplain(ctx context.Context, ov Overlay, q Query) (engine.Trace, error)
```

Both require the entity surface (`WithStorage`) so there is a base store to
overlay. `Simulate` carries the same fail-closed contract as `Check`;
`SimulateExplain` returns the trace verbatim like `Explain`. **Nothing is written
and nothing is audited** — a simulation is not a real decision.

### The overlay

`Overlay` is the set of hypothetical entities a run layers over the live model.
Every field is additive and optional; an overlay entity with the same id as a
stored one shadows it (so a what-if can model an edited grant or a re-roled
principal), and ids absent from the overlay fall through to storage.

```go
type Overlay struct {
	Principals  []model.Principal  // hypothetical or shadowing principals
	Groups      []model.Group      // hypothetical groups (union with stored memberships)
	Permissions []model.Permission // hypothetical or shadowing permissions
	Grants      []model.Grant      // the hypothetical grants — the common what-if input
	Memberships []model.Membership // hypothetical account memberships (consulted under enforcement)
	Rules       []model.Rule       // an unsaved rule being previewed (needs WithRuleSource)
}
```

The mechanism is structural, not conventional: `Simulate` builds a transient
engine (`e.WithStore(overlay)` — same coverer, membership policy, and clock as the
live engine, just a different read source) whose overlay store's writes are all
inert. A simulation *physically cannot* persist through it.

### Worked example: "what if I bestowed this grant?"

Suppose `bob` currently cannot read `document:42`, and you want to preview the
effect of a new allow grant before bestowing it. Layer the hypothetical grant (and
the permission it references, if not already stored) into an `Overlay` and ask
`SimulateExplain` — the trace shows *which* hypothetical grant decided the
verdict.

```go
import (
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"
)

ov := service.Overlay{
	Grants: []model.Grant{{
		ID:           "sim-grant-1",
		AccountID:    "acme",
		Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: "bob"},
		PermissionID: "perm-doc-read",
		Effect:       model.EffectAllow,
		Object:       "account:acme/project:atlas/**",
	}},
}

tr, err := svc.SimulateExplain(ctx, ov, service.Query{
	Account:   "acme",
	Principal: "bob",
	Action:    "read",
	Object:    "account:acme/project:atlas/document:42",
})
if err != nil {
	return err
}
fmt.Print(tr.String()) // shows sim-grant-1 as the deciding grant — nothing was written
```

Because `Simulate` reuses the engine's exact resolution, a hypothetical **deny**
overlay grant correctly carves out a stored allow, and a shadowing principal
models "what if alice had role X" — all without a write.

`SimulateExplain` returns the same `engine.Trace` `Explain` does, attribute bags
included, because it runs the live engine over an overlay store. One nuance:
when the overlay carries **unsaved rules**, those rules are evaluated through a
transient rule engine built for the overlay alone, which carries no attribute
resolvers — so a previewed rule reads the floor bags (`principal.{id, kind}`,
`account.{id}`) and nothing else, and earns an `attributes_floor_only` note if it
names a host-defined field. A simulation cannot inject an attribute bag either:
`Overlay` has no field for one.

### Related what-if reads

Two adjacent reads support the rule-builder's what-if and require
`WithProviders`:

- `ObjectMetadata(ctx, objectID) (map[string]any, error)` — the provider metadata
  a rule preview evaluates against.
- `EvaluateRule(ctx, ast *rules.Node, objectID) (bool, map[string]any, error)` —
  compiles an unsaved rule AST and evaluates it against one object's metadata,
  returning the boolean result and the metadata snapshot it saw.
- `EvaluateRulePreview(ctx, ast *rules.Node, objectID) (RulePreview, error)` —
  the same evaluation with the diagnostics a rule builder renders: `Result`,
  `Object`, `Now` (the reference instant, from the facade clock), `Bounds` (each
  relative-date operand and the concrete date it resolved to at `Now`), and
  `Notes` (the evaluation's deny-safe observations). `EvaluateRule` is its
  narrow projection.

  Unlike `Check` / `Enumerate` / `Explain`, this path compiles the AST directly
  instead of going through the decision engine, so it must supply the reference
  instant itself — a `rules.Input` with a zero `Now` has none, and every relative
  date correctly resolves to nothing.

  It also supplies **no principal and no account input at all** — not even the
  engine's floor. A rule reading `principal.tier` in the preview sees nothing,
  and no `attributes_floor_only` note is recorded, because there is no resolver
  behind it whose silence could be reported. This is the disclosure boundary of
  the whole preview surface: it is the wider-audience one (a rule author's
  editor), and handing it a principal bag would turn "evaluate this draft rule"
  into a read oracle for the principal directory — name a subject, compare a
  field, read the answer off the verdict. `Explain` takes the opposite side on
  purpose.

`ObjectIdentifiers(ctx, objectType, exclude...)` (also `WithProviders`) enumerates
a type's complete instance set minus any excluded ids — the positive allow-list an
exclusive allowance materialises to.

## The attribute directory — a system-tier read

`WithAttributes` adds the **one** administrative door onto an attribute slot.
Listing the `user` slot returns the host's whole user table, keys and bags
together, so every method here is gated **directly** through
`authz.Gate.RequireSystemAdmin` — the shape `Export` uses — rather than through a
mutation tier. Nothing here writes, and nothing here is audited.

```go
recs, err := svc.ListAttributes(ctx, actor, "user", provider.AttributeFilter{
	Fields: map[string]any{"department": "eng"},
	Limit:  100,
})
```

| Method | Does |
|---|---|
| `ListAttributes(ctx, actor, slot, filter)` | a page of one slot's directory, narrowed by the same `Fields` predicate `Enumerate` applies to object metadata |
| `InvalidateAttribute(ctx, actor, slot, id)` | drop one subject's cached bag; reports whether one was present |
| `InvalidateAttributeSlot(ctx, actor, slot)` | drop a whole slot's cache |
| `InvalidateAllAttributes(ctx, actor)` | drop every slot's cache; names no slot, so it cannot probe which exist |
| `ExplainAttributeAuthority(ctx, actor)` | the `engine.Trace` behind the authority decision above — deliberately **not** gated on holding that authority, or only the operators who were allowed could find out why the refused ones were not |

Three properties are contract, not implementation:

- **The gate runs before the slot is parsed.** A refused caller gets the
  identical `APERTURE_AUTHZ_DENIED` — nil slice, no count, no partial page — for
  a populated slot, an unregistered slot, and a slot name that does not exist, so
  a refusal cannot be used to probe which directories a deployment wires. A
  system-admin does get the real diagnostics.
- **No gate wired means `APERTURE_UNIMPLEMENTED`**, never "unrestricted": a bulk
  directory read has no narrower fallback to degrade to.
- **The decision path's fetch is not gated, and must never be.** A decision
  resolves one bag for a subject it already named, through the rules engine's
  resolvers — never through this option. The two paths reach the same registry
  through different seams.

Invalidation is a **security control**, not a performance knob: a slot's TTL is
the window a revoked clearance keeps authorizing for, and these methods are how
an operator closes it now instead of waiting it out. They clear the caches of
**this process** only.

## Related

- [Decision API](decision-api.md) — the raw engine operations the facade wraps.
- [Batch operations](batch.md) — the facade's per-item batch rendering.
- [Impersonation](impersonation.md) — the engine's `*As` operations.
- [Library quickstart](../getting-started/library-quickstart.md) — the facade
  wired end to end.
