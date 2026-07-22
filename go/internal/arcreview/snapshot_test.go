package arcreview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func userRow(text string) string {
	row := map[string]string{"role": "user", "content": text}
	b, _ := json.Marshal(row)
	return string(b)
}

func assistantArrayRow(parts ...string) string {
	contentParts := make([]map[string]string, len(parts))
	for i, p := range parts {
		contentParts[i] = map[string]string{"type": "text", "text": p}
	}
	row := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": contentParts,
		},
	}
	b, _ := json.Marshal(row)
	return string(b)
}

func TestExtractSnapshot_EmptyTranscript(t *testing.T) {
	path := writeTranscript(t, nil)
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("empty transcript should not error, got %v", err)
	}
	if len(snap.Messages) != 0 {
		t.Fatalf("empty transcript should yield 0 messages, got %d", len(snap.Messages))
	}
	if snap.Truncated {
		t.Fatalf("empty transcript should not be marked truncated")
	}
}

func TestExtractSnapshot_OnlyAssistantTranscript(t *testing.T) {
	path := writeTranscript(t, []string{
		assistantArrayRow("first assistant turn"),
		assistantArrayRow("second assistant turn"),
	})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("only-assistant transcript should not error, got %v", err)
	}
	if len(snap.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(snap.Messages))
	}
	for _, m := range snap.Messages {
		if m.Role != "assistant" {
			t.Fatalf("expected only assistant rows, got role=%q", m.Role)
		}
	}
}

func TestExtractSnapshot_MixedContentShapes(t *testing.T) {
	path := writeTranscript(t, []string{
		userRow("hello"),
		assistantArrayRow("hi there", "second part"),
		userRow("more"),
	})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("mixed-content transcript should not error, got %v", err)
	}
	if len(snap.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(snap.Messages))
	}
	if snap.Messages[0].Content != "hello" {
		t.Fatalf("expected first content 'hello', got %q", snap.Messages[0].Content)
	}
	// Post-snapshot-extractor-fix: text parts join with "\n\n" instead
	// of " " so role-prefixed lines (when present in tool_use /
	// tool_result rendering) stay visually distinct.
	if snap.Messages[1].Content != "hi there\n\nsecond part" {
		t.Fatalf("expected joined assistant content, got %q", snap.Messages[1].Content)
	}
}

func TestExtractSnapshot_TurnCapTruncatesOldest(t *testing.T) {
	lines := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		lines = append(lines, userRow("turn-"+string(rune('a'+i%26))))
	}
	path := writeTranscript(t, lines)
	snap, err := ExtractSnapshot(path, 5, 4000)
	if err != nil {
		t.Fatalf("turn-cap should not error, got %v", err)
	}
	if len(snap.Messages) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(snap.Messages))
	}
	if !snap.Truncated {
		t.Fatalf("expected truncated=true when capping turns")
	}
}

func TestExtractSnapshot_TokenCapTruncatesOldest(t *testing.T) {
	long := strings.Repeat("x", 4000) // ~1000 tokens per row
	lines := []string{
		userRow(long),
		userRow(long),
		userRow(long),
		userRow(long),
		userRow("recent and small"),
	}
	path := writeTranscript(t, lines)
	// 2000-token budget should keep the small final row plus at most
	// one of the large rows (4000 chars ≈ 1000 tokens).
	snap, err := ExtractSnapshot(path, 20, 2000)
	if err != nil {
		t.Fatalf("token-cap should not error, got %v", err)
	}
	if len(snap.Messages) == 0 {
		t.Fatalf("expected at least one message (the final small one)")
	}
	if !snap.Truncated {
		t.Fatalf("expected truncated=true when token cap binds")
	}
	// Final kept row must be the most-recent.
	if snap.Messages[len(snap.Messages)-1].Content != "recent and small" {
		t.Fatalf("expected newest row preserved, got tail=%q",
			snap.Messages[len(snap.Messages)-1].Content)
	}
}

func TestExtractSnapshot_OversizedSingleTurnStillKept(t *testing.T) {
	// A single oversize final turn must still land — otherwise the
	// snapshot would be empty and the review would have nothing to
	// look at.
	big := strings.Repeat("y", 200000) // 50K tokens; budget 4000
	path := writeTranscript(t, []string{userRow(big)})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("oversized single turn should not error, got %v", err)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("expected 1 message preserved, got %d", len(snap.Messages))
	}
}

func TestExtractSnapshot_MalformedRowsSkipped(t *testing.T) {
	path := writeTranscript(t, []string{
		userRow("first"),
		"not json at all",
		assistantArrayRow("response"),
		"{}", // role missing → skipped
		userRow("last"),
	})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("malformed rows should not error, got %v", err)
	}
	if len(snap.Messages) != 3 {
		t.Fatalf("expected 3 kept messages, got %d (%v)", len(snap.Messages), snap.Messages)
	}
}

func TestExtractSnapshot_FileMissing(t *testing.T) {
	_, err := ExtractSnapshot("/nonexistent/transcript.jsonl", 20, 4000)
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
}

// toolUseRow builds an assistant transcript row with a single tool_use
// content part. Used to verify the post-fix extractContent preserves
// the tool name + input.
func toolUseRow(name string, input map[string]string) string {
	row := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"name":  name,
					"input": input,
				},
			},
		},
	}
	b, _ := json.Marshal(row)
	return string(b)
}

// toolResultRow builds a user transcript row with a single tool_result
// content part. Claude Code stores tool_result entries on user-role
// rows historically.
func toolResultRow(content string) string {
	row := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{
					"type":    "tool_result",
					"content": content,
				},
			},
		},
	}
	b, _ := json.Marshal(row)
	return string(b)
}

// thinkingRow builds an assistant row carrying a single thinking
// block. Used to verify post-fix extractContent surfaces thinking
// content rather than dropping it.
func thinkingRow(text string) string {
	row := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type":     "thinking",
					"thinking": text,
				},
			},
		},
	}
	b, _ := json.Marshal(row)
	return string(b)
}

func TestExtractSnapshot_RetainsToolUseWithNameAndInput(t *testing.T) {
	path := writeTranscript(t, []string{
		toolUseRow("forge", map[string]string{
			"schema_name": "bug",
			"slug":        "test-bug",
			"title":       "stack frame",
		}),
	})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap.Messages))
	}
	got := snap.Messages[0].Content
	if !strings.Contains(got, "[tool_use: forge]") {
		t.Errorf("expected prefix '[tool_use: forge]' in content, got %q", got)
	}
	if !strings.Contains(got, "\"slug\"") {
		t.Errorf("expected tool_use input fields to survive, got %q", got)
	}
}

func TestExtractSnapshot_RetainsToolResultContent(t *testing.T) {
	path := writeTranscript(t, []string{
		toolResultRow("Migration script wrote 3 rows; final exit 0."),
	})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap.Messages))
	}
	got := snap.Messages[0].Content
	if !strings.Contains(got, "[tool_result]") {
		t.Errorf("expected '[tool_result]' prefix in content, got %q", got)
	}
	if !strings.Contains(got, "Migration script wrote 3 rows") {
		t.Errorf("expected tool_result body to survive, got %q", got)
	}
}

func TestExtractSnapshot_RetainsThinkingBlocks(t *testing.T) {
	path := writeTranscript(t, []string{
		thinkingRow("Let me trace the dispatch path before editing — first check what jsonResult returns."),
	})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap.Messages))
	}
	got := snap.Messages[0].Content
	if !strings.Contains(got, "[thinking]") {
		t.Errorf("expected '[thinking]' prefix in content, got %q", got)
	}
	if !strings.Contains(got, "trace the dispatch path") {
		t.Errorf("expected thinking body to survive, got %q", got)
	}
}

func TestExtractSnapshot_MixedToolAndTextPartsAllSurvive(t *testing.T) {
	// One row mixing text + tool_use + tool_result — the kind of shape
	// Claude Code emits when an assistant turn includes both prose and
	// a tool call. The post-fix extractor should surface every part
	// with its prefix.
	row := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "I'll forge a bug for this."},
				{"type": "tool_use", "name": "forge",
					"input": map[string]string{"schema_name": "bug", "slug": "x"}},
			},
		},
	}
	b, _ := json.Marshal(row)
	path := writeTranscript(t, []string{string(b)})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap.Messages))
	}
	got := snap.Messages[0].Content
	if !strings.Contains(got, "I'll forge a bug for this.") {
		t.Errorf("text part lost: %q", got)
	}
	if !strings.Contains(got, "[tool_use: forge]") {
		t.Errorf("tool_use part lost: %q", got)
	}
}

func TestExtractSnapshot_TruncatesLongToolResultWithHeadTailMarker(t *testing.T) {
	// Build a tool_result content larger than the truncation budget
	// (head 4000 + tail 500 = 4500 chars total before truncation kicks
	// in). Verify the rendered output preserves the start, has a
	// truncation marker, and preserves the end.
	body := strings.Repeat("A", 6000) + "ENDMARK"
	path := writeTranscript(t, []string{toolResultRow(body)})
	snap, err := ExtractSnapshot(path, 20, 99999)
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	got := snap.Messages[0].Content
	if !strings.Contains(got, "...[truncated") {
		t.Errorf("expected truncation marker in content, got len=%d", len(got))
	}
	if !strings.HasPrefix(got, "[tool_result] AAA") {
		t.Errorf("expected head 'AAA' preserved, got prefix %q", got[:50])
	}
	if !strings.HasSuffix(got, "ENDMARK") {
		t.Errorf("expected tail 'ENDMARK' preserved, got suffix %q", got[len(got)-20:])
	}
}

func TestExtractSnapshot_UnknownPartTypeSkipped(t *testing.T) {
	row := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "future_unknown", "text": "should be ignored"},
				{"type": "text", "text": "kept"},
			},
		},
	}
	b, _ := json.Marshal(row)
	path := writeTranscript(t, []string{string(b)})
	snap, err := ExtractSnapshot(path, 20, 4000)
	if err != nil {
		t.Fatalf("ExtractSnapshot: %v", err)
	}
	got := snap.Messages[0].Content
	if strings.Contains(got, "should be ignored") {
		t.Errorf("unknown-type content leaked into snapshot: %q", got)
	}
	if !strings.Contains(got, "kept") {
		t.Errorf("known-type content was lost alongside the unknown: %q", got)
	}
}
