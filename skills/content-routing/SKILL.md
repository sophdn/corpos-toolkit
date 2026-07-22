---
name: content-routing
description: "Where does this content go? Sharp routing table from intent to surface. Priority writers: vault (cross-project important), toolkit DB (work-shaped), skills (permanent repeatable instructions), scratchpads (intra-session/intra-bug working memory), memory (forge(memory) — owned organ for user/behavior facts). Pass-through: inline comments. Pairs with bug-filing-discipline (kind of artifact) — this one decides surface."
triggers:
  - where should this go
  - which surface
  - which file should
  - process-docs
  - CONVENTIONS.md
  - vault entry
  - vault search
  - skill or memory
  - skill vs memory
  - skill or vault
  - memory or vault
  - scratchpad or vault
  - scratchpad or memory
  - working memory
  - toolkit DB
  - inline comment
  - what goes where
  - content routing
---

# Content Routing

Sharp answer to *where does this content go?* The rule is a routing
table plus an anti-pattern list, both leaning toward fewer surfaces
than a typical project's history actually used.

## Surfaces

1. **memory** — canonical home `~/.claude/vault/memory/<kind>/`,
   surfaced per-project at `~/.claude/projects/<project>/memory/`.
   Priority writer for **facts about the user and Claude-behavior
   rules**, via `forge(memory, memory_kind=…)` (kind ∈ user / feedback
   / project / reference). The `materialize-memory.sh` SessionStart
   hook mirrors the entry to the harness dir AND injects `MEMORY.md`
   into context. **Do NOT `Write` the per-project memory dir
   directly** — it's hook-owned output; a direct write orphans (no
   vault backing, no sentinel) and diverges the index. Governed by the
   Memory section in `~/.claude/CLAUDE.md`.
2. **vault** — `~/.claude/vault/{decisions,learnings,reference,projects,…}/`.
   Priority writer for **important AND cross-project** knowledge.
   Shared across every project mounting toolkit-server.
3. **toolkit DB** — `mcp-servers/data/toolkit.db` via the `work` /
   `knowledge` meta-tools. Priority writer for **work-shaped state**:
   chains, tasks, bugs, library entries. "Stuff we are going to do"
   lives here.
4. **skills** — `~/.claude/skills/<name>/SKILL.md` (user-level,
   cross-project) or `<project>/skills/` (project-local). Priority
   instructor for **anything permanent and repeatable** an agent
   should follow on every relevant decision. Prefer user-level when
   the rule generalizes; project-local when the rule encodes
   project-specific topology.
5. **scratchpads** — `~/.claude/scratchpads/{sessions,bugs}/<key>.md`.
   Priority writer for **intra-session / intra-bug working memory**:
   todos, hypotheses, things-tried, anchors, resume hints. Survives
   conversation compaction; survives session end. Plain markdown,
   written via `Write` / `Edit`. Governed by `scratchpad-discipline`,
   which also carries the explicit don't-use-Task* rule for chain /
   bug execution sessions.
6. **process-docs / CONVENTIONS.md** — *deprecating*. Avoid adding
   to either. Existing content stays where it is; new content goes to
   vault, toolkit DB, or skills first.
7. **README.md** — repo-root contributor entry point. One-line
   pointers to the actual surfaces. Don't accrete content here.
8. **inline code comments / docstrings** — pass-through. Useful for
   why-this-line rationale. Describe the current rule and its
   reason; don't name past bugs by number or reference resolved
   tickets.

## Routing table

| You are about to write … | Surface |
|---|---|
| A user preference or Claude-behavior rule | memory via `forge(memory, memory_kind=…)` |
| Important knowledge that applies across multiple projects | vault |
| A new task / bug / chain (or a fact about one) | toolkit DB via `forge` |
| A rule agents should follow on every relevant decision | skills (user-level if generalizes) |
| Todos / hypotheses / things-tried for this session or bug investigation | scratchpads (`scratchpad-discipline`) |
| A code change's reason that helps a future reader | inline comment |
| A project-specific procedural doc that does NOT meet the above | re-evaluate before defaulting (decision tree below) |

## Re-evaluating "process-docs would have been the answer"

When the content does not fit any priority surface (not cross-project
important, not work-shaped, not instruction-shaped, not
inline-rationale), walk this tree before falling back to process-docs:

1. **Is it actually important enough to keep at all?** If no, don't
   write it.
2. **Does it generalize one step?** "A walkthrough of project X's
   CI" reads project-specific, but the topology + state inventory
   part is reusable across any similar setup — that's vault.
3. **Will the project this is "specific to" extract into its own
   repo soon?** If yes, write it where it will live post-extraction
   (the project's own repo), not the parent's process-docs.
4. **Only after those:** process-docs is the residual home. Write
   the rationale into the file's first paragraph so a future reader
   knows it's the residual, not the default.

The direction is to slowly retire process-docs as projects extract.
Do not add new content there unless steps 1–3 all rejected the
alternatives.

## Anti-patterns

- **Memory pointing at toolkit-DB state.** Don't write a memory
  entry whose content is "the chain X exists" or "open bugs include
  Y". The toolkit DB is queryable — pointer-only memory entries
  duplicate without adding.
- **Memory pointing at vault entries.** The vault is also
  agent-readable; pointer entries that just say "see vault/X" are
  surface duplication.
- **Process-docs that restate a skill or convention.** If the rule
  is permanent and repeatable, it belongs in a skill.
- **Skills that just restate a chain's task descriptions.** The
  chain is the source of truth for its tasks; skills should encode
  rules the chain assumes, not the chain itself.
- **Vault entries that only apply to one project.** That's
  process-docs or skills material. The vault's value is cross-project
  reach.
- **CONVENTIONS.md additions.** New convention-shaped content goes
  to skills.
- **Code comments that name past bugs by number.** Describe the
  current rule and its rationale, not the bug that motivated it.
- **Scratchpads with cross-project insights.** Scratchpads are
  intra-session; cross-project content goes to vault. If a
  scratchpad note turns out to generalize, copy it to vault and
  cite the scratchpad as source — don't leave the only copy
  buried in a session-keyed file.
- **Scratchpads duplicating toolkit-DB state.** The chain/task/bug
  lifecycle in the toolkit DB is canonical. Scratchpads can mirror
  a subset (todo list reflecting chain tasks) for agent convenience,
  but never replace the work surface as the source of truth.

## Skill siting: user-level vs project-local

When you decide to write a skill (per the routing table above), pick
the location:

- **User-level** (`~/.claude/skills/<name>/SKILL.md`) — when the rule
  generalizes across every project mounting toolkit-server. Most
  leverage. The skill is visible in every session.
- **Project-local** (`<project>/skills/<name>.md` or per-repo
  convention) — when the rule encodes project-specific topology that
  would mislead in another project.

Default to user-level when in doubt and the rule reads
project-agnostic. Project-specific assumptions baked into a
user-level skill (e.g. "the assay-runners chain is the paradigm")
should be replaced with anonymized examples or omitted entirely.

## Migration direction

- **process-docs → vault / toolkit DB / skills** — slowly, driven by
  project extraction. When a project moves to its own repo, its
  process-docs content moves with it (or migrates to vault if it
  became cross-project important along the way).
- **CONVENTIONS.md → skills** — gradual. New convention-shaped
  content goes directly to skills. Existing CONVENTIONS.md content
  folds into skills as authors touch it, not under deadline.

Both migrations are non-blocking: do not pause feature work to
migrate. The point of this skill is to redirect *new* writes; the
existing inventory transitions on its own.

## Adding a new persistence surface

This skill is the central index of `~/.claude/` persistence surfaces. Each surface has its own discipline doc (vault-filing-discipline / scratchpad-discipline / bug-filing-discipline / the Memory section in CLAUDE.md). When a new surface gets added — or any current surface gets renamed or relocated — the boundary between it and every neighbor has to be re-drawn or readers landing in one doc cannot navigate to the others.

Workflow when adding a new persistence surface:

1. **Add the surface to the `## Surfaces` list above** with a one-paragraph definition that names the priority writer use-case.
2. **Add a row to the routing table** keyed by the *intent* a writer would have (not the *shape* of the content), so the table stays scannable.
3. **Update every existing surface's discipline doc** — each must add the new surface to its own "adjacent persistence surfaces" or "boundary with neighbors" section. Pairs with this rule: every persistence-discipline doc carries a complete cross-reference to every sibling, so a reader who landed on one can reach the others.
4. **Add a `## Composition` entry below** linking this skill to the new surface's discipline doc, mirroring the existing vault / scratchpad / bug-filing entries.
5. **Update the Memory section** in `~/.claude/CLAUDE.md` if the new surface changes the routing test.

The cost of skipping step 3 is high and silent: a new agent landing on the *old* discipline doc cannot discover the new surface exists, and will route content into the closest-fit older surface. That's the gap bug 1345 codified after persistence-scratchpads got added without it.

## Composition

- Pairs with **bug-filing-discipline** — bug-filing-discipline
  decides *what kind of work artifact* (bug, task, nothing); this
  skill decides *which surface* the artifact lands on. Both ambient
  when toolkit-server is available.
- Pairs with **vault-pull-discipline** — vault-pull-discipline is
  the read-side reflex (consult the vault before starting a task in
  a domain you've worked in before).
- Pairs with **vault-filing-discipline** — once this skill routes
  a write to the vault, vault-filing-discipline governs *the vault
  write itself* (cross-project test, subdir routing, frontmatter,
  pre-send ritual). Split: this skill decides *whether* vault;
  vault-filing-discipline decides *how*.
- Pairs with **scratchpad-discipline** — once this skill routes a
  write to a scratchpad (intra-session todos / hypotheses /
  anchors), scratchpad-discipline governs *the scratchpad write
  itself* (file path, sections, the don't-use-Task* paired rule,
  continuation). Split: this skill decides *whether* scratchpad;
  scratchpad-discipline decides *how*.
