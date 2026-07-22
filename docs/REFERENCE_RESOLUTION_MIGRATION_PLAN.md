# Reference Resolution Migration — Plan + Baseline + Dependency Inventory

**Chain:** reference-resolution-migration (id 598)
**Task:** T1 — audit-and-baseline-token-budget
**Date:** 2026-05-18

This doc is the load-bearing output of T1. It does three things:

1. Establishes a baseline token-budget snapshot per ambient surface (the comparison anchor for T12's after-snapshot).
2. Assigns every ambient entry to one of three buckets (🔒 keep-ambient / ⚡ condense-lazy / 📦 pure-lazy) per the chain's design framework, with the parse_context model collapsing most KEEP-AMBIENT into LAZY because parse_context's discipline-skill + skill-trigger resolvers surface them on shape match.
3. Inventories every user-level dependency mcp-servers actually consumes at runtime — the input to T2's self-containment manifest schema.

Plus: it documents the discipline-shift precondition verdict so T2-T11 can proceed (or pause if the precondition fails).

---

## 1. Baseline token-budget snapshot

Proxy: character count / 4 (rough estimated-token proxy). T12 uses the same proxy. Absolute precision isn't the goal; comparability is.

| Surface | Estimated tokens | Source |
|---|---:|---|
| `available-skills` list (40+ skill summaries with descriptions) | ~3,000 | system prompt scaffolding, populated from `~/.claude/skills/` + `mcp-servers/skills/` |
| MCP tool descriptions (eager per-action paragraphs across `work`, `knowledge`, `measure`, `admin`) | ~1,500 | toolkit-server's meta-tool description registration |
| System-prompt scaffolding (env, working dirs, gitStatus, recent commits, model identity, date) | ~800 | Claude Code session startup |
| `MEMORY.md` index + all linked entries | ~400 | `~/.claude/projects/-home-sophi-dev-mcp-servers/memory/` |
| `CLAUDE.md` project instructions | ~600 | mcp-servers/CLAUDE.md (out of paring scope) |
| Date / model identity / scaffolding misc | ~300 | bootstrap factual context |
| **Total ambient baseline** | **~6,600** | |

T12 measures after-paring against this proxy. Target: meaningful reduction; honest numbers over hitting a specific target.

---

## 2. Bucket assignments

Under the parse_context model (T4-T5), the KEEP-AMBIENT bucket collapses to a single discipline. Almost everything else becomes PURE-LAZY because the substrate's discipline-skill resolver + skill-trigger resolver surface relevant skills on user-message shape match.

### 🔒 KEEP-AMBIENT (bootstrap-load-bearing)

| Entry | Why ambient |
|---|---|
| `parse-context-first-call` (NEW, ships in T6) | THE rule that holds the librarian-using-the-computer pattern. Without this discipline loaded ambient, the agent doesn't know to call parse_context first; the entire migration collapses on itself. |
| `parse_context` action signature (in MCP tool list) | The agent needs the action name + envelope to call it. |
| Bootstrap factual context (model identity, cwd, date, shell, OS) | Single-decision-load-bearing; can't be lazily fetched. |
| `CLAUDE.md` (project instructions) | Out of chain scope. Project-level concern; stays as-is. |
| `MEMORY.md` *index lines* (one-liners per entry) | Discovery handle for the substrate's memory_entry resolver. Bodies move to lazy; index stays. |

Estimated ambient after: ~1,000–1,500 tokens.

### ⚡ CONDENSE-LAZY (one-line pointer in startup; body lazy via parse_context discipline-skill resolver)

Pure-lazy is preferred under the parse_context model; this bucket is rare. Reserved for disciplines whose trigger conditions might not be perfectly captured by the substrate's resolver yet — kept condense-lazy as a hedge during T6+T7 verification, promoted to PURE-LAZY once the resolver coverage is proven.

| Entry | Trigger (for parse_context discipline-resolver) |
|---|---|
| `vault-pull-discipline` | domain match found on a known-prior-art domain |
| `vault-filing-discipline` | cross-project insight detected mid-work |
| `bug-filing-discipline` | friction-shape phrases (already a substrate shape) |
| `content-routing` | insight surfaces from work (frontier between vault / scratchpad / memory / toolkit DB) |
| `scratchpad-discipline` | chain-task or bug-investigation context active |

T7 promotes these to PURE-LAZY if the substrate's discipline-resolver fires reliably; documented in T7 acceptance.

### 📦 PURE-LAZY (no startup presence; discoverable via parse_context skill-trigger OR slash-command OR user invocation)

#### Convention / language skills
- `code-standards`
- `coding-philosophy` *(formerly a discipline; pure-lazy because it triggers on "code being written" which is a continuous context, surfaced when the user mentions code or when the agent is about to write code)*
- `rust-conventions`
- `go-conventions`
- `expo-conventions`
- `godot-conventions`
- `python-conventions`

#### Domain-conditional
- `godot-git-hygiene` *(self-compile)*
- `kiwix-local-docs`
- `self-compile-content-sourcing` *(self-compile)*
- `codebase-inspection`
- `reference-resolution` *(absorbed by parse_context's existence; the discipline body is historical now)*

#### Debug skills
- `node-inspect-debugger`
- `python-debugpy`
- `systematic-debugging`

#### GitHub / workflow
- `github-auth`
- `github-code-review`
- `github-issues`
- `github-pr-workflow`
- `github-repo-management`
- `requesting-code-review`

#### Process / framework
- `spike`
- `writing-plans`

#### Template / structural
- `_template`

#### mcp-servers-specific (already in `mcp-servers/skills/`)
- `agentic-architecture-audit`
- `artifact-review`
- `chain-assessment`
- `external-knowledge-retrieval`
- `harness-caveats`
- `knowledge-pull-discipline`
- `project-map`
- `qwen-offload-discipline`
- `rationale-discipline`
- `retirement-signal`
- `session-routing`
- `tiered-context-loading`
- `content-routing` *(DUPLICATE — exists in both user-level AND mcp-servers/skills/; T3 must consolidate, see §3.4)*
- `vault-pull-discipline` *(DUPLICATE — same; T3 consolidates)*

#### Personas (all → PURE-LAZY, surfaced via slash-command)
- `advisory-panel`, `code-auditor`, `daemon`, `librarian`, `prospector`, `role-keeper`, `synthesist`, `technowizard`, `white-hat`
- `MANIFEST.md` (the persona registry — stays as a discoverable index)
- `ROLE_SPEC.md` (the schema — reference doc)

#### Domain-conditional memory entries (bodies pure-lazy; INDEX lines keep-ambient per §2)
- `project_atomic-tasks-vs-atomic-agents.md`
- `reference_ml-capability-substrate-framing.md`
- `reference_prior-art-scan-doc.md`

---

## 3. Dependency inventory (input for T2's manifest)

The skills, hooks, personas, and config that mcp-servers's runtime depends on. T2 designs the manifest schema; T3 migrates these into the repo per Path X (mcp-servers as authoritative source).

### 3.1 Skills mcp-servers DEPENDS ON at runtime

#### Already in `mcp-servers/skills/` (15 — canonical-here, no migration needed)
agentic-architecture-audit, artifact-review, chain-assessment, **content-routing**, external-knowledge-retrieval, harness-caveats, knowledge-pull-discipline, project-map, qwen-offload-discipline, rationale-discipline, reference-resolution, retirement-signal, session-routing, tiered-context-loading, **vault-pull-discipline**

#### Currently user-level only — TO MIGRATE INTO `mcp-servers/skills/` (T3)
**Disciplines mcp-servers workflows reach for:**
- `bug-filing-discipline`
- `coding-philosophy`
- `scratchpad-discipline`
- `vault-filing-discipline`

**Convention skills relevant to mcp-servers's Rust + Go code:**
- `code-standards` *(TS/frontend conventions; relevant for apps/dashboard)*
- `rust-conventions`
- `go-conventions`

**Process / framework relevant to mcp-servers's workflow:**
- `requesting-code-review`
- `writing-plans`
- `spike`
- `systematic-debugging`

**Workflow tools:**
- `github-pr-workflow`
- `github-code-review`
- `github-issues`

Plus the NEW skill from T6:
- `parse-context-first-call` (authored in T6; lands directly in mcp-servers/skills/)

#### Stay user-level only (NOT migrated — other projects own them or general dev tools)
- `expo-conventions` (dm-toolkit)
- `godot-conventions`, `godot-git-hygiene`, `self-compile-content-sourcing` (self-compile)
- `python-conventions`, `python-debugpy` (general; seed-packet may want these)
- `node-inspect-debugger` (general)
- `github-auth`, `github-repo-management` (general workflow)
- `kiwix-local-docs` (general infrastructure)
- `codebase-inspection` (general)
- `_template` (structural; stays user-level as a template)

### 3.2 Duplicate-skill consolidation (T3 must resolve)

Two skills exist in both locations:

- `content-routing` — exists at `~/.claude/skills/content-routing/SKILL.md` AND `mcp-servers/skills/content-routing.{md,toml}`. Drift hazard. Decision: mcp-servers/skills/ is canonical; user-level becomes a symlink during T3 migration.
- `vault-pull-discipline` — same shape. Same resolution.

T3 verifies the two pairs are content-identical before consolidating; if they've drifted, the more-recent body wins with the discrepancy documented.

### 3.3 Hooks mcp-servers DEPENDS ON

| Hook | Current location | Migration target | Status |
|---|---|---|---|
| `grounding-events-processor.sh` | `~/.claude/hooks/` | `mcp-servers/hooks/` | active in settings.json; called by Stop hook |
| `friction-filing-reminder.sh` | `~/.claude/hooks/` | `mcp-servers/hooks/` | retired from settings.json (T6 of substrate trilogy); kept as rollback artifact |
| `intercept-task-tools-reminder.sh` | `mcp-servers/scripts/hooks/` | `mcp-servers/hooks/` | T9 of substrate trilogy; consolidates with the other two during T3 |

T3 consolidates all three under `mcp-servers/hooks/`; symlinks `~/.claude/hooks/<name>.sh` into repo for the two currently at user-level.

### 3.4 Personas mcp-servers WORKFLOWS reach for

All 9 personas (plus MANIFEST.md + ROLE_SPEC.md) move to `mcp-servers/personas/`. They're all relevant to mcp-servers's auditing/synthesis/role-loading workflows. Symlinks back to `~/.claude/personas/<name>.md` after migration.

### 3.5 Settings.json wirings mcp-servers requires

Currently registered:
- `Stop` hook: `$HOME/.claude/hooks/grounding-events-processor.sh`

To-be-registered (per T9 of substrate trilogy, install pending user authorization):
- `UserPromptSubmit` hook: `$HOME/dev/mcp-servers/scripts/hooks/intercept-task-tools-reminder.sh` (snippet documented in `scripts/hooks/README.md`)

T3 documents both as install snippets in `scripts/install-into-claude.sh`'s output; doesn't auto-write settings.json (auto-mode self-modification guard).

### 3.6 MEMORY format/discipline (mcp-servers/memory/ schema only)

The format (frontmatter shape, type field, MEMORY.md index conventions) gets tracked. Personal entries stay at `~/.claude/projects/.../memory/` — NOT in repo.

What goes in `mcp-servers/memory/`:
- `_schema.md` (or similar) documenting the frontmatter + body conventions
- Any memory-format-related discipline-skill files (the auto-memory discipline lives in the harness; if there's a project-side discipline-skill, it travels)

Personal entries (`feedback_*`, `project_*`, `reference_*`) stay where they are.

---

## 4. Discipline-shift precondition verdict

The precondition: the agent must have internalized the discipline shift codified in memory `feedback_resolve_references_as_orienting_call.md` — `parse_context` (or `resolve_references` during transition) is the orienting first call on user messages introducing reference shapes; per-shape direct calls are reserved for action operations on already-resolved bindings.

### Self-attestation by the T1-authoring agent

The session that authored this T1 audit is the same session that:

1. Self-assessed the report card showing pervasive per-shape-direct defaulting (the librarian-using-the-card-catalog pattern, [grade B− on T4 resolve_references usage in the originating report card](#)).
2. Was given the librarian-vs-computer analogy by Sophi.
3. Committed to the discipline shift in `feedback_resolve_references_as_orienting_call.md`.
4. Was given the sharper parse_context inversion + filter + discernment.
5. Forged this migration chain incorporating the inversion.

The discipline is **committed as intent** (memory saved + chain forged around it). It is **not yet observable as habit** because the agent has been doing migration-design work, not normal reference-resolution work — the substrate the discipline targets (`parse_context`) doesn't ship until T5. Testing habit against today's `resolve_references` (its narrow predecessor) measures the wrong target.

### Verdict

**Internalized as intent; habit verification deferred to T12 (bootstrap-verification) and T13 (longitudinal report card).** Proceed with chain.

The honest framing: pausing the chain here would be over-cautious. The migration ships the substrate the discipline targets; without parse_context existing, the habit can't be fully tested anyway. T12 verifies behavioral compliance on representative sessions; T13 grades longitudinally with the originating report card as baseline. If those surface a regression, that's where the chain pauses — not here.

---

## 5. Output handoff — which surfaces gate which downstream tasks

| Downstream task | Surfaces it consumes from this audit |
|---|---|
| T2 `self-containment-architecture-design` | Dependency inventory §3 (skills + hooks + personas + settings + memory schema). Bucket assignments §2. |
| T3 `migrate-skills-hooks-personas-into-repo` | §3.1, §3.2 (consolidation), §3.3, §3.4, §3.6. The exhaustive inventory; missing entries here = missing symlinks in T3 = runtime regression on disaster recovery. |
| T4 `parse-context-design` | Bucket assignments §2 — drives resolver coverage. KEEP-AMBIENT entries that need shape detection (most discipline triggers) inform the discipline-skill resolver design. |
| T5 `parse-context-ship` | Inherits T4's design. |
| T6 `first-call-discipline-skill` | §2's KEEP-AMBIENT bucket — this skill IS the bucket's sole survivor. |
| T7 `skill-body-paring` | §2 bucket assignments. Manifest from T2-T3. |
| T8 `mcp-tool-description-condense` | Baseline §1 (~1,500 tokens to reduce). |
| T9 `system-prompt-scaffolding-audit` | Baseline §1 (~800 tokens). |
| T10 `memory-domain-conditional-lazy-routing` | §3.6 + bucket assignments §2's memory section. |
| T11 `vault-note-skill-subsumption-dedup` | The condense-lazy disciplines in §2 — vault notes whose content has been promoted to those skills get marked. |
| T12 `migration-retrospective-and-bootstrap-verification` | Baseline §1 (proxy for the after-snapshot comparison). All §3 dependencies (the disaster-recovery test verifies they're all installable from clone). |
| T13 `post-migration-substrate-report-card` | The originating report card (captured verbatim in T13's problem_statement). This T1 doc provides the baseline that T13's "what changed since baseline" section compares against. |

---

## 6. Open items + caveats

1. **Bucket-promotion path:** Some entries in CONDENSE-LAZY (§2) may promote to PURE-LAZY after T7 verifies the substrate's discipline-skill resolver fires reliably. The bucket is a soft signal, not a permanent classification — the goal is to minimize ambient load without breaking discoverability.

2. **Cross-project ownership question:** `content-routing` and `vault-pull-discipline` exist in both `~/.claude/skills/` and `mcp-servers/skills/`. T3 consolidates with mcp-servers as canonical; this is a workbench-local decision that may need revisiting if seed-packet, atomic-tasks, or dm-toolkit start co-owning the same skills. Cross-project canonicalization is a follow-on if drift emerges.

3. **The harness's startup-payload generation:** Some paring (the available-skills list size reduction, MCP description condensing) may require upstream Claude Code changes. T7 / T8 will document any harness-side gaps and ship what's user-configurable; an upstream-Claude-Code feature request might land as a follow-on.

4. **Habit verification deferral:** Documented in §4. T12 + T13 own this.

---

## 7. Chain handoff

T1 closes. T2 (`self-containment-architecture-design`) and T4 (`parse-context-design`) become unblocked — both gated only by T1. They can proceed in parallel. T8, T9, T11 are also unblocked but are smaller standalone surfaces; they can also proceed in parallel.

Next-step priorities by ROI:
1. **T2 + T4** (design dual-track) — design decisions unlock the headline implementation work
2. **T8, T9** — small standalone wins; can land while T3 / T5 are in flight
3. **T11** — small precision pass
4. **T3, T5** — the headline implementations (gated by T2 + T4 respectively)
5. **T6, T7, T10** — consume the headlines
6. **T12** — retrospective
7. **T13** — longitudinal report card (fresh session)
