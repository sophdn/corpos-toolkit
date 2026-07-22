package construct

import (
	"errors"

	"toolkit/internal/forge/fieldvalue"
)

// Result-envelope types for the forge-shaped create/edit/delete dispatch,
// relocated from forge/types.go in chain 311 T7 Stage 6 P2-C.2 (the forge
// archive). These are the agent-facing response shapes the record-sugar
// dispatch (cmd/toolkit-server handleAgentForge/Edit/Delete) returns; their
// JSON tags + omitempty behavior are pinned by the char-net, so the wire shape
// stays byte-identical to forge's. Each struct unifies the success path and the
// error-envelope path into a single value with omitempty fields, mirroring the
// multi-shape-response pattern in vault
// reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md.

// ForgeCreateResult is the response shape for the forge-shaped create. On
// success Ok is true and the populated success fields carry the artifact
// metadata. On rejection Error carries the message and the matching envelope
// fields (Registered, Hint, etc.) are filled per the error path.
type ForgeCreateResult struct {
	// Success path
	Ok         bool   `json:"ok,omitempty"`
	SchemaName string `json:"schema_name,omitempty"`
	Slug       string `json:"slug,omitempty"`
	// Action is the verb describing what happened on disk / in the DB
	// ("created" / "updated" — vault-note's same-slug update policy).
	Action       string `json:"action,omitempty"`
	ArtifactPath string `json:"artifact_path,omitempty"`
	// RoutingNote (bug 1433) is a one-line summary of any caller-influenced
	// routing decision the create made (currently vault-note).
	RoutingNote   string `json:"routing_note,omitempty"`
	DBMirrorError string `json:"db_mirror_error,omitempty"`
	// PostCommitError carries a markdown shape's fail-open post-commit emit
	// error; the artifact already landed (commit NOT rolled back) so it rides
	// alongside Ok=true.
	PostCommitError  string `json:"post_commit_error,omitempty"`
	AfterCreateError string `json:"after_create_error,omitempty"`

	// Error-envelope path. DetectedEnvelope is set on missing_required /
	// mixed_envelope rejections so the caller knows which shape the validator
	// inspected; mixed_envelope also fills SeenAtTopLevel / SeenInFields.
	Error            string   `json:"error,omitempty"`
	Registered       []string `json:"registered,omitempty"`
	Hint             string   `json:"hint,omitempty"`
	SchemaFields     []string `json:"schema_fields,omitempty"`
	Field            string   `json:"field,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Message          string   `json:"message,omitempty"`
	DetectedEnvelope string   `json:"detected_envelope,omitempty"`
	SeenAtTopLevel   []string `json:"seen_at_top_level,omitempty"`
	SeenInFields     []string `json:"seen_in_fields,omitempty"`
}

// ForgeEditResult is the response shape for the forge-shaped edit.
type ForgeEditResult struct {
	// Success path
	Ok            bool     `json:"ok,omitempty"`
	SchemaName    string   `json:"schema_name,omitempty"`
	Slug          string   `json:"slug,omitempty"`
	Action        string   `json:"action,omitempty"`
	UpdatedFields []string `json:"updated_fields,omitempty"`
	ArtifactPath  string   `json:"artifact_path,omitempty"`
	Relocated     bool     `json:"relocated,omitempty"`
	// RoutingNote (bug 1433) mirrors the create-path field (vault-note edits).
	RoutingNote    string `json:"routing_note,omitempty"`
	AfterEditError string `json:"after_edit_error,omitempty"`
	// DroppedExtras lists the non-declared frontmatter keys the caller asked
	// to remove via `__drop_extras` AND which were actually present.
	DroppedExtras []string `json:"dropped_extras,omitempty"`

	// Error-envelope path.
	Error            string   `json:"error,omitempty"`
	Registered       []string `json:"registered,omitempty"`
	Hint             string   `json:"hint,omitempty"`
	SchemaFields     []string `json:"schema_fields,omitempty"`
	SupportedOps     []string `json:"supported_ops,omitempty"`
	Field            string   `json:"field,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Message          string   `json:"message,omitempty"`
	DetectedEnvelope string   `json:"detected_envelope,omitempty"`
	SeenAtTopLevel   []string `json:"seen_at_top_level,omitempty"`
	SeenInFields     []string `json:"seen_in_fields,omitempty"`
}

// ForgeDeleteResult is the response shape for the forge-shaped delete.
type ForgeDeleteResult struct {
	// Success path
	Ok         bool   `json:"ok,omitempty"`
	SchemaName string `json:"schema_name,omitempty"`
	Slug       string `json:"slug,omitempty"`

	// Error-envelope path
	Error        string   `json:"error,omitempty"`
	Registered   []string `json:"registered,omitempty"`
	Hint         string   `json:"hint,omitempty"`
	SupportedOps []string `json:"supported_ops,omitempty"`
}

// missingRequiredEnvelope returns the canonical "<name> is required" rejection
// message, byte-identical to forge's (the char-net pins the text).
func missingRequiredEnvelope(name string) string {
	return "forge: " + name + " is required"
}

// firstValidationViolation extracts the leading violation from a
// fieldvalue.ValidationError if err is one; returns ok=false otherwise.
func firstValidationViolation(err error) (field, kind, message string, ok bool) {
	var verr *fieldvalue.ValidationError
	if !errors.As(err, &verr) || len(verr.Violations) == 0 {
		return "", "", "", false
	}
	v := verr.Violations[0]
	return v.Field, v.Kind, v.Message, true
}
