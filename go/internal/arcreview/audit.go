package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"toolkit/internal/arcreview/arcparams"
	"toolkit/internal/db"
)

// ArcReviewAuditResult bundles every review-row in the window plus a
// human-readable note describing the heuristic correction join.
// Consumers (T9 tuning, T10 corpus export) read both: the rows feed
// metrics, the note documents the false-positive / false-negative
// classes so analysis stays honest.
type ArcReviewAuditResult struct {
	Reviews                 []ArcReviewAuditRow `json:"reviews"`
	HeuristicCorrectionNote string              `json:"heuristic_correction_note"`
	WindowStart             string              `json:"window_start"`
	CorrectionWindowHours   int                 `json:"correction_window_hours"`
	// AuthorVsFallback is the decision/authoring-split instrument (chain
	// arc-close-decision-authoring-split T7): how often a staged
	// body-heavy decision was authored by the seated agent vs captured by
	// the unreviewed fallback. nil when no staged decisions exist in the
	// window. The rate is the seat-strength signal — how drivable the
	// authoring reflex is for whatever model/harness is seated.
	AuthorVsFallback *AuthorVsFallbackSummary `json:"author_vs_fallback,omitempty"`
}

// AuthorVsFallbackSummary aggregates pending_decisions.authoring_state over
// the audit window into the author-vs-fallback rate. Warmup-aware
// (telemetry-conventions): Rate is non-nil only once Resolved meets a
// minimum sample so a 1-of-1 fire doesn't read as a 100% authoring rate.
type AuthorVsFallbackSummary struct {
	Staged         int      `json:"staged"`          // still awaiting authoring
	Authored       int      `json:"authored"`        // agent authored; fallback suppressed
	FallbackForged int      `json:"fallback_forged"` // unreviewed fallback captured the draft
	Resolved       int      `json:"resolved"`        // authored + fallback_forged
	Rate           *float64 `json:"rate,omitempty"`  // authored / resolved; nil until warmup
}

// authorVsFallbackMinSample is the warmup floor: the rate is suppressed
// (nil) until at least this many staged decisions have resolved, so an
// early 1-of-1 doesn't masquerade as a stable 100% authoring rate.
const authorVsFallbackMinSample = 5

// ArcReviewAuditRow is one ArcCloseFilingReviewed event with its
// dispatch + correction state denormalized into a flat shape.
type ArcReviewAuditRow struct {
	ReviewID               string             `json:"review_id"`
	TS                     string             `json:"ts"`
	SessionID              string             `json:"session_id"`
	ProjectID              string             `json:"project_id,omitempty"`
	TriggerSignals         []string           `json:"trigger_signals"`
	SnapshotTokenCount     int                `json:"snapshot_token_count"`
	SnapshotMessageCount   int                `json:"snapshot_message_count"`
	LatencyMS              int64              `json:"latency_ms"`
	AutoExecuteCount       int                `json:"auto_execute_count"`
	SurfaceForConfirmCount int                `json:"surface_for_confirm_count"`
	SkipCount              int                `json:"skip_count"`
	ArcSummary             string             `json:"arc_summary,omitempty"`
	Decisions              []AuditDecisionRow `json:"decisions"`
	DispatchStatus         string             `json:"dispatch_status"`
	DispatchedAt           string             `json:"dispatched_at,omitempty"`
	DispatchSessionID      string             `json:"dispatch_session_id,omitempty"`
	UserCorrectionSignals  []CorrectionSignal `json:"user_correction_signals"`
}

// AuditDecisionRow is one decision out of a review's parsed_decisions.
type AuditDecisionRow struct {
	Action     string  `json:"action"`
	Confidence float64 `json:"confidence"`
	Reasoning  string  `json:"reasoning,omitempty"`
}

// CorrectionSignal is one heuristic candidate-correction event picked
// up by the time-windowed scan after a review fire. Signal type
// names the event type that landed; entity_kind+entity_slug let
// consumers cross-reference against the events ledger.
type CorrectionSignal struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	EntityKind string `json:"entity_kind"`
	EntitySlug string `json:"entity_slug"`
	TS         string `json:"ts"`
	DeltaSec   int64  `json:"delta_sec"`
}

// heuristicCorrectionNote is the inline documentation T8's spec demands
// — what the join catches and what it misses. Consumers read this
// before drawing conclusions from the user_correction_signals slice.
const heuristicCorrectionNote = "user_correction_signals join is HEURISTIC: it scans events within correction_window_hours of each review fire for {BugReopened, BugEdited, TaskCancelled, BugResolved, ChainEdited} on the project. False-positives: the user / agent may edit unrelated rows within the window. False-negatives: corrections outside the window (multi-day reflection), or expressed via memory_write / vault note edits (which don't have typed events yet). For training-corpus use (chain T10), treat signals as candidate labels; manual triage filters before promotion to ground truth."

// correctionEventTypes is the closed set of event types the heuristic
// join treats as candidate-correction signals. Restricted to mutation-
// shaped events on bug / task / chain entities so the noise level
// stays low; future tightening or loosening is a data question.
var correctionEventTypes = []string{
	"BugReopened",
	"BugEdited",
	"TaskCancelled",
	"BugResolved", // catches wontfix-after-auto-file
	"ChainEdited", // chain mutations close to a review timestamp suggest reorg post-review
}

// HandleArcReviewAudit returns the audit rows for the project + window.
// The query is two-stage: (1) fetch ArcCloseFilingReviewed rows + their
// matching pending_decisions dispatch state; (2) for each row, scan
// candidate-correction events within the configured window.
//
// Performance: a single covering index on (type, ts) (already present
// per the events schema) makes the time-windowed scan in stage 2
// linear in the candidate count. With 10K reviews and a 24h window
// the call stays well under 500ms in practice; we keep an eye on this
// via the inline counters but don't add an index until measurements
// demand one.
func HandleArcReviewAudit(ctx context.Context, deps Deps, project string, params json.RawMessage) (ArcReviewAuditResult, error) {
	var p arcparams.ArcReviewAuditParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ArcReviewAuditResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if deps.Pool == nil {
		return ArcReviewAuditResult{}, fmt.Errorf("pool not configured")
	}

	since := p.Since
	if since == "" {
		since = time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	}
	window := p.CorrectionWindowHours
	if window <= 0 {
		window = 24
	}

	reviews, err := fetchReviews(ctx, deps.Pool, project, since)
	if err != nil {
		return ArcReviewAuditResult{}, fmt.Errorf("fetch reviews: %w", err)
	}
	for i := range reviews {
		signals, err := fetchCorrectionSignals(ctx, deps.Pool, project, reviews[i].TS, window)
		if err != nil {
			return ArcReviewAuditResult{}, fmt.Errorf("fetch corrections for %s: %w", reviews[i].ReviewID, err)
		}
		reviews[i].UserCorrectionSignals = signals
	}
	avf, err := authorVsFallbackSummary(ctx, deps.Pool, project, since)
	if err != nil {
		return ArcReviewAuditResult{}, fmt.Errorf("author-vs-fallback summary: %w", err)
	}
	return ArcReviewAuditResult{
		Reviews:                 reviews,
		HeuristicCorrectionNote: heuristicCorrectionNote,
		WindowStart:             since,
		CorrectionWindowHours:   window,
		AuthorVsFallback:        avf,
	}, nil
}

// authorVsFallbackSummary counts pending_decisions.authoring_state over the
// window for the project and computes the authoring rate. Returns nil when
// no staged-lineage rows exist (the split never engaged in the window).
// datetime() normalizes both the RFC3339 `since` and the space-separated
// stored created_at so the comparison doesn't mis-order on the 'T' vs ' '
// separator.
func authorVsFallbackSummary(ctx context.Context, pool *db.Pool, project, since string) (*AuthorVsFallbackSummary, error) {
	row := pool.DB().QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN authoring_state = 'staged'          THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN authoring_state = 'authored'        THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN authoring_state = 'fallback_forged' THEN 1 ELSE 0 END), 0)
		FROM pending_decisions
		WHERE (? = '' OR project_id = ?)
		  AND authoring_state IS NOT NULL
		  AND datetime(created_at) >= datetime(?)`,
		project, project, since)
	var s AuthorVsFallbackSummary
	if err := row.Scan(&s.Staged, &s.Authored, &s.FallbackForged); err != nil {
		return nil, err
	}
	if s.Staged == 0 && s.Authored == 0 && s.FallbackForged == 0 {
		return nil, nil
	}
	s.Resolved = s.Authored + s.FallbackForged
	if s.Resolved >= authorVsFallbackMinSample {
		r := roundTo(float64(s.Authored)/float64(s.Resolved), 3)
		s.Rate = &r
	}
	return &s, nil
}

// fetchReviews joins ArcCloseFilingReviewed events with pending_decisions
// to surface dispatch state alongside the corpus row. The LEFT JOIN
// catches reviews where no pending row was written (e.g. the Stop hook
// dispatched directly via the in-band detector path — those reviews
// fired through the harness-side flow, not the substrate listener, so
// they have no pending_decisions row).
func fetchReviews(ctx context.Context, pool *db.Pool, project, since string) ([]ArcReviewAuditRow, error) {
	binds := db.NewArgs().AddString(since)
	q := `
		SELECT e.event_id, e.ts, e.entity_project_id, e.payload,
		       p.dispatched_at, p.dispatch_session_id
		FROM events e
		LEFT JOIN pending_decisions p ON p.event_id = e.event_id
		WHERE e.type = 'ArcCloseFilingReviewed' AND e.ts >= ?
	`
	if project != "" {
		q += ` AND e.entity_project_id = ?`
		binds.AddString(project)
	}
	q += ` ORDER BY e.ts DESC`

	rs, err := pool.DB().QueryContext(ctx, q, binds.Slice()...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rs.Close() }()

	out := []ArcReviewAuditRow{}
	for rs.Next() {
		var (
			row         ArcReviewAuditRow
			projectID   sql.NullString
			payloadJSON string
			dispatched  sql.NullString
			dispatchSID sql.NullString
		)
		if err := rs.Scan(&row.ReviewID, &row.TS, &projectID, &payloadJSON, &dispatched, &dispatchSID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if projectID.Valid {
			row.ProjectID = projectID.String
		}
		if err := decodeReviewPayload(payloadJSON, &row); err != nil {
			return nil, fmt.Errorf("decode payload for %s: %w", row.ReviewID, err)
		}
		row.DispatchStatus = computeDispatchStatus(dispatched.Valid, row.AutoExecuteCount, row.SurfaceForConfirmCount)
		if dispatched.Valid {
			row.DispatchedAt = dispatched.String
		}
		if dispatchSID.Valid {
			row.DispatchSessionID = dispatchSID.String
		}
		row.UserCorrectionSignals = []CorrectionSignal{}
		out = append(out, row)
	}
	return out, nil
}

// decodeReviewPayload unpacks the ArcCloseFilingReviewed payload JSON
// into the audit row fields. The payload schema is fixed (events.ArcCloseFilingReviewedPayload);
// we read it via json.Unmarshal into a typed local rather than a
// map[string]any to keep the code lint-clean.
func decodeReviewPayload(raw string, row *ArcReviewAuditRow) error {
	var p struct {
		SessionID            string   `json:"session_id"`
		Triggers             []string `json:"triggers"`
		SnapshotTruncated    bool     `json:"snapshot_truncated"`
		SnapshotTokenCount   int      `json:"snapshot_token_count"`
		SnapshotMessageCount int      `json:"snapshot_message_count"`
		ArcSummary           *string  `json:"arc_summary,omitempty"`
		Decisions            []struct {
			Action     string  `json:"action"`
			Confidence float64 `json:"confidence"`
			Reasoning  string  `json:"reasoning,omitempty"`
		} `json:"decisions"`
		AutoExecuteCount       int   `json:"auto_execute_count"`
		SurfaceForConfirmCount int   `json:"surface_for_confirm_count"`
		SkipCount              int   `json:"skip_count"`
		LatencyMS              int64 `json:"latency_ms"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return err
	}
	row.SessionID = p.SessionID
	row.TriggerSignals = p.Triggers
	if row.TriggerSignals == nil {
		row.TriggerSignals = []string{}
	}
	row.SnapshotTokenCount = p.SnapshotTokenCount
	row.SnapshotMessageCount = p.SnapshotMessageCount
	row.AutoExecuteCount = p.AutoExecuteCount
	row.SurfaceForConfirmCount = p.SurfaceForConfirmCount
	row.SkipCount = p.SkipCount
	row.LatencyMS = p.LatencyMS
	if p.ArcSummary != nil {
		row.ArcSummary = *p.ArcSummary
	}
	for _, d := range p.Decisions {
		row.Decisions = append(row.Decisions, AuditDecisionRow{
			Action:     d.Action,
			Confidence: d.Confidence,
			Reasoning:  d.Reasoning,
		})
	}
	if row.Decisions == nil {
		row.Decisions = []AuditDecisionRow{}
	}
	return nil
}

// computeDispatchStatus derives the human-readable status string from
// the dispatch row + decision-count fields.
func computeDispatchStatus(haveDispatchRow bool, autoCount, surfaceCount int) string {
	switch {
	case haveDispatchRow:
		return "dispatched"
	case autoCount == 0 && surfaceCount == 0:
		return "skipped"
	default:
		// Harness-side fire path (Stop hook called review_arc_for_filing
		// directly + dispatched in-band): no pending_decisions row gets
		// created. From the audit's perspective the decisions did dispatch
		// (the hook's stdout system-reminder block) but we can't prove it
		// via a row — surface as "harness_in_band" so consumers can
		// distinguish from substrate-side dispatches.
		return "harness_in_band"
	}
}

// fetchCorrectionSignals scans for candidate-correction events within
// correction_window_hours of the review timestamp. Filtered by project
// + the correctionEventTypes closed set. Returns at most 50 signals
// per review (defensive cap; pathological cases — e.g. mass bug-edit
// sweeps — shouldn't pollute the output).
func fetchCorrectionSignals(ctx context.Context, pool *db.Pool, project, reviewTS string, windowHours int) ([]CorrectionSignal, error) {
	t, err := time.Parse(time.RFC3339Nano, reviewTS)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.000Z", reviewTS)
		if err != nil {
			return nil, fmt.Errorf("parse review ts %q: %w", reviewTS, err)
		}
	}
	windowEnd := t.Add(time.Duration(windowHours) * time.Hour).Format(time.RFC3339Nano)

	placeholders := make([]byte, 0, 2*len(correctionEventTypes))
	args := db.NewArgs()
	for i, typ := range correctionEventTypes {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args.AddString(typ)
	}
	q := fmt.Sprintf(`
		SELECT event_id, type, entity_kind, entity_slug, ts
		FROM events
		WHERE type IN (%s)
		  AND ts > ? AND ts <= ?
		  AND (? = '' OR entity_project_id = ?)
		ORDER BY ts ASC
		LIMIT 50
	`, placeholders)
	args.AddString(reviewTS).AddString(windowEnd).AddString(project).AddString(project)

	rs, err := pool.DB().QueryContext(ctx, q, args.Slice()...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rs.Close() }()

	var signals []CorrectionSignal
	for rs.Next() {
		var s CorrectionSignal
		if err := rs.Scan(&s.EventID, &s.EventType, &s.EntityKind, &s.EntitySlug, &s.TS); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if dt, err := time.Parse(time.RFC3339Nano, s.TS); err == nil {
			s.DeltaSec = int64(dt.Sub(t).Seconds())
		}
		signals = append(signals, s)
	}
	if signals == nil {
		signals = []CorrectionSignal{}
	}
	return signals, nil
}
