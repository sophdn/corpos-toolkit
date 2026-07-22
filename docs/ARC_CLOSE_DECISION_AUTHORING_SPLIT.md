# Arc-Close Decision / Authoring Split — design

**Chain:** `arc-close-decision-authoring-split` (mcp-servers, chain #298)
**Status:** T1 design contract — implementation tasks T3–T7 land against this spec
**Builds on:** `docs/ARC_CLOSE_FILING_REVIEW.md` (the locked five-decision design),
`docs/ARC_CLOSE_FILING_REVIEW_DEDUPE.md` (F2/F3/F4 dedupe + noise reduction).

## Mission

Today, when the arc-close review fires and Qwen returns a high-confidence
(auto-execute band) decision for a body-heavy kind, the drain hook hands the
in-session agent **Qwen's full pre-authored body** with an "auto-execute"
directive — so the agent forges Qwen's prose ~verbatim. Qwen wrote the body from
a **truncated snapshot** (20 turns / ~4–6k tokens); the seated agent has the
**full conversational arc** in context. The agent is the better author; Qwen is
the better *decider* (it fires structurally, without the agent's filing-fatigue
blind spot).

This split keeps Qwen as the **decider** (WHETHER to file + what kind + a seed
title) and moves **authoring** of the body to the in-session agent — *without
losing capture* when the seat is too weak or disengaged to author. High-value
captures land as well-authored notes regardless of which seat is driving.

## The split, in one line

> Qwen decides **whether** and **what kind**; the seated agent authors the
> **body**; an `unreviewed`-flagged fallback forge guarantees capture is never
> lost.

## In-scope kinds (and why)

"Body-heavy" = the kind's value lives in a long-form prose `Body` that benefits
from full conversational context. Two kinds qualify, both with a **required**
`Body` field in `ValidateDecision` (`schema.go`):

| Kind | Payload body field | In v1? |
|---|---|---|
| `forge_vault_note` (note_kind `decision` / `learning` / `reference`) | `Body` (required) | **YES** |
| `memory_write` (memory_kind `user` / `feedback` / `project` / `reference`) | `Body` (required) | **YES** |
| `forge_bug` | `ProblemStatement` | **NO** — see below |
| `forge_suggestion` | `ProblemStatement` | **NO** — see below |
| `skill_update` | `Content` | **NO** — already surface-for-confirm at all bands |
| `nothing_to_file` | — | **NO** — no artifact |

**Bug is OUT of v1** (the task explicitly asked us to decide this). A bug's
`problem_statement` is short, structured, and observational — "here is the
friction I saw" — not a synthesis that rewards the agent's full-arc context the
way a vault note's body does. Bugs auto-forge well today (the F5 retrospective
showed bug proposals were the highest-precision category), and the bug-filing
discipline already prescribes a tight problem-statement shape that Qwen fills
adequately from the snapshot. Re-routing bugs through agent-authoring would add
a staging round-trip for little quality gain. `forge_suggestion` is excluded for
the same reason (structured `problem_statement`, native suggestion vocabulary).
`skill_update` is already always surface-for-confirm (it edits live skill files),
so it never hit the auto-execute path the split modifies.

If the v1 measurement (T8) shows bug bodies would also benefit, a v2 can widen
the in-scope set — the staging mechanism is kind-agnostic by construction.

## Confidence bands LEFT UNCHANGED

The split touches **only the auto-execute band for in-scope body-heavy kinds**.
Everything else is behavior-preserving (characterization-net-gated in T3):

- **`< surfaceConfidence` (0.50) → Skip** — unchanged.
- **`[surfaceConfidence, autoExecuteConfidence)` → SurfaceForConfirm** — unchanged.
  Surface-for-confirm already hands the decision to the agent for an explicit
  yes/no; the agent may rewrite the body when confirming. No staging needed.
- **`>= autoExecuteConfidence` for out-of-scope kinds** (bug, suggestion) →
  AutoExecute — unchanged (still forges Qwen's payload).
- **`>= autoExecuteConfidence` for in-scope kinds** (vault_note, memory_write) →
  **NEW: stage for agent authoring** (this is the only path that changes).

### Threshold: key off the constant, not the literal `0.85`

The chain spec and the original design (`ARC_CLOSE_FILING_REVIEW.md`) say
`>= 0.85`. The **live** threshold is `autoExecuteConfidence = 0.90`
(`handler.go:114`, tuned 0.85→0.90 in the filing-review T9 against the
2026-05-19 tuning corpus). This design keys the split off the existing
`autoExecuteConfidence` constant and the `autoExecuteActions` set — **whatever
their current numeric value** — so the split tracks future tuning automatically
and never reintroduces a hardcoded `0.85`. References to "the ≥0.85 band" in the
chain/task text mean "the auto-execute band."

## Agent-authoring injection contract

When an in-scope decision lands in the auto-execute band, it is **staged** (not
auto-forged) and surfaced to the seated agent through the existing
pending-decisions drain hook (`pending-decisions-drain-hook.sh`,
UserPromptSubmit `additionalContext`). The injected block changes shape for
staged decisions:

**Carries (Qwen's seed):**
- `action` + `note_kind` / `memory_kind` (Qwen's kind decision)
- `title` (Qwen's seed title — the agent may refine it)
- `reasoning` (Qwen's why-this-is-worth-filing)
- explicit **decider attribution**: "Qwen (arc-close review) decided this arc is
  worth filing as a `<kind>`. **You** have the full conversation in context —
  author the body." Qwen is named as decider so the provenance is honest and the
  agent understands it is the author, not a rubber-stamp.
- a **staged-decision handle** (the pending-decisions row id + a stable
  `staged_decision_id`) so the agent's forge can be attributed back to this
  staging row (marks it `authored`, suppresses the fallback).

**Does NOT carry:** an auto-forged body. Qwen's draft body is **retained
server-side** (in the staged row) for the fallback path, but is **not** placed
in the injection as "here, forge this" — placing it there is exactly the
verbatim-forge behavior the split removes. (Open sub-decision for T4: whether to
show Qwen's draft body to the agent as *reference material* labelled "Qwen's
draft — rewrite, don't copy." Default: **omit it** from the prompt to avoid
anchoring the agent on Qwen's truncated-snapshot prose; revisit if T8 shows
agents want the seed. The draft still exists server-side for fallback.)

**The agent authors and forges once** via `forge(vault-note, …)` /
`forge(memory, …)`, passing the `staged_decision_id` so the staging row is
marked authored.

## Staging / pending-decision state machine

A staged decision moves through these states (stored on the pending-decisions
row — reuse the existing table, add an `authoring_state` + companion columns;
no new side-table, mirroring F3's reuse discipline):

```
                      in-scope kind, auto-execute band
   [review fires] ───────────────────────────────────────▶  staged
                                                               │
              agent forges w/ staged_decision_id               │  session end / explicit skip
                      ┌────────────────────────────────────────┤  (no authoring observed)
                      ▼                                         ▼
                  authored                              fallback_forged
            (agent-authored note,                 (Qwen draft body forged,
             staging row resolved)                 flagged `unreviewed`)
```

- **`staged`** — awaiting agent authoring. The drain hook surfaces the authoring
  prompt. Qwen's draft body is retained on the row.
- **`authored`** — the seated agent forged the note (matched back via
  `staged_decision_id`). Terminal, success.
- **`fallback_forged`** — the trigger fired without authoring; Qwen's retained
  draft was forged with the `unreviewed` flag. Terminal, capture-not-lost.

Out-of-scope kinds and other bands never enter this machine — they keep the
current `auto_execute` / `surface_for_confirm` / `skip` partition semantics
untouched.

## Unreviewed-fallback semantics

**Trigger** (either):
1. **Session end** — the Stop hook (arc-close-detector path / a dedicated
   session-end sweep) finds `staged` rows for the ending session past a grace
   window and forges their drafts. (T5 picks the concrete signal; the Stop hook
   already runs per-session and is the natural home.)
2. **Explicit skip** — the agent signals "not authoring this" (a skip action on
   the staged handle). Forges immediately rather than waiting for session end.

**Flag:** the forged note is marked **`unreviewed`** AND attributed
**`qwen-authored`** — via a tag on the vault-note forge (`tags` already exists on
`ForgeVaultNotePayload`) plus a body sentinel line
(`> ⚠️ Qwen-authored draft, unreviewed — enrich or delete.`). For `memory_write`,
the equivalent flag on the memory entry.

**Queryable / findable for cleanup:** the `unreviewed` tag is indexed
(knowledge_pointers / vault tag index) so a cleanup sweep can list all
fallback-forged notes. The staging row also records `authoring_state =
fallback_forged`, so the cleanup list can be derived from pending_decisions too.

**No-worse-than-today invariant:** a fallback forge produces *exactly the note
today's auto-execute would have produced* (Qwen's body) — only now it is clearly
marked as unreviewed and findable. The split can therefore never regress capture:
worst case == today's behavior, best case == an agent-authored note.

## Same-session dedup guard (T6 — specified here, built there)

Before a decision is staged (or auto-executed), check the **events ledger** for
the in-session agent's **own** `BugReported` / `MemoryWritten` rows this session
(and snapshot-extracted vault forges, since vault notes emit no typed event yet
— see `recent_filings.go`). If the staged decision is semantically near an
artifact the agent **already filed this session**, downgrade it from "author a
new note" to "**enrich the existing one**" (surface the existing artifact's
slug; suggest appending rather than creating a duplicate). This is the
agent-filing analogue of F3 (which dedupes against prior *Qwen proposals*); it is
scoped to **same-session agent filings only** and does **not** re-solve general
vault-semantic-dedup (bug 899).

## Graceful degradation across seat strengths

The whole point of the fallback is that the split must be **no-worse-than-today
for any seat**:

| Seat | Behavior | Outcome |
|---|---|---|
| **Strong** (engaged, capable model) | Sees the authoring prompt, authors a body from full-arc context, forges with the handle. | Best case: a well-authored note. `authored`. |
| **Weak** (thin/late authoring, small model) | May author a short body, or author after a delay. Whatever it writes is still richer than Qwen's truncated-snapshot draft, and the handle resolves the staging row. | Note authored, possibly thin — still ≥ today. `authored`. |
| **Disengaged** (ignores the prompt / no further turns) | Never authors. At session end the fallback forges Qwen's retained draft, flagged `unreviewed`. | Exactly today's note, now marked for cleanup. `fallback_forged`. Capture not lost. |

Telemetry (T7) records the **author-vs-fallback rate** per seat — the instrument
that tells us how drivable the authoring reflex is for each model/harness.

## Telemetry (T7 — specified here)

New states emit events so the rate is measurable:
- **decision-staged** — an in-scope auto-execute decision entered `staged`.
- **agent-authored** — a staged decision was resolved by an agent forge.
- **fallback-forged-unreviewed** — a staged decision hit the fallback.

Derived signal: **author-vs-fallback rate** = `agent-authored / (agent-authored +
fallback-forged)`, surfaced over time on the dashboard, sliceable per
project/seat. Follows telemetry-conventions (opt-in/privacy invariants,
warmup-aware acceptance — the rate is only meaningful after N staged decisions).

## Acceptance against the chain completion condition

A `>= autoExecuteConfidence` arc-close decision for a body-heavy kind:
1. **no longer auto-forges a Qwen-authored body** — it stages (T4). ✔
2. **the in-session agent is prompted to author it** (Qwen attributed as decider)
   — authoring injection contract (T4). ✔
3. **`unreviewed`-flagged fallback forge if unauthored by session end** — fallback
   semantics (T5). ✔
4. **same-session near-duplicates downgraded to enrich-existing** — dedup guard
   (T6). ✔
5. **telemetry records author-vs-fallback rate** — (T7). ✔
6. Verified by tests + an observed live arc-close fire (T3 net + T8 retro).

## Out of scope for this chain

- Changing the `< 0.50` skip and `0.50–0.90` confirm bands (explicit constraint).
- General vault-semantic-dedup (bug 899) — only same-session agent filings.
- The trigger false-fire investigation (T2) — separate, ledger-driven; informs
  whether a gating fix is warranted but does not block the split.
- A trained classifier for the decision/authoring boundary — fed forward to
  chains #21 / #22 in the T8 retro.
