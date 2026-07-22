package sources

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
)

// ZeroResultGapBuilder builds source material for candidates with
// origin='zero_result_gap'. The candidate's OriginRef holds the
// grounding_events.id; we look up the session_id / call_id / action,
// reconstruct the original query from the session JSONL by locating
// the tool_use block whose id matches call_id, and assemble:
//
//	"Zero-result search: action=<action>, query=<query>"
//
// Mirrors primary_pass + extract_query_from_session in
// benchmarks/src/bin/knowledge_curate.rs.
type ZeroResultGapBuilder struct {
	projectsRoot string // typically ~/.claude/projects
}

// NewZeroResultGapBuilder roots at projectsRoot. Empty defaults to
// $HOME/.claude/projects.
func NewZeroResultGapBuilder(projectsRoot string) *ZeroResultGapBuilder {
	if projectsRoot == "" {
		if home := os.Getenv("HOME"); home != "" {
			projectsRoot = filepath.Join(home, ".claude", "projects")
		}
	}
	return &ZeroResultGapBuilder{projectsRoot: projectsRoot}
}

func (ZeroResultGapBuilder) Origin() string { return "zero_result_gap" }

func (b ZeroResultGapBuilder) Build(ctx context.Context, pool *db.Pool, cand curation.Candidate) (string, error) {
	if cand.OriginRef == nil || *cand.OriginRef == "" {
		return "", fmt.Errorf("zero_result_gap build: candidate %d has no origin_ref", cand.ID)
	}

	var sessionID, callID, action string
	err := pool.DB().QueryRowContext(ctx,
		`SELECT session_id, call_id, action FROM grounding_events WHERE id = ?`,
		*cand.OriginRef,
	).Scan(&sessionID, &callID, &action)
	if err != nil {
		return "", fmt.Errorf("zero_result_gap build: grounding_event %s: %w",
			*cand.OriginRef, err)
	}

	jsonlPath := filepath.Join(b.projectsRoot,
		projectToProjectsDir(cand.ProjectID),
		sessionID+".jsonl")

	query, err := extractQueryFromSession(jsonlPath, callID)
	if err != nil {
		return "", fmt.Errorf("zero_result_gap build: reconstruct query for %s: %w",
			jsonlPath, err)
	}
	if query == "" {
		return "", fmt.Errorf("zero_result_gap build: no tool_use with call_id=%s in %s",
			callID, jsonlPath)
	}

	return fmt.Sprintf("Zero-result search: action=%s, query=%s", action, query), nil
}

// projectToProjectsDir mirrors the Rust helper of the same name: maps
// "<project>" to "-home-sophi-dev-<project>" (the encoded ~/.claude/projects/
// directory name). Today's user is always "sophi"; if that ever
// generalises, read $HOME and encode it the same way Claude Code does.
func projectToProjectsDir(projectID string) string {
	return "-home-sophi-dev-" + projectID
}

// Typed shapes for the JSONL session log. We only care about a few
// fields — assistant lines, tool_use blocks, the call id, and the
// query parameter — so the structs are narrow. Polymorphic fields
// (input, params) use json.RawMessage so we can decode each shape
// explicitly rather than reaching for map[string]any.
type sessionLine struct {
	Type    string          `json:"type"`
	Message *sessionMessage `json:"message,omitempty"`
}

type sessionMessage struct {
	Content []sessionContentBlock `json:"content"`
}

type sessionContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// toolUseInputFlat decodes the flat input.query shape (3 in the
// queryFromInput tolerance list).
type toolUseInputFlat struct {
	Query string `json:"query,omitempty"`
}

// toolUseInputNested decodes the input.params nested-object shape (1)
// or the input.params JSON-encoded-string shape (2) by holding params
// as RawMessage and inspecting it.
type toolUseInputNested struct {
	Params json.RawMessage `json:"params,omitempty"`
}

// nestedQuery decodes a {"query":"..."} object.
type nestedQuery struct {
	Query string `json:"query"`
}

// extractQueryFromSession walks the JSONL session file looking for the
// assistant tool_use block whose id matches callID, then extracts the
// query parameter from the block's input. Returns "" (not error) when
// the block exists but has no query — callers treat both "" and a
// file-open error distinctly.
//
// Tolerates three input shapes (matching the Rust impl):
//  1. input.params is a nested object with a "query" key
//  2. input.params is a JSON-encoded string whose decoded form has "query"
//  3. input.query at the top level (flat)
func extractQueryFromSession(path, callID string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 4*1024*1024)
	for scanner.Scan() {
		var line sessionLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "assistant" || line.Message == nil {
			continue
		}
		for _, block := range line.Message.Content {
			if block.Type != "tool_use" || block.ID != callID || len(block.Input) == 0 {
				continue
			}
			if q := queryFromInputBytes(block.Input); q != "" {
				return q, nil
			}
			return "", nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan: %w", err)
	}
	return "", nil
}

// queryFromInputBytes decodes the dynamic input shape into a query
// string, tolerating the three params variants from the Rust impl.
// Returns "" when no recognizable query is present (a tool_use whose
// input doesn't carry a search query, e.g. a non-query MCP action).
func queryFromInputBytes(raw json.RawMessage) string {
	// Variant 1 & 2: input.params is present.
	var nested toolUseInputNested
	if err := json.Unmarshal(raw, &nested); err == nil && len(nested.Params) > 0 {
		// Variant 1: nested object.
		var obj nestedQuery
		if err := json.Unmarshal(nested.Params, &obj); err == nil && obj.Query != "" {
			return obj.Query
		}
		// Variant 2: JSON-encoded string. The RawMessage is a quoted
		// string like `"{\"query\":\"...\"}"`; unquote then decode.
		var inner string
		if err := json.Unmarshal(nested.Params, &inner); err == nil {
			var parsed nestedQuery
			if err := json.Unmarshal([]byte(inner), &parsed); err == nil && parsed.Query != "" {
				return parsed.Query
			}
		}
	}
	// Variant 3: flat input.query.
	var flat toolUseInputFlat
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat.Query
	}
	return ""
}
