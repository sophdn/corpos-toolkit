package grounding

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"toolkit/internal/telemetry"
)

// terminalEvent is one detected `bug_resolve` / `task_complete` /
// `task_cancel` / `chain_close` tool_use call from a session JSONL.
// The processor walks these post-session, matches each to its
// corresponding events row by (entity, type, ts window), and emits one
// query_resolutions row per terminal event with the trajectory linkage
// to the prompt-scoped grounding_events + query_interactions.
type terminalEvent struct {
	RecordIdx       int
	ToolUseID       string
	PromptID        string
	EntityKind      string
	EntitySlug      string
	EntityProjectID string
	OutcomeKind     telemetry.OutcomeKind
	EventType       string
	Rationale       string
	Timestamp       string
}

// resolveActionMap names the four terminal MCP-work actions that close
// out a write-side entity, paired with the entity_kind and outcome_kind
// they emit on the events table. Reopen / unblock / stamp_sha actions
// are NOT terminal — they don't fire query_resolutions.
type resolveActionEntry struct {
	EntityKind  string
	OutcomeKind telemetry.OutcomeKind
	EventType   string
}

var resolveActionMap = map[string]resolveActionEntry{
	"bug_resolve":   {"bug", telemetry.OutcomeResolved, "BugResolved"},
	"task_complete": {"task", telemetry.OutcomeCompleted, "TaskCompleted"},
	"task_cancel":   {"task", telemetry.OutcomeCancelled, "TaskCancelled"},
	"chain_close":   {"chain", telemetry.OutcomeClosed, "ChainClosed"},
}

// collectTerminalEvents walks the parsed entries and returns one
// terminalEvent per matching work-tool tool_use. The rationale field is
// the load-bearing text resolved-from detection scans — varies by
// action: bug_resolve uses resolution_note, task_complete uses
// closure_summary, task_cancel uses reason, chain_close uses
// closure_summary.
func collectTerminalEvents(entries []jsonlEntry, defaultProjectID string) []terminalEvent {
	var out []terminalEvent
	for i, e := range entries {
		if e.Type != "assistant" || e.Message == nil {
			continue
		}
		var content []json.RawMessage
		if err := json.Unmarshal(e.Message.Content, &content); err != nil {
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
			if head.Type != "tool_use" || !strings.Contains(head.Name, "work") {
				continue
			}
			var w struct {
				Input struct {
					Action  string          `json:"action"`
					Project string          `json:"project"`
					Params  json.RawMessage `json:"params"`
				} `json:"input"`
			}
			if err := json.Unmarshal(item, &w); err != nil {
				continue
			}
			entry, ok := resolveActionMap[w.Input.Action]
			if !ok {
				continue
			}
			slug, rationale := extractEntityAndRationale(w.Input.Action, w.Input.Params)
			if slug == "" {
				continue
			}
			project := w.Input.Project
			if project == "" {
				project = defaultProjectID
			}
			out = append(out, terminalEvent{
				RecordIdx:       i,
				ToolUseID:       head.ID,
				PromptID:        e.PromptID,
				EntityKind:      entry.EntityKind,
				EntitySlug:      slug,
				EntityProjectID: project,
				OutcomeKind:     entry.OutcomeKind,
				EventType:       entry.EventType,
				Rationale:       rationale,
			})
		}
	}
	return out
}

// extractEntityAndRationale parses the params blob for an entity slug
// and the action-specific rationale field. Unknown fields stay zero;
// callers test for non-empty slug before treating the event as terminal.
func extractEntityAndRationale(action string, params json.RawMessage) (slug, rationale string) {
	var p struct {
		Slug           string `json:"slug"`
		ResolutionNote string `json:"resolution_note"`
		ClosureSummary string `json:"closure_summary"`
		Reason         string `json:"reason"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", ""
	}
	switch action {
	case "bug_resolve":
		return p.Slug, p.ResolutionNote
	case "task_complete":
		return p.Slug, p.ClosureSummary
	case "task_cancel":
		return p.Slug, p.Reason
	case "chain_close":
		return p.Slug, p.ClosureSummary
	}
	return p.Slug, ""
}

// detectResolvedFrom returns one Interaction per source_ref that the
// terminal event's rationale references. Slug-form normalization per
// TT1.5 §7.1 — agent rationales use the slug, not the full vault path.
func detectResolvedFrom(refs []string, rationale string) []Interaction {
	if rationale == "" || len(refs) == 0 {
		return nil
	}
	lowerR := strings.ToLower(rationale)
	var out []Interaction
	for i, ref := range refs {
		if !proseMentions(lowerR, ref) {
			continue
		}
		out = append(out, Interaction{
			SourceRef: ref,
			ClickKind: telemetry.ClickResolvedFrom,
			Position:  i + 1,
		})
	}
	return out
}

// lookupWriteEventIDs queries the events table for rows that match the
// terminal event by (entity_kind, entity_slug, entity_project_id, type)
// within a recent time window. Returns all matching event_ids (typically
// one); empty slice when no events row landed for this terminal call
// (the write-side handler may have failed or pre-dated the substrate).
//
// The lookup window — last `windowHours` hours from the processor's
// wall-clock — accepts late-running processors but guards against
// re-matching old reopens. Callers pass an empty slice through to
// telemetry.EmitResolution; the migration trigger accepts '[]' vacuously.
func lookupWriteEventIDs(ctx context.Context, tx *sql.Tx, te terminalEvent, windowHours int) ([]string, error) {
	if te.EntitySlug == "" || te.EventType == "" {
		return nil, nil
	}
	cutoff := time.Now().Add(-time.Duration(windowHours) * time.Hour).UTC().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `
		SELECT event_id FROM events
		WHERE entity_kind = ?
		  AND entity_slug = ?
		  AND type = ?
		  AND (entity_project_id = ? OR entity_project_id IS NULL OR ? = '')
		  AND ts >= ?
		ORDER BY ts DESC
	`, te.EntityKind, te.EntitySlug, te.EventType, te.EntityProjectID, te.EntityProjectID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
