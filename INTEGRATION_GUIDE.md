# Integrating a Project with toolkit-server

How to make the toolkit-server available to a sibling project (`~/dev/dm-toolkit`,
`~/dev/seed-packet`, etc.) so it contributes to the unified DB at
`/home/user/.local/share/toolkit/data/toolkit.db` and its tools load natively in Claude Code.

---

## Mechanical install (preferred)

```bash
toolkit-server install-into <project-path>           # write the two files
toolkit-server install-into <project-path> --check   # dry-run, show diff
toolkit-server install-into <project-path> --force   # overwrite diverging files
```

The binary writes both files with canonical content, idempotently. Pass `--check` first
if the project may already have its own `.mcp.json` (e.g. seed-packet has a sibling
`seed-mcp` entry). If `.mcp.json` already declares non-toolkit servers and `--force` is
not passed, the file is preserved and you hand-merge the `toolkit-server` entry.

To audit the whole `~/dev` tree for drift (stale env-vars, project-local DB silos,
missing entries):

```bash
toolkit-server audit-projects                  # defaults to ~/dev
toolkit-server audit-projects --root /some/dir # custom root
```

---

## Two files, both required (manual reference)

Missing either one causes silent half-broken states.

### 1. `<project-root>/.mcp.json`

```json
{
  "mcpServers": {
    "toolkit-server": {
      "command": "/path/to/mcp-servers/go/bin/toolkit-server",
      "args": [
        "--db", "/path/to/mcp-servers/data/toolkit.db",
        "--default-project", "<your-project-id>",
        "--rubrics-dir", "/path/to/mcp-servers/blueprints/rubrics",
        "--blueprints-dir", "/path/to/mcp-servers/blueprints/forge-schemas"
      ]
    }
  }
}
```

Use `toolkit-server install-into <project-path>` to generate this file with the correct
runtime paths for the current host — it derives `command` from `current_exe()` and takes
`--db` from the flag.

Rules:
- `--db <path>` is **required**. The server refuses to start without it (exits 2 with a
  hint). The previous fallbacks — `MCP_TOOLKIT_DB`, `TOOLKIT_DB`, `./toolkit.db` — were
  dropped to prevent silent silos.
- The path must be the canonical unified DB. Project-local `.local/toolkit.db` files are
  an anti-pattern; they fork the roadmap silently.
- If the project also has its own MCP server (the seed-packet pattern), add it as a
  sibling entry — don't merge.

### 2. `<project-root>/.claude/settings.local.json`

```json
{
  "enabledMcpjsonServers": ["toolkit-server"],
  "enableAllProjectMcpServers": true
}
```

Without this file, Claude Code reads `.mcp.json` but does not load the servers — the
toolkit tools never appear in any session and the assistant has to drive raw stdio
JSON-RPC. The flag is project-scoped trust; safe to commit (seed-packet does).

---

## --default-project and multi-project scoping

`--default-project <id>` sets the project assumed when Claude omits the `project`
parameter. Read actions (chain status, task reads, bug reads) are cross-project by
default. Write actions (forge, task lifecycle, bug writes) require an explicit `project`
parameter — the server rejects writes without one to prevent cross-project contamination.

Each sibling project runs its own toolkit-server instance pointing at the same shared DB.
The `project_id` values in that DB identify which project owns each artifact. This means:
- A chain created with `project=seed-packet` is visible from dm-toolkit via `chain_status`
  (cross-project read), but `forge` in dm-toolkit requires `project=dm-toolkit`.
- The `roadmap_list` action is always cross-project (it shows the unified roadmap).

---

## Skill loading setup

Action manifests live in `mcp-servers/action-manifests/`. Behavioral skills — ambient
agent instructions like `session-routing`, `vault-pull-discipline`, etc. — live in
`mcp-servers/skills/`. After mechanical install, Claude Code sessions in the target
project can call `skill_load` to pull behavioral skills from the toolkit repository.

No additional config is needed beyond the two files above; skill loading follows from the
toolkit-server being registered in `.mcp.json`.

---

## Verify

After both files are in place, restart Claude Code in the project directory. The toolkit
tools should appear as `mcp__toolkit-server__work`, `mcp__toolkit-server__knowledge`,
`mcp__toolkit-server__measure`, `mcp__toolkit-server__admin` — visible in the
deferred-tools list. If they don't, run `claude mcp list` from the project root to see
what's registered.

A successful health check from outside Claude Code:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}' \
  | toolkit-server --db /path/to/mcp-servers/data/toolkit.db --stdio-only
```

Returns `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"rmcp",...}}`.

---

## Don't

- Do not put the `mcpServers` block in `.claude/settings.json` (it sometimes works, but
  `.mcp.json` is the supported channel).
- Do not set `MCP_TOOLKIT_DB` or `TOOLKIT_DB` for the server's benefit; they're no longer
  consulted (the server warns if they're set so stale configs are visible).
- Do not point `--db` at a project-local file. The whole point is one shared DB across
  projects.

---

## Canonical-Go .mcp.json

The Go binary owns every surface. `toolkit-server-admin` was a
transition-window entry pointing at the Rust binary while admin was
being ported through the T59–T66 chain; T68 (2026-05-14) removed the
entry from every `.mcp.json`, and T69 archived the Rust binary itself.
Configs that still carry the old slot should drop it.

### Worked example

```json
{
  "mcpServers": {
    "toolkit-server": {
      "command": "/home/user/dev/corpos-toolkit/go/bin/toolkit-server",
      "args": [
        "--db", "/home/user/.local/share/toolkit/data/toolkit.db",
        "--default-project", "seed-packet",
        "--rubrics-dir", "/home/user/dev/mcp-servers/blueprints/rubrics",
        "--blueprints-dir", "/home/user/dev/mcp-servers/blueprints/forge-schemas"
      ]
    }
  }
}
```

(The transition-window `toolkit-server-admin` Rust entry was removed
from every `.mcp.json` at T68 and the Rust binary was archived at T69,
both on 2026-05-14. Configs that still carry the old slot should drop it.)

`toolkit-server` (Go) omits `--stdio-only` — the Go binary uses stdio transport by
default. `--rubrics-dir` is required for `classify_*`; `--blueprints-dir` is required
for `forge` and `admin.schema_reload`. Every admin action
(`project_register`, `project_list`, `health` / `server_health`,
`schema_version`, `server_version`, `schema_reload`, `host_register`, `host_list`,
`host_remove`, `vault_search_metrics`, `remote_exec`) is served by the Go binary
via `mcp__toolkit-server__admin`. Two actions are deferred stubs returning
`{error: "action_deferred"}` until a concrete caller drives them: `apply_recipe`
and `step_probe` (the recipe walker + parameter resolution + privilege check
chain lives in the Rust archive).

In `settings.local.json`:

```json
{
  "enabledMcpjsonServers": ["toolkit-server"],
  "enableAllProjectMcpServers": true
}
```

### Migration progression

| Stage | Rust binary (`toolkit-server-admin` post-2026-05-14) | Go binary (`toolkit-server`) |
|-------|------------------------------------------------------|------------------------------|
| T13–T16 (scaffold) | work, measure, knowledge, admin (all) | admin.health_ping only |
| T20–T32 | work, knowledge, admin, non-classify measure | classify_* actions |
| T35–T40 | work, admin, non-classify measure | classify_* + knowledge (all) |
| T44–T46 | work, admin | classify_* + knowledge + measure (all) |
| T47 | work, admin | classify_* + knowledge + measure (all) — measure-lib archived |
| T57 | admin | work + classify_* + knowledge + measure (all) — forge/chain/task/bug/roadmap on Go as of 2026-05-14 |
| T58 | admin only | full work + classify_* + knowledge + measure — work-lib + forge-lib archived |
| post-2026-05-14 (T58 close) | admin only — registered as `toolkit-server-admin` for namespace clarity | Canonical name `toolkit-server`; work/knowledge/measure live here. admin stub (`health_ping`) only; full admin reaches Rust via `mcp__toolkit-server-admin__admin`. |
| T59–T64 (2026-05-14) | admin retained as fallback only; `toolkit-server-admin` entry pending removal in T68 | full admin on Go (`mcp__toolkit-server__admin`): project, host, health, schema, vault metrics, remote_exec. `apply_recipe` + `step_probe` are deferred stubs. |
| T66 (2026-05-14) | Rust admin dispatch archived | full admin (every action) |
| T68 (2026-05-14) | `toolkit-server-admin` removed from every .mcp.json | full admin |
| **T69 (2026-05-14, final cutover)** | Rust binary archived | **work, measure, knowledge, admin (all) — canonical** |

**classify surface is Go-owned as of T32 (2026-05-13).** All 8 `classify_*` actions
are served by `mcp__toolkit-server__measure`.

**knowledge surface is Go-owned as of T40 (2026-05-13).** All knowledge actions
(`vault_search`, `vault_read`, `kiwix_search`, `kiwix_fetch`, `kiwix_list_books`,
`library_add`, `library_get`, `library_list_active`, `library_list_sections`,
`library_list_dewey`, `library_find`, `library_update`, `library_retire`,
`library_cross_reference`, `reference_add`, `reference_find`, `reference_retire`,
`knowledge_search`, `knowledge_fetch`, `knowledge_report_miss`) are served by
`mcp__toolkit-server__knowledge`.

**measure surface is Go-owned as of T46 (2026-05-13).** `benchmark_record` +
`benchmark_query` are served by `mcp__toolkit-server__measure` alongside the
`classify_*` actions. The session-journal and emotive-battery surfaces
(`session_open`/`close`/`list`/`read`, `emotive_record`/`list`/`read`/`compare`,
`friction_diff`/`track`, `full_form_retrieve`) were **retired**, not ported —
T42/T43 cancelled, T47 wipes the Rust dispatch + measure-lib crate.

**work surface is Go-owned as of T57 (2026-05-14).** `forge`, `chain_status`,
`chain_state`, `chain_find`, `chain_close`, `task_read`/`search`/`list`/
`start`/`complete`/`cancel`/`reopen`/`stamp_sha`/`block`/`unblock`/
`blockers`/`edit`, `bug_list`/`read`/`resolve`/`reopen`/`stamp_sha`,
`roadmap_list`/`set`/`preview_set`/`insert`/`diff`/`mark_reassessed` are
all served by `mcp__toolkit-server__work`. The Rust server now handles
only admin actions; T58 archives the Rust work-lib + forge-lib dispatch.

**admin surface is Go-owned as of T64 (2026-05-14).** `project_register`,
`project_list`, `health` / `server_health`, `schema_version`,
`server_version`, `schema_reload`, `host_register`, `host_list`,
`host_remove`, `vault_search_metrics`, and `remote_exec` (`system_ssh`
transport only) are served by `mcp__toolkit-server__admin`.
`apply_recipe` and `step_probe` ship as deferred stubs that return a
structured `{error: "action_deferred"}` envelope; a concrete caller
re-running a recipe is the trigger for promoting them to real
implementations (the recipe walker + parameter resolution +
privilege-check chain currently lives in the Rust archive under
`archive/admin-dispatch-2026-05-14/admin.rs`).

**observe HTTP surface is Go-owned as of go-observe-port-parity
(2026-05-14).** `/chains`, `/chains/{slug}`, `/tasks`, `/tasks/search`,
`/bugs`, `/roadmap`, `/roadmap/diff`,
`/session-routing/stats`, `/inference/stats`, `/knowledge/index-card`,
`/benchmarks`, `/benchmarks/timeseries`, `/benchmarks/cards`,
`/benchmarks/rubric-cards`, `/benchmarks/tasks`, plus the `/events` SSE
stream, are served by `go/internal/observehttp` mounted on
`--http-port 3000`. `/projects`, `/emotive`, `/tool-health` were
intentionally dropped (no live dashboard page consumes them; the
project picker degrades gracefully on `/projects` 404). The
`observe-gateway` bash alias points at `go/launch.sh`.

Verify the Go binary is live before mounting:

```bash
# build if absent
cd ~/dev/mcp-servers/go && make build

# quick health check
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}' \
  | /home/user/dev/corpos-toolkit/go/bin/toolkit-server \
      --db /home/user/.local/share/toolkit/data/toolkit.db \
      --default-project seed-packet
```

Claude Code tools from the Go server appear as `mcp__toolkit-server__work`,
`mcp__toolkit-server__admin`, etc. Call `mcp__toolkit-server__admin` with
`action=health` (or `health_ping` for the legacy shim) to confirm the
server is alive.

---

## Integrated projects

| Project | project_id | Notes |
|---------|------------|-------|
| `~/dev/seed-packet` | `seed-packet` | Has sibling `seed-mcp` MCP server in `.mcp.json` |
| `~/dev/dm-toolkit` | `dm-toolkit` | Tauri app; toolkit-server is a separate process |
| `~/dev/self-compile` | `self-compile` | Godot 4 project; toolkit-server invoked from Claude Code sessions only |
