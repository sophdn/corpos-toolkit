# Codegen

Go structs are the canonical source-of-truth for shared types that cross the
Go-backend ↔ TypeScript-dashboard boundary. `apps/dashboard/src/api/types.gen.ts`
is regenerated from the Go side via [tygo](https://github.com/gzuidhof/tygo).
The precommit gate runs the codegen + fails on drift.

Chain `rust-retirement-and-db-hardening` T8 (2026-05-22) introduced this
pattern. Pre-T8 the dashboard hand-maintained TS types that mirrored the
Rust enums/structs serving them; the Rust→Go migration broke that mirror
because the new Go-side types had subtly different shapes (e.g. pointer-to-T
in Go marshals to `null` in JSON, vs Rust's `Option<T>` serializing the same
way). Without codegen, every Go-side type change required a parallel
hand-edit in TS; the precommit gate's freshness check stops that pattern.

## What's covered today

Chain T8 (rust-retirement-and-db-hardening, 2026-05-22) seeded codegen for
the benchmark types. Chain T3 (harvest-the-consolidation, 2026-05-23)
extended coverage to the bug / task / chain / suggestion API response
types — the surfaces the dashboard polls most often.

| Generated | Source |
|---|---|
| `ModelMetrics` | `go/internal/observehttp/benchmarks_aggregate.go` |
| `ShapeCard` | `go/internal/observehttp/benchmarks.go` |
| `RubricCard` | `go/internal/observehttp/benchmarks.go` |
| `TaskCard` | `go/internal/observehttp/benchmarks.go` |
| `TimeseriesPoint` | `go/internal/observehttp/benchmarks.go` |
| `BugRow` | `go/internal/observehttp/bugs.go` |
| `ChainRow` / `ChainDetail` | `go/internal/observehttp/chains.go` |
| `TaskRow` / `TaskContentMatch` / `SearchResponse` | `go/internal/observehttp/tasks.go` |
| `SuggestionRow` | `go/internal/observehttp/suggestions.go` |

The hand-maintained TS files in `apps/dashboard/src/api/{bugs,chains,tasks,suggestions}.ts`
import the generated wire-shape types directly (no inline `ObserveBugRow`-style
duplicate). The adapter functions (e.g. `adaptBugRow`) convert from the
wire shape into the dashboard's enriched internal shapes (`BugListRow`,
`ChainSummary`, `SuggestionListRow`) — those internal shapes live in
`apps/dashboard/src/lib/{bug,chain,suggestion}Index.ts` and remain
hand-maintained because they extend the wire shape with derived /
display-only fields.

The hand-maintained TS files in `apps/dashboard/src/lib/benchmark{Cards,RubricCards,Tasks}.ts`
re-export the generated benchmark types — with narrowed unions where the TS
side wants closed-set literal types (e.g. `TaskShape = 'Extract' | 'Classify' | ...`)
and the Go side has them as `string`. The narrowing is documented inline at
each re-export.

API surfaces not yet covered (action-docs, admin, audit-events, context-pulls,
daemon-staleness, dispatch-policy, inference, knowledge, project, roadmap,
scenarios, telemetry, version) use the older hand-maintained pattern.
Extension is straightforward — add the file to `tygo.yaml`'s `include_files`
list and rerun `make -C go gen-types`.

## Workflow

1. Edit the Go struct under `go/internal/observehttp/` (or whichever package
   tygo is configured to read).
2. Run `make -C go gen-types`. This writes `apps/dashboard/src/api/types.gen.ts`.
3. Stage both the Go file and the regenerated TS file in the same commit.
   The precommit gate also auto-runs the codegen + diffs the result.

If you forget step 2, the precommit gate's freshness check fires:

```
ERROR: types.gen.ts drifted — Go-side benchmark struct changed without a regeneration.
       Run: make -C go gen-types && git add apps/dashboard/src/api/types.gen.ts
```

Despite the "benchmark struct" wording in the error message (legacy from T8),
the check now covers every package listed under `tygo.yaml`'s
`include_files`: any Go-side struct rename, field add, or field-tag change
fires it.

## Per-field overrides

tygo reads JSON struct tags by default. For Go fields whose generated TS
shape doesn't match the wire format, add a `tstype` struct tag:

```go
// `*float64` would default to optional (`accuracy?: number`); the wire
// format actually emits `null` (Go marshals nil pointer to JSON null
// unless `omitempty` is set). The tstype override fixes the shape.
Accuracy *float64 `json:"accuracy" tstype:"number | null,required"`
```

The `,required` suffix tells tygo to skip the optional `?` it would
otherwise add for pointer-type fields. The string before `,` is the
literal TypeScript type to emit verbatim.

See `go/internal/observehttp/benchmarks_aggregate.go::ModelMetrics` for
the canonical worked example.

## Configuration

`tygo.yaml` at the repo root. Documented inline; the load-bearing
options are:

- `path` — Go package path (relative to the Go module).
- `output_path` — TS file to write (relative to tygo's CWD, which is `go/`).
- `include_files` — restrict tygo to a subset of the package's `.go` files.
- `type_mappings` — global Go-type → TS-type substitutions.
- `frontmatter` — string prepended to the generated TS file (used here
  to mark it generated and point at the source).

## Limitations

- Unexported Go types are skipped. Pre-T8 the `modelMetrics` struct was
  unexported because nothing outside `observehttp` consumed it; T8
  promoted it (along with `ShapeCard`, `RubricCard`, `TaskCard`,
  `TimeseriesPoint`) to exported. Harvest-the-consolidation T3 did the
  same for `BugRow` / `ChainRow` / `ChainDetail` / `TaskRow` /
  `TaskContentMatch` / `SearchResponse` / `SuggestionRow`. Future shared
  types should be exported from the start.
- Closed-set string types (e.g. `task_shape` as a literal union) aren't
  inferred from Go `string` columns. The dashboard side re-exports
  generated types with narrowed unions via `Omit<Gen, 'field'> & { field: ... }`.
- `time.Time` maps to `string` (RFC3339) via the global `type_mappings`.
- `tygo` is declared as a Go 1.24+ `tool` directive in `go/go.mod`
  (pinned to v0.2.21 by chain harvest-the-consolidation T1, 2026-05-23).
  `make -C go gen-types` invokes it via `go tool tygo`, so the precommit
  freshness check runs unconditionally — no PATH-discovery dance, no
  silent skip when tygo is missing. A fresh clone needs `go mod download`
  (the gate runs that implicitly).

## Action-docs corpus (Go → TOML)

A second, independent codegen pipeline. The work meta-tool's per-action
documentation (`go/internal/actiondocs/corpus/work/*.toml`, `go:embed`'d into
the binary and served by `admin.action_describe(surface="work", …)`) is
GENERATED from the canonical
`work.actionSpecs` catalog in `go/internal/work/actions_discovery.go` — not
hand-authored. `actionSpecs` is the single source of truth: it carries the
typed param shape (consumed by `work_actions` + the `CallShape` param-error
renderer) AND the surface-doc fields (purpose, param aliases, value aliases,
errors, notes, envelope requirements, returns). Chain
`single-source-action-describe` introduced this so the two doc systems
(`work_actions` / `CallShape` vs `admin.action_describe`) can no longer drift.

Only the **work** surface is generated. The `knowledge` / `measure` / `admin`
corpora and `work/_general.toml` remain hand-authored.

| Generated | Source |
|---|---|
| `go/internal/actiondocs/corpus/work/<action>.toml` | `work.actionSpecs` (`go/internal/work/actions_discovery.go`) |

### Workflow

1. Edit `work.actionSpecs` (add an action, change a param/alias/note/etc.).
2. Run `scripts/action-docs-corpus-gen`. This rewrites every
   `go/internal/actiondocs/corpus/work/*.toml` chunk from the catalog.
3. Stage both `actions_discovery.go` and the regenerated chunks together.
   The chunks are `go:embed`'d, so the rebuild bakes them into the binary —
   a chunk edit is a runtime-affecting change (needs a rebuild + restart).

`scripts/action-docs-corpus-gen --check` exits non-zero if any on-disk chunk
differs from a fresh generation (the no-diff gate). `--stdout <action>` prints
one chunk for debugging without writing.

### Type vocabulary

`actionSpecs` param types map to the action-doc TOML `type` vocabulary:

| `ActionSpec` type | action-doc `type` |
|---|---|
| `string` (required) | `string` |
| `string` (optional) | `optional_string` |
| `int64` | `integer` |
| `bool` | `bool` |
| `string[]` | `list` |
| `object[]` | `object[]` |
| `object` / `json` | `object` |

`int64` MUST map to `integer` (never `optional_string`) so the
`param_type_parity` gate — which compares the doc type family against the
handler struct field kind — stays green; documenting an int64-backed param as
a string is the bug-888 trap.

### Why a custom emitter (not tygo)

Different boundary (Go → TOML, not Go → TS) and different consumer (the MCP
server's own loader, not the dashboard). The generator projects each
`ActionSpec` into an `actiondocs.ActionDoc` and serializes it with the same
`BurntSushi/toml` encoder the loader round-trips through. The encoder emits
all scalar keys (`surface` / `action` / `purpose` / `notes`) before any
`[[table]]` array — which is both valid TOML and the correct placement: the
prior hand-authored corpus put `notes` after `[[params]]` / `[[errors]]`,
where TOML parsed it into the last table element and silently dropped it.
