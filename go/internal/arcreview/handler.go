package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"toolkit/internal/arcreview/arcparams"
	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/inference/router"
	"toolkit/internal/obs"
)

// Deps is the dependency bundle the MCP action handler requires.
// Pool drives the debouncer; Router drives the two Qwen calls.
// MaxTurns / MaxTokens override the snapshot extraction caps (zero
// falls back to the package defaults). BackoffSeconds overrides the
// debouncer window (zero falls back to DefaultBackoffSeconds).
// ArcHashCacheTTLSeconds overrides the per-arc-content dedupe TTL
// (zero falls back to DefaultArcHashCacheTTLSeconds).
type Deps struct {
	Pool                   *db.Pool
	Router                 *router.Router
	MaxTurns               int
	MaxTokens              int
	BackoffSeconds         int
	ArcHashCacheTTLSeconds int
	// ForgeFn, when non-nil, forges one artifact via the forge surface.
	// Injected at wiring (cmd/toolkit-server/main.go) so arcreview does NOT
	// import internal/forge — preserving the substrate-doesn't-own-forge
	// boundary (doc.go) as a dependency-injection seam rather than a hard
	// import. It wraps forge.HandleForge (the standalone path; NOT
	// HandleForgeInTx, whose batch-scope gate rejects vault-note/memory).
	// The unreviewed-fallback sweep (chain arc-close-decision-authoring-
	// split T5) is the only consumer. nil disables the fallback (fail-safe:
	// a staged decision stays staged rather than the substrate crashing on
	// a missing forge).
	ForgeFn ArtifactForgeFn
}

// ArtifactForgeFn forges one artifact and returns an error on failure.
// params is the forge action's raw params JSON
// ({schema_name, slug, fields:{...}}). The implementation owns its own
// write transaction; the sweep logs failures and leaves the row staged so
// a later trigger retries.
type ArtifactForgeFn func(ctx context.Context, project string, params json.RawMessage) error

// PartitionedDecisions groups validated decisions by what the caller
// should do with them, per docs/ARC_CLOSE_FILING_REVIEW.md §Filing-
// dispatch Q5. The lists carry FilingDecision values verbatim; the
// caller dispatches forges / surfaces / skips accordingly.
type PartitionedDecisions struct {
	AutoExecute []FilingDecision `json:"auto_execute"`
	// StagedForAuthoring holds in-scope body-heavy decisions
	// (forge_vault_note / memory_write) in the auto-execute band that
	// are staged for the in-session agent to author rather than
	// auto-forged with Qwen's draft body (chain
	// arc-close-decision-authoring-split T4). These are still
	// actionable — the agent must see them to author — so they flow to
	// the dispatch surfaces alongside AutoExecute, but with an
	// authoring prompt instead of a verbatim forge.
	StagedForAuthoring []FilingDecision `json:"staged_for_authoring"`
	SurfaceForConfirm  []FilingDecision `json:"surface_for_confirm"`
	Skip               []FilingDecision `json:"skip"`
}

// ReviewArcForFilingResult is the action's response. Status discriminates
// the call outcome:
//
//   - "fired"     — review ran, Decisions populated, Partition split.
//   - "debounced" — call suppressed by the backoff window; LastFireAt
//     surfaces the prior fire timestamp for the caller's log.
//   - "skipped"   — call short-circuited before a fire fired (empty
//     transcript, snapshot extraction failure, etc.); Reason carries
//     the human-readable cause.
//   - "qwen_unreachable" — fail-open path; Reason carries the dispatch
//     error string. No decisions parsed.
//
// All non-"fired" outcomes are non-error from the dispatcher's
// perspective (the action's job is to report what happened; the agent
// or harness uses the status to decide whether to retry).
type ReviewArcForFilingResult struct {
	Status     string               `json:"status"`
	Decisions  []FilingDecision     `json:"decisions,omitempty"`
	Partition  PartitionedDecisions `json:"partition,omitempty"`
	Summary    string               `json:"summary,omitempty"`
	ArcSummary string               `json:"arc_summary,omitempty"`
	LatencyMS  int64                `json:"latency_ms,omitempty"`
	Triggers   []string             `json:"triggers,omitempty"`
	LastFireAt string               `json:"last_fire_at,omitempty"`
	Reason     string               `json:"reason,omitempty"`
	// EventID is the ArcCloseFilingReviewed event_id when emit succeeded.
	// Populated only on status="fired". The substrate-side observer reads
	// this to link a pending_decisions row back to the corpus event for
	// the T8 audit join.
	EventID string `json:"event_id,omitempty"`
	// PriorEventID is populated on status="arc_hash_dedup" — the
	// canonical ArcCloseFilingReviewed event_id of the prior fire that
	// covered the same arc content. Lets the caller surface the prior
	// decision without re-running Qwen. Closes bug
	// `arc-close-filing-review-fires-multiply-on-overlapping-arc-within-single-session`.
	PriorEventID string `json:"prior_event_id,omitempty"`
}

// arcHashTTLFor returns the configured arc-hash-cache TTL in seconds.
// Honors deps.ArcHashCacheTTLSeconds with a fall-through to
// DefaultArcHashCacheTTLSeconds.
func arcHashTTLFor(deps Deps) int {
	if deps.ArcHashCacheTTLSeconds > 0 {
		return deps.ArcHashCacheTTLSeconds
	}
	return DefaultArcHashCacheTTLSeconds
}

// autoExecuteActions is the closed set of action kinds the Claude Code
// Stop-hook auto-execute path may fire without confirmation. Per
// design Q5, skill_update is excluded — skill edits cross the
// filing/fixing line and always surface for confirm.
var autoExecuteActions = map[ActionKind]bool{
	ActionForgeBug:        true,
	ActionForgeVaultNote:  true,
	ActionMemoryWrite:     true,
	ActionForgeSuggestion: true,
}

// inScopeBodyHeavyActions is the closed set of body-heavy filing kinds
// whose auto-execute-band decisions are STAGED for in-session agent
// authoring rather than auto-forged with Qwen's draft body (chain
// arc-close-decision-authoring-split T4 — see
// docs/ARC_CLOSE_DECISION_AUTHORING_SPLIT.md §In-scope kinds). Both have
// a required long-form Body that benefits from the seated agent's full
// conversational context, which Qwen sees only as a truncated snapshot.
//
// forge_bug / forge_suggestion are OUT of v1 (short, structured
// problem_statement — not body-heavy); skill_update never auto-executes.
// This set is a SUBSET of autoExecuteActions, so the staging branch in
// partitionDecisions must be checked BEFORE the plain auto-execute branch.
var inScopeBodyHeavyActions = map[ActionKind]bool{
	ActionForgeVaultNote: true,
	ActionMemoryWrite:    true,
}

// autoExecuteConfidence is the threshold above which an auto-executable
// action lands in the AutoExecute partition. Below this and at or
// above SurfaceConfidence, the action lands in SurfaceForConfirm.
//
// Tuned from 0.85 → 0.90 in T9 (docs/ARC_CLOSE_FILING_REVIEW_TUNING_
// CORPUS_2026-05-19.md): the 12-fire corpus showed a bimodal
// confidence distribution where the only 0.85 proposal in the sample
// was the precision-miss "live end-to-end testing" generic vault
// note. Raising the cutoff to 0.90 admits the 11 high-quality
// proposals while filtering the lone borderline.
const autoExecuteConfidence = 0.90

// surfaceConfidence is the lower bound for the SurfaceForConfirm
// partition. Decisions below this threshold (and not skill_update)
// land in Skip.
const surfaceConfidence = 0.50

// HandleReviewArcForFiling runs one arc-close filing review. Wired
// onto the work meta-tool as `review_arc_for_filing`; called by the
// Claude Code Stop hook (T5) with the detector's trigger payload as
// params, and (Stage 3) by the substrate event-listener goroutine.
//
// Project is the meta-tool envelope's project scope; carried into the
// telemetry event (Stage 3) so per-project filing rates can be
// derived from the projection.
func HandleReviewArcForFiling(ctx context.Context, deps Deps, project string, params json.RawMessage) (ReviewArcForFilingResult, error) {
	var p arcparams.ReviewArcForFilingParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ReviewArcForFilingResult{Status: "skipped", Reason: "parse params: " + err.Error()}, nil
		}
	}
	if p.SessionID == "" {
		return ReviewArcForFilingResult{Status: "skipped", Reason: "session_id is required"}, nil
	}
	if p.TranscriptPath == "" {
		return ReviewArcForFilingResult{Status: "skipped", Reason: "transcript_path is required"}, nil
	}
	if deps.Pool == nil {
		return ReviewArcForFilingResult{Status: "skipped", Reason: "pool not configured"}, nil
	}

	// Debouncer first — cheap, self-contained, and the most common
	// suppression path. Run before the router check so backoff-skipped
	// calls don't surface as qwen_unreachable when the router is
	// configured-but-the-fire-is-redundant.
	deb := NewDebouncer(deps.Pool)
	if deps.BackoffSeconds > 0 {
		deb = deb.WithBackoffSeconds(deps.BackoffSeconds)
	}
	check, err := deb.CheckAndRecordAttempt(ctx, p.SessionID)
	if err != nil {
		return ReviewArcForFilingResult{Status: "skipped", Reason: "debouncer: " + err.Error()}, nil
	}
	if !check.Allowed {
		return ReviewArcForFilingResult{
			Status:     "debounced",
			Triggers:   p.Triggers,
			LastFireAt: formatDebouncerTimestamp(check.LastFire),
			Reason:     fmt.Sprintf("previous fire %.0fs ago; within %ds backoff", time.Since(check.LastFire).Seconds(), backoffOrDefault(deps.BackoffSeconds)),
		}, nil
	}

	if deps.Router == nil {
		return ReviewArcForFilingResult{Status: "qwen_unreachable", Reason: "router not configured", Triggers: p.Triggers}, nil
	}

	maxTurns := deps.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	maxTokens := deps.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	snap, err := ExtractSnapshot(p.TranscriptPath, maxTurns, maxTokens)
	if err != nil {
		obs.Logger(ctx).Warn("arcreview: snapshot extraction failed",
			"session_id", p.SessionID,
			"transcript_path", p.TranscriptPath,
			"err", err.Error())
		return ReviewArcForFilingResult{Status: "skipped", Reason: "snapshot: " + err.Error(), Triggers: p.Triggers}, nil
	}
	if len(snap.Messages) == 0 {
		return ReviewArcForFilingResult{Status: "skipped", Reason: "empty snapshot", Triggers: p.Triggers}, nil
	}

	// Per-arc content-hash dedupe (bug
	// `arc-close-filing-review-fires-multiply-on-overlapping-arc-within-single-session`,
	// 1482): cross-trigger fires arriving minutes apart but reflecting
	// the same underlying arc each produce their own Qwen review.
	// Hash the snapshot's joined content and short-circuit when a
	// prior fire within the TTL already covered this arc.
	arcHash := arcHashFromSnapshot(snap)
	hashCache := NewArcHashCache(deps.Pool)
	if deps.ArcHashCacheTTLSeconds > 0 {
		hashCache = hashCache.WithTTLSeconds(deps.ArcHashCacheTTLSeconds)
	}
	if entry, hit, lookupErr := hashCache.Lookup(ctx, p.SessionID, arcHash); lookupErr != nil {
		// Cache failure is non-fatal — fall through to firing.
		obs.Logger(ctx).Warn("arcreview: arc-hash-cache lookup failed; proceeding with fire",
			"session_id", p.SessionID,
			"err", lookupErr.Error())
	} else if hit {
		obs.Logger(ctx).Info("arcreview: arc-hash dedupe hit; skipping Qwen and surfacing prior decision",
			"session_id", p.SessionID,
			"prior_event_id", entry.PriorEventID,
			"prior_fired_at", entry.FiredAt.Format(time.RFC3339Nano))
		return ReviewArcForFilingResult{
			Status:       "arc_hash_dedup",
			Triggers:     p.Triggers,
			PriorEventID: entry.PriorEventID,
			Reason: fmt.Sprintf("arc content hash %s already fired at %s (session %s); within %ds TTL",
				arcHash[:12], entry.FiredAt.Format(time.RFC3339Nano), p.SessionID,
				arcHashTTLFor(deps)),
		}, nil
	}

	// In-arc dedupe enrichment. Two complementary sources:
	//   - Events-based query (bug 1472): BugReported rows in the project
	//     within the 30m window — covers cross-session and out-of-snapshot
	//     bug forges but misses vault notes (no typed event for them).
	//   - Snapshot-based extraction (bug 1480): scan the current snapshot's
	//     tool_use entries for forge(vault-note,...) and Write/Edit
	//     against /vault/ paths — covers the in-arc vault-note filings
	//     the event query can't see.
	// Both are non-fatal on failure; fall back to the un-enriched review.
	eventBased, recentErr := recentFilingsInArc(ctx, deps.Pool, project, time.Now().Add(-defaultRecentFilingsWindow))
	if recentErr != nil {
		obs.Logger(ctx).Warn("arcreview: recent-filings query failed; review fires without event-based dedupe enrichment",
			"session_id", p.SessionID,
			"err", recentErr.Error())
		eventBased = nil
	}
	snapshotBased := extractInArcFilingsFromSnapshot(snap)
	recent := mergeRecentFilings(eventBased, snapshotBased)

	result, err := DispatchReview(ctx, deps.Router, snap, p.Triggers, recent)
	if err != nil {
		obs.Logger(ctx).Warn("arcreview: dispatch failed; fail-open per design §Failure-modes",
			"session_id", p.SessionID,
			"err", err.Error())
		// Fail-open: classify Qwen-unreachable vs parse failure vs
		// prompt-too-large for the caller, but every path returns
		// non-error so the agent or harness doesn't see a tool error.
		status := "qwen_unreachable"
		switch {
		case errors.Is(err, ErrPromptTooLarge):
			status = "skipped"
		case strings.Contains(err.Error(), "arcreview parse:"):
			status = "skipped"
		}
		return ReviewArcForFilingResult{
			Status:     status,
			Reason:     err.Error(),
			Triggers:   p.Triggers,
			LatencyMS:  result.LatencyMS,
			ArcSummary: result.ArcSummary,
		}, nil
	}

	// Successful fire — record the timestamp for backoff.
	if recErr := deb.RecordFire(ctx, p.SessionID); recErr != nil {
		obs.Logger(ctx).Warn("arcreview: RecordFire failed; backoff may misbehave next call",
			"session_id", p.SessionID,
			"err", recErr.Error())
	}

	// F2 of chain arc-close-filing-review-dedupe-and-noise-reduction:
	// pre-filing dedupe against existing artifacts. Load the
	// project's bug / suggestion / vault index, then mark each
	// decision that exceeds the Jaccard title-token threshold.
	// partitionDecisions demotes matched decisions to a less-
	// aggressive bucket. Fail-open: index-load failure logs and
	// skips the dedupe step, leaving decisions to flow through
	// the normal partition.
	dedupeIndex, dedupeErr := LoadExistingArtifactsForDedupe(ctx, deps.Pool, project)
	if dedupeErr != nil {
		obs.Logger(ctx).Warn("arcreview: dedupe index load failed; F2 demotion skipped this fire",
			"session_id", p.SessionID,
			"err", dedupeErr.Error())
	} else {
		ApplyExistingArtifactDedupe(&result, dedupeIndex)
	}

	// F3 same-session dedupe window: load prior arc-close decisions
	// for this session_id within the retention window and demote
	// any current decision that matches a prior payload. Reuses
	// pending_decisions as the source (no new side-table). Fail-
	// open on query error.
	since := time.Now().Add(-sessionDedupeRetention())
	priorSessionDecisions, priorErr := LoadPriorSessionDecisions(ctx, deps.Pool, p.SessionID, since)
	if priorErr != nil {
		obs.Logger(ctx).Warn("arcreview: prior-session-decisions query failed; F3 demotion skipped this fire",
			"session_id", p.SessionID,
			"err", priorErr.Error())
	} else if len(priorSessionDecisions) > 0 {
		ApplySameSessionDedupe(&result, priorSessionDecisions)
	}

	// T6 same-session dedup guard (chain arc-close-decision-authoring-
	// split): downgrade any decision near an artifact the agent ALREADY
	// filed this session to enrich-existing (so the substrate never stages
	// a duplicate of the seat's own just-completed work). Reuses the in-arc
	// filing set already gathered for the prompt (recent = event-based bugs
	// + snapshot vault/bug forges) plus this session's MemoryWritten
	// entries. Fail-open: a memory-query error just drops the memory
	// dimension of the guard.
	enrichFilings := recent
	if memFilings, memErr := recentMemoryFilings(ctx, deps.Pool, time.Now().Add(-defaultRecentFilingsWindow)); memErr != nil {
		obs.Logger(ctx).Warn("arcreview: recent-memory-filings query failed; T6 memory dedup skipped this fire",
			"session_id", p.SessionID, "err", memErr.Error())
	} else if len(memFilings) > 0 {
		enrichFilings = mergeRecentFilings(recent, memFilings)
	}
	ApplyEnrichExistingDedupe(&result, enrichFilings)

	part := partitionDecisions(result.Decisions)

	// Emit the telemetry event. Failure to emit does not roll back the
	// fire — the review already produced typed decisions for the caller;
	// the event is the per-fire corpus row, not the load-bearing output.
	// Per design §Telemetry, every successful fire is one event row;
	// debounced / skipped / qwen_unreachable paths do NOT emit.
	eventID, emitErr := emitFilingReviewedEvent(ctx, deps.Pool, project, p, snap, result, part, maxTurns, maxTokens)
	if emitErr != nil {
		obs.Logger(ctx).Warn("arcreview: ArcCloseFilingReviewed emit failed; row lost from corpus",
			"session_id", p.SessionID,
			"err", emitErr.Error())
	}

	// Record the fire in the per-arc-content cache so a subsequent
	// cross-trigger fire on the same arc within the TTL short-circuits
	// at the dedupe gate above instead of running the Qwen pipeline a
	// second time. Skip when the emit lost the event row — without an
	// event_id the cache entry has no canonical decision to point at.
	if emitErr == nil && eventID != "" {
		if cacheErr := hashCache.Record(ctx, p.SessionID, arcHash, eventID); cacheErr != nil {
			obs.Logger(ctx).Warn("arcreview: arc-hash-cache record failed; next same-arc fire will re-run Qwen",
				"session_id", p.SessionID,
				"err", cacheErr.Error())
		}
	}

	// Reap-on-next-fire (chain arc-close-decision-authoring-split T5): this
	// arc-close fire is itself an arc boundary, so sweep the session's
	// EARLIER staged decisions that have aged past the grace window and were
	// never authored — forging their Qwen drafts flagged unreviewed. This is
	// the in-session trigger; the explicit session-end / skip trigger is the
	// sweep_unauthored_staged action. Best-effort: a sweep error never fails
	// the fire (the review already produced its output). No-op when ForgeFn
	// is unwired or nothing is past grace.
	if _, sweepErr := SweepUnauthoredStaged(ctx, deps, project, p.SessionID); sweepErr != nil {
		obs.Logger(ctx).Warn("arcreview: unreviewed-fallback sweep failed (non-fatal)",
			"session_id", p.SessionID,
			"err", sweepErr.Error())
	}

	return ReviewArcForFilingResult{
		Status:     "fired",
		Decisions:  result.Decisions,
		Partition:  part,
		Summary:    result.Summary,
		ArcSummary: result.ArcSummary,
		LatencyMS:  result.LatencyMS,
		Triggers:   p.Triggers,
		EventID:    eventID,
	}, nil
}

// emitFilingReviewedEvent writes one ArcCloseFilingReviewed row through
// the events substrate. The entity is the per-session arc-review fire;
// kind="arc_review_session" + slug=session_id. Project scoping carries
// through the envelope so per-project filing rates derive from the
// row. Returns the generated event_id so callers (the substrate-side
// observer) can link the corresponding pending_decisions row back to
// the corpus event for the T8 audit join.
func emitFilingReviewedEvent(
	ctx context.Context,
	pool *db.Pool,
	project string,
	params arcparams.ReviewArcForFilingParams,
	snap Snapshot,
	result ArcReviewResult,
	part PartitionedDecisions,
	maxTurns int,
	maxTokens int,
) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("arcreview: emit event: pool is nil")
	}
	summaries := make([]events.FilingDecisionSummary, len(result.Decisions))
	for i, d := range result.Decisions {
		summaries[i] = events.FilingDecisionSummary{
			Action:     string(d.Action),
			Confidence: d.Confidence,
			Reasoning:  d.Reasoning,
		}
	}
	var arcSummary *string
	if s := strings.TrimSpace(result.ArcSummary); s != "" {
		arcSummary = &s
	}
	triggers := params.Triggers
	if triggers == nil {
		triggers = []string{}
	}
	// F4 rejection telemetry: count + per-reason histogram from the
	// RejectedDecisions slice CheckBoilerplate populated upstream.
	var f4Reasons map[string]int
	if len(result.RejectedDecisions) > 0 {
		f4Reasons = make(map[string]int, len(result.RejectedDecisions))
		for _, rd := range result.RejectedDecisions {
			f4Reasons[string(rd.Reason)]++
		}
	}
	// F2 + F3 dedupe telemetry: count decisions where the
	// corresponding marker field is non-nil. Both filters mark
	// decisions in-place; the partition step demotes them but they
	// stay in result.Decisions for the audit trail.
	var f2Dedup, f3Dedup, enrichCount int
	for i := range result.Decisions {
		d := &result.Decisions[i]
		if d.DedupedAgainst != nil {
			f2Dedup++
		}
		if d.SameSessionDedupedAgainst != nil {
			f3Dedup++
		}
		if d.EnrichExisting != nil {
			enrichCount++
		}
	}

	payload := events.ArcCloseFilingReviewedPayload{
		SessionID:                 params.SessionID,
		Triggers:                  triggers,
		SnapshotTruncated:         snap.Truncated,
		SnapshotTokenCount:        snap.EstimatedTokens,
		SnapshotMessageCount:      len(snap.Messages),
		ArcSummary:                arcSummary,
		Decisions:                 summaries,
		AutoExecuteCount:          len(part.AutoExecute),
		SurfaceForConfirmCount:    len(part.SurfaceForConfirm),
		SkipCount:                 len(part.Skip),
		LatencyMS:                 result.LatencyMS,
		InputTokens:               result.InputTokens,
		OutputTokens:              result.OutputTokens,
		F4RejectedCount:           len(result.RejectedDecisions),
		F4RejectedReasons:         f4Reasons,
		F2DedupedCount:            f2Dedup,
		F3SameSessionDedupedCount: f3Dedup,
		// Chain arc-close-decision-authoring-split T7: the 'staged' state
		// of the author-vs-fallback instrument (paired with the
		// ArcCloseAuthoringResolved events the sweep emits), plus the
		// T6 enrich-existing downgrade count.
		StagedForAuthoringCount: len(part.StagedForAuthoring),
		EnrichExistingCount:     enrichCount,
	}
	var entity events.EntityRef
	if project == "" {
		entity = events.NewCrossCuttingEntityRef("arc_review_session", params.SessionID)
	} else {
		entity = events.NewEntityRef("arc_review_session", params.SessionID, project)
	}
	var eventID string
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  entity,
			Payload: payload,
		})
		if err != nil {
			return err
		}
		eventID = id
		// Capture the snapshot content into the training corpus in the
		// SAME tx as the event emit (chain arc-close-snapshot-corpus-
		// capture T2): no fire-with-snapshot lands without its corpus row,
		// and any corpus-insert failure rolls the whole fire back. Empty
		// snapshots never reach here (the review skips them upstream), but
		// the guard keeps event-emission decoupled from corpus-capture for
		// that edge case.
		if len(snap.Messages) > 0 {
			if err := insertSnapshotCorpusRow(ctx, tx, id, params.SessionID, snap, maxTurns, maxTokens); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return eventID, nil
}

// insertSnapshotCorpusRow writes the live snapshot-corpus row for a just-
// emitted ArcCloseFilingReviewed event, inside that event's write tx. The
// stored messages_json is the EXACT kept-message set fed to Qwen (same
// order), so the corpus row reproduces the model input faithfully. fire_ts
// is read back from the just-inserted event row (read-your-write within the
// tx) so the corpus and the event agree on the fire timestamp. See
// docs/ARC_CLOSE_SNAPSHOT_CORPUS.md + migration 074.
func insertSnapshotCorpusRow(ctx context.Context, tx *sql.Tx, eventID, sessionID string, snap Snapshot, maxTurns, maxTokens int) error {
	msgsJSON, err := json.Marshal(snap.Messages)
	if err != nil {
		return fmt.Errorf("arcreview: marshal snapshot messages for corpus: %w", err)
	}
	var fireTS string
	if err := tx.QueryRowContext(ctx, `SELECT ts FROM events WHERE event_id = ?`, eventID).Scan(&fireTS); err != nil {
		return fmt.Errorf("arcreview: read fire_ts for corpus row: %w", err)
	}
	truncated := 0
	if snap.Truncated {
		truncated = 1
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO arcreview_snapshot_corpus
			(event_id, session_id, fire_ts, messages_json, message_count,
			 estimated_tokens, truncated, max_turns, max_tokens, source, schema_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'live', 1)`,
		eventID, sessionID, fireTS, string(msgsJSON), len(snap.Messages),
		snap.EstimatedTokens, truncated, maxTurns, maxTokens)
	if err != nil {
		return fmt.Errorf("arcreview: insert snapshot corpus row (event %s): %w", eventID, err)
	}
	return nil
}

// partitionDecisions splits decisions into the three dispatch buckets
// per design Q5: auto_execute (high confidence + auto-executable
// kind), surface_for_confirm (medium confidence or skill_update at any
// confidence), skip (low confidence). nothing_to_file decisions are
// telemetry-only and land in Skip so the caller has the full set in
// one place.
func partitionDecisions(decisions []FilingDecision) PartitionedDecisions {
	out := PartitionedDecisions{
		AutoExecute:        []FilingDecision{},
		StagedForAuthoring: []FilingDecision{},
		SurfaceForConfirm:  []FilingDecision{},
		Skip:               []FilingDecision{},
	}
	for _, d := range decisions {
		switch {
		case d.Action == ActionNothingToFile:
			out.Skip = append(out.Skip, d)
		case d.Action == ActionSkillUpdate:
			// Skill updates always surface for confirm regardless of
			// confidence — per design Q5, they edit live skill files
			// and cross the auto-execute scope boundary.
			out.SurfaceForConfirm = append(out.SurfaceForConfirm, d)
		case d.EnrichExisting != nil:
			// T6 same-session dedup guard (chain arc-close-decision-
			// authoring-split): the agent already filed a matching
			// artifact THIS session. Never stage / auto-forge a
			// duplicate — surface it as "enrich the existing one"
			// regardless of confidence. Checked before staging and the
			// F2/F3 demotion because it's the strongest don't-duplicate
			// signal (the seat's own just-completed work).
			out.SurfaceForConfirm = append(out.SurfaceForConfirm, d)
		case d.DedupedAgainst != nil || d.SameSessionDedupedAgainst != nil:
			// F2 + F3 demotion (chain arc-close-filing-review-dedupe-
			// and-noise-reduction): proposed decision matches either
			// an existing artifact (F2) OR a prior arc-close fire in
			// the same session (F3). Demote one bucket regardless of
			// confidence. The operator can still confirm via the
			// surface_for_confirm bucket if the duplicate is genuinely
			// worth re-filing. A deduped in-scope kind is demoted here,
			// NOT staged — there's nothing fresh to author (T6 refines
			// the same-session case to "enrich the existing note").
			if d.Confidence >= autoExecuteConfidence && autoExecuteActions[d.Action] {
				out.SurfaceForConfirm = append(out.SurfaceForConfirm, d)
			} else {
				out.Skip = append(out.Skip, d)
			}
		case d.Confidence >= autoExecuteConfidence && inScopeBodyHeavyActions[d.Action]:
			// Decision/authoring split (chain
			// arc-close-decision-authoring-split T4): a body-heavy kind
			// in the auto-execute band is NOT auto-forged with Qwen's
			// draft body. Stage it for the in-session agent to author
			// (Qwen attributed as decider). Qwen's draft stays in
			// Payload for the T5 fallback. Checked BEFORE the plain
			// auto-execute branch because inScopeBodyHeavyActions ⊂
			// autoExecuteActions.
			d.StagedForAuthoring = true
			out.StagedForAuthoring = append(out.StagedForAuthoring, d)
		case d.Confidence >= autoExecuteConfidence && autoExecuteActions[d.Action]:
			out.AutoExecute = append(out.AutoExecute, d)
		case d.Confidence >= surfaceConfidence:
			out.SurfaceForConfirm = append(out.SurfaceForConfirm, d)
		default:
			out.Skip = append(out.Skip, d)
		}
	}
	return out
}

// backoffOrDefault returns the configured backoff or
// DefaultBackoffSeconds when not set.
func backoffOrDefault(s int) int {
	if s <= 0 {
		return DefaultBackoffSeconds
	}
	return s
}
