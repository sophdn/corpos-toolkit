package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/db"
)

// fallback_test.go — T5 of chain arc-close-decision-authoring-split.
// Verifies the unreviewed-fallback capture: capture-not-lost on agent
// disengagement, authored-suppression, the grace window, and the fail-safe
// when forge is unwired.

// forgeParamsProbe is a typed view over the fallback's marshaled forge
// params (union of vault-note + memory fields, all strings) so the test
// reads them without a bare-any map.
type forgeParamsProbe struct {
	SchemaName string `json:"schema_name"`
	Slug       string `json:"slug"`
	Fields     struct {
		Tags   string `json:"tags"`
		Body   string `json:"body"`
		Source string `json:"source"`
		Title  string `json:"title"`
		Name   string `json:"name"`
	} `json:"fields"`
}

// capturingForge records every ForgeFn call so tests can assert the
// fallback's forge params (schema, unreviewed tag, sentinel, retained body).
type capturingForge struct {
	calls []json.RawMessage
}

func (c *capturingForge) fn() ArtifactForgeFn {
	return func(_ context.Context, _ string, params json.RawMessage) error {
		c.calls = append(c.calls, params)
		return nil
	}
}

// insertStagedRow inserts one pending_decisions row in the 'staged' state
// with an explicit (old) created_at so the grace window doesn't exclude it
// unless a test wants it to.
func insertStagedRow(t *testing.T, pool *db.Pool, sessionID, createdAt string, decisions []FilingDecision) int64 {
	t.Helper()
	dj, err := json.Marshal(decisions)
	if err != nil {
		t.Fatalf("marshal decisions: %v", err)
	}
	var id int64
	err = pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		res, e := tx.ExecContext(context.Background(), `
			INSERT INTO pending_decisions
				(event_id, project_id, target_session_id, decisions_json, triggers_json, created_at, authoring_state)
			VALUES (?, ?, ?, ?, '[]', ?, 'staged')`,
			"evt-"+sessionID, "mcp-servers", sessionID, string(dj), createdAt)
		if e != nil {
			return e
		}
		id, e = res.LastInsertId()
		return e
	})
	if err != nil {
		t.Fatalf("insert staged row: %v", err)
	}
	return id
}

func rowAuthoringState(t *testing.T, pool *db.Pool, id int64) string {
	t.Helper()
	var s sql.NullString
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT authoring_state FROM pending_decisions WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read authoring_state: %v", err)
	}
	return s.String
}

func stagedVaultDecision(title, body string) FilingDecision {
	payload, _ := json.Marshal(ForgeVaultNotePayload{NoteKind: "decision", Title: title, Body: body, Tags: "arcreview"})
	return FilingDecision{Action: ActionForgeVaultNote, Confidence: 0.95, Payload: payload, Reasoning: "worth filing", StagedForAuthoring: true}
}

// TestFallback_DisengagementCapturesUnreviewed is the load-bearing
// capture-not-lost test: a staged decision the agent never authored is
// forged from Qwen's retained draft, flagged unreviewed, and the row
// transitions to fallback_forged.
func TestFallback_DisengagementCapturesUnreviewed(t *testing.T) {
	pool := openTestPool(t)
	id := insertStagedRow(t, pool, "sess-dis", "2026-05-26 00:00:00",
		[]FilingDecision{stagedVaultDecision("the decider/author split", "QWEN DRAFT BODY")})

	cf := &capturingForge{}
	deps := Deps{Pool: pool, ForgeFn: cf.fn()}
	res, err := SweepUnauthoredStaged(context.Background(), deps, "mcp-servers", "sess-dis")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.FallbackForged != 1 {
		t.Fatalf("expected 1 fallback forged, got %d (%+v)", res.FallbackForged, res)
	}
	if len(cf.calls) != 1 {
		t.Fatalf("expected 1 forge call, got %d", len(cf.calls))
	}
	if got := rowAuthoringState(t, pool, id); got != authoringStateFallback {
		t.Fatalf("expected row state %q, got %q", authoringStateFallback, got)
	}

	// Inspect the forge params: vault-note schema, unreviewed+qwen-authored
	// tags, sentinel-prefixed body that still contains Qwen's draft.
	var fp forgeParamsProbe
	if err := json.Unmarshal(cf.calls[0], &fp); err != nil {
		t.Fatalf("unmarshal forge params: %v", err)
	}
	if fp.SchemaName != "vault-note" {
		t.Fatalf("expected vault-note schema, got %q", fp.SchemaName)
	}
	tags := fp.Fields.Tags
	if !strings.Contains(tags, "unreviewed") || !strings.Contains(tags, "qwen-authored") {
		t.Fatalf("expected unreviewed+qwen-authored tags, got %q", tags)
	}
	body := fp.Fields.Body
	if !strings.Contains(body, "unreviewed") {
		t.Fatalf("expected sentinel in body, got: %.80q", body)
	}
	if !strings.Contains(body, "QWEN DRAFT BODY") {
		t.Fatalf("expected Qwen's retained draft body to be forged, got: %.120q", body)
	}
}

// TestFallback_AuthoredSuppressesForge: when the agent already authored a
// vault note whose title matches the staged seed (a knowledge_pointers
// row), no fallback is forged and the row transitions to 'authored'.
func TestFallback_AuthoredSuppressesForge(t *testing.T) {
	pool := openTestPool(t)
	id := insertStagedRow(t, pool, "sess-auth", "2026-05-26 00:00:00",
		[]FilingDecision{stagedVaultDecision("the decider author split", "qwen draft")})

	// Simulate the agent having authored the note: a vault knowledge_pointer
	// with a title near the seed.
	if err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO knowledge_pointers (project_id, source_type, source_ref, question, invoke_when)
			VALUES ('mcp-servers', 'vault', 'decisions/2026-05-26_the-decider-author-split.md', 'the decider author split', 'when')`)
		return e
	}); err != nil {
		t.Fatalf("seed knowledge_pointer: %v", err)
	}

	cf := &capturingForge{}
	deps := Deps{Pool: pool, ForgeFn: cf.fn()}
	res, err := SweepUnauthoredStaged(context.Background(), deps, "mcp-servers", "sess-auth")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(cf.calls) != 0 {
		t.Fatalf("expected NO fallback forge (agent authored), got %d", len(cf.calls))
	}
	if res.AuthoredSkips != 1 {
		t.Fatalf("expected 1 authored skip, got %d", res.AuthoredSkips)
	}
	if got := rowAuthoringState(t, pool, id); got != authoringStateAuthored {
		t.Fatalf("expected row state %q, got %q", authoringStateAuthored, got)
	}
}

// TestFallback_GraceWindowDefersFreshRows: a staged row newer than the
// grace window is NOT swept (gives the agent time to author).
func TestFallback_GraceWindowDefersFreshRows(t *testing.T) {
	pool := openTestPool(t)
	// created_at far in the future relative to (now - 15m grace) so the
	// row is "younger than grace" and excluded.
	id := insertStagedRow(t, pool, "sess-fresh", "2099-01-01 00:00:00",
		[]FilingDecision{stagedVaultDecision("too fresh to reap", "draft")})

	cf := &capturingForge{}
	deps := Deps{Pool: pool, ForgeFn: cf.fn()}
	res, err := SweepUnauthoredStaged(context.Background(), deps, "mcp-servers", "sess-fresh")
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.RowsScanned != 0 || len(cf.calls) != 0 {
		t.Fatalf("expected fresh row deferred (0 scanned, 0 forged), got scanned=%d forged=%d", res.RowsScanned, len(cf.calls))
	}
	if got := rowAuthoringState(t, pool, id); got != authoringStateStaged {
		t.Fatalf("expected row to stay %q, got %q", authoringStateStaged, got)
	}
}

// TestFallback_NilForgeFnIsFailSafe: with no forge wired, the sweep is a
// no-op that leaves staged rows untouched (never crashes, never loses).
func TestFallback_NilForgeFnIsFailSafe(t *testing.T) {
	pool := openTestPool(t)
	id := insertStagedRow(t, pool, "sess-nil", "2026-05-26 00:00:00",
		[]FilingDecision{stagedVaultDecision("no forge wired", "draft")})

	deps := Deps{Pool: pool, ForgeFn: nil}
	res, err := SweepUnauthoredStaged(context.Background(), deps, "mcp-servers", "sess-nil")
	if err != nil {
		t.Fatalf("sweep with nil ForgeFn should not error: %v", err)
	}
	if res.FallbackForged != 0 {
		t.Fatalf("expected 0 forged with nil ForgeFn, got %d", res.FallbackForged)
	}
	if got := rowAuthoringState(t, pool, id); got != authoringStateStaged {
		t.Fatalf("expected row to stay staged with nil ForgeFn, got %q", got)
	}
}

// TestFallback_MemoryWriteParams: a staged memory_write falls back to a
// forge(memory, ...) with the unreviewed source + sentinel body.
func TestFallback_MemoryWriteParams(t *testing.T) {
	pool := openTestPool(t)
	payload, _ := json.Marshal(MemoryWritePayload{MemoryKind: "project", Name: "arc-split-incident", Description: "d", Body: "QWEN MEM DRAFT"})
	dec := FilingDecision{Action: ActionMemoryWrite, Confidence: 0.92, Payload: payload, Reasoning: "r", StagedForAuthoring: true}
	insertStagedRow(t, pool, "sess-mem", "2026-05-26 00:00:00", []FilingDecision{dec})

	cf := &capturingForge{}
	deps := Deps{Pool: pool, ForgeFn: cf.fn()}
	if _, err := SweepUnauthoredStaged(context.Background(), deps, "mcp-servers", "sess-mem"); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(cf.calls) != 1 {
		t.Fatalf("expected 1 memory fallback forge, got %d", len(cf.calls))
	}
	var fp forgeParamsProbe
	if err := json.Unmarshal(cf.calls[0], &fp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fp.SchemaName != "memory" {
		t.Fatalf("expected memory schema, got %q", fp.SchemaName)
	}
	if src := fp.Fields.Source; !strings.Contains(src, "fallback-unreviewed") {
		t.Fatalf("expected unreviewed-fallback source marker, got %q", src)
	}
	if body := fp.Fields.Body; !strings.Contains(body, "QWEN MEM DRAFT") {
		t.Fatalf("expected retained memory draft body, got %.80q", body)
	}
}
