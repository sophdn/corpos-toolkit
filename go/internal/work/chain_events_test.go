package work_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/work"
)

// Chain handler event-emit assertions (T2 of agent-first-substrate).

func TestChainClose_EmitsChainClosedWithSummary(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "ev-chain-summary")

	resp, err := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":    "ev-chain-summary",
		"summary": "shipped the X",
	}))
	if err != nil {
		t.Fatalf("HandleChainClose: %v", err)
	}
	if !resp.OK {
		t.Fatalf("close rejected: %+v", resp)
	}
	typ, payload := lastEventForEntity(t, pool, "chain", "ev-chain-summary")
	if typ != "ChainClosed" {
		t.Errorf("event type: got %q, want ChainClosed", typ)
	}
	var parsed struct {
		ClosureSummary *string `json:"closure_summary"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("parse payload: %v (raw=%s)", err, payload)
	}
	if parsed.ClosureSummary == nil || *parsed.ClosureSummary != "shipped the X" {
		t.Errorf("closure_summary mismatch: %+v", parsed.ClosureSummary)
	}
}

// Bug `no-action-can-write-chain-closure-summary`: callers reaching for
// the schema field name `closure_summary` (rather than the short `summary`)
// must not have their input silently dropped. The handler accepts both.
func TestChainClose_AcceptsClosureSummaryAlias(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "ev-chain-alias")

	resp, err := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":            "ev-chain-alias",
		"closure_summary": "schema-aligned name path",
	}))
	if err != nil {
		t.Fatalf("HandleChainClose: %v", err)
	}
	if !resp.OK {
		t.Fatalf("close rejected: %+v", resp)
	}
	// Verify the column landed (DB-level check).
	var got string
	if err := pool.DB().QueryRow(
		`SELECT closure_summary FROM proj_chain_status WHERE project_id='mcp-servers' AND slug='ev-chain-alias'`,
	).Scan(&got); err != nil {
		t.Fatalf("read chains.closure_summary: %v", err)
	}
	if got != "schema-aligned name path" {
		t.Errorf("closure_summary did not persist; got %q", got)
	}
	// And the event payload mirrors it.
	typ, payload := lastEventForEntity(t, pool, "chain", "ev-chain-alias")
	if typ != "ChainClosed" {
		t.Errorf("event type: got %q, want ChainClosed", typ)
	}
	var parsed struct {
		ClosureSummary *string `json:"closure_summary"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if parsed.ClosureSummary == nil || *parsed.ClosureSummary != "schema-aligned name path" {
		t.Errorf("event payload closure_summary mismatch: %+v", parsed.ClosureSummary)
	}
}

// When both `summary` and `closure_summary` are supplied, the
// schema-aligned `closure_summary` wins — it's the more deliberate
// name and the column-aligned one.
func TestChainClose_ClosureSummaryWinsOverSummary(t *testing.T) {
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "ev-chain-both")

	resp, err := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":            "ev-chain-both",
		"summary":         "short form",
		"closure_summary": "schema form wins",
	}))
	if err != nil {
		t.Fatalf("HandleChainClose: %v", err)
	}
	if !resp.OK {
		t.Fatalf("close rejected: %+v", resp)
	}
	var got string
	pool.DB().QueryRow(
		`SELECT closure_summary FROM proj_chain_status WHERE project_id='mcp-servers' AND slug='ev-chain-both'`,
	).Scan(&got)
	if got != "schema form wins" {
		t.Errorf("closure_summary did not pick the schema-aligned key; got %q", got)
	}
}

func TestChainClose_EmitsChainClosedWithoutSummary(t *testing.T) {
	// Legacy permissive path: chain_close with no summary supplied. The
	// schema was relaxed to permit null closure_summary in this chain
	// (T2) so this path doesn't reject at emit time.
	pool := openTestPool(t)
	seedChain(t, pool, "mcp-servers", "ev-chain-nosum")

	resp, _ := work.HandleChainClose(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug": "ev-chain-nosum",
	}))
	if !resp.OK {
		t.Fatalf("close rejected: %+v", resp)
	}
	typ, _ := lastEventForEntity(t, pool, "chain", "ev-chain-nosum")
	if typ != "ChainClosed" {
		t.Errorf("event type: got %q, want ChainClosed", typ)
	}
}
