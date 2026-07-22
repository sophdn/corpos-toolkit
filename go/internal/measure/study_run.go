package measure

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/eventbus"
	"toolkit/internal/events"
)

// study_run.go implements the study_run_record measure action — the persist
// endpoint corpos-lab POSTs a behavioral-assay study run to. It mirrors
// benchmark.go's insertBenchmarkRow shape: a single events.Emit of a typed
// StudyRunRecorded payload inside pool.WithWrite, with the fold fanning out
// to proj_study_runs + proj_study_run_scores in the same tx. See
// go/internal/db/migrations/088_study_runs.sql and
// blueprints/events/StudyRunRecorded.json for the design.

// StudyRunDeps holds dependencies for the study_run_record handler. Separate
// from BenchmarkDeps because it also needs the SSE bus: unlike benchmark_record
// (which has no live dashboard-refresh requirement), the study-run dashboard
// updates live over /events, so the handler publishes an artifact_created
// event after the write commits. Bus may be nil (tests, no-HTTP mode) — the
// publish is then skipped.
type StudyRunDeps struct {
	Pool *db.Pool
	Bus  *eventbus.Bus
}

// StudyRunRecordResult is the response shape for study_run_record. On
// success: RunID + Status populated. On param error: Error populated.
// omitempty keeps successful and error envelopes minimal.
type StudyRunRecordResult struct {
	RunID  string `json:"run_id,omitempty"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// studyRunVerdictInput is the controller's nested verdict object. The handler
// FLATTENS it to verdict_kind / verdict_reason on the persisted score row.
type studyRunVerdictInput struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// studyRunRowInput accepts BOTH the flattened shape (verdict_kind /
// verdict_reason) and the controller's nested {verdict:{kind,reason}} shape;
// flattenRow reconciles them.
type studyRunRowInput struct {
	Item          string                `json:"item"`
	Condition     string                `json:"condition"`
	Run           int                   `json:"run"`
	VerdictKind   string                `json:"verdict_kind"`
	VerdictReason string                `json:"verdict_reason"`
	Verdict       *studyRunVerdictInput `json:"verdict"`
	Rationale     string                `json:"rationale"`
}

// studyRunInput mirrors the record shape corpos-lab sends as the action params.
type studyRunInput struct {
	RunID           string             `json:"run_id"`
	Name            string             `json:"name"`
	Assay           string             `json:"assay"`
	ItemID          string             `json:"item_id"`
	Image           string             `json:"image"`
	ImageDigest     string             `json:"image_digest"`
	Status          string             `json:"status"`
	Error           string             `json:"error"`
	StudyDigest     string             `json:"study_digest"`
	MaterialsHashes map[string]string  `json:"materials_hashes"`
	ModelID         string             `json:"model_id"`
	ModelVersion    string             `json:"model_version"`
	ResponsesDir    string             `json:"responses_dir"`
	RunAt           string             `json:"run_at"`
	Rows            []studyRunRowInput `json:"rows"`
}

// HandleStudyRunRecord implements the study_run_record action. It parses the
// posted run record, flattens the score-grid verdicts, emits one
// StudyRunRecorded event (whose fold writes the parent + child projection
// rows in the same tx), and — once the write commits — publishes an
// artifact_created SSE event so the dashboard refreshes live. project comes
// from the dispatch envelope, NOT params.
func HandleStudyRunRecord(ctx context.Context, deps StudyRunDeps, project string, params json.RawMessage) (StudyRunRecordResult, error) {
	if project == "" {
		return StudyRunRecordResult{Error: "project is required"}, nil
	}
	in, parseErr := parseStudyRunInput(params)
	if parseErr != "" {
		return StudyRunRecordResult{Error: parseErr}, nil
	}

	runID := in.RunID
	if runID == "" {
		runID = newUUIDv4()
	}

	payload := events.StudyRunRecordedPayload{
		RunID:           runID,
		ProjectID:       project,
		Name:            in.Name,
		Assay:           in.Assay,
		ItemID:          in.ItemID,
		Image:           in.Image,
		ImageDigest:     in.ImageDigest,
		Status:          in.Status,
		Error:           in.Error,
		StudyDigest:     in.StudyDigest,
		MaterialsHashes: in.MaterialsHashes,
		ModelID:         in.ModelID,
		ModelVersion:    in.ModelVersion,
		ResponsesDir:    in.ResponsesDir,
		RunAt:           in.RunAt,
		Rows:            flattenRows(in.Rows),
	}

	if err := deps.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, emitErr := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("study_run", runID),
			Payload: payload,
		})
		return emitErr
	}); err != nil {
		return StudyRunRecordResult{}, fmt.Errorf("record study run: %w", err)
	}

	// The write committed. Publish an artifact_created event so the observe
	// SSE stream (/events) wakes the dashboard. The measure surface has no
	// construct AfterCreate notifier (that path is forge/construct-only), so
	// this is the deliberate minimal SSE hook for the direct measure action.
	// Nil bus (tests / no-HTTP boot) → no-op.
	if deps.Bus != nil {
		deps.Bus.Publish(eventbus.ArtifactCreated(project, "study-run", runID))
	}

	return StudyRunRecordResult{RunID: runID, Status: in.Status}, nil
}

// flattenRows resolves each row's verdict to the flat verdict_kind /
// verdict_reason shape: an explicit flat value wins; otherwise the nested
// {verdict:{kind,reason}} is lifted. Always returns a non-nil slice so the
// payload's `rows` field serialises as [] (the schema requires the array).
func flattenRows(in []studyRunRowInput) []events.StudyRunScoreRow {
	out := make([]events.StudyRunScoreRow, 0, len(in))
	for _, r := range in {
		kind, reason := r.VerdictKind, r.VerdictReason
		if kind == "" && r.Verdict != nil {
			kind = r.Verdict.Kind
		}
		if reason == "" && r.Verdict != nil {
			reason = r.Verdict.Reason
		}
		out = append(out, events.StudyRunScoreRow{
			Item:          r.Item,
			Condition:     r.Condition,
			Run:           r.Run,
			VerdictKind:   kind,
			VerdictReason: reason,
			Rationale:     r.Rationale,
		})
	}
	return out
}

// parseStudyRunInput unmarshals the params and collects every missing
// required key into one error (name, assay, status, run_at). status is
// pinned to the schema enum here so the caller gets a structured param error
// rather than a raw envelope-validation failure at emit time. Returns
// parseErr=="" on success.
func parseStudyRunInput(params json.RawMessage) (studyRunInput, string) {
	var in studyRunInput
	if len(params) > 0 {
		if err := json.Unmarshal(params, &in); err != nil {
			return studyRunInput{}, fmt.Sprintf("params: %s", err.Error())
		}
	}
	var missing []string
	if in.Name == "" {
		missing = append(missing, "params.name")
	}
	if in.Assay == "" {
		missing = append(missing, "params.assay")
	}
	if in.Status == "" {
		missing = append(missing, "params.status")
	}
	if in.RunAt == "" {
		missing = append(missing, "params.run_at")
	}
	if len(missing) > 0 {
		return studyRunInput{}, "missing required params: " + strings.Join(missing, ", ")
	}
	if in.Status != "completed" && in.Status != "failed" {
		return studyRunInput{}, fmt.Sprintf("invalid status %q: must be \"completed\" or \"failed\"", in.Status)
	}
	return in, ""
}
