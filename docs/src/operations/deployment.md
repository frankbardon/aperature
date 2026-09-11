# Deployment

**Audience:** operators running Aperture as a long-lived service.

Aperture ships as a single pure-Go binary (`CGO_ENABLED=0`, no external runtime).
Running it as a service is one command — `aperture serve` — which hand-wires the
whole dependency graph (`storage → engine → service → HTTP handler`) and boots a
`net/http` server exposing the HTTP + Twirp API and the admin UI.

```bash
bin/aperture serve --addr :8080
```

```text
aperture serving on :8080
```

`serve` listens on `:8080` by default and shuts down gracefully on `SIGINT` /
`SIGTERM`, draining in-flight requests within a 10-second window before it
forces the listener closed.

## Flags

| Flag | Default | Env source | Purpose |
|---|---|---|---|
| `--addr` | `:8080` | — | TCP address to listen on. |
| `--store` | *(in-memory)* | — | SQLite DSN for the backing store. Empty ⇒ ephemeral in-memory store. |
| `--seed` | *(embedded example)* | — | Path to a JSON/YAML seed model. Empty ⇒ the embedded `acme` example fixture. |
| `--auth` | `dev` | `APERTURE_AUTH_MODE` | Authenticator adapter: `dev`, `oidc`, or `parsec`. The flag **overrides** the env var. |
| `--enforce-membership` | off | `APERTURE_ENFORCE_MEMBERSHIP` | Deny any decision whose principal is not a member of the active account, before grants are consulted. |
| `--enumerate-limit` | `1000` | `APERTURE_ENUMERATE_LIMIT` | Ceiling one enumeration is bounded by — the number a request with a non-positive `limit` receives, and the number a larger `limit` is clamped down to. Bounds the scope member gather too, so one value governs both. The flag **overrides** the env var; a non-numeric value fails the boot with `APERTURE_CONFIG_INVALID`. |

The generated, always-current flag table is the
[Command-Line Reference](../reference/cli.md#aperture-serve).

## The backing store (`--store`)

`--store` selects the storage backend:

- **Empty** (the default) → the pure-Go in-memory backend. Ideal for demos, CI,
  and read-only trials; nothing is persisted across restarts.
- **A DSN** → the SQLite backend (`modernc.org/sqlite`, pure-Go, so CGO stays
  off). The DSN is a `modernc.org/sqlite` data source name — a file path or a
  `file:` URL with pragmas, for example:

  ```bash
  bin/aperture serve --store 'file:aperture.db?_pragma=busy_timeout(5000)'
  ```

The SQLite pool is capped at a single connection so writes serialize cleanly
under SQLite's single-writer model. On startup the server runs the embedded
schema (`Setup`) and then loads the model from `--seed` (or the embedded
example) into the store.

## Configuration precedence

Aperture is configured, in order of increasing precedence:

1. **`.env` file** — when you launch through `make`, a `.env` in the working
   directory is loaded and its keys are exported into the environment before the
   binary runs (`include .env; export` in the `Makefile`). This is the dotenv
   convenience; the binary itself simply reads its process environment.
2. **`APERTURE_*` environment variables** — the primary configuration surface.
   The authenticator is built from these via `auth.ConfigFromEnv` (see below).
3. **Command-line flags** — a flag that declares an env source **overrides** that
   env var. For example `--auth oidc` wins over `APERTURE_AUTH_MODE=dev`.

The seed **model** itself is authored as YAML (or JSON) and supplied with
`--seed`; that document also carries the `connections:` and `providers:` wiring
the server turns into a live object-provider registry. Model YAML is data, not
process config — the two are separate.

One deployment consequence of that split: a `kind: sql` provider names its
database through `dsn_env:`, and a **literal `dsn:` in a seed file is refused at
parse**. So each declared connection needs its environment variable exported
where the process runs — the seed file names the variable, the environment holds
the credential. Nothing is dialled at startup (`sql.Open` is lazy and Aperture
does not ping), so a wrong host or password surfaces on the first decision
touching a SQL-backed object-type as `APERTURE_SQL_PROVIDER_QUERY`, not as a
failed boot. See
[Database-backed providers](../concepts/seed.md#database-backed-providers).

### Authentication environment variables

The authenticator adapter maps each request to an Aperture principal. The
default is `dev` (the bearer token *is* the principal id), so `serve` runs with
**no external identity provider** out of the box; `oidc` and `parsec` are opt-in.

| Variable | Applies to | Meaning |
|---|---|---|
| `APERTURE_AUTH_MODE` | all | Adapter: `dev` \| `oidc` \| `parsec`. Empty ⇒ `dev`. |
| `APERTURE_AUTH_PRINCIPAL_CLAIM` | oidc, parsec | Verified claim mapped to the principal id (`sub`, `email`, …). Empty ⇒ `sub`. The `dev` adapter ignores it. |
| `APERTURE_OIDC_ISSUER` | oidc | OIDC issuer URL. |
| `APERTURE_OIDC_AUDIENCE` | oidc | Expected token audience. |
| `APERTURE_OIDC_JWKS_URL` | oidc | JWKS endpoint for signature verification. |
| `APERTURE_PARSEC_KEYRING` | parsec | Path to the broker's persisted signing keyring (`keyring.json`) Aperture verifies brokered tokens against. |
| `APERTURE_PARSEC_STATE_DIR` | parsec | Parsec broker state directory. |

An unrecognised `APERTURE_AUTH_MODE` fails the boot with `APERTURE_CONFIG_INVALID`.
The `oidc` adapter performs network discovery at startup, so misconfiguration
there surfaces immediately when `serve` boots.

The `enforce-membership` toggle (`--enforce-membership` /
`APERTURE_ENFORCE_MEMBERSHIP`) is defence-in-depth: a non-member of the active
account is denied *before* any grant is read, which is what lets a single shared
role (manager, analyst, …) be reused across customer accounts without one
account's grants leaking to another's members.

## Manual dependency injection

`serve` builds its graph with **plain constructors — no DI framework** (no
wire/fx/dig). Each layer is a hand-written call:

```text
buildStore(--store, --seed)         # storage backend + schema + seed
  → engine.New(store, …)            # decision engine (+ scope resolution, membership)
    → service.New(eng, …)           # the fully-wired facade
      → server.New(svc)             # HTTP + Twirp handlers + admin UI
        → server.Authenticate(authn, …)   # request → principal middleware
```

The same fully-wired facade the mutation CLI commands build is what `serve` puts
behind a listener — the engine for decisions, the admin gate for tier checks, the
delegation and impersonation services, the append-only audit trail (decisions
sampled at 100 % under `serve` so the demo trail is legible), the rules engine
over a storage-backed rule source, and the seed's declared object providers. A
rule saved through the admin UI takes effect on the next decision with no
separate rule store.

Because the wiring is explicit Go, there is no configuration container to learn:
the constructor order in `internal/cli/serve.go` *is* the deployment topology.

## Related

- [serve](../cli/serve.md) — the command page, with the flag walkthrough.
- [Performance & NFR](performance.md) — the decision hot-path budget and how to
  assert it.
- [Troubleshooting](troubleshooting.md) — reading and acting on `APERTURE_*`
  boot/runtime errors.
- [Command-Line Reference](../reference/cli.md#aperture-serve) — the generated
  flag table.
