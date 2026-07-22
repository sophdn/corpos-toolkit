package grounding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureJSONL builds a minimal session transcript with one
// vault_search → tool_result → assistant-text turn. Returns the path
// to a temp file the test cleans up via t.Cleanup.
func fixtureJSONL(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

const vaultSearchSession = `{"type":"user","sessionId":"sess-X","message":{"content":"start"}}
{"type":"assistant","sessionId":"sess-X","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_search","params":{"query":"foo"}}}]}}
{"type":"user","sessionId":"sess-X","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"{\"results\":[{\"path\":\"learnings/general/2026-05-12_floor-char-boundary-for-safe-string-truncation.md\"},{\"path\":\"reference/rag-architecture.md\"}]}"}]}]}}
{"type":"assistant","sessionId":"sess-X","message":{"content":[{"type":"text","text":"Reading 2026-05-12_floor-char-boundary-for-safe-string-truncation to understand the pattern."}]}}
`

func TestProcessSession_VaultSearchUsedTrue(t *testing.T) {
	path := fixtureJSONL(t, vaultSearchSession)
	events, _, err := processSession(path)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.SessionID != "sess-X" || ev.CallID != "toolu_1" || ev.Action != "vault_search" {
		t.Errorf("envelope mismatch: %+v", ev)
	}
	if ev.ResultsCount != 2 {
		t.Errorf("results_count = %d, want 2", ev.ResultsCount)
	}
	if len(ev.SourceRefs) != 2 ||
		ev.SourceRefs[0] != "learnings/general/2026-05-12_floor-char-boundary-for-safe-string-truncation.md" {
		t.Errorf("source_refs = %v", ev.SourceRefs)
	}
	if !ev.NextTurnHasOutput {
		t.Errorf("next_turn_has_output = false, want true")
	}
	if ev.Used == nil || !*ev.Used {
		t.Errorf("used = %v, want true (slug substring matches assistant text)", ev.Used)
	}
	// Bug `reranker-projection-drops-query-text-on-positive-labels`: the
	// processor must carry query_text through so processor-created
	// grounding_events (the source of ALL positive-labeled training
	// rows) are not NULL-query_text.
	if ev.QueryText != "foo" {
		t.Errorf("query_text = %q, want %q", ev.QueryText, "foo")
	}
}

// Regression for bug `reranker-projection-drops-query-text-on-positive-
// labels`: extractQueryText pulls the query out of the tool_use input
// with the per-action key (query for vault/knowledge, pattern for
// kiwix). A processor row that lands without query_text is the exact
// shape that made the cross-encoder corpus untrainable (all positives
// are processor rows).
func TestExtractQueryText(t *testing.T) {
	cases := []struct {
		action string
		params string
		want   string
	}{
		{"vault_search", `{"query":"floor-char boundary"}`, "floor-char boundary"},
		{"knowledge_search", `{"query":"rag architecture"}`, "rag architecture"},
		{"kiwix_search", `{"zim_id":"rust","pattern":"trait objects"}`, "trait objects"},
		{"vault_search", `{}`, ""},
		{"vault_search", ``, ""},
		{"kiwix_search", `{"query":"wrong-key-for-kiwix"}`, ""}, // kiwix uses pattern, not query
	}
	for _, c := range cases {
		if got := extractQueryText(c.action, []byte(c.params)); got != c.want {
			t.Errorf("extractQueryText(%q, %q) = %q, want %q", c.action, c.params, got, c.want)
		}
	}
}

const vaultSearchUsedFalseSession = `{"type":"user","sessionId":"sess-Y","message":{"content":"start"}}
{"type":"assistant","sessionId":"sess-Y","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_search","params":{"query":"bar"}}}]}}
{"type":"user","sessionId":"sess-Y","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"{\"results\":[{\"path\":\"learnings/general/zzz.md\"}]}"}]}]}}
{"type":"assistant","sessionId":"sess-Y","message":{"content":[{"type":"text","text":"That doesn't help; let me try something else."}]}}
`

func TestProcessSession_VaultSearchUsedFalse(t *testing.T) {
	events, _, err := processSession(fixtureJSONL(t, vaultSearchUsedFalseSession))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Used == nil || *ev.Used {
		t.Errorf("used = %v, want false (zzz slug not in assistant text)", ev.Used)
	}
}

const zeroResultGapSession = `{"type":"user","sessionId":"sess-Z","message":{"content":"start"}}
{"type":"assistant","sessionId":"sess-Z","message":{"content":[{"type":"tool_use","id":"toolu_3","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_search","params":{"query":"empty"}}}]}}
{"type":"user","sessionId":"sess-Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_3","content":[{"type":"text","text":"{\"results\":[]}"}]}]}}
{"type":"assistant","sessionId":"sess-Z","message":{"content":[{"type":"text","text":"Nothing in the vault; I'll proceed without it."}]}}
`

func TestProcessSession_ZeroResultGap(t *testing.T) {
	events, _, err := processSession(fixtureJSONL(t, zeroResultGapSession))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.ResultsCount != 0 {
		t.Errorf("results_count = %d, want 0", ev.ResultsCount)
	}
	if !ev.NextTurnHasOutput {
		t.Errorf("next_turn_has_output = false, want true (zero-result gap)")
	}
}

const kiwixSearchSession = `{"type":"user","sessionId":"sess-K","message":{"content":"start"}}
{"type":"assistant","sessionId":"sess-K","message":{"content":[{"type":"tool_use","id":"toolu_K","name":"mcp__toolkit-server__knowledge","input":{"action":"kiwix_search","params":{"zim_id":"devdocs_en_rust_2026-04","pattern":"trait objects"}}}]}}
{"type":"user","sessionId":"sess-K","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_K","content":[{"type":"text","text":"{\"hits\":[{\"article_ref\":{\"zim_id\":\"devdocs_en_rust_2026-04\",\"slug\":\"book/ch17-02-trait-objects\"}}]}"}]}]}}
{"type":"assistant","sessionId":"sess-K","message":{"content":[{"type":"text","text":"Wrap-up."}]}}
`

func TestProcessSession_KiwixSearchSourceRefShape(t *testing.T) {
	events, _, err := processSession(fixtureJSONL(t, kiwixSearchSession))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	want := "devdocs_en_rust_2026-04::book/ch17-02-trait-objects"
	if len(events[0].SourceRefs) != 1 || events[0].SourceRefs[0] != want {
		t.Errorf("source_refs = %v, want [%s]", events[0].SourceRefs, want)
	}
}

const knowledgeSearchSession = `{"type":"user","sessionId":"sess-N","message":{"content":"start"}}
{"type":"assistant","sessionId":"sess-N","message":{"content":[{"type":"tool_use","id":"toolu_N","name":"mcp__toolkit-server__knowledge","input":{"action":"knowledge_search","params":{"query":"caddy reverse proxy"}}}]}}
{"type":"user","sessionId":"sess-N","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_N","content":[{"type":"text","text":"[{\"source_ref\":\"vault/decisions/2026-05-07_caddy-flush-interval-for-sse.md\"},{\"source_ref\":\"learnings/general/2026-05-08_caddy-flush-interval-for-sse.md\"}]"}]}]}}
{"type":"assistant","sessionId":"sess-N","message":{"content":[{"type":"text","text":"Both notes describe the same issue."}]}}
`

func TestProcessSession_KnowledgeSearchSourceRefShape(t *testing.T) {
	events, _, err := processSession(fixtureJSONL(t, knowledgeSearchSession))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if len(events[0].SourceRefs) != 2 {
		t.Errorf("want 2 source_refs, got %d: %v", len(events[0].SourceRefs), events[0].SourceRefs)
	}
}

func TestProcessSession_SkipNonKnowledgeToolUse(t *testing.T) {
	body := `{"type":"assistant","sessionId":"sess-A","message":{"content":[{"type":"tool_use","id":"toolu_irrelevant","name":"Bash","input":{"command":"ls"}}]}}
`
	events, _, err := processSession(fixtureJSONL(t, body))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("want 0 events (Bash is not a knowledge tool), got %d", len(events))
	}
}

func TestProcessSession_SkipUntrackedKnowledgeAction(t *testing.T) {
	body := `{"type":"assistant","sessionId":"sess-A","message":{"content":[{"type":"tool_use","id":"toolu_read","name":"mcp__toolkit-server__knowledge","input":{"action":"vault_read","params":{"path":"vault/foo.md"}}}]}}
`
	events, _, err := processSession(fixtureJSONL(t, body))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("want 0 events (vault_read is not tracked), got %d", len(events))
	}
}

// TestCheckUsed_PathStem pins the Rust-prototype-equivalent behavior:
// the needle is the FULL filename stem (date prefix preserved), .md
// stripped. The query-telemetry-substrate TT1.5 spike (§7.1) documents
// that real assistant text more often uses the slug-form WITHOUT the
// date prefix; that's the new `mentioned` tier's concern, not the
// legacy `used` bit this binary writes. Bit-identical is the contract.
func TestCheckUsed_PathStem(t *testing.T) {
	refs := []string{"learnings/general/2026-05-12_floor-char-boundary-for-safe-string-truncation.md"}
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"full stem match", "see 2026-05-12_floor-char-boundary-for-safe-string-truncation for context", true},
		{"case-insensitive full stem", "SEE 2026-05-12_FLOOR-CHAR-BOUNDARY-FOR-SAFE-STRING-TRUNCATION", true},
		{"slug-only does NOT match (legacy used-bit behavior)", "checking floor-char-boundary-for-safe-string-truncation now", false},
		{"no match", "totally unrelated text", false},
		{"empty text", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkUsed(refs, tc.text)
			if got != tc.want {
				t.Errorf("checkUsed(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestExtractSourceRefs_VaultSearch(t *testing.T) {
	count, refs := extractSourceRefs("vault_search",
		`{"results":[{"path":"a.md"},{"path":"b.md"}]}`)
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if strings.Join(refs, ",") != "a.md,b.md" {
		t.Errorf("refs = %v", refs)
	}
}

func TestExtractSourceRefs_UnknownAction(t *testing.T) {
	count, refs := extractSourceRefs("zzz", `{"results":[{"path":"a.md"}]}`)
	if count != 0 || refs != nil {
		t.Errorf("unknown action should yield (0, nil), got (%d, %v)", count, refs)
	}
}

func TestInferProjectID(t *testing.T) {
	cases := []struct {
		dir  string
		want string
	}{
		{"-home-sophi-dev-mcp-servers", "mcp-servers"},
		{"-home-sophi-dev-seed-packet", "seed-packet"},
		{"-home-sophi", "sophi"},
		{"", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			got := InferProjectID(tc.dir)
			if got != tc.want {
				t.Errorf("InferProjectID(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		})
	}
}
