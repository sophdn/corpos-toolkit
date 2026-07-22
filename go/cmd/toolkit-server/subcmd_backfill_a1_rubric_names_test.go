package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newBackfillA1TestDB returns an open *sql.DB with the
// proj_benchmark_results schema applied. Schema mirrors the live table
// (data/toolkit.db's proj_benchmark_results) — the columns that matter
// for the backfill are id (PK), run_id, task_id, run_at; the rest are
// filled with minimal valid values so the row inserts succeed.
func newBackfillA1TestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "toolkit.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE proj_benchmark_results (
			id                      TEXT    NOT NULL,
			project_id              TEXT    NOT NULL,
			scenario_id             TEXT    NOT NULL,
			tool_name               TEXT    NOT NULL,
			model_name              TEXT    NOT NULL,
			run_id                  TEXT,
			run_at                  INTEGER NOT NULL,
			wall_clock_ms           INTEGER NOT NULL,
			invocation_ok           INTEGER NOT NULL,
			invoked_contextually    INTEGER NOT NULL DEFAULT 1,
			task_id                 TEXT
		)
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// seedBackfillA1Rows inserts n rows for the given run_id with
// task_id=NULL, simulating the pre-backfill state.
func seedBackfillA1Rows(t *testing.T, db *sql.DB, runID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := db.Exec(`
			INSERT INTO proj_benchmark_results
			(id, project_id, scenario_id, tool_name, model_name, run_id, run_at, wall_clock_ms, invocation_ok)
			VALUES (?, 'p', 's', 'tool', 'model', ?, 0, 0, 1)
		`, runID+"-"+string(rune('a'+i)), runID)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// TestRunBackfillA1_TagsNullTaskIDs verifies the core behavior: rows
// matching the (run_id, task_id IS NULL) predicate get tagged. Mirrors
// the Rust binary's main loop without the print harness.
func TestRunBackfillA1_TagsNullTaskIDs(t *testing.T) {
	db := newBackfillA1TestDB(t)
	seedBackfillA1Rows(t, db, "9b88b3e8-e3f2-4776-b783-abef875061b8", 3)
	seedBackfillA1Rows(t, db, "fae1d6a4-bc1a-40fb-8909-49b5074d9f2a", 2)

	mappings := []rubricMapping{
		{"9b88b3e8-e3f2-4776-b783-abef875061b8", "artifact-review"},
		{"fae1d6a4-bc1a-40fb-8909-49b5074d9f2a", "agentic-audit"},
	}

	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()

	if err := runBackfillA1(context.Background(), db, mappings, devnull); err != nil {
		t.Fatalf("runBackfillA1: %v", err)
	}

	var artifactCount, agenticCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM proj_benchmark_results WHERE task_id = 'artifact-review'`).Scan(&artifactCount); err != nil {
		t.Fatalf("count artifact-review: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM proj_benchmark_results WHERE task_id = 'agentic-audit'`).Scan(&agenticCount); err != nil {
		t.Fatalf("count agentic-audit: %v", err)
	}
	if artifactCount != 3 {
		t.Errorf("artifact-review rows: want 3, got %d", artifactCount)
	}
	if agenticCount != 2 {
		t.Errorf("agentic-audit rows: want 2, got %d", agenticCount)
	}
}

// TestRunBackfillA1_Idempotent verifies the canonical idempotency
// invariant from the Rust binary's docstring ("Re-running is a no-op
// once the rows are tagged"): running the backfill twice produces the
// same end state as running it once.
func TestRunBackfillA1_Idempotent(t *testing.T) {
	db := newBackfillA1TestDB(t)
	seedBackfillA1Rows(t, db, "9b88b3e8-e3f2-4776-b783-abef875061b8", 3)

	mappings := []rubricMapping{
		{"9b88b3e8-e3f2-4776-b783-abef875061b8", "artifact-review"},
	}

	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()

	if err := runBackfillA1(context.Background(), db, mappings, devnull); err != nil {
		t.Fatalf("first run: %v", err)
	}
	var tagged int
	db.QueryRow(`SELECT COUNT(*) FROM proj_benchmark_results WHERE task_id = 'artifact-review'`).Scan(&tagged)
	if tagged != 3 {
		t.Fatalf("first run did not tag rows: %d", tagged)
	}

	if err := runBackfillA1(context.Background(), db, mappings, devnull); err != nil {
		t.Fatalf("second run: %v", err)
	}
	db.QueryRow(`SELECT COUNT(*) FROM proj_benchmark_results WHERE task_id = 'artifact-review'`).Scan(&tagged)
	if tagged != 3 {
		t.Errorf("second run changed count: want 3, got %d", tagged)
	}
}

// TestRunBackfillA1_DoesNotOverwriteExistingTaskID verifies the WHERE
// task_id IS NULL guard: rows already tagged with a different task_id
// should be left alone.
func TestRunBackfillA1_DoesNotOverwriteExistingTaskID(t *testing.T) {
	db := newBackfillA1TestDB(t)
	if _, err := db.Exec(`
		INSERT INTO proj_benchmark_results
		(id, project_id, scenario_id, tool_name, model_name, run_id, run_at, wall_clock_ms, invocation_ok, task_id)
		VALUES ('row-1', 'p', 's', 'tool', 'model', '9b88b3e8-e3f2-4776-b783-abef875061b8', 0, 0, 1, 'manually-set')
	`); err != nil {
		t.Fatalf("seed pre-tagged row: %v", err)
	}

	mappings := []rubricMapping{
		{"9b88b3e8-e3f2-4776-b783-abef875061b8", "artifact-review"},
	}

	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()

	if err := runBackfillA1(context.Background(), db, mappings, devnull); err != nil {
		t.Fatalf("runBackfillA1: %v", err)
	}

	var taskID string
	db.QueryRow(`SELECT task_id FROM proj_benchmark_results WHERE id = 'row-1'`).Scan(&taskID)
	if taskID != "manually-set" {
		t.Errorf("backfill overwrote existing task_id: want 'manually-set', got %q", taskID)
	}
}

// TestA1RunRubricMap_HasNineEntries pins the hardcoded mapping size.
func TestA1RunRubricMap_HasNineEntries(t *testing.T) {
	if len(a1RunRubricMap) != 9 {
		t.Errorf("a1RunRubricMap len: want 9, got %d", len(a1RunRubricMap))
	}
}
