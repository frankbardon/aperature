# Extending Aperture

**Audience:** contributors adding a new extension point to the engine or a
surface.

Each recipe below is a short, concrete how-to grounded in the real code: the
files to touch, the interface to implement, and the test or gate you must
satisfy. They assume you have read [Architecture](architecture.md) and
[Package layout](package-layout.md). Every recipe leans on a package's concept
chapter for the *why* — follow those links rather than re-deriving the domain
here.

Run `make test` (`go test ./...`), `make vet`, and `make lint` before you open a
PR. Some changes also trip the [Update-Demand rule](update-demand.md) — a
surface change that lands without its `skills/*.md` doc is a CI failure.

---

## Adding an error code

**Concept:** [Error taxonomy](../concepts/errors.md). **Files:** `errors/codes.go`,
then `make docs-gen`.

1. **Declare the constant** in the `const` block in `errors/codes.go`. Codes are
   `SCREAMING_SNAKE`, `APERTURE_`-prefixed, typed `Code`, with a doc comment
   explaining when it is raised:

   ```go
   // APERTURE_WIDGET_JAMMED — the widget resolver could not advance.
   APERTURE_WIDGET_JAMMED Code = "APERTURE_WIDGET_JAMMED"
   ```

2. **Append it to `AllCodes`** — the slice every gate walks.

3. **Add a `Registry` entry** with a `Message` and either at least one `Fixup` or
   `FixupNotApplicable: true`:

   ```go
   APERTURE_WIDGET_JAMMED: {
       Message: "the widget resolver could not advance",
       Fixups:  []string{"retry the request", "check the widget provider health"},
   },
   ```

4. **Regenerate the reference table:** `make docs-gen` reruns
   `internal/docsgen/errcodes` over `errors.Registry` and rewrites
   `docs/src/reference/error-codes.md` (committed; no CI drift gate). Commit the
   regenerated file with your change.

**Gates you must satisfy** (in `errors/codes_test.go`):

- `TestCodesHaveFixups` — every code has a Registry entry with a Message and a
  Fixup (or `FixupNotApplicable`).
- `TestRegistryHasNoOrphans` — the Registry contains nothing absent from
  `AllCodes`.
- `TestCodesAreScreamingSnakeNamespaced` — every code is `SCREAMING_SNAKE` and
  `APERTURE_`-prefixed.

Construct the error at the raise site with `errors.New` / `Newf` / `Wrap` /
`Wrapf` (or `errors.WithContext` for a details map); recover it with
`errors.CodeOf`. Any error already carrying an `APERTURE_*` code passes through
verbatim — the wrappers never re-stamp it.

---

## Adding a scope strategy

**Concept:** [Scopes & scope strategies](../concepts/scopes.md). **Files:**
`scope/` (a new resolver + factory), then register it on your `scope.Registry`.

A scope strategy decides a grant's object membership. Implement
`scope.ScopeResolver`:

```go
type ScopeResolver interface {
    Contains(ctx context.Context, object identity.Identity) (bool, error)
    Members(ctx context.Context, pattern identity.Pattern) ([]identity.Identity, error)
}
```

- `Contains` answers the hot-path question "is this concrete object a member?"
  and must never enumerate.
- `Members` performs a bounded enumeration (bounded by the `Deps.MaxMembers` the
  factory was handed, or `scope.DefaultMaxMembers` when that is zero)
  for `Enumerate`-style callers, and must agree with `Contains` — anything
  `Contains` accepts belongs in the member set, or the decision endpoints
  contradict each other. If it needs to list "all objects of a type" (including
  to filter a non-invertible predicate list-then-filter), it consults the
  injected `scope.ObjectLister`; when none is configured, return
  `APERTURE_SCOPE_LISTER_UNCONFIGURED` rather than an empty set.

Provide a `scope.Factory` that validates the parsed `scope.Spec` for your
strategy and captures the `GrantContext` + `Deps`:

```go
func newWidgetResolver(gc scope.GrantContext, deps scope.Deps) (scope.ScopeResolver, error) { … }
```

Register it under a key on a `scope.Registry` (`Register`/`MustRegister`). Reuse
the built-ins with `scope.DefaultRegistry()` and add yours, or start from
`scope.NewRegistry()`. A resolver never computes specificity — that stays the
pattern's job in the engine. Cover `Contains` and `Members` with a table test
alongside `scope/resolvers_test.go`; an unregistered key surfaces
`APERTURE_SCOPE_UNKNOWN_STRATEGY`, a bad spec `APERTURE_SCOPE_INVALID`.

---

## Adding an object provider

**Concept:** [Providers](../concepts/providers.md). **Files:** a new package (see
`csvprovider/` for a file-backed reference impl and `sqlprovider/` for a
database-backed one), then register it on a `provider.Registry`.

A provider is the host's pull source for one object-type. Implement
`provider.ObjectProvider`:

```go
type ObjectProvider interface {
    Fetch(ctx context.Context, id identity.Identity) (Metadata, error)
    List(ctx context.Context) ([]Object, error)
    Query(ctx context.Context, filter Filter) ([]Object, error)
}
```

- `Fetch` returns an object's `Metadata` (a `map[string]any`); a missing object
  must return an `APERTURE_NOT_FOUND` coded error so the Registry can distinguish
  "absent" from a fault. A plain error is wrapped as `APERTURE_PROVIDER_FETCH`.
- Return a **fresh map per object** — cached `Metadata` is treated as read-only
  and is never copied on read, so a shared map would race readers.
- `List` is the unfiltered enumeration; `Query` honours a `provider.Filter`
  (`Pattern`, `Fields`, `Limit`). Aperture re-enforces `Pattern` and `Limit` on
  the results, so a provider that ignores them is still correct, only slower.

Register it under its object-type key on a `provider.Registry`
(`provider.NewRegistry(...)`), which pairs each provider with a per-type metadata
cache. A `*provider.Registry` also satisfies `scope.ObjectLister`, so it wires
directly into the scope resolvers above. Mirror `csvprovider/csvprovider_test.go`
for coverage.

Two rules a new provider inherits rather than invents. `Query` must evaluate
`Filter.Fields` exactly as `provider.MatchFields` does — call the helper unless
you have a reason not to, because a provider that filters differently authorizes
differently. And every value it produces must satisfy the
[metadata value model](../concepts/providers.md#the-metadata-value-model);
validate at load with `provider.ValidateMetadata` so a bad shape fails where the
data enters instead of on the `Check` hot path. `sqlprovider` shows what that
costs when the source is untyped: its whole driver-value mapping exists to turn
whatever `database/sql` scanned into a value the model admits, and the cases it
cannot decide are pushed back onto the developer's `SELECT` list as casts.

---

## Adding an auth method

**Concept:** [Authentication](../concepts/auth.md). **Files:** `auth/` (a new
adapter), then select it in the server wiring.

Authentication is always external — Aperture consumes credentials, it never
issues them. Implement `auth.Authenticator`:

```go
type Authenticator interface {
    Authenticate(ctx context.Context, bearer string) (principalID string, claims Claims, err error)
}
```

- Fail closed: a missing, malformed, or unverifiable credential returns
  `APERTURE_UNAUTHENTICATED` (no principal derivable) or `APERTURE_INVALID_TOKEN`
  (credential failed verification) — never a silently-empty principal.
- Resolve the principal through the shared claim→principal mapping so "which
  claim is the principal id" stays configuration, matching the `oidc` and
  `parsec` adapters (the `dev` adapter is the one exception — the bearer *is* the
  principal). Follow `auth/oidc.go` / `auth/parsec.go` as the models and add a
  `*_test.go` beside them.

The middleware in `internal/server` extracts the bearer, calls `Authenticate`,
and attaches the resolved `auth.Principal` to the request context via
`auth.WithPrincipal` (recovered downstream with `auth.PrincipalFromContext`).

---

## Adding an MCP tool

**Concept:** [MCP surface](../surfaces/mcp.md). **Files:** `mcp/toolmeta/meta.go`,
`mcp/contract.go`, `mcp/handlers.go`, `mcp/tools.go`, `mcp/schema.go`.

The MCP core is **SDK-free**: it imports no MCP SDK. Adding a tool touches the
pure-data identity table and the typed contract, and the go-sdk adapter picks it
up automatically.

1. **Identity** — add a name constant and a description constant in
   `mcp/toolmeta/meta.go`, and a `{Name, Description}` row to `Meta()`. This is
   the single source of truth both the core and the `mcp/gosdk` adapter read, so
   they never drift.
2. **Contract** — define the typed `In`/`Out` structs in `mcp/contract.go`
   (alias the facade's surface-neutral query types where possible). Keep the
   types non-cyclic; a field that would introduce a Go-level cycle must be typed
   `any` so the JSON-Schema reflector stays error-free.
3. **Handler** — add a `func(context.Context, *service.Service, In) (Out, error)`
   in `mcp/handlers.go`. **Read-only only:** every tool calls a facade READ or
   DECISION method (`Check`/`Enumerate`/`Explain`/`Simulate`/`Get*`/`List*`); no
   handler may mutate.
4. **Wire it** — add `toolmeta.ToolYours: makeInvoke(handleYours)` to the map in
   `mcp/tools.go`, and a `register(...)` call in `mcp/schema.go`'s `init` so its
   input/output schemas are reflected.

**Gates** (`mcp/surface_test.go`, `mcp/firewall_test.go`):

- `TestCatalogMatchesToolmeta` — the catalog matches `toolmeta`.
- `TestNoMutatingTool` — no tool name carries a mutating verb
  (put/delete/create/update/bestow/revoke/grant/set/remove/write).
- `TestSchemaReflectionClean` — every tool's schema reflects without error.
- `TestMCPCore_NoSDKImport` — the core imports no MCP SDK (only `mcp/gosdk` may).

---

## Adding a rule AST node

**Concept:** [Rules engine](../concepts/rules.md). **Files:** `rules/ast.go`,
`rules/compiler.go`.

The rule AST is a small, closed node set that is *both* the engine's input and
the node editor's serialization target — there is no second rule format, and its
JSON form must round-trip byte-identically. To add a node type:

1. **Declare** a `NodeType` constant in `rules/ast.go` (alongside `NodeAnd`,
   `NodeCompare`, `NodeVar`, …). Add any fields it needs to the `Node` struct.
2. **Validate** — add a `case` to `Node.Validate()` that checks the subtree is
   structurally well-formed. Validation runs *before* compilation and is what
   keeps the rendered expression injection-free, so be strict here.
3. **Render** — add a `case` to `Node.render()` (reached via `Node.Expr()`) that
   emits the node's `expr-lang/expr` spelling. Rendering to an existing operator
   spelling means ASTs that render to the same expression share a compiled
   program in the cache.

The compiler (`rules/compiler.go`) validates, renders, and compiles once per
canonical hash. Keep evaluation pure: expose no wall-clock or random builtins.
If your node calls a function, it must be one of the curated pure functions or
one a host explicitly registers via the `rules.Function(name, fn)` compiler
option. Add round-trip and render tests beside `rules/ast.go`.

---

## Adding an RPC

**Concept / reference:** [RPC / HTTP overview](../surfaces/rpc-overview.md),
[RPC reference](../surfaces/rpc-reference.md). **Files:**
`internal/wire/rpc/service.proto`, the committed generated `*.pb.go` /
`*.twirp.go`, `internal/server/twirp.go`, and the hand-authored RPC reference doc.

1. **Declare** the method (and any new request/response messages) on
   `ApertureService` in `internal/wire/rpc/service.proto`.
2. **Regenerate** the Twirp + protobuf code with `make proto` (requires `protoc`
   + `protoc-gen-go` + `protoc-gen-twirp`). The generated `service.pb.go` /
   `service.twirp.go` are **committed** — CI does not regenerate them — so commit
   the regenerated files with your change.
3. **Implement** the method on `twirpHandler` in `internal/server/twirp.go`. The
   handler is a thin translator: decode the request, resolve the actor/principal
   from context, call the `service.Service` facade, and encode the result. Return
   `APERTURE_*` coded errors verbatim; never put policy logic here.
4. **Document it** — the RPC reference is **hand-authored** over `service.proto`
   (there is no generator; drift is accepted). Add the new method to
   `docs/src/surfaces/rpc-reference.md`.
5. **Cover it** — add a smoke test beside the matching
   `internal/server/*_smoke_test.go`.
