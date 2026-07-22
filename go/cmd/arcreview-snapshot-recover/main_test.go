package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
)

func mustExec(t *testing.T, pool *db.Pool, q string) {
	t.Helper()
	if _, err := pool.DB().Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestRecover_ReconstructsPointInTime_Idempotent end-to-ends the cmd:
// a fire is recovered from its transcript at the fire's point-in-time
// (rows after fire_ts excluded), written source=recovered, and a re-run
// writes nothing (idempotent). Chain arc-close-snapshot-corpus-capture T4.
func TestRecover_ReconstructsPointInTime_Idempotent(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = pool.Close() }()

	mustExec(t, pool, `INSERT INTO projects (id, name) VALUES ('mcp-servers','mcp-servers')`)
	// One ArcCloseFilingReviewed fire at 00:00:05 for session sess-1.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		 entity_project_id, payload, related_entities, span_id, schema_version)
		VALUES ('ev-1','2026-05-24T00:00:05Z','system','test','ArcCloseFilingReviewed',
		 'arc_review_session','sess-1','mcp-servers','{}','[]','span-1',1)`)

	// Transcript: two rows before the fire, one after.
	root := t.TempDir()
	dir := filepath.Join(root, "-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	transcript := `{"role":"user","content":"first","timestamp":"2026-05-24T00:00:01Z"}
{"role":"assistant","content":"second","timestamp":"2026-05-24T00:00:02Z"}
{"role":"user","content":"AFTER the fire","timestamp":"2026-05-24T00:00:09Z"}
`
	if err := os.WriteFile(filepath.Join(dir, "sess-1.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if err := run(context.Background(), pool, root, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	var source string
	var msgCount int
	if err := pool.DB().QueryRow(
		`SELECT source, message_count FROM arcreview_snapshot_corpus WHERE event_id='ev-1'`,
	).Scan(&source, &msgCount); err != nil {
		t.Fatalf("recovered row not found: %v", err)
	}
	if source != "recovered" {
		t.Errorf("source = %q, want recovered", source)
	}
	if msgCount != 2 {
		t.Errorf("message_count = %d, want 2 (the 00:00:09 row is after fire_ts 00:00:05)", msgCount)
	}

	// Idempotent re-run: still exactly one row.
	if err := run(context.Background(), pool, root, false); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	var n int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM arcreview_snapshot_corpus WHERE event_id='ev-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("corpus rows for ev-1 = %d, want 1 (re-run must not duplicate)", n)
	}
}

// TestRecover_NeverOverwritesLiveRow: a live row for a fire is left
// untouched by recovery (idempotency must not clobber forward-captured data).
func TestRecover_NeverOverwritesLiveRow(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = pool.Close() }()

	mustExec(t, pool, `INSERT INTO projects (id, name) VALUES ('mcp-servers','mcp-servers')`)
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug,
		 entity_project_id, payload, related_entities, span_id, schema_version)
		VALUES ('ev-live','2026-05-24T00:00:05Z','system','test','ArcCloseFilingReviewed',
		 'arc_review_session','sess-live','mcp-servers','{}','[]','span-2',1)`)
	// A pre-existing LIVE corpus row for that fire.
	mustExec(t, pool, `INSERT INTO arcreview_snapshot_corpus
		(event_id, session_id, fire_ts, messages_json, message_count, estimated_tokens,
		 truncated, max_turns, max_tokens, source, schema_version)
		VALUES ('ev-live','sess-live','2026-05-24T00:00:05Z','[{"role":"user","content":"live"}]',1,1,0,20,4000,'live',1)`)

	root := t.TempDir()
	dir := filepath.Join(root, "-proj")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sess-live.jsonl"),
		[]byte(`{"role":"user","content":"transcript","timestamp":"2026-05-24T00:00:01Z"}`+"\n"), 0o600)

	if err := run(context.Background(), pool, root, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	var source, msgs string
	if err := pool.DB().QueryRow(
		`SELECT source, messages_json FROM arcreview_snapshot_corpus WHERE event_id='ev-live'`,
	).Scan(&source, &msgs); err != nil {
		t.Fatalf("row: %v", err)
	}
	if source != "live" {
		t.Errorf("source = %q, want live (recovery must not overwrite the live row)", source)
	}
	if msgs != `[{"role":"user","content":"live"}]` {
		t.Errorf("messages_json was overwritten: %q", msgs)
	}
}
