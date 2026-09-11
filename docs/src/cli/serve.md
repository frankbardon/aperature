# serve

**Audience:** operators running Aperture as a long-lived service.

```text
aperture serve [options]
```

`serve` hand-wires the full dependency graph (`storage → engine → service →
HTTP handler`) and boots a `net/http` server exposing the HTTP + Twirp API and
the admin UI. It shuts down gracefully on `SIGINT` / `SIGTERM`, draining
in-flight requests within a 10-second window. This is the same fully-wired facade
the mutation CLI commands build — the server just puts it behind a listener and
an authenticator.

```bash
bin/aperture serve --addr :8080
```

```text
aperture serving on :8080
```

Press `Ctrl-C` to trigger a graceful shutdown (`shutting down...`).

## What the flags control

- `--addr` — the TCP address to listen on (default `:8080`).
- `--seed` / `--store` — the model to serve, exactly as elsewhere (see
  [Global options](global-options.md)). With no `--store`, the server runs
  against an in-memory model seeded from `--seed` or the embedded example.
- `--auth` — the authenticator adapter that maps each request to a principal:
  `dev` (the default — the bearer token *is* the principal id, no external IdP),
  `oidc`, or `parsec`. It overrides the `APERTURE_AUTH_MODE` env var. Because the
  default is `dev`, `serve` runs with **no external identity provider** out of
  the box; `oidc` and `parsec` are opt-in.
- `--enforce-membership` — defence-in-depth: deny any decision whose principal
  is not a member of the active account *before* grants are consulted. This lets
  a single shared role (manager, analyst, …) be reused across accounts without
  one account's grants leaking to another's members. Also settable via
  `APERTURE_ENFORCE_MEMBERSHIP`.
- `--enumerate-limit` — the ceiling one `Enumerate` is bounded by: the number a
  request with a non-positive `limit` receives, and the number a larger request
  `limit` is clamped **down** to. It also bounds the scope member gather, so the
  gather and the result cap are one value. Unset leaves the engine on its
  documented default of `1000`. Also settable via `APERTURE_ENUMERATE_LIMIT`; the
  flag wins when both are given.

  ```bash
  bin/aperture serve --enumerate-limit 1500
  APERTURE_ENUMERATE_LIMIT=1500 bin/aperture serve
  ```

  A value that is not a whole number fails the boot with
  `APERTURE_CONFIG_INVALID` naming the setting and the value it rejected —
  before the store is opened — rather than quietly serving the default.

Under `serve`, the facade is wired with everything the other surfaces expect: the
admin gate, delegation and impersonation mutators, the append-only audit trail,
the rules engine over a storage-backed rule source, and the object providers
declared in the seed's `providers:` section. A rule saved through the admin UI
takes effect on the next decision with no separate rule store.

Full flags: [`serve`](../reference/cli.md#aperture-serve).

## Related

- [Global options](global-options.md) — `--seed` / `--store`.
- [mcp](mcp.md) — the read-only stdio surface, for MCP clients rather than HTTP.
- [Command-Line Reference](../reference/cli.md#aperture-serve) — the generated flag table.
