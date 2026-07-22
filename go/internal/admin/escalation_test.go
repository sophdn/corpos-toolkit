package admin

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/events"
)

// findThreshold returns the row for a trigger_kind from a list result, or
// fails the test if absent.
func findThreshold(t *testing.T, rows []escalationThresholdRow, kind string) escalationThresholdRow {
	t.Helper()
	for _, r := range rows {
		if r.TriggerKind == kind {
			return r
		}
	}
	t.Fatalf("trigger_kind %q not found in %+v", kind, rows)
	return escalationThresholdRow{}
}

// TestEscalationThresholdList_SeededGlobalDefaults pins migration 080's seed:
// a fresh DB exposes the 5 global-default trigger rows with the documented
// defaults.
func TestEscalationThresholdList_SeededGlobalDefaults(t *testing.T) {
	d := mkDeps(t)
	rows, err := d.escalationThresholdList(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("want 5 seeded global rows, got %d: %+v", len(rows), rows)
	}
	re := findThreshold(t, rows, "retry_exhaustion")
	if re.ThresholdValue != 2 || !re.Enabled || re.DeEscalationTurns != 2 || re.ProjectID != "" {
		t.Errorf("retry_exhaustion default = %+v", re)
	}
	lc := findThreshold(t, rows, "low_confidence")
	if lc.ThresholdValue != 0.35 {
		t.Errorf("low_confidence threshold = %v, want 0.35", lc.ThresholdValue)
	}
}

// TestEscalationThreshold_SetAndListRoundtrip sets a project-specific override
// and confirms escalation_threshold_list returns the EFFECTIVE config: the
// project override wins over the global default for that trigger, while
// untouched triggers fall back to the global.
func TestEscalationThreshold_SetAndListRoundtrip(t *testing.T) {
	d := mkDeps(t)
	four := 4.0
	if _, err := d.escalationThresholdSet(context.Background(),
		json.RawMessage(`{"project_id":"proj-x","trigger_kind":"retry_exhaustion","threshold_value":4,"de_escalation_turns":3}`)); err != nil {
		t.Fatalf("set: %v", err)
	}
	rows, err := d.escalationThresholdList(context.Background(),
		json.RawMessage(`{"project_id":"proj-x"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("want 5 effective rows, got %d", len(rows))
	}
	re := findThreshold(t, rows, "retry_exhaustion")
	if re.ProjectID != "proj-x" || re.ThresholdValue != four || re.DeEscalationTurns != 3 {
		t.Errorf("override not effective: %+v", re)
	}
	// An untouched trigger still resolves to the global default row.
	pf := findThreshold(t, rows, "parse_failure")
	if pf.ProjectID != "" || pf.ThresholdValue != 2 {
		t.Errorf("parse_failure should fall back to global default: %+v", pf)
	}
}

// TestEscalationThresholdSet_PreservesOmittedFields confirms a threshold-only
// edit does not reset enabled / de_escalation_turns (read-then-upsert).
func TestEscalationThresholdSet_PreservesOmittedFields(t *testing.T) {
	d := mkDeps(t)
	ctx := context.Background()
	// Seed a project row with non-default enabled=false, K=5.
	if _, err := d.escalationThresholdSet(ctx,
		json.RawMessage(`{"project_id":"p","trigger_kind":"low_confidence","threshold_value":0.5,"enabled":false,"de_escalation_turns":5}`)); err != nil {
		t.Fatalf("seed set: %v", err)
	}
	// Now change only the threshold.
	got, err := d.escalationThresholdSet(ctx,
		json.RawMessage(`{"project_id":"p","trigger_kind":"low_confidence","threshold_value":0.6}`))
	if err != nil {
		t.Fatalf("update set: %v", err)
	}
	if got.ThresholdValue != 0.6 || got.Enabled != false || got.DeEscalationTurns != 5 {
		t.Errorf("omitted fields not preserved: %+v", got)
	}
}

func TestEscalationThresholdSet_RejectsBadTriggerKind(t *testing.T) {
	d := mkDeps(t)
	_, err := d.escalationThresholdSet(context.Background(),
		json.RawMessage(`{"trigger_kind":"not_a_trigger","threshold_value":1}`))
	if err == nil {
		t.Fatal("expected error for invalid trigger_kind")
	}
}

func TestEscalationThresholdSet_RequiresThresholdValue(t *testing.T) {
	d := mkDeps(t)
	_, err := d.escalationThresholdSet(context.Background(),
		json.RawMessage(`{"trigger_kind":"retry_exhaustion"}`))
	if err == nil {
		t.Fatal("expected error for missing threshold_value")
	}
}

// TestEscalationPropose_EmitsEventToLedger is the (b)+(f)-adjacent ledger
// proof: escalation_propose lands a schema-valid EscalationProposed row in the
// write-side events table with the payload + rationale intact and the
// orchestrator_session entity.
func TestEscalationPropose_EmitsEventToLedger(t *testing.T) {
	d := mkDeps(t)
	ctx := context.Background()
	params := `{
		"trigger":"retry_exhaustion",
		"from_model":"deepseek-v4-pro",
		"to_model":"claude-opus-4-7",
		"session_id":"orch-abc",
		"turn_index":7,
		"state_before":"cheap",
		"state_after":"escalated",
		"trigger_detail":"unit=at-3 retries_used=2",
		"fired_threshold":2,
		"project_id":"mcp-servers",
		"reason":"retry budget exhausted on at-3; handing the next turn to the strong tier"
	}`
	res, err := d.escalationPropose(ctx, json.RawMessage(params))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if !res.OK || res.EventID == "" {
		t.Fatalf("propose result = %+v", res)
	}

	var (
		typ, entityKind, entitySlug, payloadJSON string
		projectID, rationale                     *string
	)
	row := d.Pool.DB().QueryRowContext(ctx,
		`SELECT type, entity_kind, entity_slug, entity_project_id, payload, rationale
		   FROM events WHERE event_id = ?`, res.EventID)
	if err := row.Scan(&typ, &entityKind, &entitySlug, &projectID, &payloadJSON, &rationale); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if typ != "EscalationProposed" {
		t.Errorf("event type = %q", typ)
	}
	if entityKind != "orchestrator_session" || entitySlug != "orch-abc" {
		t.Errorf("entity = %s/%s", entityKind, entitySlug)
	}
	if projectID == nil || *projectID != "mcp-servers" {
		t.Errorf("project_id = %v", projectID)
	}
	if rationale == nil || *rationale == "" {
		t.Errorf("rationale not recorded")
	}
	var p events.EscalationProposedPayload
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Trigger != "retry_exhaustion" || p.FromModel != "deepseek-v4-pro" ||
		p.ToModel != "claude-opus-4-7" || p.TurnIndex != 7 ||
		p.StateBefore != "cheap" || p.StateAfter != "escalated" {
		t.Errorf("payload mismatch: %+v", p)
	}
	if p.ThresholdValue == nil || *p.ThresholdValue != 2 {
		t.Errorf("fired_threshold not carried onto payload.threshold_value: %+v", p.ThresholdValue)
	}
}

// TestEscalationPropose_CrossCutting confirms an escalation event with no
// project_id lands as a cross-cutting (NULL project) event — the
// harness-agnostic path.
func TestEscalationPropose_CrossCutting(t *testing.T) {
	d := mkDeps(t)
	ctx := context.Background()
	res, err := d.escalationPropose(ctx, json.RawMessage(`{
		"trigger":"explicit_handoff","from_model":"qwen2.5-32b","to_model":"claude-opus-4-7",
		"session_id":"s1","state_before":"cheap","state_after":"escalated"
	}`))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var projectID *string
	if err := d.Pool.DB().QueryRowContext(ctx,
		`SELECT entity_project_id FROM events WHERE event_id = ?`, res.EventID).Scan(&projectID); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if projectID != nil {
		t.Errorf("expected NULL project_id for cross-cutting event, got %v", *projectID)
	}
}

func TestEscalationPropose_RejectsBadState(t *testing.T) {
	d := mkDeps(t)
	_, err := d.escalationPropose(context.Background(), json.RawMessage(`{
		"trigger":"low_confidence","from_model":"q","to_model":"o","session_id":"s",
		"state_before":"cheap","state_after":"banana"
	}`))
	if err == nil {
		t.Fatal("expected error for invalid state_after")
	}
}
