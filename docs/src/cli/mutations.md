# Mutations

**Audience:** administrators changing the Aperture model from a shell.

The mutation commands read and change the model: generic entity CRUD (`put`,
`get`, `list`, `delete`), delegation (`bestow`, `revoke`), and impersonation
(`impersonate`). Each builds the *fully-wired* facade — the same
`storage → engine → gate → delegation → impersonation → service` graph the
`serve` command mounts — so a CLI mutation is gated exactly as the HTTP/Twirp
surface gates it. There is no CLI-only write path.

Every write command needs an **acting principal**: the authenticated caller
performing the change, taken from `--principal` (or `APERTURE_PRINCIPAL`) — not a
positional argument. The reads (`get`, `list`) need no actor. See
[Global options](global-options.md#the-acting-principal---principal) for the
subject-vs-actor distinction, and set it once per shell:

```bash
export APERTURE_PRINCIPAL=root
```

Entity bodies are the canonical JSON encoding of the corresponding `model.*`
struct — the same shape the Twirp `entity_json` field carries. Supply one with
`--json`, `--file`, or on stdin (in that order).

## Entity CRUD: `put`, `get`, `list`, `delete`

`put <kind>` creates or updates one entity; the admin tier it requires depends
on the kind. It prints `put <kind> ok`.

```bash
bin/aperture put grant --account acme --json '{
  "id": "g-analyst-read",
  "accountId": "acme",
  "subject": {"kind": "role", "id": "analyst"},
  "permissionId": "perm-doc-read",
  "object": "account:acme/project:atlas/**",
  "effect": "allow"
}'
```

`get <kind> <id>` reads one entity as pretty JSON (no actor, no tier):

```bash
bin/aperture get grant g-eng-read-atlas
```

```json
{
  "ID": "g-eng-read-atlas",
  "AccountID": "acme",
  "Subject": { "Kind": "group", "ID": "engineering" },
  "PermissionID": "perm-doc-read",
  "Object": "account:acme/project:atlas/**",
  "Effect": "allow"
}
```

`list <kind>` lists entities of a kind as JSON. Listing grants requires
`--account` (grants are per-account); the other kinds do not:

```bash
bin/aperture list principals
bin/aperture list grants --account acme
```

`delete <kind> <id>` removes one entity, gated by the kind's tier; it prints
`delete <kind> <id> ok`. Memberships are keyed by (principal, account) rather
than a single id, so `delete membership` takes `--principal-id` and
`--account-id` instead:

```bash
bin/aperture delete grant g-analyst-read --account acme
bin/aperture delete membership --account acme \
  --principal-id bob --account-id acme
```

Full flags: [`put`](../reference/cli.md#aperture-put),
[`get`](../reference/cli.md#aperture-get),
[`list`](../reference/cli.md#aperture-list),
[`delete`](../reference/cli.md#aperture-delete).

## Delegation: `bestow` and `revoke`

`bestow` lets a principal *delegate* a grant it already holds to another
principal. Unlike `put grant`, it is not gated by an admin tier — it enforces the
delegation **subset rule**: you can only bestow authority you hold, over a
delegatable permission. The delegating principal is named by `--delegator` (env
`APERTURE_PRINCIPAL`); the grant body is a normal grant JSON. It prints
`bestow <grant-id> ok`.

```bash
bin/aperture bestow --delegator alice --json '{
  "id": "g-bob-read-42",
  "accountId": "acme",
  "subject": {"kind": "principal", "id": "bob"},
  "permissionId": "perm-doc-read",
  "object": "account:acme/project:atlas/document:42",
  "effect": "allow"
}'
```

`revoke` is the inverse: it removes a grant the delegator previously bestowed,
by id. It prints `revoke <grant-id> ok`.

```bash
bin/aperture revoke --delegator alice --grant g-bob-read-42
```

Full flags: [`bestow`](../reference/cli.md#aperture-bestow),
[`revoke`](../reference/cli.md#aperture-revoke).

## Impersonation: `impersonate`

`impersonate` starts a time-boxed session in which an `--operator` acts as a
`--target` within `--account`, and prints the session as JSON. `--mode` is
`augment` (add the target's authority to the operator's — the default) or
`become` (resolve purely as the target). It is guarded: an operator with no
right covering the target is denied with `APERTURE_IMPERSONATION_DENIED`.

```bash
bin/aperture impersonate --operator root --target alice --account acme --mode augment
```

Full flags: [`impersonate`](../reference/cli.md#aperture-impersonate).

## Related

- [Global options](global-options.md) — `--principal` / `--account` / `--seed` / `--store`.
- [Decisions](decisions.md) — check the effect of a grant you just changed.
- [Provisioning](provisioning.md) — apply many grants at once via templates or `bulk`.
- [Portability](portability.md) — move a whole model between stores.
- [Command-Line Reference](../reference/cli.md) — the generated flag tables.
