package db_test

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/db"
)

func ptrBool(b bool) *bool       { return &b }
func ptrString(s string) *string { return &s }

// seedOnlineEmitRow drops one grounding_events row whose call_id has
// the dispatcher-shape UUID, mimicking the online-emit path. Caller
// supplies action / source_refs / call_id / span_id so multiple
// fixtures can coexist in one test DB.
func seedOnlineEmitRow(t *testing.T, pool *db.Pool, action, refsJSON, callID, spanID string) {
	t.Helper()
	_, err := pool.DB().Exec(`
		INSERT INTO projects (id, name) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		"mcp-servers", "mcp-servers",
	)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err = pool.DB().Exec(`
		INSERT INTO grounding_events
			(project_id, session_id, call_id, action,
			 results_count, source_refs, next_turn_has_output, span_id,
			 query_source, created_at)
		VALUES (?, ?, ?, ?, 0, ?, 0, ?, 'agent_initiated', datetime('now'))`,
		"mcp-servers", "online-session", callID, action, refsJSON, spanID,
	)
	if err != nil {
		t.Fatalf("seed online-emit row: %v", err)
	}
}

func TestInsertGroundingEventTxBackstop_HitsExistingOnlineEmit(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()

	// Seed an online-emit row that the processor's call should
	// recognise as already-covering this search.
	seedOnlineEmitRow(t, pool,
		"vault_search",
		`["vault/decisions/some-note.md"]`,
		"8e5a599a-3360-4614-aa19-c119a258b497", // UUID — online emit shape
		"8e5a599a-3360-4614-aa19-c119a258b497",
	)

	processorEvent := db.GroundingEventInsert{
		ProjectID:         "mcp-servers",
		SessionID:         "claude-session-uuid", // different shape vs online row
		CallID:            "toolu_01abc",
		Action:            "vault_search",
		ResultsCount:      1,
		SourceRefs:        []string{"vault/decisions/some-note.md"},
		NextTurnHasOutput: true,
		Used:              ptrBool(true),
		PromptID:          ptrString("prompt-7"),
	}

	var id int64
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = db.InsertGroundingEventTxBackstop(ctx, tx, processorEvent, 0)
		return err
	})
	if err != nil {
		t.Fatalf("Backstop: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero id returned even on dedupe hit")
	}

	// The processor's call_id should NOT have produced a second row.
	var count int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM grounding_events WHERE action = 'vault_search'`,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("dedupe should collapse to 1 row; got %d", count)
	}

	// The online-emit row should now carry the processor-computed
	// enrichment fields.
	var usedCol sql.NullInt64
	var promptID, parentSpanID sql.NullString
	var nextTurn int
	if err := pool.DB().QueryRow(
		`SELECT next_turn_has_output, used, prompt_id, parent_span_id
		 FROM grounding_events WHERE id = ?`, id,
	).Scan(&nextTurn, &usedCol, &promptID, &parentSpanID); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if nextTurn != 1 {
		t.Errorf("next_turn_has_output should be enriched to 1; got %d", nextTurn)
	}
	if !usedCol.Valid || usedCol.Int64 != 1 {
		t.Errorf("used should be enriched to true; got valid=%v val=%d", usedCol.Valid, usedCol.Int64)
	}
	if !promptID.Valid || promptID.String != "prompt-7" {
		t.Errorf("prompt_id should be enriched; got %+v", promptID)
	}
}

func TestInsertGroundingEventTxBackstop_FallsThroughWhenNoOnlineEmit(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()

	_, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers') ON CONFLICT DO NOTHING`,
	)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	processorEvent := db.GroundingEventInsert{
		ProjectID:    "mcp-servers",
		SessionID:    "session-x",
		CallID:       "toolu_01xyz",
		Action:       "kiwix_search",
		ResultsCount: 3,
		SourceRefs:   []string{"rust-book::ch1", "rust-book::ch2"},
	}

	var id int64
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		var inErr error
		id, inErr = db.InsertGroundingEventTxBackstop(ctx, tx, processorEvent, 0)
		return inErr
	})
	if err != nil {
		t.Fatalf("Backstop: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero id from fallthrough insert")
	}

	var count int
	pool.DB().QueryRow(`SELECT COUNT(*) FROM grounding_events`).Scan(&count)
	if count != 1 {
		t.Fatalf("expected one inserted row; got %d", count)
	}
	var callID string
	pool.DB().QueryRow(`SELECT call_id FROM grounding_events WHERE id = ?`, id).Scan(&callID)
	if callID != "toolu_01xyz" {
		t.Errorf("inserted row's call_id mismatch: %q", callID)
	}
}

func TestInsertGroundingEventTxBackstop_IgnoresProcessorRowsInDedupe(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()

	// Seed a row with a toolu_* call_id — the dedupe filter must NOT
	// treat this as an online-emit row, so a subsequent processor
	// insert in a different session should still happen (no
	// false-positive coverage claim).
	seedOnlineEmitRow(t, pool,
		"vault_search",
		`["vault/decisions/some-note.md"]`,
		"toolu_01prior", // processor-shape call_id
		"toolu_01prior",
	)

	processorEvent := db.GroundingEventInsert{
		ProjectID:    "mcp-servers",
		SessionID:    "new-session",
		CallID:       "toolu_01new",
		Action:       "vault_search",
		ResultsCount: 1,
		SourceRefs:   []string{"vault/decisions/some-note.md"},
	}

	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, inErr := db.InsertGroundingEventTxBackstop(ctx, tx, processorEvent, 0)
		return inErr
	})
	if err != nil {
		t.Fatalf("Backstop: %v", err)
	}
	var count int
	pool.DB().QueryRow(`SELECT COUNT(*) FROM grounding_events WHERE action = 'vault_search'`).Scan(&count)
	if count != 2 {
		t.Fatalf("expected processor row to land alongside prior processor row; got %d", count)
	}
}
