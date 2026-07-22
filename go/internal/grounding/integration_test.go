package grounding

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/telemetry"
	"toolkit/internal/testutil"
)

// Acceptance items (a)–(d) from TT2's task body. Each test drives
// processFile against a synthetic session JSONL and asserts the
// resulting telemetry rows. Items (e)–(g) are covered by
// go/internal/telemetry/telemetry_test.go and the in-flight
// upsert/UNIQUE-triple invariants exercised there.

// runProcessor drives processFile against a synthetic JSONL fixture
// inside a fresh test DB. Returns the pool so the test can SELECT the
// resulting rows.
func runProcessor(t *testing.T, jsonl, projectID string) *db.Pool {
	t.Helper()
	pool := testutil.NewTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ProcessFile(context.Background(), pool, path, projectID, "", false); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	return pool
}

func assertInteraction(t *testing.T, pool *db.Pool, sourceRef string, kind telemetry.ClickKind) int64 {
	t.Helper()
	var id int64
	row := pool.DB().QueryRowContext(context.Background(),
		`SELECT id FROM query_interactions WHERE source_ref = ? AND click_kind = ?`,
		sourceRef, string(kind))
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			rows, _ := pool.DB().QueryContext(context.Background(),
				`SELECT source_ref, click_kind FROM query_interactions`)
			defer rows.Close()
			var dump []string
			for rows.Next() {
				var s, k string
				_ = rows.Scan(&s, &k)
				dump = append(dump, s+"/"+k)
			}
			t.Fatalf("interaction (%s, %s) not found; have: %v", sourceRef, kind, dump)
		}
		t.Fatalf("query_interactions scan: %v", err)
	}
	return id
}

// (a) vault_search + slug mention in next turn → click_kind=mentioned.
func TestIntegration_A_Mentioned(t *testing.T) {
	resultJSON := `{\"results\":[{\"path\":\"learnings/general/2026-05-12_floor-char-boundary.md\"}]}`
	jsonl := makeSession("sess-A", "prompt-A", resultJSON) +
		assistantTextLine("sess-A", "prompt-A",
			"As 2026-05-12_floor-char-boundary documents, the boundary handling is non-trivial.")
	pool := runProcessor(t, jsonl, "mcp-servers")
	assertInteraction(t, pool, "learnings/general/2026-05-12_floor-char-boundary.md", telemetry.ClickMentioned)
}

// Regression for bug `reranker-projection-drops-query-text-on-positive-
// labels`: a processor-created grounding_events row (online emit missed
// the search) must carry query_text. Pre-fix this column was NULL on
// every processor row, and since all positive-labeled training rows
// derive from processor rows, the cross-encoder corpus was untrainable.
func TestIntegration_ProcessorPopulatesQueryText(t *testing.T) {
	resultJSON := `{\"results\":[{\"path\":\"learnings/general/2026-05-12_floor-char-boundary.md\"}]}`
	jsonl := makeSession("sess-QT", "prompt-QT", resultJSON)
	pool := runProcessor(t, jsonl, "mcp-servers")
	var queryText sql.NullString
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT query_text FROM grounding_events WHERE call_id = 'toolu_search'`).Scan(&queryText); err != nil {
		t.Fatalf("scan query_text: %v", err)
	}
	if !queryText.Valid || queryText.String != "floor-char boundary" {
		t.Errorf("query_text = %v, want %q (processor must carry query text through)", queryText, "floor-char boundary")
	}
}

// (b) vault_search + native Read of the exact path → click_kind=followed.
func TestIntegration_B_Followed(t *testing.T) {
	resultJSON := `{\"results\":[{\"path\":\"learnings/general/2026-05-12_floor-char-boundary.md\"}]}`
	readLine := `{"type":"assistant","sessionId":"sess-B","promptId":"prompt-B","message":{"content":[{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"/home/user/dev/mcp-servers/learnings/general/2026-05-12_floor-char-boundary.md"}}]}}` + "\n"
	jsonl := makeSession("sess-B", "prompt-B", resultJSON) + readLine
	pool := runProcessor(t, jsonl, "mcp-servers")
	assertInteraction(t, pool, "learnings/general/2026-05-12_floor-char-boundary.md", telemetry.ClickFollowed)
}

// (b2) knowledge_search surfacing a non-vault (chain) candidate +
// work.chain_state follow → click_kind=followed. End-to-end production
// smoke for bug follow-detector-only-credits-vault-kiwix-read: pre-fix the
// chain candidate recorded NO interaction (isFollowAction rejected the
// work tool), so it could only ever be labeled negative/hard_negative in
// proj_training_data_for_reranker. A followed interaction carries weight
// 1.0, which the query_training projection maps to a positive label.
func TestIntegration_B2_FollowedNonVaultChain(t *testing.T) {
	body := `[{\"source_ref\":\"chain:mcp-servers::action-docs-corpus\"}]`
	jsonl := `{"type":"user","sessionId":"sess-B2","promptId":"prompt-B2","message":{"content":"help me find the chain"}}
{"type":"assistant","sessionId":"sess-B2","promptId":"prompt-B2","message":{"content":[{"type":"tool_use","id":"toolu_search","name":"mcp__toolkit-server__knowledge","input":{"action":"knowledge_search","params":{"query":"action docs corpus"}}}]}}
{"type":"user","sessionId":"sess-B2","promptId":"prompt-B2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[{"type":"text","text":"` + body + `"}]}]}}
{"type":"assistant","sessionId":"sess-B2","promptId":"prompt-B2","message":{"content":[{"type":"tool_use","id":"toolu_chain","name":"mcp__toolkit-server__work","input":{"action":"chain_state","project":"mcp-servers","params":{"slug":"action-docs-corpus"}}}]}}
`
	pool := runProcessor(t, jsonl, "mcp-servers")
	assertInteraction(t, pool, "chain:mcp-servers::action-docs-corpus", telemetry.ClickFollowed)
}

// (c) vault_search + 50-char quote in assistant text → click_kind=cited.
// The vault_search response is extended with an `excerpt` field so the
// quoted-block branch has body text to match against (it would be a
// no-op against today's vault_search response, but the detector is
// agnostic to schema shape — see detect.go::extractResultBodies).
func TestIntegration_C_Cited(t *testing.T) {
	excerpt := "this is a fifty-character excerpt right exactly!!!"
	if len(excerpt) < minCitationChars {
		t.Fatalf("test setup: excerpt < %d chars", minCitationChars)
	}
	// Tool_result text is double-escaped (it's a JSON string inside a JSON
	// content block). Building it with json.Marshal keeps the escaping
	// honest.
	body := map[string]any{
		"results": []map[string]any{
			{"path": "learnings/general/2026-05-12_floor-char-boundary.md", "excerpt": excerpt},
		},
	}
	bodyBytes, _ := json.Marshal(body)
	resultEscaped := string(bodyBytes)
	resultEscaped = strings.ReplaceAll(resultEscaped, `"`, `\"`)
	jsonl := makeSession("sess-C", "prompt-C", resultEscaped) +
		assistantTextLine("sess-C", "prompt-C",
			`As the note says: "`+excerpt+`" — that's the pattern.`)
	pool := runProcessor(t, jsonl, "mcp-servers")
	id := assertInteraction(t, pool, "learnings/general/2026-05-12_floor-char-boundary.md", telemetry.ClickCited)
	var citationKind sql.NullString
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT citation_kind FROM query_interactions WHERE id = ?`, id).Scan(&citationKind); err != nil {
		t.Fatalf("citation_kind scan: %v", err)
	}
	if !citationKind.Valid || citationKind.String != string(telemetry.CitationQuotedBlock) {
		t.Errorf("citation_kind = %v, want quoted-block", citationKind)
	}
}

// (d) prompt arc with vault_search + bug_resolve whose rationale cites
// the result → both a query_resolutions row with write_event_ids populated
// AND a query_interactions row with click_kind=resolved-from. Pre-inserts
// a corresponding events row so the write_event_ids lookup hits.
func TestIntegration_D_ResolvedFromAndResolution(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Pre-seed events table with a BugResolved row matching the
	// terminal tool_use in the synthetic session. The processor's
	// write_event_ids lookup picks this up.
	eventID := "01940000-1111-7222-8333-aaaaaaaaaaaa"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := pool.DB().ExecContext(context.Background(), `
		INSERT INTO events (event_id, ts, actor_kind, actor_id, type,
			entity_kind, entity_slug, entity_project_id, payload, rationale,
			related_entities, span_id, schema_version)
		VALUES (?, ?, 'agent', 'claude-test', 'BugResolved',
			'bug', 'forge-bug-title-omitted', 'mcp-servers', ?, ?,
			'[]', 'span-write-1', 1)`,
		eventID, now,
		`{"kind":"fixed","commit_sha":"deadbeef"}`,
		"Fixed per learnings/general/2026-05-12_floor-char-boundary insights.",
	)
	if err != nil {
		t.Fatalf("seed events: %v", err)
	}

	// Synthetic transcript: user prompt → vault_search → tool_result →
	// assistant text → bug_resolve tool_use. The bug_resolve's
	// resolution_note carries the slug, triggering resolved-from.
	resultEscaped := `{\"results\":[{\"path\":\"learnings/general/2026-05-12_floor-char-boundary.md\"}]}`
	jsonl := makeSession("sess-D", "prompt-D", resultEscaped) +
		`{"type":"assistant","sessionId":"sess-D","promptId":"prompt-D","message":{"content":[{"type":"tool_use","id":"toolu_resolve","name":"mcp__toolkit-server__work","input":{"action":"bug_resolve","project":"mcp-servers","params":{"slug":"forge-bug-title-omitted","resolution_note":"Fixed per learnings/general/2026-05-12_floor-char-boundary insights."}}}]}}` + "\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := ProcessFile(context.Background(), pool, path, "mcp-servers", "", false); err != nil {
		t.Fatalf("processFile: %v", err)
	}

	// Assert the resolved-from interaction landed.
	assertInteraction(t, pool, "learnings/general/2026-05-12_floor-char-boundary.md", telemetry.ClickResolvedFrom)

	// Assert one query_resolutions row landed with write_event_ids
	// containing the seeded events row.
	var (
		entityKind, entitySlug, outcome  string
		writeEvents, groundingIDs, qiIDs string
	)
	row := pool.DB().QueryRowContext(context.Background(), `
		SELECT entity_kind, entity_slug, outcome_kind,
			write_event_ids, grounding_event_ids, query_interaction_ids
		FROM query_resolutions
		WHERE entity_slug = 'forge-bug-title-omitted'`)
	if err := row.Scan(&entityKind, &entitySlug, &outcome,
		&writeEvents, &groundingIDs, &qiIDs); err != nil {
		t.Fatalf("query_resolutions scan: %v", err)
	}
	if entityKind != "bug" || outcome != "resolved" {
		t.Errorf("envelope mismatch: kind=%s outcome=%s", entityKind, outcome)
	}
	if !strings.Contains(writeEvents, eventID) {
		t.Errorf("write_event_ids missing seeded event: %s (want %s)", writeEvents, eventID)
	}
	if groundingIDs == "[]" {
		t.Errorf("grounding_event_ids should be non-empty: %s", groundingIDs)
	}
	if qiIDs == "[]" {
		t.Errorf("query_interaction_ids should be non-empty: %s", qiIDs)
	}
}

// (sidechain) Per TT1.5 §7.5, sidechain subagent transcripts live at
// ~/.claude/projects/<proj>/subagents/agent-*.jsonl and their grounding
// rows need parent_span_id stamped from the parent agent's span. The
// processor accepts --parent-span-id as the hook-supplied linkage.
func TestIntegration_SidechainStampsParentSpanID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	resultEscaped := `{\"results\":[{\"path\":\"learnings/general/2026-05-12_x.md\"}]}`
	jsonl := makeSession("sess-Sub", "prompt-Sub", resultEscaped)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	parent := "span-parent-1"
	if _, err := ProcessFile(context.Background(), pool, path, "mcp-servers", parent, false); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	var got sql.NullString
	if err := pool.DB().QueryRow(`SELECT parent_span_id FROM grounding_events WHERE session_id='sess-Sub'`).Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !got.Valid || got.String != parent {
		t.Errorf("parent_span_id = %v, want %s", got, parent)
	}
}

// Re-run idempotency: processing the same JSONL twice does not duplicate
// query_resolutions rows nor crash on the UNIQUE (entity_kind,
// entity_slug, entity_project_id, prompt_id) constraint.
func TestIntegration_ReRunIdempotent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	resultEscaped := `{\"results\":[{\"path\":\"learnings/general/2026-05-12_x.md\"}]}`
	jsonl := makeSession("sess-R", "prompt-R", resultEscaped) +
		`{"type":"assistant","sessionId":"sess-R","promptId":"prompt-R","message":{"content":[{"type":"tool_use","id":"toolu_close","name":"mcp__toolkit-server__work","input":{"action":"chain_close","project":"mcp-servers","params":{"slug":"some-chain","closure_summary":"Closed via 2026-05-12_x notes."}}}]}}` + "\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := ProcessFile(context.Background(), pool, path, "mcp-servers", "", false); err != nil {
			t.Fatalf("run %d: processFile: %v", i, err)
		}
	}
	var n int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM query_resolutions WHERE entity_slug='some-chain'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("query_resolutions count after 2 runs = %d, want 1", n)
	}
}

// TestPreserveTranscriptTimestamps verifies the --preserve-transcript-timestamps
// flag threads each tool_use's transcript timestamp through to the
// grounding_events row's created_at. Without the flag, the column gets
// datetime('now'). The repair use case is bug
// click-detection-stop-hook-unwired's pre-fix gap — running the processor
// against historical transcripts needs to stamp rows with original call
// times so the /inference/health-cards proximity-join can find them.
func TestPreserveTranscriptTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preserved.jsonl")
	// One assistant tool_use with an explicit transcript timestamp two
	// days ago — far enough from "now" that any drift makes the
	// preserved-vs-default distinction unambiguous in the assertion.
	transcriptTS := "2026-05-17T15:30:00.000Z"
	bodyJSON := `{\"results\":[{\"path\":\"decisions/foo.md\"}]}`
	jsonl := `{"type":"user","sessionId":"sess-T","promptId":"prompt-T","message":{"content":"q"}}` + "\n" +
		`{"type":"assistant","sessionId":"sess-T","promptId":"prompt-T","timestamp":"` + transcriptTS + `","message":{"content":[{"type":"tool_use","id":"toolu_search","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_search","params":{"query":"q"}}}]}}` + "\n" +
		`{"type":"user","sessionId":"sess-T","promptId":"prompt-T","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[{"type":"text","text":"` + bodyJSON + `"}]}]}}` + "\n"
	if err := os.WriteFile(path, []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	pool := testutil.NewTestDB(t)
	if _, err := ProcessFile(context.Background(), pool, path, "mcp-servers", "", true); err != nil {
		t.Fatalf("processFile preserveTimes=true: %v", err)
	}
	var createdAt string
	if err := pool.DB().QueryRow(
		`SELECT created_at FROM grounding_events WHERE call_id='toolu_search'`,
	).Scan(&createdAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if createdAt != "2026-05-17 15:30:00" {
		t.Errorf("created_at = %q, want 2026-05-17 15:30:00 (preserved from transcript timestamp)", createdAt)
	}
}

// TestPreserveTranscriptTimestamps_OffByDefault confirms that without
// the flag, created_at falls back to datetime('now'). Anchors the
// invariant that normal Stop-hook-on-session-end mode keeps its
// existing behavior.
func TestPreserveTranscriptTimestamps_OffByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "default.jsonl")
	// Same transcript shape, two-day-old timestamp.
	transcriptTS := "2026-05-17T15:30:00.000Z"
	bodyJSON := `{\"results\":[{\"path\":\"decisions/foo.md\"}]}`
	jsonl := `{"type":"user","sessionId":"sess-D","promptId":"prompt-D","message":{"content":"q"}}` + "\n" +
		`{"type":"assistant","sessionId":"sess-D","promptId":"prompt-D","timestamp":"` + transcriptTS + `","message":{"content":[{"type":"tool_use","id":"toolu_search","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_search","params":{"query":"q"}}}]}}` + "\n" +
		`{"type":"user","sessionId":"sess-D","promptId":"prompt-D","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[{"type":"text","text":"` + bodyJSON + `"}]}]}}` + "\n"
	if err := os.WriteFile(path, []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	pool := testutil.NewTestDB(t)
	if _, err := ProcessFile(context.Background(), pool, path, "mcp-servers", "", false); err != nil {
		t.Fatalf("processFile preserveTimes=false: %v", err)
	}
	var createdAt string
	if err := pool.DB().QueryRow(
		`SELECT created_at FROM grounding_events WHERE call_id='toolu_search'`,
	).Scan(&createdAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	// Without preservation, created_at should be "now"-ish (datetime
	// format, not the literal 2026-05-17 transcript value).
	if createdAt == "2026-05-17 15:30:00" {
		t.Errorf("created_at picked up the transcript timestamp without the flag — leak: %q", createdAt)
	}
}

// makeSession composes a three-record JSONL prefix: user prompt →
// assistant vault_search tool_use → user tool_result. resultEscaped is
// the tool_result's text payload (pre-escaped JSON string content).
func makeSession(sessID, promptID, resultEscaped string) string {
	tpl := `{"type":"user","sessionId":"__S__","promptId":"__P__","message":{"content":"please help"}}
{"type":"assistant","sessionId":"__S__","promptId":"__P__","message":{"content":[{"type":"tool_use","id":"toolu_search","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_search","params":{"query":"floor-char boundary"}}}]}}
{"type":"user","sessionId":"__S__","promptId":"__P__","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[{"type":"text","text":"__BODY__"}]}]}}
`
	out := strings.ReplaceAll(tpl, "__S__", sessID)
	out = strings.ReplaceAll(out, "__P__", promptID)
	out = strings.ReplaceAll(out, "__BODY__", resultEscaped)
	return out
}

func assistantTextLine(sessID, promptID, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"assistant","sessionId":"` + sessID + `","promptId":"` + promptID +
		`","message":{"content":[{"type":"text","text":` + string(b) + `}]}}` + "\n"
}
