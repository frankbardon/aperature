---
name: object-search
description: Resolving a NAME to an object id — the entitlement-scoped, ranked text search over object metadata, why it is one operation rather than an enumerate the caller filters, and the batch metadata read beside it.
applies_to: [engine, service, provider, cli, mcp]
---

# Object search

A surface that takes questions in a person's own words — a chat turn, an agent,
a search box — receives names, and every step after it needs ids. Aperture is
where the names live: it holds the object metadata, and it is the only thing
that knows which of those objects a given subject may see. `Search` is that
lookup.

```go
matches, err := svc.Search(ctx, service.SearchQuery{
    Account: "acme", Principal: "alice", Action: "read",
    Pattern: "account:acme/brand:*",
    Query:   "nike",
    Limit:   5,
})
// matches[0].Object   "account:acme/brand:42"
// matches[0].Score    0.875
// matches[0].Field    "label"
// matches[0].Metadata {"label": "Nike, Inc.", "sector": "Footwear", ...}
```

## Why this is not an Enumerate the caller filters

Every ingredient was already here: `Enumerate` returns the ids a subject may act
on, `ObjectMetadata` returns a bag per id, and a caller could match in its own
process. That composition is not a smaller version of this feature. It is a
different and worse one, on four counts — and only the last two decide it:

1. **Volume.** One id at a time is one round-trip per candidate, per question.
2. **No ranking.** Exact string equality is all a caller can implement without
   inventing a scoring function in the wrong repository, and "Nike", "Nike Inc",
   "nike" and a typo are then four misses.
3. **It inverts the entitlement model.** Enumerating a type and filtering
   afterwards is the bare-enumeration shape: the question stops being "what may
   this subject see?" and becomes "what exists?", answered first and narrowed
   second.
4. **It leaks.** Even with a correct filter, the unscoped set has already
   crossed the boundary. Any bug in that filter turns the calling surface into
   an oracle for every object in the system — and an LLM-driven turn is exactly
   where an over-broad result set does the most damage.

So the requirement is stated positively and held structurally:

> The result is the intersection of "matches the query" and "this subject could
> have obtained it via `Enumerate`", computed as ONE operation.

## How that is held, mechanically

`engine.Search` does not re-derive who may see what. It walks
`engine.walkAllowed` — the same function `Enumerate` walks — which gathers
candidates from the allow grants, decides every one of them with the same
deny-overrides/specificity evaluation `Check` uses, applies the reference
restriction and the `Fields` predicate, and hands over only the survivors. Only
then is anything scored.

The order is the guarantee. **Decide, then match.** A score can only ever
subtract from the allowed set, so a `Search` result is always a subset of the
`Enumerate` result for the same subject, action and pattern. Tests assert that
containment at three layers (`engine`, `service`, `mcp`) rather than once,
because it is the property every other one rests on.

## Search selects; it never authorizes

A score is a ranking hint for whoever is choosing between candidates. **No
grant, rule, or verdict anywhere in Aperture reads one.** A caller that acts on
a selected id still `Check`s it. That this call proposed the id is a shortlist,
not an authority — and the MCP tool description says so in those words, because
an agent has no other source of truth.

## What is matched

Matching is `provider.MatchText`, and only STRING material is matched: a
top-level string field, and the string elements of a top-level array. Numbers,
bools and nil are never text-matched — filtering by one of those is what
`Fields` is for — and nested objects are not descended into, because a match
NAMES the field it came from and a nested path is not a field name a caller
could restrict the search to.

**Aperture has no notion of a "label".** A label is an ordinary metadata field
whose name the host chose, so no field is privileged: the score is the best over
every searched field, `MatchFields` names the ones to search, and
`SearchMatch.Field` reports which one actually won. Building in a blessed field
name would put one host's schema into the engine.

## The scorer

`provider.ScoreText(candidate, query)` returns `[0,1]`. It lives in `provider`
for the same reason `MatchFields` does: a host that can push a search down into
its own storage — a SQL `LIKE`, a trigram index, an external engine — must be
able to rank the way Aperture would, or the ids it proposes and the ids Aperture
would have proposed are two different answers to one question.

Both sides are normalised first (`provider.NormalizeText`): lower case, every
non-alphanumeric rune a separator, runs collapsed. That is what makes
`"Nike, Inc."` and `"nike inc"` the same string.

Each query token then takes its best score against any candidate token, in
descending tiers — exact, prefix, substring, approximate — and the base score is
their **mean**, so a query is only as good as its weakest term. That base is
scaled by **coverage**: the share of the candidate the query accounts for. Two
consequences worth knowing:

- `"nike"` ranks `"Nike Inc"` above `"Nike Air Max Collection 2024"`. Without
  coverage every name containing the word scores identically and a
  disambiguation prompt cannot tell a company from one of its product lines.
- Spelling right always beats spelling close: every approximate match is bounded
  below every substring match, which is bounded below every prefix match.

### The approximate tier, and its two guards

Distance is **Damerau**-Levenshtein, not plain Levenshtein: a transposition
counts as the one slip it is. `"nkie"` for `"nike"` is the commonest way a name
is mistyped, and to plain Levenshtein it is two edits — far enough from a
four-letter word to fall under any threshold strict enough to also exclude
unrelated words. Counting it as one is what lets the floor stay strict.

Two guards keep the tier from becoming a guess:

- **A token shorter than 4 runes must be spelled exactly or be a prefix.** One
  edit in three characters is a third of the word: admitting it would let `IBM`
  answer for `IBN`, and a short ticker or code is where a wrong answer is most
  plausible to a reader.
- **Similarity must reach 2/3** — at most one edit in three characters — before
  an approximate match counts at all.

### `MinScore` is a floor, not a knob

`provider.DefaultMinScore` (0.4) sits just below the weakest score the
approximate tier can produce for a candidate the query fully explains, so a
single typo still resolves while an unrelated word scores zero and is dropped.

A fuzzy matcher with NO floor ranks the subject's entire entitled set and returns
it — which is both useless as a shortlist and precisely the bulk disclosure a
ranked search exists to make unnecessary. Raising `MinScore` narrows the
shortlist; nothing about it ever widens what the subject may see.

## `Limit` caps the ranking, not the scan

This is the one thing about the bound worth reading twice.

- The **scan** is bounded by the engine's configured enumeration bound
  (`WithEnumerateLimit`, default 1000) — the same bound, over the same candidate
  set, that `Enumerate` runs under.
- **`Limit`** then takes the top of the finished ranking.

They are separate numbers deliberately. Bounding the scan by `Limit` would
return the first `Limit` objects that matched *at all* rather than the `Limit`
best, which is not a ranking — it is an arbitrary prefix with a score column.

A scan that comes back holding exactly its bound is WARNED about through the
engine's logger (`WithLogger`). That warning matters more here than it does for
`Enumerate`: a truncated enumeration returns a short list, which at least looks
short, while a truncated search returns a full, confidently ranked shortlist
whose best entry may simply be the best among the candidates that fit. As with
`Enumerate`'s warning it is a HINT, not an assertion — a candidate set of exactly
the bound's size is indistinguishable from a truncated one.

## Composition

`Fields` and `References` mean exactly what they mean on `EnumerateQuery`, and
they compose with `Query`: the predicate and the reference edge narrow the
candidate set, the query ranks what is left. "The brand called Nike in dataset
7" is one call. All three precede `Limit`.

The reference edge keeps its fail-closed rule unchanged here: a holder the
principal may not see yields an EMPTY result and no error, so "you may not see
dataset X" and "dataset X lists nothing you may see" stay untellable apart. The
search surface must not become the place that distinction leaks.

## Failure modes

| Situation | Answer |
|---|---|
| No metadata source wired (`WithMetadata`) | `APERTURE_PROVIDER_UNREGISTERED` — an error, never an empty result |
| Empty `Query` | `APERTURE_INVALID_INPUT` — it is not "match everything" |
| `MinScore` above 1 | `APERTURE_INVALID_INPUT` — no match could ever reach it |
| Principal is not a member of the account | empty result, no error |
| Candidate has no metadata row | skipped; nothing to name is not a failure |
| Holder of a reference edge is invisible | empty result, no error |

Search REQUIRES a metadata source, unlike the `Fields` predicate, which is only
consulted when a request carries one. The asymmetry is the usual one: an empty
result reads as "you may see nothing" and would hide the misconfiguration behind
a plausible answer.

## The batch metadata read

`ObjectMetadataBatch(ctx, ids) -> []engine.BatchResult[map[string]any]` is the
bulk form of `ObjectMetadata`, aligned by index, one bad id never failing its
siblings — the same convention `CheckBatch` / `EnumerateBatch` / `ExplainBatch`
follow.

Most callers will not need it: `Search` already returns each match's metadata
inline, which is what keeps labelling a result set from costing a round-trip per
id. It is for the case where ids come from somewhere else — a stored scope, a
persisted selection — and only their labels are missing.

It is **not** a way to discover ids. It authorizes nothing and filters nothing;
it is `ObjectMetadata`, N times, with the same posture, so a caller passes ids it
already holds legitimately. The path that PRODUCES ids a subject may see is
`Enumerate`, or `Search` when the input is a name.

The batch stops at the facade boundary on purpose. The saving is the round-trip
and the round-trip is there: below it the provider registry already serves
repeats from its per-type cache, and `ObjectProvider.Fetch` cannot grow a batch
form without breaking every host implementation of the interface.

## Surfaces

| Surface | Spelling |
|---|---|
| Library | `service.Search` / `service.SearchBatch`; `engine.Search` / `SearchAs` / `SearchBatch` |
| CLI | `aperture search <principal> <action> <pattern> <query>`, with `--in`, `--min-score`, `--limit`, `--scores`, plus `--field` / `--fields-json` / `--via` |
| Twirp/HTTP | `Search` / `SearchBatch` (`SearchRequest` → `SearchResponse`), converters in `internal/server/twirp.go` |
| MCP | `aperture_search`, `aperture_search_batch` (read-only, like every MCP tool) |

The containment property is asserted **on every one of them** — `engine`,
`service`, `internal/server` and `mcp` each have their own "search never proposes
what enumerate withholds" test — rather than once at the bottom. That is the same
reasoning the enumerate reference edge's empty-vs-`NOT_FOUND` split is tested
per-surface under: a relaxation in one translator is a silent disclosure channel,
and a test that lives only in the engine would not see it.

Two things the wire adds that the other surfaces do not have to think about:

- **The ranking IS the payload.** `SearchResponse.matches` is ordered, and a
  translator that reordered it would return the right SET while discarding the
  whole answer. That is why the order is asserted over a real HTTP round trip
  rather than in-process.
- **A match that cannot be encoded fails the call**, rather than returning with
  an empty `metadata` map. A client cannot tell an object with genuinely empty
  metadata from one whose bag failed to encode, so the second must not be able to
  masquerade as the first.
