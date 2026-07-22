package measure_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/measure"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
)

// study_run_test.go drives the study_run_record action end-to-end: the
// handler emits a StudyRunRecorded event, the fold populates the projection
// pair, and the result is queryable. Exercises BOTH the flat verdict shape
// and the controller's nested {verdict:{kind,reason}} shape (the handler
// flattens the latter).

func newStudyRunDeps(t *testing.T) measure.StudyRunDeps {
	t.Helper()
	pool := testutil.NewTestDB(t)
	seedProjectRow(t, pool, "corpos-lab")
	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		return projections.FoldAll(ctx, tx, projections.RawEvent{
			EventID:         evt.EventID,
			Ts:              evt.Ts,
			ActorKind:       evt.ActorKind,
			ActorID:         evt.ActorID,
			Type:            evt.Type,
			EntityKind:      evt.EntityKind,
			EntitySlug:      evt.EntitySlug,
			EntityProjectID: evt.EntityProjectID,
			Payload:         evt.Payload,
			Rationale:       evt.Rationale,
			CausedByEventID: evt.CausedByEventID,
			RelatedEntities: evt.RelatedEntities,
			SpanID:          evt.SpanID,
			SchemaVersion:   evt.SchemaVersion,
		})
	})
	t.Cleanup(func() { events.SetFoldHook(nil) })
	// Bus nil — the SSE publish is a post-commit no-op in tests.
	return measure.StudyRunDeps{Pool: pool}
}

func seedProjectRow(t *testing.T, pool *db.Pool, id string) {
	t.Helper()
	if _, err := pool.DB().Exec(`INSERT OR IGNORE INTO projects (id, name) VALUES (?, ?)`, id, id); err != nil {
		t.Fatal(err)
	}
}

// The exact record shape corpos-lab POSTs (nested verdict object on the rows).
const studyRunRecordJSON = `{
  "name": "casg-direct-v3-smoke",
  "assay": "grounded-glyph-probe",
  "item_id": "casg-direct",
  "image": "localhost/lab-grounded-glyph-probe:dev",
  "image_digest": "sha256:abc",
  "status": "completed",
  "error": "",
  "study_digest": "sha256-hex-study",
  "materials_hashes": {"scenario.md":"h1","glyph.md":"h2","ground.md":"h3"},
  "model_id": "Qwen2.5-32B-Instruct-Q4_K_M.gguf",
  "model_version": "q4km",
  "responses_dir": "/abs/path/to/out/responses",
  "run_at": "2026-07-09T00:33:10Z",
  "rows": [
    {"item":"casg-direct","condition":"baseline","run":1,"verdict":{"kind":"fail","reason":"weak"},"rationale":"baseline:response=2249chars"},
    {"item":"casg-direct","condition":"glyph_only","run":1,"verdict":{"kind":"pass","reason":""},"rationale":"glyph"}
  ]
}`

func TestHandleStudyRunRecord_PersistsAndQueryable(t *testing.T) {
	deps := newStudyRunDeps(t)
	ctx := context.Background()

	res, err := measure.HandleStudyRunRecord(ctx, deps, "corpos-lab", json.RawMessage(studyRunRecordJSON))
	if err != nil {
		t.Fatalf("HandleStudyRunRecord: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error result: %q", res.Error)
	}
	if res.RunID == "" {
		t.Fatal("expected a generated run_id")
	}
	if res.Status != "completed" {
		t.Errorf("status = %q, want completed", res.Status)
	}

	// Parent row persisted + queryable.
	var name, assay, model string
	if err := deps.Pool.DB().QueryRow(
		`SELECT name, assay, model_id FROM proj_study_runs WHERE id = ?`, res.RunID).
		Scan(&name, &assay, &model); err != nil {
		t.Fatalf("query parent: %v", err)
	}
	if name != "casg-direct-v3-smoke" || assay != "grounded-glyph-probe" ||
		model != "Qwen2.5-32B-Instruct-Q4_K_M.gguf" {
		t.Errorf("parent fields wrong: name=%q assay=%q model=%q", name, assay, model)
	}

	// Nested verdicts flattened into the child grid.
	var total, pass, fail int
	if err := deps.Pool.DB().QueryRow(
		`SELECT COUNT(*),
		        SUM(CASE WHEN verdict_kind='pass' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN verdict_kind='fail' THEN 1 ELSE 0 END)
		 FROM proj_study_run_scores WHERE run_id = ?`, res.RunID).
		Scan(&total, &pass, &fail); err != nil {
		t.Fatalf("query scores: %v", err)
	}
	if total != 2 || pass != 1 || fail != 1 {
		t.Errorf("score grid wrong: total=%d pass=%d fail=%d", total, pass, fail)
	}
	// The flattened verdict_reason came from the nested object.
	var reason string
	if err := deps.Pool.DB().QueryRow(
		`SELECT verdict_reason FROM proj_study_run_scores WHERE run_id = ? AND condition = 'baseline'`,
		res.RunID).Scan(&reason); err != nil {
		t.Fatalf("query reason: %v", err)
	}
	if reason != "weak" {
		t.Errorf("nested verdict reason not flattened: %q", reason)
	}
}

func TestHandleStudyRunRecord_RespectsSuppliedRunID(t *testing.T) {
	deps := newStudyRunDeps(t)
	params := `{"run_id":"fixed-1","name":"n","assay":"a","status":"completed","run_at":"2026-07-09T00:00:00Z","rows":[]}`
	res, err := measure.HandleStudyRunRecord(context.Background(), deps, "corpos-lab", json.RawMessage(params))
	if err != nil {
		t.Fatalf("HandleStudyRunRecord: %v", err)
	}
	if res.RunID != "fixed-1" {
		t.Errorf("run_id = %q, want fixed-1", res.RunID)
	}
}

func TestHandleStudyRunRecord_MissingRequiredParams(t *testing.T) {
	deps := newStudyRunDeps(t)
	res, err := measure.HandleStudyRunRecord(context.Background(), deps, "corpos-lab", json.RawMessage(`{"name":"n"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Error == "" {
		t.Fatal("expected a param error for missing assay/status/run_at")
	}
}

func TestHandleStudyRunRecord_RejectsBadStatus(t *testing.T) {
	deps := newStudyRunDeps(t)
	params := `{"name":"n","assay":"a","status":"weird","run_at":"2026-07-09T00:00:00Z"}`
	res, err := measure.HandleStudyRunRecord(context.Background(), deps, "corpos-lab", json.RawMessage(params))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Error == "" {
		t.Fatal("expected a param error for invalid status")
	}
}

func TestHandleStudyRunRecord_MissingProject(t *testing.T) {
	deps := newStudyRunDeps(t)
	res, err := measure.HandleStudyRunRecord(context.Background(), deps, "", json.RawMessage(studyRunRecordJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Error != "project is required" {
		t.Errorf("error = %q, want 'project is required'", res.Error)
	}
}
