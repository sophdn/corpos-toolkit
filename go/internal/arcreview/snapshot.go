package arcreview

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// DefaultMaxTurns is the snapshot turn cap per design Q1 (N=20).
const DefaultMaxTurns = 20

// DefaultMaxTokens is the snapshot token cap per design Q1 (M=4000).
const DefaultMaxTokens = 4000

// charsPerTokenEstimate is the rough char→token ratio. The vault
// rerank path uses the same heuristic for prompt-size budgeting; we
// don't pull a real tokenizer into this package because the cap is a
// soft budget — Qwen will silently truncate any overshoot.
const charsPerTokenEstimate = 4

// Message is one transcript entry retained in a snapshot. Role is
// "user" or "assistant"; Content is the role's text (string-content
// shapes and array-of-text-parts shapes both collapse to a single
// joined string at parse time). The transcript's tool-use rows and
// system events are filtered out before this struct is constructed.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Snapshot is the conversation window passed into the Qwen review.
// EstimatedTokens is the char/4 approximation (see charsPerTokenEstimate);
// Truncated is true when the transcript was longer than the budget
// allowed and earlier turns were dropped.
type Snapshot struct {
	Messages        []Message `json:"messages"`
	EstimatedTokens int       `json:"estimated_tokens"`
	Truncated       bool      `json:"truncated"`
}

// transcriptRow is the shape of one Claude Code .jsonl row that the
// snapshot extractor cares about. Real transcripts carry more fields
// (uuid, parent_uuid, timestamp, etc.); we ignore them. Content is
// json.RawMessage because it's polymorphic — sometimes a bare string,
// sometimes an array of {type, text} parts. extractContent does the
// decode.
type transcriptRow struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Message *transcriptRow  `json:"message,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// Timestamp is the row's top-level ISO-8601 time (RFC3339, e.g.
	// "2026-05-24T00:11:01.410Z"). Present on every user/assistant row;
	// used only by the as-of point-in-time reconstruction path
	// (ExtractSnapshotAsOf) — the live ExtractSnapshot ignores it.
	Timestamp string `json:"timestamp,omitempty"`
}

// contentPart is one entry of the array-shaped content field. Claude
// Code mixes several shapes in the same array — text / thinking /
// tool_use / tool_result — and historically extractContent only kept
// text. Per the snapshot-extractor-drops-tool-use bug + the chosen
// fix shape, we now keep every shape with role-aware prefixes and
// per-part truncation (constants below).
//
// Fields are a union: only the subset relevant to each Type is
// populated by the underlying transcript. json.Unmarshal silently
// drops unknown keys; absent keys land as zero values; we read
// per-type in renderPart.
type contentPart struct {
	Type string `json:"type"`
	// type=text / thinking
	Text string `json:"text"`
	// type=text-with-thinking shape (some clients use `thinking` instead
	// of `text` for thinking blocks; cover both).
	Thinking string `json:"thinking"`
	// type=tool_use
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// type=tool_result — content can itself be string or array of
	// {type:"text",text} parts (mirrors the outer content shape).
	Content json.RawMessage `json:"content"`
}

// Per-part truncation budgets (chars). Picked from the
// context-compression pattern in ~/dev/mining/hermes-agent's
// context_compressor.py, scaled down because arcreview's overall
// 4000-token snapshot budget bites earlier than hermes's much larger
// context window. Head + tail truncation preserves both the start and
// end of long blocks so the closing-shape signals (rationale strings
// at the end of forge payloads; final lines of tool_result stdout
// blocks) reach Qwen.
const (
	toolUseInputHeadChars  = 1200
	toolUseInputTailChars  = 200
	toolResultHeadChars    = 4000
	toolResultTailChars    = 500
	thinkingHeadChars      = 1500
	thinkingTailChars      = 500
	truncationMarkerFormat = "...[truncated %d chars]..."
)

// renderPart renders one content part into the snapshot's text
// representation per the role-aware-prefix scheme. Returns "" for
// shapes we can't read (forward-compat with future content types
// Claude Code may add). Per the chosen fix shape on bug
// arcreview-snapshot-extractor-drops-tool-use-and-tool-result-content,
// every known shape is preserved with a prefix that names what kind
// of work the agent did so Qwen can ground its review in the actual
// tool calls / results / reasoning rather than only the agent's
// user-facing narration.
func renderPart(p contentPart) string {
	switch p.Type {
	case "text":
		// No prefix; text parts are the user-facing narration that
		// the original extractContent already surfaced.
		return p.Text
	case "thinking":
		body := p.Thinking
		if body == "" {
			// Some clients emit thinking content in the text field.
			body = p.Text
		}
		body = truncateHeadTail(body, thinkingHeadChars, thinkingTailChars)
		if body == "" {
			return ""
		}
		return "[thinking] " + body
	case "tool_use":
		name := p.Name
		if name == "" {
			name = "?"
		}
		input := string(p.Input)
		input = truncateHeadTail(input, toolUseInputHeadChars, toolUseInputTailChars)
		return "[tool_use: " + name + "] " + input
	case "tool_result":
		// content here is itself polymorphic — string or array of
		// text-parts. Reuse the same flatten logic the outer
		// extractContent uses for top-level array content.
		body, _ := extractTextFromPolymorphicContent(p.Content)
		body = truncateHeadTail(body, toolResultHeadChars, toolResultTailChars)
		if body == "" {
			return ""
		}
		return "[tool_result] " + body
	default:
		// Forward-compat: unknown part type — skip rather than fail.
		return ""
	}
}

// truncateHeadTail returns s when len(s) <= head+tail; otherwise
// emits the first head chars + a truncation marker + the last tail
// chars. The marker names the dropped char count so callers reading
// the snapshot can tell something was cut.
func truncateHeadTail(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	dropped := len(s) - head - tail
	return s[:head] + fmt.Sprintf(truncationMarkerFormat, dropped) + s[len(s)-tail:]
}

// extractTextFromPolymorphicContent collapses a content field that
// may be a JSON string OR an array of {type,text} parts into a
// single joined string. Mirrors the top-level array-of-text-parts
// logic in extractContent; pulled out so tool_result rendering can
// reuse it.
func extractTextFromPolymorphicContent(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) == 0 {
			return "", false
		}
		return strings.Join(texts, " "), true
	}
	return "", false
}

// ExtractSnapshot reads a Claude Code transcript at transcriptPath,
// walks from end to start, keeps user/assistant rows in conversation
// order, and stops when either maxTurns or maxTokens would be
// exceeded. Returns the snapshot in oldest→newest message order so the
// Qwen prompt reads forward.
//
// Edge cases (per task acceptance §(c)):
//   - Empty transcript → Snapshot{} with no messages, not an error.
//   - Only-assistant transcript → returns the kept assistant turns;
//     no user-turn requirement at this layer.
//   - Mixed content shapes — string content survives as-is; array
//     content collapses to the joined text-part text fields with a
//     single space separator.
//   - File missing / unreadable → returns a wrapped error so the
//     caller can fail-open per design §Failure-modes.
//
// maxTurns ≤ 0 falls back to DefaultMaxTurns; maxTokens ≤ 0 falls
// back to DefaultMaxTokens. Clamping happens in the caller's typed
// surface, but ExtractSnapshot is defensive so direct test calls
// don't surprise.
func ExtractSnapshot(transcriptPath string, maxTurns, maxTokens int) (Snapshot, error) {
	rows, err := scanTranscriptRows(transcriptPath)
	if err != nil {
		return Snapshot{}, err
	}
	msgs := make([]Message, len(rows))
	for i := range rows {
		msgs[i] = rows[i].msg
	}
	return truncateToBudget(msgs, maxTurns, maxTokens), nil
}

// ExtractSnapshotAsOf reconstructs the snapshot as the review would have
// seen it AT a past point in time: it keeps only transcript rows whose
// timestamp is <= asOf, then applies the identical turn/token truncation
// as ExtractSnapshot. This is the point-in-time path for the historical-
// recovery cmd (chain arc-close-snapshot-corpus-capture T4) — a session's
// final transcript contains turns from AFTER a given fire, so a plain
// ExtractSnapshot would not reproduce what that fire's review actually saw
// (load-bearing for multi-fire sessions: 261/265 fires).
//
// Rows whose timestamp can't be parsed are DROPPED in this mode (they
// can't be placed in time, and a point-in-time cut must not risk
// including a post-fire turn). In practice every user/assistant transcript
// row carries a timestamp, so this is a defensive no-op.
func ExtractSnapshotAsOf(transcriptPath string, maxTurns, maxTokens int, asOf time.Time) (Snapshot, error) {
	rows, err := scanTranscriptRows(transcriptPath)
	if err != nil {
		return Snapshot{}, err
	}
	msgs := make([]Message, 0, len(rows))
	for i := range rows {
		if rows[i].ts.IsZero() || rows[i].ts.After(asOf) {
			continue
		}
		msgs = append(msgs, rows[i].msg)
	}
	return truncateToBudget(msgs, maxTurns, maxTokens), nil
}

// timedMessage pairs a kept user/assistant Message with its parsed row
// timestamp (zero when the row carried no parseable timestamp).
type timedMessage struct {
	msg Message
	ts  time.Time
}

// scanTranscriptRows is the shared first pass for ExtractSnapshot and
// ExtractSnapshotAsOf: open the transcript, keep every non-empty
// user/assistant row in source order, and parse each row's timestamp.
// Transcripts are bounded in size (Claude Code rotates them per session);
// a single in-memory pass is cheap.
func scanTranscriptRows(transcriptPath string) ([]timedMessage, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("open transcript %s: %w", transcriptPath, err)
	}
	defer func() { _ = f.Close() }()

	var rows []timedMessage
	scanner := bufio.NewScanner(f)
	// 1 MiB line buffer — bumped past bufio's default 64 KiB because a
	// single assistant turn (with embedded tool-result blocks) can run
	// long. Real Claude Code transcripts have been observed up to
	// ~500 KiB per row in pathological cases.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row transcriptRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			// Drift-tolerant: skip malformed rows rather than abort.
			// Real transcripts have occasionally surfaced ragged tail
			// rows during session interrupt.
			continue
		}
		role, content := flattenRow(row)
		if role != "user" && role != "assistant" {
			continue
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		var ts time.Time
		if row.Timestamp != "" {
			if parsed, perr := time.Parse(time.RFC3339, row.Timestamp); perr == nil {
				ts = parsed
			}
		}
		rows = append(rows, timedMessage{msg: Message{Role: role, Content: content}, ts: ts})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan transcript: %w", err)
	}
	return rows, nil
}

// truncateToBudget applies the snapshot turn/token budget to messages in
// source (oldest→newest) order: walk from the end, keep rows while under
// both budgets, then restore oldest→newest order. Shared by
// ExtractSnapshot and ExtractSnapshotAsOf so the live and recovered paths
// truncate identically (faithful recovery). maxTurns/maxTokens <= 0 fall
// back to the defaults; empty input returns the zero Snapshot.
func truncateToBudget(rows []Message, maxTurns, maxTokens int) Snapshot {
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	if len(rows) == 0 {
		return Snapshot{}
	}
	keep := make([]Message, 0, maxTurns)
	tokens := 0
	truncated := false
	for i := len(rows) - 1; i >= 0; i-- {
		if len(keep) >= maxTurns {
			truncated = true
			break
		}
		rowTokens := estimateTokens(rows[i].Content)
		if tokens+rowTokens > maxTokens && len(keep) > 0 {
			// Keep the first row (newest) even if it exceeds the
			// budget alone — otherwise an oversized final turn would
			// produce an empty snapshot. Subsequent rows only land
			// when they fit.
			truncated = true
			break
		}
		keep = append(keep, rows[i])
		tokens += rowTokens
	}
	if len(rows) > len(keep) {
		truncated = true
	}

	// Reverse keep into oldest→newest order for the Qwen prompt.
	for l, r := 0, len(keep)-1; l < r; l, r = l+1, r-1 {
		keep[l], keep[r] = keep[r], keep[l]
	}
	return Snapshot{
		Messages:        keep,
		EstimatedTokens: tokens,
		Truncated:       truncated,
	}
}

// flattenRow extracts (role, content) from a transcript row, handling
// the two real-world shapes Claude Code emits:
//
//  1. Top-level {"role": "...", "content": "..."} on user-turn rows.
//  2. Nested {"type": "...", "message": {"role": "...", "content": [...]}}
//     on the standard SDK message rows.
//
// Returns ("", "") for rows that don't match either shape.
func flattenRow(row transcriptRow) (string, string) {
	role := row.Role
	rawContent := row.Content
	if role == "" && row.Message != nil {
		role = row.Message.Role
		rawContent = row.Message.Content
	}
	if role == "" {
		return "", ""
	}
	content, ok := extractContent(rawContent)
	if !ok {
		return "", ""
	}
	return role, content
}

// extractContent decodes a content field that may be either a JSON
// string or an array of mixed-shape parts (text / thinking / tool_use
// / tool_result). Returns (rendered-text, true) on success; ("",
// false) when nothing renderable was found.
//
// Per the snapshot-extractor-drops-tool-use bug + the chosen fix
// shape, every known part type is preserved with a role-aware prefix
// + per-part truncation. Forward-compat: unknown part types are
// skipped silently (extractContent succeeds as long as at least one
// part rendered to a non-empty string).
func extractContent(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	// Try string first — cheapest and most common for user rows.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	// Fall back to array of parts.
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			rendered := renderPart(p)
			if rendered != "" {
				texts = append(texts, rendered)
			}
		}
		if len(texts) == 0 {
			return "", false
		}
		// "\n\n" between parts so the role-prefix lines stay visually
		// distinct in the Qwen prompt's snapshot block. A single space
		// (the pre-fix behaviour) ran sibling parts together and made
		// long tool_use payloads hard to skim.
		return strings.Join(texts, "\n\n"), true
	}
	return "", false
}

// estimateTokens applies the char/4 heuristic to s. Mirrors the
// budget approach in qwenretrieve — the Qwen prompt's actual tokenizer
// will land within 10-20% of this in practice; the budget exists to
// keep prompts from exceeding the 32K context window, not to be a
// precise token count.
func estimateTokens(s string) int {
	n := len(s) / charsPerTokenEstimate
	if n == 0 {
		return 1
	}
	return n
}
