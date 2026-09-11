# CLAUDE.md — Aperture

Conventions catalog for `github.com/frankbardon/aperture`. **This file is the
authority.** It supersedes the per-effort planning artifacts under `.planning/`,
which are story scratch and must never be cited as conventions — several of them
predate decisions this file records (the Pulse dependency, SQLite-only storage).
Where this file is silent, `skills/*.md` and `docs/src/` govern.

## Project overview

Aperture is a fine-grained access-control engine for the frankbardon/* family.
It is **library-first**: the public Go packages at the module root are the
product, and every surface (CLI, Twirp/HTTP, MCP) is a thin translator over a
single decision engine. Aperture mirrors **Orbit** structurally and **Lattice**
for the MCP boundary, but keeps its public packages at the module root like
**Pulse** rather than under `internal/`.

The decision API is `Check` / `Enumerate` / `Explain`, each available single and
bulk-batched.

## Stack & build

- **Go 1.26.1**, `CGO_ENABLED=0`, pure-Go end to end. Module
  `github.com/frankbardon/aperture`.
- **CLI** is `urfave/cli/v3`; `cmd/aperture/main.go` only assembles the command
  tree (no business logic).
- **Rules** use the `github.com/expr-lang/expr` evaluator **directly** (pure-Go,
  the same engine Pulse wraps) — Aperture has no dependency on Pulse. The rules
  package renders its AST to an expr-lang expression and compiles it in-process.
- **Storage**: hand-written SQL, `modernc.org/sqlite` (pure-Go) +
  `storage/postgres` (pgx, a full peer — not a variant) + an in-memory impl
  behind one `Storage` interface. No ORM / sqlc / migration tool / schema
  versioning: `Setup` **creates**, never migrates, and a schema change is a hard
  break. See `skills/storage-schema.md`.
- **SQL providers** (`sqlprovider/`, a *host* data source — not Aperture's own
  storage) depend on a two-method `Querier` and link **no** driver. Two packages
  do, each with a blank import of `github.com/jackc/pgx/v5/stdlib` used through
  `database/sql`, Postgres only: `seed/connection.go` (host-provider
  connections) and `storage/postgres/postgres.go` (Aperture's own backend). The
  two share no connection handling and there is **no import edge between `seed/`
  and `storage/`** in either direction — do not create one. pgx over `lib/pq` on
  a correctness argument —
  `lib/pq` returns `[]byte` for `numeric` and `uuid`, which the value model
  cannot tell from `jsonb` — at a measured cost of +3,589,088 bytes for the
  driver alone (+4,246,608, +14.8%, for the epic as landed). Both pure Go, so
  `CGO_ENABLED=0` holds.
- **RPC/HTTP**: `net/http` ServeMux + Twirp (`internal/server/`, proto at
  `internal/wire/rpc/service.proto`), with an admin UI shell served from
  `internal/server/static/`.
- **MCP**: SDK-free core (`mcp/`, surfaced as `aperture mcp` over stdio); one
  adapter package may import the protocol SDK, enforced by a firewall test.

```bash
make build   # produce bin/aperture (-ldflags="-s -w" -trimpath, CGO off)
make test    # go test ./...
make fmt     # go fmt ./...
make vet     # go vet ./...
make lint    # vet + staticcheck (degrades to vet-only when not installed)
```

## Coding conventions

### Errors

- Every failure is an `APERTURE_*` coded error from the root `errors/` package.
- Codes are **SCREAMING_SNAKE**, namespaced `APERTURE_*`, defined in
  `errors/codes.go` and listed in `AllCodes`.
- Each code has a `Registry` entry with a `Message` and either at least one
  `Fixup` or `FixupNotApplicable=true`. Gated by `TestCodesHaveFixups`.
- Construct via `errors.New` / `Newf` / `Wrap` / `Wrapf`; recover the code with
  `errors.CodeOf`.
- **`Wrap`/`Wrapf` DO re-stamp.** They are not pass-through. `errors/coded_error.go`
  builds a fresh `CodedError` with whatever code it is handed, and `CodeOf` uses
  `errors.As`, which reports the **outermost** code — so wrapping an already-coded
  error in a different code observably replaces the code a caller reads.
- Pass-through is a **call-site idiom**, and you must write it yourself whenever
  the error you are wrapping might already be coded:

  ```go
  if aerr.CodeOf(err) != "" { return err }        // it already says something better
  return aerr.Wrap(aerr.APERTURE_X, "...", err)   // only classify what nothing else did
  ```

  Live examples: `internal/cli/buildStore`'s `bootError`, `delegation.authorize`,
  `provider/registry.go`, `storage/sqlite/sqlite.go`.
- Forgetting the guard is not cosmetic: it buries a specific, actionable code
  (`APERTURE_STORAGE_CONSTRAINT`, `APERTURE_STORAGE_SCHEMA_INCOMPATIBLE`,
  `APERTURE_CONFIG_INVALID`) and its registry fixups under a generic one, and the
  operator loses the remedy. Two surfaces were doing exactly that; both are fixed,
  and a same-code re-stamp is invisible to `CodeOf`, so tests assert **chain
  depth** — exactly one Aperture-coded error in the chain — not just the code.

### Library-first

- Business logic lives in the public root packages. `cmd/aperture/` is a thin
  adapter; later stories add manual DI in the `serve` command (no wire/fx/dig).
- Config is env vars (`APERTURE_*`) + optional YAML; `.env` via dotenv.

### Naming

- No predecessor references (no `Aperture2` / `LegacyX`).

## Update Demand

Any change to a registered surface MUST update the matching `skills/` document in
the **same PR**. A surface change that lands without its doc update is a
non-skippable CI failure. The rule itself is documented in
`skills/update-demand.md` and is self-protecting.

| If you change... | You MUST also update... | Enforced by |
|---|---|---|
| An Aperture error code | `errors/codes.go` (`AllCodes` + `Registry` entry with Message + Fixups) | `TestCodesHaveFixups` |
| A `skills/*.md` doc | its YAML frontmatter (`name` matching the file stem + `description`) | `TestEverySkillHasFrontmatter` |
| The Update-Demand rule | `skills/update-demand.md` (must remain present with frontmatter) | `TestUpdateDemandDocPresent` |
| A rule operator (`Op*` / `opSpecs` in `rules/ast.go`) | `OP_SPECS` in `internal/server/static/js/rules-serializer.js`, the palette in `rules.js`, and `skills/rules-engine.md` | `TestEditorOperatorTablesAgree`, `TestEditorASTContractCoversEveryOperator` |
| A right-operand shape (`rightShape` in `rules/ast.go`) | `RIGHT` in `rules-serializer.js` **and** `jsShapeNames` + `editorJSONForOp` in `rules/editor_js_contract_test.go` | `TestEditorOperatorTablesAgree`, `TestEditorASTContractCoversEveryOperator` |
| A collection operator's shape expectation (`collOps` in `rules/shape.go`) | the matching `opSpecs` entry in `rules/ast.go` | `TestCollectionOperatorTablesAgree` |
| A date operator's runtime policy (`dateOps` in `rules/date.go`) | the matching `opSpecs` entry in `rules/ast.go`, and the deny-safe policy in `skills/rules-engine.md` | `TestDateOperatorTablesAgree` |
| The date-operator SET (an `opSpecs` entry gaining or losing `kind: renderDate`) | `DATE_OPS` in `rules-serializer.js` (it cannot live on an `OP_SPECS` entry — the Go scanner requires the literal `{ right: RIGHT.X }` form) and the serializer section of `skills/ui-shell.md` | `TestEditorOperatorTablesAgree` |
| What `rules.Clock` drives (`rules/engine.go` `WithClock`, `rules/now.go`) | the `WithClock` doc comment and "The clock, and one `NOW` per decision" in `skills/rules-engine.md` | reviewed; no registry gate |
| The `principal` floor bag (`rules/engine.go` `principalBag`: which keys it stamps, that it stamps them LAST, that the map is fresh) or which attribute slot a principal kind resolves (`provider.principalSlot`) | the `principal` root in `skills/rules-engine.md` ("Context variables"), "The floor bags, and why the floor wins" in `skills/attribute-providers.md`, and `docs/src/concepts/rules.md` ("The floor bag, and `principal.kind`" + the roots table) | reviewed; no registry gate — behaviour by `rules/principal_floor_test.go` (`TestTheFloorIsNotShadowedByTheProviderBag`) and `engine/principal_attributes_test.go`. **The floor winning on collision is the load-bearing half**: if a host bag could shadow `id`, `principal.id == object.owner` would silently compare something else, with no error anywhere |
| The `account` floor bag (`rules/engine.go` `accountBag`), the `AccountResolver` seam, or the account-wildcard SHORT-CIRCUIT (`Engine.accountAttributes`: `"*"` never reaches a resolver and `account` reads floor-only) | the `account` root in `skills/rules-engine.md` ("Context variables"), "The `account` root, and the wildcard" in `skills/attribute-providers.md`, and `docs/src/concepts/rules.md` ("The `account` floor, and the wildcard") | reviewed; no registry gate — behaviour by `rules/account_floor_test.go`, `engine/account_attributes_test.go` and `engine/account_boundary_test.go`. **Making the wildcard an error instead of a floor breaks platform scope**: `service/reads.go` really does run `engine.Check` with `Request.Account == model.AccountWildcard`, so every rule-backed grant would become undecidable there — including the ones that never mention `account` |
| When `attributes_floor_only` FIRES (`rules/engine.go` `recordFloorOnly` / `readsBeyondFloor` — that it gates on the paths the rule NAMES, not on what a comparison did) | "`attributes_floor_only`" in `skills/attribute-providers.md` and the note-kind list in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `rules/floor_only_test.go`. **Both directions are failures**: unconditional emission puts two notes on every trace of every rule-backed grant in the many deployments that wire no provider and buries the shape/date notes, while narrowing it past the declared reads hides the one hazard the note exists to expose |
| What the per-decision attribute memo spans (`rules/attributes.go` `WithDecisionAttributes` / `DecisionAttributes`, or a decision boundary in `engine/` that opens — or stops opening — the scope) | the type's doc comment and "One bag per decision" in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `rules/attributes_test.go` and `engine/attribute_memo_test.go`, which count resolutions with a fake directory. **A boundary that stops opening the scope fails by passing**: the decision is still correct, only per-object and inconsistent |
| A `rules.NoteKind` (`rules/notes.go`) | its `Note.String()` case and the note-kind list in `skills/rules-engine.md` | reviewed; no registry gate |
| The rule-evaluation context — `scope.GrantContext`'s fields, `scope.RuleEvaluator.Selected`, or `rules.PrincipalResolver.Attributes` | all three move together (they are one signature in three packages), plus `docs/src/concepts/scopes.md` ("The resolver contract" and the `RuleEvaluator` seam row, including its worked `GrantContext` literal), `docs/src/concepts/rules.md` (`Engine.Selected`'s step list), and "Context variables" in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `engine/grant_context_test.go` (every entry point supplies the account and kind; the kind costs no second `GetPrincipal`), `scope/resolvers_test.go` (`TestRuleContextReachesTheEvaluator`, both rule-backed strategies), `rules/engine_test.go`. **A value carried but not handed on compiles and is silently wrong** — a rule would read attributes for the wrong account — which is why the assertions are per-seam rather than end-to-end only |
| A callable rule function (`defaultFunctions` in `rules/compiler.go`) or a blocked builtin (`blockedCallNames`) | `FUNCTIONS` / `BLOCKED_CALLS` in `rules-serializer.js` | `TestEditorVocabularyTablesAgree` |
| An AST node type, variable root, or var-path grammar | `TYPES` / `ROOTS` / `VAR_PATH` in `rules-serializer.js` | `TestEditorVocabularyTablesAgree` |
| A relative-date vocabulary (`relativeAnchors` / `relativeUnits` / `relativeSnaps` in `rules/relative.go`) | `ANCHORS` / `UNITS` / `SNAPS` in `rules-serializer.js`, the four controls in `rules.js`, and "Relative dates" in `skills/rules-engine.md` | `TestEditorVocabularyTablesAgree` |
| The relative-date node's JSON shape or field validation (`Node.Anchor/Offset/Unit/Snap`, `validateRelativeDate`) | the node's validator + `NODE_SPECS` entry in `rules-serializer.js` and "Relative dates" in `skills/rules-engine.md` | `TestEditorValidationMessagesAgree`, `TestEditorASTContract` |
| The relative-date resolution semantics (`rules/calendar.go`: month-end clamping, the snap-then-offset order, `endOf*` precision, ISO-Monday weeks, the representable range) | "UTC, clamping, and the order of operations" in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `rules/calendar_test.go`, including a source scan that forbids `AddDate` / `time.Local` in `rules/` |
| The metadata value model (`provider/metadata.go`: legal shapes, depth cap, size cap) | `skills/metadata-values.md` and `docs/src/concepts/providers.md` | `TestEverySkillHasFrontmatter` (doc presence); model behaviour by `provider/metadata_test.go` |
| The date value model (`provider/date.go`: the canonical forms, the accept/reject set, `DateReason`) | `skills/metadata-values.md` ("Dates") and `docs/src/concepts/providers.md` | `TestEverySkillHasFrontmatter` (doc presence); model behaviour by `provider/date_test.go` |
| A loader's spelling of the value model (a CSV column suffix — `:int`, `:list<T>`, `:json`, `:date`, `:datetime` — or a seed key such as `objects:` / `field_types:` / `attributes:`) | `skills/metadata-values.md` ("How each loader spells the model"), the loader's package doc, `docs/src/concepts/providers.md`, and `docs/src/concepts/seed.md` for a seed key | reviewed; no registry gate |
| The seed `attributes:` schema (`seed.Attribute`'s keys, the `subject:` vocabulary, the per-slot dedup rule) or what `Document.BuildAttributeRegistry` / `Document.HasAttributeSources` cover | the field's doc comment, `skills/metadata-values.md` ("The seed document's `attributes:` section"), `skills/attribute-providers.md` ("From a seed document"), `docs/src/concepts/seed.md` ("Inline subject attributes"), and `docs/src/concepts/rules.md` ("Wiring a `*provider.AttributeRegistry`") — a NEW attribute source section must be OR'd into `HasAttributeSources`, which is why that gate lives beside the fields it counts rather than in `internal/cli` | reviewed; no registry gate — behaviour by `seed/attribute_test.go` (incl. `TestAttributeWiringIsNotModelState`) and `internal/cli/attributes_test.go` |
| The attribute SEAM — the `AttributeSlot` set (`provider.AttributeSlots()` / `ParseAttributeSlot`), the `AttributeProvider` / `AttributeFilter` / `AttributeRecord` contract, or `AttributeRegistry`'s registration and LENIENCY behaviour (which codes `Attributes` / `AccountAttributes` collapse to a nil bag) | `skills/attribute-providers.md` ("The three slots", "The leniency contract", "Containment"), `docs/src/concepts/providers.md` ("Attribute providers", which restates the slot set, the leniency contract and the containment boundary), the `APERTURE_ATTRIBUTE_*` fixups in `errors/codes.go`, and — for a slot's spelling — `seed/attribute.go`'s `slotNames` and `internal/cli`'s `slotList`, both of which DERIVE the set rather than restate it | `provider.TestAttributeRegistryIsNotAScopeLister` + `TestProviderPackageImportsOnlyIdentityAndErrors` gate the containment half; the leniency half is reviewed, with behaviour by `provider/attribute_leniency_test.go`, `provider/attribute_wildcard_test.go` and `rules/attribute_leniency_test.go` (incl. `TestAMissingBagWidensAnExclusiveGrant`). **A fourth slot is a contract change, not a feature** — the closed set is what stops a host registering a party the engine cannot fetch, which surfaces as an empty bag, which is a silent denial. **Widening leniency by one more code is access-widening in an exclusive grant**, and nothing in a verdict says so |
| Attribute cache INVALIDATION (`provider.AttributeRegistry.Invalidate` / `InvalidateSlot` / `InvalidateAll`, or the three `service.Invalidate*Attribute*` facade methods and their gate) | `skills/attribute-providers.md` ("Staleness is a security window, not a tuning knob" **and** "The administrative read"), `docs/src/cli/attributes.md` ("`invalidate`"), `docs/src/library/service-facade.md` ("The attribute directory"), the methods' doc comments, and the `aperture attributes invalidate` description — **plus `make docs-gen`** when the CLI text moves | reviewed; no registry gate — behaviour by `provider/attribute_invalidate_test.go`, `service/attributes_invalidate_test.go` and `internal/cli/attributes_cli_test.go`. **A slot's `ttl:` is the window a REVOKED clearance keeps authorizing for**, so these are a security control and not a performance knob; documenting them as tuning is the actual regression |
| The seed `attribute_providers:` schema (a `seed.AttributeProvider` YAML key, the `kind:` set in `attributeKinds()`, or the per-slot `ttl:` / `max_size:` handling) or the precedence reporters (`Document.AttributeSlotSources` / `AttributeSourceInline` / `AttributeCollisions`) | the field's doc comment, `skills/attribute-providers.md` ("From a seed document", "Precedence: the external source wins, entirely"), `docs/src/concepts/seed.md` ("External attribute sources", including "Precedence: the external source wins, entirely"), and the `attributes slots` output in `internal/cli/attributes.go` — the CLI reads the precedence rule from `AttributeSlotSources` and must never re-derive it | reviewed; no registry gate — behaviour by `seed/attribute_provider_test.go`, `seed/attribute_provider_csv_test.go`, `seed/attribute_provider_sql_test.go`. **The external entry wins and the inline bags for that slot are discarded ENTIRELY** — no per-subject merge, no fallback — and the discard is reported, never silent |
| The attribute LOADERS' bare-id contract (`csvprovider.Attributes`' id column, or `sqlprovider.AttributeConfig`'s `FetchQuery` binding the key verbatim / `ListQuery` selecting a bare id / `ListQuery` being optional) | ALL THREE statements of it move together — the `csvprovider/attributes.go` file doc, the `sqlprovider/attributes.go` file doc, and `seed.AttributeProvider.GetAll` — plus "The `get_all` bare-id contract" in `skills/attribute-providers.md`, "The attribute seam" in `skills/sql-provider.md`, "The attribute variant" under each loader in `skills/metadata-values.md`, "The bare-id contract" in `docs/src/concepts/seed.md`, and `docs/src/concepts/providers.md` ("Attribute providers") | reviewed; no registry gate, and **there cannot be one**: an identity-shaped key (`'user:' \|\| u.id AS id`) is a legal opaque string that enumerates, caches, and then matches no id any `Fetch` presents. The slot silently never answers. That is precisely why it is written down in three places instead of tested in none |
| The SQL driver-value mapping table (`metadataValue`'s type switch **or** `mappedDriverTypes` in `sqlprovider/values.go`) | the other half of the pair in the same file, the `sqlprovider` package doc ("Driver values become metadata"), `skills/sql-provider.md`, `skills/metadata-values.md` ("`sqlprovider`"), and `docs/src/concepts/providers.md` | `TestDriverValueMappingTableMatchesTheTypeSwitch` — parses `sqlprovider/values.go` with `go/ast` and diffs the type switch against `mappedDriverTypes`; adding a case to one and not the other is build-red |
| The seed `connections:` / `kind: sql` schema (a `Connection` or `Provider` YAML key in `seed/`) or one of its defaults (`DefaultQueryTimeout` / `DefaultMaxOpenConns` / `DefaultMaxIdleConns` / `DefaultConnMaxLifetime` in `seed/connection.go`) | the field's doc comment, the defaults table in `skills/sql-provider.md`, and "Database-backed providers" in `docs/src/concepts/seed.md` | reviewed; no registry gate — behaviour by `seed/connection_test.go` (defaults, pool sharing, the `dsn:` refusal, `BuildRegistry`'s refusal of `connections:`) |
| The SQL provider's statement contract (the four casting rules, what `Fetch` binds, the id column, `Querier`, `sqlprovider.Config`) | the `sqlprovider` package doc, `skills/sql-provider.md`, `docs/src/concepts/providers.md` ("Worked example: `sqlprovider`"), and the `APERTURE_SQL_PROVIDER_*` fixups in `errors/codes.go` | `TestEverySkillHasFrontmatter` (doc presence); behaviour by `sqlprovider/*_test.go` — the casting rules themselves are un-gatable, which is exactly why they must be written down |
| How the rule editor displays a date (anything in `internal/server/static/js/rules.js` or `rules-serializer.js` that renders a stored date, a resolved bound, or the reference instant) | "Reading a saved rule back" + "The date diagnostics" in `skills/ui-shell.md` | `TestRuleEditorNeverFormatsADateThroughADateObject` — scans the served JS and fails on `new Date` / `toLocale*String` / `Intl.DateTimeFormat` and friends |
| The rule what-if preview's response fields (`EvaluateRuleResponse` in `service.proto`, `service.RulePreview`) | `skills/ui-shell.md` ("The date diagnostics"), `skills/api-surface.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/library/service-facade.md` — **and `make proto`** | reviewed; no registry gate |
| The enumerate filter input (`engine.EnumerateRequest.Fields` / `engine.MetadataFetcher`) | ALL of: `service.EnumerateQuery` + its `request()` converter (keep the `json:"...,omitempty"` + `jsonschema` tags — `mcp.EnumerateIn` aliases the struct and drops the REQUIRED marking through `omitempty`); `EnumerateRequest.fields` in `service.proto` **and `make proto`**; `internal/wire/rpc/fields.go` (`FieldsFromWire`/`FieldsToWire`); `internal/server/twirp.go` (`enumerateQuery`, single **and** batch); `internal/cli/fields.go` + the `enumerate` command description; then `skills/api-surface.md`, `skills/decision-api.md`, `mcp/skills/mcp-surface.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/surfaces/mcp.md`, `docs/src/library/decision-api.md`, `docs/src/library/service-facade.md`, `docs/src/cli/decisions.md` — **and `make docs-gen`** (the CLI flags/description are a generated page) | reviewed; no registry gate — behaviour by `engine/enumerate_fields_test.go`, `service/enumerate_fields_test.go`, `internal/wire/rpc/fields_test.go` (incl. `TestFieldsRoundTrip_LargeIntegerLosesPrecision`), `internal/server/enumerate_fields_test.go`, `internal/cli/{fields,enumerate_filter}_test.go`, `mcp/enumerate_fields_test.go`; `TestEnumerateHelpStatesThePrecedence` pins the help text |
| The `Filter.Fields` matching semantics (`provider/match.go` `MatchFields` / `ValuesEqual`) | `docs/src/concepts/providers.md` ("The `Filter.Fields` contract") **and** every restatement of it on the enumerate filter: `skills/api-surface.md`, `skills/decision-api.md`, `docs/src/library/decision-api.md`, `docs/src/library/service-facade.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/surfaces/mcp.md`, `docs/src/cli/decisions.md` | reviewed; no registry gate — one definition, one implementation: a provider that filters differently authorizes differently |
| The `references:` schema (`seed.Provider.References`, `Registry.DeclareReference` / `ReferenceTarget` / `ResolveReference*` in `provider/reference.go`, or what a declaration may carry) | the field's doc comment, `skills/object-references.md` ("Declaring a reference"), `docs/src/concepts/seed.md` ("Declaring a reference"), and `docs/src/concepts/providers.md` ("Declared references") — a **new descriptor kind** (e.g. a `type:` key) needs its "three closed doors" rationale rewritten, not appended to | reviewed; no registry gate — behaviour by `provider/reference_test.go`, `seed/reference_test.go` (incl. `TestReferenceWiringIsNotModelState`) |
| The via-reference enumerate input (`engine.EnumerateRequest.References` / `engine.ReferenceEdge` / `engine.ReferenceSource` / `WithReferences`) | ALL of: `service.EnumerateQuery.References` + `service.ReferenceEdge` + the `references()` converter (keep the `json:"...,omitempty"` + `jsonschema` tags — `mcp.EnumerateIn` aliases the struct and `omitempty` is what keeps the edges OPTIONAL); `EnumerateRequest.references` + `message ReferenceEdge` in `service.proto` **and `make proto`**; `internal/server/twirp.go` (`enumerateQuery` + `referenceEdges`, single **and** batch); `internal/cli/references.go` + the `enumerate` command description; then `skills/object-references.md`, `skills/api-surface.md`, `skills/decision-api.md`, `mcp/skills/mcp-surface.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/surfaces/mcp.md`, `docs/src/library/decision-api.md`, `docs/src/library/service-facade.md`, `docs/src/cli/decisions.md` — **and `make docs-gen`** (the CLI flags/description are a generated page) | reviewed; no registry gate — behaviour by `engine/enumerate_reference_test.go` (incl. `TestTheFourQuestions`), `service/`, `internal/server/`, `internal/cli/`, `mcp/enumerate_references_test.go`. **The empty-vs-`NOT_FOUND` split is asserted PER SURFACE on purpose:** relaxing it in one place is a silent disclosure channel, so a change there must move all five test files together |
| The configured enumeration bound (`engine.WithEnumerateLimit` / `DefaultEnumerateLimit`, the unconditional `Deps.MaxMembers` stamp in `engine.stampScopeBound`, `scope.Deps.MaxMembers` / `provider.DefaultListLimit` no longer clamping DOWN) or the on-bound warning (`engine.warnAtEnumerateBound`) | `skills/decision-api.md` ("Enumerate is bounded" and its four subsections — the one full account), `docs/src/library/decision-api.md` (the `WithEnumerateLimit` and `WithLogger` option rows **and** "A result on the bound is a warning"), the `Limit` clause in `skills/api-surface.md`'s decisions bullet, `docs/src/concepts/scopes.md` + `docs/src/internals/extending.md` (both say `Members` gathers against `Deps.MaxMembers`, not the constant), `docs/src/concepts/providers.md` + `skills/attribute-providers.md` (both say a positive `List` limit is honoured verbatim and `AttributeRegistry.Enumerate` is uncapped), `docs/src/operations/performance.md`, `docs/src/operations/deployment.md`, `docs/src/cli/serve.md`, `docs/src/cli/global-options.md`, `docs/src/cli/decisions.md` — **and `make docs-gen`** when the flag or a command description moves | reviewed; no registry gate — behaviour by `engine/enumerate_limit_test.go`, `engine/enumerate_bound_e2e_test.go`, `engine/enumerate_bound_warning_test.go`, `scope/max_members_test.go`, `internal/cli/enumerate_limit_test.go`, `bench/enumerate_test.go`. **The library NORMALISES a non-positive bound to the default and the CLI REFUSES one**, deliberately — and that divergence is written in three places (`skills/decision-api.md`, `docs/src/cli/serve.md`, the `n <= 0` comment in `internal/cli/enumerate_limit.go`) with **nothing binding them**: drop the normalisation and all three go silently wrong while every test still passes. **The warning is a hint, not an assertion** — a complete set of exactly that size is indistinguishable, `Enumerate` still returns `([]string, error)`, and restating it as proof of truncation is itself the regression |
| The persisted timestamp encoding (`storage/storagetime`: the int64-nanosecond unit, the `0` unset sentinel, the representable window, `Encode`/`Decode`/`Validate`) | the "Time" header section in **both** `storage/sqlite/schema.sql` and `storage/postgres/schema.sql`, `skills/storage-schema.md`, and `docs/src/concepts/storage.md` | `TestStorageTimeIsTheOnlyTimeIntegerConversion` (no `Unix*` conversion under `storage/` outside `storagetime`) + `storage/storagetest` per backend; the prose itself is reviewed |
| A stamped model entity (a new `CreatedAt`/`UpdatedAt` pair on a `model.Storage` entity) | `stampedEntities()` in `storage/storagetest/storagetest.go` — it is the suite's definition of "every stamped entity", so an entity missing from it has its timestamp cases **silently skipped** — plus the entity list in `skills/storage-schema.md` | reviewed; no registry gate — this is the one that fails by passing |
| The foreign-key edge set or any edge's `ON DELETE` / `ON UPDATE` action | **both** `storage/sqlite/schema.sql` and `storage/postgres/schema.sql` (including the comment stating the reason), the edge table in `skills/storage-schema.md`, `docs/src/concepts/storage.md`, and — if the edge cannot be expressed in SQL — `storage/sqlite/integrity.go`, `storage/postgres/integrity.go` and `storage/memory` **with the refusal wording verbatim**, since `storagetest` asserts the text | `TestDialectSchemasDeclareTheSameForeignKeys` (changing one dialect only is build-red), `storage/sqlite/foreign_keys_test.go` (actions read back via `PRAGMA foreign_key_list`), `storage/storagetest` per backend |
| `physicalTypes` / `refusedTypes` (`internal/schemagate/schema_parity_test.go`) | the affected `schema.sql` header's divergence section and "The two dialects" in `skills/storage-schema.md` — editing this table changes what "the dialects agree" **means**, so it is a contract change, not a refactor | `TestEveryDialectHasATypeMapping`, `TestTheTypeMappingRefusesTheNarrowingSpellings` |
| Which principal a rule is told about (`engine.effectivePrincipal`, `elevatedSubjects` in `engine/impersonation.go`) — the effective subject is the TARGET under `become` and the OPERATOR under `augment` | `skills/rules-engine.md` (the `principal` root), `docs/src/library/impersonation.md` ("Under `become`, `principal.*` is the target"), `docs/src/concepts/impersonation.md` ("Two modes"), and a release note — `principal.id` changing meaning is invisible to every compiler and every test a host owns | reviewed; no registry gate — behaviour by `engine/impersonation_attributes_test.go` and `TestImpersonationTellsTheRuleAboutTheEffectiveSubject`. The audit half (`Decision.Impersonation` / `Trace.Request.Principal` stay the operator) moves together with it or the change becomes a loss of accountability |
| The attribute-directory read (`service.ListAttributes` / `ExplainAttributeAuthority` / `WithAttributes` in `service/attributes.go`) — the tier it requires, the order the wiring/auth/authority checks run in, or what a refusal carries | `skills/api-surface.md` ("The attribute directory read" **and** the facade surface list), "The administrative read" in `skills/attribute-providers.md`, and `docs/src/library/service-facade.md` ("The attribute directory — a system-tier read", including the `WithAttributes` row in the options table) | reviewed; no registry gate — behaviour by `service/attributes_test.go`. **The check ORDER is the contract**: the gate runs before the slot is resolved, so a non-admin's refusal is identical for a populated, an unregistered, and a nonexistent slot. Moving the slot parse in front of it turns the refusal into a probe, and `TestARefusalDisclosesNothingAboutTheSlot` is what catches that |
| The `aperture attributes` command tree (`internal/cli/attributes.go`: a subcommand, a flag, or which of the three is gated) | `skills/attribute-providers.md` ("The operator surface") and `docs/src/cli/attributes.md` (the narrative page, plus its row in `docs/src/cli/overview.md` and its `SUMMARY.md` entry) — **and `make docs-gen`**, since `docs/src/reference/cli.md` is generated from the flags and descriptions | reviewed; no registry gate — behaviour by `internal/cli/attributes_cli_test.go` and `internal/cli/attributes_csv_test.go`. **`slots` is ungated on purpose and the other two are not**: `slots` restates the seed file the operator just passed in, while `query` and `invalidate` go through `service.requireAttributeAdmin`. Gating `slots` would mean nobody could diagnose "is the user slot even wired?" without already holding the authority the diagnosis explains |
| What a decision trace DISCLOSES about attributes (`engine.TraceAttributes`, `traceAttributes`, or `Trace.String()`'s attribute lines) | `skills/decision-api.md` (the `Attributes` field + the shape-and-path-only note rule it is the named exception to), "The administrative read" in `skills/attribute-providers.md`, `docs/src/library/decision-api.md` ("The attribute bags"), `docs/src/library/service-facade.md`, **and both MCP statements of it** — `docs/src/surfaces/mcp.md` and `mcp/skills/mcp-surface.md`, which disclose it through the `ExplainOut` / `SimulateOut` = `engine.Trace` ALIAS with no `mcp/` code of their own — plus `docs/src/surfaces/rpc-reference.md` if the wire shape moves | reviewed; no registry gate — behaviour by `engine/attribute_trace_test.go`. **This field carries VALUES on purpose** and is the one place a `Trace` does: the two bags are the subjects of the request being explained, so it discloses the asker's own decision and nothing else. Do not redact it into a `Note`, do not widen it past those two subjects, and keep `service.EvaluateRulePreview` supplying neither — a rule editor must not become a directory read oracle |
| Whether an attribute fetch key can carry an ACCOUNT (`provider.AttributeRegistry.Fetch`'s signature, or anything that would key a principal bag per account) | "Account neutrality: a principal bag is global" in `skills/attribute-providers.md` | reviewed; **no gate is possible** — a principal is global (`model.Principal` has no account; `model.Membership` binds it to several), so one principal bag is visible to rules in every account that principal belongs to, and the host obligation to keep principal attributes account-neutral is opaque host data Aperture cannot inspect. The bounded slot is `account`, whose containment IS proven (`engine/account_boundary_test.go`) |
| `codeToTwirp` (`internal/server/twirp.go`) — adding a code, or moving one between Twirp codes | the code→status table in `docs/src/surfaces/rpc-overview.md`, plus its "why not 500" prose when the mapping is not the default | reviewed; no registry gate — the absence of this row is why the table was already stale for `APERTURE_ENTITY_UNMANAGED` |

The Go↔JS rows matter more than they look: **CI is node-free**, so
`rules-serializer.test.js` never runs in the pipeline.
`rules/editor_js_contract_test.go` is what actually enforces parity — it reads
the JS file from disk and diffs the tables, and it **fails** rather than skips if
that file moves.

As the remaining surfaces land (identity, model, engine, scope, account, auth,
audit, mcp), each story adds a `skills/<feature>.md` doc and a coverage gate in
`skills/skills_test.go` that walks the surface's registry, then a row here.

## Non-skippable CI gates

- `TestCodesHaveFixups` — every `APERTURE_*` code has a Registry entry with a
  Message and a Fixup (or `FixupNotApplicable`).
- `TestRegistryHasNoOrphans` — `Registry` contains nothing absent from `AllCodes`.
- `TestCodesAreScreamingSnakeNamespaced` — every code is SCREAMING_SNAKE and
  `APERTURE_`-prefixed.
- `TestUpdateDemandDocPresent` — the Update-Demand seed doc exists with
  frontmatter.
- `TestEverySkillHasFrontmatter` — every `skills/*.md` has a `name` (matching its
  file stem) and a `description`.
- `TestEditorOperatorTablesAgree` / `TestEditorVocabularyTablesAgree` /
  `TestEditorUnaryOperatorsAgree` / `TestEditorValidationMessagesAgree` — the Go
  rule AST and the JS serializer expose the same operators, operand shapes, node
  types, roots, functions, blocked builtins, and validation wording. Reads
  `rules-serializer.js` from disk; fails (never skips) if it is missing.
- `TestEditorASTContractCoversEveryOperator` — every operator in `opSpecs` has a
  byte-stable AST JSON case, so coverage cannot fall behind the registry.
- `TestDriverValueMappingTableMatchesTheTypeSwitch` — the driver-value mapping in
  `sqlprovider/values.go` is one table in two places (`metadataValue`'s type
  switch and `mappedDriverTypes`); the test parses the file with `go/ast` and
  fails if they disagree.
- `TestCollectionOperatorTablesAgree` — `collOps` (`rules/shape.go`) stays in
  lockstep with `opSpecs` (`rules/ast.go`).
- `TestDateOperatorTablesAgree` — `dateOps` (`rules/date.go`) stays in lockstep
  with `opSpecs` (`rules/ast.go`), membership and ternary arity alike.
- `TestRuleEditorNeverFormatsADateThroughADateObject` — the rule editor's served
  JS (`rules.js`, `rules-serializer.js`) constructs no JS `Date` and calls no
  locale date formatter, so a stored UTC date is never restated in the viewer's
  zone. Comments are stripped first, so documenting the hazard is still allowed.
- `TestAttributeRegistryIsNotAScopeLister` (`provider`) — an
  `*AttributeRegistry` must NOT satisfy `scope.ObjectLister`, asserted against the
  real interface with `*Registry` as the positive control. Go's typing is
  structural, so the four differences that make the signature unassignable
  (`Enumerate` not `List`, an `AttributeSlot` not a type string, an
  `AttributeFilter` carrying no `identity.Pattern`, `[]AttributeRecord` not
  `[]identity.Identity`) are the guarantee that a principal directory can never
  become an enumerable object set inside a decision. Attribute enumeration is a
  system-tier admin read and never a scope-resolution source.
- `TestProviderPackageImportsOnlyIdentityAndErrors` (`provider`) — parses every
  non-test file in the package and fails on any import outside `identity`,
  `errors` and the standard library. It is why a principal KIND arrives as a
  function argument rather than being resolved inside the registry, and why the
  attribute-directory gate lives on the facade rather than next to the thing it
  protects.
- `TestSchemaUsesNoReservedIdentifiers` (`internal/schemagate`) — the `apt_`
  database-identifier convention, per dialect. Parses each `schema.sql` with a
  real SQL tokenizer (never greps) and **fails**, never skips, if a file moved.
- `TestEveryDialectSchemaIsGoverned` — globs `storage/*/schema.sql` and fails in
  **both** directions, so a new backend's schema cannot arrive ungoverned and a
  registered path that vanished is caught.
- `TestDialectSchemasDeclareTheSameTables` /
  `TestDialectSchemasDeclareTheSameColumns` /
  `TestDialectSchemasDeclareTheSameForeignKeys` — the two hand-written schema
  files describe the same database: same tables, same columns per table, same
  foreign-key edges including each edge's `ON DELETE` / `ON UPDATE`. Symmetric —
  there is no reference dialect the other must match.
- `TestEveryDialectHasATypeMapping` / `TestTheTypeMappingRefusesTheNarrowingSpellings`
  — the dialects' legitimate type divergences are an **explicit** mapping
  (`physicalTypes` / `refusedTypes`), not a blanket exemption: an unmapped
  spelling fails, and Postgres `INTEGER` / `BOOLEAN` / `JSONB` / `TIMESTAMP*` are
  refused by name with a reason.
- `TestStorageTimeIsTheOnlyTimeIntegerConversion` — a `go/ast` scan over every
  non-test file under `storage/`, banning `Unix*` conversions outside
  `storage/storagetime`.
- `TestConformanceSuiteHasNoPrecisionKnob` / `TestConformanceSuiteIsBackendBlind`
  — `storage/storagetest` carries no tolerance/truncation knob and no
  backend-conditional assertion.

Gated, NOT in `make test` (a loaded runner would flake them):

- `APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/` — cached `Check`
  p99 < 1ms and ≥ 10k checks/sec, including the collection and nested-access
  fixtures. Every case in the gate is NAMED `TestCheckNFR*` on purpose, because
  `-run` is an unanchored regexp and this is the only invocation anyone runs; a
  case reachable only by a second command will not be run. One of them,
  `TestCheckNFREnumerateBound`, asserts a **ratio** rather than a wall clock — a
  rule-backed `Enumerate` at a bound raised through `engine.WithEnumerateLimit`
  must cost no more than 1.5× the default bound's cost **per candidate** — so
  that it catches a super-linear term in the fan-out without flaking on a loaded
  runner. Do not convert it to an absolute ceiling: a bound-sized enumeration is
  milliseconds by design, and the justification for the number lives beside it in
  `bench/enumerate_test.go`.
- `node internal/server/static/js/rules-serializer.test.js` — CI is node-free, so
  this is a manual development aid; the Go contract tests above are the real gate.
- `APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresIntegration ./seed/`
  — the host-provider (`connections:` / `kind: sql`) run against a real Postgres
  (CI has no service containers). It skips when ungated and **fails** when gated
  with an empty `APERTURE_PG_DSN`, so asking for it and silently not getting it
  cannot happen. Never put a DSN in a file; pass it in the environment.
- `APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./storage/postgres/`
  — the **only** proof that Aperture's own Postgres backend behaves: the whole
  `storage/storagetest` conformance suite against a real server, unqualified and
  schema-pinned, plus concurrency, hostile schema names, and a residue check.
  Same gate contract (skip ungated, fail on an empty DSN); an unrecognised value
  of `APERTURE_PG_INTEGRATION` also fails rather than skipping. `make test`
  cannot prove this backend behaves — only that it has not fallen behind its
  twin (the parity gates above).

## House rules not derivable from the code

These are conventions a reader cannot infer by reading the repo, so they are
written down here rather than rediscovered.

### Commits

`<type>(<effort-slug>/<epic>-<story>): <sentence-case subject>`

```
feat(schema-time-and-keys/E4-S2): two processes can boot against one database
fix(schema-time-and-keys/E3-S1): every service and delegation delete path survives RESTRICT
test(schema-time-and-keys/E5-S2): a dialect cannot drift from its twin
docs(schema-time-and-keys/E6-S1): the schema, time and key contracts get a permanent home
```

Types in use: `feat`, `fix`, `test`, `docs`, `refactor`. When an epic closes, a
`milestone` commit records the slice:

```
milestone(schema-time-and-keys/E5): vertical slice complete — Guardrails that keep the dialects honest
```

Subjects are declarative sentences about what is now true, not imperatives about
what was done.

### Security non-negotiables

- **Account isolation is a hard line.** Account-scoped grants — bestowed or
  direct — must never leak across a principal that belongs to several accounts.
  The lone deliberate exception is `model.AccountWildcard`.
- **Authentication is always external.** Aperture consumes credentials and never
  issues them. `auth/` ships an OIDC/JWT verifier, a Parsec broker adapter, and a
  dev/static authenticator (bearer token = principal id, for local use only).
- **The MCP surface is read + decide + simulate only** — no mutations, ever.
- **No cross-account data in error messages.**

### Frontend

No node build pipeline in development or CI. Everything under
`internal/server/static/vendor/` is a committed pre-built blob, `//go:embed`-ed,
so the binary ships self-contained. Visual conventions — including the
load-bearing "AI-pink is reserved for AI affordances" and "no emoji" rules — are
in [`docs/src/contributing/design-system.md`](docs/src/contributing/design-system.md).

### Example domain

Fixtures, tests, docs, and demos use one generic hierarchy: `org → project →
document`, spelled `account:acme/project:atlas/document:42`.

## What NOT to do

- Don't put business logic in `cmd/aperture/`.
- Don't add a dependency on Pulse — the rules engine uses `expr-lang/expr`
  directly; keep `CGO_ENABLED=0` (no geo/h3 or other CGO packages).
- Don't return bare `errors.New`/`fmt.Errorf` across package boundaries — wrap in
  an `APERTURE_*` coded error.
- Don't commit `.planning/`.
- Don't leak cross-account data through error messages.
