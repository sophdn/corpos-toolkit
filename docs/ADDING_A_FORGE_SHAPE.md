# Adding a forge shape

A "forge shape" is one artifact kind the `forge` / `forge_edit` / `forge_delete`
meta-tools can create, edit, and delete (bug, task, chain, vault-note, …).
After chain `refactor-forge-shape-dispatch`, adding a shape is **two files and
one registration line** — there is no `switch schemaName` to extend anywhere in
the dispatch path. The central handler/create/index-sync/edit/delete paths are
generic; every per-shape behavior lives behind one registered interface.

This is the restored "forge-is-the-artifact-path" convention: you describe the
shape declaratively, implement only the behavior that differs from the generic
default, register it, and a startup gate refuses to let an unregistered or
inconsistent shape ship.

## The two layers

| Layer | What it owns | Where |
|---|---|---|
| **A — declarative schema** (`registry.Schema`) | fields, storage target (db / markdown / dual), table + column_map, sections, lifecycle, `[[hooks]]`, `cross_project`, `supported_ops` | `blueprints/forge-schemas/<name>.toml` |
| **B — imperative Strategy** | typed events, defaults, markdown routing-field derivation, knowledge-index pointer build/read, batch eligibility, the actual create/edit/delete storage op | `go/internal/forge/strategy.go` |

Layer A is data. Layer B is the one interface (`forge.Strategy`) the dispatch
paths call. Most shapes need **no** custom Layer B — see "the trivial case".

## Step 1 — write the blueprint TOML

Drop `blueprints/forge-schemas/<name>.toml`. Copy the nearest existing shape:

- **db-backed, event-sourced** (projection rebuilt from a typed event): see
  `bug.toml` / `chain.toml`.
- **db-backed, plain CRUD row** (no event): see `trained_model.toml`.
- **markdown file artifact**: see `vault-note.toml` (slug-keyed filename) or
  `retrospective.toml` (chain-anchored, slug-less filename).

The TOML declares `[storage] target = "db" | "markdown" | "dual"`, the
`filename_pattern` for markdown shapes, the `[[fields]]`, and `supported_ops`
(omit `"delete"` unless the shape is genuinely deletable — deletion is opt-in).

The server loads blueprints at startup; after adding one call
`admin.schema_reload` (or restart) so the registry rescans.

## Step 2 — implement a Strategy (only the parts that differ)

The interface (`go/internal/forge/strategy.go`):

```go
type Strategy interface {
    SchemaName() string

    // storage ops — the caller owns the *sql.Tx (standalone wraps pool.WithWrite,
    // batch passes its outer tx); reads use db.Queryer (both *sql.DB and *sql.Tx satisfy it)
    Create(ctx, tx *sql.Tx, sc, project, slug, fields) (CreateResult, eventID string, err error)
    Edit(ctx, tx *sql.Tx, sc, project, slug, chainSlug, fields, opts EditOpts) (EditResult, eventID string, err error)
    Delete(ctx, tx *sql.Tx, sc, project, slug, chainSlug) (DeleteResult, error)

    // derived routing fields shared by create + edit (vault-note {subdir}, retro {chain_slug_upper}, memory {kind})
    DeriveRoutingFields(project, slug, fields) (extra map[string]FieldValue, routingNote string, err error)

    // unified knowledge-index contract (replaces the old buildPointer / readArtifactFieldsForIndex / indexDelete switches)
    Indexed() bool
    BuildPointer(project, slug, fields) pointers.KnowledgePointer
    ReadCanonicalFields(ctx, q db.Queryer, schemas, project, slug, editedPath) (map[string]FieldValue, bool, error)

    // capabilities (replace the old hardcoded allowlists)
    BatchEligible() bool
}
```

Every method has a `GenericStrategy` default, so the interface is **total** — a
partial strategy doesn't compile, and the compiler is the first completeness
tier. Embed `GenericStrategy` and override only what differs:

```go
type widgetStrategy struct{ GenericStrategy }

func (widgetStrategy) Indexed() bool       { return true }            // lands in knowledge_pointers
func (widgetStrategy) BatchEligible() bool  { return true }            // accepted inside work.batch

func (widgetStrategy) Create(ctx context.Context, tx *sql.Tx, sc registry.Schema, project, slug string, fields map[string]FieldValue) (CreateResult, string, error) {
    // emit the shape's typed event on tx, apply defaults, etc.
}
func (widgetStrategy) BuildPointer(project, slug string, fields map[string]FieldValue) pointers.KnowledgePointer { /* … */ }
func (widgetStrategy) ReadCanonicalFields(ctx context.Context, q db.Queryer, schemas *registry.Registry, project, slug, editedPath string) (map[string]FieldValue, bool, error) { /* read post-edit row back */ }
```

Guidance by shape family:

- **db, event-sourced** (bug/chain/task/suggestion): override `Create` (emit the
  typed event + apply defaults), `Edit` (delegate to `EditDBInTx`, the shared
  storage-keyed db-edit engine), and — if searchable — `Indexed`+`BuildPointer`+
  `ReadCanonicalFields`. Set `BatchEligible()=true` only for db-target shapes.
- **markdown** (vault-note/memory/retro/report-card): override `Create` (write
  the file + fail-open typed event), `Edit` (delegate to `editMarkdown` /
  `editMemoryMarkdown`), `DeriveRoutingFields` (the filename-routing fields), and
  the index methods if searchable. **Never** `BatchEligible()=true` — the
  capability-consistency gate (below) rejects it, because batch create runs every
  op on one tx and markdown does non-transactional file I/O.

### The trivial case

A plain db CRUD shape with no typed event, no index entry, and no batch
eligibility needs **zero overrides** — register a bare `GenericStrategy`.
`trained_model` is exactly this (the proof the generic path still serves the
simple shape).

## Step 3 — register it

Add one line to `WithCoreStrategies()` in `strategy.go`:

```go
reg(widgetStrategy{GenericStrategy{Name: "widget"}})
// or, trivial case:
reg(GenericStrategy{Name: "widget"})
```

That's the whole wiring. `strategyFor(name)` resolves it for every dispatch path.

## What enforces this (so it can't silently re-cobble)

Three tiers, in `strategy.go`, run by the boot path in `cmd/toolkit-server/main.go`
(`ValidateStrategyRegistry`):

1. **Compiler** — the `Strategy` interface is total; a half-implemented strategy
   doesn't build.
2. **Bijection** (`CheckStrategyCompleteness`) — every blueprint schema has a
   registered strategy and vice-versa. A new TOML with no strategy (or a typo'd
   registration) is caught at startup, not silently routed to generic behavior.
3. **Capability consistency** (`CheckStrategyCapabilityConsistency`) — declared
   capabilities must match the declarative storage. Today: markdown-target ⇒
   `!BatchEligible()`.

Both checks **WARN** at startup by default (a malformed blueprint degrades to a
loud warning rather than bricking boot). Set `TOOLKIT_FORGE_STRICT_STRATEGY_CHECK`
to make either failure **refuse boot** — wire that in CI / production to ensure a
re-cobble can't ship. The gate is also a test:
`TestValidateStrategyRegistry_LivePasses`.

## Don't touch (deliberately central, not per-shape dispatch)

A handful of `if schemaName == …` branches remain in the dispatch files **by
design** — they are envelope / hint / locate concerns a new shape never needs to
edit, not storage-behavior dispatch:

- `handler.go` chain-`tasks` peel — a pre-validation param transform unique to
  the chain+tasks bundle.
- `handler.go` task/chain deferral nudges + `malformedFieldHint` — agent-UX hint
  composition.
- `indexsync.go` vault-note re-forge orphan cleanup — a documented vault-note
  notifier side-effect (only shape that relocates its file on same-slug re-forge).

Adding a shape never requires changing these. If your new shape needs genuinely
new *behavior* (a new event, a new routing field, a new index mapping), that
behavior goes in your `Strategy` — that is the whole point.

## Checklist

- [ ] `blueprints/forge-schemas/<name>.toml` written (copied from the nearest family).
- [ ] `Strategy` implemented in `strategy.go` (or a bare `GenericStrategy` for the trivial case).
- [ ] Registered in `WithCoreStrategies()`.
- [ ] `admin.schema_reload` (or restart) so the registry rescans.
- [ ] `go test -tags sqlite_fts5 ./internal/forge/...` green (bijection + capability gates pass).
