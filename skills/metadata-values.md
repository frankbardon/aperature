---
name: metadata-values
description: The shared metadata value model — what shapes a provider.Metadata field may hold (scalar, scalar array, one object level), the two canonical date forms a date-typed string scalar may take and what is rejected at load (non-UTC offsets, impossible dates, non-RFC3339 layouts), the depth and size caps, the load-time validation entry point every loader calls, how each loader spells the model (the csvprovider header grammar including its :date and :datetime column suffixes, the sqlprovider driver-value mapping table whose spelling is a cast in the SELECT list rather than a column suffix, the seed document's inline objects section, its optional field_types: date declarations, provider.Static, the inline attributes: section, and the two attribute loaders whose value model is identical and whose keys are BARE subject ids), the type-level precedence when a seed's providers: and objects: sections claim the same object type, and how a Filter.Fields predicate compares against each shape (membership for a collection, typed equality for the rest).
applies_to: [cli, http, mcp]
---

# Metadata value model

`provider.Metadata` is a **type alias** for `map[string]any`, so the type
constrains nothing. The **shape** of a field value is constrained anyway, in one
place, by the model in `provider/metadata.go`.

The reason is the hot path. Metadata reaches the expression evaluator with **no
conversion** (`rules/compiler.go` puts it straight into the expr environment), so
a wrong-shaped value is a decision made on data nobody meant to write. The rules
engine is deny-safe about it — a collection operator over a non-collection is
`false`, plus an `Explain` note, never an `APERTURE_RULE_EVAL` error (see
`rules-engine`) — but a silent deny is still a bug in the model. Validating
shapes at **load** time is what turns it into a loud one, at the point the data
enters, with the offending field named.

Load-time validation cannot reach everything: a host-implemented `ObjectProvider`
and the `principal` attribute bag bypass the loaders. That is why the runtime
policy exists as well; the two are complements, not alternatives.

## The model

A metadata field value is one of:

- **scalar** — `nil`, `bool`, `string`, any Go integer or float type, or a
  `json.Number`;
- **array** — a `[]any` whose elements are **all scalars**;
- **object** — a `map[string]any` whose values are scalars, scalar arrays, or one
  further object level.

Rejected everywhere:

- **arrays of objects**, at any position (`tags: [{name: "a"}]`,
  `owner: {members: [{id: 1}]}`) — `in` / `not in` over a list of maps has no
  useful meaning, so permitting it only buys evaluation-time failures;
- **nested arrays** (`[["a"]]`), for the same reason;
- **typed containers** — `[]string`, `map[string]string`, structs. The model is
  spelled in the two types the expression environment and JSON share so a loader
  normalises once;
- **`time.Time`** — a rule literal is a JSON scalar and could never be compared
  against one. Loaders format timestamps as RFC 3339 strings (see
  [Dates](#dates) for the two canonical forms).

## Dates

A date is **not a fourth shape**. It is an ordinary **string scalar** that a
loader has additionally been told is a date, and the value model is unchanged by
it. What `provider/date.go` adds is the constraint that such a string must be one
of exactly **two canonical forms**:

| Form | Layout | Example |
|---|---|---|
| calendar day | `2006-01-02` (RFC 3339 full-date) | `2026-03-04` |
| timestamp | `2006-01-02T15:04:05Z` (RFC 3339 date-time, always `Z`) | `2026-03-04T01:02:03Z` |

**Granularity is carried by the string itself** — a value containing a `T` is a
timestamp, one that does not is a calendar day — so no type tag travels beside
the value, and a canonical date round-trips through YAML, JSON, CSV, and the
expression environment as the string it already is.

The model stays **date-blind**: `ValidateField` cannot know which strings a host
means as dates, so it keeps accepting any string. Declaring a field to be a date
— and so subjecting its values to `provider.ParseDateValue` — is a **loader's**
job, through its typed column or field schema.

### Accepted and rejected

| Input | Result |
|---|---|
| `2026-03-04` | accepted, calendar day |
| `2026-03-04T01:02:03Z` | accepted, timestamp |
| `2026-03-04T01:02:03` | accepted — **no offset is read as UTC** |
| `2026-03-04T01:02:03.456Z` | accepted, **truncated** to `…T01:02:03Z` |
| `2026-03-04T01:02:03+05:00` | **rejected** — `DateReasonNonUTCOffset` |
| `2026-02-30`, `2026-13-01`, `2023-02-29` | **rejected** — `DateReasonCalendar` |
| `03/04/2026`, `2026-3-4`, `Mar 4 2026` | **rejected** — `DateReasonLayout` |
| `""` | **rejected** — `DateReasonEmpty` (a loader that reads an empty cell as an *absent* field checks that before parsing) |

Three properties are load-bearing, each because its absence is a silently wrong
answer rather than a loud failure:

- **An explicit offset is rejected, not converted.** Canonicalising is
  instant-correct and calendar-surprising: a host writing
  `2026-01-01T00:00:00+05:00` means January 1st, and the UTC instant is
  `2025-12-31T19:00:00Z`, so every question about the day, month, or year would
  answer 2025. Rejecting makes the host convert deliberately. An **offset-free**
  value is accepted because it asserts no other zone; there is no timezone knob
  anywhere in Aperture, and every instant is constructed and formatted in
  `time.UTC`.
- **Fractional seconds truncate; they never round.** Rounding can carry a value
  across the very boundary a rule is testing.
- **Parsing is mandatory — comparing the canonical strings is not a permissible
  shortcut.** The two forms sort correctly *within* one granularity but not
  across it: the date-only form is a strict prefix of the timestamp form, so
  `"2026-03-04" < "2026-03-04T00:00:00Z"` as text while the two name the **same
  instant**. A cutoff written against a date field and compared against a
  timestamp would be wrong at exactly the boundary instant.

### The entry points

```go
v, err := provider.ParseDateValue("2026-03-04T01:02:03.456Z") // load path
v.Time()                       // 2026-03-04 01:02:03 +0000 UTC, never sub-second
v.Granularity()                // provider.GranularityDateTime
v.String()                     // "2026-03-04T01:02:03Z" — the form a loader stores
v.Compare(other)               // -1 / 0 / +1 over the INSTANTS, never the text

s, err := provider.CanonicalizeDate(raw)  // parse + render, idempotent
v, ok := provider.DateValueOf(value)      // evaluation path: any value, no error built
provider.DateOf(t) / provider.DateTimeOf(t)  // from a time.Time, converted to UTC
```

`DateValue`'s fields are unexported, so there is no way to express one in another
zone, with sub-second precision, or with a granularity its instant cannot
support. The zero `DateValue` is the only invalid one and `IsZero` identifies it.

A rejection is **`APERTURE_CONFIG_INVALID`** — a loader-side failure, not a
value-model violation — carrying `reason` (the `DateReason`) and `expected` (the
two layouts) in its context. Read the cause back with `provider.DateReasonOf(err)`
rather than matching message text. As everywhere in this model the error **never
carries the value**, which for dates is sharper than usual: a date can be personal
data, a birth date or a termination date. The `*time.ParseError` is classified and
then **discarded**, never wrapped — its own `Error` string quotes the input
verbatim. A loader adds its own locating context around the rejection: the
**column, line, and field**, never the cell.

## Caps

Both caps live in `provider.ValueLimits`, never as constants at a call site. The
zero `ValueLimits` means "the defaults", and any field left zero (or negative)
keeps its default, so a caller overrides only what it cares about.

| Cap | Field | Default | Measured by |
|---|---|---|---|
| Depth | `MaxDepth` | `DefaultMaxValueDepth` = 2 | `provider.ValueDepth` |
| Size (per field value) | `MaxBytes` | `DefaultMaxValueBytes` = 64 KiB | `provider.ValueBytes` |

**Depth** is counted in containers below the field root: a scalar is 0, every
array or object entered adds one, and an empty container is 1.

| Value | Depth | Legal? |
|---|---|---|
| `tags: ["a", "b"]` | 1 | yes |
| `owner: {dept: "eng"}` | 1 | yes |
| `owner: {lead: {name: "x"}}` | 2 | yes |
| `owner: {tags: ["a"]}` | 2 | yes |
| `owner: {a: {b: {c: "x"}}}` | 3 | no — past the depth cap |
| `owner: {members: [{id: 1}]}` | 3 | no — array of objects |
| `tags: [{name: "a"}]` | 2 | no — array of objects |

**Size** is measured structurally rather than by an encoding round-trip, so
nothing is allocated on a load path: a string costs its length in **bytes**, a
number 8, a bool 1, `nil` 0, an array the sum of its elements, and an object the
sum of `len(key) + value` per entry. Container framing costs nothing.
`provider.MetadataBytes` sums a whole object, but the enforced cap is per
**field**.

### What the size cap costs on the hot path

The size cap is not only a memory bound — it bounds how long a **collection
operator** can spend traversing a field. A rule whose collection operand is a
variable evaluates through the guarded dispatcher, which walks the array, so
`Check` latency is linear in the array's element count.

Measured (Apple M1 Max, `bench/`, see [benchmarks](../docs/benchmarks.md)): about
**12 ns and 16.9 bytes per element, per `Check`**. At the 64 KiB default and a
9-byte element, the cap admits **7 281** elements — roughly 88 µs and 123 KB of
per-`Check` cost, which takes sustained throughput to ~6 200 checks/sec against
the 10 000 checks/sec NFR floor. The practical ceiling for that floor is nearer
**3 000 elements (~27 KB)**.

`ValueLimits.MaxBytes` is the lever: a deployment whose rules read large arrays
should set it from the array length its `Check` budget allows, not leave it at the
default and discover the cost as latency. `docs/benchmarks.md` → *Findings*
carries the numbers and the open question of whether the default should move.

## The entry point

Every loader — `csvprovider`, the seed document, a future database-backed
provider — calls the same function. That is what lets a new loader inherit the
semantics instead of renegotiating them.

```go
// defaults
err := provider.ValidateMetadata(md)          // whole object
err = provider.ValidateField("tags", value)   // one field

// tuned
limits := provider.ValueLimits{MaxDepth: 3}   // MaxBytes stays default
err = limits.ValidateMetadata(md)
err = limits.ValidateField("tags", value)
```

## How each loader spells the model

The model is one thing; each loader's *syntax* for reaching it is its own. A
loader normalises into the model — it never widens it.

### `csvprovider`

A header column carries `name:type[<elem>][(delim)]`:

```text
id,tier,seats:int,hired_at:date,last_seen:datetime,tags:list,ranks:list<int>,aliases:list(;),owner:json
brand:1,gold,40,2026-03-04,2026-03-04T12:30:00Z,premium|launch,3|5,acme;acme-co,"{""dept"":""eng"",""tags"":[""a""]}"
brand:2,silver,,,,,,,
```

| Suffix | Value |
|---|---|
| none / `:string`, `:int`, `:float`, `:bool` | a **scalar** (`int` → `int64`, `float` → `float64`) |
| `:date` | a **string scalar** in `2006-01-02`, canonicalised at load through `provider.ParseDateValue` |
| `:datetime` | a **string scalar** in `2006-01-02T15:04:05Z`, same parser |
| `:list` | an **array** of strings, split on `\|` |
| `:list<int>` / `:list<float>` / `:list<bool>` | an array whose elements go through the **same** scalar coercion |
| `:list(;)` | the delimiter override, for that column only |
| `:json` | an **object**, decoded from the cell — takes no `<elem>` and no `(delim)` |

The suffix spelling is **lower-case and exact**. An unrecognised one — `:timestamp`,
`:Date`, `:time` — is `APERTURE_CONFIG_INVALID` ("unknown column type"), never a
silently untyped column. The header is cut at its **first** colon, so a column
name cannot itself contain one: `hired:at:date` reads as the column `hired` with
the unknown type `at:date`.

Five rules the value model does not impose, but the CSV encoding must:

- **A date column's cells are validated and canonicalised at load**, and the
  **canonical** string is what is stored — not the cell as written. `:datetime`
  turns `2026-03-04T12:30:00.750Z` and `2026-03-04T12:30:00` alike into
  `2026-03-04T12:30:00Z`, so two rows naming one instant are one string and a
  `Filter.Fields` equality predicate over the column means something. The
  predicate must itself be canonical; range querying is not a provider concern.
  **The declared type also fixes the granularity** — a `:date` column rejects a
  timestamp and a `:datetime` column rejects a bare day, rather than quietly
  widening it to midnight. **An empty cell omits the field**, following the
  scalar rule: an absent date differs meaningfully from any date, and a zero time
  would silently satisfy every `before` rule written against the column. A
  rejection is `APERTURE_CONFIG_INVALID` naming the **column, line, and field**
  and carrying the `DateReason` — never the cell. **`:list<date>` does not
  exist**: arrays of dates are out of scope and rejected by name. Neither does a
  time-of-day type. A date-shaped string inside a `:json` cell is **not**
  date-validated — `:json` is opaque structured data, and only a declared column
  gets date treatment.

- **Element typing is mandatory for numeric membership.** expr does no
  numeric/string coercion, so `5 in object.seats` is `false` against `["3","5"]`.
  A silently wrong `false` is the worst failure mode here; `:list<int>` is what
  prevents it.
- **No escape syntax.** A value that must contain the delimiter needs a
  per-column delimiter it does not contain. A stray, doubled, leading, or
  trailing delimiter yields an empty element and is a **hard error at parse**
  (`APERTURE_CONFIG_INVALID`, naming the column, the line, and the cell) — never
  a silently mis-split row.
- **An empty list cell is `[]`, not an absent field** — the one departure from
  the scalar rule, so a membership rule gets a definite `false` instead of
  evaluating against `nil`. Empty *scalar* **and `:json`** cells still omit the
  field: an absent object is meaningfully different from an empty one.
- **A `:json` cell must decode to a JSON _object_.** An array, a scalar, or
  `null` is rejected (`APERTURE_CONFIG_INVALID`), so `:list` stays the only array
  path. The cell is RFC 4180 quoted — the whole cell in double quotes, every
  inner double quote doubled — and its numbers decode through `UseNumber` and
  then normalise **exactly as the scalar columns do**: an exact integer that fits
  `int64` becomes an `int64`, everything else a `float64`. That is what keeps
  `object.owner.seats == object.seats` from being a silent `false`.

Grammar, coercion, `:date`/`:datetime`, and `:json` decode failures are
`APERTURE_CONFIG_INVALID`; the value-model check that runs on every parsed cell —
including a date, which reaches it as the plain string scalar it is — is
`APERTURE_METADATA_INVALID`. A `:json` rejection carries the column, the line,
and the JSON kind or the decoder's message; a date rejection carries the column,
the line, the `reason`, and the layout `expected`. Neither ever carries the cell.

#### The attribute variant: `csvprovider.NewAttributes`

An attribute slot's CSV file uses **the same header grammar and every one of the
suffixes above**, unchanged — `parseTable` is literally the same walk, so
`clearance:int` in a user file normalises exactly as `seats:int` in a brand file:

```text
id,department,clearance:int,teams:list,hired_at:date
alice,eng,3,platform|oncall,2024-03-04
```

Two things differ, and neither is about the value model:

- **The `id` column holds a BARE subject id** — `alice`, never `user:alice`. An
  attribute key is an opaque handle into the host's directory, so an
  identity-shaped key is a legal string that loads, caches, enumerates, and then
  matches no id any `Fetch` presents. The slot silently never answers, and there
  is nothing the loader can test the key against. The seed entry's `id_column:`
  is a `kind: sql` concept and does not apply here: the header column is `id`,
  the same spelling the object file uses.
- **Loading is EAGER**, where `*Provider` is lazy. An unparseable attribute file
  is not one object type failing to answer, it is *every decision for that slot*,
  so the coded error lands at construction — naming the file and the row — while
  the operator is present. `Reload` re-reads afterwards and leaves the current
  set untouched if the new one does not parse.

### `sqlprovider`

The SQL loader's spelling of the model is **a cast in the `SELECT` list**. That
is the contrast with `csvprovider`, and it is the whole design: there is no
`:int` / `:list<T>` / `:date` suffix to learn and no per-column type declaration
in YAML, because the developer is already writing a statement and two spellings
for one intent drift apart. A column becomes whatever Go type `database/sql`
scanned it into, through a **closed** mapping table:

| Scanned Go type | Value |
|---|---|
| `nil` (SQL NULL) | the field is **omitted** — the same absent-vs-zero rule as an empty CSV cell |
| `bool` / `int64` / `float64` / `string` | the **scalar**, as-is |
| `[]byte` | JSON-decoded — an **array** or an **object**, and the only way either arrives |
| `time.Time` | `.UTC()`, then a **string scalar** in `2006-01-02T15:04:05Z`, through the same `provider.ParseDateValue` every other loader calls |
| anything else | `APERTURE_SQL_PROVIDER_SCAN` naming the column, the row's identity, and the Go type |

So the statement is where the model is reached:

| Want | Write | Because |
|---|---|---|
| an **array** | `to_jsonb(tags) AS tags` | no driver produces a `[]any` for a SQL array — measured. `SELECT tags` yields the raw literal `"{a,b}"`, a valid **string** that silently fails every membership predicate, and nothing in the loader can tell it from a string a host meant to store |
| a **day-granular date** | `hired_on::text AS hired_on` | a database `date` and a `timestamp` are the same Go type, so granularity is not inferable; every `time.Time` becomes the **datetime** form |
| a **number** from `numeric` | `amount::float8 AS amount` | a `numeric` arrives as a `string` (pgx), not a number |
| an **identifier** from `numeric`/`uuid` | `sku::text AS sku` | same reason, opposite intent |
| the object's **identity** | `'brand:' \|\| b.id AS id` | the id column is the identity, not a field; it is composed by the developer and removed before the rest becomes metadata |

Four rules the value model does not impose, but the SQL encoding must:

- **A `[]byte` is JSON, unconditionally** — there is no fall back to a string. A
  fallback would let one column change *type* depending on its contents (a
  `numeric` of `1.50` decoding as the float `1.5` while a `uuid` stayed a
  string). A decode failure is a hard `APERTURE_SQL_PROVIDER_SCAN` naming the
  column and the row. Consequently a **`bytea` does not work**: encode it in the
  statement or leave it out. A JSON `null` omits its field, exactly as SQL NULL
  does.
- **Numbers inside a JSON column normalise exactly as scalar columns do** — an
  exact integer that fits `int64` becomes an `int64`, everything else a
  `float64`, at every depth. That is `csvprovider`'s rule verbatim, so a host
  migrating a CSV to a table gets the same decisions, and
  `object.limits.seats == object.seats` is not a silent `false`.
- **A `time.Time` is converted to UTC first.** A `timestamptz` comes back in the
  *process's* local zone in both candidate drivers, so the same row read on two
  hosts would otherwise be two strings and, near midnight, two calendar days.
- **The column name is the field key**, so an unnamed expression or two columns
  with the same name is `APERTURE_SQL_PROVIDER_SCAN`, never a field dropped or
  overwritten by whichever came last.

The mapping table is gated: `TestDriverValueMappingTableMatchesTheTypeSwitch`
parses `sqlprovider/values.go` with `go/ast` and fails if the type switch and
`mappedDriverTypes` disagree. Every mapped row then goes through
`provider.ValidateMetadata` like every other loader's output — the loader never
re-implements the rules. See `skills/sql-provider.md` for the wiring, the
connection defaults, and the errors.

#### The attribute variant: `sqlprovider.NewAttributes`

`*Attributes` reads through the **same** `metadataValue` — literally the same
function, the same closed mapping table, the same four casting rules — so a
column read as object metadata and the same column read as an attribute produce
the same Go value and therefore the same decision. Cast it in the statement here
too:

```sql
get_one: SELECT department, to_jsonb(teams) AS teams, hired_on::text AS hired_on
           FROM users WHERE id = $1
get_all: SELECT u.id AS id, u.department, to_jsonb(u.teams) AS teams FROM users u
```

What differs is only what a **key** is, and both differences are contract:

- `get_one` binds the **bare subject id verbatim** (`Fetch(ctx, "alice")` →
  `QueryContext(stmt, "alice")`), where an object provider binds the identity's
  terminal segment value;
- `get_all` selects a **bare id** in the id column (`SELECT u.id AS id`, never
  `SELECT 'user:' || u.id AS id`) — the trap with no error attached, for the same
  reason the CSV `id` column has it;
- `ListQuery` is **optional** here (an empty one yields a fetch-only slot; only
  the admin enumeration refuses), where `Config.ListQuery` is required.

A diagnostic names the slot, the column, the row's position, and the key — never
a value, exactly as on the object seam.

### The seed document's `objects:` section

YAML nests natively, so the seed file spells the model **directly** — no encoding
to learn, no delimiter to pick:

```yaml
objects:
  - id: account:acme/brand:1
    metadata:
      tier: gold
      seats: 5
      tags: [premium, launch]
      owner: { dept: eng, lead: alice }
  - id: account:acme/brand:2      # metadata: may be omitted
```

The object-type is **derived from the identity's terminal segment**, never
declared. `Document.BuildRegistry` groups the entries by type and registers one
`provider.Static` per type with a TTL of 0; declaration order is what `List` and
`Query` return. This is runtime **wiring**, not model state: `Apply` writes no row
and an export never reproduces the section.

Three properties, all of them the same ones the CSV loader owes:

- **Numbers normalise identically.** The section is carried as raw JSON and
  decoded with `UseNumber`, then an exact integer that fits `int64` becomes an
  `int64` and everything else a `float64` — the same rule as `:int`/`:float` and a
  `:json` cell. Without it, YAML would hand over an `int` and JSON a `float64` and
  the *same document* would answer `object.seats == 5` differently depending on the
  format it was written in.
- **`metadata:` must be a mapping.** A scalar or an array there is rejected, so
  the section cannot smuggle in a shape the field-level model would refuse.
- **Every field goes through `provider.ValidateField`**, in sorted key order, at
  build time — never re-implemented. A violation is **`APERTURE_CONFIG_INVALID`
  naming the object id and the field**, wrapping the value model's own
  `APERTURE_METADATA_INVALID` (with its `path`/`type`/`depth` context) in the
  chain. A missing or duplicate `id`, or metadata that is not a mapping, is the
  same `APERTURE_CONFIG_INVALID`; a malformed id passes through as
  `APERTURE_IDENTITY_INVALID`. Ids are deduplicated across the **whole** section,
  not per type.

#### The seed document's `field_types:` section

A CSV header can say `hired_at:date`; a YAML mapping has nowhere to put that. The
optional `field_types:` section is the missing declaration — and **only** that:

```yaml
field_types:
  - object_type: brand
    fields:
      hired_at: date
      last_seen: datetime

objects:
  - id: account:acme/brand:1
    metadata:
      hired_at: "2026-03-04"
      last_seen: "2026-03-04T12:30:00Z"
```

| Spelling | Meaning |
|---|---|
| `date` | a **string scalar** in `2006-01-02`, canonicalised at load through `provider.ParseDateValue` |
| `datetime` | a **string scalar** in `2006-01-02T15:04:05Z`, same parser |

The vocabulary is **exactly those two words, lower-case and exact** — the CSV
column suffix with the colon dropped, so one word means one thing in both
loaders. `timestamp`, `Date`, `time`, and an empty type are all
`APERTURE_CONFIG_INVALID`, never a silently ignored declaration. Each entry names
an `object_type` (matched against the identity's terminal segment, as `providers:`
is) and may appear at most once per type.

It is a **third top-level wiring section**, beside `providers:` and `objects:`
and shaped like `providers:` — a list of entries each naming its `object_type`,
rather than a map keyed by type, so a duplicate declaration is a detectable error
and not a last-one-wins merge. It is deliberately **not** a `fields:` key on an
`object_types:` entry: `object_types:` is model state that `Apply` writes and
`Export` rebuilds from storage, so a declaration hung there would be silently
dropped by every export.

**Quote date values in YAML.** YAML resolves an *unquoted* calendar day as a
`!!timestamp`, and the seed loader's YAML path normalises through JSON, so
`hired_at: 2026-03-04` arrives already widened to `2026-03-04T00:00:00Z` and a
field declared `date` rejects it. The widening happens inside the YAML decoder,
before `seed` sees the document, so it cannot be undone there — the rejection
carries a `hint` naming the fix when the instant is exactly midnight. JSON has no
date type and so no trap; the two formats agree on every quoted value.

Six rules, five of them shared verbatim with the CSV loader:

- **Validated and canonicalised at `BuildRegistry` time**, through
  `provider.ParseDateValue` — never re-implemented here. The **canonical** text is
  stored, so `2026-03-04T01:02:03.456Z` and `2026-03-04T01:02:03` both become
  `2026-03-04T01:02:03Z`, and a `Filter.Fields` predicate over the field is
  canonical-string equality.
- **The declared type fixes the granularity** — a `date` field rejects a timestamp
  and a `datetime` field rejects a bare day, rather than quietly widening it to
  midnight.
- **An empty value omits the field**, exactly as an empty CSV cell does.
- **A declared field is not a required field.** The section declares a *type*; an
  object that omits the field is valid. Declaring a type for an object type with
  **no** `objects:` entries is likewise legal (the entries may be arriving, or the
  type may be served by a `providers:` entry) and registers nothing on its own —
  but the declaration is still validated, so a typo fails the build immediately.
- **It applies to `objects:` only.** A `providers:` entry carries its own typing
  (the CSV suffix), so a declaration is never imposed on provider-loaded rows; one
  type declaration in two places could disagree with itself. Inline entries are
  still fully validated against the declaration when a `providers:` entry wins
  their type — validation never depends on precedence — and the canonicalised
  values are discarded with the rest of those entries.
- **A non-string value is its own error**, unlike a CSV cell which is always
  text: a number, array, or object in a declared date field is
  `APERTURE_CONFIG_INVALID` naming the declared `type` and the `kind` found, not a
  confusing parse failure.

A rejection is `APERTURE_CONFIG_INVALID` naming the **object id and the field**,
in both the message and the context, and carries the `reason`
(`provider.DateReasonOf`) for a parse failure. A granularity mismatch is **not** a
parse failure, so it carries no `reason` and `DateReasonOf` correctly reports
none — the same shape the CSV loader's granularity rejection has. Neither ever
carries the value: a date is frequently personal data. The value model's own
error is classified and dropped rather than wrapped, because `DateReasonOf` reads
the **outermost** coded error and wrapping would hide the reason.

**This is a date-type declaration, not a general metadata schema.** No
`required:`, `default:`, `enum:`, `pattern:`, `int`/`float`/`bool`, or nested
field paths. The value model already governs shape, depth, and size; the one
thing it cannot govern is which strings a host means as dates. Like `providers:`
and `objects:`, the section is runtime **wiring**: no storage row, no `Apply`
write, no export.

#### When `providers:` and `objects:` claim the same type

**Precedence is type-level and total: the file-backed `providers:` entry wins,
and every inline `objects:` entry for that type is discarded.** No object-level
merge, no field-level merge, no fallback — an inline id the file lacks is not
resolvable (`APERTURE_NOT_FOUND`, and absent from enumeration), and a field only
the inline entry declared never appears on an object the file does carry. A rule
reading a field the CSV silently did not override is a support ticket nobody can
reproduce; predictability wins over usefulness here.

**That is the default, and a collision builds rather than failing.** Adding a CSV
for a type that still has inline entries is an ordinary migration step, not a
fault: a seed that booted yesterday must not refuse to boot today because a
`providers:` row was added.

The discard is still surfaced. **`doc.ProviderCollisions()` returns the object
types the build discarded** — sorted, deduplicated, types only and never ids
(which can embed an account) — reading the document alone, with no file IO and no
registry, so it answers the same before and after `BuildRegistry`. Nothing in
`seed` picks a logger for the host; the collision is returned as a fact the host
logs however it already logs.

A host that reads the overlap as an authoring mistake instead opts into a
refusal: `doc.BuildRegistry(dir, seed.StrictProviderCollision())` returns
`APERTURE_CONFIG_INVALID` naming every colliding object *type* (sorted, in both
the message and the error context — never the object ids). It is Go wiring, not a
seed-file key, so the file stays a plain declaration and the strictness sits where
a reviewer sees it.

**Validation does not depend on precedence.** Every inline entry is validated
before any type is discarded, so a malformed declaration fails the load even when
its type loses to a `providers:` entry. Two `providers:` entries for one type are
still `APERTURE_PROVIDER_INVALID` — a contradiction with no winner to pick, not a
precedence question.

`provider.Static` is the in-memory `ObjectProvider` behind the section, and is
reusable on its own (tests, embedded data). It is immutable after `NewStatic`
returns, validates the value model again itself rather than trusting its caller,
and **deep-copies its input** so a caller that keeps and mutates those maps cannot
reach into metadata the Registry has already cached. Reads hand out its own maps
**by reference**, exactly as `csvprovider` does.

### The seed document's `attributes:` section

The same YAML spelling, pointed at the party **asking** rather than the thing
being acted on:

```yaml
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
```

Two keys differ from `objects:`, and both differences follow from what an
attribute key *is*:

- **`subject:` is declared, not derived.** An `objects:` id is a segmented
  identity, so its terminal segment names its type. An attribute key is a bare
  opaque handle into the host's directory with no segments to derive anything
  from, so the slot — `user`, `machine`, or `account`, a **closed** set — is
  stated. An unknown one is `APERTURE_ATTRIBUTE_SLOT_UNKNOWN` naming the entry and
  the three legal subjects.
- **Keys are deduplicated per slot**, not across the whole section: a tenant
  called `acme` and a service principal called `acme` are unrelated subjects.

Everything about the VALUE is identical, and deliberately so — one value model,
one normalisation, one rejection:

- **Numbers normalise identically** (raw JSON, `UseNumber`, exact `int64` else
  `float64`), so `principal.clearance == 3` answers the same in YAML and JSON;
- **`metadata:` must be a mapping**, and an omitted or null one is a subject that
  exists and carries nothing — not a missing subject;
- **the bag goes through `provider.ValidateMetadata`** at build. A rejection keeps
  the value model's own **`APERTURE_METADATA_INVALID`**, with the offending entry
  added to the message: `aerr.Wrap` re-stamps, so re-coding it
  `APERTURE_CONFIG_INVALID` would replace the fixups that name the legal shapes
  and the two caps with generic ones. A missing `subject:`/`id:`, a duplicate
  pair, or a non-mapping `metadata:` **is** `APERTURE_CONFIG_INVALID`; the account
  wildcard `"*"` as a key is `APERTURE_ATTRIBUTE_PROVIDER_INVALID`, refused by
  `provider.NewStaticAttributes`, which this loader does not restate.

`Document.BuildAttributeRegistry(baseDir)` registers one
`provider.StaticAttributes` per declared slot with a TTL of 0. Like every other
wiring section: no storage row, no `Apply` write, no export
(`TestAttributeWiringIsNotModelState`).

## Querying the model: `Filter.Fields`

The shape a field holds decides how a `provider.Filter.Fields` predicate compares
against it. A provider *evaluates* `Fields`, but it does not *define* it — `Query`
is how scope enumeration bounds itself, so the rule is stated once on
`provider.Filter` and implemented once in `provider.MatchFields`. A provider
either calls that helper or reproduces it exactly (pushing the predicate into SQL,
say).

| Field shape | Rule | Example |
|---|---|---|
| **array** | **membership** | `Fields{"tags": "premium"}` selects every object whose `tags` **contains** `"premium"` |
| **scalar** | equality | `Fields{"tier": "gold"}` |
| **object** | equality, deep — **not** key membership | `Fields{"owner": map[string]any{"dept": "eng"}}` matches; `Fields{"owner": "dept"}` is a plain `false` |
| *absent* | never matches, `nil` want included | |

Two properties are load-bearing:

- **Comparison is typed, never a string rendering.** Numbers compare across Go
  numeric types by value (`int(5)` == `int64(5)` == `float64(5)`), but `"5"` is
  never `5` and a string equals only a string. That is the same rule the
  expression evaluator applies, so `Enumerate` cannot select an object a `Check`
  over the same value then denies. This is why element typing at load
  (`:list<int>`) is mandatory rather than decorative — it is what a `Fields`
  predicate compares against.
- **A want that is itself a container compares by equality at both ends**, since
  no element of a legal array (scalars only) could ever equal an array or object.

The same rule governs the **enumerate metadata filter**
(`engine.EnumerateRequest.Fields`, and its `service.EnumerateQuery.Fields` /
`--field` / `--fields-json` / Twirp `fields` / MCP `Fields` spellings): it calls
`provider.MatchFields` too, so a shape that behaves one way inside a provider's
`Query` behaves identically when the engine filters an enumeration. Two rules are
the enumeration's own rather than the value model's — the predicate runs only on
candidates that already survived deny-overrides, and it runs **before** the
enumeration's `Limit`. See `skills/decision-api.md`.

`provider.ValuesEqual` is the exported leaf comparison. It **reimplements**
expr-lang's equality rather than importing it, because `provider/` is a strict
leaf; `csvprovider/membership_equivalence_test.go` runs both over the same
metadata so the two cannot drift. The only documented divergences —
`time.Time`/`time.Duration`, and a `uint64` above `math.MaxInt64` — are outside
the value model.

## Searching the model: `MatchText`

`Fields` answers "which objects hold exactly this value?". It is exact, typed and
coercion-free — deliberately, because a predicate that coerced would let
`Enumerate` select an object a `Check` then denies. That same property makes it
unable to answer the other question a caller has: "which objects are people
NAMING when they type `nike`?", where `Nike`, `nike`, `Nike, Inc.` and a typo are
all one intent and none of them is an exact value.

`provider.MatchText(md, query, fields)` is that second question, and it reads the
same value model — only the STRING material in it:

| Field shape | Searched? |
|---|---|
| **string scalar** | yes |
| **array** | yes, each string ELEMENT scored separately (so a match reports the element, not the whole list) |
| number / bool / nil | no — filtering by one of those is `Fields`' job |
| **object** | no — a match NAMES its field, and a nested path is not a field name a caller could restrict to |

The two are kept apart on purpose, and a search composes them: `Fields` narrows
the candidate set, the text query ranks what is left. Nothing about a score is a
decision — see `skills/object-search.md` for the scorer, the floor, and the
entitlement containment that makes the surface safe.

## Errors

A violation is **`APERTURE_METADATA_INVALID`**. Its context carries:

| Key | Meaning |
|---|---|
| `field` | the metadata field name |
| `path` | the path within the value, e.g. `owner.members[0]` |
| `type` | the offending Go type (shape violations) |
| `depth` / `max_depth` | depth violations |
| `bytes` / `max_bytes` | size violations |

The error **never carries the value** — only the field, the path, and the Go
type. That is the cross-account leak rule (`CLAUDE.md`): a validation failure can
be logged and surfaced without one account's data reaching another's diagnostics.

Fields and object keys are walked in **sorted order**, so an object with several
offending values always reports the same one.

## The read-only contract is transitive

The cache stores a provider's map **by reference** and never copies it on read
(allocation-aware on the `Check` hot path). Now that values nest, the nested maps
and slices are shared too:

- a provider returns a **fresh** map per object, with fresh nested containers, and
  a reload builds a new value rather than editing the old one in place — in
  `csvprovider` every list cell is split into its own slice and every `:json`
  cell decoded into its own map, so no two rows share one, and `provider.Static`
  deep-copies its whole object set once at construction so two entries built from
  one source value never share a container either;
- **no holder** — engine, rules, scope, CLI, server, host code — writes to a
  `Metadata` it was given, **at any depth**;
- a consumer that needs to modify metadata copies it deeply first.

## Update-Demand

Changing the value model means changing all of these in the same PR:

| Change | Also update |
|---|---|
| The legal shapes, the caps, or their defaults | this doc + `docs/src/concepts/providers.md` |
| The date value model (`provider/date.go`: the canonical forms, the accept/reject set, `DateReason`) | the "Dates" section above + `docs/src/concepts/providers.md` |
| The `Filter.Fields` rule (`MatchFields`, `MatchField`, `ValuesEqual`) | the `Filter` doc comment in `provider/provider.go` — it is the contract every provider implements — plus this doc and `docs/src/concepts/providers.md`, **and** every restatement of it on the enumerate filter (`skills/api-surface.md`, `skills/decision-api.md`, `docs/src/library/decision-api.md`, `docs/src/library/service-facade.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/surfaces/mcp.md`, `docs/src/cli/decisions.md`) |
| `APERTURE_METADATA_INVALID` | `errors/codes.go` (`AllCodes` + `Registry`), then `make docs-gen` |
| A loader's coercion (`csvprovider`, `sqlprovider`, seed) | the loader must call `provider.Validate*`, not re-implement the rules |
| A loader's **encoding** (the CSV header grammar, a seed key) | "How each loader spells the model" above + the loader's package doc + `docs/src/concepts/providers.md` |
| The **SQL driver-value mapping** (`metadataValue`'s type switch or `mappedDriverTypes` in `sqlprovider/values.go`) | the "`sqlprovider`" section above + `skills/sql-provider.md` + the `sqlprovider` package doc + `docs/src/concepts/providers.md` — gated by `TestDriverValueMappingTableMatchesTheTypeSwitch`, which parses the source with `go/ast` |
| The seed `field_types:` section (its spelling, its vocabulary, what it applies to) | "The seed document's `field_types:` section" above + `docs/src/concepts/seed.md` + `docs/src/concepts/providers.md` — and its two type words must stay identical to the CSV `:date`/`:datetime` suffixes, which is the whole reason it exists |
| The seed `objects:` shape, or `provider.Static` | "The seed document's `objects:` section" above + `docs/src/concepts/seed.md` + `docs/src/concepts/providers.md` — and it must stay **wiring**: no storage table, no `Apply` row, no export |
| The seed `attributes:` shape (a key, the subject vocabulary, the dedup rule), or `provider.StaticAttributes` | "The seed document's `attributes:` section" above + `docs/src/concepts/seed.md` ("Inline subject attributes") + the `Attribute` / `BuildAttributeRegistry` doc comments in `seed/attribute.go` — and it must stay **wiring**: no storage table, no `Apply` row, no export, and no `metadata:` key on `principals:`/`accounts:` |
| An attribute LOADER's spelling of the model (`csvprovider.Attributes`' header handling or its eager load; `sqlprovider.AttributeConfig`'s statements) | "The attribute variant" under the loader above + the loader's file doc + `skills/attribute-providers.md` ("Wiring") + `skills/sql-provider.md` ("The attribute seam") + `docs/src/concepts/providers.md` ("Attribute providers") + `docs/src/concepts/seed.md` ("External attribute sources") — the VALUE half must stay identical to the object loader's (one `parseTable`, one `metadataValue`), and the KEY half — a bare subject id, never an identity — has **no gate and cannot have one**, which is why it is restated in every one of those places |
| The `providers:` / `objects:` precedence rule, or `BuildRegistry`'s options | "When `providers:` and `objects:` claim the same type" above + `docs/src/concepts/seed.md` + the `BuildRegistry` / `StrictProviderCollision` doc comments in `seed/provider.go` and `Document.ProviderCollisions` in `seed/object.go` |

`provider/` imports only `identity`, `errors`, and the standard library — the
value model must not change that.
