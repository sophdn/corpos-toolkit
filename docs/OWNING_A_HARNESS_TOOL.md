# Owning a harness tool

How to reimplement a Claude Code harness tool (Read/Grep/Glob/LS, and later
Edit/Write/Bash/WebFetch/…) as an owned toolkit-server action, so the agent runs
on surfaces we own instead of the rented harness. This is the repeatable
playbook behind the `owned-tooling-easy-targets` chain and every tool-ownership
chain after it.

**Design spine:** `vault/decisions/2026-05-29_own-equals-fit-to-substrate-not-clone.md`
— *own = fit-to-substrate, not clone*. Two passes:

1. **PARITY PASS (the floor).** Match the harness tool byte-for-byte first.
   Parity is the gate for the deny-list swap — you cannot deny the harness tool
   until the owned one at least matches, or every session regresses below
   baseline. Predictability is itself a feature of an agent tool.
2. **UPGRADE PASS (the point).** Layer substrate-native capability ON TOP as
   OPT-IN modes, with the DEFAULT staying parity. This is where owning pays off
   (event log, knowledge_pointers, doc.go intended-use, CODEMAP, projections,
   go/ast). Scope each upgrade *only after its parity tech lands* — design
   against concrete code, not a guess. (Enforced in the chain: each upgrade task
   is `task_block`ed on its parity task.)

---

## 0. Build the characterization net FROM SOURCE first (TDD) — this is the spine

**The authoritative oracle is the harness tool's source at
`~/dev/claude-code/src/tools/<Tool>/`** (plus its helpers). The FIRST step of any
tool-ownership chain is: read that implementation, encode its exact behavior as a
Go characterization net (tests first), THEN build the Go action to pass. This is
`code-migration-discipline` (skill) with the harness tool as the source surface;
the net is the gate. (Canonical decision: feedback memory
`characterization-tdd-from-claude-code-source-for-owned-tools`.)

**Why source, not live observation.** Building `fs.read` from the chain's authored
"cat -n style" description plus live tool-observation shipped a wrong parity
floor. The real `addLineNumbers` (`src/utils/file.ts`) is feature-flagged
(compact tab-default vs legacy `→`+6-pad), splits `/\r?\n/` and `.join('\n')` (NO
trailing newline), and Read caps by **256 KB bytes + 25000 tokens, not a line
count**. Live observation is lossy — it can't see edge cases the rendered output
doesn't exercise, and **not every harness tool is even mounted in a given
session** (only Read was, the session this was written). A wrong "parity" floor
poisons the deny-list swap, because every session then regresses below baseline.

**Model-agnostic carve-out.** PORT behavior intrinsic to the tool (output format,
param/range semantics, encoding handling, byte caps, warnings); DROP behavior
coupled to Claude's model. Read's 25000-token cap (`countTokensWithAPI`) is
deliberately NOT ported — the owned harness is model-agnostic, so the byte cap is
the size guard. Apply the same test to any model-coupled logic in a future tool.

**How:** commit a small fixture tree (`go/internal/fs/testdata/parity/tree/`),
record the source-derived contract (with `src/...` file:line citations) in
`OBSERVED_HARNESS_CONTRACT.md` next to it, pin the expected output as golden
assertions (`TestRead_HarnessParity`), then implement. Validate the goldens
against the live tool too when it happens to be mounted — but the source is the
spec. A tool with **no** claude-code counterpart (there is no LS tool — listing is
Bash/Glob) is SELF-DEFINED, not parity-matched.

---

## 1. Two checklists: new SURFACE vs new ACTION

The first tool on a brand-new meta-tool surface (e.g. `fs`) pays the full wiring
cost. Every subsequent tool on that surface is cheap.

### 1a. New SURFACE (do this once per surface) — the 6 gate-enforced seams

Using `fs` as the worked example; copy `go/internal/ml/` (the smallest surface)
as the template.

1. **Package** `go/internal/<surface>/`:
   - `doc.go` with the four-field intended-use block (**Workflow served /
     Invocation pattern / Success shape / Non-goals**) — the `codemap-gen --lint`
     gate rejects a missing field.
   - `<action>.go`: a typed `<Action>Params` struct (json tags are the source of
     truth for derived doc param types), a `<Action>Result` struct, and a
     `Handle<Action>(ctx, params) (Result, error)` (or `(ctx, project, params)`).
   - `table.go`: `BuildTable(deps Deps) dispatch.Table` mapping action name →
     `dispatch.AdaptParamsOnly` / `Adapt` of the handler.
   - `action_doc.go`: an `actionspec.ActionEntry` registry + `<Surface>ActionSpecs()`
     (copy `ml/action_doc.go`). Leave `DocParam.Type` empty — it derives from the
     param struct (`int64`→`integer`, `string`→`string`, `bool`→`boolean`).
2. **Description constant** in `go/internal/actiondocs/meta_tool_descriptions.go`:
   `<Surface>Description`. The `Actions (alphabetical): …` line is parity-checked
   against the generated corpus — keep it in sync as you add actions.
3. **Contract-net test table** in `go/internal/actiondocs/surface_contract_net_test.go`:
   add `{"<surface>", actiondocs.<Surface>Description}` to `surfaceNets`.
4. **Meta-tool description test table** in
   `go/internal/actiondocs/meta_tool_descriptions_test.go`: add the
   `{"<surface>", <Surface>Description}` case.
5. **Corpus generator** in `go/cmd/action-docs-corpus-gen/main.go`: add
   `{name: "<surface>", specs: <surface>.<Surface>ActionSpecs()}` to
   `generatedSurfaces()` (and the import).
6. **Server wiring** in `go/cmd/toolkit-server/main.go`: a `mcp.AddTool(server,
   &mcp.Tool{Name: "<surface>", Description: actiondocs.<Surface>Description,
   InputSchema: metaSchema}, …)` block **and** an
   `observehttp.RegisterDispatchTable("<surface>", …)` line. (Alias the import if
   the package name collides, e.g. `fsapi "toolkit/internal/fs"`.)

Then hand-author `go/internal/actiondocs/corpus/<surface>/_general.toml`
(cross-cutting prose; the generator never touches `_general.toml`).

### 1b. New ACTION on an existing surface (the cheap path)

1. Add `<Action>Params`/`<Action>Result`/`Handle<Action>` in a new file.
2. Add the action to `BuildTable`.
3. Add an `ActionEntry` to the surface's `action_doc.go` registry.
4. Bump the `Actions (alphabetical): …` line in `<Surface>Description`.
5. Regenerate (see §2). That's it — no new contract-net rows.

---

## 2. Regenerate + gate (the exact commands)

Run from the repo root (Go is driven via `make -C go`; bare `go` needs `cd go`):

```bash
make -C go build-all                                   # compile everything
bash scripts/action-docs-corpus-gen                    # regenerate corpus/<surface>/*.toml
bash scripts/codemap-gen                                # regenerate CODEMAP.md
# regenerate the contract-net goldens for the surface you touched:
cd go && UPDATE_CONTRACT_NET=1 go test -tags sqlite_fts5 ./internal/actiondocs/ -run 'TestContractNet_Surfaces/<surface>'
go test -tags sqlite_fts5 ./internal/<surface>/ ./internal/actiondocs/   # fast inner-loop check
```

Then commit — the worktree's gate-only `pre-commit` (installed by
`scripts/worktree-setup.sh`) runs the full `scripts/precommit.sh`.

**Gotchas:**
- The corpus TOMLs are **generated, not hand-written** — edit the Go descriptor
  in `action_doc.go`, never the `.toml` (the no-diff gate will catch a hand-edit).
  `_general.toml` is the one exception (hand-authored).
- gofmt will realign your inline comments on commit — expected, not a failure.
- A new package's `doc.go` must have all four intended-use fields or
  `codemap-gen --lint` fails.

---

## 3. The deny-list swap — a GLOBAL, path-scoped rule (NOT project settings)

Adding the harness tool to `permissions.deny` is the **final** step. The
mechanism is non-obvious and was nailed down by reading the harness source
(`~/dev/claude-code`); get it wrong and the deny silently never loads.

**Why NOT project-local settings.** Project/local settings resolve their
directory from `getOriginalCwd()` — the dir the session was *launched* from —
not the git root or the file's repo (`src/utils/settings/settings.ts`
`getSettingsRootPathForSource`). Sessions here routinely launch from `~/dev` (or
`~`), so a deny placed at `~/dev/mcp-servers/.claude/settings.json` is **never
read**. Verified: a fresh session launched from `~/dev` had `Read` fully enabled
despite that file existing.

**The correct mechanism: a GLOBAL rule with a PATH SPECIFIER.** File-tool deny
rules accept a path glob in `ruleContent` (`Read(<glob>)`), matched gitignore-
style against the file's absolute path (`src/utils/permissions/filesystem.ts`
`matchingRuleForInput` / `patternWithRoot`). A `~/`-rooted pattern resolves
against homedir. So put this in the **global** `~/.claude/settings.json` (the
user source, always loaded regardless of cwd):

```json
"permissions": { "deny": ["Read(~/dev/mcp-servers/**)"] }
```

This denies `Read` **only** for files under `~/dev/mcp-servers`, from any launch
cwd, and leaves every other repo's `Read` intact. It is the cwd-independent,
repo-scoped deny that project settings cannot express. Global settings
**hot-reload** — editing the file denies matching `Read` calls in the running
session immediately (observed).

**Sequencing (unchanged):** only deny a tool that is owned AND deployed — merged
to main, `make -C go build` in the main checkout, `/mcp reconnect` — or the agent
has neither the harness tool nor a working replacement. Add each tool's pattern
as it ships (`Read(...)`, then `Grep(...)`, then `Glob(...)`).

**CONFIRMED CONSTRAINT — deny Read/Edit/Write as a SET, never Read alone.** The
harness `Edit` requires a prior harness `Read` of the file, and **`fs.read` does
NOT satisfy that guard** (verified live: after an `fs.read`, `Edit` still errors
"File has not been read yet"). So denying `Read` for a repo breaks the Read→Edit
loop for every existing file there — you can Write new files (Write sets
read-state, so Edit-after-Write works) but you cannot Edit an existing file you
have only `fs.read`. Therefore:

- Do **NOT** deny `Read` until the owned `fs` surface also provides `Edit` +
  `Write`, and deny the three **together** — they are a coupled family. The
  deny step moves to the END of the whole read/edit/write group, not per-tool.
- Reverting a deny is self-permission-widening: the auto-mode classifier blocks
  the agent from removing its own deny rule. Only the **user** can revert it.
  (So the agent that lands the deny cannot un-land it — land it deliberately.)
- The complementary **ALLOW** (the owned-surface sanction landed alongside the
  deny) is *also* self-permission-widening, and editing it mid-session degrades
  THAT session: the self-widening guard then blocks write-shaped Bash
  (build/test/`rm`) on the deny-scoped repo until a fresh session. (The deny edit
  does not trigger this; the allow does — confirmed live in chain
  own-read-write-edit-family, 2026-05-30.) So land the complementary allow as the
  **user** or from a **fresh session**, then build/test/commit from that fresh
  session — never self-edit the allow and keep working in the repo. See bug
  `self-editing-permissions-allow-degrades-the-editing-session`.

---

## 4. Where the substrate upgrades come from (upgrade pass)

Default stays parity; these are opt-in modes. No LSP exists in-repo — symbol
modes use **go/ast (stdlib)**; provenance uses **git blame + commit-landed
events** until native artifact-diff events land.

- **Read:** outline (go/ast signatures+doc), symbol-addressed (go/ast resolve),
  provenance (git blame + events), oriented (doc.go intended-use +
  knowledge_pointers).
- **Grep:** knowledge-aware (fold knowledge_pointers/FTS5 hits), substrate-ranked
  (recency/hotness/quality), symbol-scoped (go/ast).
- **Glob:** named convention queries (CONVENTIONS-derived, single source of
  truth — no drift-prone hardcoded paths), projection-joined (open-bug /
  recent-change annotations).
- **LS:** orientation (doc.go one-liner + last-change event + open-bug count +
  CODEMAP package summary).

Each upgrade mode needs an explicit param and its own integration test; the
default output must stay byte-identical to the parity pass.
