// Package arcparams holds the pure-data param structs for the arc-close
// filing-review work-surface actions (review_arc_for_filing, emit_commit_landed,
// arc_review_audit, pending_decisions_claim).
//
// They live in this dependency-light leaf package — deliberately separate from
// internal/arcreview, which pulls in the Qwen inference stack (inference/llamacpp,
// inference/router, qwenctx) — so the work action-doc registry (chain
// establish-action-doc-contract-on-work) can reference each struct's reflect.Type
// to DERIVE that action's documented param shape WITHOUT coupling package work to
// the inference dependencies. Importing internal/arcreview just to read a
// reflect.Type would drag the whole inference stack into the task-tracker package.
//
// Separation of concerns: these are the wire-INPUT contracts of the actions; the
// behavior (handlers, result types, the review pipeline) stays in internal/arcreview.
// This package imports nothing — it is a pure leaf.
package arcparams

// ReviewArcForFilingParams captures the trigger payload emitted by
// hooks/arc-close-detector.sh on the harness side or by the substrate
// event listener (Stage 3) on the toolkit-server side. Field tags
// match the JSON shape the detector emits.
type ReviewArcForFilingParams struct {
	SessionID            string   `json:"session_id"`
	FiredAt              string   `json:"fired_at"`
	Triggers             []string `json:"triggers"`
	UserTurnsSinceReview int      `json:"user_turns_since_review"`
	TranscriptPath       string   `json:"transcript_path"`
}

// EmitCommitLandedParams is the input to work.emit_commit_landed. The
// post-commit advisor invokes this via HTTP so the emit lands inside
// the daemon's process — the per-process fold hook installed at startup
// (with SubstrateReviewObserver) fires only on events emitted from the
// daemon itself, NOT on events emitted from one-shot binaries. Routing
// the emit through the action handler is what makes the substrate
// listener see the CommitLanded trigger.
type EmitCommitLandedParams struct {
	CommitSHA         string  `json:"commit_sha"`
	Branch            *string `json:"branch,omitempty"`
	FilesChangedCount *int    `json:"files_changed_count,omitempty"`
	Author            *string `json:"author,omitempty"`
	Subject           *string `json:"subject,omitempty"`
}

// ArcReviewAuditParams is the input to work.arc_review_audit.
//
// All fields are optional. The defaults below shape the standard
// "what fired recently and how did it land?" query the T9 prompt-
// tuning and T10 corpus-export consumers run.
type ArcReviewAuditParams struct {
	// Since filters by event ts (ISO-8601, inclusive lower bound).
	// Default: 7 days ago. The audit's primary use is recent-history
	// review; older corpora are queried by explicit export rather than
	// per-call audit.
	Since string `json:"since,omitempty"`
	// CorrectionWindowHours sets the look-ahead window for the
	// heuristic user-correction join: the audit treats subsequent
	// BugReopened / BugEdited / TaskCancelled events within this window
	// of a review fire as candidate corrections of the auto-filed
	// decisions. Default 24h. See HeuristicCorrectionNote on the result
	// for the false-positive / false-negative classes documented inline.
	CorrectionWindowHours int `json:"correction_window_hours,omitempty"`
}

// PendingDecisionsClaimParams is the input to work.pending_decisions_claim.
// The Stop hook calls the action with its own session_id (which lands on
// every claimed row as dispatch_session_id) and the project scope. Limit
// caps the number of rows claimed in one call; the default below applies
// when omitted or zero.
type PendingDecisionsClaimParams struct {
	SessionID string `json:"session_id"`
	Limit     int    `json:"limit,omitempty"`
}

// SweepUnauthoredStagedParams is the input to work.sweep_unauthored_staged
// (chain arc-close-decision-authoring-split T5). A session-end / session-
// start reaper hook (or an explicit agent skip) calls it with the session
// whose stale staged decisions should be captured: any decision the seat
// never authored is forged from Qwen's retained draft, flagged unreviewed.
type SweepUnauthoredStagedParams struct {
	SessionID string `json:"session_id"`
}
