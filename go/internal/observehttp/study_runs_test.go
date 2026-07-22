package observehttp

import (
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// study_runs_test.go exercises the observe read endpoints against a seeded
// projection pair (direct INSERTs into proj_study_runs + proj_study_run_scores,
// mirroring the benchmarks_test fixture style — the fold path is covered in the
// projections package).

func seedStudyRun(t *testing.T, pool *db.Pool, id, project, assay, modelID, status, runAt string) {
	t.Helper()
	seedProject(t, pool, project)
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_study_runs
		   (id, project_id, name, assay, item_id, image_ref, image_digest,
		    study_digest, materials_hash_json, model_id, model_version,
		    status, error, responses_dir, run_at)
		 VALUES (?, ?, ?, ?, 'casg-direct', 'img:dev', 'sha256:x', 'sha256-study',
		         '{"scenario.md":"aaa"}', ?, 'q4km', ?, '', '/out/responses', ?)`,
		id, project, "run-"+id, assay, modelID, status, runAt,
	); err != nil {
		t.Fatal(err)
	}
}

func seedStudyScore(t *testing.T, pool *db.Pool, runID, project, condition string, runIdx int, verdictKind string) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_study_run_scores
		   (run_id, project_id, condition, run_idx, verdict_kind, verdict_reason, item, rationale)
		 VALUES (?, ?, ?, ?, ?, 'reason', 'casg-direct', 'rat')`,
		runID, project, condition, runIdx, verdictKind,
	); err != nil {
		t.Fatal(err)
	}
}

func TestStudyRunsList_FiltersAndOrder(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedStudyRun(t, pool, "r1", "corpos-lab", "grounded-glyph-probe", "qwen", "completed", "2026-07-09T00:00:00Z")
	seedStudyRun(t, pool, "r2", "corpos-lab", "grounded-glyph-probe", "qwen", "failed", "2026-07-09T02:00:00Z")
	seedStudyRun(t, pool, "r3", "corpos-lab", "other-assay", "qwen", "completed", "2026-07-09T01:00:00Z")

	srv := newTestServer(t, pool)

	// Filter by assay → 2 rows, ordered run_at DESC (r2 before r1).
	var got []studyRunRow
	getJSON(t, srv, "/study-runs?assay=grounded-glyph-probe", &got)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].ID != "r2" || got[1].ID != "r1" {
		t.Errorf("order wrong: %s, %s (want r2, r1)", got[0].ID, got[1].ID)
	}
	// materials_hashes re-hydrated as a map.
	if got[0].MaterialsHashes["scenario.md"] != "aaa" {
		t.Errorf("materials_hashes not hydrated: %+v", got[0].MaterialsHashes)
	}

	// Filter by status.
	var failed []studyRunRow
	getJSON(t, srv, "/study-runs?status=failed", &failed)
	if len(failed) != 1 || failed[0].ID != "r2" {
		t.Errorf("status filter wrong: %+v", failed)
	}

	// Filter by run_id.
	var byRun []studyRunRow
	getJSON(t, srv, "/study-runs?run_id=r3", &byRun)
	if len(byRun) != 1 || byRun[0].Assay != "other-assay" {
		t.Errorf("run_id filter wrong: %+v", byRun)
	}

	// since (RFC 3339 lower bound) → only r2 (>= 02:00) ... and r3 excluded.
	var recent []studyRunRow
	getJSON(t, srv, "/study-runs?since=2026-07-09T01:30:00Z", &recent)
	if len(recent) != 1 || recent[0].ID != "r2" {
		t.Errorf("since filter wrong: %+v", recent)
	}
}

func TestStudyRunDetail_ParentPlusGrid(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedStudyRun(t, pool, "r1", "corpos-lab", "grounded-glyph-probe", "qwen", "completed", "2026-07-09T00:00:00Z")
	seedStudyScore(t, pool, "r1", "corpos-lab", "baseline", 1, "fail")
	seedStudyScore(t, pool, "r1", "corpos-lab", "glyph_only", 1, "pass")

	srv := newTestServer(t, pool)
	var got studyRunDetailResponse
	getJSON(t, srv, "/study-runs/r1", &got)

	if got.ID != "r1" || got.Assay != "grounded-glyph-probe" {
		t.Errorf("parent wrong: %+v", got.studyRunRow)
	}
	if len(got.Scores) != 2 {
		t.Fatalf("got %d scores, want 2", len(got.Scores))
	}
	// Ordered by condition — baseline before glyph_only.
	if got.Scores[0].Condition != "baseline" || got.Scores[1].Condition != "glyph_only" {
		t.Errorf("score order wrong: %+v", got.Scores)
	}
	if got.Scores[0].VerdictKind != "fail" || got.Scores[1].VerdictKind != "pass" {
		t.Errorf("verdicts wrong: %+v", got.Scores)
	}
}

func TestStudyRunDetail_NotFound(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "corpos-lab")
	srv := newTestServer(t, pool)
	if code := getJSON(t, srv, "/study-runs/nope", nil); code != 404 {
		t.Errorf("missing run status = %d, want 404", code)
	}
}
