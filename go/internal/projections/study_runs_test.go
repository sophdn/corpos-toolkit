package projections_test

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/testutil"
)

// study_runs_test.go exercises the study-run projection: the emit→fold→read
// path (which also proves the literal-filter contract — the writer emits the
// exact entity_kind='study_run' the fold filters on) and the rebuild-from-
// empty byte-identical parity.

// emitStudyRun emits one StudyRunRecorded event through the production fold
// hook (installed by installProjectionsFoldHook) so the projection tables
// populate in the same tx.
func emitStudyRun(t *testing.T, pool *db.Pool, p events.StudyRunRecordedPayload) {
	t.Helper()
	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  events.NewCrossCuttingEntityRef("study_run", p.RunID),
			Payload: p,
		})
		return err
	}); err != nil {
		t.Fatalf("emit study run: %v", err)
	}
}

func sampleStudyRun(runID, project string) events.StudyRunRecordedPayload {
	return events.StudyRunRecordedPayload{
		RunID:           runID,
		ProjectID:       project,
		Name:            "casg-direct-v3-smoke",
		Assay:           "grounded-glyph-probe",
		ItemID:          "casg-direct",
		Image:           "localhost/lab-grounded-glyph-probe:dev",
		ImageDigest:     "sha256:deadbeef",
		Status:          "completed",
		StudyDigest:     "sha256-hex-study",
		MaterialsHashes: map[string]string{"scenario.md": "aaa", "glyph.md": "bbb"},
		ModelID:         "Qwen2.5-32B-Instruct-Q4_K_M.gguf",
		ModelVersion:    "q4km",
		ResponsesDir:    "/abs/out/responses",
		RunAt:           "2026-07-09T00:33:10Z",
		Rows: []events.StudyRunScoreRow{
			{Item: "casg-direct", Condition: "baseline", Run: 1, VerdictKind: "fail", VerdictReason: "r1", Rationale: "grounded-glyph-probe:baseline"},
			{Item: "casg-direct", Condition: "glyph_only", Run: 1, VerdictKind: "pass", VerdictReason: "", Rationale: "grounded-glyph-probe:glyph_only"},
		},
	}
}

// TestStudyRuns_EmitFoldRead is the literal-filter proof: a writer emits a
// StudyRunRecorded event on entity_kind='study_run', and the fold populates
// proj_study_runs (parent) + proj_study_run_scores (child) with the exact
// literal the projection filters on.
func TestStudyRuns_EmitFoldRead(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "corpos-lab")
	installProjectionsFoldHook(t)

	emitStudyRun(t, pool, sampleStudyRun("run-1", "corpos-lab"))

	if got := tableCount(t, pool, "proj_study_runs"); got != 1 {
		t.Fatalf("proj_study_runs rows = %d, want 1", got)
	}
	if got := tableCount(t, pool, "proj_study_run_scores"); got != 2 {
		t.Fatalf("proj_study_run_scores rows = %d, want 2", got)
	}

	// Literal proof: the parent row carries the payload fields, and the child
	// rows are joined by run_id.
	var name, assay, status, runAt, materials string
	if err := pool.DB().QueryRow(
		`SELECT name, assay, status, run_at, materials_hash_json FROM proj_study_runs WHERE id = ?`,
		"run-1").Scan(&name, &assay, &status, &runAt, &materials); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if name != "casg-direct-v3-smoke" || assay != "grounded-glyph-probe" || status != "completed" {
		t.Errorf("parent fields wrong: name=%q assay=%q status=%q", name, assay, status)
	}
	if runAt != "2026-07-09T00:33:10Z" {
		t.Errorf("run_at = %q", runAt)
	}
	if materials == "" || materials == "null" {
		t.Errorf("materials_hash_json not stored as object: %q", materials)
	}

	var passVerdict, failVerdict int
	if err := pool.DB().QueryRow(
		`SELECT
		    SUM(CASE WHEN verdict_kind='pass' THEN 1 ELSE 0 END),
		    SUM(CASE WHEN verdict_kind='fail' THEN 1 ELSE 0 END)
		 FROM proj_study_run_scores WHERE run_id = ? AND project_id = ?`,
		"run-1", "corpos-lab").Scan(&passVerdict, &failVerdict); err != nil {
		t.Fatalf("read scores: %v", err)
	}
	if passVerdict != 1 || failVerdict != 1 {
		t.Errorf("verdict counts wrong: pass=%d fail=%d", passVerdict, failVerdict)
	}
}

// TestStudyRuns_FoldIsIdempotent proves re-folding the same run (a second
// StudyRunRecorded for the same run_id) upserts the parent and replaces the
// child grid rather than duplicating.
func TestStudyRuns_FoldIsIdempotent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "corpos-lab")
	installProjectionsFoldHook(t)

	emitStudyRun(t, pool, sampleStudyRun("run-1", "corpos-lab"))
	// Second emit for the same run_id with an updated status + a single row.
	p := sampleStudyRun("run-1", "corpos-lab")
	p.Status = "failed"
	p.Error = "boom"
	p.Rows = []events.StudyRunScoreRow{{Item: "casg-direct", Condition: "baseline", Run: 2, VerdictKind: "fail", VerdictReason: "again", Rationale: "retry"}}
	emitStudyRun(t, pool, p)

	if got := tableCount(t, pool, "proj_study_runs"); got != 1 {
		t.Fatalf("proj_study_runs rows = %d, want 1 (upsert)", got)
	}
	if got := tableCount(t, pool, "proj_study_run_scores"); got != 1 {
		t.Fatalf("proj_study_run_scores rows = %d, want 1 (grid replaced)", got)
	}
	var status, errMsg string
	if err := pool.DB().QueryRow(`SELECT status, error FROM proj_study_runs WHERE id = ?`, "run-1").
		Scan(&status, &errMsg); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if status != "failed" || errMsg != "boom" {
		t.Errorf("upsert did not update: status=%q error=%q", status, errMsg)
	}
}

// TestStudyRuns_RebuildFromEmpty seeds two runs, checksums the projection
// pair, TRUNCATEs + RebuildFromEmpty, and asserts byte-identical convergence
// (excluding the volatile watermark columns, per tableChecksum).
func TestStudyRuns_RebuildFromEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "corpos-lab")
	installProjectionsFoldHook(t)

	emitStudyRun(t, pool, sampleStudyRun("run-1", "corpos-lab"))
	r2 := sampleStudyRun("run-2", "corpos-lab")
	r2.Name = "casg-direct-v4"
	r2.RunAt = "2026-07-09T01:00:00Z"
	emitStudyRun(t, pool, r2)

	referenceParent := tableChecksum(t, pool, "proj_study_runs")
	referenceScores := tableChecksum(t, pool, "proj_study_run_scores")

	mustRebuild(t, pool, []string{"study_runs"})
	afterParent := tableChecksum(t, pool, "proj_study_runs")
	afterScores := tableChecksum(t, pool, "proj_study_run_scores")

	if referenceParent != afterParent {
		t.Errorf("proj_study_runs checksum drift: ref=%s after=%s", referenceParent, afterParent)
	}
	if referenceScores != afterScores {
		t.Errorf("proj_study_run_scores checksum drift: ref=%s after=%s", referenceScores, afterScores)
	}
	if got := tableCount(t, pool, "proj_study_runs"); got != 2 {
		t.Errorf("proj_study_runs rows = %d, want 2 after rebuild", got)
	}
	if got := tableCount(t, pool, "proj_study_run_scores"); got != 4 {
		t.Errorf("proj_study_run_scores rows = %d, want 4 after rebuild", got)
	}
}
