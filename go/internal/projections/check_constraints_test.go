package projections_test

import (
	"strings"
	"testing"

	"toolkit/internal/testutil"
)

// TestCheckConstraint_Bugs_OpenWithResolvedAtIsRejected reproduces the
// 2026-05-22 incident shape (bug 'rebuild-projections-wipes-pre-t2-
// terminal-state-when-events-incomplete'): a row with status='open'
// and resolved_at populated. Migration 066's biconditional CHECK on
// proj_current_bugs rejects it.
func TestCheckConstraint_Bugs_OpenWithResolvedAtIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(`
		INSERT INTO proj_current_bugs (slug, project_id, id, title, status,
		    resolved_at, filed_at, updated_at)
		VALUES ('test-open-with-resolved', 'mcp-servers', 99991, 'test', 'open',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`)
	if err == nil {
		t.Fatal("INSERT with status='open' AND resolved_at IS NOT NULL should be rejected by CHECK")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("expected CHECK constraint error, got: %v", err)
	}
}

// TestCheckConstraint_Bugs_TerminalWithoutResolvedAtIsRejected
// asserts the inverse direction: terminal status (fixed/wontfix/etc.)
// must carry a non-NULL resolved_at.
func TestCheckConstraint_Bugs_TerminalWithoutResolvedAtIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	for _, status := range []string{"fixed", "wontfix", "upstream", "dup", "routed"} {
		_, err := pool.DB().Exec(`
			INSERT INTO proj_current_bugs (slug, project_id, id, title, status, filed_at, updated_at)
			VALUES (?, 'mcp-servers', 99990, 'test', ?,
			    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`,
			"test-"+status+"-no-resolved", status)
		if err == nil {
			t.Errorf("INSERT with status=%q AND resolved_at NULL should be rejected", status)
			continue
		}
		if !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Errorf("status=%q: expected CHECK error, got: %v", status, err)
		}
	}
}

// TestCheckConstraint_Bugs_UnknownStatusIsRejected covers the vocab
// invariant. Six values are accepted; anything else is rejected.
func TestCheckConstraint_Bugs_UnknownStatusIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(`
		INSERT INTO proj_current_bugs (slug, project_id, id, title, status, filed_at, updated_at)
		VALUES ('test-bad-vocab', 'mcp-servers', 99989, 'test', 'archived',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`)
	if err == nil {
		t.Fatal("INSERT with status='archived' should be rejected — not in vocab")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("expected CHECK constraint error, got: %v", err)
	}
}

// TestCheckConstraint_Bugs_HappyPaths exercises every accepted shape:
// status='open' AND resolved_at IS NULL; status=terminal AND resolved_at
// IS NOT NULL. All 6 vocab values present at least once.
func TestCheckConstraint_Bugs_HappyPaths(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// open row with no resolved_at
	if _, err := pool.DB().Exec(`
		INSERT INTO proj_current_bugs (slug, project_id, id, title, status, filed_at, updated_at)
		VALUES ('test-open-clean', 'mcp-servers', 99988, 'test', 'open',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`); err != nil {
		t.Fatalf("clean-open row rejected: %v", err)
	}

	// terminal rows with resolved_at
	for i, status := range []string{"fixed", "wontfix", "upstream", "dup", "routed"} {
		_, err := pool.DB().Exec(`
			INSERT INTO proj_current_bugs (slug, project_id, id, title, status,
			    resolved_at, filed_at, updated_at)
			VALUES (?, 'mcp-servers', ?, 'test', ?,
			    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`,
			"test-terminal-"+status, 90000+i, status)
		if err != nil {
			t.Errorf("clean terminal row status=%q rejected: %v", status, err)
		}
	}
}

// TestCheckConstraint_Tasks_ClosedWithoutCommitShaIsRejected asserts
// the closed-status implication on proj_current_tasks.
func TestCheckConstraint_Tasks_ClosedWithoutCommitShaIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Need a chain first (FK / cross-reference); use a row id that
	// doesn't collide with seeded data.
	if _, err := pool.DB().Exec(`
		INSERT INTO proj_chain_status (slug, project_id, id, status, created_at, updated_at)
		VALUES ('test-chain', 'mcp-servers', 99987, 'open',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`); err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	_, err := pool.DB().Exec(`
		INSERT INTO proj_current_tasks (id, chain_id, slug, status, commit_sha,
		    created_at, updated_at)
		VALUES (99987, 99987, 'test-closed-no-commit', 'closed', NULL,
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`)
	if err == nil {
		t.Fatal("INSERT with status='closed' AND commit_sha IS NULL should be rejected")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("expected CHECK error, got: %v", err)
	}
}

// TestCheckConstraint_Tasks_CancelledWithoutCommitShaIsAccepted asserts
// cancelled is a terminal state that does NOT require commit_sha (923
// such rows exist in production; the constraint is targeted at closed
// only).
func TestCheckConstraint_Tasks_CancelledWithoutCommitShaIsAccepted(t *testing.T) {
	pool := testutil.NewTestDB(t)
	if _, err := pool.DB().Exec(`
		INSERT INTO proj_chain_status (slug, project_id, id, status, created_at, updated_at)
		VALUES ('test-chain2', 'mcp-servers', 99986, 'open',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`); err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	if _, err := pool.DB().Exec(`
		INSERT INTO proj_current_tasks (id, chain_id, slug, status, commit_sha,
		    created_at, updated_at)
		VALUES (99986, 99986, 'test-cancelled-clean', 'cancelled', NULL,
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`); err != nil {
		t.Errorf("cancelled + NULL commit_sha rejected (shouldn't be): %v", err)
	}
}

// TestCheckConstraint_Tasks_UnknownStatusIsRejected covers vocab.
func TestCheckConstraint_Tasks_UnknownStatusIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	if _, err := pool.DB().Exec(`
		INSERT INTO proj_chain_status (slug, project_id, id, status, created_at, updated_at)
		VALUES ('test-chain3', 'mcp-servers', 99985, 'open',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`); err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	_, err := pool.DB().Exec(`
		INSERT INTO proj_current_tasks (id, chain_id, slug, status,
		    created_at, updated_at)
		VALUES (99985, 99985, 'test-bad-vocab', 'archived',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`)
	if err == nil {
		t.Fatal("INSERT with task status='archived' should be rejected")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("expected CHECK error, got: %v", err)
	}
}

// TestCheckConstraint_Chains_ClosedWithoutClosureSummaryIsRejected
// asserts the closed-status implication on proj_chain_status. The
// closure_summary column is itself NOT NULL DEFAULT ", so an explicit
// NULL is rejected by the column-level constraint before the CHECK
// fires. Both constraints reject the same shape; the test pins the
// rejection regardless of which fires first (the chain author's intent
// per acceptance criteria was "closed implies closure_summary is
// recorded" — the column-level NOT NULL already enforces this at the
// hard floor, and the migration-066 CHECK is the documented invariant
// matching it).
func TestCheckConstraint_Chains_ClosedWithoutClosureSummaryIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(`
		INSERT INTO proj_chain_status (slug, project_id, id, status, closure_summary,
		    created_at, updated_at)
		VALUES ('test-closed-no-summary', 'mcp-servers', 99984, 'closed', NULL,
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`)
	if err == nil {
		t.Fatal("INSERT with chain status='closed' AND closure_summary IS NULL should be rejected")
	}
	// Accept either constraint surface: the column-level NOT NULL
	// fires first when an explicit NULL is provided; the CHECK fires
	// when something later UPDATEs closure_summary to NULL.
	if !strings.Contains(err.Error(), "NOT NULL constraint failed") &&
		!strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("expected NOT NULL or CHECK error, got: %v", err)
	}
}

// TestCheckConstraint_Chains_UnknownStatusIsRejected covers vocab.
// proj_chain_status's two-value vocab is open / closed.
func TestCheckConstraint_Chains_UnknownStatusIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(`
		INSERT INTO proj_chain_status (slug, project_id, id, status, created_at, updated_at)
		VALUES ('test-bad-vocab', 'mcp-servers', 99983, 'archived',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`)
	if err == nil {
		t.Fatal("INSERT with chain status='archived' should be rejected")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("expected CHECK error, got: %v", err)
	}
}

// TestCheckConstraint_Bugs_UpdateThatViolatesBiconditionalIsRejected
// covers the UPDATE path. The 2026-05-22 incident was an UPDATE
// (rebuild-projections flipped status from terminal back to 'open'
// while leaving resolved_at populated); migration 066's CHECK fires
// on UPDATE just as on INSERT.
func TestCheckConstraint_Bugs_UpdateThatViolatesBiconditionalIsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Seed a clean terminal row.
	if _, err := pool.DB().Exec(`
		INSERT INTO proj_current_bugs (slug, project_id, id, title, status,
		    resolved_at, filed_at, updated_at)
		VALUES ('test-rebuild-target', 'mcp-servers', 99982, 'test', 'fixed',
		    '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z', '2026-05-22T00:00:00Z')`); err != nil {
		t.Fatalf("seed clean-terminal: %v", err)
	}

	// Simulate the wrongful rebuild UPDATE: flip status to 'open',
	// leave resolved_at populated. This is exactly the 2026-05-22
	// regression shape and MUST be rejected.
	_, err := pool.DB().Exec(`
		UPDATE proj_current_bugs SET status='open'
		WHERE slug='test-rebuild-target' AND project_id='mcp-servers'`)
	if err == nil {
		t.Fatal("UPDATE flipping status='open' on a row with resolved_at populated should be rejected")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("expected CHECK error, got: %v", err)
	}
}

// ── proj_training_data_for_reranker (migration 071, chain
//    substrate-health-audit-projections T3) ────────────────────────────

// TestCheckConstraint_TrainingData_QueryTextNullOrEmptyRejected pins the
// query_text invariant: every (query, candidate, label) row must carry a
// non-empty query (a cross-encoder reranker scores (query, candidate)).
// NULL trips the column NOT NULL; " trips the CHECK. Regression for bug
// reranker-projection-drops-query-text-on-positive-labels.
func TestCheckConstraint_TrainingData_QueryTextNullOrEmptyRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)

	_, err := pool.DB().Exec(`
		INSERT INTO proj_training_data_for_reranker
		    (grounding_event_id, query_text, source_ref, candidate_position, label_kind, last_event_ts)
		VALUES (1, NULL, 'ref-null', 1, 'positive', '2026-05-23T00:00:00Z')`)
	if err == nil {
		t.Error("INSERT with query_text NULL should be rejected")
	} else if !strings.Contains(err.Error(), "constraint failed") {
		t.Errorf("NULL query_text: expected constraint failure, got: %v", err)
	}

	_, err = pool.DB().Exec(`
		INSERT INTO proj_training_data_for_reranker
		    (grounding_event_id, query_text, source_ref, candidate_position, label_kind, last_event_ts)
		VALUES (1, '', 'ref-empty', 1, 'positive', '2026-05-23T00:00:00Z')`)
	if err == nil {
		t.Error("INSERT with empty query_text should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("empty query_text: expected CHECK failure, got: %v", err)
	}
}

// TestCheckConstraint_TrainingData_LastEventTsNullOrEmptyRejected pins the
// last_event_ts invariant: it must carry the source event's real
// timestamp (chain 272's time-based held-out split needs it). Migration
// 068 dropped the masking " default; 071 makes the gap a rejected insert
// (NULL → NOT NULL, " → CHECK) instead of a silently-blank column.
// Regression for bug reranker-projection-last-event-ts-never-populated.
func TestCheckConstraint_TrainingData_LastEventTsNullOrEmptyRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)

	_, err := pool.DB().Exec(`
		INSERT INTO proj_training_data_for_reranker
		    (grounding_event_id, query_text, source_ref, candidate_position, label_kind, last_event_ts)
		VALUES (1, 'q', 'ref-ts-null', 1, 'positive', NULL)`)
	if err == nil {
		t.Error("INSERT with last_event_ts NULL should be rejected")
	} else if !strings.Contains(err.Error(), "constraint failed") {
		t.Errorf("NULL last_event_ts: expected constraint failure, got: %v", err)
	}

	_, err = pool.DB().Exec(`
		INSERT INTO proj_training_data_for_reranker
		    (grounding_event_id, query_text, source_ref, candidate_position, label_kind, last_event_ts)
		VALUES (1, 'q', 'ref-ts-empty', 1, 'positive', '')`)
	if err == nil {
		t.Error("INSERT with empty last_event_ts should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("empty last_event_ts: expected CHECK failure, got: %v", err)
	}
}

// TestCheckConstraint_TrainingData_HappyPathAccepted confirms a fully
// populated row (non-empty query_text + last_event_ts) inserts cleanly —
// the invariants reject the gap shapes, not legitimate rows.
func TestCheckConstraint_TrainingData_HappyPathAccepted(t *testing.T) {
	pool := testutil.NewTestDB(t)
	if _, err := pool.DB().Exec(`
		INSERT INTO proj_training_data_for_reranker
		    (grounding_event_id, query_text, source_ref, candidate_position, label_kind, last_event_ts)
		VALUES (1, 'how do I configure X?', 'ref-ok', 1, 'positive', '2026-05-23T00:00:00Z')`); err != nil {
		t.Errorf("fully-populated training row rejected: %v", err)
	}
}

// ── proj_memories (migration 072, chain substrate-health-audit-projections
//    T7) ────────────────────────────────────────────────────────────────
//
// Population invariants locking the columns the dedup loader (and future
// dashboard / curate consumers) rely on: kind in the payload enum,
// non-empty description / vault_path / last_event_ts. A fold regression
// that dropped one of these surfaces as a REJECTED insert, not a silently
// blank column.

const memoryInsertTmpl = `INSERT INTO proj_memories
	(name, kind, description, body_length_bytes, vault_path, filed_at, last_event_ts)
	VALUES (?, ?, ?, ?, ?, '2026-05-24T00:00:00Z', ?)`

// TestCheckConstraint_Memories_BadKindRejected pins the kind enum: only the
// four MemoryWritten payload kinds are storable.
func TestCheckConstraint_Memories_BadKindRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(memoryInsertTmpl,
		"m-badkind", "nonsense", "desc", 10, "/v/x.md", "2026-05-24T00:00:00Z")
	if err == nil {
		t.Error("INSERT with kind outside the enum should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("bad kind: expected CHECK failure, got: %v", err)
	}
}

// TestCheckConstraint_Memories_EmptyDescriptionRejected pins description
// non-empty (it's the dedup signature + the MEMORY.md index line).
func TestCheckConstraint_Memories_EmptyDescriptionRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(memoryInsertTmpl,
		"m-emptydesc", "feedback", "", 10, "/v/x.md", "2026-05-24T00:00:00Z")
	if err == nil {
		t.Error("INSERT with empty description should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("empty description: expected CHECK failure, got: %v", err)
	}
}

// TestCheckConstraint_Memories_EmptyVaultPathRejected pins vault_path
// non-empty (consumers verify the on-disk entry without re-deriving it).
func TestCheckConstraint_Memories_EmptyVaultPathRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(memoryInsertTmpl,
		"m-emptypath", "feedback", "desc", 10, "", "2026-05-24T00:00:00Z")
	if err == nil {
		t.Error("INSERT with empty vault_path should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("empty vault_path: expected CHECK failure, got: %v", err)
	}
}

// TestCheckConstraint_Memories_EmptyLastEventTsRejected pins last_event_ts
// non-empty — same shape as the reranker projection's ts lock (migration
// 071): a future writer regression surfaces as a rejected insert, not a
// blank time column.
func TestCheckConstraint_Memories_EmptyLastEventTsRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(memoryInsertTmpl,
		"m-emptyts", "feedback", "desc", 10, "/v/x.md", "")
	if err == nil {
		t.Error("INSERT with empty last_event_ts should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("empty last_event_ts: expected CHECK failure, got: %v", err)
	}
}

// TestCheckConstraint_Memories_HappyPathAccepted confirms a fully populated
// row inserts cleanly — the invariants reject the gap shapes, not
// legitimate rows. body_length_bytes=0 is legal (empty body).
func TestCheckConstraint_Memories_HappyPathAccepted(t *testing.T) {
	pool := testutil.NewTestDB(t)
	if _, err := pool.DB().Exec(memoryInsertTmpl,
		"m-ok", "feedback", "a real description", 0, "/v/feedback/m-ok.md", "2026-05-24T00:00:00Z"); err != nil {
		t.Errorf("fully-populated memory row rejected: %v", err)
	}
}

// ── query-telemetry projections (migration 073, chain substrate-health-
//    audit-projections T4 follow-on) ───────────────────────────────────
//
// last_event_id / last_event_ts must carry the source grounding_event's
// id / created_at; the empty-string shape (the masked-gap regression) is
// rejected at the DB. Bug query-telemetry-projections-hardcode-empty-last-
// event-id-ts. Mirrors the reranker invariant tests above.

// TestCheckConstraint_QueryVolume_EmptyWatermarkRejected pins the invariant
// on proj_query_volume_by_source.
func TestCheckConstraint_QueryVolume_EmptyWatermarkRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Empty last_event_ts → CHECK failure (the masked-gap shape).
	_, err := pool.DB().Exec(`INSERT INTO proj_query_volume_by_source
		(project_id, action, query_source, day, last_event_id, last_event_ts)
		VALUES ('p', 'vault_search', 'agent_initiated', '2026-05-24', '42', '')`)
	if err == nil {
		t.Error("INSERT with empty last_event_ts should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("empty last_event_ts: expected CHECK failure, got: %v", err)
	}
	// Happy path: non-empty watermark inserts cleanly.
	if _, err := pool.DB().Exec(`INSERT INTO proj_query_volume_by_source
		(project_id, action, query_source, day, last_event_id, last_event_ts)
		VALUES ('p', 'vault_search', 'agent_initiated', '2026-05-24', '42', '2026-05-24T00:00:00Z')`); err != nil {
		t.Errorf("fully-populated volume row rejected: %v", err)
	}
}

// TestCheckConstraint_RetrievalSuccess_EmptyWatermarkRejected pins the
// invariant on proj_retrieval_success_per_query.
func TestCheckConstraint_RetrievalSuccess_EmptyWatermarkRejected(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := pool.DB().Exec(`INSERT INTO proj_retrieval_success_per_query
		(grounding_event_id, project_id, action, last_event_id, last_event_ts)
		VALUES (1, 'p', 'vault_search', '1', '')`)
	if err == nil {
		t.Error("INSERT with empty last_event_ts should be rejected")
	} else if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("empty last_event_ts: expected CHECK failure, got: %v", err)
	}
	if _, err := pool.DB().Exec(`INSERT INTO proj_retrieval_success_per_query
		(grounding_event_id, project_id, action, last_event_id, last_event_ts)
		VALUES (1, 'p', 'vault_search', '1', '2026-05-24T00:00:00Z')`); err != nil {
		t.Errorf("fully-populated retrieval-success row rejected: %v", err)
	}
}
