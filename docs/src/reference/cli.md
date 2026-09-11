<!-- DO NOT EDIT — regenerate with `make docs-gen` -->

# Command-Line Reference

**Audience:** operators and integrators driving Aperture from a shell.

`aperture` — Fine-grained access control engine. This page is generated from the urfave/cli command tree in `internal/cli` (`cli.NewApp`); every command, subcommand, and flag below is read from the live definitions.

## Global flags

`aperture` declares no persistent global flags. The commonly shared options — `--seed`, `--store`, `--account`, and `--principal` (the acting principal on mutations, sourced from `APERTURE_PRINCIPAL`) — are defined per command and appear in each command's flag table below.

## Commands

| Command | Summary |
| --- | --- |
| [`attributes`](#aperture-attributes) | Inspect the attribute directories a seed wires, read one, or drop cached bags |
| [`bestow`](#aperture-bestow) | Bestow (delegate) a grant you hold to another principal |
| [`bulk`](#aperture-bulk) | Provision or deprovision many grants in one transactional call |
| [`check`](#aperture-check) | Decide whether a principal may take an action on an object |
| [`delete`](#aperture-delete) | Delete an entity (object-type\|permission\|principal\|role\|group\|account\|grant\|membership) |
| [`enumerate`](#aperture-enumerate) | List the objects a principal may act on |
| [`explain`](#aperture-explain) | Explain why a decision resolved the way it did |
| [`export`](#aperture-export) | Export the whole model to a single JSON/YAML state file (system-admin tier) |
| [`get`](#aperture-get) | Read one entity by id (object-type\|permission\|principal\|role\|group\|account\|grant) |
| [`identifiers`](#aperture-identifiers) | List all valid instance ids of an object type from its provider |
| [`impersonate`](#aperture-impersonate) | Start a time-boxed impersonation session (prints the session) |
| [`import`](#aperture-import) | Apply a JSON/YAML state file as an idempotent transactional upsert (system-admin tier) |
| [`list`](#aperture-list) | List entities of a kind (object-types\|permissions\|principals\|roles\|groups\|accounts\|grants) |
| [`mcp`](#aperture-mcp) | Serve the read-only Aperture MCP surface over stdio |
| [`put`](#aperture-put) | Create or update an entity (object-type\|permission\|principal\|role\|group\|account\|membership\|grant) |
| [`revoke`](#aperture-revoke) | Revoke a grant you previously bestowed |
| [`serve`](#aperture-serve) | Run the Aperture HTTP server |
| [`template`](#aperture-template) | Manage and apply provisioning templates |

## `aperture attributes`

Inspect the attribute directories a seed wires, read one, or drop cached bags

An attribute slot is a HOST DIRECTORY — the user table, the service-account
registry, the tenant catalogue — that a rule reads `principal.*` and `account.*`
out of. There are exactly three slots (user, machine, account) and each caches
the bags it has fetched, per slot, with its own ttl: and max_size:.

THE CACHE WINDOW IS A SECURITY PROPERTY, not only a tuning knob. Object metadata
that goes stale for a TTL is usually tolerable. An attribute bag is the ASKER'S
STANDING — the clearance, the department, the plan — so until a cached bag
expires, every decision about that subject keeps evaluating against access the
host has ALREADY TAKEN AWAY. Principals are the classic revoke case, and a
revocation that takes effect `ttl:` later is a revocation that has not happened
yet. What a slot's ttl: buys in fetch traffic it pays for in that delay.

So: pick a slot's ttl: for how fast its revocations must land, read back what a
deployment is actually running with `aperture attributes slots`, and close the
window on a specific subject with `aperture attributes invalidate`.

Reading a directory in bulk (`query`) and dropping cached bags (`invalidate`)
are SYSTEM-TIER operations: both require --principal holding system-admin
authority in --account, and a refusal returns nothing at all — no partial page,
no count, and nothing that tells an unauthorized caller which slots exist.

```
aperture attributes <command>
```

### `aperture attributes invalidate`

Drop cached attribute bags so the next decision re-reads them (system-admin tier)

Drops cached bags, so the next decision about the affected subjects pulls fresh
ones from the host directory. Three forms:

```text
  aperture attributes invalidate user --id alice   one subject, one slot
  aperture attributes invalidate user             every bag in one slot
  aperture attributes invalidate --all            every bag in every slot
```

INVALIDATION IS A SECURITY CONTROL, NOT A PERFORMANCE KNOB. A cached attribute
bag is the asker's standing, so a REVOKED CLEARANCE KEEPS AUTHORIZING until that
bag expires: for the length of the slot's ttl:, every decision about that
subject is made against access the host has already removed. Waiting the window
out is not a remedy, it is the exposure. An operator who has just removed
someone's access invalidates that subject here, and then the removal is true.

Scope: this drops the caches of THE PROCESS THAT RUNS IT. That makes it exact
for a host embedding Aperture (it is the operator's spelling of
provider.AttributeRegistry.Invalidate, which such a host calls the moment its
directory changes) and it makes a ONE-SHOT invocation self-contained: this
process starts with a cold cache and exits with it, so there is nothing here for
a stale bag to survive in. For a long-running `aperture serve`, the controls
that reach ITS cache are the slot's ttl: — set it to how fast that directory's
revocations must land — and a restart.

Requires --principal holding system-admin authority in --account: the result
reports whether a bag was cached, which is a fact about who has recently been
decided about, and clearing a large slot costs the next wave of decisions a
provider round-trip each.

```
aperture attributes invalidate [options] <slot>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--all` | — | bool | — | clear EVERY slot's cache; takes no &lt;slot&gt; argument and no --id |
| `--id` | — | string | — | drop only this subject's cached bag (a bare principal or account id); omit to clear the whole slot |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture attributes query`

Read a page of one attribute slot's directory (system-admin tier)

Returns up to --limit records of &lt;slot&gt; — user, machine, or account — as a JSON
array of {id, attributes}, narrowed by attribute predicates.

THIS IS A SYSTEM-TIER READ. Unfiltered, it returns the head of the host's user
table, keys and bags together, so it requires --principal holding system-admin
authority in --account. A refusal returns NOTHING — no partial page, no count,
and no way to tell an empty slot from a full one or from an unwired one. Ask
`aperture explain` about your own authority if a refusal is unexpected.

--field and --fields-json narrow the result by ATTRIBUTE, on exactly the
predicate `aperture enumerate` applies to object metadata: predicates are ANDed,
a field the bag does not carry never matches, a list-valued field matches by
membership, and everything else matches by TYPED equality, so the string "5"
never matches the number 5. --field always sends a string; use --fields-json
when a number, bool, or list is genuinely meant:

```text
  aperture attributes query user --principal alice --account acme \
    --field department=eng --fields-json '{"clearance":3}'
```

Both may be given together: --fields-json is merged FIRST and --field entries
then override it by key.

A slot whose sql: entry declares no get_all: is FETCH-ONLY by design — it can
answer the decision path without exposing the whole table to an enumeration —
and this command reports that provider's coded refusal rather than an empty
page.

```
aperture attributes query [options] <slot>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--field` | — | string | — | object-metadata predicate as key=value, repeatable; the value is ALWAYS a string, so --field seats=5 matches the string "5" and never the number 5 (use --fields-json for that). Overrides --fields-json on a key collision |
| `--fields-json` | — | string | — | object-metadata predicates as a JSON object, for values that are genuinely a number, bool, or list (e.g. '{"seats":5,"active":true,"tags":["a"]}'). Merged first; --field entries then override by key |
| `--limit` | — | int | `0` | cap the number of returned records (&lt;=0 means the default; the registry clamps it regardless) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture attributes slots`

List the three attribute slots, the source each is wired to, and its cache settings

Prints one row per slot — user, machine, account — with the source the seed
declares for it (csv, sql, or inline), the cache freshness window, the cached-bag
cap, and how many bags this process currently holds.

THE TTL COLUMN IS THE REVOCATION WINDOW. A slot's cached bag keeps authorizing
until it expires, so `ttl` is the longest a removed clearance can keep working.
`never` means a bag, once fetched, is only dropped by eviction or by an explicit
`aperture attributes invalidate` — correct for a fixed inline block, dangerous
for a live directory.

The `cached` column counts THIS process's cache. A one-shot invocation starts
cold, so it reads 0; it is the number that matters in a long-running
`aperture serve`.

No actor is required: this reports the wiring in the seed file you passed and
the configuration this process built from it. It contacts no provider and prints
no subject key and no attribute value.

```
aperture attributes slots [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture bestow`

Bestow (delegate) a grant you hold to another principal

```
aperture bestow [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--delegator` | — | string | — | principal bestowing the grant (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--file` | — | string | — | path to a JSON grant body |
| `--json` | — | string | — | grant body as inline JSON |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture bulk`

Provision or deprovision many grants in one transactional call

```
aperture bulk <command>
```

### `aperture bulk grant`

Apply many grants atomically (account-admin tier)

```
aperture bulk grant [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON array of grant bodies |
| `--json` | — | string | — | a JSON array of grant bodies |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture bulk revoke`

Delete many grants atomically (account-admin tier)

```
aperture bulk revoke [options] [<grant-id>...]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--grant` | — | string | — | grant id to revoke (repeatable) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture check`

Decide whether a principal may take an action on an object

```
aperture check [options] <principal> <action> <object>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the decision is scoped to |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture delete`

Delete an entity (object-type|permission|principal|role|group|account|grant|membership)

```
aperture delete [options] <kind> [<id>]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--account-id` | — | string | — | membership account id (kind=membership) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--principal-id` | — | string | — | membership principal id (kind=membership) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture enumerate`

List the objects a principal may act on

Lists every object id under &lt;pattern&gt; that &lt;principal&gt; may take &lt;action&gt; on.

--field and --fields-json narrow that list by OBJECT METADATA. The predicate is typed:
a field matches only when its value equals the wanted value AND is of the same kind, so
the string "5" never matches the number 5. --field always sends a STRING; use
--fields-json when a number, bool, or list is genuinely meant:

```text
  --field tier=premium --field current_brands=brand:Y
  --fields-json '{"seats":5,"active":true,"tags":["public"]}'
```

Both may be given together: --fields-json is merged FIRST and --field entries then
OVERRIDE it by key. Predicates are ANDed; a field the object does not carry never
matches; a list-valued field matches by membership. Filtering happens before --limit.

--via restricts the list to what a DECLARED REFERENCE names — the other direction:
--field asks "which datasets contain brand Y?", --via asks "which brands does
dataset X list?". It is spelled &lt;holder-identity&gt;.&lt;field&gt;, where the field is
everything after the LAST '.', and it is repeatable (edges are ANDed):

```text
  --via account:acme/dataset:x.current_brands
```

A holder you may not read yields an EMPTY list and no error, which is deliberate:
"you may not see dataset X" and "dataset X lists nothing you may see" must not be
tellable apart. Restriction, like filtering, happens before --limit.

```
aperture enumerate [options] <principal> <action> <pattern>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the enumeration is scoped to |
| `--field` | — | string | — | object-metadata predicate as key=value, repeatable; the value is ALWAYS a string, so --field seats=5 matches the string "5" and never the number 5 (use --fields-json for that). Overrides --fields-json on a key collision |
| `--fields-json` | — | string | — | object-metadata predicates as a JSON object, for values that are genuinely a number, bool, or list (e.g. '{"seats":5,"active":true,"tags":["a"]}'). Merged first; --field entries then override by key |
| `--limit` | — | int | `0` | cap the number of returned object ids (&lt;=0 means the default) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--via` | — | string | — | restrict the result to the objects a holder's declared reference field names, as &lt;holder-identity&gt;.&lt;field&gt; (e.g. --via account:acme/dataset:x.current_brands); repeatable, and several edges are ANDed. The FIELD is everything after the LAST '.' |

## `aperture explain`

Explain why a decision resolved the way it did

```
aperture explain [options] <principal> <action> <object>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | `"acme"` | active account the decision is scoped to |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture export`

Export the whole model to a single JSON/YAML state file (system-admin tier)

```
aperture export [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--format` | — | string | — | output format: json (default) or yaml |
| `--out` | — | string | — | write the state file to this path (default: stdout) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture get`

Read one entity by id (object-type|permission|principal|role|group|account|grant)

```
aperture get [options] <kind> <id>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture identifiers`

List all valid instance ids of an object type from its provider

```
aperture identifiers [options] <object_type>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--exclude` | — | string | — | id to omit from the result (repeatable); expands an exclusive allowance |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture impersonate`

Start a time-boxed impersonation session (prints the session)

```
aperture impersonate [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (**required**) |
| `--mode` | — | string | `"augment"` | augment\|become |
| `--operator` | — | string | — | operator principal (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--target` | — | string | — | target principal to impersonate (**required**) |

## `aperture import`

Apply a JSON/YAML state file as an idempotent transactional upsert (system-admin tier)

```
aperture import [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to the JSON/YAML state file (default: stdin, treated as JSON) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture list`

List entities of a kind (object-types|permissions|principals|roles|groups|accounts|grants)

```
aperture list [options] <kind>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | account to list grants for (required for kind=grant) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture mcp`

Serve the read-only Aperture MCP surface over stdio

Exposes Aperture's decision API (check/enumerate/explain, single + bulk), a read-only what-if simulator, and model inspection as MCP tools over stdio. No tool mutates. Intended to be spawned over stdio by an MCP client.

```
aperture mcp [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture put`

Create or update an entity (object-type|permission|principal|role|group|account|membership|grant)

```
aperture put [options] <kind>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON entity body |
| `--json` | — | string | — | entity body as inline JSON |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture revoke`

Revoke a grant you previously bestowed

```
aperture revoke [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--delegator` | — | string | — | principal revoking the grant (env: `APERTURE_PRINCIPAL`) (**required**) |
| `--grant` | — | string | — | id of the grant to revoke (**required**) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture serve`

Run the Aperture HTTP server

```
aperture serve [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--addr` | — | string | `":8080"` | TCP address to listen on |
| `--auth` | — | string | — | authenticator adapter: dev\|oidc\|parsec (overrides APERTURE_AUTH_MODE; defaults to dev — bearer is the principal id, no external IdP) (env: `APERTURE_AUTH_MODE`) |
| `--enforce-membership` | — | bool | — | deny any decision whose principal is not a member of the active account, before grants are consulted (defence-in-depth; lets shared roles be reused across accounts safely) (env: `APERTURE_ENFORCE_MEMBERSHIP`) |
| `--enumerate-limit` | — | string | — | maximum number of object ids one enumeration returns, and the ceiling a larger request Limit is clamped down to (a whole number greater than zero; default 1000; overrides APERTURE_ENUMERATE_LIMIT) (env: `APERTURE_ENUMERATE_LIMIT`) |
| `--manage-accounts` | — | bool | — | manage the lifecycle of account records — allow account create/update/delete through the API (default true; overrides APERTURE_MANAGE_ACCOUNTS). Pass --manage-accounts=false when accounts are mastered by an upstream system: Aperture then refuses every account write regardless of the caller's authority, while account reads and every decision stay unaffected. Read once at startup; a restart is required to change it |
| `--manage-memberships` | — | bool | — | manage the lifecycle of principal-to-account memberships — allow membership create/update/delete through the API (default true; overrides APERTURE_MANAGE_MEMBERSHIPS). Independent of the other two, so a deployment can master accounts and principals upstream and still decide who belongs to what, or the reverse. Read once at startup; a restart is required to change it |
| `--manage-principals` | — | bool | — | manage the lifecycle of principal records — allow principal create/update/delete through the API (default true; overrides APERTURE_MANAGE_PRINCIPALS). Pass --manage-principals=false when principals are mastered by an upstream directory or IdP: Aperture then refuses every principal write regardless of the caller's authority, while principal reads and every decision stay unaffected. Read once at startup; a restart is required to change it |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

## `aperture template`

Manage and apply provisioning templates

```
aperture template <command>
```

### `aperture template apply`

Apply a template transactionally into --account (account-admin tier)

```
aperture template apply [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--id-prefix` | — | string | — | prefix for generated grant ids |
| `--name` | — | string | — | template name to apply (**required**) |
| `--param` | — | string | — | parameter as name=value (repeatable) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--version` | — | int | `0` | template version (0 = latest) |

### `aperture template delete`

Delete a template version, or all versions (system-admin tier)

```
aperture template delete [options] <name>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--version` | — | int | `0` | template version to delete (0 = all versions of the name) |

### `aperture template get`

Read a template by name (latest version unless --version)

```
aperture template get [options] <name>
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |
| `--version` | — | int | `0` | template version (0 = latest) |

### `aperture template list`

List every template version

```
aperture template list [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

### `aperture template put`

Create or update a template (system-admin tier)

```
aperture template put [options]
```

| Name | Aliases | Type | Default | Usage |
| --- | --- | --- | --- | --- |
| `--account` | — | string | — | active account (required for system-tier authority resolution) |
| `--file` | — | string | — | path to a JSON template body |
| `--json` | — | string | — | template body as inline JSON |
| `--principal` | — | string | — | authenticated principal performing the mutation (env: `APERTURE_PRINCIPAL`) |
| `--seed` | — | string | — | path to a JSON/YAML seed model (defaults to the embedded example) |
| `--store` | — | string | — | DSN for the backing store: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (defaults to in-memory). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema; unset uses the connection's search_path |

