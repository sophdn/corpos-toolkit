package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"toolkit/internal/arcreview/arcparams"
	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// Skip reason values for the ArcReviewListenerFired event. Closed set
// matching the enum description in blueprints/events/ArcReviewListenerFired.json.
const (
	listenerSkipNoProjectID            = "no_project_id"
	listenerSkipSessionLookupFailed    = "session_lookup_failed"
	listenerSkipNoActiveSession        = "no_active_session"
	listenerSkipMarshalParamsFailed    = "marshal_params_failed"
	listenerSkipHandlerError           = "handler_error"
	listenerSkipReviewNonFired         = "review_non_fired"
	listenerSkipPendingDecisionsFailed = "pending_decisions_write_failed"
	// listenerSkipMetaSubstrateChain fires when the trigger commit
	// modifies primarily the arc-close substrate ITSELF (per
	// isMetaSubstrateChainCommit). Suppresses the feedback loop
	// where Qwen reviews the substrate's own commits.
	listenerSkipMetaSubstrateChain = "meta_substrate_chain"
)

// SubstrateReviewObserver is the production TriggerObserver that wires
// substrate event triggers (BugResolved / TaskCompleted / ChainClosed /
// CommitLanded / RoadmapUpdated) into real review fires.
//
// On Observe: kicks a detached goroutine that
//  1. looks up the most-recently-active session for the event's project
//     in session_registry (written by hooks/arc-close-filing-review-hook.sh
//     on every Stop event — see T3),
//  2. synthesizes a ReviewArcForFilingParams with the resolved
//     session_id + transcript_path and a single-element triggers slice
//     carrying the substrate trigger slug,
//  3. invokes HandleReviewArcForFiling (debouncer-guarded; emits the
//     ArcCloseFilingReviewed corpus event),
//  4. on status="fired" enqueues a pending_decisions row for the next
//     Stop hook to claim and surface via system-reminder.
//
// Observe itself returns immediately; the actual review goroutine runs
// out-of-tx so the events-fold hook does not extend the write
// transaction's lifetime past its actual work.
//
// Replaces LogOnlyTriggerObserver per
// docs/ARC_CLOSE_FILING_REVIEW_SUBSTRATE_LISTENER.md §Sequence diagram.
type SubstrateReviewObserver struct {
	Deps Deps
}

// NewSubstrateReviewObserver returns a SubstrateReviewObserver bound to
// the dependency bundle the action handler also uses (Pool + Router).
func NewSubstrateReviewObserver(deps Deps) SubstrateReviewObserver {
	return SubstrateReviewObserver{Deps: deps}
}

// Observe kicks the review goroutine. Returns immediately so the
// events-fold transaction can commit without waiting on the review.
func (o SubstrateReviewObserver) Observe(_ context.Context, evt SubstrateTriggerEvent) {
	// Detach from the caller's ctx — the fold-tx ctx will be cancelled
	// when the tx commits. A fresh background ctx lets the goroutine
	// outlive the tx.
	go o.fireReview(context.Background(), evt)
}

// fireReview runs the resolve → review → enqueue pipeline for one
// substrate trigger. Every exit path emits one ArcReviewListenerFired
// event so the events ledger carries a structured record of the
// observer's outcome (fix for bug
// `stdio-process-observer-logs-not-captured-in-central-log-file` —
// stdio MCP processes' obs.Logger lines don't reach /tmp/toolkit-http.log,
// so the events row is the canonical observer-activity signal). Logs
// are kept alongside for the HTTP-process case where they're visible in
// the central log.
func (o SubstrateReviewObserver) fireReview(ctx context.Context, evt SubstrateTriggerEvent) {
	if evt.ProjectID == nil {
		obs.Logger(ctx).Info("arcreview substrate observer: skip (no project_id on trigger event)",
			"trigger_event_id", evt.EventID,
			"event_type", evt.EventType)
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{status: "skipped", skipReason: listenerSkipNoProjectID})
		return
	}
	project := *evt.ProjectID

	sess, err := lookupActiveSession(ctx, o.Deps.Pool, project)
	if err != nil {
		obs.Logger(ctx).Warn("arcreview substrate observer: session_registry lookup failed",
			"trigger_event_id", evt.EventID,
			"project", project,
			"err", err.Error())
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{status: "skipped", skipReason: listenerSkipSessionLookupFailed})
		return
	}
	if sess == nil {
		obs.Logger(ctx).Info("arcreview substrate observer: no active session for project; skip",
			"trigger_event_id", evt.EventID,
			"project", project,
			"trigger", evt.TriggerSlug)
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{status: "skipped", skipReason: listenerSkipNoActiveSession})
		return
	}

	// Meta-substrate-chain detection: when the trigger commit modifies
	// primarily the arc-close substrate itself, suppress the review to
	// avoid the feedback loop where Qwen reviews the substrate's own
	// commits and produces paraphrase-shape filings. See suggestion
	// `meta-session-arc-close-downweighting-when-chain-targets-
	// arcreview-itself` for rationale + the F5 retrospective data
	// documenting the 22% signal-to-noise depression during chain 618.
	// Fail-open: detection errors return false; the review still fires.
	if isMetaSubstrateChainCommit(ctx, evt) {
		obs.Logger(ctx).Info("arcreview substrate observer: meta-substrate-chain commit detected; suppress review",
			"trigger_event_id", evt.EventID,
			"project", project,
			"trigger", evt.TriggerSlug,
			"session_id", sess.SessionID)
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{
			status:     "skipped",
			skipReason: listenerSkipMetaSubstrateChain,
			sessionID:  sess.SessionID,
		})
		return
	}

	paramsJSON, err := json.Marshal(arcparams.ReviewArcForFilingParams{
		SessionID:      sess.SessionID,
		TranscriptPath: sess.TranscriptPath,
		Triggers:       []string{evt.TriggerSlug},
		FiredAt:        time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		obs.Logger(ctx).Warn("arcreview substrate observer: marshal params failed",
			"trigger_event_id", evt.EventID,
			"err", err.Error())
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{
			status:     "skipped",
			skipReason: listenerSkipMarshalParamsFailed,
			sessionID:  sess.SessionID,
		})
		return
	}

	res, err := HandleReviewArcForFiling(ctx, o.Deps, project, paramsJSON)
	if err != nil {
		obs.Logger(ctx).Warn("arcreview substrate observer: review handler returned error",
			"trigger_event_id", evt.EventID,
			"session_id", sess.SessionID,
			"err", err.Error())
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{
			status:     "skipped",
			skipReason: listenerSkipHandlerError,
			sessionID:  sess.SessionID,
		})
		return
	}
	if res.Status != "fired" {
		obs.Logger(ctx).Info("arcreview substrate observer: review non-fired",
			"trigger_event_id", evt.EventID,
			"session_id", sess.SessionID,
			"review_status", res.Status,
			"reason", res.Reason)
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{
			status:       "skipped",
			skipReason:   listenerSkipReviewNonFired,
			sessionID:    sess.SessionID,
			reviewStatus: res.Status,
		})
		return
	}

	if err := writePendingDecisions(ctx, o.Deps.Pool, project, sess.SessionID, res); err != nil {
		obs.Logger(ctx).Warn("arcreview substrate observer: pending_decisions write failed",
			"trigger_event_id", evt.EventID,
			"review_event_id", res.EventID,
			"session_id", sess.SessionID,
			"err", err.Error())
		o.emitListenerFired(ctx, evt, listenerFiredOutcome{
			status:        "skipped",
			skipReason:    listenerSkipPendingDecisionsFailed,
			sessionID:     sess.SessionID,
			reviewEventID: res.EventID,
			reviewStatus:  res.Status,
		})
		return
	}
	obs.Logger(ctx).Info("arcreview substrate observer: review fired and queued for dispatch",
		"trigger_event_id", evt.EventID,
		"review_event_id", res.EventID,
		"session_id", sess.SessionID,
		"decisions", len(res.Decisions))
	decisions := len(res.Decisions)
	o.emitListenerFired(ctx, evt, listenerFiredOutcome{
		status:         "fired",
		sessionID:      sess.SessionID,
		reviewEventID:  res.EventID,
		reviewStatus:   res.Status,
		decisionsCount: &decisions,
	})
}

// listenerFiredOutcome bundles the fields that populate the
// ArcReviewListenerFired event payload. Constructed at each fireReview
// exit and passed to emitListenerFired.
type listenerFiredOutcome struct {
	status         string // "fired" | "skipped"
	skipReason     string
	sessionID      string
	reviewEventID  string
	reviewStatus   string
	decisionsCount *int
}

// emitListenerFired writes one ArcReviewListenerFired event capturing
// the observer's outcome. Failures drift-log and swallow — never
// re-enter the trigger detection loop and never block Stop flow.
//
// Safe against listener re-entrance: ArcReviewListenerFired is NOT in
// arcreview.SubstrateTriggerEventTypes, so the chained fold-hook will
// not invoke Observe again on this emission.
func (o SubstrateReviewObserver) emitListenerFired(ctx context.Context, trig SubstrateTriggerEvent, out listenerFiredOutcome) {
	if o.Deps.Pool == nil {
		return
	}
	payload := events.ArcReviewListenerFiredPayload{
		TriggerEventID:   trig.EventID,
		TriggerEventType: trig.EventType,
		TriggerSlug:      trig.TriggerSlug,
		ProjectID:        trig.ProjectID,
		Status:           out.status,
	}
	if out.skipReason != "" {
		s := out.skipReason
		payload.SkipReason = &s
	}
	if out.sessionID != "" {
		s := out.sessionID
		payload.SessionID = &s
	}
	if out.reviewEventID != "" {
		s := out.reviewEventID
		payload.ReviewEventID = &s
	}
	if out.reviewStatus != "" {
		s := out.reviewStatus
		payload.ReviewStatus = &s
	}
	payload.DecisionsCount = out.decisionsCount

	// Entity key: the trigger's project + an arc_review_listener slug
	// per session so the per-session timeline shows the observer's
	// activity alongside the underlying ArcCloseFilingReviewed rows.
	// Fall back to trigger entity slug when no session resolved (the
	// no_active_session / no_project_id paths).
	slug := out.sessionID
	if slug == "" {
		slug = trig.EntitySlug
		if slug == "" {
			slug = trig.EventID
		}
	}
	var entity events.EntityRef
	if trig.ProjectID == nil || *trig.ProjectID == "" {
		entity = events.NewCrossCuttingEntityRef("arc_review_listener", slug)
	} else {
		entity = events.NewEntityRef("arc_review_listener", slug, *trig.ProjectID)
	}
	caused := trig.EventID
	err := o.Deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  entity,
			Payload: payload,
			Refs:    events.Refs{CausedByEventID: &caused},
		})
		return err
	})
	if err != nil {
		obs.Logger(ctx).Warn("arcreview substrate observer: ArcReviewListenerFired emit failed",
			"trigger_event_id", trig.EventID,
			"err", err.Error())
	}
}

// filterActionableDecisions returns the decisions the agent should actually
// see at the next Stop drain: the auto_execute + surface_for_confirm
// partition, per partitionDecisions (the single source of truth). The skip
// tier is dropped — nothing_to_file, sub-threshold confidence, AND demoted
// non-auto duplicates — so the drain never injects noise the agent must skim
// past and be told to skip (chain quiet-and-instrument-operator-surface T3;
// extends bug 1471's nothing_to_file-only filter to the whole skip tier).
// Re-partitions res.Decisions, which still carry their F2/F3 DedupedAgainst
// markers, so dedup demotion is honored. NOTE: high-confidence decisions that
// SHOULD have been deduped but weren't (an F2/F3 gap) still land in
// auto/surface and get enqueued — that's a separate dedup-gap lever, not this
// partition filter.
func filterActionableDecisions(decisions []FilingDecision) []FilingDecision {
	part := partitionDecisions(decisions)
	out := make([]FilingDecision, 0, len(part.AutoExecute)+len(part.StagedForAuthoring)+len(part.SurfaceForConfirm))
	out = append(out, part.AutoExecute...)
	// Staged-for-authoring decisions (chain arc-close-decision-authoring-
	// split T4) are actionable: the drain hook must surface them so the
	// agent can author the body. They ride along with their
	// StagedForAuthoring flag set, which the drain hook reads to render an
	// authoring prompt rather than a verbatim-forge directive.
	out = append(out, part.StagedForAuthoring...)
	out = append(out, part.SurfaceForConfirm...)
	return out
}

// activeSession is the row session_registry lookup returns.
type activeSession struct {
	SessionID      string
	TranscriptPath string
}

// lookupActiveSession returns the most-recently-active session for the
// project, or (nil, nil) when no row exists.
//
// Best-effort attribution (bug 947, decided option B). A project-level trigger
// (CommitLanded / BugResolved / TaskCompleted / ChainClosed / RoadmapUpdated)
// carries no originating session_id, so the observer attributes it to the
// most-recently-active session for the project. Under concurrent same-project
// sessions (multi-agent) that may not be the session that produced the trigger —
// e.g. session B's commit can trigger a review of session A's transcript when A
// was active more recently. This is accepted as deliberate best-effort, NOT a
// bug to chase further, because the read-side delivery scoping (bug 945:
// claimPendingDecisions filters on target_session_id) already guarantees a
// session only ever RECEIVES decisions targeted at it. So the residual cost of a
// mis-attributed review is a possibly-mis-timed review of an *active* session —
// never a cross-session content bleed.
//
// Precise attribution was rejected as infeasible for the trigger that actually
// caused the 945 incident: CommitLanded is emitted by the post-commit hook
// (cmd/commit-landed-emit), which has no Claude-session identity (CommitLandedPayload
// has no session_id field). The MCP-emitted triggers (BugResolved/TaskCompleted/
// ChainClosed/RoadmapUpdated) DO have MCPSessionIDFromContext available and could
// stamp an originating session_id — the reopening condition if precise
// per-trigger attribution is ever needed; CommitLanded would remain best-effort.
func lookupActiveSession(ctx context.Context, pool *db.Pool, project string) (*activeSession, error) {
	if pool == nil {
		return nil, fmt.Errorf("arcreview: lookupActiveSession: pool is nil")
	}
	row := pool.DB().QueryRowContext(ctx, `
		SELECT session_id, transcript_path
		FROM session_registry
		WHERE project_id = ?
		ORDER BY last_active_at DESC, session_id DESC
		LIMIT 1
	`, project)
	var s activeSession
	if err := row.Scan(&s.SessionID, &s.TranscriptPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("arcreview: session_registry scan: %w", err)
	}
	return &s, nil
}

// writePendingDecisions inserts one pending_decisions row carrying the
// fired review's actionable decisions + triggers + arc_summary. The
// next Stop hook for the project claims the row via
// work.pending_decisions_claim and surfaces the decisions as a
// system-reminder block (T5).
//
// nothing_to_file decisions are FILTERED OUT before persistence (bug
// 1471): they're telemetry-only in the in-band partitionDecisions
// path (handler.go) and the substrate-listener path should match.
// The corpus row (ArcCloseFilingReviewed event) retains them
// untouched so T9 tuning sees the full distribution; only the
// dispatch queue is filtered.
//
// If every decision is nothing_to_file (or the slice is empty), no
// pending_decisions row is written at all — surfacing "1 typed
// filing decision: nothing_to_file" via system-reminder was
// load-bearing noise the agent had to skim past every Stop drain.
func writePendingDecisions(ctx context.Context, pool *db.Pool, project, targetSessionID string, res ReviewArcForFilingResult) error {
	if pool == nil {
		return fmt.Errorf("arcreview: writePendingDecisions: pool is nil")
	}
	actionable := filterActionableDecisions(res.Decisions)
	if len(actionable) == 0 {
		// Nothing to dispatch — the corpus event already landed; skip
		// the pending_decisions write entirely.
		return nil
	}
	decisionsJSON, err := json.Marshal(actionable)
	if err != nil {
		return fmt.Errorf("marshal decisions: %w", err)
	}
	triggers := res.Triggers
	if triggers == nil {
		triggers = []string{}
	}
	triggersJSON, err := json.Marshal(triggers)
	if err != nil {
		return fmt.Errorf("marshal triggers: %w", err)
	}
	arcSummary := sql.NullString{String: res.ArcSummary, Valid: res.ArcSummary != ""}
	// authoring_state='staged' when any actionable decision is staged for
	// agent authoring (chain arc-close-decision-authoring-split T5) — this
	// is what the unreviewed-fallback sweep keys on. NULL for rows carrying
	// only auto_execute / surface_for_confirm decisions (the steady state).
	authoringState := sql.NullString{}
	for _, d := range actionable {
		if d.StagedForAuthoring {
			authoringState = sql.NullString{String: authoringStateStaged, Valid: true}
			break
		}
	}
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `
			INSERT INTO pending_decisions
				(event_id, project_id, target_session_id, decisions_json, triggers_json, arc_summary, authoring_state)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, res.EventID, project, targetSessionID, string(decisionsJSON), string(triggersJSON), arcSummary, authoringState)
		if execErr != nil {
			return fmt.Errorf("insert pending_decisions: %w", execErr)
		}
		return nil
	})
}
