# Arc-Close Filing Review — design

**Chain:** `arc-close-filing-review` (roadmap position 5 in mcp-servers)
**Status:** T2 design — implementation tasks T3-T5 land against this spec
**Originating audit:** 2026-05-19 — Sophi's current proactive-filing
approach (memory, skills, retired Stop hook, substrate-mediated-
discovery decision) was audited against a known-working external pattern
from another agent project; the audit findings inform the three
structural moves enumerated below.

## Mission

Substrate-mediated proactive filing: on detected arc-close, the
substrate fires a Qwen-driven review against a conversation snapshot
and dispatches typed filing decisions back to the agent (or executes
them autonomously in bridge-harness). Replaces the agent-internalized
"compulsion to file" — which the chain-close-self-audit failure mode
demonstrated as procedurally incomplete — with a structurally-enforced
firing mechanism.

This doc specifies the **whole mechanism**, not just the detector. The
detector is one of several components; treating it in isolation
produces a thin design. The directive (2026-05-19) was an honest mock,
so the doc covers triggers + dispatch + review surface + schema +
telemetry as one design.

## The three structural moves the design adopts

The audit named three load-bearing structural moves that make
proactive filing reliable when the agent-internalized version was not:

1. **Scheduled trigger** — counter-based, not context-shape. Fires on
   every Nth user turn (and on every Mth tool iteration when the
   harness supports that signal). Independent of what's in the
   conversation.
2. **Computational separation** — the review runs in a runtime context
   separate from the main agent loop, against a snapshot of the
   conversation + a dedicated review prompt + a restricted output
   surface. The main loop never blocks; the review prompt's cache
   stays warm.
3. **Prescriptive prompt language** — "Be ACTIVE"; "nothing-to-file is
   valid but should NOT be default"; signal taxonomy; preference order;
   "missed learning opportunity, not a neutral outcome."

This design adopts all three, with one substitution: the firing surface
is Qwen via llama-server (not a recursive Claude subagent), per the
orchestrator-swap framing. Future ML upgrade is a hot-swap through
`ml-capability-substrate`'s A/B harness.

## Trigger model — inclusive primary

Per Sophi 2026-05-19: **inclusive bias**, tune down later from telemetry
on false-positive rate. Worst case is filing a few too many bugs.

Three independent trigger paths fire into the same debouncer:

### A. Counter-based (harness-driven)

Counter-based scheduled trigger — the first of the three structural
moves named above. Fires regardless of conversation content.

| Counter | Initial threshold | Source |
|---|---|---|
| User turns since last review | **5** | Stop hook counts `Stop` events with `is_user_turn=true` (harness state) |
| Tool iterations since last review | **10** | Stop hook counts `tool_call` events per turn (harness state) |

Reset on every successful review fire.

### B. Event-based (substrate-driven)

Listens to `agent-first-substrate` events table. Any of these events
fires a review:

| Event type | Why |
|---|---|
| `CommitLanded` | Commits are the canonical end-of-work-unit signal |
| `TaskCompleted` | Task close is an arc-close by definition |
| `ChainClosed` | Bigger arc-close; multiple tasks just settled |
| `BugResolved` | Resolution rebuilds context that may surface adjacent friction |
| `RoadmapUpdated` | Roadmap edits indicate planning-tier reflection |

Future events to add as they accumulate enough volume to be useful:
`MultipleFileMoves` (≥5 file moves within W seconds), `LargeDiff`
(commit > 200 lines), `ChainStateTransition` (open→active, etc.).

### C. User-shape (harness-driven)

Stop hook scans the user's last message for arc-close shapes.
Inclusive trigger list (kebab-bounded):

- `\bdone\b`
- `\bthanks?\b`
- `\bwrapping\b`
- `\bthat'?s\s+all\b`
- `\blooks?\s+good\b`
- `\b/clear\b`
- `\bsession\s+end\b`
- `\bany(thing)?\s+else\b`

Case-insensitive substring match. Inclusive by design; debouncer
prevents over-firing.

### Debouncer

Multiple triggers within W = **30 seconds** coalesce to a single
review fire. The fire payload carries all triggers that contributed
(for telemetry).

Backoff: after a review fires, suppress new fires for B = **60
seconds** regardless of triggers. Prevents runaway firing during
high-activity sessions.

## Architecture

Two independent firing paths land on one MCP action; the MCP action
is the single dispatcher.

```
  [harness side: Stop hook]              [substrate side: event listener]
       │                                       │
       │ counters + user-shape match           │ event-table tail
       │                                       │
       └─────────────────► [debouncer] ◄──────┘
                                │
                                ▼
                   work.review_arc_for_filing
                   (MCP action on toolkit-server)
                                │
                                ▼
                Qwen via llama-server (127.0.0.1:8081)
                Qwen2.5-32B with structured-output schema
                                │
                                ▼
                   filing_decisions[] payload
                                │
                ┌───────────────┴───────────────┐
                │                               │
                ▼                               ▼
        [Claude Code: agent dispatch]   [bridge-harness: auto-execute]
        in-band forge calls             post_turn hook fires forges
        per filing_decisions[]          autonomously
```

### Why both firing paths

- **Substrate-side** catches arc-closes the agent never sees: async
  work, multi-session work, work where the agent commits + immediately
  starts the next thing. Counter-based scheduled triggers fire
  regardless of conversation content.
- **Harness-side** catches user-shape arc-closes that don't trigger
  ledger events: "thanks, that's all", explicit session ends.
  Direct user escalation by message-shape.

Both paths run side-by-side: scheduled counters + user-triggered
shapes feed the same debouncer.

### Why parallel processes, not in-band on the main loop

The audit pattern uses a daemon thread for the review so the parent
agent's main loop never blocks and the review's prompt cache stays
warm. This design achieves the same property via two separate runtime
contexts:

- **Stop hook**: runs in a separate OS process from the Claude Code
  session. The main session continues unimpeded; the hook fires its
  MCP action async and returns the payload via a system-reminder
  injection on the next user turn.
- **Substrate event listener**: runs as a goroutine in toolkit-server,
  separate from the request-handling goroutines. The event fold
  triggers asynchronously; the review fires; the result is stored as
  an `ArcCloseFilingReviewed` event row.

Neither path blocks the main session. The parallel-execution property
is preserved.

## Review surface — Qwen with structured output

### Why Qwen (not Claude subagent)

Per Sophi 2026-05-19: a Claude subagent costs a full Claude turn
(~$0.05-0.20 paid, ~30s). Qwen via llama-server is local (~$0,
~1s, deterministic with seed). The directive was explicit:
"explore having either Qwen or an ML model handle that, just like
our own context function that gives you data from our search index."

`parse_context` already uses Qwen-rerank for its discovery surface;
this chain ports the same pattern to the firing surface. Substrate
symmetry: pull-side (parse_context) and push-side (this chain) both
hit Qwen via the same llama-server endpoint.

### Future upgrade: trained classifier

Per `ml-capability-substrate` (closed 2026-05-19), the Qwen→ML swap
is the A/B harness pattern. Once telemetry corpus from this chain
accumulates (~500 reviews), a trained classifier becomes a
ml-opportunity-scan candidate:

- Slug: `trained-arc-close-filing-classifier-v1`
- Inputs: snapshot embedding + arc-summary embedding + trigger
  signal one-hot
- Outputs: filing_decisions[] in same schema as Qwen output
- Latency target: ≤ 50ms p95 (vs Qwen's ~1s)
- Promotion gate: filing precision/recall matches Qwen baseline on
  held-out slice

### Structured-output schema (the action whitelist)

The schema *is* the action whitelist. Qwen cannot propose actions
outside this taxonomy because the schema rejects them. The whitelist
is enforced at the response parser instead of at call time —
malformed actions never make it to a forge call.

```json
{
  "filing_decisions": [
    {
      "action": "forge_bug" | "forge_vault_note" | "skill_update"
              | "memory_write" | "nothing_to_file",
      "payload": {
        // action-specific shape — see below
      },
      "confidence": 0.0..1.0,
      "reasoning": "one-sentence why-this-decision"
    }
  ],
  "summary": "one-paragraph human-readable arc summary"
}
```

Per-action payload shapes:

**`forge_bug`** (matches `mcp__toolkit-server__work` forge bug shape):
```json
{
  "title": "...",
  "problem_statement": "...",
  "surface": "comma,kebab,tags",
  "severity": "low|medium|high",
  "tags": "..."
}
```

**`forge_vault_note`** (matches vault-note forge shape):
```json
{
  "note_kind": "decision|learning|reference",
  "title": "...",
  "body": "markdown body",
  "tags": "..."
}
```

**`skill_update`** (extends or patches an existing skill):
```json
{
  "skill_slug": "...",
  "patch_kind": "add_section|extend_paragraph|add_trigger",
  "content": "..."
}
```

**`memory_write`** (auto-memory feedback entry):
```json
{
  "memory_kind": "user|feedback|project|reference",
  "name": "kebab-slug",
  "description": "one-line",
  "body": "markdown"
}
```

**`nothing_to_file`**: payload is `null`; reasoning explains why.

### Arc-summary pre-call

Before the main review prompt fires, a short Qwen call generates the
arc_summary from the snapshot. Prompt: "In one paragraph, summarize
the activity in this conversation snapshot — what was the agent
working on, what was accomplished, what surprises or workarounds
occurred." Output flows into the main review prompt's `{arc_summary}`
placeholder. ~500ms latency on Qwen2.5-32B; total per-review cost
becomes ~1.5-2s.

### Review prompt — prescriptive, references T1 skill bodies

Prompt template (sketch — final wording in T4 implementation):

```
ROLE: You are reviewing a recent activity arc for filing-worthy
content. Be ACTIVE — most non-trivial arcs surface at least one
filing-worthy bug, vault note, skill update, or memory write. A
pass that does nothing is a missed learning opportunity, not a
neutral outcome. "Nothing to file" is a valid outcome but should
NOT be the default.

SIGNAL TAXONOMY (full content in vault-filing-discipline.SKILL.md
and bug-filing-discipline.SKILL.md — summarized here):

  Vault-worthy (any one warrants action):
    • Decision made + rationale that constrains future choices
    • Lesson re-derived (pattern, gotcha, failure mode)
    • Reference assembled (durable how-to for future-you)
    • Cross-project framing in user-facing text

  Bug-worthy (any one warrants action):
    • Workaround applied silently
    • Tool surprise (correct-but-unergonomic)
    • Spec underspecified (derived scope)
    • Inter-task seam (orphan work)
    • Documentation drift

  Skill-worthy:
    • User corrected style/format/workflow
    • New technique that future sessions would benefit from
    • Existing skill turned out wrong/missing

  Memory-worthy:
    • User revealed preferences/expectations
    • Durable user-behavior fact

DO NOT FILE (anti-patterns):
  • Environment-dependent failures (missing binaries, etc.)
  • Negative claims about tools ("X is broken")
  • Session-specific transient errors that resolved by retry
  • One-off observations with no recurrence shape

PREFERENCE ORDER (pick the earliest that fits):
  1. AMEND or SUPERSEDE existing artifact (≥80% overlap)
  2. EXTEND an existing umbrella
  3. FILE NEW

CONVERSATION SNAPSHOT:
{snapshot}

ARC SUMMARY (auto-derived):
{arc_summary}

TRIGGER SIGNAL:
{trigger_signal}

TASK: Output a `filing_decisions[]` array in the schema above. Each
decision either FILES something or returns `nothing_to_file` with
clear reasoning. Be specific. Confidence ≥ 0.85 = auto-execute;
0.50-0.85 = surface for confirm; <0.50 = skip.
```

The prompt is built dynamically per fire — the snapshot + arc_summary
are session-specific; the rest is template.

## Filing dispatch

**Scope of "auto-execute" — important.** This stack auto-executes by
FILING ONLY — bug rows, vault notes, suggestions (future, chain 11
agent-suggestion-box), memory entries. **No code modification, no
file writes outside the filing surfaces.** The constraint: we trust
Qwen to surface filings; we do NOT trust Qwen (or future trained
classifier) to write code or modify non-filing files. The
auto-execute path's blast radius is bounded to toolkit-db forge calls
+ the auto-memory write path. Skill updates (action `skill_update`)
surface for confirm even at high confidence —
they edit live skill files, which crosses the filing/fixing line.

Two consumer paths:

### Claude Code (today)

Stop hook's flow: (1) Qwen review returns `filing_decisions[]` with
confidence scores. (2) Hook partitions decisions into
auto-execute (confidence ≥ 0.85 AND action in {forge_bug,
forge_vault_note, memory_write}) vs surface-for-confirm (0.50 - 0.85
OR action == skill_update at any confidence) vs skip (< 0.50).
(3) For auto-execute decisions, the hook calls toolkit-server's HTTP
endpoint directly (curl POST to `/mcp/work/forge` etc.) — landing the
forge before the next user turn even begins. (4) Surface-for-confirm
decisions land as a system-reminder on the next user turn, formatted as:

```
<system-reminder>
Arc-close filing review fired (trigger: {trigger_signal}). Qwen
returned {N} filing_decisions. Review and execute:

1. [confidence=0.92] forge_bug "long-running-tasks-stale-after-30min" — ...
2. [confidence=0.71] forge_vault_note "..." — surfacing for confirm
3. [confidence=0.84] skill_update vault-filing-discipline ...

Execute via mcp__toolkit-server__work.forge(...) per the typed
payloads. Decisions with confidence ≥ 0.85 should auto-execute;
0.50-0.85 confirm explicitly; <0.50 skip.
</system-reminder>
```

The agent then dispatches forges in-band on its next response.

This is the v1 integration. It still requires the agent to act on the
payload, but the agent now has typed decisions (not a question — a
list of forges to execute). The enumeration-then-ask failure mode is
structurally impossible because there's nothing to enumerate; the
decisions are already made.

### bridge-harness (future — chain 17)

bridge-harness's `post_turn` hook fires the same MCP action, but
dispatches forges autonomously (high-confidence auto-execute; medium
surfaces to the user via the harness's hook surface; low skip). Full
substrate-side execution: review fires, decisions execute, agent
never sees the firing decision in-band.

Until chain 17 lands, the Claude Code Stop-hook path covers the case.

## Telemetry — corpus from day one

Every review fire emits `ArcCloseFilingReviewed` event:

```json
{
  "event_type": "ArcCloseFilingReviewed",
  "trigger_signals": ["counter_turns_5", "event_commit_landed"],
  "conversation_snapshot_hash": "sha256:...",
  "snapshot_token_count": 4823,
  "arc_summary": "...",
  "qwen_response_raw": "...",
  "qwen_latency_ms": 1042,
  "parsed_decisions": [...],
  "executed_forges": ["bug:long-running-tasks-stale", ...],
  "skipped_low_confidence": [...],
  "user_corrections": [],  // populated later if user patches an auto-fire
  "trigger_to_fire_ms": 14,
  "review_total_ms": 1056
}
```

The event payload is the training corpus for the eventual classifier
AND the validation surface for tuning Qwen prompt + thresholds.

Projection: `arc_close_filing_review_performance` — rebuildable from
events, surfaces:
- Fire rate per trigger type
- Decision distribution by action type
- Auto-execute vs confirm vs skip ratios
- User-correction rate (auto-fires the user later patched)
- Latency distribution

## Thresholds — initial inclusive values

| Knob | Initial value | Notes |
|---|---|---|
| User-turns trigger threshold | **5** | Inclusive by design; tune up from telemetry |
| Tool-iterations trigger threshold | **10** | Inclusive by design; tune up from telemetry |
| Debounce window | **30 s** | Aggregates near-simultaneous triggers |
| Backoff window | **60 s** | Suppresses fires post-review |
| Auto-execute confidence | **≥ 0.85** | Fires the forge without confirm |
| Surface-for-confirm confidence | **0.50 - 0.85** | Agent decides |
| Skip confidence | **< 0.50** | Logged, not surfaced |

Tuning paths (from telemetry):
- High false-positive rate (many auto-fires user later removes) →
  raise auto-execute threshold to 0.90
- High debounce-coalesce rate → consider extending debounce to 60s
- Low recall (real frictions slip through) → lower turn threshold to 3
- Qwen response quality drops on long snapshots → cap snapshot at
  N tokens and use sliding window

## Failure modes — fail-open semantics

| Failure | Behavior |
|---|---|
| Qwen unreachable (llama-server down) | Log + skip; no review fires |
| Qwen returns malformed JSON | Log + skip; no decisions parsed |
| All decisions = `nothing_to_file` | Telemetry-only; no surface |
| Schema validation fails | Log + skip the malformed decision |
| Event-listener crash | Goroutine restart; missed events not replayed |
| Stop hook timeout (>5s) | Hook returns; review event logged as `timed_out` |

No failure mode should regress the current discipline. The current
discipline keeps working underneath; this chain ADDs a structural
firing path on top.

## Migration from retired Stop hook

The retired `friction-filing-reminder.sh` Stop hook had the right
*scheduled-trigger* instinct. What was missing:

1. **Scoped action surface** — the old hook surfaced a generic reminder
   text; the agent had its full tool surface available; the prompt
   competed with everything else in context.
2. **Prescriptive language** — the old hook said "Consider filing..."
   which is informational. This chain uses prescriptive language
   ("Be ACTIVE / nothing-to-file is valid but NOT default") sourced
   from T1's rewritten skill bodies.
3. **Typed decisions, not a question** — the old hook surfaced
   "phrase-hits > filings, here's the count, please file." This chain
   surfaces "here are N typed decisions, execute them." Enumeration-
   then-ask is structurally impossible.
4. **Restricted output** — the old hook's reminder didn't constrain
   the agent's response; the agent could enumerate, ask, defer. The
   schema constrains.

This chain is the *retired-hook's-instinct, done right*.

## Forward-compat with chain 17 (cross-harness-reflex-port)

Chain 17 ships substrate-side discipline content via parse_context's
`recommended_disciplines` array. That's the **pull-side** mechanism.

This chain ships the **push-side** firing mechanism. They're sibling,
not redundant:

- Pull-side surfaces discipline content when message-shape triggers
- Push-side fires decisions on arc-close cadence

When chain 17 lands, parse_context's recommended_disciplines can
include the arc-close review prompt as a discipline reminder for
in-flight catching of the same patterns. Both fire into different
moments of the session.

## Implementation decisions (locked 2026-05-19)

These were deferred questions in the T2 design draft. Sophi's
answers (2026-05-19) lock them in for T3-T5 implementation:

1. **Snapshot scope: last-N-turns capped at M tokens (N=20, M=4000).**
   For v1. A future ML candidate `06-smart-snapshot-filter.md`
   (logged in `~/Documents/files/ideas-to-process/ml-temp/`) will
   replace the dumb truncation with a learned per-turn relevance
   filter once arc-close-filing-review corpus accumulates ~500
   reviews.
2. **Arc-summary derivation: Qwen-driven pre-summarization.** A
   short pre-call to Qwen generates the arc_summary from the
   snapshot before the main review prompt fires. Two Qwen calls per
   review (~1.5-2s total latency). Trade-off accepted: better
   summary → better review-prompt framing → better filing precision.
3. **Debouncer location: MCP-action (substrate-side).** Both firing
   paths (Stop hook + substrate event listener) call into
   `work.review_arc_for_filing`; the action's first step is debounce
   check. Single source of truth.
4. **Counter persistence: per-session JSON at
   `~/.claude/.arc-review/<session_id>.json`.** Tiny (~3 ints),
   human-readable for debugging. Stop hook reads/writes. Cleanup:
   periodic sweep on session-start removes files older than 7 days.
5. **Auto-execute path for Claude Code: auto-execute high-confidence
   (≥ 0.85) for {forge_bug, forge_vault_note, memory_write}; surface
   skill_update at any confidence + surface medium-confidence
   (0.50-0.85) for confirm.** Important constraint: this chain
   auto-executes by FILING ONLY (forge calls + auto-memory writes) —
   not by writing code, editing skill files, or modifying any
   non-filing artifact. The blast radius is bounded; the agent's
   compulsion-to-act is structurally removed from the filing path.
   See §Filing-dispatch "Scope of auto-execute" for the full
   constraint.

## Schema extensibility (chain 11 forward-compat)

When `agent-suggestion-box` (roadmap position 12 post-shift) lands,
the schema extends to include `forge_suggestion`. Same auto-execute
treatment as `forge_bug` / `forge_vault_note` / `memory_write` —
filing only, no fixing.

## Acceptance against completion_condition

T2 acceptance is "documented signal list + tunable thresholds +
telemetry shape." This doc covers:
- Trigger signal list: §A/B/C above (three categories, total ~10
  initial signals)
- Tunable thresholds: §Thresholds above with initial values + tuning
  paths
- Telemetry shape: §Telemetry above with the `ArcCloseFilingReviewed`
  event payload spec

T3 implements the detector (§A/B/C triggers + debouncer + backoff).
T4 implements the MCP action (§Review-surface + §Filing-dispatch).
T5 wires the Claude Code Stop hook (§Filing-dispatch Claude Code
path).

## Out-of-scope for this chain

- bridge-harness post_turn integration (lives in chain 17 of the
  orchestrator-swap initiative)
- Trained classifier (future ml-opportunity-scan candidate; deferred
  per chain design)
- File-moves trigger (deferred until event-volume warrants)
- Multi-session arc-close detection (single-session in v1)

## Dedupe-and-noise-reduction pipeline (chain 618, 2026-05-21)

Chain `arc-close-filing-review-dedupe-and-noise-reduction` (id 618) added four mechanism layers between Qwen's raw output and the typed-decisions dispatch:

1. **F4 — output validation** (`go/internal/arcreview/validation.go`): rejects decisions whose payload matches content-shape noise (test-restatement, high-code-ratio, single-thought, diary-opener, outcome-paraphrase, operator-error-marker, generic-title-no-specifics, placeholder-date-source). The rejected set surfaces in `ArcCloseFilingReviewedPayload.f4_rejected_count` + `.f4_rejected_reasons`.

2. **F2 — pre-filing dedupe vs existing artifacts** (`go/internal/arcreview/dedupe.go`): Jaccard 0.30 threshold on title tokens against `proj_current_bugs` / `proj_current_suggestions` / `knowledge_pointers` (vault index). Match → `FilingDecision.DedupedAgainst` populated; `partitionDecisions` demotes the decision one bucket. Telemetry: `.f2_deduped_count`.

3. **F3 — same-session dedupe window** (`go/internal/arcreview/session_dedupe.go`): Jaccard 0.40 threshold + 1-hour retention against prior `pending_decisions` rows for the same `session_id`. Match → `FilingDecision.SameSessionDedupedAgainst`; same demotion shape as F2. Telemetry: `.f3_same_session_deduped_count`.

4. **F7 — prompt-side suppression** (`go/internal/arcreview/compose.go`'s `reviewSystemBase`): the review prompt names the content-shape anti-patterns (diary-opener, outcome-paraphrase, commit-narrative, procedural-how-to, operator-error, under-400-no-cross-project) and routes them to `nothing_to_file` / `skill_update` at source. Source-side > downstream filtering because (a) the decision is recorded in Qwen's reasoning field (auditable), (b) saves output tokens, (c) skill_update routing needs context F4 doesn't have.

Compose order in the dispatch pipeline:
```
Qwen → ParseReviewResponse → ValidateDecision (shape)
     → CheckBoilerplate (F4)
     → ApplyExistingArtifactDedupe (F2)
     → ApplySameSessionDedupe (F3)
     → partitionDecisions (auto/surface/skip + dedupe-demotion)
     → emitFilingReviewedEvent (telemetry)
     → return PartitionedDecisions
```

F7 suppresses noise BEFORE F4 sees it. The post-pipeline emit captures all four telemetry signals for F5's retrospective measurement.

## F5 retrospective (measurement window 2026-05-21 22:00 → 2026-05-22 00:39)

N=9 post-telemetry-landing arc-close fires observed during chain 618's own development. Classification:

| Time | F4 rej | Forge proposals | Operator verdict |
|---|---|---|---|
| 22:00:42 | 0 | forge_vault_note "ensuring telemetry fields..." + nothing_to_file | NOISE — commit paraphrase |
| 22:05:20 | 1 (outcome_paraphrase) | nothing_to_file × 1 | **SIGNAL — F4 caught noise** |
| 22:27:42 | 0 | forge_vault_note "false-positive retire..." + nothing_to_file | NOISE — commit paraphrase |
| 23:03:54 | 0 | forge_vault_note + skill_update (non-existent skill) + nothing_to_file | NOISE — 2 noise filings |
| 23:10:31 | 0 | forge_vault_note "T6 Walk-Back..." | NOISE — commit paraphrase |
| 00:26:58 | 0 | forge_vault_note "neighbour-pollution gotcha" + nothing_to_file | NOISE — commit paraphrase |
| 00:32:40 | 0 | forge_vault_note "Cross-subdirectory Vault Linkage..." + nothing_to_file | NOISE — commit paraphrase |
| 00:35:53 | 0 | forge_vault_note "Implementing Intra-Subdir Wikilinks..." + nothing_to_file | NOISE — commit paraphrase |
| 00:39:45 | 0 | nothing_to_file × 1 | **SIGNAL — Qwen declined post-F7** |

**Signal-to-noise: 2 / 9 = 22%** (below the 70% target and below the pre-rollout 33% estimate).

The number alone is misleading. Three contextual factors:

1. **Self-referential session bias**: chain 618's own development produced unusually paraphrase-prone arcs because the agent (Claude) was repeatedly committing changes whose commit messages closely mirrored the immediately-preceding work. Qwen reviewing those arcs naturally re-states the commit message as a "vault note." Real sessions (substantive feature work, not self-referential meta-work) should exhibit lower paraphrase rates.

2. **F4's diary-opener regex was incomplete**: the rule matches `^This note captures...`, `^Documenting...`, `^This/The {decision,learning,reference} documents...`. The actual noise body opener pattern in 7 of 9 fires was `During the...` — past-tense narrative voice. This shape was NOT in F4's regex set; the rule needs extending. (F4 v2 tuning input.)

3. **F7 only had ONE post-rollout fire**: the prompt update landed at 7914518 (~00:39). Only the 00:39:45 fire ran on the F7-updated prompt; it cleanly returned `nothing_to_file`. The measurement window is too short to attribute the F7 effect; the 22% signal-to-noise is dominated by pre-F7 fires.

### Per-category yield vs F1 audit baselines

F1 audit (the labelled historical corpus) categorised noise into:
- A: already-filed-elsewhere
- B: same-session-duplicate
- C: test-docstring-restatement
- D: operator-error
- F: insufficient-payload-boilerplate

Live measurement (N=9):
- Category A (existing-artifact match): 0 fires hit F2's threshold. F2 ready but had no provoking inputs.
- Category B (same-session repeat): 0 fires hit F3's threshold. F3 ready; the paraphrases were of DIFFERENT commits each time, so no same-session repeat.
- Category C/D: not present in this measurement window (test-paraphrase + operator-error didn't surface).
- Category F (boilerplate / under-400-no-cross-project): the dominant pattern. F4 caught 1 of 7 (the outcome_paraphrase rule); the other 6 used `During the...` openers that F4's diary regex didn't cover.

### F4 v2 tuning input (deferred to a follow-on task)

The dominant post-F4-v1 noise pattern is the "During the..." commit-narrative opener:
- "During the vault-sweep triage process..."
- "During the process of applying tags..."
- "During the cleanup attempt (T6)..."

These should be added to F4's diary-opener regex set. The F7 prompt update already names this pattern (`"During the {session,implementation,process}..."`), but the F4 layer should also catch it for slip-through cases.

### Closure decision: ship with documented partial measurement

Per the F5 task's constraint ("the 70% threshold is a target not a gate"), the chain closes with:

- F2/F3/F4/F6/F7 all shipped and live in the dispatch pipeline.
- F1 audit corpus + labelled-categories doc landed.
- Measurement window evidence noted; F7 effect not yet measurable.
- F4 v2 tuning input documented (the `During the...` opener pattern) for a follow-on task.
- Vault hygiene pass (T6) yielded 139 file mutations across ~95 entries; Obsidian graph now densely clustered.

Next tuning pass (deferred): add `"During the..."` opener + 2-3 other narrative-voice openers observed in the wild to F4's diary-opener regex set. Re-measure after N≥10 organic fires post-F7.
