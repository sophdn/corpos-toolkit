package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/knowledge/curation"
)

// CurationListResult is the response for the curation_list MCP action.
type CurationListResult struct {
	Candidates []CurationCandidateSummary `json:"candidates"`
	Count      int                        `json:"count"`
}

// CurationCandidateSummary is the compact projection returned by
// curation_list — mirrors the bug_list compact projection shape so
// agents have one mental model across surfaces.
type CurationCandidateSummary struct {
	ID           int64    `json:"id"`
	ProjectID    string   `json:"project_id"`
	SourceType   string   `json:"source_type"`
	SourceRef    string   `json:"source_ref"`
	Question     string   `json:"question"`
	Origin       string   `json:"origin"`
	QualityScore *float64 `json:"quality_score,omitempty"`
	Status       string   `json:"status"`
}

// CurationReadResult is the full-body response for curation_read.
type CurationReadResult struct {
	Candidate CurationCandidateSummary `json:"candidate"`
	// Full body fields the summary omits — kept flat at the top level so
	// the JSON shape is one-deep, matching bug_read.
	InvokeWhen            string   `json:"invoke_when"`
	Description           string   `json:"description"`
	Tags                  []string `json:"tags"`
	OriginRef             *string  `json:"origin_ref,omitempty"`
	PromotedAutomatically bool     `json:"promoted_automatically"`
	CreatedAt             string   `json:"created_at"`
}

// CurationPromoteResult is the response for curation_promote.
type CurationPromoteResult struct {
	PointerID int64  `json:"pointer_id"`
	SourceRef string `json:"source_ref"`
	Status    string `json:"status"` // always "promoted"
}

// CurationRejectResult is the response for curation_reject.
type CurationRejectResult struct {
	OK bool `json:"ok"`
}

// CurationBulkActionResult is the response for curation_bulk_action.
// On dry-run, Counts reflects what would happen; Sample shows the
// affected source_refs so the caller can sanity-check before re-running
// without dry_run.
type CurationBulkActionResult struct {
	Action       string   `json:"action"`
	DryRun       bool     `json:"dry_run"`
	Matched      int      `json:"matched"`
	Succeeded    int      `json:"succeeded"`
	Failed       int      `json:"failed"`
	SampleRefs   []string `json:"sample_refs"`
	FailureNotes []string `json:"failure_notes,omitempty"`
}

// curation_list params.
type curationListParams struct {
	Origin       string `json:"origin,omitempty"`
	Status       string `json:"status,omitempty"`
	UnscoredOnly bool   `json:"scored,omitempty"` // "scored=false" semantically; field named for boolean clarity
	Limit        int    `json:"limit,omitempty"`
}

// HandleCurationList implements the curation_list MCP action.
//
// Filters: origin (optional), status (default 'pending'), unscored_only
// (optional bool — true means quality_score IS NULL). Limit defaults
// to 50. Returns the compact projection.
func HandleCurationList(ctx context.Context, deps Deps, project string, raw json.RawMessage) (CurationListResult, error) {
	var p curationListParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return CurationListResult{}, fmt.Errorf("curation_list: parse params: %w", err)
		}
	}
	if p.Status != "" && p.Status != "pending" {
		return CurationListResult{}, fmt.Errorf("curation_list: status filter currently only supports 'pending' (got %q)", p.Status)
	}

	cands, err := curation.ListPending(ctx, deps.Pool, curation.ListFilter{
		ProjectID:    project,
		Origin:       p.Origin,
		UnscoredOnly: p.UnscoredOnly,
		Limit:        p.Limit,
	})
	if err != nil {
		return CurationListResult{}, fmt.Errorf("curation_list: %w", err)
	}

	out := CurationListResult{
		Candidates: make([]CurationCandidateSummary, 0, len(cands)),
		Count:      len(cands),
	}
	for _, c := range cands {
		out.Candidates = append(out.Candidates, summarize(c))
	}
	return out, nil
}

// curation_read params.
type curationReadParams struct {
	ID int64 `json:"id"`
}

// HandleCurationRead implements the curation_read MCP action — returns
// the full candidate body.
func HandleCurationRead(ctx context.Context, deps Deps, project string, raw json.RawMessage) (CurationReadResult, error) {
	var p curationReadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return CurationReadResult{}, fmt.Errorf("curation_read: parse params: %w", err)
	}
	if p.ID == 0 {
		return CurationReadResult{}, errors.New("curation_read: id is required")
	}
	c, err := curation.ReadCandidate(ctx, deps.Pool, p.ID)
	if err != nil {
		return CurationReadResult{}, fmt.Errorf("curation_read: %w", err)
	}
	return CurationReadResult{
		Candidate:             summarize(c),
		InvokeWhen:            c.InvokeWhen,
		Description:           c.Description,
		Tags:                  c.Tags,
		OriginRef:             c.OriginRef,
		PromotedAutomatically: c.PromotedAutomatically,
		CreatedAt:             c.CreatedAt.Format("2006-01-02 15:04:05"),
	}, nil
}

// curation_promote params. The override_* fields let the caller refine
// metadata at promotion time — useful when reviewing a low-score
// candidate that has good content but a templated question.
type curationPromoteParams struct {
	ID                  int64  `json:"id"`
	OverrideQuestion    string `json:"override_question,omitempty"`
	OverrideInvokeWhen  string `json:"override_invoke_when,omitempty"`
	OverrideDescription string `json:"override_description,omitempty"`
}

// HandleCurationPromote implements the curation_promote MCP action.
func HandleCurationPromote(ctx context.Context, deps Deps, project string, raw json.RawMessage) (CurationPromoteResult, error) {
	var p curationPromoteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return CurationPromoteResult{}, fmt.Errorf("curation_promote: parse params: %w", err)
	}
	if p.ID == 0 {
		return CurationPromoteResult{}, errors.New("curation_promote: id is required")
	}

	// Load to capture pre-promote state for the event payload.
	cand, err := curation.ReadCandidate(ctx, deps.Pool, p.ID)
	if err != nil {
		return CurationPromoteResult{}, fmt.Errorf("curation_promote: %w", err)
	}
	if cand.Status != "pending" {
		return CurationPromoteResult{}, fmt.Errorf("curation_promote: candidate %d is not pending (status=%q)", p.ID, cand.Status)
	}

	// Apply overrides via UpdateCandidateScoring if any are set. This
	// path keeps the audit trail (the pre-promote score) intact.
	if p.OverrideQuestion != "" || p.OverrideInvokeWhen != "" || p.OverrideDescription != "" {
		meta := curation.ExtractedMeta{
			Question:    fallbackTo(p.OverrideQuestion, cand.Question),
			InvokeWhen:  fallbackTo(p.OverrideInvokeWhen, cand.InvokeWhen),
			Description: fallbackTo(p.OverrideDescription, cand.Description),
		}
		score := 0.0
		if cand.QualityScore != nil {
			score = *cand.QualityScore
		}
		if err := curation.UpdateCandidateScoring(ctx, deps.Pool, p.ID, meta, score); err != nil {
			return CurationPromoteResult{}, fmt.Errorf("curation_promote apply overrides: %w", err)
		}
		// Re-read post-override.
		cand, err = curation.ReadCandidate(ctx, deps.Pool, p.ID)
		if err != nil {
			return CurationPromoteResult{}, fmt.Errorf("curation_promote re-read: %w", err)
		}
	}

	pointerID, err := curation.PromoteCandidate(ctx, deps.Pool, p.ID, false)
	if err != nil {
		return CurationPromoteResult{}, fmt.Errorf("curation_promote: %w", err)
	}

	// Emit substrate event.
	err = deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("curation_candidate", fmt.Sprintf("%d", p.ID), cand.ProjectID),
			Payload: events.CurationCandidatePromotedPayload{
				CandidateID:           p.ID,
				PointerID:             pointerID,
				SourceType:            cand.SourceType,
				SourceRef:             cand.SourceRef,
				Origin:                cand.Origin,
				QualityScore:          cand.QualityScore,
				PromotedAutomatically: false,
			},
		})
		return err
	})
	if err != nil {
		// Promote already landed; event emission failed. Log via response
		// rather than rolling back — the durable substrate is the row,
		// not the event mirror.
		return CurationPromoteResult{
				PointerID: pointerID,
				SourceRef: cand.SourceRef,
				Status:    "promoted",
			}, fmt.Errorf("curation_promote: candidate promoted (pointer_id=%d) but event emission failed: %w",
				pointerID, err)
	}

	return CurationPromoteResult{
		PointerID: pointerID,
		SourceRef: cand.SourceRef,
		Status:    "promoted",
	}, nil
}

// curation_reject params. Reason is REQUIRED — empty rejected at both
// the handler boundary and curation.RejectCandidate.
type curationRejectParams struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// HandleCurationReject implements the curation_reject MCP action.
func HandleCurationReject(ctx context.Context, deps Deps, project string, raw json.RawMessage) (CurationRejectResult, error) {
	var p curationRejectParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return CurationRejectResult{}, fmt.Errorf("curation_reject: parse params: %w", err)
	}
	if p.ID == 0 {
		return CurationRejectResult{}, errors.New("curation_reject: id is required")
	}
	if p.Reason == "" {
		return CurationRejectResult{}, errors.New("curation_reject: reason is required and must be non-empty")
	}

	// Load for event payload.
	cand, err := curation.ReadCandidate(ctx, deps.Pool, p.ID)
	if err != nil {
		return CurationRejectResult{}, fmt.Errorf("curation_reject: %w", err)
	}

	if err := curation.RejectCandidate(ctx, deps.Pool, p.ID, p.Reason); err != nil {
		return CurationRejectResult{}, fmt.Errorf("curation_reject: %w", err)
	}

	err = deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("curation_candidate", fmt.Sprintf("%d", p.ID), cand.ProjectID),
			Payload: events.CurationCandidateRejectedPayload{
				CandidateID: p.ID,
				SourceType:  cand.SourceType,
				SourceRef:   cand.SourceRef,
				Origin:      cand.Origin,
				Reason:      p.Reason,
			},
		})
		return err
	})
	if err != nil {
		return CurationRejectResult{OK: true},
			fmt.Errorf("curation_reject: rejected but event emission failed: %w", err)
	}
	return CurationRejectResult{OK: true}, nil
}

// curation_bulk_action params. Filter must be non-empty (no accidental
// table-nukes); action ∈ {promote, reject}; reason required if reject.
type curationBulkActionParams struct {
	Filter struct {
		Origin       string `json:"origin,omitempty"`
		UnscoredOnly bool   `json:"unscored_only,omitempty"`
	} `json:"filter"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	DryRun bool   `json:"dry_run,omitempty"`
	Limit  int    `json:"limit,omitempty"` // safety cap; default 100
}

// HandleCurationBulkAction implements the curation_bulk_action MCP action.
func HandleCurationBulkAction(ctx context.Context, deps Deps, project string, raw json.RawMessage) (CurationBulkActionResult, error) {
	var p curationBulkActionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return CurationBulkActionResult{}, fmt.Errorf("curation_bulk_action: parse params: %w", err)
	}
	if p.Action != "promote" && p.Action != "reject" {
		return CurationBulkActionResult{}, fmt.Errorf("curation_bulk_action: action must be 'promote' or 'reject' (got %q)", p.Action)
	}
	if p.Action == "reject" && p.Reason == "" {
		return CurationBulkActionResult{}, errors.New("curation_bulk_action: reason required when action='reject'")
	}
	// Non-empty filter guard: project alone counts (the project arg from
	// dispatch). But if action='reject' over a whole project with no
	// origin / unscored filter, that's too broad — require an explicit
	// scoping field beyond project.
	if p.Filter.Origin == "" && !p.Filter.UnscoredOnly {
		return CurationBulkActionResult{}, errors.New("curation_bulk_action: filter must include at least one of {origin, unscored_only} (no whole-project bulk actions)")
	}

	limit := p.Limit
	if limit <= 0 {
		limit = 100
	}

	cands, err := curation.ListPending(ctx, deps.Pool, curation.ListFilter{
		ProjectID:    project,
		Origin:       p.Filter.Origin,
		UnscoredOnly: p.Filter.UnscoredOnly,
		Limit:        limit,
	})
	if err != nil {
		return CurationBulkActionResult{}, fmt.Errorf("curation_bulk_action list: %w", err)
	}

	out := CurationBulkActionResult{
		Action:  p.Action,
		DryRun:  p.DryRun,
		Matched: len(cands),
	}

	const sampleCap = 5
	for i, c := range cands {
		if i < sampleCap {
			out.SampleRefs = append(out.SampleRefs, c.SourceRef)
		}
		if p.DryRun {
			continue
		}
		err := bulkApply(ctx, deps, c, p.Action, p.Reason)
		if err != nil {
			out.Failed++
			out.FailureNotes = append(out.FailureNotes,
				fmt.Sprintf("id=%d: %v", c.ID, err))
			continue
		}
		out.Succeeded++
	}
	return out, nil
}

// bulkApply runs one promote/reject + event emit. Errors from either
// step (DB or event) bubble up; the bulk loop counts them.
func bulkApply(ctx context.Context, deps Deps, c curation.Candidate, action, reason string) error {
	switch action {
	case "promote":
		pointerID, err := curation.PromoteCandidate(ctx, deps.Pool, c.ID, false)
		if err != nil {
			return err
		}
		return deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
			_, err := events.Emit(ctx, tx, events.EmitArgs{
				Entity: events.NewEntityRef("curation_candidate", fmt.Sprintf("%d", c.ID), c.ProjectID),
				Payload: events.CurationCandidatePromotedPayload{
					CandidateID:           c.ID,
					PointerID:             pointerID,
					SourceType:            c.SourceType,
					SourceRef:             c.SourceRef,
					Origin:                c.Origin,
					QualityScore:          c.QualityScore,
					PromotedAutomatically: false,
				},
			})
			return err
		})
	case "reject":
		if err := curation.RejectCandidate(ctx, deps.Pool, c.ID, reason); err != nil {
			return err
		}
		return deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
			_, err := events.Emit(ctx, tx, events.EmitArgs{
				Entity: events.NewEntityRef("curation_candidate", fmt.Sprintf("%d", c.ID), c.ProjectID),
				Payload: events.CurationCandidateRejectedPayload{
					CandidateID: c.ID,
					SourceType:  c.SourceType,
					SourceRef:   c.SourceRef,
					Origin:      c.Origin,
					Reason:      reason,
				},
			})
			return err
		})
	default:
		return fmt.Errorf("bulkApply: unknown action %q", action)
	}
}

func summarize(c curation.Candidate) CurationCandidateSummary {
	return CurationCandidateSummary{
		ID:           c.ID,
		ProjectID:    c.ProjectID,
		SourceType:   c.SourceType,
		SourceRef:    c.SourceRef,
		Question:     c.Question,
		Origin:       c.Origin,
		QualityScore: c.QualityScore,
		Status:       c.Status,
	}
}

func fallbackTo(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

// Compile-time check: db is unused in this file currently, but the
// types reference it via Deps.Pool. Avoid an "imported and not used"
// goof if Deps signature ever loses Pool.
var _ = (*db.Pool)(nil)
