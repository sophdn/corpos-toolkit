# skills/ — corpos-toolkit canonical skill tree

**This tree is CANONICAL** for the agent disciplines that cannot be followed
without operating toolkit-server's own surfaces. It is the single source of
truth for these skills' text; every other copy is downstream.

Established by chain `skills-ownership-split-toolkit-core-canonical-corpos-manages-the-rest`
(T2). The full ownership table for all 43 skills — the decision rule, every
skill's canonical repo, and the borderline rationale — lives at
[`docs/SKILLS_OWNERSHIP.md`](../docs/SKILLS_OWNERSHIP.md).

## What lives here

Skills whose instructions **require toolkit-server** (the work/knowledge ledger:
`forge`, the bug/suggestion/chain/task/roadmap surfaces, `vault_search` /
`vault_read` / `forge(vault-note)`, `memory`, `parse_context`,
`knowledge_search`). A reader cannot act on these without the substrate, so the
substrate repo owns them:

| skill | why it is toolkit-core |
|---|---|
| `bug-filing-discipline` | `forge` kind=bug; dedupe via `knowledge_search` |
| `suggestion-filing-discipline` | the suggestion surface; `knowledge_search` dedupe |
| `vault-filing-discipline` | `forge(vault-note)` → `knowledge_pointers` |
| `vault-pull-discipline` | `vault_search` / `vault_read` |
| `content-routing` | a routing map INTO toolkit surfaces (DB / vault / memory / skills) |
| `parse-context-first-call` | the `parse_context` reflex |
| `deterministic-extraction-discipline` | extract a soft-corpus fact into an owned toolkit service + wire `parse_context` |

## What does NOT live here

- **General craft, stack conventions, and harness/agent workflow** are owned by
  **corpos** (`internal/skills/library` + `userlib`) — debugging, refactoring,
  language conventions, worktrees, scratchpads. They work in any repo and do not
  depend on toolkit-server.
- **Temporary / experimental / personal** skills are **overlay-native**, living
  only in `~/.claude/skills` (e.g. `corpos-swap-rehearsal`, which retires with
  chain 270). Giving a temporary skill a canonical home is how it becomes
  permanent.

## Relationship to the other two trees

- **corpos embed** (`corpos/internal/skills/library`): corpos ships the
  toolkit-core skills embedded so a headless session still loads them. Those
  embedded copies are a **mirror** of this tree, kept honest by a drift gate —
  see chain 444 T3. Do not edit them by hand; edit here and sync.
- **`~/.claude/skills`** is a **downstream overlay**, not a source of truth for
  any repo-owned skill. Editing a repo-owned skill there is the category error
  that made two 2026-07-16 corrections land nowhere (they survived only because
  this tree now carries them). The overlay IS canonical for overlay-native
  skills only. See chain 444 T4 for the overlay demotion + `~/.claude` git-index
  repair.

## Layout

Each skill is `<name>/SKILL.md`, matching both loader shapes corpos discovers
(`<name>/SKILL.md` and top-level `<name>.md`). A leading-underscore directory
name (`_wip`, `_template`) is hidden from corpos's loader (corpos
`internal/skills` `discoverFS`, bug 1194).
