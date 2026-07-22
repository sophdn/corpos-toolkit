package arcreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"toolkit/internal/inference/router"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
)

// arcSummaryMaxTokens caps the pre-call's response budget. One short
// paragraph fits comfortably under 256 tokens; the cap protects against
// runaway summaries leaking into the main review prompt.
const arcSummaryMaxTokens = 256

// reviewMaxTokens caps the main review's response budget. Sized for
// up to ~6 typed filing_decisions plus the summary paragraph — each
// decision is ~150 tokens with prose payloads, so 1024 leaves headroom
// for the summary and a few extra decisions before any truncation.
const reviewMaxTokens = 1024

// defaultMaxPromptTokens is the input-prompt budget for the main review
// call. Closes bug `qwen-context-size-exceeded-on-long-session-
// arcreview-fires`: empirically, the static 4000-token snapshot cap
// alone is insufficient because the assembled prompt also carries the
// prescriptive system prompt (~1k tokens), the structured-output
// schema, and the arc_summary block. On a llamacpp server with a
// modest n_ctx (8192 is a common default) the total can exceed the
// loaded context window, producing HTTP 500 "Context size has been
// exceeded" from llama.cpp.
//
// 6000 tokens leaves headroom for the 1024-token main-review response
// + per-model overhead under a conservative n_ctx=8192 deployment.
// Operators with larger contexts (Qwen2.5-32B's n_ctx_train is 32768)
// should bump this via TOOLKIT_ARCREVIEW_MAX_PROMPT_TOKENS so the
// snapshot can land more turns without being trimmed.
const defaultMaxPromptTokens = 6000

// minSnapshotTurns is the floor below which fitSnapshotToPromptBudget
// stops trimming. A review with fewer than three conversation turns
// has lost too much signal; better to return ErrPromptTooLarge and let
// the handler surface status="skipped" than to fire a degenerate
// review and pollute the corpus with low-information rows.
const minSnapshotTurns = 3

// ErrPromptTooLarge is returned from DispatchReview when the assembled
// main-review prompt exceeds the prompt-token budget even after
// trimming the snapshot down to minSnapshotTurns. The handler
// catches this and surfaces status="skipped" with reason naming the
// budget, keeping the fail-open contract intact (per design
// §Failure-modes — a prompt that can't fit is operationally a
// can't-fire condition, not a crash).
var ErrPromptTooLarge = errors.New("arcreview: assembled prompt exceeds max_prompt_tokens budget")

// maxPromptTokens returns the configured budget. Honors the
// TOOLKIT_ARCREVIEW_MAX_PROMPT_TOKENS env var for operators tuning
// against a llamacpp deployment with a larger context window. Invalid
// or non-positive values fall back to defaultMaxPromptTokens.
func maxPromptTokens() int {
	if v := os.Getenv("TOOLKIT_ARCREVIEW_MAX_PROMPT_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxPromptTokens
}

// estimatePromptTokens returns the rough char→token approximation for
// the assembled (system + user) prompt. Same heuristic the snapshot
// extractor uses (charsPerTokenEstimate = 4). Real tokenization can
// run heavier on code/JSON-heavy content; defaultMaxPromptTokens is
// sized conservatively to absorb that variance.
func estimatePromptTokens(system, user string) int {
	return (len(system) + len(user)) / charsPerTokenEstimate
}

// fitSnapshotToPromptBudget trims the snapshot's oldest turns until the
// assembled main-review prompt fits below the budget. Returns the
// possibly-trimmed snapshot + a flag indicating whether trimming
// occurred. If even minSnapshotTurns won't fit, returns the trimmed
// snapshot + ErrPromptTooLarge so the caller can short-circuit cleanly.
//
// Trimming drops from the front (oldest turns) because the most-recent
// turns carry the strongest filing signals — the activity that prompted
// the trigger is at the tail.
//
// recentFilings is passed through to the cost estimator unchanged; the
// ALREADY FILED block in the prompt costs ~30-60 tokens per row in
// practice, which the budget loop accounts for via the same chars/4
// approximation it uses for everything else.
func fitSnapshotToPromptBudget(snap Snapshot, arcSummary string, triggers []string, recentFilings []RecentFiling, budget int) (Snapshot, bool, error) {
	trimmed := false
	for {
		sys, user := ComposeReviewPrompt(snap, arcSummary, triggers, recentFilings)
		if estimatePromptTokens(sys, user) <= budget {
			return snap, trimmed, nil
		}
		if len(snap.Messages) <= minSnapshotTurns {
			return snap, true, ErrPromptTooLarge
		}
		snap.Messages = snap.Messages[1:]
		snap.Truncated = true
		snap.EstimatedTokens = approxTokensForMessages(snap.Messages)
		trimmed = true
	}
}

// approxTokensForMessages re-estimates the snapshot's EstimatedTokens
// after the budget-fitter trims oldest turns. Mirrors snapshot.go's
// charsPerTokenEstimate so the stored count stays consistent with the
// surface the corpus row exposes.
func approxTokensForMessages(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) + len(m.Role) + 2 // ": " separator
	}
	return total / charsPerTokenEstimate
}

// arcSummaryTaskID is the qwenctx stamp for arc-summary pre-calls;
// reviewTaskID stamps the main review call. Surfaces in the inference_invocations
// telemetry table so the /inference dashboard attributes both calls to
// this package.
const (
	arcSummaryTaskID = "arc-review-summary"
	reviewTaskID     = "arc-review-decisions"
)

// DispatchReview runs the two-call Qwen pipeline for one arc-close
// fire: arc-summary pre-call → main review with the prescriptive
// prompt + structured-output schema. Returns the parsed ArcReviewResult
// (decisions already validated; invalid decisions dropped with a log
// entry per design §Failure-modes — fail-open by skipping the
// offender, not by abandoning the fire).
//
// The router is required (nil → typed error so the handler can return
// a structured fail-open response). triggers may be empty; the design
// permits fire-without-trigger paths for ad-hoc reviews.
//
// recentFilings carries the in-arc dedupe enrichment (bug 1472):
// artifacts filed earlier in the same conversation that the review
// prompt should NOT re-propose. The handler queries these via
// recentFilingsInArc and passes them through unmodified; the dispatch
// path treats nil/empty identically (no ALREADY FILED block in the
// prompt) so the caller doesn't have to short-circuit on its own.
//
// Latency / token telemetry sums across both calls. Callers persist
// the result onto the ArcCloseFilingReviewed event (Stage 3).
func DispatchReview(ctx context.Context, r *router.Router, snap Snapshot, triggers []string, recentFilings []RecentFiling) (ArcReviewResult, error) {
	if r == nil {
		return ArcReviewResult{}, fmt.Errorf("arcreview dispatch: router is nil")
	}
	if len(snap.Messages) == 0 {
		return ArcReviewResult{}, fmt.Errorf("arcreview dispatch: snapshot is empty")
	}

	// Pre-call: arc summary. Failure here is non-fatal — the main
	// review can run without an explicit summary; the prompt has a
	// fallback ("(no arc summary; rely on snapshot)").
	arcSummary, preLatency, preIn, preOut := runArcSummary(ctx, r, snap)

	// Fit the assembled prompt under the configured budget by trimming
	// the oldest snapshot turns when needed. Closes
	// `qwen-context-size-exceeded-on-long-session-arcreview-fires`: the
	// static snapshot cap alone doesn't account for the system prompt +
	// schema + arc_summary overhead; long sessions blew through Qwen's
	// runtime n_ctx and returned HTTP 500. If even the minimum-floor
	// snapshot can't fit, return ErrPromptTooLarge so the handler
	// surfaces status="skipped" cleanly.
	budget := maxPromptTokens()
	trimmedSnap, didTrim, fitErr := fitSnapshotToPromptBudget(snap, arcSummary, triggers, recentFilings, budget)
	if errors.Is(fitErr, ErrPromptTooLarge) {
		obs.Logger(ctx).Info("arcreview: prompt-too-large after trimming to floor; returning skip",
			"budget_tokens", budget,
			"floor_turns", minSnapshotTurns)
		return ArcReviewResult{
			ArcSummary:   arcSummary,
			LatencyMS:    preLatency,
			InputTokens:  preIn,
			OutputTokens: preOut,
		}, ErrPromptTooLarge
	}
	if didTrim {
		obs.Logger(ctx).Info("arcreview: snapshot trimmed to fit prompt budget",
			"budget_tokens", budget,
			"final_turn_count", len(trimmedSnap.Messages))
	}
	snap = trimmedSnap

	reviewSystem, reviewUser := ComposeReviewPrompt(snap, arcSummary, triggers, recentFilings)
	mainCtx := qwenctx.WithTaskID(ctx, reviewTaskID)
	mainResp, err := r.GenerateWithOpts(mainCtx, reviewUser, reviewSystem, router.GenerateOpts{MaxTokens: reviewMaxTokens})
	if err != nil {
		return ArcReviewResult{
			ArcSummary:   arcSummary,
			LatencyMS:    preLatency + mainResp.LatencyMS,
			InputTokens:  preIn,
			OutputTokens: preOut,
		}, fmt.Errorf("arcreview main call: %w", err)
	}

	parsed, parseErr := ParseReviewResponse(mainResp.Text)
	totalLatency := preLatency + mainResp.LatencyMS
	totalIn := sumOptional(preIn, mainResp.InputTokens)
	totalOut := sumOptional(preOut, mainResp.OutputTokens)
	if parseErr != nil {
		return ArcReviewResult{
			ArcSummary:   arcSummary,
			LatencyMS:    totalLatency,
			InputTokens:  totalIn,
			OutputTokens: totalOut,
		}, fmt.Errorf("arcreview parse: %w", parseErr)
	}

	parsed.ArcSummary = arcSummary
	parsed.LatencyMS = totalLatency
	parsed.InputTokens = totalIn
	parsed.OutputTokens = totalOut

	// Two-stage filter:
	//   1. ValidateDecision (shape) — drops malformed decisions
	//      silently (action enum, payload shape, required fields).
	//   2. CheckBoilerplate (content) — F4 of chain
	//      arc-close-filing-review-dedupe-and-noise-reduction.
	//      Rejects decisions whose content matches the noise
	//      patterns characterised in F1's labelled corpus
	//      (test-docstring restatements, operator-error filings,
	//      generic-title boilerplate). Rejected decisions surface
	//      in RejectedDecisions for telemetry; only Decisions
	//      flows downstream to pending_decisions + the Stop hook.
	kept := make([]FilingDecision, 0, len(parsed.Decisions))
	rejected := make([]RejectedDecision, 0)
	for _, d := range parsed.Decisions {
		if err := ValidateDecision(d); err != nil {
			// Shape-invalid. Skip silently; preserve order on the
			// rest. (Stage 3 of the original chain wires obs.Logger
			// here to capture the drop reason; out of F4 scope.)
			continue
		}
		if reason := CheckBoilerplate(d); reason != BoilerplateNotRejected {
			rejected = append(rejected, RejectedDecision{Decision: d, Reason: reason})
			continue
		}
		kept = append(kept, d)
	}
	parsed.Decisions = kept
	if len(rejected) > 0 {
		parsed.RejectedDecisions = rejected
	}
	return parsed, nil
}

// runArcSummary is the pre-call wrapper. Returns ("", 0, nil, nil) on
// failure so DispatchReview can fall through to the main review with
// the snapshot-only fallback.
func runArcSummary(ctx context.Context, r *router.Router, snap Snapshot) (string, int64, *int64, *int64) {
	sys, user := ComposeArcSummaryPrompt(snap)
	preCtx := qwenctx.WithTaskID(ctx, arcSummaryTaskID)
	resp, err := r.GenerateWithOpts(preCtx, user, sys, router.GenerateOpts{MaxTokens: arcSummaryMaxTokens})
	if err != nil {
		return "", resp.LatencyMS, nil, nil
	}
	return strings.TrimSpace(resp.Text), resp.LatencyMS, resp.InputTokens, resp.OutputTokens
}

// ParseReviewResponse extracts the structured-output JSON object from
// Qwen's text response and decodes it into ArcReviewResult. Drift-
// tolerant: strips ```json fences, trims preface text up to the first
// '{', and trims trailing commentary after the matching '}'. Returns
// a typed error when no JSON object is recoverable.
//
// Exposed (capitalized) so tests can exercise the parser without
// firing the router.
func ParseReviewResponse(raw string) (ArcReviewResult, error) {
	body := extractJSONObject(raw)
	if body == "" {
		return ArcReviewResult{}, fmt.Errorf("no JSON object found in response")
	}
	var out ArcReviewResult
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		// Retry without DisallowUnknownFields — Qwen occasionally
		// adds extras like "notes" alongside the schema fields. Real
		// drift (missing top-level fields) still fails below.
		dec2 := json.NewDecoder(strings.NewReader(body))
		if err2 := dec2.Decode(&out); err2 != nil {
			return ArcReviewResult{}, fmt.Errorf("decode response JSON: %w", err)
		}
	}
	if out.Decisions == nil {
		out.Decisions = []FilingDecision{}
	}
	return out, nil
}

// extractJSONObject finds the first top-level JSON object in s and
// returns its substring (including the surrounding braces). Handles
// ```json fenced blocks and naked-object responses with leading or
// trailing prose. Returns "" when no balanced object is found.
//
// Uses a depth counter that respects string-literal boundaries so a
// '}' inside a string doesn't close the object prematurely.
func extractJSONObject(s string) string {
	// Strip a leading ```json or ``` fence if present.
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "```") {
		if i := strings.Index(trimmed, "\n"); i > 0 {
			trimmed = trimmed[i+1:]
		}
		if i := strings.LastIndex(trimmed, "```"); i >= 0 {
			trimmed = trimmed[:i]
		}
		trimmed = strings.TrimSpace(trimmed)
	}
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(trimmed); i++ {
		c := trimmed[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return trimmed[start : i+1]
			}
		}
	}
	return ""
}

// sumOptional adds two *int64 with nil-safe semantics. nil + nil = nil;
// nil + x = x; x + y = x + y. Mirrors knowledge/handler.go's sumOptInt64
// pattern; duplicated here to avoid a cross-package import for a
// six-line helper.
func sumOptional(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	var sum int64
	if a != nil {
		sum += *a
	}
	if b != nil {
		sum += *b
	}
	return &sum
}
