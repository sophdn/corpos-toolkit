# Ecosystem topology — corpos + toolkit + dev services (RATIFIED 2026-06-07)

The authoritative answer to "how do the services and corpos canonically link."
Ratified by Sophi 2026-06-07 (chain `corpos-substrate-topology`) after a cascade of
half-finished work — the dashboard outage, corpos's stale `:3000` default, the
rehearsal fs-namespace breakage, deploy-without-commit drift — was traced to one
missing decision: nobody had ratified the topology, so the containerization flip
half-collided with the locked client-first strategy. This doc is that decision.
**When a wiring question comes up, the answer is here or it gets added here.**

## The strategy this serves

Locked client-first arch (`corpos/docs/` design-orientation + `SWAP_VALIDATION_CRITERIA`
§7): **corpos owns the agent loop; the model stays rented; the toolkit-server is the
shared substrate that BOTH corpos and Claude Code are clients of.** "Toolkit
retirement" is **decompose-not-delete** — a toolkit surface is removed only once
corpos owns a native replacement **AND** Claude Code no longer needs it. The shared
ledger surfaces never qualify (Claude Code consumes them); `fs`/`exec` do (Claude
Code has its own native tools).

## Three planes, split by requirement

The root cause of the cascade was forcing surfaces with conflicting deployment
requirements into one process. They are separated by plane:

### Plane 1 — Shared ledger substrate (containerized, single-writer)
- **Service:** `toolkit-server`, container on host **:3001** (→ container :3000).
- **Properties:** distroless, **sole writer** of the canonical ledger
  `~/.local/share/toolkit/data/toolkit.db` (bind-mounts that repo-independent host
  ledger dir at `/data` + `~/.claude/vault`); mounts ONLY the ledger dir + vault.
  **No host workspace mount, no shell — ever.** That isolation is the point of the
  flip. (The ledger was relocated out of the now-archived `mcp-servers` working tree
  on 2026-06-09, `finish-sophdn-repo-split` T6 — see the repo-ownership map below.)
- **Surfaces:** `work` / `knowledge` / `memory` / `measure` / `ml` / `admin`.
- **Clients:**
  - **Claude Code** → `.mcp.json` spawns `~/.local/bin/toolkit-proxy` (stdio MCP) →
    `--http-base http://localhost:3001`.
  - **corpos** → HTTP `POST /mcp/<surface>` at the canonical endpoint (below).
- This plane is what "retirement" must **preserve** for Claude Code.

### Plane 2 — Agent-loop / host-effecting (per-harness, NOT shared, NOT containerized)
- **Surfaces:** `fs` / `exec` / `sys` — the harness-loop families.
- **Claude Code:** uses its **own native** tools (Read/Write/Edit/Bash/Grep/Glob).
- **corpos:** owns these as **native organs** via the `internal/tool.Provider`
  seam — operating the host workspace **directly, no container hop** (chain
  `corpos-substrate-topology` T4/T5; `cannibalize-discipline`). This is the
  `SWAP_VALIDATION` §0 entry-gate work (owned exec substrate + `fs.move`/`rm`).
- The `fs` surface and the `sys.exec` action that were a **vestigial client-first
  stopgap** inside the containerized toolkit are **RETIRED (T6, 2026-06-07)** now
  that corpos owns them natively: the toolkit no longer registers `fs` (stdio +
  HTTP + proxy) nor `sys.exec`. The `sys` **introspection** actions
  (`ps`/`ports`/`units`/`containers`) are **retained** — read-only, and corpos's
  native sys organ delegates them back here. The `internal/fs` package and the
  `exec_action.go`/`exec_runner.go` implementation are kept in-tree as the parity
  oracle (decompose-not-delete), just unwired from any live surface. These
  surfaces must not be "fixed" by adding host mounts or a shell to the Plane-1
  container; that conflates the DB-isolation boundary with host-effecting reach
  (the mistake that caused the rehearsal failure).

### Plane 3 — Inference + observability (containerized, corpos-net)
- **`llama-server-container`** — GPU inference (Qwen2.5-32B), host **:8081**. The
  native `~/.config/systemd/user/llama-server.service` is the **disabled fallback**;
  `Conflicts=` enforces the GPU/:8081 cutover so the two never co-run.
- **`toolkit-dashboard`** — static observe SPA, host **:8082** → container :8080;
  talks to `:3001` from the **browser** (`VITE_API_BASE_URL`, baked).

## The linking law

1. **One canonical toolkit endpoint.** Host clients use **`http://localhost:3001`**;
   container↔container uses the **corpos-net DNS name** (`toolkit-server:3000`
   inside the net). Defined in ONE config; **no `:3000` host references** — the
   native `:3000` daemon is retired (post-flip). A future port change is one edit.
2. **`deploy/quadlet/` is the sole source of truth.** Units are *materialized* into
   `~/.config/containers/systemd/` by `scripts/install-quadlet-units.sh`; never
   hand-edit the materialized copies. A deployed==committed check (chain T3) +
   the host-gateway/sibling-container guard (already in the installer) enforce this.
3. **corpos-net DNS for container↔container**, host loopback for host→container.
   A host->container dependency flip MUST sweep dependents for stale
   `localhost`/`host-gateway`/`:port` assumptions (the guard automates the
   host-gateway case).

## Canonical repo ownership (exactly one source per service)

The cascade this doc closes had a sibling root cause: the monorepo split was left
half-done, so a service could be built from two diverged repos at once (the
toolkit-server image came from `mcp-servers` while its proxy came from `toolkit` —
split-brain). The rule going forward: **each running artifact has exactly ONE
canonical source repo.** This table is that registry; the provenance guard
(`scripts/check-canonical-provenance.sh`) reads it and fails loudly on a mismatch.

| Service / artifact          | Canonical repo (gitea)        | Local dir              | Provenance signal |
|-----------------------------|-------------------------------|------------------------|-------------------|
| toolkit-server image        | `sophdn/corpos-toolkit`      | `~/dev/corpos-toolkit` | OCI `org.opencontainers.image.source` label |
| toolkit-proxy binary        | `sophdn/corpos-toolkit` (`go/cmd/toolkit-proxy`) | `~/dev/corpos-toolkit` | `toolkit-proxy -version` build SHA (ancestry check) |
| toolkit-dashboard image     | `sophdn/corpos-toolkit-dashboard` | `~/dev/corpos-toolkit-dashboard` | OCI source label |
| corpos (agent-OS CLI)       | `sophdn/corpos`              | `~/dev/corpos`         | n/a (oneshot CLI) |
| canonical ledger DB         | *(none — host data, not a repo)* | `~/.local/share/toolkit/data/toolkit.db` | bind-mount path |
| agent hooks (substrate glue) | `sophdn/corpos-toolkit` (`hooks/`) | `~/dev/corpos-toolkit/hooks` | `~/.claude/hooks/*` symlink here |
| agent skills | corpos embeds its referenced subset (`internal/skills`); the full Claude-harness corpus is real files | `~/.claude/skills/*` (real, de-symlinked 2026-06-09) | — |
| **`sophdn/mcp-servers`**    | **ARCHIVED / FROZEN** (read-only on gitea) — no canonical role; assets relocated off it 2026-06-09 | `~/dev/mcp-servers` (no live dependency; removable) | — |

> **Rename landed (T9, 2026-06-09):** `sophdn/toolkit` → `sophdn/corpos-toolkit` and
> `sophdn/toolkit-dashboard` → `sophdn/corpos-toolkit-dashboard`; local dirs, remotes,
> image OCI labels, the provenance registry, and the ledger project (`mcp-servers` →
> `corpos-toolkit`, T10) all moved together. The separate `corpos` project/repo is unchanged.

## Quick reference

| Service | Plane | Host port | Container? | Clients |
|---|---|---|---|---|
| toolkit-server | 1 ledger | 3001→3000 | yes, distroless, data+vault only | Claude Code (proxy), corpos (HTTP) |
| corpos fs/exec/sys | 2 agent-loop | — | **no** (native to corpos) | corpos itself |
| Claude Code fs/exec | 2 agent-loop | — | no (harness-native) | Claude Code itself |
| llama-server-container | 3 inference | 8081 | yes, corpos-net, GPU/CDI | corpos, toolkit health probe |
| toolkit-dashboard | 3 observability | 8082→8080 | yes, corpos-net | browser → :3001 |

## See also
- `deploy/quadlet/README.md` — the quadlet units + install/teardown.
- `docs/INFERENCE_DISPATCH.md` — what `toolkit-decomposition` moves to corpos and
  what the retirement chain must **retain** for Claude Code.
- `corpos/docs/SWAP_VALIDATION_CRITERIA.md` — §0 entry gate (exec + fs.move/rm), §7
  (swap rides client-first, not gated on the flip).
