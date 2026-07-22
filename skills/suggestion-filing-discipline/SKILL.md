---
name: suggestion-filing-discipline
description: "Activate when the user asks to file improvement ideas from this session into the suggestion box, OR when you yourself spot a GAP — a missing feature or affordance in your own tooling that is not a defect (a defect is bug-filing-discipline's job). Agent-spotted gaps are a first-class trigger: noticing what your instruments lack is an observation you are uniquely placed to make, and it does not wait for an invitation. Codifies the dedupe-search-first protocol (prefer `knowledge_search`, which indexes bugs/tasks/chains semantically — `suggestion_list`/`bug_list` have no text search), the native suggestion vocabulary (priority/adopted/deferred/rejected — NOT severity/fixed/wontfix), the broadened surface taxonomy that spans testing/lint/docs/tooling/prose/architecture/skill/workflow, and the verbatim friction-vs-suggestion definition that's the single source of truth shared with the arcreview Qwen pipeline. Cross-project: works in any repo whose `.mcp.json` mounts toolkit-server."
triggers:
  - i noticed a gap
  - missing feature
  - missing affordance
  - the tooling lacks
  - there's no way to
  - no mechanism for
  - undocumented constraint
  - would be nice if
  - file a suggestion
  - file as a suggestion
  - file it as a suggestion
  - file the suggestion
  - forge suggestion
  - forge(suggestion
  - resolve suggestion
  - reopen suggestion
  - suggestion_resolve
  - suggestion_reopen
  - suggestion_list
  - suggestion box
  - improvement ideas
  - add to the suggestion box
  - any improvements
  - any suggestions
  - session retro suggestions
  - what would you change
  - what would you improve
  - file improvement ideas
  - file anything you noticed
  - file anything you'd like
  - put it in the suggestion box
  - propose
  - propose a change
  - propose an improvement
  - small refactor
  - missing test
  - prose tidy
  - skill update
---

# Suggestion Filing Discipline

When the user explicitly asks for improvement ideas from the session
("add anything you'd like to the suggestion box", "file any improvements
you noticed", "what would you change about how this went?"), the reflex
is `forge(suggestion, …)` — NOT `forge(bug, …)`. Suggestions and bugs
are separate entities in the toolkit DB (different table, different
vocabulary, different lifecycle) because the work they describe is
fundamentally different.

The toolkit DB is shared across every project whose `.mcp.json` mounts
`toolkit-server` — suggestions filed in one project are queryable from
any other.

## When this skill fires — two triggers, not one

**Corrected 2026-07-16 (Sophi, session 13c4ecfc).** This skill previously
declared itself user-prompted ONLY, on the reasoning that a suggestion is
*a forward-looking proposal to revisit a past decision*, which an agent
shouldn't fire on unbidden. That reasoning is sound — but it was reasoning
about a SUBSET. A suggestion is also, and mostly, **an agent-spotted gap
in its own tooling**: a missing feature or affordance, not a defect. The
old gate suppressed exactly the filings the surface exists to collect, and
did so silently (see suggestion `task-title-silently-derived-from-problem-
statement-first-line-undocumented`, found and then withheld under the old
rule in the very session that corrected it).

### Trigger 1 — you spot a gap (fires on its own, no invitation needed)

You are noticing something about **your own instruments**: a missing
feature, an absent affordance, an undocumented constraint, a step that
should exist and doesn't. This is not presumptuous and does not wait to be
asked — it is the observation you are best placed to make and the user is
worst placed to make. **ACT, don't ask** (see memory
`act-dont-ask-on-proactive-filing`). Asking "want me to file that?" for a
gap this discipline already covers is itself a discipline violation.

Gap vs defect, the sorting rule:

- The mechanism does what it says, and the shortfall is that **it doesn't
  exist / isn't reachable / isn't disclosed** → **suggestion**.
- The mechanism claims something and **does otherwise**, or two paths
  disagree → **bug** (see bug-filing-discipline).

When genuinely torn: file a bug. Under-filing is the worse failure; a
mis-shelved item is recoverable via the routed_bug_slug /
routed_suggestion_slug pointers, an unfiled one is not.

### Trigger 2 — the user invites proposals

- "add anything you'd like from this session to the suggestion box"
- "file any improvement ideas you noticed"
- "what would you change about how this went?"
- (any phrasing explicitly inviting forward-looking proposals)

### Nothing waits to be asked — the name IS the design

**Corrected again, same session, by Sophi:** *"consider the name — a
suggestion. Not a demand or a new feature, just a suggestion. Never really
any harm in filing that."*

The first correction added agent-spotted gaps but kept a narrowed gate:
"don't unilaterally propose relitigating a decision the user deliberately
made — that one waits." **That gate is also gone.** It modelled filing as
if it were a demand that obligates the reader. It isn't. The surface is
named to defuse exactly that, and the lifecycle is built to absorb it:
`rejected` and `deferred` are normal, cheap, expected outcomes, not
failures. The entire cost of a bad suggestion is Sophi spending two
seconds declining it.

**The asymmetry that settles it:** a rejected suggestion leaves a record —
you can query what was considered and declined. An unfiled suggestion
leaves nothing at all. The DB can never show you what an agent withheld.
So under-filing is the failure mode that is invisible by construction, and
a discipline should be biased against the failure it cannot see.

This means even "this past decision may have aged badly" is fileable
unprompted. It's a suggestion. She can reject it.

### What remains is a standard, not a gate

The only live question is **"is this worth someone's read?"** — quality and
volume, not permission. Fifty low-value suggestions per session impose a
real triage cost. The answer to that is filing things worth filing, never
waiting to be asked.

- Standard: *is this any good?* → always applies.
- Gate: *am I allowed?* → does not exist here. Do not reinvent one.

If you catch yourself drafting the sentence "I noticed X but didn't file
it because you hadn't asked" — that sentence is the bug. File it.

## The verbatim friction-vs-suggestion definition

This is the single source of truth shared with the arcreview Qwen
filing-decision pipeline — both surfaces read this exact string at
startup to ensure agent and Qwen apply the same rule without drift:

> Friction is something that interrupted the normal flow, slowed you
> down, confused you, and is unintentional in our design. Suggestions
> are friction which go against past decisions in favour of optimizations
> we can see now, argue for removing content from code/prose if it serves
> no purpose, suggest missing tests, suggest code cleanup that will help,
> etc.

Apply the rule per observation:

- An error you recovered from silently → **bug** (observed friction, file proactively).
- A spec that underspecified the scope you derived → **bug** (observed friction).
- A workaround you applied because the canonical path was awkward → **bug**.
- "The current X approach works but Y would be cleaner" → **suggestion**.
- "We could add a test for Z that doesn't exist" → **suggestion**.
- "This skill section is stale / could go" → **suggestion**.
- "The naming convention for these structs doesn't match the rest of the package" → **suggestion**.

The boundary is *intent vs unintent*: bugs document unintentional
friction that already happened; suggestions propose forward-looking
optimizations against intentional past decisions.

## Mandatory dedupe search — first call before forge

Before every `forge(suggestion, …)` call, run `suggestion_list({})`
(optionally narrowed by `surface`) and scan for near-duplicate titles.
A near-duplicate means update the existing entry via `forge_edit(suggestion, …)`
rather than spawn a new row.

```
# Step 1: dedupe scan
mcp__toolkit-server__work(action="suggestion_list", params={"surface": "arcreview"})
# scan returned titles for shapes that overlap with what you're about to file

# Step 2: either forge_edit (existing) or forge new
mcp__toolkit-server__work(
  action="forge",
  project="<project>",
  params={
    "schema_name": "suggestion",
    "title": "...",
    "problem_statement": "...",
    "priority": "low|medium|high",
    "source": "session retro on <YYYY-MM-DD>",
    "surface": "<comma,kebab,tags>",
    "tags": "<comma,kebab,tags>"
  }
)
```

Suggestions accrete over time — the same idea surfaces in retros across
multiple sessions. Dedupe-first keeps the table coherent; otherwise it
fragments into N near-copies and the deferred / rejected resolution
states stop being meaningful.

## Native vocabulary — distinct from bug

| Suggestion field | Bug field | Notes |
|---|---|---|
| `priority` (low/medium/high) | `severity` (low/medium/high) | Priority = "how much would this improve things"; severity = "how broken is this". |
| `resolution_kind`: `adopted` / `deferred` / `rejected` | `resolution_kind`: `fixed` / `wontfix` / `upstream` / `dup` / `routed` | `suggestion_resolve` REJECTS bug-side kinds with an explicit error naming the suggestion enum. |
| `routed_bug_slug` (suggestion→bug) | `routed_suggestion_slug` (bug→suggestion) | Bidirectional cross-table routing. |

`adopted` + `routed_chain_slug` + `routed_task_slug` + (optional
`commit_sha`) is the canonical "this shipped" shape — there is no
separate `implemented` kind. Use `routed_bug_slug` when adoption
uncovers a concrete fix tracked as a bug.

## Surface tag taxonomy — broadened beyond bug's

Suggestions span "anything at all":

- **testing** — missing test, brittle test, test-shape improvement
- **lint** — lint rule we should add, lint suppression worth replacing
- **docs** — docstring drift, README staleness, comment cleanup
- **tooling** — local script, dev-loop ergonomic, build-config tidy
- **prose** — wording clarity in shipped strings (error messages, action-doc descriptions, skill bodies)
- **architecture** — structural refactor proposal that goes against an explicit past decision
- **skill** — skill body update, new skill, retire old skill
- **workflow** — chain shape, task spec template, ritual prose

Multi-tag is encouraged: `tags="testing,prose"` is fine when both apply.

## Resolution vocabulary — only `suggestion_resolve` writes it

- **adopted** — the proposal lands. May ship in this session (`commit_sha`
  supplied → defaults `kind=adopted`) or queue work in a chain (supply
  `routed_chain_slug` + `routed_task_slug`; stamp the `commit_sha` later
  via `suggestion_stamp_sha`).
- **deferred** — agreed in principle, not now. `resolution_note` typically
  carries the revisit signal — when the conditions change, `suggestion_reopen`
  and then adopt.
- **rejected** — the proposal is declined. `resolution_note` carries the
  reasoning.

`forge_edit(suggestion, …)` updates content fields; it does NOT touch
lifecycle. Status transitions go through `suggestion_resolve` /
`suggestion_reopen` so the events ledger picks up a typed
`SuggestionResolved` / `SuggestionReopened` payload.

## Pre-send ritual

Before every reply that emits suggestion-shaped observations (typically
in response to "add anything you'd like to the suggestion box"), scan
the draft for these exact phrases:

- "would be cleaner"
- "could be improved"
- "would help if"
- "minor improvement"
- "small refactor"
- "missing test"
- "stale prose"
- "we could add"
- "could remove"
- "should consider"
- "worth proposing"
- "could propose"
- "want me to file as a suggestion"

For each hit, **stop composing**, run `suggestion_list({})` for dedupe,
then call `forge(suggestion, …)` with the proposal shape. Rewrite the
sentence to reference the filed slug ("filed as `<suggestion-slug>`" —
not "could file" or "worth proposing"). Then resume.

**Don't float observations and wait for permission to file** — when the
user has invited the discipline, the dispatch IS the permission. Asking
"should I file these N items as suggestions?" replicates the same retro-
time enumeration failure mode bug-filing-discipline calls out.

## Contrast with bug-filing-discipline

- **Trigger**: bug-filing fires proactively on observed friction; this
  skill fires only on user invitation.
- **Vocabulary**: bug uses severity/fixed/wontfix/upstream/dup/routed;
  suggestion uses priority/adopted/deferred/rejected. Each handler
  rejects the other's kinds with an explicit error.
- **Voice**: bug.problem_statement is observational (reproduction +
  expected vs observed); suggestion.problem_statement is the case for
  the change (why this would help; what would be different after).
- **Don't double-file**: a single observation belongs in one surface,
  not both. If the same shape qualifies as both — e.g. "this prose
  drifted because step X is awkward and we should also tidy it" — file
  the friction as a bug (the prose drift), then optionally adopt-routed
  to a suggestion if there's a forward-looking improvement worth queuing
  separately.

## Contrast with vault-filing-discipline

Suggestions are not vault notes:

- Vault notes are cross-project decision/learning/reference records
  intended to inform future agents on similar problems.
- Suggestions are *queued work*: a forward-looking proposal that
  resolves via `adopted` / `deferred` / `rejected`, with optional
  routing into a chain+task.

If a session retro surfaces a *learning* (a non-obvious thing the agent
discovered, useful next time), that's a vault note. If it surfaces a
*proposal* (a thing the project should do that goes against a past
decision), that's a suggestion. Both can land from the same session.

## Composition

- Pairs with **bug-filing-discipline** — bugs capture observed friction
  proactively; this skill captures user-invited forward-looking
  proposals. The two surfaces stay distinct in vocabulary and lifecycle.
- Pairs with **vault-filing-discipline** — vault captures durable
  decisions/learnings/references; suggestions are queued *work*.
- Pairs with **scratchpad-discipline** — scratchpads track intra-session
  state; suggestions are durable records that survive past the session.
- The arcreview Qwen filing-decision pipeline (`go/internal/arcreview/`)
  also emits `forge_suggestion` decisions, gated at a higher confidence
  threshold (0.90 vs 0.85 for bugs/vault-notes) because forward-looking
  proposals are noisier than observed-friction filings. The verbatim
  friction-vs-suggestion definition above is the single source of truth
  shared between this skill and the Qwen prompt.

## Examples

### Filing a clean adoption-ready suggestion

```
mcp__toolkit-server__work(action="suggestion_list", params={"surface": "roadmap"})
# scan: no overlap

mcp__toolkit-server__work(
  action="forge",
  project="mcp-servers",
  params={
    "schema_name": "suggestion",
    "title": "roadmap_list lacks the FTS5 coverage other lists have",
    "problem_statement": "Other list surfaces (bug_list, task_search, knowledge_search) all back onto FTS5 virtual tables for substring-match. roadmap_list currently scans the full table on every query. Adding a roadmap_fts shadow table would close the gap with minimal new code — pattern is already established by knowledge_pointers_fts and bugs_fts/suggestions_fts.",
    "priority": "medium",
    "source": "session retro on 2026-05-20",
    "surface": "roadmap,fts5",
    "tags": "architecture,tooling",
    "acceptance_criteria": "roadmap_fts virtual table exists\nroadmap_list accepts a 'q' filter that drives an FTS5 MATCH\napp-code maintains roadmap_fts in the parent tx (no triggers)\nbackfill from existing roadmap_items in the migration"
  }
)
```

### Filing a prose-tidy suggestion

```
mcp__toolkit-server__work(action="suggestion_list", params={"surface": "skill"})

mcp__toolkit-server__work(
  action="forge",
  project="mcp-servers",
  params={
    "schema_name": "suggestion",
    "title": "bug-filing-discipline duplicates the forge-call shape across 3 sections",
    "problem_statement": "Sections 'Forge call shape', 'Composition', and the examples block all rewrite the kind=bug call template. A reader scrolling for the canonical form sees 3 near-identical snippets. Collapsing to one canonical block + 2 references would tighten the skill by ~30 lines.",
    "priority": "low",
    "source": "session retro on 2026-05-20",
    "surface": "skill,bug-filing-discipline",
    "tags": "skill,prose"
  }
)
```

### Adopting + routing to a chain

```
mcp__toolkit-server__work(
  action="suggestion_resolve",
  project="mcp-servers",
  rationale="Adopted in agent-suggestion-box T6; the migration lands the FTS5 backing other lists already have. Resolved at decision time; suggestion_stamp_sha will follow with the implementing commit.",
  params={
    "slug": "roadmap-list-lacks-fts5-coverage-other-lists-have",
    "kind": "adopted",
    "routed_chain_slug": "roadmap-fts5-coverage",
    "routed_task_slug": "migration-add-roadmap-fts"
  }
)
```

### Stamping the commit SHA later

```
mcp__toolkit-server__work(
  action="suggestion_stamp_sha",
  project="mcp-servers",
  rationale="implementing commit landed in main",
  params={
    "slug": "roadmap-list-lacks-fts5-coverage-other-lists-have",
    "sha": "abc1234"
  }
)
```

## When NOT to apply

- The user did NOT prompt for improvement ideas — file observed
  friction as a bug; capture decisions as a vault note; leave
  suggestions empty.
- A suggestion already exists for the proposal — `suggestion_list`
  first; use `forge_edit(suggestion, …)` to update content if needed.
- The work is already queued in a chain+task — no parallel suggestion
  needed; the chain IS the queued work.
- The proposal is about observed-friction-that-already-happened — that's
  a bug, not a suggestion.

## SKILL artifact location

This skill lives at `~/.claude/skills/suggestion-filing-discipline/SKILL.md`,
outside any git repo. The chain's `agent-suggestion-box` task that
authored this skill resolves its commit_sha as `unversioned` per the
toolkit convention for artifacts that live outside the repo.
