// Package arcreview implements the substrate-side review pipeline for
// arc-close filing review (chain arc-close-filing-review in mcp-servers).
// Canonical design lives at docs/ARC_CLOSE_FILING_REVIEW.md — all five
// implementation decisions are locked there; this package's behaviour
// honors them verbatim.
//
// ## Intended use
//
// Workflow served: on a detected arc-close, the substrate fires a
// Qwen-driven review against a conversation snapshot and returns typed
// filing decisions (bug, vault note, skill update, memory write, or
// nothing_to_file). The agent (Claude Code Stop hook) or the harness
// (bridge-harness post_turn hook) dispatches the auto-execute decisions
// in-band and surfaces medium-confidence ones for confirm. Replaces the
// agent-internalized "compulsion to file" with a structurally-enforced
// firing path.
//
// Invocation pattern: ExtractSnapshot(transcriptPath, maxTurns, maxTokens)
// then DispatchReview(ctx, router, snapshot, triggers) returns an
// ArcReviewResult whose Decisions are already schema-validated. Stage 2
// wraps this in the work.review_arc_for_filing MCP action handler with
// debounce + backoff in front; Stage 3 emits ArcCloseFilingReviewed
// events for the telemetry corpus.
//
// Success shape: ArcReviewResult with a (possibly empty) slice of
// validated FilingDecision and a one-paragraph arc summary; latency
// and token-count fields populated from the router result. Invalid
// decisions are dropped (logged at Stage 3), not propagated.
//
// Non-goals: this package does not own the action handler (Stage 2),
// the substrate event listener (Stage 3), the Claude Code Stop hook
// wrapper (T5), or any forge call — it produces typed decisions; the
// caller dispatches the consequences. Failure semantics are fail-open
// (per design §Failure-modes): Qwen unreachable, malformed JSON, or
// all-nothing-to-file responses surface as a typed error or empty
// decision list so the caller can log + skip without regressing the
// current discipline.
//
// ## Pipeline overview
//
//  1. Debounce — 30s coalesce + 60s backoff per design §Debouncer
//     (Stage 2 — debouncer.go).
//  2. ExtractSnapshot — read the JSONL transcript, walk from end,
//     accumulate the last N turns capped at M tokens (Q1 = 20 turns,
//     4000 tokens). Mixed content shapes (string vs array of text parts)
//     handled.
//  3. DispatchReview — two Qwen calls. First the arc-summary pre-call
//     (Q2 = Qwen-driven summary); second the main review with the
//     prescriptive prompt + structured-output schema. Both run through
//     toolkit/internal/inference/router.
//  4. Parse + validate — the schema IS the action whitelist (per design
//     §Structured-output-schema); ValidateDecision rejects out-of-schema
//     actions, malformed payloads, and confidence outside [0, 1].
//  5. Emit ArcCloseFilingReviewed event (Stage 3) so the per-fire row
//     becomes the training corpus for the future trained classifier.
//
// ## Where the prompt content comes from
//
// compose.go does NOT reinvent the prescriptive language. The signal
// taxonomy, "be ACTIVE" framing, anti-patterns, and preference order
// are the source-of-truth in T1's rewritten skill bodies:
//
//   - ~/.claude/skills/vault-filing-discipline/SKILL.md
//   - ~/.claude/skills/bug-filing-discipline/SKILL.md
//
// compose.go references the same prescriptive bullets so the substrate
// firing surface stays aligned with the agent's in-flight discipline.
//
// ## Stage map
//
//   - Stage 1 (this commit): doc.go, schema.go, snapshot.go, compose.go,
//     dispatch.go — pure functions + Qwen dispatch wrapper. No MCP
//     action handler, no debouncer, no event emission yet.
//   - Stage 2: handler.go (action handler), debouncer.go (DB-backed
//     coalesce + backoff), action manifest TOML, work.BuildTable
//     registration.
//   - Stage 3: listener.go (substrate event-table tail goroutine),
//     ArcCloseFilingReviewedPayload + JSON schema, integration test
//     against live llama-server (skip-if-unreachable).
//   - Stage 4: full build + test sweep; handoff to T5 (Stop hook).
package arcreview
