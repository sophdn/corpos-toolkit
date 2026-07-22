package grounding

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// trackedActions matches the prior Rust prototype's enum: vault_search,
// kiwix_search, and knowledge_search are the only knowledge-meta-tool
// actions that emit grounding_events rows. Other knowledge actions
// (vault_read, kiwix_fetch, kiwix_list_books, library_*) are
// retrieval-shaped consumers and not search calls.
var trackedActions = map[string]struct{}{
	"vault_search":     {},
	"kiwix_search":     {},
	"knowledge_search": {},
}

// ProcessedEvent is one extracted search-call lifecycle from a session
// JSONL: the search call itself, its tool_result, and the next
// assistant-text-turn's "used" detection. Mirrors the row shape of
// internal/db.GroundingEventInsert but stays at session-parser scope —
// the caller stamps project_id and dispatches to db.InsertGroundingEvent.
//
// PromptID, RecordIdx, ResultText, and IsSidechain are populated for the
// click_kind detectors that run after the grounding_events row lands.
// They don't affect the existing grounding_events writer path.
type ProcessedEvent struct {
	SessionID         string
	PromptID          string
	CallID            string
	Action            string
	QueryText         string // the search query/pattern from the tool_use input
	ResultsCount      int64
	SourceRefs        []string
	NextTurnHasOutput bool
	Used              *bool
	RecordIdx         int    // position of the tool_use record in the parsed entry list
	ResultText        string // raw tool_result content body (for cited-kind quote detection)
	IsSidechain       bool   // true when sourced from a subagents/agent-*.jsonl file
	ParentSpanID      string // parent agent's span_id when IsSidechain
	// ToolUseTimestamp is the transcript's `timestamp` field on the
	// assistant entry containing this tool_use.
	ToolUseTimestamp string
	// ToolResultTimestamp is the transcript's `timestamp` field on
	// the user entry carrying the tool_result block for this call.
	// The --preserve-transcript-timestamps mode threads THIS through
	// to grounding_events.created_at, NOT the tool_use timestamp —
	// reason: the online emit fires at handler-exit (after qwen
	// responds), so its natural created_at aligns with the
	// tool_result's arrival, not with when the call was issued. A
	// vault_search with 10s qwen latency has tool_use at T+0 and
	// tool_result at T+10; inference_invocations.created_at lands at T+10
	// too. Using tool_use time would put the backfilled row 10s away
	// from inference_invocations and miss the /inference/health-cards
	// proximity-join window.
	ToolResultTimestamp string
}

// jsonlEntry is a single JSONL record. The fields are pulled lazily;
// only the ones the processor inspects are typed here, the rest stay
// as RawMessage to keep parsing cheap on large session files.
//
// PromptID is the optional record-level field; transcript JSONL records
// of type=user carry it directly, and the walker threads it forward to
// every following record (including assistant turns + tool_results)
// until the next user record. See TT1.5 spike §1 for the threading
// rationale.
type jsonlEntry struct {
	Type      string        `json:"type"`
	SessionID string        `json:"sessionId"`
	PromptID  string        `json:"promptId"`
	Timestamp string        `json:"timestamp"`
	Message   *jsonlMessage `json:"message,omitempty"`
}

type jsonlMessage struct {
	Content json.RawMessage `json:"content,omitempty"`
}

// processSession walks a session JSONL file and extracts every
// grounding_events row that should land for it. Behavior is
// bit-identical to the Rust prototype:
//
//  1. Tool_use blocks on assistant turns whose name contains "knowledge"
//     and whose action is in trackedActions are search-call candidates.
//  2. The matching tool_result is the next user turn carrying a
//     tool_result block with tool_use_id == call.id.
//  3. The "next turn has output" + "used" detection scans every
//     subsequent assistant turn until one with a non-empty text block;
//     "used" is set per check_used (slug/path substring).
func processSession(path string) ([]ProcessedEvent, []jsonlEntry, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from operator-supplied --session/--dir
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Read every line into Value so the second-pass tool_use ↔ tool_result
	// correlation can index forward without re-parsing. Sessions are
	// typically <10 MB; the in-memory cost is fine.
	var entries []jsonlEntry
	scanner := bufio.NewScanner(f)
	// JSONL records can be long (one tool_result blob from a vault_search
	// holds the entire result set serialized as one string); raise the
	// scanner cap from the 64 KB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e jsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan %s: %w", path, err)
	}

	sessionID := ""
	for _, e := range entries {
		if e.SessionID != "" {
			sessionID = e.SessionID
			break
		}
	}
	if sessionID == "" {
		base := filepath.Base(path)
		sessionID = strings.TrimSuffix(base, filepath.Ext(base))
	}

	// Thread promptId forward from each user record. Per TT1.5 §1 the
	// transcript-shape reality is that promptId lives only on the user
	// record and subsequent assistant/tool records inherit it by sequence.
	// Newer transcripts may carry promptId on every record; preserve it
	// when present, otherwise inherit the most-recent user-record value.
	currentPrompt := ""
	for i := range entries {
		switch {
		case entries[i].PromptID != "":
			currentPrompt = entries[i].PromptID
		case entries[i].Type == "user" && entries[i].PromptID == "":
			// User record without promptId resets the inherited prompt —
			// distinct prompts that happen to lack the field shouldn't
			// stay glued to the previous arc.
			currentPrompt = ""
		default:
			entries[i].PromptID = currentPrompt
		}
	}

	var events []ProcessedEvent
	for i, entry := range entries {
		if entry.Type != "assistant" || entry.Message == nil {
			continue
		}
		var content []json.RawMessage
		if err := json.Unmarshal(entry.Message.Content, &content); err != nil {
			continue
		}
		for _, item := range content {
			var head struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(item, &head); err != nil {
				continue
			}
			if head.Type != "tool_use" || !strings.Contains(head.Name, "knowledge") {
				continue
			}
			var withInput struct {
				Input map[string]json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(item, &withInput); err != nil {
				continue
			}
			var action string
			if a, ok := withInput.Input["action"]; ok {
				_ = json.Unmarshal(a, &action)
			}
			if _, ok := trackedActions[action]; !ok {
				continue
			}
			if head.ID == "" {
				continue
			}

			ev := ProcessedEvent{
				SessionID:        sessionID,
				PromptID:         entry.PromptID,
				CallID:           head.ID,
				Action:           action,
				QueryText:        extractQueryText(action, withInput.Input["params"]),
				RecordIdx:        i,
				ToolUseTimestamp: entry.Timestamp,
			}
			if completeEvent(&ev, entries[i+1:]) {
				events = append(events, ev)
			}
		}
	}
	return events, entries, nil
}

// completeEvent walks the entries AFTER the tool_use to find first the
// matching tool_result and then the next assistant-text turn. Returns
// true when a tool_result was found (the row is worth emitting even if
// no subsequent assistant text appeared — the next_turn_has_output and
// used fields stay at their zero values, matching the Rust prototype).
func completeEvent(ev *ProcessedEvent, rest []jsonlEntry) bool {
	foundResult := false
	for _, e := range rest {
		if !foundResult {
			if e.Type != "user" || e.Message == nil {
				continue
			}
			var content []json.RawMessage
			if err := json.Unmarshal(e.Message.Content, &content); err != nil {
				continue
			}
			for _, item := range content {
				var rh struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
				}
				if err := json.Unmarshal(item, &rh); err != nil {
					continue
				}
				if rh.Type != "tool_result" || rh.ToolUseID != ev.CallID {
					continue
				}
				resultText := extractToolResultText(item)
				count, refs := extractSourceRefs(ev.Action, resultText)
				ev.ResultsCount = count
				ev.SourceRefs = refs
				ev.ResultText = resultText
				ev.ToolResultTimestamp = e.Timestamp
				foundResult = true
				break
			}
		} else {
			if e.Type != "assistant" || e.Message == nil {
				continue
			}
			var content []json.RawMessage
			if err := json.Unmarshal(e.Message.Content, &content); err != nil {
				continue
			}
			text, ok := firstNonEmptyAssistantText(content)
			if !ok {
				continue
			}
			ev.NextTurnHasOutput = true
			used := checkUsed(ev.SourceRefs, text)
			ev.Used = &used
			return true
		}
	}
	return foundResult
}

// extractQueryText pulls the search query string out of a tracked
// action's tool_use input params. Bug `reranker-projection-drops-query-
// text-on-positive-labels`: the processor's fall-through insert (for
// searches the online emit missed) previously left query_text NULL,
// and ALL positive-labeled training rows come from those processor rows
// — making the cross-encoder corpus untrainable. The param key differs
// by action: vault_search / knowledge_search use `query`, kiwix_search
// uses `pattern` (matches the online handlers' param reads). Returns ""
// when params is absent or the key is missing — InsertGroundingEvent
// maps "" to a NULL column, preserving the nil-safe contract.
func extractQueryText(action string, paramsRaw json.RawMessage) string {
	if len(paramsRaw) == 0 {
		return ""
	}
	var params struct {
		Query   string `json:"query"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return ""
	}
	switch action {
	case "vault_search", "knowledge_search":
		return params.Query
	case "kiwix_search":
		return params.Pattern
	default:
		return ""
	}
}

// extractToolResultText returns the textual body of a tool_result block,
// handling both shapes the Claude Code session JSONL produces:
//
//   - content: [{type: "text", text: "..."}]   (the common case)
//   - content: "..."                            (legacy/string form)
func extractToolResultText(item json.RawMessage) string {
	// Try the array-of-blocks shape first.
	var arrayForm struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(item, &arrayForm); err == nil && len(arrayForm.Content) > 0 {
		return arrayForm.Content[0].Text
	}
	// Fall back to the bare-string shape.
	var stringForm struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(item, &stringForm); err == nil {
		return stringForm.Content
	}
	return ""
}

// firstNonEmptyAssistantText scans a content array for the first
// {type:"text", text:"..."} block with non-whitespace content. Returns
// the text and true; returns ("", false) when the turn is pure tool_use
// (no text blocks). Mirrors the Rust prototype's text_opt filter.
func firstNonEmptyAssistantText(content []json.RawMessage) (string, bool) {
	for _, item := range content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item, &block); err != nil {
			continue
		}
		if block.Type != "text" {
			continue
		}
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		return block.Text, true
	}
	return "", false
}

// extractSourceRefs parses tool_result text into (results_count, source_refs)
// per the action's known result shape:
//
//   - vault_search:     {"results":[{"path":...},...]}             → path strings
//   - kiwix_search:     {"hits":[{"article_ref":{"zim_id","slug"}}]} → "<zim>::<slug>"
//   - knowledge_search: [{"source_ref":...},...]                   → source_ref strings
//
// Each shape mirrors the Rust prototype's extraction; unknown action or
// unparseable JSON returns (0, nil) so the row still lands with the
// minimum schema.
func extractSourceRefs(action, resultText string) (int64, []string) {
	if resultText == "" {
		return 0, nil
	}
	switch action {
	case "vault_search":
		var parsed struct {
			Results []struct {
				Path string `json:"path"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(resultText), &parsed); err != nil {
			return 0, nil
		}
		refs := make([]string, 0, len(parsed.Results))
		for _, r := range parsed.Results {
			if r.Path != "" {
				refs = append(refs, r.Path)
			}
		}
		return int64(len(refs)), refs
	case "kiwix_search":
		var parsed struct {
			Hits []struct {
				ArticleRef struct {
					ZimID string `json:"zim_id"`
					Slug  string `json:"slug"`
				} `json:"article_ref"`
			} `json:"hits"`
		}
		if err := json.Unmarshal([]byte(resultText), &parsed); err != nil {
			return 0, nil
		}
		refs := make([]string, 0, len(parsed.Hits))
		for _, h := range parsed.Hits {
			if h.ArticleRef.ZimID != "" && h.ArticleRef.Slug != "" {
				refs = append(refs, h.ArticleRef.ZimID+"::"+h.ArticleRef.Slug)
			}
		}
		return int64(len(refs)), refs
	case "knowledge_search":
		var parsed []struct {
			SourceRef string `json:"source_ref"`
		}
		if err := json.Unmarshal([]byte(resultText), &parsed); err != nil {
			return 0, nil
		}
		refs := make([]string, 0, len(parsed))
		for _, r := range parsed {
			if r.SourceRef != "" {
				refs = append(refs, r.SourceRef)
			}
		}
		return int64(len(refs)), refs
	default:
		return 0, nil
	}
}

// checkUsed is the approximation: for each source_ref, test whether its
// filename stem (no extension, no path prefix) appears as a
// case-insensitive substring of `text`. Matches the Rust prototype's
// behavior including the .md-only stripping (other extensions stay).
func checkUsed(sourceRefs []string, text string) bool {
	if len(sourceRefs) == 0 || text == "" {
		return false
	}
	lowerText := strings.ToLower(text)
	for _, r := range sourceRefs {
		needle := r
		if strings.Contains(r, "/") {
			idx := strings.LastIndex(r, "/")
			needle = r[idx+1:]
			needle = strings.TrimSuffix(needle, ".md")
		}
		if needle == "" {
			continue
		}
		if strings.Contains(lowerText, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}
