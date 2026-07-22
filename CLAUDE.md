# corpos-toolkit (sophdn/corpos-toolkit) — agent conventions

This is the standalone toolkit backend repo, split out of the `mcp-servers`
monorepo (2026-06, chain `auto-startup-dev-services` T2) and renamed
`sophdn/toolkit` → `sophdn/corpos-toolkit` (finish-sophdn-repo-split T9, 2026-06-09;
local `~/dev/corpos-toolkit`, ledger project `corpos-toolkit`). The frontend lives in
`sophdn/corpos-toolkit-dashboard` and the inference service in `sophdn/llama-server`;
they reach this backend over its HTTP API, not a shared tree.

## Working directory

Claude Code has no native per-project `cwd` setting. When a session opens in or descends from this repo, the harness's "Primary working directory" may still be `~` (the launch dir), forcing every `Bash` command to prefix the full repo path — wasted tokens AND an active source of bugs (a `make -C go build` from `~` silently uses a stale binary).

**First action of any session that touches this repo:** `cd` into this repo's checkout root (the directory holding `go/`, `CONVENTIONS.md`, etc.). After that, every `Bash` command in this conversation can use repo-relative paths: `make -C go build`, `git log -3`, etc. Don't bake a literal `/home/user/...` form into commands; the agent's home expansion handles it.

**Worktree-subagent exception (closes `spawned-subagents-in-worktree-commit-to-main-checkout-not-worktree`).** The reflex above means "the checkout root *this session is operating in*" — NOT a hardcoded main checkout. Two cases need care: (1) if your session is inside a linked worktree, `cd` to the **worktree** root, not the main checkout. (2) When you **spawn a subagent from inside a worktree**, the subagent reads this CLAUDE.md and will dutifully `cd` to the checkout root — and unless told otherwise, that resolves to the MAIN checkout, so it commits to `main` instead of your worktree branch (silently defeating worktree isolation). When spawning a subagent from a worktree, **pin its working directory to the worktree path in its prompt and explicitly forbid it from `cd`-ing to the main checkout.** See `docs/MULTI_AGENT_WORKTREE_WORKFLOW.md` §0.3 for the prompt template + post-spawn verification.

**Go commands need an extra hop.** The Go module is rooted at `go/go.mod`, not the repo root. Bare `go test ./go/internal/...` from the workspace root fails with `directory prefix ... does not contain main module`. From the repo root, drive Go through `make -C go <target>` (preferred — matches the precommit-gate convention), or `cd go && go test ./internal/...` for ad-hoc invocations. Make and git work bare from the repo root; only direct `go` invocations need the carve-out.

The repo is single-language Go (post-rust-retirement-and-db-hardening T6/T7, 2026-05-22). Historical Rust source was archived in the monorepo and dropped from this split — see `CONVENTIONS.md` for the layer history.

## Pre-commit gate

`scripts/precommit.sh` is the single entrypoint. It chains: CONVENTIONS layout integrity → forbidden-pattern guards (retired CRUD refs, log-discipline) → migration sync (canonical → testutil mirror) → **`corpos-gate run --tier=pre-push`** (the Go CHECK stages + 3 read-only custom guards) → chain forge byte-identity replay. Run it before every commit; the `.git/hooks/pre-commit` symlink wires it as the gate. (The dashboard CSS-token + TypeScript stages moved to `sophdn/corpos-toolkit-dashboard` when the frontend split out — this gate is Go-only.)

**corpos-gate drives the Go checks (chain 434 dogfood cutover).** The pure read-only Go CHECK stages — gofmt-drift, `go vet`, golangci-lint (forbidigo any-rule), build-all, and the cover-floor test+coverage gate (66% floor) + the vuln scan — plus three custom guards (`quadlet-directives`, `cmd-gitignore-parity`, `runtime-affecting-paths-parity`) now run through the stack-agnostic gate orchestrator (`go/cmd/corpos-gate` + `go/internal/gate`, config at `gate.yml` in the repo root), NOT inline shell stages. precommit.sh builds the binary (`make -C go corpos-gate` → `go/bin/corpos-gate`, gitignored) and invokes `corpos-gate run --tier=pre-push`, scrubbing the git env (bug 921/937) exactly as the old cover-floor stage did since corpos-gate runs the same hermetic-test-spawning suite. The **mutating / restaging / specialized scaffolding** stays inline in precommit.sh: gofmt+ASCII-quote normalization (must run BEFORE corpos-gate so the tree is gofmt-clean), migration + event-schema sync, codemap regen, the stash/trap partial-staging machinery, and `make -C go replay-verify`. **Vuln bypass:** `TOOLKIT_PRECOMMIT_SKIP_VULN=1` (also `true`/`yes`) is preserved — precommit.sh threads it through as `corpos-gate run … --skip=vuln`. The lint check runs `golangci-lint config verify` (bug 1315 guard) before `golangci-lint run ./...`, preserving the retired inline stage's behavior exactly.

**Orphan-stash auto-reap (opt-in).** The gate's orphan-stash check (bug 1425) WARNs when `precommit-fmt-<PID>-<label>` stashes accumulate from dead PIDs. Setting `TOOLKIT_PRECOMMIT_AUTOREAP_EMPTY_ORPHANS=1` (also accepts `true`/`yes`/`on`) auto-drops the EMPTY (no-op format-only) orphans after the warning prints. Non-empty orphans are NEVER auto-dropped — they may hold recoverable work per bug 1425's recovery flow. Default is OFF so users who haven't read this section aren't surprised by silent stash deletions.

### Post-commit advisor: stdio-session preservation

After the commit lands, the advisor (run from the post-commit hook) rebuilds the toolkit-server binary, runs a smoke harness against a fresh DB, then restarts the HTTP daemon. **Stdio MCP processes attached to live `claude` sessions are deliberately preserved** — killing them mid-session would drop in-flight tool calls.

Effect: a session that just committed a behavior-affecting change to the MCP server is **still talking to the old binary** until it voluntarily reconnects. New sessions and the HTTP daemon pick up the new binary immediately; only the in-flight stdio sessions lag.

To pick up the new binary in the current session: `/mcp reconnect`. The advisor's output makes this explicit ("the active session continues on the OLD binary until voluntary restart") but it's easy to miss when scanning a successful commit.

To force-kill the stdio process anyway (CI runners with no live session, or explicit override): `TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL=1 git commit …`. Don't use this with a live session — it drops in-flight calls.

If you commit a fix and then verify it from the same session, **`/mcp reconnect` first**. Otherwise your verification calls run against pre-fix behavior, and any "the fix didn't take" diagnosis is downstream of binary staleness.

**Agent-side staleness check.** There is no proactive in-session signal that the stdio binary has drifted from the deployed one — the preservation behavior is deliberate. When an agent needs to know whether the live MCP reflects a recent Go-touching commit, the ritual is: call `admin.server_version` (returns the running binary's build-time SHA) and compare against `git rev-parse HEAD`. If they differ, either `/mcp reconnect` before exercising the new code, or rely on the test suite + the post-commit advisor's smoke for verification. Closes bug `no-in-session-signal-when-stdio-binary-is-stale-vs-deployed`.

**Merges also fire the advisor (`post-merge` hook).** git runs the `post-commit` hook for `git commit` but **not** for the merge commit a `git merge`/`git pull` creates — so a chain branch merged to main (the primary way work lands) historically deployed nothing until a manual `make -C go build`. The `post-merge` hook (`.git-hooks/post-merge`, installed by `scripts/install-hooks.sh` alongside `pre-commit` + `post-commit`) closes that gap: it re-runs the advisor over `ORIG_HEAD..HEAD` (the full merged-in change set, correct for both `--no-ff` and fast-forward merges), so a merge rebuilds + restarts `:3000` exactly like a normal commit. Closes bug `git-merge-to-main-skips-post-commit-advisor-deploy`. (`scripts/worktree-merge.sh` already builds via its own gate; this covers the plain-`git merge` path too.) Note: merge commits skip the `pre-commit` gate by git design — run the gate on the branch before merging, or use `worktree-merge.sh` which gates the merged result.

**Post-flip (T7 container-canonical) disposition.** Once the cutover in §"DB ownership" lands, the *live* canonical surface is the **container**, advanced only by an explicit image rebuild (`scripts/build-toolkit-image.sh && systemctl --user restart toolkit-server`) — that lag IS the image-lags-HEAD boundary. The advisor described above keeps rebuilding the **native** binary, but only as the retained fallback; it no longer drives the canonical path. And the stdio-session-preservation hack above (the SIGHUP re-exec, `/mcp reconnect`, `TOOLKIT_PRECOMMIT_FORCE_STDIO_KILL`) becomes moot for proxied sessions: a session running the `toolkit-proxy` holds **no** Go binary to preserve — the proxy is behavior-free and the container advances out-of-band. The hack is retained solely for the native fallback path.

### Worktree & concurrent-agent workflow

Two traps bite agents working in a linked worktree or alongside another agent, both with documented paths in **`docs/MULTI_AGENT_WORKTREE_WORKFLOW.md`**:

- **A Go change committed from a linked worktree does NOT deploy.** The advisor builds into the worktree's `go/bin`, but the stdio MCP + HTTP daemon load the MAIN checkout's `go/bin/toolkit-server`. To go live: land the commit on main, `make -C go build` in the main checkout, `/mcp reconnect`. The post-commit advisor warns at commit time ("built in a LINKED WORKTREE …") and the SessionStart staleness hook flags it DIVERGENT.
- **To preview an unmerged branch's HTTP surface** without disturbing the shared `:3000` daemon, run a private view: `HTTP_PORT=<port> TOOLKIT_DB=/tmp/toolkit-<branch>.db go/launch.sh` serves your checkout's binary on `<port>`. Point a `sophdn/corpos-toolkit-dashboard` dev server at it via `VITE_API_BASE_URL=http://localhost:<port>` (the dashboard is a separate repo now). `:3000` stays the canonical default.

## DB ownership: container owns the canonical DB via a stdio→HTTP proxy (T7)

The end-state of chain `auto-startup-dev-services` (T7) is: the **toolkit container is the single owner of the canonical SQLite ledger** (`~/.local/share/toolkit/data/toolkit.db` — relocated 2026-06-09 out of the now-archived `mcp-servers` working tree by finish-sophdn-repo-split T6; service-named so it survives the repo rename), and every Claude session reaches it through a thin **stdio→HTTP proxy** instead of opening the file itself.

**Why.** Historically each session's `.mcp.json` ran the native binary with `--db <canonical>`, so N sessions opened the canonical file directly — safe only because every opener was a same-host process under POSIX locks. Making the *container* the canonical owner while sessions still stdio-opened the host file would be the cross-mount-namespace WAL hazard the deploy chain deliberately avoided. So "container owns the DB" requires the per-session access path to stop opening the file.

**The pieces (this repo):**
- `go/cmd/toolkit-proxy` — the replacement `.mcp.json` command. Speaks MCP stdio to Claude Code, registers the same seven surface meta-tools (descriptions + schema imported from `internal/actiondocs` + `internal/dispatch` so they can't drift), and forwards every call to the container's `POST /mcp/<surface>`. It **opens no DB**, runs no migrations, starts no jobs. Per-session attribution (`X-MCP-Actor`, `X-MCP-Session`) and the per-session default project (`X-MCP-Default-Project`) ride as HTTP headers; `internal/observehttp/mcp_dispatch.go` stamps them back onto the dispatch context (absent headers → prior behavior, so the existing shell-hook callers are unaffected). Per-call rationale rides in the JSON body.
- `deploy/quadlet/toolkit-server-canonical.container` — the flip-target unit: binds the canonical host dir at `/data` with `UserNS=keep-id:uid=65532,gid=65532` (the distroless image runs as nonroot/65532; keep-id maps that back to the host user so the nonroot process reads **and writes** the host file).
- `scripts/install-proxy.sh` — builds + installs the proxy binary (safe anytime; touches no DB). Stamps the build SHA via `-ldflags -X` (mirrors `go/Makefile`), so the installed proxy reports `toolkit-proxy -version`. The proxy is a SPOF every `.mcp.json` depends on, and the post-commit advisor rebuilds the *server*, NOT the proxy — so after a commit touching `cmd/toolkit-proxy` / `internal/dispatch` / `internal/actiondocs` the advisor prints a refresh reminder, and `scripts/check-proxy-staleness.sh` (pre-flight; self-tested by `test-proxy-staleness.sh`) confirms installed-vs-source drift on demand. Fallback for a stale/missing proxy is always: re-run `scripts/install-proxy.sh`.
- `scripts/cutover-canonical-db.sh` — `check | flip --yes | rollback --yes | migrate-configs --yes | restore-configs --yes`. The flip stops the separate-volume container + native `:3000`, **asserts single-writer** (aborts if anything still holds the file), installs the canonical-bind quadlet, atomically swaps **every** toolkit-server MCP config to the proxy — the `~/dev/*/.mcp.json` fleet, `~/dev/.mcp.json` one level up, AND the global `mcpServers.toolkit-server` block in `~/.claude.json` (which governs any session rooted outside a fleet project dir; bug 986) — and starts the container. Rollback restores the quadlet, all those config backups, and the native `:3000` daemon. `check` reports global/non-fleet coverage so the gap is visible pre-flip; `migrate-configs`/`restore-configs` swap or undo just the config layer (e.g. to remediate a global `~/.claude.json` an earlier flip missed) without re-running the whole cutover.

**Operational notes.** The flip must be run from a shell that is **not** a toolkit-mounted session (it would hold the DB open and trip the single-writer guard — by design). The cutover takes effect for **new** sessions; you cannot retire the native path from inside a session that depends on it. The **native binary path + the `tks-data` separate volume are retained as a proven fallback** — rollback is one script away. The **dashboard** now reads the container's HTTP surface on `:3001` rather than the native `:3000`; its same-repo daemon-staleness banner (a monorepo-era signal) is superseded by the `/version` build-SHA contract.

## Migrations

Schema migrations live in **two** locations that must stay in sync — `go/internal/db/migrations/` (canonical, Go binary embed) and `go/internal/testutil/migrations/` (testutil hermetic fixture; real copy because Go embed rejects symlinks). The precommit gate syncs canonical→mirror inline. Pre-T6 had a third canonical location at `crates/shared-db/migrations/`; that's gone with the Rust workspace. See CONVENTIONS.md §"Migration runner ownership" for the rationale.

## Skills: this repo is canonical for the toolkit-core disciplines

`skills/` is the **canonical** home for the agent disciplines that can't be
followed without toolkit-server's own surfaces (filing, vault, content-routing,
parse-context, deterministic-extraction). The full 43-skill ownership table +
the decision rule that classifies a new skill live in `docs/SKILLS_OWNERSHIP.md`
(chain 444). `~/.claude/skills` is a downstream overlay; corpos owns everything
else.

**corpos embeds a committed mirror of these.** Because `//go:embed` needs the
files at build time and rejects symlinks (the same reason the migrations mirror
is a real copy), corpos keeps real copies under `internal/skills/library/`.
Keep them in sync:

- Edit the **canonical** copy here (`skills/<name>/SKILL.md`), never corpos's
  mirror.
- Run `scripts/sync-skills-to-corpos.sh` (`CORPOS_DIR=` to target a worktree) —
  it copies canonical → corpos, stripping the `pii-allow` markers this repo uses
  to keep real infra strings past `pii-scan` for its public mirror (corpos has
  no public mirror and shouldn't carry the markers into prompts). Copy-only;
  corpos-owned `library`/`userlib` skills are never touched.
- The `skills-embed-drift` gate (`gate.yml` custom guard →
  `scripts/check-skills-embed-drift.sh`) fails the pre-commit, naming the drifted
  skill, if the canonical and the mirror disagree. It's a **dev-machine** check:
  no sibling corpos checkout (CI / container) → loud skip. Then commit the corpos
  side via corpos's worktree workflow.

## Task-tracking ownership

Task state on this repo is owned by `mcp__toolkit-server__work` — chains, tasks, bugs, roadmap, with project scoping + event emission + rationale envelopes. That MCP surface is the canonical task tracker for every chain-driven session here.
