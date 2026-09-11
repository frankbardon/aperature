---
name: decision-api
description: The full decision API beyond Check — Enumerate (which objects a principal may act on), Explain (the structured decision trace), and bulk-batched forms of all three, all behind one service facade.
applies_to: [engine, service, twirp, mcp, what-if]
---

# Decision API

Aperture's Policy Decision Point answers three questions, each single and
bulk-batched (FR-10). All surfaces — the HTTP/Twirp service (E4-S1), the MCP
read-and-simulate tools (E4-S3), and the what-if simulator (E6-S4) — call ONE
facade (`service.Service`) over the engine, so the fail-closed policy and the
trace contract live in one place.

## The three ops

- **Check** `(account, principal, action, object) -> Decision{Allow, Reason,
  DecidingGrantIDs}`. The enforcement gate; deny-overrides with a specificity
  tiebreak. The hot path (NFR p99 < 1ms).
- **Enumerate** `(account, principal, action, pattern, fields, limit) -> []objectID`.
  The inverse of Check: which objects under `pattern` the principal may act on,
  optionally narrowed by object-metadata predicates (`fields`). Every id returned
  is one Check would allow.
- **Explain** `(account, principal, action, object) -> Trace`. The full
  derivation: the subject set, every grant considered with its per-grant
  outcome, which grants decided the verdict, and the final Decision.

Each has a bulk form — `CheckBatch`, `EnumerateBatch`, `ExplainBatch` — that
takes many queries and returns results **aligned by index** (`result[i]` for
`query[i]`).

## Account isolation & membership

Every decision is scoped to an **active account** (`Request.Account`), and the
(principal, active-account) pair is a hard isolation boundary (FR-14): a
multi-account principal's grants in one account NEVER apply in another. This is
guaranteed at the storage seam — `GrantsForSubjects(account, subjects)` and
`GroupsForPrincipal` are account-scoped, so a grant stamped to another account is
never even loaded. The invariant holds identically under direct, role, group,
wildcard, and scope-strategy grants, and switching `Request.Account` changes the
effective grant set deterministically.

Accounts and memberships are first-class (`model.Account`, `model.Membership`):
a membership is the edge admitting a principal to an account. Enforcement of
membership is **opt-in**:

```go
eng := engine.New(store, engine.WithMembershipEnforcement())
```

With it on, a request whose principal is not a member of the active account is
denied at the door — a fail-closed default-deny (Check), an empty result
(Enumerate), and a deny Trace that considers no grants (Explain) — before any
grant is consulted. It is a defence-in-depth layer over the always-on
account-scoped grant query; off by default, deployments that model membership
purely through grants are unaffected.

## Enumerate is bounded

Enumerate is the most cache-sensitive op. It is deliberately bounded and never
enumerates unboundedly:

- Candidates come from each ALLOW grant's covered objects — a scope resolver's
  bounded `Members` (implicit/exclusive enumerate "all of type" through the
  provider `ObjectLister`; inclusive returns the union of its id-list and, when
  it declares a rule, the same all-of-type listing filtered by `Contains`;
  literal yields a concrete-identity grant or an explicit "{a,b,c}" id-set
  expanded to its members, but never a wildcard), intersected with the query
  pattern.
- **`Enumerate` never disagrees with `Check` about a grant.** A rule-backed
  inclusive grant enumerates list-then-filter — there is no reverse index, and no
  attempt to invert a rule, which is an arbitrary expression over object
  metadata. Its rule half needs the `ObjectLister`, so a rule-backed grant
  enumerated without one reports `APERTURE_SCOPE_LISTER_UNCONFIGURED` instead of
  quietly returning nothing; an empty member list is an answer, and a missing
  dependency must not be able to impersonate one.
- Each candidate is then run through the SAME deny-overrides/specificity
  decision as Check, so a candidate carved out by a more-specific or
  equal-specificity deny is dropped. A denied object is **never** returned.
- The result is capped by `Limit`, and each resolver's `Members` is itself
  bounded. Output order is deterministic (sorted by canonical id).

### The bound is configured, not compiled in

`engine.WithEnumerateLimit(n int)` sets the ceiling. A request `Limit <= 0`
receives it; a larger request `Limit` is clamped **down** to it. An engine built
without the option uses `engine.DefaultEnumerateLimit` (1000), which is the
default and not the ceiling.

A non-positive `n` is **normalised to the default**, never stored. An `Option`
cannot report an error, and a zero bound would turn every enumeration into an
empty result that reads exactly like "no access" — so a misconfiguration degrades
to the documented default. Parse and validate an operator-supplied value at the
surface that reads it, where an `APERTURE_CONFIG_INVALID` can still reach them.

The CLI is one such surface: `--enumerate-limit` / `APERTURE_ENUMERATE_LIMIT`
(flag > env > default). It is declared as a `ucli.StringFlag` and parsed by the
CLI on purpose — an `IntFlag` carrying the same env source lets urfave fail the
command with its own **uncoded** parse error before the action runs, so a
mistyped value would report something other than `APERTURE_CONFIG_INVALID`.

**The bound belongs to the process, not to `serve`.** Every command that decides
carries the same flag — `serve`, `check`, `enumerate`, `identifiers`, `explain`,
`mcp` — and the option is applied in the SHARED half of `internal/cli`'s
`buildDecisionStack`, never as one of the per-command `engOpts`. That split
exists so `serve` can add `--enforce-membership` without forcing it on the
one-shot commands, and it is exactly the wrong place for a bound: a deployment
configured to 1500 whose `aperture enumerate` still answered 1000 would be two
surfaces of one binary disagreeing about the same question, with nothing in
either answer saying so.

The CLI refuses `0` and every negative as well as `banana`, with the same code
and before the store is opened. That is not a contradiction of the normalisation
above, it is the point of it: the library absorbs a non-positive number because
an `Option` cannot report one, and the boundary rejects it because a human who
typed `-5` and got `1000` has no way to find out. **Lenient normalisation in the
library, input validation at the surface.** A surface that passes an operator's
number straight through inherits the silent fallback.

`WithScopeResolution` stamps the configured bound into the `ScopeDeps` the engine
keeps, as `Deps.MaxMembers`, whichever order the two options are passed in. The
stamp is **unconditional** and overwrites a `MaxMembers` a caller-built
`ScopeDeps` literal carried — `internal/cli` builds exactly such a literal. One
enumeration must not be governed by two numbers: a member gather bounded lower
than the engine's clamp truncates the set before the engine ever sees it, and
nothing in the result says so. Configure the bound on the engine; the deps
inherit it.

### A result on the bound is a warning, not a flag

When an enumeration comes back holding **exactly** its effective bound, the
engine logs a WARN through `engine.WithLogger` (`slog.Default()` when none is
wired) naming the bound that was hit:

```
engine: enumeration returned exactly its bound; the result may be truncated
  bound=1000 configured_bound=1000 requested_limit=0
  account=acme action=read pattern=account:acme/**
```

Read the wording literally. It is a **hint, not an assertion** — a complete set
of exactly that size looks identical from inside the engine, and the log must
never be quoted as proof that anything was dropped. `bound` is the cap this
enumeration actually ran under (the caller's own `Limit` when that was smaller);
`configured_bound` is the engine's ceiling. A result **below** the bound logs
nothing.

This is **log-only** signalling: `Enumerate` still returns `([]string, error)`
and grows no truncation flag, so a caller still cannot distinguish a truncated
result from a complete one. That is the accepted cost of not breaking the return
shape — an operator who sees the warning re-asks with a higher bound.

The engine is the **only** place it is raised. One bound flows all the way down
(the engine clamp, `scope.Deps.MaxMembers`, the provider list), so a starve in
the scope or provider layer surfaces here as a result sitting on the bound;
`scope` and `provider` carry no logger and are not given one.

## Enumerate's metadata filter

`EnumerateRequest.Fields` (`map[string]any`, optional) narrows the result by
object metadata: an ALLOWED candidate is returned only when its metadata
satisfies every predicate. Nil or empty — the default — filters nothing and does
not consult a metadata source at all, so an unfiltered enumeration is unchanged
and needs nothing wired.

The meaning is `provider.Filter`'s `Fields` contract verbatim, evaluated by the
same `provider.MatchFields` a provider's `Query` uses, so an enumeration filtered
by the engine and one filtered inside a provider select the same objects:

- **AND across keys** — every predicate must hold.
- **A collection field matches by MEMBERSHIP**; a list-valued *want* is a
  container compared by equality.
- **An absent field never matches**, not even against a nil want.
- **Comparison is typed** — `int64(5)` matches a `float64(5)` want, `"5"` does
  not match `5`.

Two orderings are load-bearing:

1. **Deny first.** The predicate runs on candidates that already survived
   deny-overrides and specificity, so the filter can only ever SUBTRACT from the
   allowed set. No predicate surfaces an object `Check` would deny.
2. **Filter before `Limit`.** The candidate set is predicated *before* it is
   truncated, so "the first 10 datasets tagged brand:Y" searches every candidate
   rather than tagging the first 10 candidates. Truncating first would return a
   silently wrong answer.

Metadata is read through the `MetadataFetcher` seam (`engine.WithMetadata`),
whose signature is `*provider.Registry.Fetch` — and matches
`rules.MetadataFetcher` — so a deployment wires ONE source for the scope lister,
the rule evaluator, and the filter, and a candidate is served from the per-type
cache the enumeration already warmed. The returned map is read-only,
transitively.

Failure is deliberately asymmetric, because an enumeration returning fewer
objects reads as "no access" while one returning more is an authorization bug:

- no source wired, or no provider for the candidate's object-type →
  `APERTURE_PROVIDER_UNREGISTERED`, never a silently empty result. Note the
  predicate runs **per candidate**, so this only surfaces when the enumeration
  has at least one ALLOWED candidate; an empty allowed set returns empty
  regardless of wiring;
- the object has no metadata row (`APERTURE_NOT_FOUND` from `Fetch`) → every
  field is absent, absent never matches, so the object is EXCLUDED — the
  restrictive direction;
- any other provider failure → surfaced verbatim.

`EnumerateBatch` and `EnumerateAs` carry `Fields` through the same shared path.
For how each non-Go surface spells it — the Twirp `map<string,
google.protobuf.Value>` and its `>2^53` caveat, the CLI's `--field` /
`--fields-json` precedence, and the MCP schema's load-bearing `omitempty` — see
`skills/api-surface.md`.

## Enumerate's reference edges

`EnumerateRequest.References` (`[]ReferenceEdge`, optional) restricts the result
to the identities a holder object's DECLARED reference field contains — "the
brands in dataset X". Nil or empty restricts nothing and consults no reference
source.

It is a **dereference, not a predicate**, which is why it is not another entry in
`Fields`. `Fields` answers the mirror image ("which datasets contain brand Y?")
because the dataset holds the field; a brand holds no field naming its datasets,
so no predicate on brand can express the first question at all.

Composition mirrors the filter's: several edges **AND**, an edge composes with
`Fields`, both precede `Limit`, and restriction can only SUBTRACT. Exactly **one
hop** is taken. The restriction is resolved **once per enumeration**, before
candidates are gathered, against the same grants and subject set the candidates
are decided with — so `EnumerateAs` checks the holder with the impersonated
authority.

The failure modes are the security model, and they are asymmetric on purpose:

- an unreadable holder → **empty result, no error** (an error here is an oracle
  for objects the caller was never allowed to know about);
- an absent holder → `APERTURE_NOT_FOUND` **only** inside the request's account
  and **only** for a member; out of account, or for a non-member, empty;
- a dangling referenced identity → **skipped**, warning-logged, and noted as
  `rules.NoteDanglingReference`;
- an undeclared field, an unregistered holder type, or no reference source
  (`engine.WithReferences`) → a **coded error**, never an empty list that would
  read as "no access";
- a value not pointing at the declared target →
  `APERTURE_PROVIDER_REFERENCE_MISMATCH`.

A rules-engine dereference is deliberately **not** supported: it would be a join
on the `Check` hot path (p99 < 1ms) with a recursive cache-miss path behind it.

`EnumerateBatch` and `EnumerateAs` carry `References` through the same shared
path. The declaration side and the full reasoning are in
`skills/object-references.md`; the per-surface spellings are in
`skills/api-surface.md`.

## The Explain trace (public contract)

`engine.Trace` is serialized by E4 and E6, so its shape is part of the API:

- `Request` — the question asked.
- `Subjects` — the principal's expanded subject set (itself, roles, groups).
- `Considered []GrantEvaluation` — every loaded grant with `ActionMatched`,
  `Covered`, `Specificity`, `Strategy` (the scope strategy consulted),
  `Deciding`, and a human-readable `Outcome` note. Action-mismatched and inert
  (dangling-permission) grants are listed too, so the trace shows what was
  ruled out, not only what decided.
- `MaxSpecificity` — the tier the tiebreak resolved at.
- `Notes []EvaluationNote` — diagnostics rule evaluation recorded while resolving
  a grant's scope (see below). Empty when no rule was evaluated.
- `Attributes TraceAttributes` — the `principal` and `account` roots the rules
  were evaluated against, **values included**, each with the engine's floor
  stamped in exactly as a rule read it. Zero when no rule was evaluated;
  `Account` is also nil for a decision made at the account wildcard, which
  resolves no account bag at all.
- `Decision` — identical to what Check returns.

`Attributes` **deliberately discloses values**, and it is the one field that
does. A note is held to shape-and-path-only because it is produced by
comparisons against whatever objects a wide decision swept; these two bags are
the *subjects of the request being explained* — the principal in
`Request.Principal`, the account in `Request.Account` — so the trace tells the
asker about the decision they asked about and nothing else. It is also the point:
"why was this denied?" is unanswerable from a grant list when the deciding
comparison was `principal.tier == "gold"` and the operator cannot see that the
tier is `"silver"`. Do not redact it into a note, and do not widen it past those
two subjects. The what-if preview (`service.EvaluateRulePreview`) takes the
opposite side on purpose — it supplies **no** principal or account input, not
even the floor, so a rule author's editor cannot become a directory read oracle.

`Trace.String()` renders an operator-readable report (subjects, the attribute
roots the rules read, each grant's disposition, any evaluation notes, the verdict
and reason), with the deciding grants marked. The attribute lines are printed
with their keys sorted, and are omitted entirely when no rule ran.

The report is **deterministic**: two traces of the same decision render
byte-identically, so a trace can be diffed, snapshotted, or pasted into a bug
report. `Storage` promises no order for `GrantsForSubjects` or
`GroupsForPrincipal` — `storage/memory` answers both from a Go map — so `String`
sorts the subjects, considered grants and notes itself, on copies. The `Trace`
struct still carries its lists in storage order, which is what the Twirp and MCP
surfaces serialize; only the rendered report is normalised.

### Evaluation notes

A rule-backed scope can decide `false` for a reason the verdict does not show:
the metadata field it reads is the **wrong shape** (a string where an array was
meant), or it **matched only because the field is absent**. Both are deny-safe by
policy — a shape mismatch evaluates to `false` rather than raising
`APERTURE_RULE_EVAL` — and both would otherwise be invisible, so `Explain`
records them:

```
  evaluation notes (1):
     g-doc [rule tagged]: object.tags: expected collection, got string
```

A rule can also fail to select because the attribute bag it read came back
**floor-only** — no provider wired for the slot, or a directory with no record
for this subject — which is the same invisible `false`, and worse in an
exclusive grant, where a rule that stops selecting stops excluding:

```
  evaluation notes (1):
     g-alice [rule gold-only]: principal: floor-only; no host attributes were resolved, so every comparison against a host-defined field is false
```

Each `EvaluationNote` carries `GrantID`, `Rule`, `Kind` (`shape_mismatch` /
`absent_field` / `date_invalid` / `date_bounds_inverted` / `dangling_reference` /
`attributes_floor_only`), `Op`, `Path`, `Expected`, `Actual` and a rendered
`Message`. The kinds are defined in `skills/rules-engine.md`.

Three rules govern them:

1. **Diagnostic only.** Notes never influence a verdict.
2. **`Explain` only.** `Check` and `Enumerate` collect nothing and pay nothing;
   their behavior is unchanged.
3. **Shape and path only.** A note names the variable path and the shapes
   involved — **never a metadata value**, never anything that could cross an
   account boundary. Same rule as error messages. `Trace.Attributes` is the
   deliberate, separately-named exception described above; it is not a licence to
   put values in a note.

They reach every surface for free: the CLI prints `Trace.String()`, Twirp carries
the whole trace as `trace_json`, and the MCP `aperture_explain` tool returns
`engine.Trace` with a reflected schema.

## Fail-closed rendering

The facade renders engine outcomes per op:

- **Check / CheckBatch** keep the original contract: an input-validation error
  (`APERTURE_INVALID_INPUT` / `APERTURE_IDENTITY_INVALID`) is returned; every
  other engine error folds into a **deny** Result. A decision point never fails
  open.
- **Enumerate / Explain** return engine errors verbatim. Enumerate cannot fail
  open by construction (denied objects are excluded inside the engine), so an
  operational failure is a returned error, not a silent partial set. Explain is
  a diagnostic.
- The **batch** ops carry each item's error in its `BatchResult{Result, Err}`,
  so one bad query in a batch yields a per-item error and never fails the whole
  batch.

## Scoped-engine assembly

Enumerate/Explain over implicit/exclusive/rule strategies need the E2 pieces
wired together:

```go
eng := engine.New(store,
    engine.WithScopeResolution(nil,
        engine.ScopeDeps{Lister: providerRegistry, Rules: rulesEngine}),
    engine.WithMetadata(providerRegistry))
svc := service.New(eng)
```

`*provider.Registry` satisfies the `ObjectLister` seam, `*rules.Engine`
satisfies the `RuleEvaluator` seam, and the same registry satisfies
`MetadataFetcher` for `Enumerate`'s field predicates. The assembly is optional —
with no providers the literal default still works, Check never needs the lister
(membership is computable without enumeration), and an unfiltered Enumerate never
needs the metadata source. Note `WithMetadata` takes the **strict** registry, not
the lenient fetcher a rule uses: leniency is right for a rule (empty metadata,
rule denies) and wrong here, where it would turn a misconfiguration into an empty
result.

**Every Aperture surface assembles the same graph.** `internal/cli` builds it
once (`buildDecisionStack`) and `serve`, the one-shot commands — `check`,
`enumerate`, `identifiers`, `explain` — and `aperture mcp` all use that one
builder. This is
load-bearing, not tidiness: the one-shot commands used to construct
`service.New(engine.New(store))` with no rules engine and no scope resolution, so
a permission with a rule-backed strategy had no `RuleEvaluator`, the resolver
reported `APERTURE_SCOPE_RULE_UNCONFIGURED`, and the facade folded that into a
fail-closed deny. The CLI returned a **different verdict** from the server for
the same model. A surface that skips the assembly does not merely lose
diagnostics — it decides differently. `aperture mcp` was the last surface still
building a bare `engine.New(store)`; once the metadata filter landed, a filtered
`aperture_enumerate` there could only report `APERTURE_PROVIDER_UNREGISTERED`
however well the seed declared its objects. It now shares the builder.
