package arcreview

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ActionKind names a typed filing action Qwen may propose. The closed
// enum IS the action whitelist — enforced at the response parser
// instead of at call time, so out-of-schema actions are rejected
// before any forge call is made (per docs/ARC_CLOSE_FILING_REVIEW.md
// §Structured-output-schema).
//
// Unknown action strings → ValidateDecision returns ErrUnknownAction;
// the dispatcher logs and skips the malformed decision.
type ActionKind string

const (
	// ActionForgeBug fires mcp__toolkit-server__work forge(kind=bug, ...)
	// with the typed payload. Auto-execute candidate at confidence ≥0.85
	// per design §Filing-dispatch Q5.
	ActionForgeBug ActionKind = "forge_bug"

	// ActionForgeVaultNote fires the vault-note forge flow. Auto-execute
	// candidate at confidence ≥0.85 per design §Filing-dispatch Q5.
	ActionForgeVaultNote ActionKind = "forge_vault_note"

	// ActionSkillUpdate patches an existing skill body. Surfaces for
	// confirm at any confidence per design §Filing-dispatch Q5 — skill
	// updates edit live skill files, which crosses the filing/fixing
	// line that bounds auto-execute's blast radius.
	ActionSkillUpdate ActionKind = "skill_update"

	// ActionMemoryWrite writes an auto-memory entry under
	// ~/.claude/projects/<dir>/memory/. Auto-execute candidate at
	// confidence ≥0.85 per design §Filing-dispatch Q5.
	ActionMemoryWrite ActionKind = "memory_write"

	// ActionForgeSuggestion fires mcp__toolkit-server__work
	// forge(schema_name=suggestion, ...) with the typed payload.
	// Distinct from forge_bug per chain `agent-suggestion-box`: suggestions
	// are forward-looking proposals to revisit a past decision, bugs are
	// observed friction. The verbatim friction-vs-suggestion definition
	// is loaded from ~/.claude/skills/suggestion-filing-discipline/SKILL.md
	// at prompt-compose time so the agent and Qwen apply the same rule.
	// Auto-execute candidate at confidence ≥ autoExecuteConfidence (0.90).
	ActionForgeSuggestion ActionKind = "forge_suggestion"

	// ActionNothingToFile is the explicit "no signal in this snapshot"
	// outcome. Payload is null; reasoning carries the why-this-is-empty
	// rationale. Distinct from a failed dispatch — telemetry records
	// it as a real review-with-no-signal datapoint.
	ActionNothingToFile ActionKind = "nothing_to_file"
)

// allActions is the closed enum used by ValidateDecision and by the
// compose.go prompt builder so the structured-output schema and the
// validator agree on the action set.
var allActions = []ActionKind{
	ActionForgeBug,
	ActionForgeVaultNote,
	ActionSkillUpdate,
	ActionMemoryWrite,
	ActionForgeSuggestion,
	ActionNothingToFile,
}

// ForgeBugPayload mirrors the mcp__toolkit-server__work forge(kind=bug)
// shape (the action whitelist binds Qwen to the same fields the user-
// facing forge surface accepts). Severity is enum-validated; tags is
// a comma-separated kebab list.
type ForgeBugPayload struct {
	Title            string `json:"title"`
	ProblemStatement string `json:"problem_statement"`
	Surface          string `json:"surface,omitempty"`
	Severity         string `json:"severity,omitempty"`
	Tags             string `json:"tags,omitempty"`
}

// ForgeVaultNotePayload mirrors the vault-note forge shape — see
// vault-filing-discipline.SKILL.md for the subdir routing rules (Qwen
// proposes the kind; the dispatcher routes to decisions/ vs learnings/
// vs reference/ at write time).
type ForgeVaultNotePayload struct {
	NoteKind string `json:"note_kind"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Tags     string `json:"tags,omitempty"`
}

// SkillUpdatePayload describes a patch to an existing skill body.
// patch_kind is one of {add_section, extend_paragraph, add_trigger}
// per design §Structured-output-schema; the dispatcher rejects others.
type SkillUpdatePayload struct {
	SkillSlug string `json:"skill_slug"`
	PatchKind string `json:"patch_kind"`
	Content   string `json:"content"`
}

// MemoryWritePayload describes an auto-memory entry. memory_kind is
// the auto-memory taxonomy (user / feedback / project / reference).
type MemoryWritePayload struct {
	MemoryKind  string `json:"memory_kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// ForgeSuggestionPayload mirrors the mcp__toolkit-server__work
// forge(schema_name=suggestion) shape. Native vocabulary throughout —
// priority (NOT severity), tags broadened to span
// testing/lint/docs/tooling/prose/architecture/skill/workflow per
// suggestion-filing-discipline. Resolution_kind is NOT in the filing
// payload (Qwen only proposes new suggestions; resolution is a separate
// MCP action via suggestion_resolve). See chain `agent-suggestion-box`
// design_decisions §6 for the threshold rationale.
type ForgeSuggestionPayload struct {
	Title              string `json:"title"`
	ProblemStatement   string `json:"problem_statement"`
	Surface            string `json:"surface,omitempty"`
	Priority           string `json:"priority,omitempty"`
	Source             string `json:"source,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	Constraints        string `json:"constraints,omitempty"`
	Tags               string `json:"tags,omitempty"`
}

// FilingDecision is one element of the structured Qwen response. The
// Payload is decoded lazily so a malformed payload for action X doesn't
// poison the rest of the array — ValidateDecision is the single place
// that unmarshals against the action-specific shape.
type FilingDecision struct {
	Action     ActionKind      `json:"action"`
	Payload    json.RawMessage `json:"payload"`
	Confidence float64         `json:"confidence"`
	Reasoning  string          `json:"reasoning"`
	// DedupedAgainst is non-nil when F2's pre-filing dedupe pass
	// (chain arc-close-filing-review-dedupe-and-noise-reduction)
	// matched this decision against an existing artifact in
	// bug_list / suggestion_list / vault index. The partition step
	// demotes matched decisions to a less-aggressive bucket
	// (auto_execute → surface_for_confirm; surface_for_confirm
	// → skip). Telemetry consumers surface the match for operator
	// visibility ("this is similar to existing X").
	DedupedAgainst *DedupeMatch `json:"deduped_against,omitempty"`
	// SameSessionDedupedAgainst is non-nil when F3's same-session
	// dedupe pass matched this decision against a prior arc-close
	// fire in the same session_id within the retention window.
	// Same demotion shape as DedupedAgainst.
	SameSessionDedupedAgainst *SameSessionMatch `json:"same_session_deduped_against,omitempty"`
	// EnrichExisting is non-nil when T6's same-session dedup guard
	// (chain arc-close-decision-authoring-split) matched this decision
	// against an artifact the in-session agent ALREADY FILED this session
	// (its own forge / MemoryWritten / in-snapshot vault forge). The
	// decision is then downgraded from "author/forge a new note" to
	// "suggest enriching the existing one" — partitionDecisions demotes it
	// to surface_for_confirm and both dispatch surfaces render an
	// enrich-existing prompt naming the existing artifact, rather than
	// staging a duplicate. Distinct from SameSessionDedupedAgainst (F3),
	// which matches prior QWEN PROPOSALS; this matches the agent's actual
	// filings. Scoped to same-session agent filings only — does not
	// re-solve general vault-semantic-dedup (bug 899).
	EnrichExisting *EnrichExistingMatch `json:"enrich_existing,omitempty"`
	// StagedForAuthoring is set by partitionDecisions when an in-scope
	// body-heavy decision (forge_vault_note / memory_write) in the
	// auto-execute band is staged for the in-session agent to AUTHOR,
	// rather than auto-forged with Qwen's draft body (chain
	// arc-close-decision-authoring-split T4). Both dispatch surfaces
	// (the Stop hook + the pending-decisions drain hook) read this flag
	// to render an authoring prompt — Qwen attributed as decider, agent
	// asked to write the body — instead of forging Payload verbatim.
	// Qwen's draft body stays in Payload so the T5 fallback can forge it
	// (flagged unreviewed) if the agent never authors.
	StagedForAuthoring bool `json:"staged_for_authoring,omitempty"`
}

// ArcReviewResult is the parsed structured output from a single review
// fire. Decisions and Summary come from Qwen; LatencyMS / token counts
// are populated by DispatchReview from the router result.
//
// RejectedDecisions is populated by the F4 content-validation pass
// in dispatch.go's keep-loop: decisions that pass ValidateDecision
// (shape-correct) but trip a CheckBoilerplate rule (content-noise)
// land here instead of in Decisions. Telemetry consumers
// (arc_review_audit, the ArcCloseFilingReviewed event) surface the
// rejection counts for F5's retrospective measurement.
type ArcReviewResult struct {
	Decisions         []FilingDecision   `json:"filing_decisions"`
	RejectedDecisions []RejectedDecision `json:"rejected_decisions,omitempty"`
	Summary           string             `json:"summary"`
	ArcSummary        string             `json:"-"`
	LatencyMS         int64              `json:"-"`
	InputTokens       *int64             `json:"-"`
	OutputTokens      *int64             `json:"-"`
}

// ErrUnknownAction signals an action string outside the closed enum.
// Returned by ValidateDecision; dispatcher logs and skips the offender.
type ErrUnknownAction struct{ Action string }

func (e *ErrUnknownAction) Error() string {
	return "unknown action: " + e.Action + " (allowed: " + allActionsList() + ")"
}

// ErrMissingPayload is returned when a non-nothing_to_file decision
// arrives with a null or empty payload.
type ErrMissingPayload struct{ Action string }

func (e *ErrMissingPayload) Error() string {
	return "missing payload for action " + e.Action
}

// ErrInvalidConfidence is returned when confidence falls outside
// the closed [0, 1] range.
type ErrInvalidConfidence struct{ Value float64 }

func (e *ErrInvalidConfidence) Error() string {
	return fmt.Sprintf("confidence %g outside [0, 1]", e.Value)
}

// ErrInvalidPayloadShape wraps a JSON-decoding failure for the
// action-specific payload. The wrapped err is the underlying
// json.Unmarshal error so the dispatch layer can log structurally.
type ErrInvalidPayloadShape struct {
	Action string
	Err    error
}

func (e *ErrInvalidPayloadShape) Error() string {
	return "invalid payload shape for action " + e.Action + ": " + e.Err.Error()
}

func (e *ErrInvalidPayloadShape) Unwrap() error { return e.Err }

// ErrInvalidPayloadField names a payload field that failed shape rules
// (required field empty, enum field out of range). Distinct from
// ErrInvalidPayloadShape, which wraps the structural JSON decode.
type ErrInvalidPayloadField struct {
	Action string
	Field  string
	Reason string
}

func (e *ErrInvalidPayloadField) Error() string {
	return "invalid payload for action " + e.Action + ": field " + e.Field + " " + e.Reason
}

// validNoteKinds is the closed enum for ForgeVaultNotePayload.NoteKind.
var validNoteKinds = map[string]bool{
	"decision":  true,
	"learning":  true,
	"reference": true,
}

// validPatchKinds is the closed enum for SkillUpdatePayload.PatchKind.
var validPatchKinds = map[string]bool{
	"add_section":      true,
	"extend_paragraph": true,
	"add_trigger":      true,
}

// validMemoryKinds is the closed enum for MemoryWritePayload.MemoryKind.
// Mirrors the four auto-memory types in ~/.claude/CLAUDE.md.
var validMemoryKinds = map[string]bool{
	"user":      true,
	"feedback":  true,
	"project":   true,
	"reference": true,
}

// validSeverities is the closed enum for ForgeBugPayload.Severity.
var validSeverities = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
}

// validSuggestionPriorities is the closed enum for
// ForgeSuggestionPayload.Priority. Same string set as validSeverities
// but kept distinct so a future rename of one vocab doesn't accidentally
// move the other. Native vocabulary per chain `agent-suggestion-box`.
var validSuggestionPriorities = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
}

// ValidateDecision returns nil when d's action is in the closed enum,
// confidence sits within [0, 1], and (for non-nothing_to_file actions)
// the payload decodes against the action-specific Go struct with all
// required string fields non-empty. Returns one of the typed errors
// above on each failure path. Per design §Failure-modes the dispatcher
// logs the validator error and skips the offending decision; the rest
// of the array still flows through.
func ValidateDecision(d FilingDecision) error {
	if !isKnownAction(d.Action) {
		return &ErrUnknownAction{Action: string(d.Action)}
	}
	if d.Confidence < 0 || d.Confidence > 1 {
		return &ErrInvalidConfidence{Value: d.Confidence}
	}
	if d.Action == ActionNothingToFile {
		// Payload may be null or absent; reasoning carries the why-empty
		// rationale. No shape validation required.
		return nil
	}
	if len(d.Payload) == 0 || string(d.Payload) == "null" {
		return &ErrMissingPayload{Action: string(d.Action)}
	}
	switch d.Action {
	case ActionForgeBug:
		var p ForgeBugPayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return &ErrInvalidPayloadShape{Action: string(d.Action), Err: err}
		}
		if strings.TrimSpace(p.Title) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "title", Reason: "required"}
		}
		if strings.TrimSpace(p.ProblemStatement) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "problem_statement", Reason: "required"}
		}
		if p.Severity != "" && !validSeverities[p.Severity] {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "severity", Reason: "must be one of low|medium|high"}
		}
	case ActionForgeVaultNote:
		var p ForgeVaultNotePayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return &ErrInvalidPayloadShape{Action: string(d.Action), Err: err}
		}
		if strings.TrimSpace(p.Title) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "title", Reason: "required"}
		}
		if strings.TrimSpace(p.Body) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "body", Reason: "required"}
		}
		if !validNoteKinds[p.NoteKind] {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "note_kind", Reason: "must be one of decision|learning|reference"}
		}
	case ActionSkillUpdate:
		var p SkillUpdatePayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return &ErrInvalidPayloadShape{Action: string(d.Action), Err: err}
		}
		if strings.TrimSpace(p.SkillSlug) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "skill_slug", Reason: "required"}
		}
		if strings.TrimSpace(p.Content) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "content", Reason: "required"}
		}
		if !validPatchKinds[p.PatchKind] {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "patch_kind", Reason: "must be one of add_section|extend_paragraph|add_trigger"}
		}
	case ActionMemoryWrite:
		var p MemoryWritePayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return &ErrInvalidPayloadShape{Action: string(d.Action), Err: err}
		}
		if strings.TrimSpace(p.Name) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "name", Reason: "required"}
		}
		if strings.TrimSpace(p.Description) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "description", Reason: "required"}
		}
		if strings.TrimSpace(p.Body) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "body", Reason: "required"}
		}
		if !validMemoryKinds[p.MemoryKind] {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "memory_kind", Reason: "must be one of user|feedback|project|reference"}
		}
	case ActionForgeSuggestion:
		var p ForgeSuggestionPayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return &ErrInvalidPayloadShape{Action: string(d.Action), Err: err}
		}
		if strings.TrimSpace(p.Title) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "title", Reason: "required"}
		}
		if strings.TrimSpace(p.ProblemStatement) == "" {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "problem_statement", Reason: "required"}
		}
		if p.Priority != "" && !validSuggestionPriorities[p.Priority] {
			return &ErrInvalidPayloadField{Action: string(d.Action), Field: "priority", Reason: "must be one of low|medium|high"}
		}
	}
	return nil
}

// isKnownAction reports whether a is in the closed enum. Linear scan
// over allActions — five entries, no map needed.
func isKnownAction(a ActionKind) bool {
	for _, k := range allActions {
		if a == k {
			return true
		}
	}
	return false
}

// allActionsList joins the enum into a comma-separated string for
// ErrUnknownAction.Error().
func allActionsList() string {
	parts := make([]string, len(allActions))
	for i, a := range allActions {
		parts[i] = string(a)
	}
	return strings.Join(parts, ", ")
}
