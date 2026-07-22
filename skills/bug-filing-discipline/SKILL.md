---
name: bug-filing-discipline
description: "File every observed friction in the unified toolkit DB via `mcp__toolkit-server__work` action `forge` with `kind=bug`. Codifies severity rubric (two-axis observer-impact × blast-radius), surface multi-tag taxonomy, resolve-state decision tree, pre-send ritual phrase list, prefer-fix-over-patch rule, fix→resolve→commit ordering, and the retro-time inter-task-seam / authoring-underspec detectors (work I followed orders to skip + scope I figured out that the spec should have prescribed both count as friction). Cross-project: works in any repo whose `.mcp.json` mounts toolkit-server."
triggers:
  - file a bug
  - file as a bug
  - file that as a bug
  - file it as a bug
  - file the bug
  - forge bug
  - forge(bug
  - resolve bug
  - reopen bug
  - bug_resolve
  - bug_reopen
  - bug_list
  - friction
  - paper cut
  - worth filing
  - want me to file
  - want to file
  - could file
  - separate bug
  - minor observation
  - noted but not filed
  - clippy complaint
  - subagent-flagged
  - silent workaround
  - severity
  - wontfix
  - fixed vs routed
  - routed vs dup
  - fell through the seam
  - fell through the seams
  - unowned work
  - both tasks excluded
  - followed orders but
  - task didn't pick up
  - had to derive
  - had to infer
  - deferred follow-on
  - deferred follow-ons
  - deferred items
  - retro doc lists
  - retro enumerates
  - listed in the retro
  - routing table
  - chain close
  - chain_close
  - orphan
  - orphaned
  - task underspecified
  - figured it out
  - obvious in hindsight
---

# Bug Filing Discipline

Every observed friction goes into the unified toolkit DB as a bug,
filed via `mcp__toolkit-server__work` action `forge` with
`kind=bug`. Memory and hooks point at this skill rather than
duplicating the rules.

The toolkit DB is shared across every project whose `.mcp.json`
mounts `toolkit-server` — bugs filed in one project are queryable
from any other.

## The active discipline — file as the default

Be **ACTIVE**. Most non-trivial sessions surface at least one
filing-worthy friction: a workaround applied silently, a tool that
did the right thing in a surprising way, a spec that underspecified
the work, an inter-task seam, documentation that drifted. A
retrospective that files nothing is rarely a neutral outcome — it's
almost always a missed signal.

**"Nothing to file" is a valid outcome but it should NOT be the
default.** If a session ran cleanly — no workarounds, no surprises,
no derivation of underspecified scope, no doc drift caught — fine,
nothing to file. Otherwise, act. A pass that does nothing is a missed
learning opportunity, not a neutral outcome.

The append-only bug table is designed for paper cuts. Noise closes
with a short cancellation rationale; unfiled friction evaporates.

### Signal taxonomy — any one warrants action

(See "When to file" below for the full list; this is the prescriptive
summary.)

1. **Workaround applied silently.** Fixed in-task without naming the
   friction. The fix is in the diff; the *friction* isn't.
2. **Tool surprise.** Handler did the wrong thing, or the right thing
   in a way that took more calls than it should have. Includes
   correct-but-unergonomic MCP behavior.
3. **Spec underspecified.** Derived scope that should have been
   prescribed: transitive deps not in `context_required`, internal
   modules not listed alongside public surface, build failures from
   missing context.
4. **Inter-task seam.** Work that fell between two tasks that both
   excluded it. "Followed orders" + "next task didn't pick it up" =
   orphan. File even though no single task author is at fault.
5. **Documentation drift.** What docs say doesn't match what the code
   does — stale line counts, stale tool lists, broken conventions.

### Preference order — pick the earliest that fits

1. **FILE AS DUP** against an existing open bug when there's ≥90%
   overlap. Dups are cheap to file and prevent fragmentation. The dup's
   `resolution_note` names the target slug.

   **Search with `knowledge_search`, NOT `bug_list`.** `bug_list` has NO
   text-search param — it accepts only status / severity / surface /
   since / verbose / titles_only / all / limit / offset, so "`bug_list`
   first" can only mean "pull N titles and eyeball them," which does not
   find semantic overlap. `knowledge_search` indexes bugs, tasks, AND
   chains semantically (`source_type`), so it also surfaces the chain
   that already built the thing you're about to file against:

   ```
   knowledge(action="knowledge_search",
             params={query: "<the bug in one sentence>", limit: 5})
   ```

   Corrected 2026-07-16 (session 13c4ecfc): filing bug
   `dependency-driven-roadmap-is-aspirational-…`, the mandated
   `bug_list` scan of 100 titles missed chain 375
   `dependency-driven-roadmap` — the chain that DESIGNED AND BUILT the
   exact feature being reported. One `knowledge_search` call surfaced it
   instantly. Use `bug_list` for what it can do (surface/status
   filtering, step 3 below), not for search.
2. **AMEND an existing open bug** when the new observation extends
   the original without contradicting it. Add to `acceptance_criteria`
   or extend `problem_statement` with a new instance.
3. **FILE NEW BUG with existing surface tags.** Reuse the surface
   taxonomy before inventing new tags — `bug_list({surface:"tag"})`
   to verify.
4. **FILE NEW BUG with a new surface tag** — last resort. Confirm the
   surface isn't already named under a slight variation.

### Do NOT file as bugs (anti-patterns)

These shapes look bug-adjacent but harden into persistent constraints
that bite later when the environment changes:

- **Environment-dependent failures.** Missing binary, unconfigured
  cred, fresh-install path mismatch, "command not found." The user
  can fix these; they aren't durable rules. The fix (install command,
  config step, env var) belongs in a setup skill — never "this tool
  doesn't work" as a standalone bug.
- **Negative claims about tools or features.** "X handler is broken"
  / "Y tool doesn't work for Z." These harden into refusals the agent
  cites against itself months after the actual problem was fixed. If
  a tool failed, file the *fix*, not the broken-tool claim.
- **Session-specific transient errors** that resolved by retry. The
  lesson is the retry pattern (a skill update), not the original
  failure (a bug).
- **One-off observations with no recurrence shape.** Paper cuts count,
  but they need *shape* — a recognizable category that could recur. "I
  noticed this once with no obvious pattern" is below the bar.

The reflex is **fire first, narrate second.** Asking "should I file
this as a bug?" is itself the friction the discipline eliminates.

## When to file — default is *yes*

File **every** observed friction, including paper cuts. Friction
shapes that count:

- Clippy / type-check / test complaints about your own first-draft code.
- Surprising git-status noise — files you didn't expect, untracked
  artefacts, lingering staged edits from a prior session.
- Subagent-flagged tricky cases (something a research subagent had to
  work around or ask about).
- Workflow surprises you silently worked around — a tool that should
  have been the answer but wasn't; a sequence that took more calls
  than it should have.
- Documentation that doesn't match the code (stale line counts, stale
  tool lists, broken conventions).
- MCP handler behaviour that is correct-but-unergonomic.
- **Inter-task seams** — work that should exist but doesn't because
  two related tasks each excluded it from scope. Symptom: "the spec
  told me not to touch X, and I followed orders, but the follow-on
  task didn't pick it up either." The early rubric drafts only
  flagged friction I *worked around* — work I followed orders to
  skip is invisible to that detector. File the orphan as a bug at
  retrospective time even though no single task author is at fault.
- **Task-authoring underspec** — work the spec underspecified that
  the agent had to derive. Symptom: "task listed public call sites
  but not transitive deps"; "build failed because a Cargo dep wasn't
  in context_required"; "had to infer which internal modules
  travelled with the public surface." A single instance is a paper
  cut; a recurring shape across ≥2 tasks is the meta-bug — see
  Recurring-shape escalation.

Default: file. Don't ask "is this big enough?" The append-only bug
table is designed for paper cuts. Noise closes with a short
cancellation rationale; unfiled friction evaporates.

## What the rubric is blind to without help

The friction list above grew from session-end retros. Two failure
modes the rubric *missed* in early drafts, now patched in but worth
calling out so future drafts don't forget:

- **"I followed orders" is not the same as "no friction."** If a
  task constraint kept you from doing obvious work and the
  follow-on task didn't pick it up, the gap is real friction even
  though you executed perfectly. File the orphan.
- **"I figured it out" is not the same as "the task was clear."**
  If you spent time deriving scope that a tighter spec would have
  prescribed, that's authoring friction. File it (lightly for one
  instance; aggressively for a pattern across ≥2 tasks). The bug
  is about the *authoring convention*, not the specific extraction.

- **"Listed in the retro doc" is not the same as "routed."**
  When a chain closes with a list of deferred follow-ons in its
  retrospective, every item must end up at a concrete pickup home
  *before the chain closes*, not just enumerated. Allowed homes are:
    1. **Filed bug** with concrete acceptance criteria (small +
       independent items).
    2. **Task added to an existing chain that's already on the
       roadmap** (item belongs naturally inside that chain's scope
       and the chain is actively flowing).
    3. **New chain forged and roadmap-inserted** (substantial work
       that needs its own design + tasks).
    4. **Explicit condition-gated entry** with the trigger written
       down ("when row count crosses N", "when sibling chain X
       closes", "after 30-day bake-in"). Acceptable when the item
       is real but not actionable yet; the written condition is the
       hand-off to future-self.
  Retro docs growing a "Routing table" section that pairs each
  deferred item with one of the four homes is the verification
  surface — at a glance the table proves nothing orphaned.

  This rule fires during chain close, not at session end. The retro
  question to ask: *"For each deferred item — does this surface in
  `task_list`, `bug_list`, the roadmap, OR have a written trigger
  condition? If none of those, it's an orphan."*

These three surface most strongly at session-end retros, not mid-task.
The retro question to ask: *"What did I do correctly that the next
agent shouldn't have to redo?"* — the answer often names an
inter-task seam, an authoring underspec, or an orphan deferral
worth filing.

## Pre-send ritual

Before every reply that mentions any observation, scan the draft for
these exact phrases:

- "also noted"
- "could file"
- "noted but not filed"
- "want me to file"
- "minor observation"
- "worth filing"
- "separate bug"
- "paper cut"
- "fell through"
- "followed orders"
- "task didn't pick up"
- "had to derive" / "had to infer"
- "deferred follow-on" / "deferred items"
- "listed in the retro" / "retro lists" / "retro enumerates"
- "orphan" / "orphaned"
- "we can come back to" / "will get picked up"
- "figured it out"
- "obvious in hindsight"

For each hit, **stop composing**, call `forge(bug, …)` with
reproduction + expected-vs-observed + fix direction. Rewrite the
sentence to reference the filed slug ("filed as `bug-<slug>`" — not
"could file" or "worth filing"). Then resume.

**Don't float observations and wait for permission to file.** "Could
file this as…" / "want me to file…" pushes the filing decision onto
the user — that's itself the friction the rule eliminates.

## Failure mode: enumerating-but-not-filing at retro time

Bug 1442 codifies an observed shape across two consecutive sessions
(2026-05-18 + 2026-05-19, same agent model, both retros): the user
asked "did you experience friction you didn't feel drawn to file?"
and the agent responded with a **list of candidates** ("here are 4
friction items and 3 vault candidates — which should I file?") rather
than firing the discipline. The user then said "please file all of
them" and only then did filing happen.

The list-then-ask pattern is the same friction "don't float as a
question" rules out — just delayed to retro time. Two specific
recovery cues:

- **Pre-retro mandatory scan.** Before producing any end-of-session
  summary or responding to a "what about friction?" prompt, scan the
  conversation buffer for: workarounds you applied silently, tool
  calls that produced an error envelope you recovered from, scope
  the spec underspecified that you derived, inter-task seams. Every
  match → `forge(bug, …)` BEFORE the summary, not "list them and ask."
- **Self-detect the enumeration shape.** If you find yourself
  drafting a sentence like "I noticed N things worth filing — should
  I file?", that draft is the failure mode. Delete the question.
  File the N items. Then write a summary line that names the slugs
  ("Filed as `bug-<slug-1>`, `bug-<slug-2>`, …").

The reflex is **fire first, narrate second.** Asking "should I?"
re-introduces the per-observation decision the discipline exists to
eliminate.

## Severity rubric

Severity is **two-axis**: observer-impact × blast-radius.

### Axis 1 — observer impact

What breaks for someone downstream of the bug?

- **none** — internal-only; nothing breaks for any observer.
- **paper-cut** — working-but-annoying; the observer notices but
  completes the task.
- **workflow-break** — a specific workflow fails or requires a
  workaround to continue.
- **user-visible-break** — a shipped surface is fully nonfunctional
  or produces wrong output.

### Axis 2 — blast radius

How many places does this bug touch?

- **single-site** — one specific call site, one file, one handler.
- **one-subsystem** — one crate or one feature area.
- **cross-subsystem** — spans multiple crates or crosses an
  architectural boundary.

### Rubric

| observer-impact | single-site | one-subsystem | cross-subsystem |
|---|---|---|---|
| **none** | low | low | low |
| **paper-cut** | low | low | medium |
| **workflow-break** | low | medium | high |
| **user-visible-break** | high | high | high |

When attesting, name both axis values + the assigned severity. If the
rubric says `medium` and you're filing `high`, document the override
reason (or reconsider).

## Surface multi-tag taxonomy

The `surface` field is a **single comma-delimited string** — NOT a
JSON list. Pass `"surface": "dx,workflow"`, never
`"surface": ["dx", "workflow"]` (forge returns
`"field 'surface': expected string, got list"` for the latter).
`bug_list`'s filter splits on `,` and match-any's each token — a bug
tagged only with the primary crate surfaces only on a query of that
exact crate.

### Three tag axes

1. **Primary crate or subsystem** — required. At least one tag like
   `dm-toolkit`, `seed-mcp`, `observability`, `persistence`,
   `workflow-runtime`, etc.
2. **Concern** — cross-cutting concepts: `non-exhaustive`,
   `module-threshold`, `error-boundary`, `surface-filter`,
   `agent-discipline`, `api-ergonomics`, etc. Reuse existing concern
   tags before inventing new ones — search with
   `bug_list({ surface: "concern-tag" })`.
3. **Epic / chain follow-up** (when applicable) — a chain slug or
   epic marker the bug should be picked up under.

### Rules

- Every bug has at least one primary-crate or primary-subsystem tag.
- Cross-cutting bugs get a concern tag for each concern (err toward
  more tags).
- Lowercase-kebab only; no spaces, no uppercase, no underscores.
- Reuse existing tag values when possible.
- Empty surface is the anti-pattern — the bug is unreachable from
  subsystem queries.

## Resolve-state decision tree

When moving a bug from `open` to closed:

```
did a commit land that satisfies the AC?
├── yes → fixed
│        — see prefer-fix-over-patch below before committing
└── no
    ├── does the AC require a design decision that hasn't happened?
    │   └── yes → LEAVE OPEN
    │           wontfix here reads "decided no" when the actual state
    │           is "not yet discussed" — buries a real signal.
    │
    ├── does this need a chain's worth of work that isn't in progress?
    │   └── yes → routed
    │           (resolution_kind='routed' + chain + task pointers)
    │
    ├── is the work out-of-scope for this project?
    │   └── yes → wontfix
    │           rationale must be concrete; "not a bug" is insufficient
    │
    └── does this row overlap ≥90% with an existing open row?
        └── yes → dup
                (resolution_note names the target slug)
```

Use `bug_reopen` when a fix regresses or a newly-observed instance of
the same bug recurs. File a fresh bug when the problem is structurally
similar but the root cause differs — cross-reference the two in their
resolution_notes.

## Prefer-fix-over-patch (mandatory)

When a bug's root cause is architectural, **routed** is preferred over
**fixed** even when the routed path is materially more expensive.

### Patch indicators

- Special-case branch added at a call site to avoid the failing path.
- Lint silenced (`#[allow(...)]`, `// eslint-disable`, etc.) without
  addressing the cause.
- Wrapper / adapter that hides a structural mismatch between two
  layers rather than reconciling them.
- Default-value fallback papering over a data-shape divergence.
- Conditional path that skips a workflow step "for now."

### True-fix indicators

- Removes the class of failure (not just this one instance).
- Resolves the root invariant the failure violated.
- Callers get simpler — not the same, not more complex.
- Affected code paths converge rather than branch.

### Decision

When patch indicators dominate, prefer `routed` over `fixed`. Let the
chain-scoping conversation decide scope. **Do not pre-emptively patch
to avoid opening a chain.** Opening a chain is cheap; propagating
architectural debt is expensive.

When in doubt, route. The chain conversation can scope down to a tight
fix; it cannot undo a patch that's already landed.

## Recurring-shape escalation

Before filing a bug whose surface tag, slug pattern, or
problem-statement shape duplicates ≥2 already-filed bugs (open or
closed) within a recent window, **stop and consider filing the meta-
bug instead** — the structural finding that explains the recurrence —
and route the symptom-bugs to a chain that addresses it.

### Recognition signals

- **Slug-pattern recurrence** — `*-stale-after-vN-bump`,
  `*-cant-X-without-Y`. The repeating fragment names the structural
  cause.
- **Surface-tag recurrence** — three bugs in a short window carrying
  the same primary subsystem tag plus a recurring concern tag.
- **Resolution-note recurrence** — each prior fix's resolution note
  patches one symptom (one fixture, one config value) without naming a
  mechanism that prevents recurrence.

When the signals fire on a third instance: file a meta-bug, route it
to a chain whose output is a structural mechanism (drift detection,
regen pipeline, invariant test), and resolve symptom-bugs as `dup`
against the meta-bug or `routed` to the same chain.

Anti-pattern: filing the fourth and fifth instance because each
individual fix is small. The cost of N small fixes is rarely smaller
than one structural fix once N ≥ 3.

## Fix → resolve → commit ordering

Canonical sequence:

1. Land the fix commit.
2. Call `bug_resolve(slug=…, resolution_kind='fixed', resolution_note=…)` —
   stamps HEAD's SHA as `resolved_commit_sha`.
3. Continue.

### Dirty-tree resolves

`bug_resolve(resolution_kind='fixed')` with a dirty working tree
stamps HEAD's pre-fix SHA, so the resolved row points at a commit
that does not contain the fix.

- Prefer: commit, then resolve.
- If you resolved against a dirty tree, use `bug_stamp_sha(slug,
  commit_sha=<actual-fix-sha>)` after committing to correct the stamp.
- Or pass `commit_sha='<explicit-sha>'` to `bug_resolve` directly.

### SHA shape and validation

The stamp validator (`isValidCommitSHA`) only enforces ASCII-hex shape
and length 7–40. It does **not** check that the SHA resolves to a
commit in any git repo, so a typo, truncated string, or
"close-enough-looking" approximation will silently persist and break
future audits.

- Always paste the **full 40-character SHA** — never a short hash, never
  a "first-7-of-the-commit-message" guess. Get it from `git rev-parse HEAD`
  or `git log -1 --format=%H` after the fix commit lands.
- The sentinel `'unversioned'` is the only acceptable non-SHA value.

### `bug_stamp_sha` vs `task_stamp_sha` response keys

The two stamp endpoints emit the SHA under different JSON keys — a
wire-format asymmetry preserved across the Rust→Go migration for
backwards compatibility:

- `bug_stamp_sha` → `{ok, slug, resolved_commit_sha}`
- `task_stamp_sha` → `{ok, slug, commit_sha}`

Parity-aware clients (dashboards, audits) need to read both keys.
Inputs are uniform — pass `commit_sha=<sha>` to either endpoint.

### Batching

Multiple bugs fixed by one commit can be resolved sequentially against
the same HEAD — no re-commit per bug.

### Unversioned-artifact exception

Some bugs document friction whose in-scope mitigation lives outside
any git repo — user-level scripts under `~/.claude/`, vault docs under
`~/.claude/vault/`, dotfile config, or any other unversioned location.
There is no commit SHA to stamp.

When `bug_resolve(resolution_kind='fixed')` lands in this shape, pass
`commit_sha='unversioned'` (or use `bug_stamp_sha(slug,
sha='unversioned')` after the fact). The sentinel marks the row as
resolved-with-no-git-commit so audits don't treat it as missing-stamp.

Required when using the sentinel:

- The `resolution_note` names full paths to the unversioned artifacts
  that constitute the fix. The note is the receipt; without paths, the
  resolved row is opaque.
- The artifacts genuinely exist on disk and satisfy the AC at resolve
  time. The sentinel is not an escape hatch — verify before stamping.

If you find yourself reaching for the sentinel on a project-internal
fix, stop: a real commit is the right answer there. The sentinel
applies only when the fix shape is fundamentally unversioned (the
artifact's home directory is not a git repo).

## Forge call shape

`forge` takes a top-level `kind` + optional `slug` + flat field params.
`slug` is **optional** — when omitted, forge auto-derives it from
`title` (lowercase, punctuation→hyphens). Supply a slug explicitly only
when you want a specific canonical identifier.

```
mcp__toolkit-server__work(
  action="forge",
  project="<project>",
  params={
    "kind": "bug",
    "title": "<one-line human sentence>",
    "problem_statement": "<reproduction + expected/observed + fix direction>",
    "surface": "<comma,kebab,tags>",
    "severity": "low|medium|high",
    "source": "<where surfaced>",
    "acceptance_criteria": "<bullet text>",
    "constraints": "<don't-over-fix notes>"
  }
)
```

`bug_resolve` parameter is `resolution_kind`, not `kind`. Accepts
`commit_sha` or `sha` as alias.

### Read-call shape — project belongs at top level, not under params

For `bug_list`, `bug_read`, `bug_resolve`, `bug_reopen`, and `bug_stamp_sha`,
the project scope is the **top-level** `project` field consumed by
dispatch.Args — *not* a `project_id` key inside `params`. Passing
`params.project_id` is silently ignored: the call succeeds, the response is
`[]`, and there is no shape-mismatch hint. Same convention applies to
task / chain / roadmap read actions.

```
# Correct
mcp__toolkit-server__work(action="bug_list", project="mcp-servers")

# Silently wrong — returns []
mcp__toolkit-server__work(action="bug_list", params={"project_id": "mcp-servers"})
```

## Composition

- Pairs with **content-routing** — content-routing decides *which
  surface* a piece of content lands on; this skill governs *what kind
  of work artifact* (bug vs task vs nothing) gets created on that
  surface.
- Pairs with **scratchpad-discipline** — that skill governs the
  agent's persistent working memory during bug investigation
  (hypotheses, things tried, repro details) at
  `~/.claude/scratchpads/bugs/<bug-slug>.md`. This skill (bug-filing)
  governs the durable bug record itself. The scratchpad is the
  exhaustive intra-investigation log; the bug's `resolution_note` is
  the durable summary distilled from it at resolve time.
- A project's pre-commit / commit discipline complements the fix →
  resolve → commit ordering rule. Don't resolve before committing.

## When NOT to apply

- Casual conversation about an artifact already filed elsewhere — no
  new bug needed.
- Documenting a decision that isn't a friction — that's a vault entry
  (`decisions/`), not a bug.
- A task already exists for the work — `task_search` first; don't
  parallel-file a bug.
