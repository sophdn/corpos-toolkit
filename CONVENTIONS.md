# toolkit Conventions

Reference for agents and humans working on this repo. Every change follows these rules; deviations must be documented inline with a reason. This doc is updated whenever a new convention is established.

This is the standalone **toolkit** backend (`sophdn/corpos-toolkit`), split out of the `mcp-servers` monorepo (2026-06, chain `auto-startup-dev-services` T2). It is **single-language Go** post-rust-retirement (T6) and post-db-hardening (T7). The frontend lives in `sophdn/corpos-toolkit-dashboard` and the inference service in `sophdn/llama-server`; both reach this backend over its HTTP API, not a shared tree. Language-level Go conventions defer to the `go-conventions` and `coding-philosophy` skills; this doc carries the repo-specific architecture, layout, and discipline.

---

## File Layout

```
go/
  cmd/
    toolkit-server/            # the MCP server binary (stdio MCP + HTTP observe surface)
    toolkit-proxy/             # stdio→HTTP proxy — the .mcp.json command post-T7 cutover
    codemap-gen/ audit-emit/ ...  # one-shot maintenance / audit-emit / codegen binaries
  internal/                    # all library packages (dispatch, db, work, knowledge, ...)
  go.mod / go.sum / Makefile / launch.sh
  bin/                         # built binaries (gitignored)
blueprints/
  forge-schemas/ rubrics/ events/  # TOML/JSON loaded at startup (schemas, rubrics, event specs)
action-manifests/              # MCP action-manifest TOML + instruction files
clients/                       # reference client libraries that consume the MCP surfaces
  bridge-harness/ escalation/
deploy/                        # container deploy: quadlet/ user units + toolkit-server/ image
hooks/                         # Claude Code harness hook scripts (read by the harness, not the daemon)
measure/                       # offline corpus-build / classification Python helpers + corpora
scripts/                       # precommit.sh gate + migration/event-sync/maintenance scripts
docs/                          # architecture notes + retrospective / audit-trail records
skills/                        # canonical toolkit-core agent disciplines (see docs/SKILLS_OWNERSHIP.md); corpos mirrors these into its embed
```

**Rules:**
- `cmd/<binary>/main.go` is a thin I/O shell; business logic lives in `internal/` packages.
- Library packages live under `go/internal/`. Public API is the package's exported surface; cross-package helpers stay unexported unless a second consumer exists.
- Fixtures are plain JSON or TOML — no code in fixture files.
- `go/bin/` is gitignored. Committed artifacts are source only.

---

## Architecture

The server is organized into layers. Each entry names the layer's home and responsibility. All layers are Go in a single process (`cmd/toolkit-server`); the dashboard and inference service are separate repos reached over HTTP.

### Layer table

| Layer | Home | Responsibility |
|---|---|---|
| **Transport** | `go/cmd/toolkit-server` (+ `internal/mcp*`) | stdio JSON-RPC framing; MCP protocol lifecycle (initialize, shutdown, capabilities) |
| **Dispatch/RPC** | `go/internal/dispatch/` + per-surface packages (`work/`, `admin/`, `measure/`, `knowledge/`, `observehttp/`) | Action vocabulary, argument parsing, routing calls to the data layer |
| **Data** | `go/internal/db/` (pool + embedded migration runner); `go/internal/work/` + `go/internal/construct/` (work-surface queries, write orchestration, schema validation) | SQLite schema, migrations, domain helpers — Go reads the DB directly via `database/sql` + `mattn/go-sqlite3` |
| **Inference Routing** | `go/internal/inference/` (`anthropic/`, `llamacpp/`, `router/`) + `go/internal/qwenretrieve/` | Routes calls to local Qwen (llama-server) and remote Claude; manages retry + fallback |
| **Event Bus** | `go/internal/eventbus/`, mounted under `go/internal/observehttp/` at `/events` | SSE broadcast for artifact-create events; consumed by the dashboard SPA over HTTP |
| **Observe HTTP** | `go/internal/observehttp/` | Read-only HTTP surface (`/chains`, `/tasks`, `/bugs`, `/roadmap`, `/benchmarks/*`, `/inference/stats`, `/knowledge/index-card`, `/events`, `/version`, `/healthz`) |

### Layer detail

**Transport** — reads/writes newline-delimited JSON-RPC on stdin/stdout via the MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`). MCP capability negotiation and lifecycle only; no business logic.

**Dispatch/RPC** — routes MCP `tools/call` invocations to the correct handler by meta-tool and action name. Validates required fields; returns structured JSON errors for unknown actions. The dispatcher mints a `span_id` per request (the join key between structured logs and the events ledger).

**Data** — all SQLite access is Go. Production code reads through projection tables and writes through the events log (post agent-substrate-crud-retirement T6); the eight artifact-lifecycle CRUD tables were dropped in migration 060 and a non-migration SQL reference to them fails the gate. The Go binary applies any pending migrations on `db.Open` via the embedded runner in `go/internal/db/migrate.go` — see §Migrations.

**Inference Routing** — decides which model handles a request, formats the prompt, calls the endpoint, and parses the response. Local Qwen now runs in the `sophdn/llama-server` container, reached at `http://llama-server:8081` on the shared `corpos-net` (see `deploy/`); the base URL is an env override so the native `localhost:8081` fallback still works.

**Event Bus + Observe HTTP** — the read-only HTTP surface the dashboard SPA consumes. No direct SQLite access from the dashboard; every read goes through this surface. `/version` (build-time gitSHA) is the image-lags-HEAD contract's source of truth.

---

## Go module layout

Go code lives at `go/` with its own module.

```
go/                   # Go module root (module: toolkit)
  go.mod / go.sum
  cmd/toolkit-server/main.go
  internal/{dispatch,db,work,knowledge,measure,inference,observehttp,forge,...}/
  Makefile / launch.sh
  bin/                # built binaries (gitignored)
```

**Module name:** `toolkit`. Import paths: `toolkit/internal/dispatch`, `toolkit/internal/db`, etc.

The Go module is rooted at `go/go.mod`, not the repo root. From the repo root, drive Go through `make -C go <target>` (matches the gate), or `cd go && go test ./internal/...` for ad-hoc runs. See the repo `CLAUDE.md` for the cwd/Go-hop discipline.

| Command | Run from |
|---|---|
| `make -C go build` / `build-all` | repo root |
| `make -C go test` | repo root |
| `make -C go smoke` | repo root |
| `scripts/precommit.sh` (the gate) | repo root |

---

## Pre-commit gate

`scripts/precommit.sh` is the single entrypoint; `.git-hooks/pre-commit` wires it as the hook (install once via `bash scripts/install-hooks.sh`). CI runs the same script, so local and CI gates are structurally identical. It is **Go-only** — the dashboard CSS-token / TypeScript / Go→TS codegen stages moved to `sophdn/corpos-toolkit-dashboard` with the frontend split. Stages, in order:

1. **CONVENTIONS.md layout integrity** — every bare `dir/` entry at the root of a fenced layout block in this file must exist on disk.
2. **Agent-primary Go log discipline** — the agent-primary packages (`work`, `forge`, `knowledge`, `measure`, `admin`) must log through `obs.Logger(ctx)` (structured slog carrying the request `span_id`), never `log.Printf` / `fmt.Println`. CLI scaffolding under `cmd/` is out of scope (user-facing stdout).
3. **Retired-CRUD-ref guard** — no non-migration SQL against the eight artifact-lifecycle tables dropped in migration 060 (T6).
4. **Quadlet deployment-artifact integrity** — the canonical quadlet unit must keep the host-vault bind + the `llama-server:8081` inference URL (guards bug 989/990 regressions).
5. **Single-source syncs** — migrations (canonical → testutil mirror), event-schemas, runtime-affecting-paths manifest parity, cmd-binary gitignore parity, action-docs corpus no-diff.
6. **CODEMAP regen + package `doc.go` lint**, orphan precommit-fmt stash detection.
7. **Go build/test** — `make -C go fmt` (+ re-stage), `vet`, `golangci-lint run ./...`, `build-all`, `test`.
8. **Dependency vuln scan** — `make -C go vuln` (`go mod verify` + `govulncheck`).

Fail-fast on the first non-zero stage. Language-level style (error wrapping, no-panic, typing) is enforced by `golangci-lint` and the `go-conventions` skill rather than restated here.

---

## Literal-filter contract (silently-inert-arm guard)

A reader that runs `... WHERE col = 'literal'` (or `col IN (...)`) on an enum / `source_type` / `status` / `kind` value forms a **contract with the writer**: if no writer ever emits that literal, the query matches zero rows forever and the feature it powers degrades to a silent no-op (no error, no panic). This bit us twice — the `vault-note` dedup bug (`source_type='vault-note'` vs. the written `'vault'`, fix `c9388bcb`) and the `parse_context` skip-rate arm (chain `latent-inert-arm-audit` S1). In-memory unit tests that build the index directly never exercise the loader query, so the class ships untested.

**Rule — every literal-filter reader arm must be contract-guarded, by column kind:**

- **CHECK-governed enum columns** (e.g. `grounding_events.query_source`, `query_interactions.click_kind`): covered automatically by `telemetry.TestLiteralFilterArms_StayWithinCheckSet`, which scans every non-test reader under `go/internal` and asserts each filtered literal is admitted by the live schema CHECK set. No registry to maintain — adding a forbidden literal fails the test with a `file:line`. When a *new* CHECK-constrained enum column gains reader arms, add its `(table, column)` to `checkGovernedEnumColumns` in that file.
- **Free-text columns** (no CHECK — e.g. `knowledge_pointers.source_type`, `grounding_events.action`): there is no admitted-set to validate against, so each loader needs a **seed+load DB test** that seeds the *writer's* literal into a `testutil.NewTestDB` and asserts it reaches the reader. Model: `arcreview.TestLoadExistingArtifactsForDedupe_IncludesVaultNotes`. In-memory index fixtures do **not** satisfy this — they bypass the loader query, which is exactly where the literal lives.

Background + reflexes: `vault/learnings/general/2026-05-24_lookup-arm-querying-unwritten-literal-is-silently-inert.md`.

---

## Migrations

Schema migrations live in **two** in-sync locations — `go/internal/db/migrations/` (canonical, embedded into the binary via `//go:embed` in `go/internal/db/migrate.go`) and `go/internal/testutil/migrations/` (testutil hermetic fixture; a real on-disk copy because Go embed rejects symlinks). The pre-commit gate's migration-sync stage mirrors canonical → testutil and re-stages touched mirror files.

### Runner ownership

The runner in `go/internal/db/migrate.go` applies pending migrations on `db.Open`, so a fresh or behind DB is brought current automatically when the binary starts. Load-bearing properties:

- single connection held for the whole migration loop (SQLite WAL snapshot isolation prevents stale-schema reads),
- per-migration savepoints (rolls back partial application cleanly),
- a SQL statement splitter that ignores `;` inside line/block comments, single-quoted strings, and `BEGIN…END` trigger bodies.

`internal/db.TestOpen_AppliesAllMigrations` asserts the embedded head matches what `_migrations` records on open; the sync script makes that head match automatic rather than disciplined.

### Source-of-truth discipline

The canonical authoring path is `forge(schema_name="migration", slug=<kebab_name>, up_sql=<body>, docstring=<optional>)`. forge owns the next-NNN scan (max+1 — gaps are NOT auto-filled), the canonical + testutil-mirror dual-write with rollback on partial failure, the byte-identical hash-compare, and a forge-time SQL parse-check against an in-memory SQLite DB so unparseable bodies reject before the file lands.

**Collision-resistant under parallel worktree agents:** the next-number scan takes `max(filesystem_max, substrate_max)+1`, where `substrate_max` is the highest `migration_number` ever recorded in a `MigrationForged` event in the shared `data/toolkit.db`. Two agents in separate worktrees each see the same *filesystem* max, but because every forge emits `MigrationForged` into the same DB and those writes are cross-process serialized (`busy_timeout` + `BEGIN IMMEDIATE`), the second forge observes the first's committed number and steps past it. (An agent forging against a *private* DB copy loses this coordination; forge against the shared substrate.) Idempotent on re-run: re-forging an existing slug returns the existing path tuple without overwriting — use `forge_edit` for intentional updates.

Raw-edit fallback (still supported): hand-author an `.sql` file under `go/internal/db/migrations/<NNN>_<name>.sql` and the gate's migration-sync stage mirrors it to testutil. Prefer `forge(migration)` — it eliminates the off-by-one numbering, forgotten mirror-sync, and mirror-edited-by-mistake foot-guns. To test a draft before commit, run `bash scripts/sync-migrations.sh` manually after editing canonical (same script the gate uses). Editing a mirror dir directly is reverted at the next sync — canonical always wins.

---

## Polymorphic-ref naming

A table that references *either* a chain *or* a task (rather than one or the other) uses two columns instead of a foreign key: `ref_kind` (with a `CHECK (ref_kind IN (...))` constraint) plus `ref_slug`. This is the *polymorphic reference* shape. The canonical example is `roadmap_items`, which carries a row per chain or task on the live roadmap; the polymorphism is deliberate because a single ordered list spans both kinds.

### Naming consequences

The columns are deliberately *not* named `chain_slug` / `task_slug`, even though the sibling tables (`chains.slug`, `tasks.chain_slug`, `bugs.routed_chain_slug`) all use those names. The divergence is the price of the polymorphism — agents pattern-matching the sibling schemas will write SQL with column names that don't exist on a polymorphic table, so the trade-off is visible at the schema level rather than hidden inside a wider column. (`roadmap_items` does *also* carry a non-polymorphic `chain_slug` column, but only as a denormalised hint for the dashboard — it points at the **parent** chain of a task-kind row, not the polymorphic target.)

### Lifecycle: row-presence, not `status`

Polymorphic-ref tables do **not** mirror a `status` column from the target. The lifecycle of a row is "is the row still present?" — when the underlying chain/task closes, the row is DELETEd by the handler that closes it (see `internal/work/task.go:transitionTask`'s roadmap-cleanup branch). The target's actual status lives on `chains.status` / `tasks.status`, joined in via the convenience view when callers need it (e.g. `roadmap_items_v_with_status` — migration 028).

### Adding a new polymorphic-ref table

- `ref_kind TEXT NOT NULL CHECK (ref_kind IN ('chain', 'task', …))`
- `ref_slug TEXT NOT NULL`
- `project_id TEXT NOT NULL REFERENCES projects(id)` (every namespaced toolkit table carries `project_id`)
- `UNIQUE (project_id, ref_kind, ref_slug)`
- No `status` column. Add a `<table>_v_with_status` view in the same migration so the natural-shape query works without thinking about the polymorphism.
- Lead the migration with a comment block explaining the divergence (see migrations 002 + 028 as the reference pair).

If you intentionally diverge — e.g. a polymorphic-ref table where target status genuinely needs mirroring because deletion-on-close is inappropriate — document the divergence inline in the migration so the next reader doesn't conclude the convention was forgotten.

---

## Skill Format

Each MCP action's skill is a TOML definition file + a Markdown instructions file under `action-manifests/`.

### Definition file: `action-manifests/<tool-name>.toml`

```toml
[skill]
name = "tool-name"           # kebab-case, matches the TOML filename
version = "1.0.0"
description = "One sentence: what this skill does and when it fires."
spectrum = "contextual"      # one of: contextual, skill-gated, explicit

[trigger]
keywords = [                 # REQUIRED — must not be empty
    "phrase the user would say",
    "another natural phrasing",
]
when_not = [                 # scope exclusions — what NOT to trigger on
    "adjacent request that should not trigger this",
]

[instructions]
path = "action-manifests/instructions/<tool-name>.md"
```

**`spectrum` values:**
- `contextual` — agent reaches for this automatically when context matches; description does the work.
- `skill-gated` — part of a larger procedure; a skill document chains the steps together.
- `explicit` — user must ask for it by name; description actively discourages contextual use.

**Registration rule:** skills without at least two entries in `trigger.keywords` fail at registration. Vague keywords ("helps with tasks") are rejected on review. Keywords must match how users actually phrase requests.

### Instructions file: `action-manifests/instructions/<tool-name>.md`

```markdown
# <Tool Name>

## When to use
[Specific conditions, written as a user would describe the problem]

## When not to use
[Scope exclusions and adjacent requests this skill does not cover]

## Steps
[For skill-gated spectrum only: the procedure the agent follows]
```

---

## Workflow Intent Doc Standard

Every workflow definition (whether in code or as a procedure doc) must state:

1. **Workflow served** — what user request or agent state triggers entry.
2. **Entry conditions** — what must be true before the workflow begins (tools available, data present, etc.).
3. **Expected tool sequence** — the ordered steps and the decision points between them.
4. **Completed vs. aborted** — what a successful terminal state looks like vs. a failed/partial one, and what the agent should do on abort.

Workflows that lack explicit entry conditions cannot be tested for correct triggering. Workflows that lack a completed-vs-aborted distinction cannot be tested for partial-failure behavior.

---

## Benchmark result schema

Agent-loop benchmark runs (driven from `go/internal/benchmarks/`) write result rows to the benchmark DB.

### Tool result row

| Column | Type | Notes |
|---|---|---|
| `id` | UUID | primary key |
| `tool_name` | TEXT | matches the MCP tool name exactly |
| `model` | TEXT | e.g. `claude-sonnet-4-6`, `qwen2.5-32b` |
| `run_at` | INTEGER | Unix timestamp (seconds) |
| `wall_clock_ms` | INTEGER | end-to-end wall time |
| `input_tokens` | INTEGER | prompt tokens; null if API does not expose |
| `output_tokens` | INTEGER | completion tokens; null if API does not expose |
| `tool_use_tokens` | INTEGER | tool-use tokens where available; null otherwise |
| `invoked_contextually` | BOOLEAN | true if prompt described the problem, not the tool name |
| `invocation_ok` | BOOLEAN | tool was invoked with reasonable arguments |
| `notes` | TEXT | optional; free-form; deviations, failures, observations |

### Workflow result row

Extends the tool row with per-step breakdown: `workflow_name`, `step_id`, `step_wall_clock_ms`, `step_input_tokens`, `step_output_tokens`, `sequence_correct` (agent followed the declared step order), `failure_injected`, `failure_handled` (agent handled failure without fabricating output).

**Tagging invariant:** every row has `tool_name` (or `workflow_name`) + `model` + `run_at`. Without all three, cross-model and longitudinal queries break.

---

## Project Integration

See [`INTEGRATION_GUIDE.md`](INTEGRATION_GUIDE.md) for the full onboarding walkthrough: `.mcp.json` template, canonical DB path, `--default-project` usage, multi-project scoping, skill loading setup, and worked examples.

Post-T7 cutover, the canonical DB is owned by the **toolkit container** and reached through the behavior-free `toolkit-proxy` (the `.mcp.json` command); the proxy opens no DB and forwards every call to the container's `POST /mcp/<surface>`. See the repo `CLAUDE.md` §"DB ownership" for the proxy + single-writer model.

### CI / CONVENTIONS.md boundary

`CONVENTIONS.md` is the authoritative source for repo layout and conventions. The pre-commit gate enforces path-level integrity: any bare top-level `dir/` entry in a fenced layout block here must exist on disk. Update this file when adding or retiring top-level directories.

---

## Adding a New MCP Action or Workflow

Checklist for every new action:

1. Implement the handler in the appropriate surface package under `go/internal/<surface>/` (e.g. `work/`, `knowledge/`); wire it into that surface's dispatch table.
2. Agent-primary surfaces log through `obs.Logger(ctx)`; return structured JSON errors (never panic on caller input).
3. Write the action-manifest TOML + instructions file under `action-manifests/`.
4. Add unit tests (inline `_test.go`) and at least one integration test against a `testutil.NewTestDB`.
5. For DB-backed actions, exercise the loader query in a seed+load test (see §Literal-filter contract), not just an in-memory fixture.
6. Record a baseline benchmark run per model where the action is agent-invocable.
7. Keep the gate green: `scripts/precommit.sh` must pass (fmt/vet/golangci-lint/build/test/vuln + the structural stages).
8. If this establishes a new convention, update this document.
