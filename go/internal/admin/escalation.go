package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"toolkit/internal/events"
)

// escalation.go — the admin-surface actions for the orchestrator-tier
// escalation contract (chain orchestrator-tier-escalation-contract T2). Three
// actions, all co-located on admin so a harness targets exactly one route:
//
//   - escalation_threshold_list — read the effective per-trigger threshold
//     config (global defaults overlaid by project-specific overrides).
//   - escalation_threshold_set  — upsert one (project_id, trigger_kind) row.
//   - escalation_propose        — emit an EscalationProposed event through the
//     write-side ledger (the contract's observable escalation signal).
//
// See docs/ORCHESTRATOR_ESCALATION.md §6–§7.

// escalationTriggerKinds is the closed set of trigger kinds — mirrors the
// CHECK constraint in migration 080 and the enum in
// blueprints/events/EscalationProposed.json. Validated handler-side so the
// caller gets a clean error before the DB CHECK / schema validator fires.
var escalationTriggerKinds = map[string]struct{}{
	"retry_exhaustion":    {},
	"low_confidence":      {},
	"repeated_tool_error": {},
	"parse_failure":       {},
	"explicit_handoff":    {},
}

// escalationStates is the closed set of router states for the
// EscalationProposed state_before / state_after fields.
var escalationStates = map[string]struct{}{
	"cheap":        {},
	"escalated":    {},
	"de_escalated": {},
}

// escalationThresholdRow mirrors one escalation_thresholds row in its
// JSON-response shape.
type escalationThresholdRow struct {
	ProjectID         string  `json:"project_id"`
	TriggerKind       string  `json:"trigger_kind"`
	ThresholdValue    float64 `json:"threshold_value"`
	Enabled           bool    `json:"enabled"`
	DeEscalationTurns int64   `json:"de_escalation_turns"`
	UpdatedAt         string  `json:"updated_at"`
}

// escalationThresholdListParams is the request body for
// escalation_threshold_list.
type escalationThresholdListParams struct {
	ProjectID string `json:"project_id"`
}

// escalationThresholdList returns the EFFECTIVE per-trigger config for a
// project: the global-default rows (project_id = ") overlaid by any
// project-specific override row for the same trigger_kind. With no
// project_id, it returns the global defaults alone. Rows are sorted by
// trigger_kind for a stable response.
func (d Deps) escalationThresholdList(ctx context.Context, params json.RawMessage) ([]escalationThresholdRow, error) {
	var p escalationThresholdListParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
	}
	rows, err := d.Pool.DB().QueryContext(ctx,
		`SELECT project_id, trigger_kind, threshold_value, enabled, de_escalation_turns, updated_at
		   FROM escalation_thresholds
		  WHERE project_id = '' OR project_id = ?`, p.ProjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Merge keyed by trigger_kind, preferring the project-specific row over
	// the global default.
	effective := map[string]escalationThresholdRow{}
	for rows.Next() {
		var r escalationThresholdRow
		var enabledInt int64
		if err := rows.Scan(&r.ProjectID, &r.TriggerKind, &r.ThresholdValue,
			&enabledInt, &r.DeEscalationTurns, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabledInt != 0
		existing, seen := effective[r.TriggerKind]
		// project-specific (non-empty project_id) always wins over a global.
		if !seen || (r.ProjectID != "" && existing.ProjectID == "") {
			effective[r.TriggerKind] = r
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]escalationThresholdRow, 0, len(effective))
	for _, r := range effective {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TriggerKind < out[j].TriggerKind })
	return out, nil
}

// escalationThresholdSetParams is the request body for
// escalation_threshold_set. ThresholdValue is required; Enabled and
// DeEscalationTurns are optional pointers — when omitted they preserve the
// existing row's values (or fall back to enabled=true / K=2 when the row is
// new).
type escalationThresholdSetParams struct {
	ProjectID         string   `json:"project_id"`
	TriggerKind       string   `json:"trigger_kind"`
	ThresholdValue    *float64 `json:"threshold_value"`
	Enabled           *bool    `json:"enabled"`
	DeEscalationTurns *int64   `json:"de_escalation_turns"`
}

// EscalationThresholdSetResult is the response shape for
// escalation_threshold_set.
type EscalationThresholdSetResult struct {
	OK                bool    `json:"ok"`
	ProjectID         string  `json:"project_id"`
	TriggerKind       string  `json:"trigger_kind"`
	ThresholdValue    float64 `json:"threshold_value"`
	Enabled           bool    `json:"enabled"`
	DeEscalationTurns int64   `json:"de_escalation_turns"`
}

// escalationThresholdSet upserts one (project_id, trigger_kind) threshold
// row. Omitted enabled / de_escalation_turns preserve the existing row's
// values (read-then-upsert) so a threshold-only edit doesn't silently reset
// the other knobs; for a brand-new row the defaults are enabled=true, K=2.
func (d Deps) escalationThresholdSet(ctx context.Context, params json.RawMessage) (EscalationThresholdSetResult, error) {
	var p escalationThresholdSetParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return EscalationThresholdSetResult{}, err
		}
	}
	if p.TriggerKind == "" {
		return EscalationThresholdSetResult{}, errors.New("params.trigger_kind is required")
	}
	if _, ok := escalationTriggerKinds[p.TriggerKind]; !ok {
		return EscalationThresholdSetResult{}, fmt.Errorf("invalid trigger_kind %q (one of: retry_exhaustion, low_confidence, repeated_tool_error, parse_failure, explicit_handoff)", p.TriggerKind)
	}
	if p.ThresholdValue == nil {
		return EscalationThresholdSetResult{}, errors.New("params.threshold_value is required")
	}
	if p.DeEscalationTurns != nil && *p.DeEscalationTurns < 1 {
		return EscalationThresholdSetResult{}, errors.New("params.de_escalation_turns must be >= 1")
	}

	// Read the existing row (if any) so omitted optional fields preserve it.
	enabled := true
	deEsc := int64(2)
	var curEnabledInt, curDeEsc int64
	err := d.Pool.DB().QueryRowContext(ctx,
		`SELECT enabled, de_escalation_turns FROM escalation_thresholds
		  WHERE project_id = ? AND trigger_kind = ?`, p.ProjectID, p.TriggerKind).
		Scan(&curEnabledInt, &curDeEsc)
	switch {
	case err == nil:
		enabled = curEnabledInt != 0
		deEsc = curDeEsc
	case errors.Is(err, sql.ErrNoRows):
		// new row — keep the defaults above.
	default:
		return EscalationThresholdSetResult{}, err
	}
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	if p.DeEscalationTurns != nil {
		deEsc = *p.DeEscalationTurns
	}
	enabledInt := int64(0)
	if enabled {
		enabledInt = 1
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	_, err = d.Pool.DB().ExecContext(ctx,
		`INSERT INTO escalation_thresholds
		    (project_id, trigger_kind, threshold_value, enabled, de_escalation_turns, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (project_id, trigger_kind) DO UPDATE SET
		    threshold_value     = excluded.threshold_value,
		    enabled             = excluded.enabled,
		    de_escalation_turns = excluded.de_escalation_turns,
		    updated_at          = excluded.updated_at`,
		p.ProjectID, p.TriggerKind, *p.ThresholdValue, enabledInt, deEsc, updatedAt)
	if err != nil {
		return EscalationThresholdSetResult{}, err
	}
	return EscalationThresholdSetResult{
		OK:                true,
		ProjectID:         p.ProjectID,
		TriggerKind:       p.TriggerKind,
		ThresholdValue:    *p.ThresholdValue,
		Enabled:           enabled,
		DeEscalationTurns: deEsc,
	}, nil
}

// escalationProposeParams is the request body for escalation_propose — the
// EscalationProposed event payload plus an optional project_id (entity scope)
// and reason (recorded as the envelope rationale so the proposal's "why"
// lands even though HTTP-emitted events carry the system actor).
type escalationProposeParams struct {
	Trigger       string `json:"trigger"`
	FromModel     string `json:"from_model"`
	ToModel       string `json:"to_model"`
	SessionID     string `json:"session_id"`
	TurnIndex     int    `json:"turn_index"`
	StateBefore   string `json:"state_before"`
	StateAfter    string `json:"state_after"`
	TriggerDetail string `json:"trigger_detail"`
	// FiredThreshold is the snapshot of the threshold_value that fired; it
	// maps onto the EscalationProposed payload's threshold_value field. The
	// action param is named distinctly from the threshold_set action's
	// (required) threshold_value param so the surface-wide action-doc
	// required-parity gate — which keys required-ness by param NAME across
	// the whole surface — doesn't conflate an optional propose snapshot with
	// the mandatory config-write value.
	FiredThreshold *float64 `json:"fired_threshold"`
	ProjectID      string   `json:"project_id"`
	Reason         string   `json:"reason"`
}

// EscalationProposeResult is the response shape for escalation_propose.
type EscalationProposeResult struct {
	OK      bool   `json:"ok"`
	EventID string `json:"event_id"`
}

// escalationPropose validates the proposal and emits one EscalationProposed
// event through the write-side ledger inside a single write transaction. The
// event's schema validator is the authoritative backstop for enum / required
// checks; the handler-side validation here just yields cleaner errors before
// the Emit call. Entity is the orchestrator_session (slug = session_id),
// project-scoped when project_id is supplied and cross-cutting otherwise.
func (d Deps) escalationPropose(ctx context.Context, params json.RawMessage) (EscalationProposeResult, error) {
	var p escalationProposeParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return EscalationProposeResult{}, err
		}
	}
	if p.Trigger == "" || p.FromModel == "" || p.ToModel == "" || p.SessionID == "" ||
		p.StateBefore == "" || p.StateAfter == "" {
		return EscalationProposeResult{}, errors.New("params.trigger, params.from_model, params.to_model, params.session_id, params.state_before, and params.state_after are required")
	}
	if _, ok := escalationTriggerKinds[p.Trigger]; !ok {
		return EscalationProposeResult{}, fmt.Errorf("invalid trigger %q", p.Trigger)
	}
	if _, ok := escalationStates[p.StateBefore]; !ok {
		return EscalationProposeResult{}, fmt.Errorf("invalid state_before %q (one of: cheap, escalated, de_escalated)", p.StateBefore)
	}
	if _, ok := escalationStates[p.StateAfter]; !ok {
		return EscalationProposeResult{}, fmt.Errorf("invalid state_after %q (one of: cheap, escalated, de_escalated)", p.StateAfter)
	}
	if p.TurnIndex < 0 {
		return EscalationProposeResult{}, errors.New("params.turn_index must be >= 0")
	}

	payload := events.EscalationProposedPayload{
		Trigger:        p.Trigger,
		FromModel:      p.FromModel,
		ToModel:        p.ToModel,
		SessionID:      p.SessionID,
		TurnIndex:      p.TurnIndex,
		StateBefore:    p.StateBefore,
		StateAfter:     p.StateAfter,
		ThresholdValue: p.FiredThreshold,
	}
	if td := strings.TrimSpace(p.TriggerDetail); td != "" {
		payload.TriggerDetail = &td
	}

	entity := events.NewCrossCuttingEntityRef("orchestrator_session", p.SessionID)
	if p.ProjectID != "" {
		entity = events.NewEntityRef("orchestrator_session", p.SessionID, p.ProjectID)
	}
	var reason *string
	if r := strings.TrimSpace(p.Reason); r != "" {
		reason = &r
	}

	var eventID string
	err := d.Pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:    entity,
			Payload:   payload,
			Rationale: reason,
		})
		if err != nil {
			return err
		}
		eventID = id
		return nil
	})
	if err != nil {
		return EscalationProposeResult{}, err
	}
	return EscalationProposeResult{OK: true, EventID: eventID}, nil
}
