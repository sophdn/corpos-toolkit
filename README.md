# toolkit

The unified agent OS backend — a single Go MCP server exposing four meta-tools
(`work`, `knowledge`, `measure`, `admin`) over stdio, plus an HTTP observe API,
backed by a shared SQLite database scoped per project.

Split out of the `mcp-servers` monorepo (2026-06, chain
`auto-startup-dev-services`). The React/Vite dashboard now lives in the sibling
`sophdn/corpos-toolkit-dashboard` repo and reaches this server over the HTTP API;
the llama-server inference backend lives in `sophdn/llama-server`.

## Stack

Go only. The build is CGO-free (modernc SQLite; the optional ONNX `ml` path is
CGO-gated), so `CGO_ENABLED=0` yields a static binary — see
`deploy/toolkit-server/Containerfile`. Schema migrations are embedded from
`go/internal/db/migrations/`.

## Build

```bash
make -C go build          # → go/bin/toolkit-server
```

The Go module is rooted at `go/go.mod`; drive it through `make -C go <target>`
(`build`, `test`, `vet`, `build-all`, `vuln`).

## Start / stop

### Native (dev)

Stdio MCP — how a Claude Code session mounts the server (via the project's
`.mcp.json`; see [INTEGRATION_GUIDE.md](INTEGRATION_GUIDE.md)):

```bash
go/bin/toolkit-server --db <path>/toolkit.db --default-project <project>
```

HTTP observe daemon — what the dashboard talks to (port 3000 by default):

```bash
HTTP_PORT=3000 TOOLKIT_DB=<path>/toolkit.db go/launch.sh   # start (foreground)
```

Stop the daemon with Ctrl-C, or:

```bash
pgrep -af toolkit-server      # find the pid
kill <pid>                    # stop it
```

`go/launch.sh` runs `--http-only` (no stdio MCP). Override the DB with
`TOOLKIT_DB` and the port with `HTTP_PORT`; it pre-flights the port and refuses
to start if it's already bound.

### Container (rootless Podman + Quadlet)

The server ships a Containerfile plus Quadlet units under `deploy/`. Install the
units once, then control the service with systemd `--user`:

```bash
bash scripts/install-quadlet-units.sh      # → ~/.config/containers/systemd/
systemctl --user start  toolkit-server     # start
systemctl --user stop   toolkit-server     # stop
systemctl --user status toolkit-server
```

See [`deploy/quadlet/README.md`](deploy/quadlet/README.md) for the image-build,
digest-pinning, and boot-start (linger) details.

### Proxy (container owns the canonical DB)

The end-state has the **container** own the canonical ledger, with each session
reaching it through a thin **stdio→HTTP proxy** (`go/cmd/toolkit-proxy`) instead
of opening the DB file directly. The proxy holds no DB; it forwards every MCP
call to the container's `POST /mcp/<surface>`.

```bash
bash scripts/install-proxy.sh                       # build + install ~/.local/bin/toolkit-proxy

bash scripts/cutover-canonical-db.sh check          # readiness report — NO changes
bash scripts/cutover-canonical-db.sh flip --yes     # perform the cutover (run from a NON-toolkit shell)
bash scripts/cutover-canonical-db.sh rollback --yes # revert to native :3000 + stdio
```

The proxy `.mcp.json` command form:

```json
{ "mcpServers": { "toolkit-server": {
  "command": "/home/<you>/.local/bin/toolkit-proxy",
  "args": ["--default-project", "<project>", "--http-base", "http://localhost:3001"]
} } }
```

The native binary path is **retained as a fallback** — `rollback` restores it
in one step. See [`CLAUDE.md`](CLAUDE.md) §"DB ownership" for the full model.

## Quality gate

`scripts/precommit.sh` is the single gate: CONVENTIONS layout integrity →
forbidden-pattern guards → migration + event-schema sync → Go
fmt / vet / golangci-lint / build-all / test → dependency vulnerability scan.
Wire it as the local pre-commit hook (and the post-commit / post-merge deploy
advisors) once after cloning:

```bash
bash scripts/install-hooks.sh
```

The same script runs in Gitea CI ([`.gitea/workflows/ci.yaml`](.gitea/workflows/ci.yaml)),
so local and CI enforcement can't drift.

## Layout

| Path | What |
|------|------|
| `go/` | Go module: `cmd/toolkit-server/` (the binary), `internal/` (work, knowledge, measure, admin, forge, rubric, inference, eventbus, observehttp, mcp, db, dispatch) |
| `blueprints/forge-schemas/` | Canonical TOML schema definitions (chain, task, bug, …); rubric definitions in `blueprints/rubrics/` |
| `action-manifests/` | MCP action documentation (one file per dispatched action) |
| `clients/` | Harness-facing reference client libraries that consume the MCP surfaces |
| `measure/` | Measure-surface assets (rubric corpora) |
| `deploy/` | Containerfile + Quadlet units for the rootless-Podman deployment |
| `docs/` | Internal references, including `docs/ACTION_REFERENCE.md` |
| `scripts/precommit.sh` | Quality gate (Go fmt + vet + golangci-lint + build + test + vuln) |

The canonical SQLite database (`data/toolkit.db`, shared across every integrated
project) is not committed; point the server at it with `--db` / `TOOLKIT_DB`.

## Where to read next

- **Discovery entry point:** [CODEMAP.md](CODEMAP.md) — auto-generated index of meta-tool actions, forge schemas, Go package intended-use blocks, and scripts. Regenerated by the precommit gate.
- **Internals & conventions:** [CONVENTIONS.md](CONVENTIONS.md) — file layout, error shape, handler organisation, migration strategy.
- **Mounting in a sibling project:** [INTEGRATION_GUIDE.md](INTEGRATION_GUIDE.md) — `.mcp.json` template, multi-project scoping, skill loading.
- **MCP action signatures:** [docs/ACTION_REFERENCE.md](docs/ACTION_REFERENCE.md) — every action, its params, its return shape.
