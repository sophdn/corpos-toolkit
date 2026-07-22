package grounding

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

func groundingEventCount(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var n int
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM grounding_events`).Scan(&n); err != nil {
		t.Fatalf("count grounding_events: %v", err)
	}
	return n
}

// TestHandleIngest_MatchesProcessFile is the parity gate for the HTTP-ingestion
// refactor: the container action path (Parse -> IngestRequest JSON -> HandleIngest)
// must produce exactly the same rows/counts as the direct --db path (ProcessFile)
// for the same transcript. If they diverge, the host-parse / container-write split
// has drifted from the single-process behavior.
func TestHandleIngest_MatchesProcessFile(t *testing.T) {
	resultJSON := `{\"results\":[{\"path\":\"learnings/general/2026-05-12_floor-char-boundary.md\"}]}`
	jsonl := makeSession("sess-HI", "prompt-HI", resultJSON) +
		assistantTextLine("sess-HI", "prompt-HI",
			"As 2026-05-12_floor-char-boundary documents, the boundary handling is non-trivial.")

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Path A — direct --db.
	poolA := testutil.NewTestDB(t)
	rA, err := ProcessFile(context.Background(), poolA, path, "corpos-toolkit", "", false)
	if err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}

	// Path B — the wire path: Parse host-side, marshal IngestRequest, HandleIngest.
	poolB := testutil.NewTestDB(t)
	events, entries, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	params, err := json.Marshal(IngestRequest{Events: events, Entries: entries})
	if err != nil {
		t.Fatalf("marshal IngestRequest: %v", err)
	}
	rB, err := HandleIngest(context.Background(), poolB, "corpos-toolkit", params)
	if err != nil {
		t.Fatalf("HandleIngest: %v", err)
	}

	if rB.Events == 0 {
		t.Fatal("expected >0 grounding events from the fixture")
	}
	if rA != rB {
		t.Fatalf("HandleIngest result %+v != ProcessFile result %+v", rB, rA)
	}
	if ca, cb := groundingEventCount(t, poolA), groundingEventCount(t, poolB); ca != cb || cb == 0 {
		t.Fatalf("grounding_events row count mismatch: ProcessFile=%d HandleIngest=%d", ca, cb)
	}
}

func TestHandleIngest_Validation(t *testing.T) {
	pool := testutil.NewTestDB(t)
	if _, err := HandleIngest(context.Background(), pool, "", nil); err == nil {
		t.Error("expected error for empty project")
	}
	if _, err := HandleIngest(context.Background(), pool, "p", json.RawMessage(`{bad json`)); err == nil {
		t.Error("expected error for malformed params")
	}
	// Empty (well-formed) params against a real project is a valid no-op: no events.
	r, err := HandleIngest(context.Background(), pool, "p", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("empty params should be a no-op, got: %v", err)
	}
	if r.Events != 0 {
		t.Errorf("empty ingest should yield 0 events, got %d", r.Events)
	}
}
