---
name: attribute-providers
description: The attribute seam — the three slots (user, machine, account) a decision resolves `principal.*` and `account.*` from, the shared value model, the floor bags {id, kind} and {id} that are stamped last and cannot be shadowed, the leniency contract and the exclusive-grant widening it leaves, the kind-dependence hazard that `principal.kind` exists to state, the account-neutrality obligation on globally-visible principal bags, the account wildcard short-circuiting to the floor, impersonation reading the effective subject, the per-decision memo, the cache TTL as a revocation window, the containment guarantee that attribute enumeration can never be scope resolution, the `attributes:` / `attribute_providers:` seed schemas and their slot-level precedence, the silent `get_all` bare-id trap, and the system-tier admin read behind `service.ListAttributes` and `aperture attributes`.
applies_to: [library, cli]
---

# Attribute providers

An **object provider** answers "what do you know about this OBJECT?". An
**attribute provider** answers "what do you know about the party ASKING?" — the
principal's department and clearance, the account's plan and region — so a rule
can be written about the asker instead of only about the thing being acted on:

```
principal.kind == "user" && principal.department == object.department
account.plan == "enterprise"
```

The two seams are not variants of each other. Object metadata is resolved **per
object**: a decision touching a thousand objects reads a thousand bags, each
describing something different. An attribute bag is resolved **once per
decision** and then read by every rule against every object in it. They sit on
opposite sides of the fan-out, and almost everything below follows from that.

Everything here is a property of code in `provider/` (`attribute.go`,
`attribute_registry.go`, `attribute_static.go`), `rules/` (`engine.go`,
`attributes.go`, `notes.go`), `seed/` (`attribute.go`, `attribute_provider.go`),
`csvprovider/attributes.go`, `sqlprovider/attributes.go`, `service/attributes.go`
and `internal/cli/attributes.go`.

## The three slots, and there is no fourth

`provider.AttributeSlot` is a **closed** set — `AttributeSlots()` returns it in
this order, and it is the definition of "every slot":

| Slot | Constant | Keyed by | Backs |
|---|---|---|---|
| `user` | `provider.AttributeSlotUser` | a principal id | `principal.*` for a human principal |
| `machine` | `provider.AttributeSlotMachine` | a principal id | `principal.*` for a service account / API client / job runner |
| `account` | `provider.AttributeSlotAccount` | an account id | `account.*` for the tenant a decision is made in |

The object `Registry` is an **open**, type-keyed map because the host's object
types are the host's business and Aperture cannot know them. The slots are not:
they are the parties a decision has, and a decision has exactly these. An open
map would let a host register a fourth "kind" of subject nothing in the engine
knows how to fetch — discovered at evaluation time as an empty bag, which is to
say as a silent denial. A further distinction is a **field in the bag**, never a
fourth slot.

`user` and `machine` are separate slots rather than one slot with a field
because in every host that has both, the two are served by different systems; one
provider forced to answer for both would have to invent an empty bag for the half
it does not know.

The slot keys are opaque string constants declared in `provider`, not
`model.PrincipalKind` values, because **`provider` imports only `identity` and
`errors`** (enforced by `TestProviderPackageImportsOnlyIdentityAndErrors`). A
caller holding a kind as a bare string crosses over through
`provider.ParseAttributeSlot`, which is the one declared crossing point: an
unknown kind fails *at the conversion* with `APERTURE_ATTRIBUTE_SLOT_UNKNOWN`
naming the closed set, instead of resolving to no provider and then to an empty
bag.

`principalSlot` is a deliberately **narrower** door used on the decision path:
only `user` and `machine` are principal kinds. Routing a principal fetch through
`ParseAttributeSlot` would let the string `"account"` resolve the account
directory and serve a tenant's bag as a principal's. No caller wants that, so the
mapping that would make it expressible does not exist.

## One value model, reused entire

An attribute bag **is** a `provider.Metadata`. Not a parallel model — the same
one:

- the same legal shapes, the same depth cap and size cap, validated by the same
  `provider.ValidateMetadata`;
- the same two canonical date forms (`provider/date.go`);
- the same `MatchFields` predicate for filtering;
- the same number normalisation in every loader (exact `int64` else `float64`),
  so `principal.clearance == 3` answers identically whether the bag was authored
  in YAML, in JSON, read from a CSV `:int` column, or read from a SQL `integer`.

A second model would be a second set of shapes the expression evaluator has to
survive, and a second place for the two to drift. See `skills/metadata-values.md`
— it is the authority, and it documents the `attributes:` spelling alongside
`objects:`.

`provider.AttributeRecord` pairs one **bare string key** with its bag. It is
`Object`'s counterpart, and the key's type is the whole difference: an object
identity is a segmented path (`account:acme/project:atlas/document:42`) precisely
so a scope can contain and pattern-match it; an attribute key is a principal id
or an account id — an opaque handle into the host's directory that Aperture never
parses. There is no hierarchy to spell and no containment relation to anything.

### Bags are read-only, transitively, and the blast radius is why

The cache stores a provider's map **by reference** and never copies it on read,
so writing through a returned bag at any depth races every other reader. For an
object that corrupts one object's view. An attribute bag is the subject's bag for
the **whole decision**, shared across every object being checked and — through the
per-slot cache — across every concurrent decision for that key. One write is
therefore every rule, for every object, in every in-flight decision for that
subject.

So: a provider returns a **fresh** map per key with fresh nested containers and
never retains-and-mutates what it handed out; no holder (engine, rules, scope,
CLI, server, host code) writes into a bag at any depth; a consumer that must
modify one deep-copies it first. The engine honours this itself — the floor is
stamped into a fresh map, never into the resolver's.

## The floor bags, and why the floor wins

The engine stamps a **floor** over whatever a resolver returns:

| Root | Floor | Built by |
|---|---|---|
| `principal` | `{id, kind}` | `rules.principalBag` |
| `account` | `{id}` | `rules.accountBag` |

A resolver returns **only what the host knows**. It never has to publish `id` or
`kind`, and returning `nil` is not an error and not an empty root — it is exactly
the floor. That is what makes `principal.id`, `principal.kind` and `account.id`
writable against **any** deployment, wired or not.

`account`'s floor is `{id}` and not `{id, kind}` on purpose: there is one account
slot, it answers for every account, so a `kind` key would be the same constant in
every bag in every deployment. A value that can never discriminate is not
information — it is noise a rule author would eventually compare against.

**The floor is stamped LAST, so it wins on collision.** This is the load-bearing
half. The realistic collision is innocent, not hostile: a host directory table
with its own internal `id` column would otherwise silently redefine what
`principal.id == object.owner` compares, and an ownership rule would start
answering a different question with no error anywhere. `principal.kind` must stay
trustworthy for the same reason — it is the branch key a rule author uses to
state kind-dependence (below). A floor that can be shadowed is not a floor.
Pinned by `rules.TestTheFloorIsNotShadowedByTheProviderBag`.

`principal.kind` is published **even when empty**. An empty kind is a value a
rule can compare against (`""` matches neither `"user"` nor `"machine"`); an
*absent* key would make "unknown" and "not published by this build" the same
observation.

## The leniency contract

`provider.AttributeRegistry` satisfies both rules seams structurally, without
importing `rules`:

```go
rules.NewEngine(ruleSource, fetcher,
    rules.WithPrincipalResolver(attrs),  // Attributes(ctx, kind, principal)
    rules.WithAccountResolver(attrs),    // AccountAttributes(ctx, account)
)
```

Two methods rather than one overload of `Attributes` because Go has no
overloading, and **one registry must be able to serve both seams** — it already
holds both directories and both caches.

Two outcomes are **lenient**: they yield a nil bag and **no error**, so the
decision proceeds against the floor.

- the slot has no registered provider (`APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED`),
  or the principal's kind names no principal slot at all (an empty kind is the
  live case);
- a registered provider has no record for this key (`APERTURE_NOT_FOUND`).

Everything else — an unreachable directory, a bag the value model rejects —
surfaces **verbatim**, keeping its code and its registry fixups, and every
consumer treats it as a **non-decision**. That distinction is the point of the
seam: an outage must not read as "this principal has no attributes", because that
is an authorization change wearing an infrastructure failure's clothes. A
provider therefore returns `APERTURE_NOT_FOUND` for a key it does not know, and
the registry passes an already-coded error through unwrapped (`Wrap` **re-stamps**
— a wrapped `APERTURE_NOT_FOUND` would read to every caller as an operational
failure).

A key that can **never** name one subject is a third thing and is refused rather
than collapsed: an empty key, or the account wildcard `"*"`, is
`APERTURE_ATTRIBUTE_PROVIDER_INVALID`. That is a caller that has not resolved
what it is asking about, not a deployment that chose not to wire a directory.

### The hazard leniency leaves: an exclusive grant WIDENS

This is accepted, not solved, and it is the single most important sentence in
this document.

An absent attribute makes every comparison against it **false**. In an
**inclusive** grant that is deny-safe: a rule that fails to select covers
nothing. In an **exclusive** grant, selection means *excluded* — so a rule that
quietly stops selecting stops excluding, and the object the exclusion was written
to withhold becomes **covered**. Nothing in the verdict says so.

The alternative is worse in the direction that matters more: erroring on an
absent bag makes a deployment with an unwired slot **undecidable** for every
principal of that kind, which is the outage the leniency exists to prevent. So
the mitigation is **visibility**, not refusal:

- `principal.kind`, so an author can state a rule's dependence instead of hiding
  it, and
- the `attributes_floor_only` note, so a trace says the bag was empty.

`rules.TestAMissingBagWidensAnExclusiveGrant` keeps the hazard executable so it
cannot quietly stop being true.

### The kind-dependence hazard

Providers are registered **per kind**, which makes a rule silently
kind-dependent. A rule written against a `user` directory's field reads nothing
for a machine principal — the field is simply absent and the comparison is false.
Deny-safe in an inclusive grant, access-**widening** in an exclusive one, exactly
as above.

State the dependence out loud:

```
principal.kind == "user" && principal.tier == "gold"
```

That is what `principal.kind` is *for*. A rule that does not name it is a rule
whose author has not decided which kinds it is about.

### `attributes_floor_only`

A `rules.NoteKind` recorded when a rule **reads a host-defined field** off
`principal` or `account` while that root carried nothing but the floor:

```
g-alice [rule gold-only]: principal: floor-only; no host attributes were resolved,
so every comparison against a host-defined field is false
```

Three deliberate properties:

- **It is recorded per evaluation, not per failed comparison.** Every other note
  kind is produced *by* a comparison that went a surprising way; this one
  describes the **input**, because the failure it exposes is a comparison that
  never visibly happened.
- **It fires only when the rule NAMES a non-floor path** on that root (a bare
  reference to the whole root counts). A rule reading only `object.*`, or only
  `principal.id`, is not exposed to the hazard — the floor is always present and
  always says the same thing. Unconditional emission put two lines on every trace
  of every rule-backed grant in the very common deployment that wires no
  provider, and buried the shape and date notes; the traces where it matters
  would be the ones nobody could find.
- **One kind, not two.** "No provider wired" and "a directory answered and had
  nothing" are different operator situations, but that is not the distinction
  this layer can draw: the live shape of "unwired" is an `AttributeRegistry` that
  *is* wired and simply has no slot for this principal's kind, indistinguishable
  from a slot whose directory lacks the row. A second kind keyed on "is a
  resolver installed" would be confidently wrong a lot of the time. Narrowing it
  properly would mean widening both resolver interfaces to report *why* a bag is
  empty — diagnostic plumbing on the signature every host implements and on the
  path every decision takes.

`Path` is the root name and that is **the whole of what the note discloses** — no
key, no value, no account. Same rule as every other note.

## Account neutrality: a principal bag is global

**Nothing enforces this. It is a host obligation, and it is why it is written
down.**

`model.Principal` has no account field; `model.Membership` binds a principal to
accounts, so a principal can belong to several. An attribute fetch is keyed by
the **bare principal id alone** — `Fetch(ctx, slot, id)` carries no account —
so a principal's single bag is visible to rules evaluating in **every account
that principal is a member of**.

Therefore: **a host must keep principal attributes account-neutral.** Facts about
the person or the machine (department, clearance, employment status, kind of
client) are account-neutral. Facts about the person *in one tenancy* (the plan
they bought here, their role in this workspace, a per-tenant entitlement flag) are
not, and putting one in a principal bag publishes one account's data into every
other account that principal touches. Aperture cannot detect it: the values are
opaque host data, and a rule comparing them is doing exactly what it was written
to do.

Per-tenant facts belong on the **account** slot, which *is* bounded — the account
bag is resolved from the **active** account, and `engine/account_boundary_test.go`
proves the boundary with a spy directory that records which tenancy was asked
about, using one wildcard-stamped grant so the grant's stamp and the active
account are different strings.

If a host genuinely needs a per-tenant principal fact, model it as object
metadata or as a grant, not as a principal attribute.

## The `account` root, and the wildcard

`account` is the **ACTIVE** account — the tenancy the decision is being made in —
never the account a grant happens to be stamped to. A wildcard-stamped grant is
evaluated inside whatever account is active, so reading its stamp would give one
tenant's decision another tenant's plan.

`"*"` (`model.AccountWildcard`) is the all-accounts grant/membership **sentinel**,
not an account: `ValidateAccount` refuses to store a row for it, and the only bag
that could answer "the attributes of every account" is one tenant's data served as
every other's. But it is a **live active account** — platform-tier authority is
anchored there, and `service/reads.go` genuinely runs `engine.Check` with
`Request.Account == model.AccountWildcard` for system-admin authority.

So `rules.Engine.accountAttributes` **short-circuits to the floor** rather than
erroring: `account.id` reads `"*"` (truthfully — "this decision is not scoped to a
tenant") and every host-defined field is absent, exactly as for an unwired slot.
Erroring would make **every** rule-backed grant undecidable at platform scope,
including the overwhelming majority that never mention `account`, and would turn
one invariant about attribute keys into a restriction on which scope strategies
may guard the system anchor.

The refusal still exists one layer down: presenting `"*"` to the registry
(`AccountAttributes`, or `Fetch` on any slot) is
`APERTURE_ATTRIBUTE_PROVIDER_INVALID`. The short-circuit is what keeps that a
**backstop** rather than the mechanism. A wildcard decision resolves no account
bag at all, so `Trace.Attributes.Account` is nil for it — and a rule that reads
`account.plan` there earns an `attributes_floor_only` note, truthfully.

## Impersonation reads the effective subject

Under an active impersonation session, `principal.*` describes the **effective
subject** — the target under `become`, the operator under `augment` — which is
the same principal the resolved grant set describes. `become` resolves the
target's id **and the target's kind**, so `principal.*` comes from the target's
directory.

The invariant is that the rule and the grant set always describe the same
principal. A decision resolving the target's grants while reading the operator's
attributes is an authorization bug that leaves no mark in a trace.

Audit is unaffected: `Request.Principal` and `Decision.Impersonation` still name
the real operator, and the reason string still names the operator. An inert or
expired session elevates nothing and reads the operator's own bag. The target's
kind costs no extra read — it arrives with the subject-set expansion `become`
already performs.

## One bag per decision

`principal` and `account` are **constant for the whole decision**, so they are
resolved once per decision through `rules.DecisionAttributes`, exactly as `NOW` is
snapshotted once. The decision engine opens the scope
(`rules.WithDecisionAttributes`) at the same three boundaries it opens
`WithDecisionInstant` at — `Check`, `Enumerate`, `Explain` — and nesting is
idempotent. Unscoped evaluation is correct, merely unmemoized.

It is a **correctness** property, not only a cost one: a bag is served through a
cache with a TTL, and a TTL expiring halfway through an enumeration would judge
the first candidates against one version of the principal and the last against
another — a result set no single view of the principal justifies, with no error
anywhere.

**An absence memoizes; a failure does not.** An absence (no provider wired, or a
directory with no record) reaches the memo as a successful resolution of a nil
bag and is retained: it is a complete answer describing a steady state, so a
1,000-object enumeration against an unwired slot performs **one** resolution. A
failure is not retained — and that costs almost nothing, because a failure *ends*
the decision (every consumer treats a rule error as a non-decision and returns
immediately), so "retried" means at most one further attempt per decision, never
one per object. Freezing a blip instead would be unbounded in the direction that
matters: a wrong answer held for the whole decision.

The memo is **keyed** by the subject each bag was resolved for and re-resolves on
a mismatch, so a scope travelling somewhere its author did not picture cannot
serve one principal's attributes as another's. It is **not an injection point**:
there is no API to hand in a bag, because a caller-supplied principal bag is a
caller-supplied answer to "who is asking". Full reasoning: "One bag per decision"
in `skills/rules-engine.md`.

## Staleness is a security window, not a tuning knob

Each slot gets **its own cache** — its own TTL, size cap and counters — because
the three have genuinely different change rates and cardinalities. Defaults are
`provider.DefaultTTL` (30s) and `provider.DefaultMaxSize` (10,000) unless the
slot overrides them; inline seed bags are registered with a TTL of `0` (never
expires), which is correct only because nothing can change them.

An object's metadata going stale for a TTL is usually tolerable: a document's
category is a fact about a thing. **An attribute bag is the asker's standing** —
the clearance, the department, the plan — so until a cached bag expires, every
decision about that subject is made against access the host has **already taken
away**. Principals are the classic revoke case. What a slot's `ttl:` buys in
fetch traffic it pays for in the time a revocation takes to become true.

So: pick a slot's `ttl:` for how fast its revocations must land, and close the
window explicitly when you cannot wait:

- `provider.AttributeRegistry.Invalidate(slot, id)` — one subject; reports whether
  an entry was present;
- `InvalidateSlot(slot)` — a whole directory changed (bulk role sync, reorg,
  restored backup);
- `InvalidateAll()` — every slot; not an error on an empty registry, and the right
  instrument exactly once: an operator who knows a directory changed but not which
  subjects.

`Invalidate` validates the key through the **same guard** `Fetch` uses, so the
empty string and `"*"` are refused here too. Neither could ever have been cached,
so the refusal costs nothing and answers "why did invalidating `*` report
nothing?" with the reason instead of a `false`.

**Invalidation is process-local.** It clears the caches of the process that runs
it. That makes `aperture attributes invalidate` exact for a host embedding
Aperture, and self-contained for a one-shot CLI invocation (which starts cold and
exits cold) — but it **cannot reach a running `aperture serve`**. The controls
that reach that process's cache are the slot's `ttl:` and a restart.

## Containment: enumeration is never scope resolution

`*provider.Registry` deliberately **does** satisfy `scope.ObjectLister`.
`*provider.AttributeRegistry` deliberately **does not**, and there is no
compile-time assertion to match precisely because the assertion is the thing being
refused.

If the principal directory were reachable through that seam, the principal table
would become an enumerable object set **inside a decision** — every principal in
the deployment listable by anything holding a lister, bounded only by the grant's
own scope, with no admin tier consulted.

Go's typing is **structural**, so intending that is worth nothing: a method
spelled `List(ctx, string, identity.Pattern, int) ([]identity.Identity, error)`
satisfies the interface whether or not anybody meant it to, and the wiring
mistake it enables is silent — the resolver compiles, runs, and enumerates. So
containment is structural too, four times over:

- enumeration is called **`Enumerate`**, not `List`;
- it is keyed by an **`AttributeSlot`**, not a bare object-type string;
- it takes an **`AttributeFilter`**, which carries **no `identity.Pattern`** to
  bound with;
- it returns **`[]AttributeRecord`** (bare string keys), not
  `[]identity.Identity`.

Any one makes the signature unassignable; all four make it unassignable by
accident. `provider.TestAttributeRegistryIsNotAScopeLister` asserts the negative
against the real `scope.ObjectLister`, with `*provider.Registry` as the positive
control.

`AttributeFilter` has no `Pattern` for the same reason: there is nothing to match
(an attribute key has no segments, so a pattern over it could only be a substring
test dressed as containment), and `Filter.Pattern` exists solely to bound an
enumeration **to a grant's scope**. Admitting one here would give the type the one
field that makes it look like the seam a scope resolver wants — and shapes get
wired to what they look like.

`Fields` and `Limit` are re-enforced by the registry on whatever a provider
returns (`MatchFields`, plus the limit), so a provider that ignores them is still
correct, only less efficient. The limit is **honoured, not clamped down**: a
positive `Limit` is passed through verbatim and enforced at that value, and
`provider.DefaultListLimit` (= 1000) is only what a non-positive `Limit` means.
`Enumerate` is deliberately **uncapped** — it is the system-tier admin read of a
directory, and an operator asking "who is in the user slot?" may legitimately need
all of it; a page size chosen inside the registry would only make the honest
answer arrive in pieces. What keeps the read safe is the authority required to
reach it (`service.requireAttributeAdmin`), not a number in `provider`.
`Enumerate` opportunistically warms the slot's cache with each returned bag, since
the provider call already paid to produce it.

## Wiring

### From Go

```go
attrs := provider.NewAttributeRegistry()
attrs.MustRegister(provider.AttributeSlotUser, dir, provider.WithTTL(60*time.Second))
attrs.MustRegister(provider.AttributeSlotAccount, tenants)

eng := rules.NewEngine(ruleSource, objectRegistry,
    rules.WithPrincipalResolver(attrs),
    rules.WithAccountResolver(attrs))
```

A host implements `provider.AttributeProvider` (`Fetch` / `List` / `Query`), which
must be safe for concurrent use and must return `APERTURE_NOT_FOUND` for a key it
does not know. A slot left unregistered is **not** an error at construction — a
deployment with no machine principals wires no machine provider.

Registering a slot twice is refused, not replaced: "last writer wins" is how one
deployment's directory quietly shadows another's during wiring, and the failure
surfaces as attributes that are merely *wrong* rather than absent.

Three implementations ship:

| Implementation | Source | Notes |
|---|---|---|
| `provider.StaticAttributes` | an in-memory slice | immutable after construction; everything validated at construction, so a read can never fail for a reason wiring could have reported |
| `csvprovider.NewAttributes(path)` | one CSV file | same header grammar and column-type suffixes as the object loader; **loaded eagerly**, so a malformed file is a coded error at boot naming the row |
| `sqlprovider.NewAttributes(q, cfg)` | two statements over a `Querier` | same driver-value mapping, same value model, same four casting rules as the object provider — word for word (`skills/sql-provider.md`) |

CSV loading is eager where the object loader is lazy, because of blast radius: an
unparseable attribute file is not one object type failing to answer, it is *every
decision for that slot*, and boot is where the operator is present to fix it.

### From a seed document

Two sections, both **runtime wiring and never model state** — `Apply` writes no
row for either, and because `Export` reads the model back out of storage, an
export reproduces neither. The seed **file** is their source of truth, exactly as
`providers:` / `objects:` / `field_types:` / `connections:` are.

```yaml
connections:
  main:
    dsn_env: APP_DATABASE_URL

# Inline bags, served from memory (TTL 0).
attributes:
  - subject: user
    id: alice
    metadata:
      department: eng
      clearance: 3
      teams: [platform, infra]
  - subject: account
    id: acme
    metadata: { plan: enterprise }

# External sources, one entry per slot.
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department, clearance FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department, u.clearance FROM users u
    ttl: 60s
  - subject: machine
    kind: csv
    path: machines.csv
```

Build it with `doc.BuildAttributeRegistry(baseDir)`, or — when any entry is
`kind: sql` — `doc.BuildAttributeRegistryWithConnections(baseDir, conns)` using
the `*Connections` `BuildRegistryWithConnections` already returned. `connections:`
is **one shared pool set**: object providers and attribute providers read through
the same pools, and a second entry point that opened its own would double every
deployment's connections and hand nothing back to close them. The plain form
passes no pools, so a `kind: sql` entry fails there with
`APERTURE_SQL_PROVIDER_CONNECTION` naming the form to call.

The registry is always usable — empty when the document declares neither section
— so a caller can wire it unconditionally. Slots are filled in
`AttributeSlots()` order, not file order, so a document with two bad slots always
fails on the same one.

Key details:

- `subject:`, not `kind:`, names the slot — `account` is a slot but not a
  principal *kind*, and `kind:` is already the implementation selector on
  `attribute_providers:`.
- Inline keys are deduplicated **per slot**, not across the section: a tenant
  called `acme` and a service principal called `acme` are unrelated subjects.
- `ttl:` / `max_size:` are **per slot**, because one number covering all three
  would tune for whichever was declared last. `ttl: "0"` never expires.
- `dsn:` is refused **by name** wherever it appears: credentials belong to a
  `connections:` entry's `dsn_env:`.
- A value-model rejection keeps `APERTURE_METADATA_INVALID` (whose fixups name the
  shapes and the caps); a malformed *entry* is `APERTURE_CONFIG_INVALID`; an
  unknown `subject:` keeps `APERTURE_ATTRIBUTE_SLOT_UNKNOWN`.

`Document.HasAttributeSources()` is the one question a host asks before wiring the
resolvers, and it lives beside the fields it counts — a **third** attribute
section owes a one-line edit there, in one place. (`internal/cli`'s
`hasObjectSources` records the bug that taught this: a gate written over
`providers:` alone, in a different package from the field list, so adding
`objects:` did not look like touching the gate.)

#### Precedence: the external source wins, entirely

When both sections declare the same slot, the `attribute_providers:` entry
**wins and every inline entry for that slot is discarded entirely**. There is no
per-subject merge and no fallback: an inline id the external source happens to
lack is simply not resolvable, exactly as if the entry had never been written.

Field-level merging is the most useful-sounding behaviour and the most impossible
to debug — a rule reading a department the directory silently did not override is
a support ticket nobody can reproduce. It is `ProviderCollisions`' rule at slot
granularity.

The discard is **not silent**: `Document.AttributeCollisions()` reports the
affected slots and the caller surfaces them (the CLI prints a warning). Only slot
**names** are reported, never keys, so the warning cannot leak a directory's
contents. `Document.AttributeSlotSources()` reports where each slot's bags come
from (`"csv"`, `"sql"`, or `seed.AttributeSourceInline` = `"inline"`) so a surface
that displays the wiring does not re-derive the precedence rule and eventually
disagree with it.

#### The `get_all` bare-id contract — a failure with no error

This is the trap, and **nothing can catch it**:

```sql
-- providers:            an OBJECT provider selects the FULL IDENTITY
get_all: SELECT 'user:' || u.id AS id, u.department FROM users u   -- WRONG here
-- attribute_providers:  an ATTRIBUTE provider selects a BARE ID
get_all: SELECT u.id AS id, u.department FROM users u              -- CORRECT
```

An identity-shaped key is a perfectly legal opaque string. It enumerates happily,
caches happily, and then matches **no principal id any `Fetch` ever presents** —
the decision path fetches by the bare id. The slot simply never answers, and
nothing anywhere complains. There is no check any package could add, because the
key is opaque and there is nothing to test it against. The same trap exists for a
CSV `id` column (`alice`, not `user:alice`).

`get_one` differs in the same direction: an object provider binds the identity's
**terminal segment value** (`brand:42` and `account:acme/brand:42` both bind
`42`); an attribute provider binds the **bare subject id verbatim**, because there
is nothing to strip.

This asymmetry is exactly why `attribute_providers:` is a separate top-level key
rather than a discriminated variant of `providers:`. Sharing one struct would mean
doc comments that contradict themselves depending on which key the entry was filed
under, and would make copying a statement across a silent fault.

#### `get_all` is optional; `ListQuery` on an object provider is not

An object provider's `get_all` is required because an `ObjectProvider` that can be
fetched from but not enumerated answers `List` with an error, and an errored
enumeration reads as "no access" one layer up — a denial caused by a wiring gap.

That reason does not apply here: attribute enumeration **never** participates in
scope resolution (see Containment). Omitting `get_all` yields a **fetch-only**
slot — every decision path works unchanged, and only `List`/`Query` refuse with a
coded error. That is a feature: a host can expose the attributes of the principal
currently being decided about **without** exposing its whole user table to an
admin enumeration.

## The administrative read

`service.ListAttributes` is the **one** administrative door onto a slot, and the
only place `AttributeRegistry.Enumerate` is reachable from a surface. Listing the
`user` slot returns the host's whole user table, keys and bags together, so it is
a **system-tier read**: gated directly through `authz.Gate.RequireSystemAdmin`,
the shape `Export` uses, never a `Mutation` row (it writes nothing) — and it is
not audited.

```go
recs, err := svc.ListAttributes(ctx, actor, "user", provider.AttributeFilter{
    Fields: map[string]any{"department": "eng"},
    Limit:  100,
})
```

- **The gate is above the registry, deliberately.** `provider` imports only
  `identity` + `errors`, and `authz` imports `engine` — a gate inside the registry
  would invert the dependency. The facade is where it belongs on the merits too:
  one gate serves every surface at once.
- **The gate runs BEFORE the slot is parsed**, so a refused caller cannot probe
  which slots a deployment wires. A non-admin gets the identical
  `APERTURE_AUTHZ_DENIED` — with a nil slice, no count, no partial page — for a
  populated slot, an unregistered slot, and a slot name that does not exist. A
  system-admin does get the real diagnostics
  (`APERTURE_ATTRIBUTE_SLOT_UNKNOWN`, `APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED`).
  The gate's error is returned **verbatim**, because `Wrap` re-stamps and
  `APERTURE_AUTHZ_DENIED`'s fixups are the operator's remedy.
- **No gate wired → `APERTURE_UNIMPLEMENTED`**, never "unrestricted": a bulk
  directory read has no narrower fallback to degrade to.
- **`Fetch` on the decision path is NOT gated, and must never be.** A decision
  resolves one bag for a subject it already named. The two paths reach the same
  registry through different seams — the resolvers for a decision,
  `service.WithAttributes` for the admin read.
- The three `Invalidate*Attribute*` facade methods are gated identically, through
  the same `requireAttributeAdmin` in the same order. Invalidation writes nothing
  and discloses no bag, but its boolean says whether this process had that key
  cached — a fact about who has recently been decided about — and clearing a large
  slot costs the next wave of decisions a provider round-trip each.
  `InvalidateAllAttributes` names no slot, so it cannot be used to probe which
  slots exist.
- `ExplainAttributeAuthority(actor)` returns the engine `Trace` behind that
  authority decision. It is deliberately **not** gated on holding the authority it
  explains — gating it that way would mean only the operators who were allowed
  could find out why the refused ones were not — and it needs no attribute
  registry. It discloses nothing new: the trace is of a `Check` on the caller's own
  authority over the `system:schema` anchor, and it names no slot, key, or bag.

**This is not the only way an attribute value is seen.** `Trace.Attributes`
(`engine.TraceAttributes`) carries the `principal` and `account` roots a decision
was evaluated against, **values included** — a deliberate disclosure, because "why
was this denied?" is unanswerable when the deciding comparison was
`principal.tier == "gold"` and the operator cannot see that the tier is
`"silver"`. Those two bags are the subjects of the very request being explained, so
a trace tells the asker about their own decision and nothing else. Do not redact
it into a note, and do not widen it past those two subjects. The what-if preview
(`service.EvaluateRulePreview`) takes the opposite side on purpose: it supplies
**no** principal or account input, not even the floor, so a rule author's editor
cannot become a directory read oracle.

## The operator surface: `aperture attributes`

The CLI is the **only** surface this seam adds. There is no RPC method and no MCP
tool, deliberately: MCP is where an agent listing a host's entire user table is
least defensible, and a directory read is exactly the shape of request that should
require a human at a shell with the deployment's seed file in hand.

| Command | Gate | What it does |
|---|---|---|
| `aperture attributes slots` | none | one row per slot: source (`csv`/`sql`/`inline`/`(host)`/`(unwired)`), `ttl`, `max-size`, `cached` |
| `aperture attributes query <slot>` | system-admin | a page of the directory as `[{id, attributes}]`, narrowed by `--field` / `--fields-json` |
| `aperture attributes invalidate <slot> [--id X] [--all]` | system-admin | drop cached bags |

`slots` is ungated because it discloses nothing the caller did not supply: it
reads the seed file named on the command line plus the cache configuration this
process built from it. It contacts no provider, names no key, and prints no bag.
Requiring system-admin authority to read back a file you just passed in would mean
nobody could diagnose "is the user slot even wired?" without already holding the
authority the diagnosis exists to explain.

`query` and `invalidate` hand the slot string over **unparsed**, because the
facade gates first and parses second; parsing in the CLI would move the disclosure
that ordering prevents back into the CLI. The `--field` predicate is exactly
`aperture enumerate`'s: ANDed, absent-never-matches, membership for collections,
typed equality for everything else (so `"5"` never matches `5`).

The `ttl` column **is the revocation window**; `never` means a fetched bag is
dropped only by eviction or an explicit invalidate — correct for a fixed inline
block, dangerous for a live directory. The `cached` column counts *this* process,
so a one-shot invocation reads `0`.

`invalidate`'s three forms are mutually exclusive and a conflict is **refused**
rather than resolved by precedence: "`--all` plus a slot" has two plausible
readings, and guessing the broader one would clear caches the operator did not ask
to clear. "Nothing was cached" is reported, not silently succeeded — an operator
invalidating a subject they believe is cached wants to know their key did not
match.

The wiring behind all three comes from the **same** `buildDecisionStack` every
other command uses, so the slots a listing reports are the slots a decision
resolves through: the CLI cannot describe a wiring it does not itself run.

## Error codes

| Code | Means |
|---|---|
| `APERTURE_ATTRIBUTE_SLOT_UNKNOWN` | not one of the three slots — a programming error at the call site |
| `APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED` | the slot is empty — a wiring gap. **Never** reaches you from a decision; only from a direct registry read |
| `APERTURE_ATTRIBUTE_PROVIDER_INVALID` | a nil/duplicate registration, a duplicate key, an empty key, or the account wildcard as a key |
| `APERTURE_ATTRIBUTE_PROVIDER_FETCH` | a host provider returned a plain (uncoded) error |
| `APERTURE_NOT_FOUND` | the directory has no record for this key — returned by the provider, and **lenient** on the decision path |

`UNREGISTERED` and `SLOT_UNKNOWN` are distinct because their fixups are:
a wiring gap and a call-site bug have different remedies.

## When you change this

The Update-Demand rows for this seam are in `CLAUDE.md`. Several of the most
important properties here **cannot be gated** — the account-neutrality obligation,
the `get_all` bare-id contract, the exclusive-grant widening, the staleness window
— which is exactly why they live in prose. A rule nothing can enforce survives
only in writing, and prose that was never written is a rule that does not exist.
Change one of them and this document is the thing that has to move.
