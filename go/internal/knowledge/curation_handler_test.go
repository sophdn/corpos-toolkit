package knowledge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/knowledge"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/testutil"
)

func seedTestCandidate(t *testing.T, pool *db.Pool, sourceRef string, score *float64) int64 {
	t.Helper()
	ins := curation.CandidateInsert{
		ProjectID:    "mcp-servers",
		SourceType:   "task",
		SourceRef:    sourceRef,
		Question:     "Initial question",
		InvokeWhen:   "Initial invoke_when",
		Description:  "Initial description",
		Tags:         []string{},
		QualityScore: score,
		Origin:       "task_handoff",
	}
	id, err := curation.AddCandidate(context.Background(), pool, ins)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

func TestHandleCurationList_FiltersAndShape(t *testing.T) {
	pool := testutil.NewTestDB(t)
	score := 0.7
	seedTestCandidate(t, pool, "mcp-servers::high", &score)
	seedTestCandidate(t, pool, "mcp-servers::unscored", nil)

	deps := knowledge.Deps{Pool: pool}
	got, err := knowledge.HandleCurationList(context.Background(), deps, "mcp-servers",
		json.RawMessage(`{"limit":10}`))
	if err != nil {
		t.Fatalf("HandleCurationList: %v", err)
	}
	if got.Count != 2 {
		t.Errorf("Count: want 2, got %d", got.Count)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("Candidates len: want 2, got %d", len(got.Candidates))
	}
	// Scored row first (quality_score DESC NULLS LAST).
	if got.Candidates[0].SourceRef != "mcp-servers::high" {
		t.Errorf("ordering: first should be 'high', got %q", got.Candidates[0].SourceRef)
	}

	// Unscored filter.
	got, _ = knowledge.HandleCurationList(context.Background(), deps, "mcp-servers",
		json.RawMessage(`{"scored":true,"limit":10}`))
	if got.Count != 1 {
		t.Errorf("UnscoredOnly Count: want 1, got %d", got.Count)
	}
}

func TestHandleCurationRead_FullBody(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedTestCandidate(t, pool, "mcp-servers::read", nil)

	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"id":` + intStr(id) + `}`)
	got, err := knowledge.HandleCurationRead(context.Background(), deps, "mcp-servers", body)
	if err != nil {
		t.Fatalf("HandleCurationRead: %v", err)
	}
	if got.Candidate.ID != id {
		t.Errorf("ID: want %d, got %d", id, got.Candidate.ID)
	}
	if got.InvokeWhen != "Initial invoke_when" {
		t.Errorf("InvokeWhen: got %q", got.InvokeWhen)
	}
}

func TestHandleCurationRead_RejectsMissingID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	deps := knowledge.Deps{Pool: pool}
	_, err := knowledge.HandleCurationRead(context.Background(), deps, "mcp-servers",
		json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("want error on missing id, got nil")
	}
}

func TestHandleCurationPromote_PromotesAndEmitsEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedTestCandidate(t, pool, "mcp-servers::promote", nil)

	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"id":` + intStr(id) + `}`)
	got, err := knowledge.HandleCurationPromote(context.Background(), deps, "mcp-servers", body)
	if err != nil {
		t.Fatalf("HandleCurationPromote: %v", err)
	}
	if got.PointerID == 0 {
		t.Fatal("PointerID 0")
	}
	if got.Status != "promoted" {
		t.Errorf("Status: want promoted, got %q", got.Status)
	}

	// Candidate is now promoted, NOT promoted_automatically.
	cand, _ := curation.ReadCandidate(context.Background(), pool, id)
	if cand.Status != "promoted" {
		t.Errorf("candidate status: want promoted, got %q", cand.Status)
	}
	if cand.PromotedAutomatically {
		t.Errorf("PromotedAutomatically: want false (MCP-driven), got true")
	}

	// Event was emitted — verify a row exists.
	var count int
	if err := pool.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE type='CurationCandidatePromoted' AND entity_slug=?`,
		intStr(id)).Scan(&count); err != nil {
		t.Fatalf("event count: %v", err)
	}
	if count != 1 {
		t.Errorf("CurationCandidatePromoted event count: want 1, got %d", count)
	}
}

func TestHandleCurationPromote_AppliesOverrides(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedTestCandidate(t, pool, "mcp-servers::override", nil)

	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"id":` + intStr(id) + `,"override_question":"Refined Q","override_invoke_when":"Refined IW"}`)
	_, err := knowledge.HandleCurationPromote(context.Background(), deps, "mcp-servers", body)
	if err != nil {
		t.Fatalf("HandleCurationPromote: %v", err)
	}

	// The pointer should have the refined metadata.
	var pq string
	if err := pool.DB().QueryRow(
		`SELECT question FROM knowledge_pointers WHERE source_ref=?`,
		"mcp-servers::override").Scan(&pq); err != nil {
		t.Fatalf("pointer query: %v", err)
	}
	if pq != "Refined Q" {
		t.Errorf("pointer question: want Refined Q, got %q", pq)
	}
}

func TestHandleCurationReject_RejectsAndEmitsEvent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedTestCandidate(t, pool, "mcp-servers::reject", nil)

	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"id":` + intStr(id) + `,"reason":"off-topic session noise"}`)
	got, err := knowledge.HandleCurationReject(context.Background(), deps, "mcp-servers", body)
	if err != nil {
		t.Fatalf("HandleCurationReject: %v", err)
	}
	if !got.OK {
		t.Error("OK: want true")
	}

	cand, _ := curation.ReadCandidate(context.Background(), pool, id)
	if cand.Status != "rejected" {
		t.Errorf("Status: want rejected, got %q", cand.Status)
	}

	var count int
	pool.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE type='CurationCandidateRejected' AND entity_slug=?`,
		intStr(id)).Scan(&count)
	if count != 1 {
		t.Errorf("CurationCandidateRejected event count: want 1, got %d", count)
	}
}

func TestHandleCurationReject_RejectsEmptyReason(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedTestCandidate(t, pool, "mcp-servers::reject-empty", nil)

	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"id":` + intStr(id) + `,"reason":""}`)
	_, err := knowledge.HandleCurationReject(context.Background(), deps, "mcp-servers", body)
	if err == nil {
		t.Fatal("want error on empty reason, got nil")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("error should mention reason: %v", err)
	}
}

func TestHandleCurationBulkAction_DryRun(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id1 := seedTestCandidate(t, pool, "mcp-servers::bulk-1", nil)
	id2 := seedTestCandidate(t, pool, "mcp-servers::bulk-2", nil)

	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"filter":{"unscored_only":true},"action":"reject","reason":"bulk noise","dry_run":true,"limit":10}`)
	got, err := knowledge.HandleCurationBulkAction(context.Background(), deps, "mcp-servers", body)
	if err != nil {
		t.Fatalf("HandleCurationBulkAction: %v", err)
	}
	if !got.DryRun {
		t.Error("DryRun: want true")
	}
	if got.Matched != 2 {
		t.Errorf("Matched: want 2, got %d", got.Matched)
	}
	if got.Succeeded != 0 || got.Failed != 0 {
		t.Errorf("DryRun should not write: succeeded=%d failed=%d", got.Succeeded, got.Failed)
	}

	// Both candidates still pending.
	for _, id := range []int64{id1, id2} {
		c, _ := curation.ReadCandidate(context.Background(), pool, id)
		if c.Status != "pending" {
			t.Errorf("DryRun: candidate %d should remain pending, got %q", id, c.Status)
		}
	}
}

func TestHandleCurationBulkAction_AppliesReject(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedTestCandidate(t, pool, "mcp-servers::bulk-1", nil)
	seedTestCandidate(t, pool, "mcp-servers::bulk-2", nil)

	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"filter":{"unscored_only":true},"action":"reject","reason":"bulk cleanup","limit":10}`)
	got, err := knowledge.HandleCurationBulkAction(context.Background(), deps, "mcp-servers", body)
	if err != nil {
		t.Fatalf("HandleCurationBulkAction: %v", err)
	}
	if got.Succeeded != 2 {
		t.Errorf("Succeeded: want 2, got %d", got.Succeeded)
	}

	// Two rejected events emitted.
	var count int
	pool.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE type='CurationCandidateRejected'`).Scan(&count)
	if count != 2 {
		t.Errorf("event count: want 2, got %d", count)
	}
}

func TestHandleCurationBulkAction_RejectsEmptyFilter(t *testing.T) {
	pool := testutil.NewTestDB(t)
	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"filter":{},"action":"reject","reason":"oops"}`)
	_, err := knowledge.HandleCurationBulkAction(context.Background(), deps, "mcp-servers", body)
	if err == nil {
		t.Fatal("want error on empty filter, got nil")
	}
	if !strings.Contains(err.Error(), "filter") {
		t.Errorf("error should mention filter requirement: %v", err)
	}
}

func TestHandleCurationBulkAction_RejectsRejectWithoutReason(t *testing.T) {
	pool := testutil.NewTestDB(t)
	deps := knowledge.Deps{Pool: pool}
	body := []byte(`{"filter":{"unscored_only":true},"action":"reject"}`)
	_, err := knowledge.HandleCurationBulkAction(context.Background(), deps, "mcp-servers", body)
	if err == nil {
		t.Fatal("want error on reject without reason, got nil")
	}
}

// --- helpers ---

func intStr(n int64) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
