// Result types for every work meta-tool handler. One named struct per
// handler keeps the JSON wire format pinned (omitempty fields drop
// zero values verbatim to match the prior map-based shape) and lets
// every Handle* function in this package return a concrete type.
// Widening to `any` happens once per registration via the dispatch.Adapt
// helpers in table.go; nothing else in this package returns `any`.
//
// Discriminated-union shapes (chain_status with/without slug, bug_list
// across titles_only/verbose/default, library_find equivalents) use a
// single struct with optional typed slices and a custom MarshalJSON
// that picks the right inline shape per mode — mirrors the pattern
// established in knowledge.LibraryFindResult.
package work

import (
	"encoding/json"

	"toolkit/internal/mcpresult"
)

// ErrorEnvelope re-exports the canonical envelope shape from
// internal/mcpresult so existing callers across the work package
// keep using `ErrorEnvelope{...}` literals unchanged. The wire shape
// is owned by mcpresult.ErrorEnvelope — `error` always present,
// `hint` / `action` / `editable_fields` / `chains` populated only
// when the handler had useful context.
type ErrorEnvelope = mcpresult.ErrorEnvelope

// ---------- bug surface ----------

// BugListResult discriminates the three projections bug_list emits
// (titles_only / verbose / default) plus the project-or-filter error
// envelope. Exactly one of TitlesItems / Verbose / Default populates,
// picked by the corresponding bool flag.
type BugListResult struct {
	TitlesOnly   bool
	Verbose      bool
	TitlesItems  []BugListTitle
	VerboseItems []Bug
	DefaultItems []BugListItem
	Err          *ErrorEnvelope
}

// MarshalJSON emits the per-mode JSON shape. Lists marshal as JSON
// arrays directly; errors marshal as the structured envelope. Stays
// hand-rolled because the three-branch discriminator
// (titles_only / verbose / default) doesn't fit mcpresult's two-
// branch OkOrError shape.
func (r BugListResult) MarshalJSON() ([]byte, error) {
	if r.Err != nil {
		return json.Marshal(r.Err)
	}
	if r.TitlesOnly {
		return json.Marshal(r.TitlesItems)
	}
	if r.Verbose {
		return json.Marshal(r.VerboseItems)
	}
	return json.Marshal(r.DefaultItems)
}

// BugReadResult emits either the typed Bug row or an error envelope.
type BugReadResult struct {
	Bug *Bug
	Err *ErrorEnvelope
}

// MarshalJSON unwraps the populated branch.
func (r BugReadResult) MarshalJSON() ([]byte, error) {
	return mcpresult.MarshalOkOrError(r.Bug, r.Err)
}

// BugResolveResult is the response shape for bug_resolve / bug_reopen /
// bug_stamp_sha — success or error envelope on the same struct via
// omitempty. OK==true distinguishes the success path.
type BugResolveResult struct {
	OK                bool   `json:"ok,omitempty"`
	Slug              string `json:"slug,omitempty"`
	Status            string `json:"status,omitempty"`
	ResolvedCommitSHA string `json:"resolved_commit_sha,omitempty"`
	// Error path mirrors ErrorEnvelope inline so JSON shape matches
	// the previous map-based form without a wrapping object.
	Error  string `json:"error,omitempty"`
	Hint   string `json:"hint,omitempty"`
	Action string `json:"action,omitempty"`
}

// ---------- suggestion surface ----------

// SuggestionListResult discriminates the three projections suggestion_list
// emits (titles_only / verbose / default). Mirrors BugListResult — same
// shape, distinct table per chain `agent-suggestion-box` design.
type SuggestionListResult struct {
	TitlesOnly   bool
	Verbose      bool
	TitlesItems  []SuggestionListTitle
	VerboseItems []Suggestion
	DefaultItems []SuggestionListItem
	Err          *ErrorEnvelope
}

func (r SuggestionListResult) MarshalJSON() ([]byte, error) {
	if r.Err != nil {
		return json.Marshal(r.Err)
	}
	if r.TitlesOnly {
		return json.Marshal(r.TitlesItems)
	}
	if r.Verbose {
		return json.Marshal(r.VerboseItems)
	}
	return json.Marshal(r.DefaultItems)
}

// SuggestionReadResult emits either the typed Suggestion row or an
// error envelope.
type SuggestionReadResult struct {
	Suggestion *Suggestion
	Err        *ErrorEnvelope
}

func (r SuggestionReadResult) MarshalJSON() ([]byte, error) {
	return mcpresult.MarshalOkOrError(r.Suggestion, r.Err)
}

// SuggestionResolveResult is the response shape for suggestion_resolve /
// suggestion_reopen. OK==true distinguishes the success path. Mirrors
// BugResolveResult.
type SuggestionResolveResult struct {
	OK                bool   `json:"ok,omitempty"`
	Slug              string `json:"slug,omitempty"`
	Status            string `json:"status,omitempty"`
	ResolvedCommitSHA string `json:"resolved_commit_sha,omitempty"`
	Error             string `json:"error,omitempty"`
	Hint              string `json:"hint,omitempty"`
	Action            string `json:"action,omitempty"`
}

// ---------- chain surface ----------

// ChainStatusResult discriminates the three response shapes chain_status
// emits (slug-not-supplied list / slug-match single / slug-no-match
// error envelope).
type ChainStatusResult struct {
	List      []ChainSummary
	Single    *ChainSummary
	Err       *ErrorEnvelope
	HasList   bool
	HasSingle bool
}

// MarshalJSON unwraps to the populated branch's wire form.
func (r ChainStatusResult) MarshalJSON() ([]byte, error) {
	if r.Err != nil {
		return json.Marshal(r.Err)
	}
	if r.HasSingle {
		return json.Marshal(r.Single)
	}
	if r.HasList {
		return json.Marshal(r.List)
	}
	// Empty list (defensive) — emit `[]` not `null`.
	return json.Marshal([]ChainSummary{})
}

// ChainStateResult emits either the typed ChainDetail or an error envelope.
type ChainStateResult struct {
	Detail *ChainDetail
	Err    *ErrorEnvelope
}

func (r ChainStateResult) MarshalJSON() ([]byte, error) {
	return mcpresult.MarshalOkOrError(r.Detail, r.Err)
}

// ChainFindResult emits either a list of summaries or an error envelope.
type ChainFindResult struct {
	List []ChainSummary
	Err  *ErrorEnvelope
}

func (r ChainFindResult) MarshalJSON() ([]byte, error) {
	return mcpresult.MarshalOkOrErrorList(r.List, r.Err)
}

// ChainCloseResult is the bug-style success-or-error envelope for chain_close.
// closure_summary_chars is omitempty so the wire format matches the prior
// conditional emission.
type ChainCloseResult struct {
	OK                  bool   `json:"ok,omitempty"`
	ChainSlug           string `json:"chain_slug,omitempty"`
	ClosureSummaryChars *int   `json:"closure_summary_chars,omitempty"`
	Error               string `json:"error,omitempty"`
	Hint                string `json:"hint,omitempty"`
}

// ---------- task surface ----------

// TaskReadResult discriminates a typed Task row from an error envelope.
// The ambiguous-slug error path carries the candidate chain list so
// callers can disambiguate with a follow-up chain_slug.
type TaskReadResult struct {
	Task *Task
	Err  *ErrorEnvelope
}

func (r TaskReadResult) MarshalJSON() ([]byte, error) {
	return mcpresult.MarshalOkOrError(r.Task, r.Err)
}

// TaskListResult emits either a list of compact task projections or
// an error envelope.
type TaskListResult struct {
	List []TaskListItem
	Err  *ErrorEnvelope
}

func (r TaskListResult) MarshalJSON() ([]byte, error) {
	return mcpresult.MarshalOkOrErrorList(r.List, r.Err)
}

// TaskTransitionResult is the response shape for task_start /
// task_complete / task_cancel / task_reopen / task_block / task_unblock.
// Inline error fields keep the wire format identical to the prior
// map-based envelopes.
type TaskTransitionResult struct {
	OK        bool   `json:"ok,omitempty"`
	Slug      string `json:"slug,omitempty"`
	Status    string `json:"status,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Error     string `json:"error,omitempty"`
	Hint      string `json:"hint,omitempty"`
	Action    string `json:"action,omitempty"`
}

// TaskBlockersResult emits either a list of BlockerEntry or an error envelope.
type TaskBlockersResult struct {
	List []BlockerEntry
	Err  *ErrorEnvelope
}

func (r TaskBlockersResult) MarshalJSON() ([]byte, error) {
	return mcpresult.MarshalOkOrErrorList(r.List, r.Err)
}

// TaskEditResult is the response shape for task_edit. EditableFields
// populates only on the no-fields-supplied error path. FieldsWritten
// names the fields that landed (in spec order); FieldErrors maps any
// per-field validation failure to a hint. A call can return OK=true
// with a non-empty FieldErrors when some fields wrote and others
// didn't validate — bug 1319's partial-update contract.
//
// Errors aggregates envelope-level validation problems (missing slug,
// unknown field names, mutually-exclusive field combinations) so a
// single call returns every problem the handler can detect rather than
// one-at-a-time. The dispatch-layer rationale gate fires before the
// handler runs, so a missing rationale still produces a separate
// envelope — that boundary is structural. Bug 1422.
type TaskEditResult struct {
	OK             bool              `json:"ok,omitempty"`
	Slug           string            `json:"slug,omitempty"`
	FieldsWritten  []string          `json:"fields_written,omitempty"`
	Error          string            `json:"error,omitempty"`
	Errors         []string          `json:"errors,omitempty"`
	FieldErrors    map[string]string `json:"field_errors,omitempty"`
	EditableFields []string          `json:"editable_fields,omitempty"`
}

// ---------- sha helpers ----------

// ShaStampResult is the response shape for bug_stamp_sha / task_stamp_sha.
// The two handlers use different JSON keys for the stamped SHA (Rust
// parity: bug emits `resolved_commit_sha`, task emits `commit_sha`), so
// both keys are typed here with omitempty — the handler populates the
// one its wire format expects.
type ShaStampResult struct {
	OK                bool   `json:"ok,omitempty"`
	Slug              string `json:"slug,omitempty"`
	CommitSHA         string `json:"commit_sha,omitempty"`
	ResolvedCommitSHA string `json:"resolved_commit_sha,omitempty"`
	Error             string `json:"error,omitempty"`
	Hint              string `json:"hint,omitempty"`
	Action            string `json:"action,omitempty"`
}

// ---------- roadmap surface ----------
//
// The roadmap actions' result types live alongside their handlers in
// roadmap.go (RoadmapSetResult, RoadmapPreviewResult, RoadmapInsertResult,
// RoadmapMarkReassessedResult) — each action emits a different field
// set and packaging them with the handler keeps the wire shapes
// adjacent to the code that populates them. The shared RoadmapEntry /
// RoadmapListEntries types are also in roadmap.go.
