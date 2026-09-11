# Decision API

The `engine` package is Aperture's Policy Decision Point. It exposes three single
operations — `Check`, `Enumerate`, and `Explain` — on `*engine.Engine`. This page
gives their real signatures, input and result shapes, and the rule for choosing
between them. Their bulk and impersonation-aware forms live in
[Batch operations](batch.md) and [Impersonation](impersonation.md).

All three are methods on an engine you construct once and reuse. The engine is
stateless beyond its storage handle and safe for concurrent use to whatever
degree the underlying `Storage` is.

## Constructing the engine

```go
func New(store model.Storage, opts ...Option) *Engine
```

With no options the engine uses the literal identity-pattern coverer — a grant's
object pattern is matched directly against the requested object. The options
extend that behaviour:

| Option | Effect |
|---|---|
| `WithScopeResolution(registry *scope.Registry, deps ...ScopeDeps)` | Consult each grant's pluggable scope resolver (selected by its permission's scope-strategy) for object membership, instead of only literal pattern matching. A `nil` registry uses `scope.DefaultRegistry()`. |
| `WithEnumerateLimit(n int)` | Set the ceiling an [enumeration](#enumerate) is bounded by: the value a request with a non-positive `Limit` receives, and the value a larger `Limit` is clamped down to. It is also stamped into the `ScopeDeps` the engine wires, so the member gather and the result cap are one number. A non-positive `n` is **normalised** to `DefaultEnumerateLimit`, never stored — a silently zero bound would make every enumeration read as "no access". |
| `WithMembershipEnforcement()` | Require the request's principal to be a member of the active account before any grant is consulted. A non-member is denied at the door (a fail-closed default-deny), rather than erroring. Off by default. |
| `WithMetadata(f MetadataFetcher)` | Supply the object-metadata source `Enumerate`'s [`Fields` filter](#filtering-by-object-metadata) reads through — normally the same `*provider.Registry` wired as the scope lister. Consulted **only** by a request that carries `Fields`. |
| `WithReferences(r ReferenceSource)` | Supply the declared-reference source `Enumerate`'s [reference edges](#restricting-through-a-declared-reference) are dereferenced through — normally the same `*provider.Registry` again. Consulted **only** by a request that carries `References`; an engine wired without it fails *loudly* for one that does, never with an empty result. |
| `WithLogger(l *slog.Logger)` | The sink for non-fatal operational findings — a skipped [dangling reference](#restricting-through-a-declared-reference), and an [enumeration that came back on its bound](#a-result-on-the-bound-is-a-warning). Both are invisible to an operator otherwise, since neither changes the result's shape. Nil, or unset, means `slog.Default()`. It is **not** a decision log: nothing on the `Check` hot path logs. |
| `WithClock(now func() time.Time)` | Override the engine clock. It governs impersonation time-box expiry only; the non-impersonated path never reads it. Production uses `time.Now`. |

Two further seams return a **shallow copy** of the engine rather than mutating it,
for the read-only what-if paths: `(*Engine).WithStore(store)` re-points the copy
at a different (e.g. overlay) store, and `(*Engine).WithRuleEvaluator(re)`
redirects rule-backed scope strategies at a different rule evaluator. Both leave
the original engine untouched, so a live engine and a transient what-if engine
never interfere. The facade's [`Simulate`](service-facade.md#simulate--what-if)
path is built on exactly these.

## Check

```go
func (e *Engine) Check(ctx context.Context, req Request) (Decision, error)
```

`Check` resolves a single authorization decision: *may this principal take this
action on this one object, in this account?*

### Request

`Request` is a value type; every field is mandatory.

```go
type Request struct {
	Account   string // active account the decision is scoped to
	Principal string // id of the principal requesting access
	Action    string // the verb being attempted, e.g. "read"
	Object    string // canonical object-identity string
}
```

`Principal` is a principal **id** (the key storage and the subject set are keyed
on), not the principal's identity string. `Object` is a canonical object-identity
string such as `account:acme/project:atlas/document:42`. Grants stamped to any
account other than `Account` are never consulted (the sole exception is a grant
stamped to the account wildcard `*`, which spans all tenancies).

### Decision

```go
type Decision struct {
	Allow            bool                  // the verdict: true permits, false denies
	Reason           string                // human-readable explanation naming the deciding grant(s)
	DecidingGrantIDs []string              // ids of the grant(s) that produced the verdict, sorted; empty on a default-deny
	Impersonation    *ImpersonationContext // non-nil only under an active impersonation session (see Impersonation)
}
```

`Reason` names the deciding grant(s), their specificity, and how many grants were
considered. `DecidingGrantIDs` is sorted for determinism and is empty on a
default-deny. `Impersonation` is `nil` on the ordinary path.

### Error contract

`Check` **never** returns an allow-on-error. Any operational failure — a
malformed request, an unknown principal, a storage fault — is returned as an
`APERTURE_*` coded error and the caller treats it as a *non-decision*. A
well-formed request that simply matches no grant is a clean default-deny
(`Allow: false`, no error). Default-deny is the floor: with no candidate grant
the answer is DENY.

```go
dec, err := eng.Check(ctx, engine.Request{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Object:    "account:acme/project:atlas/document:42",
})
if err != nil {
	// operational failure — not a decision
	return err
}
fmt.Println(dec.Allow, dec.Reason)
```

> Building your own surface? Prefer the facade's
> [`Service.Check`](service-facade.md#check-fail-closed), which folds operational
> errors into a fail-closed deny so a decision point can never fail open. The raw
> `engine.Check` here returns those errors for the facade to render.

## Enumerate

```go
func (e *Engine) Enumerate(ctx context.Context, req EnumerateRequest) ([]string, error)
```

`Enumerate` is the inverse of `Check`: it returns the object ids under a pattern
that the principal may take the action on, in the active account. Every id it
returns is one `Check` would allow — a denied object is **never** returned — so
the two operations agree by construction.

### EnumerateRequest

```go
type EnumerateRequest struct {
	Account    string          // active account the enumeration is scoped to
	Principal  string          // id of the principal whose access is enumerated
	Action     string          // the verb being enumerated, e.g. "read"
	Pattern    string          // identity pattern bounding the search
	Fields     map[string]any  // optional object-metadata predicates; nil/empty filters nothing
	References []ReferenceEdge // optional reference edges; nil/empty restricts nothing
	Limit      int             // caps the number of returned ids; <= 0 means the default bound
}
```

`Pattern` both bounds the candidate set and is intersected with each grant's own
scope — for example `account:acme/**` (everything in the account) or
`account:acme/document:*` (every document at the account root). `Limit` caps the
result; a non-positive `Limit` (or one above the bound) is clamped to the bound
the engine was configured with, so an enumeration can never materialise an
unbounded set. That bound is `engine.WithEnumerateLimit`'s value, or
`engine.DefaultEnumerateLimit` (1000) on an engine built without it. Object order
is deterministic (sorted by canonical id).

```go
ids, err := eng.Enumerate(ctx, engine.EnumerateRequest{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Pattern:   "account:acme/project:atlas/**",
	Limit:     100,
})
```

An operational failure — a storage fault, an unresolvable scope strategy, or an
unconfigured object lister an implicit/exclusive grant needs — is returned as a
coded error, never a silent partial set.

### A result on the bound is a warning

A truncated result and a complete one are the same `[]string`. When an
enumeration comes back holding **exactly** its effective bound, the engine
therefore says so through `WithLogger` (`slog.Default()` when none is wired):

```
WARN engine: enumeration returned exactly its bound; the result may be truncated
  bound=100 configured_bound=1000 requested_limit=100
  account=acme action=read pattern=account:acme/project:atlas/**
```

`bound` is the cap this enumeration actually ran under — your own `Limit` when
that was the smaller of the two — and `configured_bound` is the engine's ceiling.
A result **below** the bound logs nothing.

The wording is deliberate: this is a **hint, not an assertion**. A complete set
that happens to be exactly that size is indistinguishable from a truncated one
here, so the line never claims anything was dropped. `Enumerate`'s return shape
is unchanged — there is no truncation flag, and a caller still cannot tell the
two apart. The intended response is an operator's, not a caller's: raise the
bound (or the request `Limit`) and ask again.

It is raised at the engine only. One bound governs the engine clamp, the scope
member gather (`scope.Deps.MaxMembers`) and the provider list alike, so a starve
further down surfaces here as a result sitting on the bound.

### Filtering by object metadata

`Fields` is an optional set of object-metadata predicates. An allowed candidate
is returned only when its metadata satisfies **all** of them; a nil or empty map
(the default) filters nothing and never touches a metadata source, so an
unfiltered enumeration costs exactly what it always did.

```go
ids, err := eng.Enumerate(ctx, engine.EnumerateRequest{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Pattern:   "account:acme/**",
	Fields:    map[string]any{"tier": "premium", "brands": "brand:Y"},
	Limit:     10,
})
```

The meaning is
[`provider.Filter`'s `Fields` contract](../concepts/providers.md#the-filterfields-contract)
verbatim, evaluated by the same `provider.MatchFields` a provider's own `Query`
calls — so an enumeration filtered here and one filtered inside a provider select
the same objects. **AND** across keys; a collection field matches by
**membership**; an **absent field never matches**, not even a `nil` want; and
comparison is **typed** (`int64(5)` matches a `float64(5)` want, `"5"` does not
match `5`).

Two orderings are load-bearing:

1. **Deny first.** The predicate runs on candidates that already survived
   deny-overrides and specificity, so it can only *subtract* from the allowed
   set. A denied object is never returned, whatever the predicate says.
2. **Filter before `Limit`.** The candidate set is predicated *before* it is
   truncated, so asking for the first 10 objects tagged `brand:Y` searches every
   candidate rather than tagging the first 10 candidates and returning the few
   that stuck.

Metadata is read through `engine.WithMetadata(fetcher)`, whose seam
(`MetadataFetcher`) has the signature of `*provider.Registry.Fetch` — the same
registry that backs the scope lister and the rule evaluator, so a candidate is
served from the per-type cache the enumeration already warmed:

```go
eng := engine.New(store,
	engine.WithScopeResolution(nil, engine.ScopeDeps{Lister: reg, Rules: rulesEngine}),
	engine.WithMetadata(reg))
```

Failure is deliberately asymmetric — an enumeration returning fewer objects reads
as "no access", one returning more is an authorization bug:

- **no metadata source wired, or no provider for the candidate's object-type** →
  `APERTURE_PROVIDER_UNREGISTERED`, never a silently empty result. The predicate
  runs per candidate, so this only surfaces once the enumeration has at least one
  allowed candidate; an empty allowed set returns empty regardless of wiring.
- **the object has no metadata row** (`APERTURE_NOT_FOUND` from `Fetch`) → every
  field is absent, absent never matches, so the object is **excluded**.
- **any other provider failure** → returned verbatim.

`EnumerateBatch` and `EnumerateAs` carry `Fields` through the same path.

### Restricting through a declared reference

`References` restricts the enumeration to the identities a holder object's
**declared reference field** contains — "the brands in dataset X". Nil or empty
(the default) restricts nothing.

```go
ids, err := eng.Enumerate(ctx, engine.EnumerateRequest{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Pattern:   "account:acme/brand:*",
	References: []engine.ReferenceEdge{{
		HolderID: "account:acme/dataset:x", // HolderType is optional
		Field:    "current_brands",         // must be a DECLARED reference
	}},
})
```

**It is a dereference, not a filter, and the distinction is the whole reason it
exists.** `Fields` answers "which datasets contain brand Y?" because the dataset
holds `current_brands`. A brand holds no field naming its datasets — references
are declared on the [holding side
only](../concepts/providers.md#declared-references) — so no predicate on brand
can express "which brands belong to dataset X?" at all.

Composition mirrors the filter's: several edges **AND**, an edge composes with
`Fields`, both apply **before `Limit`**, and the restriction can only *subtract*
from the allowed set. Exactly **one hop** is taken — the identities an edge
yields are never themselves dereferenced. The whole restriction is resolved
**once per enumeration**, before candidates are gathered, against the same grants
and subject set the candidates are decided with; that is what makes
`EnumerateAs` check the holder with the **impersonated** authority.

The failure modes are asymmetric, and the asymmetry is the security model:

| Situation | Result |
|---|---|
| The principal **may not read the holder** | **Empty result, no error.** "You may not see dataset X" and "dataset X contains nothing you may see" have to be indistinguishable, or the edge is an oracle for objects the caller was never allowed to know about. |
| The holder is **absent**, inside `Account`, caller is a **member** | `APERTURE_NOT_FOUND` — the ergonomics a typo deserves, confined to a caller already inside the account. Existence is resolved *before* the holder's check precisely so this answer survives. |
| The holder is **outside** `Account` | **Empty**, whether or not it exists. The disclosure boundary. |
| The caller is **not a member** | **Empty**, always — membership is decided before the holder is looked up. |
| A referenced identity **no longer exists** | **Skipped**, with a warning log naming it and a `dangling_reference` note that does not. An application-level foreign key has no database constraint behind it, so one deleted brand must not fail every decision on the dataset that still lists it. |
| The field is **not declared**, the holder type has **no provider**, or no reference source is wired | `APERTURE_PROVIDER_REFERENCE_INVALID` / `APERTURE_PROVIDER_UNREGISTERED` — loud, never an empty list. A wiring fault describes the deployment, not the data. |
| A reference value **does not point at the declared target** | `APERTURE_PROVIDER_REFERENCE_MISMATCH` — never a silently dropped value. |

Wire the source alongside the metadata source; it is the same registry:

```go
eng := engine.New(store,
	engine.WithScopeResolution(nil, engine.ScopeDeps{Lister: reg, Rules: rulesEngine}),
	engine.WithMetadata(reg),
	engine.WithReferences(reg),
	engine.WithLogger(logger))
```

A **rules-engine dereference is deliberately not supported**: `Check` owes a p99
under a millisecond, and following a reference inside rule evaluation is a join
on the decision hot path with a recursive cache-miss path behind it. Enumeration
computes the restriction once, off that path.

`EnumerateBatch` and `EnumerateAs` carry `References` through the same path.

## Explain

```go
func (e *Engine) Explain(ctx context.Context, req Request) (Trace, error)
```

`Explain` resolves the request *exactly* as `Check` does but records the full
derivation instead of only the verdict. Use it as a diagnostic — the "why" behind
a verdict — not as an enforcement gate. It takes the same `Request` as `Check`.

### Trace

`Trace` is a **stable public contract**: the RPC surface, the MCP inspect tool,
and the what-if simulator all serialize it, so its fields are part of the API.

```go
type Trace struct {
	Request        Request               // the question that was asked
	Subjects       []model.Subject       // the principal's expanded subject set (itself, roles, groups)
	Considered     []GrantEvaluation     // every grant loaded, each tagged with how it fared
	MaxSpecificity int                   // top specificity among covering candidates; 0 when nothing covered
	Notes          []EvaluationNote      // diagnostics rule evaluation recorded; empty when no rule ran
	Attributes     TraceAttributes       // the `principal` and `account` bags the rules read; zero when no rule ran
	Now            time.Time             // the reference instant rule evaluation resolved against; zero when no rule ran
	Decision       Decision              // the final verdict — identical to what Check returns
	Impersonation  *ImpersonationContext // non-nil only under an active impersonation session
}
```

Each entry in `Considered` is a `GrantEvaluation` recording one grant's
contribution — its subject, permission, effect, object pattern, whether its
action matched, whether it covered the object and at what specificity, which
scope strategy it used, whether it was a deciding grant, and a short
human-readable `Outcome`. A grant that failed the action match is still listed
(with `ActionMatched: false`) so the trace shows what was ruled out.

### Evaluation notes

`Notes` is where a rule-backed scope explains itself. A rule can decide `false`
for a reason the verdict never shows — the metadata field it reads is the **wrong
shape** (a string where an array was meant), or it **matched only because the
field is absent**. Both are deny-safe by policy: a collection operator over a
non-collection evaluates to `false` rather than raising `APERTURE_RULE_EVAL`, so
one mistyped field cannot break every decision that touches it. See
[Rules](../concepts/rules.md#wrong-shaped-fields-deny-and-are-recorded).

```go
type EvaluationNote struct {
	GrantID  string // the grant whose scope resolution recorded it
	Rule     string // the rule reference that was evaluated
	Kind     string // "shape_mismatch" | "absent_field" | "date_invalid" |
	                //   "date_bounds_inverted" | "dangling_reference" | "attributes_floor_only"
	Op       string // the comparison operator ("hasAll", …)
	Path     string // the dotted variable path ("object.tags")
	Expected string // the shape the operator requires ("collection")
	Actual   string // the shape found ("string")
	Message  string // "object.tags: expected collection, got string"
}
```

Six kinds are recorded today: `shape_mismatch`, `absent_field`, `date_invalid`,
`date_bounds_inverted`, `dangling_reference` (an enumeration skipped a declared
reference whose target no longer exists), and `attributes_floor_only` (a rule
read a **host-defined** field off `principal` or `account` while that root
carried nothing but the engine's floor, so every such comparison was false).

Three rules govern them:

1. **Diagnostic only** — a note never influences a verdict.
2. **`Explain` only** — `Check` and `Enumerate` install no collector, so they
   record nothing, allocate nothing, and behave exactly as before.
3. **Shape and path only** — never a metadata value, never anything that could
   cross an account boundary, the same discipline error messages follow.

### The attribute bags

`Attributes` carries the `principal` and `account` roots this decision's rules
were evaluated against — the host's bag with the engine's floor stamped over it,
exactly as a rule read it:

```go
type TraceAttributes struct {
	Principal map[string]any `json:"principal,omitempty"`
	Account   map[string]any `json:"account,omitempty"`
}
```

Both are nil on a decision that evaluated no rule, and `Account` is *also* nil for
a decision made at the account wildcard `"*"` — that is not an account, so no bag
is resolved for it and none is invented here.

**This field deliberately discloses VALUES**, and it is the one place a `Trace`
does. It is not an oversight to fix, and it does not contradict rule 3 above: a
note is produced by a comparison against an *object's* metadata, on whatever
objects a wide decision happened to sweep, while these two bags are the
**subjects of the very question being asked** — the principal in
`Request.Principal` and the account in `Request.Account`. A trace that discloses
them tells the asker about their own decision and nothing else.

The disclosure is also the point: *"why was this denied?"* is unanswerable from a
grant list when the deciding comparison was `principal.tier == "gold"` and the
operator cannot see that the tier is `"silver"`, or absent entirely. So do not
redact these into a note, and do not widen them past those two subjects. The
wider-audience [rule what-if
preview](service-facade.md#related-what-if-reads) takes the opposite side on
purpose and supplies no principal or account input at all, so a rule author's
editor cannot become a directory read oracle.

`Trace` implements `String()`, which renders an operator-readable, deterministic
report:

```go
tr, err := eng.Explain(ctx, engine.Request{
	Account:   "acme",
	Principal: "alice",
	Action:    "read",
	Object:    "account:acme/project:atlas/document:42",
})
if err != nil {
	return err
}
fmt.Print(tr.String())
```

`tr.Decision` is byte-for-byte the decision `Check` returns for the same request,
so a surface can render a verdict and its explanation from a single `Explain`
call. The rendered report prints each non-empty attribute root as one
key-sorted line **before** the grants — they are the input the grants' rules were
judged against — and stays byte-identical for the same decision.

## When to use each

| Question | Operation |
|---|---|
| "May this principal do this one thing?" — an enforcement gate on the hot path. | `Check` |
| "Which of these objects may this principal act on?" — building a filtered listing or a picker. | `Enumerate` |
| "Why did that decision come out the way it did?" — a diagnostic, an audit view, a support tool. | `Explain` |

`Check` is the allocation-conscious hot path; reach for it in enforcement.
`Enumerate` is the most cache-sensitive op and is deliberately bounded — use it to
answer "what can they see", not as a substitute for repeated `Check`s on a known
object. `Explain` does the same work as `Check` plus recording the derivation, so
use it when a human (or a machine) needs to understand the verdict, not on every
hot-path call.

## Related

- [Batch operations](batch.md) — resolve many of these in one call.
- [Impersonation](impersonation.md) — the `*As` variants that resolve over an
  elevated subject set.
- [The service facade](service-facade.md) — the fail-closed wrapper surfaces
  call.
- [Concepts primer](../getting-started/concepts.md) — deny-overrides, specificity,
  and the subject set these operations resolve over.
