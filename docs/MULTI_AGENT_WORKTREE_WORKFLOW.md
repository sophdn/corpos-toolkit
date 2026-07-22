# Multi-agent & worktree workflow

How concurrent branch-agents deploy Go changes and view their own work without
fighting over the single shared toolkit-server. Two distinct problems, two
documented paths.

Background memory: `vault/memory/feedback/worktree-agents-pattern.md`,
`vault/decisions/2026-05-21_parallel-worktree-agents-and-bare-config-friction.md`.

---

## 0. Step-0 caution: after EnterWorktree, captured absolute paths still point at the MAIN checkout

Closes bug `worktree-cwd-vs-captured-absolute-paths-silently-edits-main-checkout`.

### The trap

`EnterWorktree` switches the session CWD to `.claude/worktrees/<branch>/`, but it
does **not** rebase absolute paths a tool already holds. The recurring failure
flow:

1. You explore the repo first — `Read`/`Grep` against main-checkout absolute paths
   (`/home/user/dev/mcp-servers/go/internal/...`), capturing those paths.
2. You `EnterWorktree` — CWD is now the linked worktree.
3. You `Edit`/`Write` reusing the **captured main-checkout absolute paths**.

The edits land in the **MAIN checkout** (on whatever branch it has checked out,
usually `main`), **not** in the worktree branch — and **no error surfaces**; the
writes "succeed" against the wrong tree. A `cd` into the worktree from `Bash` does
not help: it doesn't rebase a tool call's absolute-path argument. This is the
exact cross-checkout-contamination class the worktree flow exists to prevent —
a subagent that explored before its worktree existed, or that mixes
main-relative and worktree-relative paths, silently dirties the wrong checkout.

### The discipline (until a harness guard exists)

- **After `EnterWorktree`, re-derive every path from the new CWD.** Never reuse an
  absolute path captured during pre-worktree exploration for a write. Prefer
  worktree-relative paths, or paths explicitly rooted at
  `.claude/worktrees/<branch>/…`.
- **Verify where a write landed** when in doubt: `git -C <worktree> status` should
  show your change; `git -C ~/dev/mcp-servers status` (the main checkout) should
  **not**. If the main checkout is unexpectedly dirty, you hit this trap.
- **Recovery** (if edits landed in main): copy the affected files into the
  worktree, then `git restore` / `git rm` them from the main checkout to return it
  to clean. Re-apply in the worktree if needed.

A harness-level guard (warn when an `Edit`/`Write` targets a path outside the
active worktree) would make this structural rather than disciplinary; that lives
above this repo (the tool layer), so this caution is the in-repo mitigation.

### The subagent variant: a spawned subagent commits to the MAIN checkout

A SPAWNED subagent (via the Agent tool) does **not** inherit the parent's
`EnterWorktree` state the way you'd hope — and it reads this repo's `CLAUDE.md`,
whose **first instruction is `cd ~/dev/mcp-servers`** (the MAIN checkout). A
subagent that dutifully follows that reflex will edit + **commit to `main`**,
not the parent's worktree branch — silently defeating worktree isolation and any
"don't touch main" constraint. (Observed 2026-05-27 during the record live test:
a subagent's guard fix landed on local `main`; caught because the commit's parent
was `origin/main` and the worktree tree was clean. Remediated by cherry-picking
onto the worktree branch + `git reset --hard origin/main`.)

**Discipline when spawning a subagent from inside a worktree:**

- **Pin it in the prompt.** State the absolute worktree path as its working
  directory and explicitly tell it **NOT to `cd ~/dev/mcp-servers`** — the
  CLAUDE.md `cd`-to-main reflex is for top-level sessions, not worktree-spawned
  subagents.
- **Verify after it returns:** `git -C ~/dev/mcp-servers log --oneline origin/main..HEAD`
  should be empty. If the subagent committed to main, cherry-pick the commit(s)
  onto the worktree branch, then `git -C ~/dev/mcp-servers reset --hard origin/main`.

**Prompt template — paste the worktree pin into every subagent prompt:**

```
Your working directory is the worktree at <ABSOLUTE_WORKTREE_PATH>. Run
`cd <ABSOLUTE_WORKTREE_PATH>` as your FIRST action and stay there. Do NOT
`cd` to the main checkout — the project CLAUDE.md's cd-to-checkout-root
reflex does NOT apply to you; it would land your commits on `main` instead
of this worktree's branch. All edits, builds, and commits happen in the
worktree. Commit on the worktree's current branch only.
```

This is also noted at the project `CLAUDE.md` cd-reflex (the worktree-subagent
exception). Closes suggestion
`spawned-subagents-in-worktree-commit-to-main-checkout-not-worktree`.

---

## 1. Deploying a Go change made in a linked worktree

Closes bug `worktree-commits-dont-deploy-to-stdio-binary-path-no-staleness-signal`.

### The trap

`.mcp.json`'s stdio command and `go/launch.sh` both load **the MAIN checkout's**
binary: `~/dev/corpos-toolkit/go/bin/toolkit-server`. But the post-commit advisor
runs `make -C go build` in the **committing** checkout's directory — so a commit
made from a *linked worktree* builds into that **worktree's** `go/bin`, which no
running service loads.

Consequence: a worktree agent's Go change is **never live** in their own stdio
session via the normal `commit → /mcp reconnect` flow — `/mcp reconnect`
re-execs the main-checkout binary, which is whatever branch the main checkout
last built. Under concurrent multi-agent work the deployed binary is a
last-writer-wins race between each agent's post-commit advisor.

### The signals (both already wired)

- **At commit time:** the post-commit advisor prints a `built in a LINKED
  WORKTREE …` warning naming the main checkout's deployed binary path when the
  rebuild happened in a linked worktree (`scripts/post-commit-restart-advisor.sh`).
- **At session start:** the `toolkit-binary-staleness-check.sh` SessionStart hook
  compares the deployed binary's `gitSHA` against the session checkout's HEAD and
  emits a **DIVERGENT** warning for the worktree case (deployed SHA is not an
  ancestor of HEAD). Activation is per-user (install + settings merge — see
  `scripts/hooks/README.md`).

### The deploy path

To get a worktree's committed Go change **live**:

1. Land the commit on **main** — merge or `git cherry-pick <sha>` onto the main
   checkout's branch.
2. In the **main checkout** (`~/dev/mcp-servers`), rebuild: `make -C go build`.
   (When the commit lands on main *in the main checkout*, its own post-commit
   advisor does this for you.)
3. `/mcp reconnect` in each live session to re-exec the freshly built binary.
   New sessions + the HTTP daemon pick it up automatically.

**Verifying a worktree change WITHOUT deploying:** run the tests directly —
`make -C go test` (or `cd go && go test -tags sqlite_fts5 ./internal/<pkg>/`).
Do **not** rely on the live MCP for verification from a worktree; it serves the
main-checkout binary, not your branch's. (See
`vault/.../reconnect-after-migration-never-workaround`.)

---

## 2. Viewing your branch's work on the dashboard (without fighting :3000)

Closes bug
`shared-http-daemon-singleton-blocks-concurrent-branch-agents-from-viewing-own-work`.

### The trap

The dashboard backend is a **single shared HTTP daemon on :3000** serving exactly
one toolkit-server binary. During concurrent branch work, each agent's
post-commit advisor restarts :3000 to *its* binary, so :3000 flip-flops between
branches; and an agent on an unmerged branch can't show its work without
restarting the shared daemon (which the auto-mode classifier correctly blocks —
it would disrupt the other agent + the user's pending `/mcp` reconnect).

### The isolation path (verified)

`go/launch.sh` already honours `HTTP_PORT` and `TOOLKIT_DB`, and serves **the
launching checkout's own** `go/bin/toolkit-server`. The dashboard reads
`VITE_API_BASE_URL` (`apps/dashboard/src/lib/http.ts`, default
`http://localhost:3000`). Combine them to run a private per-branch view that
leaves the canonical :3000 daemon untouched:

```bash
# 1. From YOUR checkout/worktree, build + launch your binary on a private port
#    against a throwaway DB copy (avoids sqlite write contention with :3000).
make -C go build
cp data/toolkit.db /tmp/toolkit-<branch>.db
HTTP_PORT=3999 TOOLKIT_DB=/tmp/toolkit-<branch>.db go/launch.sh
#    → "observe HTTP listening addr=:3999"; serves THIS checkout's binary.

# 2. Point a dashboard dev server at that port (apps/dashboard/):
VITE_API_BASE_URL=http://localhost:3999 npm run dev
```

Two agents each pick a distinct port (3999, 3998, …) and view their respective
branches simultaneously; :3000 stays the canonical single-agent default.

Notes:
- `launch.sh` refuses to start if the chosen port is already bound and prints the
  holder — pick another port.
- The DB **copy** is the safe default (full isolation). If you instead point at
  the canonical `data/toolkit.db` to see live work-state with your branch's
  binary, be aware a second daemon's startup vault-integrity sweep may write —
  prefer a copy unless you specifically need live data.
- This is an **additive** path for concurrent branch work, not a replacement for
  the shared :3000 daemon.

---

## 3. Shared-DB concurrency model under parallel worktree agents

Chain `worktree-multi-agent-orchestration-support` T1 (the gating decision).
Routes the `concurrent-writer-model-for-shared-toolkit-db-under-multi-agent-worktrees`
suggestion; supersedes the ad-hoc symptoms bugs 932 (PK-reassign) + 934
(duplicate-create overwrite).

### The trap

Every worktree, the :3000 daemon, and each stdio session can open the **same**
`data/toolkit.db` (work-tracker, events ledger, projections). The substrate is
event-sourced: a `forge` runs `events.Emit` (INSERT event + fold projections) in
one `pool.WithWrite` transaction, and folds assign projection primary keys with
`SELECT COALESCE(MAX(id),0)+1`. `Pool.WithWrite` serializes writes through an
**in-process** mutex — which does nothing across *processes*. So when two
toolkit-server processes write the shared DB at once, the danger is twofold:

- **Lost forges.** Without a busy timeout, the second writer hits the WAL
  single-writer lock and fails immediately with `SQLITE_BUSY` — the agent's
  forge errors out.
- **PK collision / fold race.** Two deferred transactions can each take a read
  snapshot, both read the same `MAX(id)`, and assign colliding primary keys (or
  fail to upgrade reader→writer with `SQLITE_BUSY_SNAPSHOT`).

### The model (option **c** — genuinely concurrency-safe writes)

The chosen model makes the shared DB safe for concurrent cross-process writers
rather than forbidding them. Three pieces, all landed in T1:

1. **`_busy_timeout=5000` on the connection DSN** (`db.Open`) — a contending
   writer *waits* up to 5s for the lock instead of failing. Cross-process write
   contention becomes serialization-by-waiting.
2. **`_txlock=immediate` on the connection DSN** — every `WithWrite` transaction
   issues `BEGIN IMMEDIATE`, taking the write lock at transaction *start*, before
   the fold's read-then-write `MAX(id)+1`. Each writer therefore observes a
   post-commit snapshot, so the next id is collision-free across processes. (The
   only `BeginTx` caller is `WithWrite`; reads go through `Pool.DB()` in
   autocommit and are unaffected — WAL lets them proceed without blocking.)
3. **PK-stable folds** — the `chain`/`task`/`bug`/`suggestion` Created folds no
   longer carry `id = excluded.id` in their `ON CONFLICT … DO UPDATE`. A re-fired
   Created event refreshes content only; the primary key is stable for the row's
   lifetime (referrers — child tasks, blockers, commit stamps — depend on it).

The regression + stress pins live in
`go/internal/projections/concurrent_writer_test.go`:
`TestConcurrentWriters_SharedDB` opens N independent `*db.Pool`s (each a separate
process from SQLite's lock view) on one file and has them forge + complete tasks
concurrently, asserting zero errors, no lost writes, unique PKs, and consistent
chain_status counters; `TestFold_DuplicateCreatePreservesPK` pins the PK-stability
fold fix for task/bug/suggestion.

### Recommended discipline (option **a**, complementary)

The model above is the hard safety floor — concurrent forges are now *correct*,
not *forbidden*. For **ordering-sensitive** multi-agent work (e.g. several
subagents contributing to one chain where task positions or sequencing matter),
still prefer **orchestrator-mediated forges**: subagents report results and the
orchestrator forges, so ordering is deterministic. Reserve direct concurrent
forges for disjoint entities (each agent owns its own slugs).

### Caveat: migrations from one process

`db.Open` applies pending migrations. Two processes opening a DB with the *same*
new migration pending can still race (one applies it, the other re-attempts and
trips "table already exists"). Apply migrations from a single process — in
practice the main checkout's daemon, which opens first. A worktree's ephemeral
server (the per-worktree MCP path, §5) runs against an **already-migrated**
shared DB or its own private copy, so it never races schema creation.

---

## 4. Bootstrapping a fresh worktree (do this first)

Closes bug 938 (no worktree-setup helper) and routes the advisor opt-out from
bug 936. A fresh `git worktree add` carries only tracked files, so two things
are missing before it can run the full gate and commit cleanly: `node_modules`
(the dashboard CSS-token + tsc precommit stages need it) and a commit path that
runs the quality gate **without** the post-commit advisor (which otherwise
restarts the shared :3000 daemon with the worktree's binary — §1, bug 936).

### Step 0: run the bootstrap

```bash
cd <your-worktree>
./scripts/worktree-setup.sh
```

Idempotent, and it never touches the main checkout's state. It:

1. **Symlinks `apps/dashboard/node_modules`** from the main checkout. The
   worktree shares the base commit's `package-lock`, so the link is
   version-correct; a symlink (not a copy) avoids duplication and version skew.
2. **Installs a gate-only hooks dir** in the worktree's private git dir
   (`pre-commit → scripts/precommit.sh`, **no** post-commit advisor) and points
   *this worktree's* `core.hooksPath` at it via per-worktree config
   (`extensions.worktreeConfig` + `git config --worktree`). The main checkout's
   shared `core.hooksPath` is left untouched.

After that, a plain `git commit` in the worktree runs the **full pre-commit
quality gate** and stops there — no advisor rebuild, smoke, or :3000 restart.

### The post-commit advisor in a worktree (bug 936)

Even without the gate-only hooks, the advisor is now worktree-safe: from a
**linked worktree** it **skips the :3000 daemon restart by default** (it detects
the linked-worktree case and prints the private-port guidance from §2 instead of
hijacking the shared daemon). Two knobs:

- `TOOLKIT_PRECOMMIT_NO_DAEMON_RESTART=1` — skip the :3000 restart for **any**
  checkout (e.g. a main-checkout commit that shouldn't recycle the shared
  daemon). Honored everywhere.
- The main checkout's default is **unchanged**: it still restarts :3000 so the
  canonical daemon tracks `main`.

The gate-only hooks path (step 0) and the advisor's own linked-worktree skip are
complementary: the hooks path avoids running the advisor at all, while the
default skip protects the case where the shared hooks still fire.

---

## 5. Self-verifying a worktree's MCP surface (live, isolated)

Routes the `per-worktree-live-mcp-server-for-subagent-branch-correct-self-verification`
suggestion (chain T4). Pairs with §1 and the bug-936 fix.

### The trap

The stdio MCP and the :3000 daemon both load the **main checkout's** binary
(§1), so a worktree's branch-correct MCP surface is never live to a session —
only `make -C go test` exercises it. A subagent that changed a work-surface
handler can't call it end to end to confirm the wiring.

### The tooled path

`scripts/worktree-mcp.sh` builds **this worktree's** binary and launches it on
an ephemeral HTTP port against a **private** database, exposing the full MCP
surface over `POST /mcp/{surface}`:

```bash
# In the worktree; run it in the background (it blocks serving):
./scripts/worktree-mcp.sh           # private snapshot of the shared DB
./scripts/worktree-mcp.sh --fresh   # empty auto-migrated DB
PORT=3994 ./scripts/worktree-mcp.sh # pin the port (else auto-probed 3990-3999)
```

Then exercise the branch's own handlers live — the response is the typed
handler result, exactly as the stdio MCP would return it:

```bash
curl -sS -XPOST localhost:<port>/mcp/work -H 'content-type: application/json' \
     -d '{"action":"chain_find","params":{"query":"my-chain"}}'
```

Surfaces: `work` / `knowledge` / `measure` / `admin` / `ml`. Confirm the binary
is the branch's via `GET /version` (its `git_sha` is the worktree's HEAD, not
the main checkout's).

### Isolation guarantees

- **Never :3000.** The port is auto-probed in 3990-3999 (or your `PORT`); the
  script refuses `PORT=3000`.
- **Never the shared DB.** The DB is a private `/tmp/toolkit-worktree-<branch>.db`
  — a point-in-time snapshot (WAL-checkpointed before copy) or `--fresh` empty.
  The canonical `data/toolkit.db` is only read once for the snapshot, never
  opened by the server.
- Branch-correct blueprints/rubrics: the server reads the worktree's
  `blueprints/` so a schema/rubric change on the branch is live too.

Tear down with Ctrl-C (or kill the process group); the private DB is reaped on
the next launch for the same branch.

---

## 6. Merging parallel agents back (the canonical path)

CAPSTONE of chain worktree-multi-agent-orchestration-support (T8). This is the
canonical multi-agent merge-back path — it codifies the
spawn → conflict-check → merge-back → cleanup ritual that was previously a
hand-run sequence of `git config core.bare false`, `git merge --ff-only`,
`git merge --no-ff`, gate, and `git worktree remove`.

### The helper

```bash
# From the integration target (normally the main checkout):
scripts/worktree-merge.sh agent-1 agent-2 …        # merge + gate + reap
scripts/worktree-merge.sh --check-only agent-1 …   # pre-merge dry-run only
scripts/worktree-merge.sh --no-gate --no-reap …    # merge only
```

It performs, with no manual babysitting:

1. **core.bare auto-reset.** The Agent tool's `isolation: "worktree"` flips
   `core.bare=true` on the shared config; left flipped, the main checkout can't
   even run `git rev-parse --show-toplevel`. The helper resets it first, before
   any work-tree-requiring command. Idempotent — safe to re-run each session.
2. **Conflict-surface check.** Reports files modified by 2+ branches and
   duplicate migration NNN numbers. With `--check-only` it stops here (exit 1 if
   conflicts) — the pre-merge dry-run. The non-check path refuses to auto-merge
   when the surface is non-empty. (T3 makes migration collisions improbable and
   T7 made the registry guards conflict-free, so this is a fast safety net, not
   the primary defense.)
3. **Ordered merge.** Fast-forwards to the first branch when possible, `--no-ff`
   the rest; aborts cleanly (`git merge --abort`) on a real textual conflict
   rather than leaving a half-merge.
4. **Build/test gate.** Runs `make -C go build && make -C go test` on the merged
   result when a Go module is present (override with `--gate-cmd`, skip with
   `--no-gate`).
5. **Reap.** Removes each merged worktree (`--force` handles the Agent-tool
   lock) and deletes the merged branch, then `git worktree prune`.

### Validation

`scripts/test-worktree-merge.sh` is a hermetic, repeatable multi-"agent"
dry-run: it builds throwaway repos in `/tmp` whose linked worktrees stand in for
parallel subagents, flips `core.bare` to mimic the Agent tool, and asserts the
full cycle — clean disjoint work (with distinct migration numbers) merges with
`core.bare` auto-reset and full reaping; and both conflict classes (overlapping
file, duplicate migration number) are flagged by `--check-only`. It never
touches this repo, its main branch, or a remote.

### How it all fits

The chain's other tasks remove the *causes* of merge friction; this helper
automates the *mechanics* of the merge:

- **T1** makes the shared DB safe for concurrent forges (§3).
- **T2** stops a worktree commit hijacking :3000 (§4).
- **T3** keeps parallel migrations from colliding on a number (the conflict
  check here is the backstop).
- **T4** lets each agent self-verify its branch live before merge (§5).
- **T5** makes each worktree committable in one bootstrap (§4).
- **T6** keeps worktree commits from poisoning the hermetic git tests.
- **T7** makes the central registry guards append-conflict-free.

Net: a multi-subagent run — spawn worktrees, each forges/edits, self-verifies,
commits gate-only — integrates through `worktree-merge.sh` with zero manual
core.bare / migration / registry intervention.
