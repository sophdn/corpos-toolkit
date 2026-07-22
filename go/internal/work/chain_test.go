package work_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/projections"
	"toolkit/internal/testutil"
	"toolkit/internal/work"
)

// openTestPool returns a temp-path pool with the full toolkit schema
// applied via db.Open's embedded migration runner (bug 1326), seeded
// with the `mcp-servers` project row work tests bind to. Also registers
// the events→projections fold hook so post-T4 handlers (which read from
// proj_*) see the fold updates emitted by HandleBug*/Task*/Chain*/etc.
//
// Post-agent-substrate-crud-retirement T6: the retired CRUD tables
// (bugs/tasks/chains/task_blockers/roadmap_items/suggestions) are gone;
// seeds write directly into the projection tables (proj_*).
func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	pool, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
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
	return pool
}

// seedChain wraps [testutil.SeedChain] with this package's narrow
// shape: open status, "o"/"cc" output/completion stubs. The wrapper
// survives because work_test callers don't need the chain id.
func seedChain(t *testing.T, pool *db.Pool, project, slug string) {
	t.Helper()
	testutil.SeedChain(t, pool, project, slug, "open", testutil.SeedChainOpts{
		Output:              "o",
		CompletionCondition: "cc",
	})
}

// seedTask resolves chainSlug → chainID via the projection table, then
// seeds the task and refreshes the parent chain's counter columns. The
// slug-to-id hop is the package-specific value-add; the counter refresh
// is load-bearing for chain_close assertions that read total_tasks etc.
func seedTask(t *testing.T, pool *db.Pool, chainSlug, taskSlug, status string) {
	t.Helper()
	var chainID int64
	if err := pool.DB().QueryRow(
		`SELECT id FROM proj_chain_status WHERE slug = ?`, chainSlug,
	).Scan(&chainID); err != nil {
		t.Fatalf("lookup chain %q: %v", chainSlug, err)
	}
	testutil.SeedTask(t, pool, chainID, taskSlug, status, testutil.SeedTaskOpts{
		ProblemStatement: "p",
	})
	testutil.RefreshChainCounters(t, pool, chainID)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestChainStatus_NoSlugListsOpenChains(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "a-chain")
	seedChain(t, pool, "mcp-servers", "another-chain")

	resp, err := work.HandleChainStatus(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("HandleChainStatus: %v", err)
	}
	if !resp.HasList {
		t.Fatalf("expected list result, got %+v", resp)
	}
	if len(resp.List) != 2 {
		t.Errorf("count: want 2, got %d", len(resp.List))
	}
}

func TestChainStatus_WithSlugReturnsSingleChain(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "my-chain")

	resp, err := work.HandleChainStatus(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "my-chain"}))
	if err != nil {
		t.Fatalf("HandleChainStatus: %v", err)
	}
	if !resp.HasSingle || resp.Single == nil {
		t.Fatalf("expected single result, got %+v", resp)
	}
	if resp.Single.Slug != "my-chain" {
		t.Errorf("slug: %q", resp.Single.Slug)
	}
}

func TestChainStatus_UnknownSlugReturnsError(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "my-chain")

	resp, _ := work.HandleChainStatus(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "absent"}))
	if resp.Err == nil || resp.Err.Error != "chain_not_found" {
		t.Errorf("error: want chain_not_found, got %+v", resp)
	}
}

func TestChainState_ReturnsDetail(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "x")
	resp, err := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "x"}))
	if err != nil {
		t.Fatalf("HandleChainState: %v", err)
	}
	if resp.Detail == nil {
		t.Fatalf("expected detail, got %+v", resp)
	}
	if resp.Detail.Output != "o" || resp.Detail.CompletionCondition != "cc" {
		t.Errorf("detail: %+v", resp.Detail)
	}
}

func TestChainState_MissingSlugError(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if resp.Err == nil || resp.Err.Error != "params.slug is required" {
		t.Errorf("error: %+v", resp)
	}
}

// Bug 1404/1406: chain_state accepts the numeric PK returned by
// chain_find — both `id` and `chain_id` aliases — so a chain_find →
// chain_state pivot doesn't have to round-trip through the slug.
func TestChainState_AcceptsChainID(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "by-id-chain")
	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_chain_status WHERE slug = ?`, "by-id-chain").Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	respByID, err := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"id": id}))
	if err != nil {
		t.Fatalf("HandleChainState id: %v", err)
	}
	if respByID.Detail == nil || respByID.Detail.Slug != "by-id-chain" {
		t.Errorf("want detail for by-id-chain via id, got %+v", respByID)
	}

	respByChainID, err := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"chain_id": id}))
	if err != nil {
		t.Fatalf("HandleChainState chain_id: %v", err)
	}
	if respByChainID.Detail == nil || respByChainID.Detail.Slug != "by-id-chain" {
		t.Errorf("want detail for by-id-chain via chain_id, got %+v", respByChainID)
	}
}

// Bug 1404/1406: unknown numeric id returns a structured not-found
// envelope, not a slug-shaped error.
func TestChainState_UnknownChainIDReturnsError(t *testing.T) {
	pool := openTestPool(t)
	resp, err := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"id": int64(999999)}))
	if err != nil {
		t.Fatalf("HandleChainState: %v", err)
	}
	if resp.Err == nil || resp.Err.Error == "" {
		t.Errorf("want error envelope, got %+v", resp)
	}
}

// Bug work-chain-identifier-handling-inconsistent-across-actions: a SLUG passed
// in the chain_id field (the handle parse_context surfaces) must resolve, not
// leak a raw "cannot unmarshal string into int64" error.
func TestChainState_AcceptsSlugInChainID(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "slug-in-id-chain")

	// slug passed as chain_id (string)
	resp, err := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"chain_id": "slug-in-id-chain"}))
	if err != nil {
		t.Fatalf("HandleChainState slug-in-chain_id: %v (must not leak a raw unmarshal error)", err)
	}
	if resp.Detail == nil || resp.Detail.Slug != "slug-in-id-chain" {
		t.Errorf("want detail via slug-in-chain_id, got %+v", resp)
	}

	// numeric id passed as a string still resolves by id
	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_chain_status WHERE slug = ?`, "slug-in-id-chain").Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}
	respNumStr, err := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"chain_id": fmt.Sprintf("%d", id)}))
	if err != nil {
		t.Fatalf("HandleChainState numeric-string chain_id: %v", err)
	}
	if respNumStr.Detail == nil || respNumStr.Detail.Slug != "slug-in-id-chain" {
		t.Errorf("want detail via numeric-string chain_id, got %+v", respNumStr)
	}
}

// A malformed chain_id (object) returns the typed missing-identifier envelope,
// never a raw Go unmarshal error.
func TestChainState_MalformedChainIDReturnsTypedEnvelope(t *testing.T) {
	pool := openTestPool(t)
	resp, err := work.HandleChainState(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"chain_id": map[string]any{"nested": true}}))
	if err != nil {
		t.Fatalf("must not leak a raw error for malformed chain_id, got: %v", err)
	}
	if resp.Err == nil {
		t.Errorf("want typed error envelope for malformed chain_id, got %+v", resp)
	}
}

func TestChainFind_PatternMatches(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "alpha-chain")
	seedChain(t, pool, "mcp-servers", "beta-chain")
	seedChain(t, pool, "mcp-servers", "alpha-other")

	resp, err := work.HandleChainFind(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"pattern": "alpha"}))
	if err != nil {
		t.Fatalf("HandleChainFind: %v", err)
	}
	if len(resp.List) != 2 {
		t.Errorf("match count: want 2, got %d", len(resp.List))
	}
}

// Suggestion #60: an empty pattern now LISTS chains rather than erroring.
func TestChainFind_EmptyPatternLists(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "alpha-chain")
	seedChain(t, pool, "mcp-servers", "beta-chain")

	resp, err := work.HandleChainFind(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("HandleChainFind: %v", err)
	}
	if resp.Err != nil {
		t.Fatalf("empty pattern must list, got error envelope: %+v", resp.Err)
	}
	if len(resp.List) != 2 {
		t.Errorf("expected 2 chains listed, got %d", len(resp.List))
	}
}

func TestChainClose_RejectsOpenTasks(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "x")
	seedTask(t, pool, "x", "t1", "pending")

	resp, _ := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{"slug": "x"}))
	if resp.Error == "" || resp.Error == "chain_not_found" {
		t.Errorf("error: %q (want non-terminal tasks message)", resp.Error)
	}

	// Chain row must still be open.
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_chain_status WHERE slug = 'x'`).Scan(&status)
	if status != "open" {
		t.Errorf("chain status: want open, got %q", status)
	}
}

func TestChainClose_SucceedsWhenAllTasksTerminal(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "y")
	seedTask(t, pool, "y", "t1", "closed")
	seedTask(t, pool, "y", "t2", "cancelled")

	resp, err := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":    "y",
		"summary": "all done",
	}))
	if err != nil {
		t.Fatalf("HandleChainClose: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ok: %+v", resp)
	}
	if resp.ClosureSummaryChars == nil || *resp.ClosureSummaryChars != 8 {
		t.Errorf("closure_summary_chars: %v", resp.ClosureSummaryChars)
	}
	var status, summary string
	pool.DB().QueryRow(`SELECT status, closure_summary FROM proj_chain_status WHERE slug = 'y'`).Scan(&status, &summary)
	if status != "closed" || summary != "all done" {
		t.Errorf("chain row: status=%q summary=%q", status, summary)
	}
}

func TestChainStatus_AcceptsChainAndChainSlugAliases(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "z")

	for _, key := range []string{"slug", "chain", "chain_slug"} {
		resp, _ := work.HandleChainStatus(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{key: "z"}))
		if !resp.HasSingle {
			t.Errorf("alias %q: expected single result, got %+v", key, resp)
		}
	}
}

// TestChainClose_AcceptsIDAlias pins bug 1329 parity for the chain
// surface: chain_close accepts {id: N} as a slug alias so chain_find →
// chain_close can stay id-keyed without a round-trip back through the
// slug.
func TestChainClose_AcceptsIDAlias(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "ida")
	seedTask(t, pool, "ida", "t1", "closed")
	var id int64
	if err := pool.DB().QueryRow(`SELECT id FROM proj_chain_status WHERE slug = 'ida'`).Scan(&id); err != nil {
		t.Fatalf("fetch id: %v", err)
	}

	resp, err := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id":      id,
		"summary": "closed-by-id",
	}))
	if err != nil {
		t.Fatalf("HandleChainClose: %v", err)
	}
	if !resp.OK || resp.ChainSlug != "ida" {
		t.Fatalf("expected ok+chain_slug=ida, got %+v", resp)
	}
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_chain_status WHERE slug = 'ida'`).Scan(&status)
	if status != "closed" {
		t.Errorf("chain status: want closed, got %q", status)
	}
}

// TestChainClose_IDNotFoundErrors locks in the error path: an id that
// doesn't resolve surfaces 'chain id N not found' so the caller can fix
// the call without re-reading docs.
func TestChainClose_IDNotFoundErrors(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"id": 99999,
	}))
	if resp.Error == "" || !contains(resp.Error, "99999") || !contains(resp.Error, "not found") {
		t.Errorf("expected not-found error citing id 99999, got %q", resp.Error)
	}
}

// TestChainClose_NeitherSlugNorIDErrors keeps the missing-identifier
// path honest: with the id alias added, the error wording names both
// fields so the caller knows either is accepted.
func TestChainClose_NeitherSlugNorIDErrors(t *testing.T) {
	pool := openTestPool(t)
	resp, _ := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{}))
	if resp.Error == "" || !contains(resp.Error, "slug") || !contains(resp.Error, "id") {
		t.Errorf("expected error naming slug AND id, got %q", resp.Error)
	}
}
