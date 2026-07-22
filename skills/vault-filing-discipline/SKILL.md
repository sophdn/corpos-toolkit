---
name: vault-filing-discipline
description: "Capture cross-project-worthy insight in ~/.claude/vault/ as a decision, learning, or reference note. Forge-first: new entries go through `forge(vault-note, …)` so they auto-index into knowledge_pointers; `Write` is the exception (custom frontmatter, scratch/, projects/, meta/). Codifies the cross-project test, subdir routing (decisions vs learnings vs reference), pre-send ritual phrase list, frontmatter convention, and the don't-float-as-question rule. Cross-project: the vault is shared across every project mounting toolkit-server."
triggers:
  # Insight-articulation phrases (original set — fire when a synthesis
  # is being written and may be vault-worthy).
  - vault entry
  - vault note
  - vault write
  - vault filing
  - capture in vault
  - decision doc
  - design decision
  - architectural decision
  - we decided
  - we settled on
  - going with
  - i learned
  - learned that
  - the gotcha
  - the trap
  - worth remembering
  - worth capturing
  - for future reference
  - future me
  - in hindsight
  - cross-project
  - applies elsewhere
  - applies to other projects
  - durable note
  - reference note
  - lesson learned
  # User-directive phrases (fire when the user asks the agent to file).
  # Catches the case where the user says "file these" rather than
  # writing out the synthesis themselves.
  - file in the vault
  - file as vault
  - file as a vault
  - file to vault
  - add to vault
  - add to the vault
  - file all
  - file these
  # Agent first-person about-to-file phrases (fire on the agent's own
  # draft response right before reaching for the Write tool). Catches
  # the agent-initiated case where the original triggers above were
  # never said in user-facing text.
  - writing the vault note
  - writing the vault entry
  - creating the vault note
  - creating the vault entry
  - going to vault
  - going to file
  - as a vault learning
  - as a vault decision
  - as a vault reference
  - vault learning candidate
  - vault decision candidate
  - vault note candidate
  - vault learnings to file
---

# Vault Filing Discipline

Cross-project-worthy insight goes into `~/.claude/vault/` so that
agents in any other project can retrace the reasoning. Memory and
hooks point at this skill rather than duplicating the rules.

The vault is read-side gated by `vault-pull-discipline` (reflex on
task pickup) and write-side routed by `content-routing` (which
decides vault vs toolkit-DB vs skill). This skill governs *the
write itself* once those upstream rules say "yes, vault."

## TL;DR — forge-first reflex

For any new `decision` / `learning` / `reference` entry headed for the
routed subdirs (`vault/decisions/`, `vault/learnings/<scope>/`,
`vault/reference/`):

```
mcp__toolkit-server__work({
  action: "forge",
  schema_name: "vault-note",
  slug: "...",
  scope: "mcp-servers",      // routing input for kind=learning; empty = learnings/general
  note_kind: "decision",     // | "learning" | "reference"
  title: "...",
  body: "...",
  tags: "..."
  // top-level `project` is DB-attribution only (defaults to the `vault` sentinel
  // when empty); see "Filing path" for the scope-vs-project rule.
})
```

**Do not** reach for `Write` first. `Write` skips the indexsync hook
that creates the `knowledge_pointers` row → the entry is invisible to
`vault_search` until a separate seeder pass runs. The full forge-vs-
Write rationale is in "Filing path" below; the `Write`-acceptable
exceptions (`projects/<project>/`, `meta/`, `scratch/`, custom
frontmatter shapes) are listed under "Still preferred for these
cases".

If `forge(vault-note)` errors with `shape backend not implemented`
the stdio MCP server is on a pre-`5a240e6` binary — that's the one
documented fallback case for `Write` on routed-subdir entries. See
"Fallback when `forge(vault-note)` errors" below.

## The active discipline — file as the default

Be **ACTIVE**. Most non-trivial sessions surface at least one
vault-worthy insight: a decision named in a synthesis, a lesson
re-derived from a familiar failure, a reference assembled while
solving the immediate problem, a cross-project framing that appeared
during the work. A retrospective that files nothing into the vault is
rarely a neutral outcome — it's almost always a missed signal.

**"Nothing to file" is a valid outcome but it should NOT be the
default.** If a session ran on rails and produced no synthesis, no
decision, no re-derived pattern — fine, nothing to file. Otherwise,
act. A pass that does nothing is a missed learning opportunity, not a
neutral outcome.

### Signal taxonomy — any one warrants action

1. **Decision made + rationale.** A design choice plus the constraints
   it depended on that limit future options. The rationale is the
   load-bearing part — the choice without the constraint is incomplete
   and a future agent can't tell whether the decision still applies.
2. **Lesson re-derived.** A pattern, gotcha, or failure mode you
   noticed twice — or once but recognized as something that will
   recur. Includes failure modes named only after the fact.
3. **Reference assembled.** Durable how-to material gathered while
   solving the immediate task — kiwix queries, command recipes,
   convention summaries that future-you will look up again. Concise
   and task-focused, not full mirrors of upstream docs.
4. **Cross-project framing.** A way of thinking that appeared during
   synthesis and reads cleanly without naming this project. If you
   wrote a sentence like "the pattern is..." / "the trade-off is..." /
   "two distinct axes..." that's a candidate.

### Preference order — pick the earliest that fits

1. **AMEND or SUPERSEDE an existing vault note** when there's ≥80%
   overlap. `vault_search` first; if a near-equivalent entry exists,
   edit it (or supersede with a new dated decision) rather than
   parallel-file. Supersession is itself signal.
2. **EXTEND an existing umbrella** — add a section, paragraph, or
   subsection to a related note. Don't fork umbrellas unless the new
   content is structurally distinct.
3. **FILE NEW in the appropriate subdir** — `decisions/` /
   `learnings/<project>/` / `reference/`, per the routing table below.
   Last resort, not first reflex.

The reflex is **fire first, narrate second** (see "Failure mode"
below). Asking "should I file this?" is itself the friction the
discipline eliminates.

### Do NOT file as vault notes (anti-patterns)

These shapes look vault-adjacent but harden into noise:

- **One-off task narratives.** "I analyzed today's PR" / "I summarized
  this report." Not a class of cross-project insight; not vault.
- **Project-state facts.** "Bug X is open" / "chain Y is at task 3."
  Queryable from `mcp__toolkit-server__work`. A pointer-only vault
  entry adds nothing.
- **User-preference / agent-behavior rules.** Memory's job — written via `forge(memory, memory_kind=…)`, not the vault.
- **Repeatable rules that fire on every applicable turn.** Skill's
  job. Vault notes are *consulted*; skills are *applied*.
- **Session-specific transient observations.** "I retried and it
  worked." The lesson (if any) is the retry pattern — that's a skill
  update, not a vault note.

## The single test — cross-project value

> Would an agent in a *different* project benefit from retracing
> this reasoning?

- **Yes** → vault. Examples: an architectural choice that
  constrains future projects, a recurrent pattern across MCP
  servers, a kiwix / hardware / tooling note, a class of failure
  observed in two different repos.
- **No, project-internal only** → not vault. Goes to project-local
  `process-docs/`, a project-local skill, or nowhere.

Bias: when the insight is *abstract enough to restate without
naming this project*, it's vault. When it depends on this project's
specific file paths, naming, or topology, it's project-local.

## Subdir routing

| Subdir | Use for | Filename |
|---|---|---|
| `decisions/` | An architectural choice + the rationale that constrained other choices | `YYYY-MM-DD_<kebab-slug>.md` |
| `learnings/<project>/` | A pattern, gotcha, or failure mode you re-derived; project-tagged but the *shape* generalizes | free-form, kebab-case |
| `reference/` | Durable factual material future-you will look up (kiwix, hardware, conventions, tool catalogues) | topic name, kebab-case |
| `projects/<project>/` | Long-running per-project state notes (rare; prefer toolkit DB for work-shaped state) | free-form |
| `meta/` | Notes about the vault itself, navigation, or cross-cuts | free-form |
| `scratch/` | Throwaway exploratory notes, expected to age out | free-form |

`decisions/` and `learnings/` are the most-used surfaces. If
unsure, default to `learnings/<project>/` — promotion to
`decisions/` is cheap if the pattern proves durable.

## Pre-send ritual

Before every reply that names a synthesis, scan the draft for these
exact phrases:

- "we settled on"
- "we decided"
- "going with"
- "the gotcha is"
- "the trap was"
- "i learned"
- "lesson learned"
- "worth remembering"
- "worth capturing"
- "for future reference"
- "in hindsight"
- "design decision"
- "architectural decision"
- "vault entry" / "vault note"

For each hit, ask the cross-project test. If yes, **stop
composing**, write the vault file, and rewrite the sentence to
reference the filed path ("captured at
`vault/decisions/<slug>.md`" — not "worth a vault entry" or "could
note this").

**Don't float vault candidates and wait for permission.** "Should I
capture this?" pushes the filing decision onto the user — that's
itself the friction the rule eliminates. If the cross-project test
passes, write.

## Failure mode: enumerating-but-not-filing at retro time

Bug 1442 codifies an observed shape across two consecutive sessions
(2026-05-18 + 2026-05-19, same agent model, both retros): the user
asked "did you think of vault entries you didn't ask to file or just
file yourself?" and the agent responded with a **list of candidates**
("here are 3 vault-worthy items — which should I write?") rather than
firing `forge(vault-note, …)`. The user then said "please file all"
and only then did filing happen.

The list-then-ask pattern is the same friction "don't float as a
question" rules out — just delayed to retro time. Two specific
recovery cues:

- **Pre-retro mandatory scan.** Before producing any end-of-session
  summary or responding to a "what synthesis?" prompt, scan the
  conversation buffer for: decisions made, learnings derived,
  patterns named, reference material assembled. Each candidate that
  passes the cross-project test → `forge(vault-note, …)` BEFORE the
  summary, not "list them and ask."
- **Self-detect the enumeration shape.** If you find yourself
  drafting a sentence like "I have N vault candidates — should I
  file?", that draft is the failure mode. Delete the question.
  Apply the cross-project test to each item, file the ones that
  pass, and write a summary line that names the paths ("Captured at
  `vault/decisions/<path>.md`, `vault/learnings/general/<path>.md`,
  …").

The reflex is **fire first, narrate second.** Asking "should I?"
re-introduces the per-candidate decision the discipline exists to
eliminate.

### Bypass shape — direct Write instead of forge(vault-note)

Bug 1444 records a related anti-pattern: at retro time the agent
reached for `Write` directly (less ceremony, "I'm writing many notes,
Write feels lower-friction") instead of `forge(vault-note, …)`. The
result: non-canonical frontmatter, no inline FTS5 indexing, entries
invisible to `vault_search` until `knowledge_seeder` runs.

**Forge is the default; Write is the documented exception.** The
exceptions (custom-frontmatter shapes, `projects/`/`meta/`/`scratch/`
subdirs, throwaway notes) are listed under "Still preferred for these
cases" below. If the entry you're about to write is a
`decision` / `learning` / `reference` headed for the routed subdirs,
forge is correct — Write is the bypass. "Many notes at once" is not
a valid Write exception; it's exactly when forge's auto-indexing
pays off most.

## Filing path — forge vs Write

Two authoring paths reach `~/.claude/vault/`:

### Preferred for new entries: `forge(vault-note, …)`

`mcp__toolkit-server__work` with `action="forge"` and
`schema_name="vault-note"` creates the entry through the schema-
enforced path. Two benefits over a plain `Write` call:

1. **Schema-enforced frontmatter** — date, slug, kind, scope, and
   tags are validated. You can't accidentally ship an entry with a
   typo'd date or a missing kind.
2. **Auto-index into `knowledge_pointers` + FTS5 on create** — the
   new entry is reachable via `vault_search` *immediately*. No wait
   for `knowledge_seeder` to batch-pick it up.

Parameter shape (sugar — top-level field params; the structured
`fields: {...}` shape works too):

```text
mcp__toolkit-server__work({
  action: "forge",
  schema_name: "vault-note",
  slug: "use-fts5-virtual-table-for-sync",
  scope: "mcp-servers",             // routing input; see "scope vs project" below
  note_kind: "decision",            // decision | learning | reference
  title: "Use FTS5 virtual table for index sync",
  body: "...",                      // long-form markdown; paragraphs separated by blank lines
  tags: "go,forge,fts5"             // comma-delimited; reuse existing tags where possible
})
```

The schema field is named `note_kind` (not `kind`) because top-level
`kind` is a reserved alias for `schema_name` on the forge sugar
shape. If you forget, the field is silently stripped and you get
`required field "note_kind" is missing` on validation.

#### `scope` vs top-level `project` — two distinct concerns

Chain `forge-vault-note-schema-rework` (T3, 2026-05-20) split the
old `project` parameter into two fields with non-overlapping jobs.
Bug 1433 codified the trap that motivated the split: a single
`project=...` argument was doing double duty as both routing-subdir
input and knowledge-pointer DB attribution, so an agent passing
`project="mcp-servers"` to a cross-project decision silently misrouted
the file under `learnings/mcp-servers/` instead of `decisions/`.

| Parameter | Where it lives | What it controls | Empty-value behavior |
|---|---|---|---|
| `scope` | Inside the schema fields (top-level sugar field) | Vault subdir routing for `kind=learning` only | Empty/absent → `learnings/general/` |
| top-level `project` | Dispatch envelope (sibling of `action` / `params`) | The `knowledge_pointers.project_id` stamp (DB attribution) | Empty → `vault` sentinel stamp |

**Special-case alignment (chain 617 T1):** `scope="general"` is semantically equivalent to `scope=""` — both name the explicit cross-project bucket for learnings. The pointer's `project_id` resolves to the `vault` sentinel in both cases, NOT to `"general"`. Without this alignment, the legacy seeder (which stamped `project_id="vault"` for `learnings/general/*`) and post-rework forge would write parallel pointer rows for the same `source_ref` — different `project_id` values would each survive the `UNIQUE (project_id, source_type, source_ref)` constraint, and downstream projections that LEFT JOIN `knowledge_pointers ON source_ref` would multiply rows. Code: `resolveVaultNoteProjectID` returns `"vault"` for `kind="learning" && scope IN ("", "general")`.

The schema is marked `cross_project = true`, which exempts it from
the forge dispatcher's project-required gate AND from the
auto-injection of top-level `project` into the schema's same-named
field. **Do not pass top-level `project` unless you specifically want
to override the DB-attribution stamp**; for `decisions/` and
`reference/`, omit it (the `vault` sentinel is correct). For
`learnings/<scope>/` where `<scope>` is a real project name
(`mcp-servers`, `seed-packet`, `hermes`, …), the convention is to set
top-level `project` to the same value as `scope` so the file and its
pointer carry matching attribution — the dispatcher will NOT do this
for you, the cross_project exemption blocks the auto-fill. For
`learnings/general/` (or empty scope), omit top-level `project` and
let it resolve to the `vault` sentinel; the `general` bucket is
cross-project and must NOT carry a `project_id="general"` stamp on
its pointer (chain 617 T1).

`scope` only routes when `note_kind = "learning"`. For
`kind=decision` and `kind=reference`, `scope` is ignored — those
kinds always land at the cross-project top level
(`vault/decisions/` and `vault/reference/`).

#### Response shape — `action` verb + `routing_note`

Every successful `forge(vault-note, …)` call returns:

```json
{
  "ok": true,
  "schema_name": "vault-note",
  "slug": "...",
  "action": "created" | "updated",
  "artifact_path": "...",
  "routing_note": "..."
}
```

- `action="created"` — the slug was new; a fresh file + pointer row landed.
- `action="updated"` — a same-slug entry already existed (chain T4 policy
  is auto-update; see below). The pointer row's content-derived columns
  refreshed and the file rewrote in place. If the re-forge changed
  `scope` (e.g. promoting a `learning` to a `decision`), the old file
  was unlinked and the pointer's `source_ref` realigned to the new
  subdir. Usage counters (`usage_count`, `last_used_at`) survive
  re-forges by design — re-running a sync should not wipe history.
- `routing_note` (bug 1433) — one-line summary of any caller-influenced
  routing decision (e.g. `"routed to learnings/general (no scope field
  set — explicit cross-project bucket)"`). Empty for routing-determined
  schemas, populated for vault-note.

#### Same-slug re-forge policy (chain T4)

Forge keys vault-note identity off `slug` alone, not `(project, slug)`:
a second `forge(vault-note, slug="foo", …)` with different
`scope` / `project` / `body` **updates** the existing pointer in place
rather than creating a parallel pointer row (the bug 1435 trap shape
this policy fixes). The same-slug path is *auto-update by default* —
you don't need a separate `replace=true` flag, and there is no
"duplicate-rejected" error to handle in the response. Future opt-in
verbs (`replaced`, `duplicate-rejected`) are reserved for a hypothetical
explicit-replace flag and are NOT part of the current return surface.

If your intent is to file a genuinely new entry that overlaps an
existing slug, pick a new slug. The auto-update path is the right
behavior for re-syncing or refining a previously-filed note; it's the
wrong behavior if you meant to file a sibling.

`forge_edit` is supported for vault-note entries: pass `schema_name="vault-note"`, the artifact's `slug`, and any subset of fields (`title`, `body`, `tags`, etc.). Omitted fields stay at their existing values, the file rewrites in place, and the FTS5 index refreshes. Use `Edit` only when you need to author frontmatter the schema doesn't model (e.g., `supersedes:`).

#### Fallback when `forge(vault-note)` errors

If `forge(vault-note)` returns `shape backend not implemented: schema "vault-note" is markdown-target`, the stdio MCP server you're talking to is running a pre-`5a240e6` binary that ships the schema but not the `createMarkdown` write path. Don't burn cycles diagnosing it — fall back immediately:

1. `Write` the file directly under the appropriate subdir (see routing table below), authoring the frontmatter by hand to match the vault-note shape.
2. `knowledge_seeder` will index it on its next pass; until then `vault_search` won't see it.
3. After the user restarts Claude Code (or the stdio toolkit-server otherwise gets the fresh binary), the schema-validated path resumes.

Same fallback applies for any other `shape backend not implemented` error against a markdown-target schema.

Subdir routing is automatic:

| `note_kind` | Resolved path |
|---|---|
| `decision` | `~/.claude/vault/decisions/<date>_<slug>.md` |
| `learning` + `scope=X` | `~/.claude/vault/learnings/X/<date>_<slug>.md` |
| `learning` + no scope | `~/.claude/vault/learnings/general/<date>_<slug>.md` |
| `reference` | `~/.claude/vault/reference/<date>_<slug>.md` |

### Still preferred for these cases: `Write` / `Edit`

- **Schemas the forge schema doesn't cover.** `projects/<project>/`,
  `meta/`, and `scratch/` aren't routed by vault-note. Use
  `Write` for those.
- **Custom frontmatter shapes.** The vault-note schema's frontmatter
  is fixed (date / slug / title / kind / project / tags). If the
  entry needs `supersedes:`, `superseded_by:`, `topic:`, or any
  other key, use `Write` and author the YAML by hand.
- **Throwaway / ad-hoc captures.** If the content might not pass the
  cross-project test on a second look, `Write` to `scratch/` is
  lower ceremony than going through forge.

The two paths coexist; `vault_search` reads from
`knowledge_pointers` regardless of which path created the entry.
Entries authored via `Write` get indexed when `knowledge_seeder`
runs (periodic / on demand); entries authored via `forge` are
indexed inline at create time.

## File shape

### Frontmatter

```yaml
---
date: YYYY-MM-DD          # decisions only; required
tags: [...]               # short kebab tokens; reuse existing where possible
topic: <kebab-topic>      # decisions / reference; one per file
---
```

Date is required for `decisions/` (encoded in filename and
frontmatter). `learnings/` and `reference/` files don't need a date
prefix.

#### Historical drift (bug 1434)

You'll see many existing vault files (~90 with `created:`, ~39 with
`status:`) using older frontmatter shapes:

```yaml
---
created: 2026-05-18       # superseded by `date:`
status: accepted          # superseded — supersession lives in body now
tags: [...]
---
```

**Do not inherit the older shape when authoring new entries.** If you
`vault_read` a pre-canonical exemplar to inform your own entry,
re-derive the frontmatter from this SKILL body, not from the exemplar.
The canonical shape above is the target; the drifted shapes are
tolerated for read but not authored. This is the exemplar-beats-
discipline failure mode bug 1429 codified: a concrete adjacent
exemplar in conversation context tends to win against the discipline
body even when the body is loaded; re-derive from the SKILL on every
authoring pass, don't pattern-match against a recent read.

Migration is on-demand: `forge_edit(schema_name="vault-note", slug=…)`
rewrites in place and refreshes the FTS5 index. Bulk migration is
deferred — most entries are stable and don't need touching.

### Body

- Lead with a one-line title sentence (`# <claim>`).
- **Decisions**: state the choice, then the alternatives weighed,
  then the rationale and the failure mode the alternative would
  hit. A future agent should be able to tell whether the decision
  still applies in a new context — name the *constraints* it
  depended on.
- **Learnings**: state the pattern, the failure mode, and the
  recognition signal that lets a future agent notice it before
  re-deriving.
- **Reference**: state the fact, where it came from, and how to
  verify it again if it goes stale.

### Length

Vault notes are *durable* — write enough that an agent six months
out can act on the content without re-running the conversation
that produced it. Two paragraphs is fine; one sentence usually
isn't. But don't pad — if the insight is one sentence, it probably
belongs in a memory entry, not the vault.

### Supersession

A new decision that supersedes an old one gets a new dated file
plus a one-line edit on the old file:
`Superseded by <new-path>.` Don't delete history — supersession
is itself signal.

## Anti-patterns

- **One-project-only insight in `decisions/` or `reference/`.**
  Specific paths, schema names, or topology that wouldn't read in
  another project. Move to `learnings/<project>/` or project-local
  docs.
- **Memory-shaped content.** User preferences ("I prefer X") and
  Claude-behavior rules belong in memory, written via
  `forge(memory, memory_kind=…)` (NOT a direct dir write — that
  orphans). Vault is for substantive knowledge, not preferences.
- **Toolkit-DB-shaped content.** "Bug X is open" / "task Y is in
  progress" — that's queryable from `mcp__toolkit-server__work`.
  A pointer-only vault entry adds nothing.
- **Skill-shaped content.** A repeatable rule the agent should
  follow on every relevant decision belongs in a skill, not a
  vault note. Vault notes are *consulted* on demand; skills are
  *applied* on every applicable turn.
- **Floating as a question.** "Want me to capture this?" The
  question itself is the friction the discipline eliminates.
- **Re-deriving an existing entry.** Run `vault_search` first; if
  a 90%-overlap entry exists, amend it (or supersede with a new
  dated decision) rather than parallel-filing.

## Composition

- Pairs with **vault-pull-discipline** — pull is the read-side
  reflex (consult vault on task pickup); this is the write-side
  reflex (capture when the cross-project test passes).
- Pairs with **content-routing** — content-routing decides *which
  surface* (vault vs toolkit-DB vs skill); this skill governs *the
  vault write itself* once routing says vault.
- Pairs with **bug-filing-discipline** — bugs capture friction;
  vault captures synthesis. A bug fix may *yield* a vault entry
  (the structural lesson the fix taught), but the bug and the
  vault note are separate writes.

## When NOT to apply

- The insight is project-internal — write to `process-docs/` or a
  project-local skill, not vault.
- The content is a state fact — toolkit DB.
- The content is a user preference or behavior rule — memory (`forge(memory)`).
- The content is a repeatable rule — skill (user-level if it
  generalizes).
- The content is intra-session / intra-bug working memory (todos,
  hypotheses, things-tried, anchors) — that's a scratchpad. See
  `scratchpad-discipline`. Scratchpads are radically intra-session
  and fail the cross-project test by design; mixing them into the
  vault dilutes the cross-project signal `vault_search` relies on.
- An equivalent vault entry already exists — amend or supersede;
  don't parallel-file.

## Boundary with adjacent persistence surfaces

The vault sits in a small family of agent-readable persistence
surfaces under `~/.claude/`. The boundaries are sharper than they
might seem on first read — confusing them is itself a friction
worth filing.

| Surface | Lifetime | Audience | What lives here |
|---|---|---|---|
| **vault** (`~/.claude/vault/`) | Durable; designed to outlive any one session | Any future agent in any project mounting toolkit-server | Cross-project insights, decisions, learnings, reference |
| **scratchpad** (`~/.claude/scratchpads/`) | Durable file, but content is intra-session/intra-bug by intent | The same chain or bug's next session | Todos, hypotheses, things-tried, anchors, resume hints |
| **memory** (canonical `~/.claude/vault/memory/<kind>/`, surfaced at `~/.claude/projects/<project>/memory/`) | Durable; written via `forge(memory)`, surfaced by the materialize hook | Future sessions | User/Claude-behavior facts (kind ∈ user/feedback/project/reference) |
| **toolkit DB** (`mcp-servers/data/toolkit.db`) | Durable | Cross-project queries via MCP | Chains, tasks, bugs, library entries — work-shaped state |

Routing test if you're not sure:

- Would another *project's* agent benefit? → vault.
- Would only the *same chain or bug's next session* benefit? → scratchpad.
- Is it a preference *about Claude* the user expressed? → memory (write it via `forge(memory)`).
- Is it the existence/state of a work artifact? → toolkit DB.

If a scratchpad note turns out to generalize, copy it to vault and
cite the scratchpad as source. If a memory entry turns out to be
cross-project insight rather than personal preference, copy it to
vault. The boundary isn't impermeable, but each surface's default
should be respected.
