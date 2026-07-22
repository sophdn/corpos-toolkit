package construct

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"toolkit/internal/events"
	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/forge/registry"
	"toolkit/internal/work"
)

// ── Bucket 1: event-sourced create schemas ──────────────────────────────────
//
// These schemas' projection rows are written ENTIRELY by an event fold (e.g.
// BugReported → proj_current_bugs, ChainCreated → proj_chain_status), so
// emitting the typed event through record reproduces forge's projection
// byte-for-byte for equivalent input. The Build* functions in this file are
// pure: they validate + slug-derive + default + marshal, returning a
// work.RecordEvent the caller submits through work.HandleRecord (or, after T2,
// construct.Create handles the submission internally).
//
// Forge's other create schemas are heterogeneous (see file_schemas.go and
// docs/EMIT_SURFACE_PHASE2.md §15 for the 5-bucket finding).

// BugInput carries forge(bug)'s FULL field surface — every field
// events.BugReportedPayload folds into proj_current_bugs, so construct.Create
// reproduces forge(bug) byte-for-byte regardless of which fields the caller
// supplied. Severity defaults to "medium" when empty (forge's blueprint
// default); Slug derives from Title when empty (B-G3, via forge.SlugifyTitle).
// The optional fields (Surface/Source/Tags/AcceptanceCriteria/Constraints/
// QwenTaskID/RoutedSuggestionSlug) map "" → absent in the payload, matching
// forge.optionalStringPtr / listField. (T7 Stage 4: closes the field-incomplete
// gap — buildBug previously emitted only Title/ProblemStatement/Severity.)
type BugInput struct {
	Slug                 string
	Title                string
	ProblemStatement     string
	Surface              string
	Severity             string
	Source               string
	Tags                 string
	AcceptanceCriteria   []string
	Constraints          string
	QwenTaskID           string
	RoutedSuggestionSlug string
}

// buildBug returns a BugReported event: validates required fields, derives
// the slug (B-G3), defaults severity, marshals the payload byte-identical to
// forge(bug)'s (mirrors forge.createBugInTx field-for-field). Package-private —
// construct.Create is the public entry; this runs as a step inside the
// umbrella's orchestration.
func buildBug(project string, in BugInput) (work.RecordEvent, error) {
	if strings.TrimSpace(in.Title) == "" {
		return work.RecordEvent{}, fmt.Errorf("bug: title is required")
	}
	if strings.TrimSpace(in.ProblemStatement) == "" {
		return work.RecordEvent{}, fmt.Errorf("bug: problem_statement is required")
	}
	slug := deriveSlug(in.Slug, in.Title)
	severity := in.Severity
	if severity == "" {
		severity = "medium" // forge's bug.severity default (blueprints/forge-schemas/bug.toml)
	}
	payload, err := json.Marshal(events.BugReportedPayload{
		Title:                in.Title,
		ProblemStatement:     in.ProblemStatement,
		Surface:              optionalStr(in.Surface),
		Severity:             &severity,
		Source:               optionalStr(in.Source),
		Tags:                 optionalStr(in.Tags),
		AcceptanceCriteria:   in.AcceptanceCriteria,
		Constraints:          optionalStr(in.Constraints),
		QwenTaskID:           optionalStr(in.QwenTaskID),
		RoutedSuggestionSlug: optionalStr(in.RoutedSuggestionSlug),
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal BugReported payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "BugReported",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// SuggestionInput carries forge(suggestion)'s FULL field surface — every field
// events.SuggestionReportedPayload folds, so construct.Create reproduces
// forge(suggestion) byte-for-byte. Priority defaults to "medium" (note the
// native priority vocabulary, not severity); Slug derives from Title when empty
// (B-G3, via forge.SlugifyTitle). Optional fields map "" → absent, matching
// forge.createSuggestionInTx. (T7 Stage 4: closes the field-incomplete gap.)
type SuggestionInput struct {
	Slug               string
	Title              string
	ProblemStatement   string
	Surface            string
	Priority           string
	Source             string
	Tags               string
	AcceptanceCriteria []string
	Constraints        string
}

// buildSuggestion returns a SuggestionReported event. Package-private —
// construct.Create dispatches here for the "suggestion" schema. Mirrors
// forge.createSuggestionInTx field-for-field.
func buildSuggestion(project string, in SuggestionInput) (work.RecordEvent, error) {
	if strings.TrimSpace(in.Title) == "" {
		return work.RecordEvent{}, fmt.Errorf("suggestion: title is required")
	}
	if strings.TrimSpace(in.ProblemStatement) == "" {
		return work.RecordEvent{}, fmt.Errorf("suggestion: problem_statement is required")
	}
	slug := deriveSlug(in.Slug, in.Title)
	priority := in.Priority
	if priority == "" {
		priority = "medium" // forge's suggestion.priority default (native vocabulary)
	}
	payload, err := json.Marshal(events.SuggestionReportedPayload{
		Title:              in.Title,
		ProblemStatement:   in.ProblemStatement,
		Surface:            optionalStr(in.Surface),
		Priority:           &priority,
		Source:             optionalStr(in.Source),
		Tags:               optionalStr(in.Tags),
		AcceptanceCriteria: in.AcceptanceCriteria,
		Constraints:        optionalStr(in.Constraints),
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal SuggestionReported payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "SuggestionReported",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// ChainInput carries forge(chain)'s fields. Slug derives from Title via
// forge.SlugifyTitle when empty (B-G3) — chain has no `title` field of its
// own, so the title is consumed for the slug only. design_decisions rides the
// payload (the substrate's source of truth) even though the projection no
// longer caches it (retired in migration 065).
type ChainInput struct {
	Slug                string
	Title               string
	Output              string
	DesignDecisions     string
	CompletionCondition string
}

// buildChain returns a ChainCreated event. Package-private —
// construct.Create dispatches here for ("chain", ChainInput); the fan-out
// shape ("chain", ChainWithTasksInput) routes to buildChainWithTasks.
func buildChain(project string, in ChainInput) (work.RecordEvent, error) {
	if strings.TrimSpace(in.Output) == "" {
		return work.RecordEvent{}, fmt.Errorf("chain: output is required")
	}
	if strings.TrimSpace(in.DesignDecisions) == "" {
		return work.RecordEvent{}, fmt.Errorf("chain: design_decisions is required")
	}
	if strings.TrimSpace(in.CompletionCondition) == "" {
		return work.RecordEvent{}, fmt.Errorf("chain: completion_condition is required")
	}
	slug := deriveSlug(in.Slug, in.Title)
	if slug == "" {
		return work.RecordEvent{}, fmt.Errorf("chain: slug is required (give slug, or a title to derive it from)")
	}
	payload, err := json.Marshal(events.ChainCreatedPayload{
		Output:              in.Output,
		DesignDecisions:     in.DesignDecisions,
		CompletionCondition: in.CompletionCondition,
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal ChainCreated payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "ChainCreated",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// TaskInput carries forge(task)'s fields. The slug is supplied directly (task
// has no `title` field). Position is intentionally NOT here: foldTaskCreated
// assigns MAX(position)+1 within the chain at fold time, so the construction
// layer must not set it (§15) — letting the fold own it keeps record's
// sequential-within-one-call positions correct and matches forge's row.
// AcceptanceCriteria is the list shape; the fold joins it on "\n- ".
type TaskInput struct {
	Slug               string
	ChainSlug          string
	ProblemStatement   string
	AcceptanceCriteria []string
	ContextRequired    string
	Constraints        string
	HandoffOutput      string
}

// buildTask returns a TaskCreated event. Package-private — construct.Create
// dispatches here for "task". The chain must already exist (be folded) for
// the TaskCreated fold to land the row.
func buildTask(project string, in TaskInput) (work.RecordEvent, error) {
	if strings.TrimSpace(in.ChainSlug) == "" {
		return work.RecordEvent{}, fmt.Errorf("task: chain_slug is required")
	}
	if strings.TrimSpace(in.ProblemStatement) == "" {
		return work.RecordEvent{}, fmt.Errorf("task: problem_statement is required")
	}
	if strings.TrimSpace(in.Slug) == "" {
		return work.RecordEvent{}, fmt.Errorf("task: slug is required")
	}
	payload, err := json.Marshal(events.TaskCreatedPayload{
		ChainSlug:          in.ChainSlug,
		ProblemStatement:   in.ProblemStatement,
		AcceptanceCriteria: in.AcceptanceCriteria,
		ContextRequired:    optionalStr(in.ContextRequired),
		Constraints:        optionalStr(in.Constraints),
		HandoffOutput:      optionalStr(in.HandoffOutput),
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal TaskCreated payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "TaskCreated",
		EntitySlug:      in.Slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// ChainTaskInput is one full-object task entry for the chain+tasks fan-out
// (forge's full-object mode). Rationale is mandatory — forge rejects a
// full-object entry that omits it (B-C3), and so does BuildChainWithTasks.
type ChainTaskInput struct {
	Slug               string
	ProblemStatement   string
	AcceptanceCriteria []string
	ContextRequired    string
	Constraints        string
	Rationale          string
}

// ChainWithTasksInput is the chain+tasks atomic fan-out input: the chain
// fields plus the per-task entries. Submit the returned slice as ONE
// record(events[]) call with strict_all_or_nothing=true for forge's atomic
// semantics — chain + every task land together or not at all.
type ChainWithTasksInput struct {
	ChainInput
	Tasks []ChainTaskInput
}

// buildChainWithTasks reproduces forge(chain, tasks=[full-object…]) as the
// event sequence record expresses it: ChainCreated FIRST (so each TaskCreated
// fold resolves the chain), one TaskCreated per task carrying its per-task
// rationale, then an OPTIONAL ChainAndTasksForged grouping signal (no fold
// consumes it; included so the event log carries forge's parent+children
// marker). Position is omitted on every TaskCreated (the fold assigns 1..N
// sequentially in the one tx — same as forge's hook). Package-private —
// construct.Create dispatches here for ("chain", ChainWithTasksInput).
func buildChainWithTasks(project string, in ChainWithTasksInput) ([]work.RecordEvent, error) {
	chainEv, err := buildChain(project, in.ChainInput)
	if err != nil {
		return nil, err
	}
	chainSlug := chainEv.EntitySlug
	evs := make([]work.RecordEvent, 0, len(in.Tasks)+2)
	evs = append(evs, chainEv)
	taskSlugs := make([]string, 0, len(in.Tasks))
	rationales := make([]string, 0, len(in.Tasks))
	for i, tk := range in.Tasks {
		if strings.TrimSpace(tk.Rationale) == "" {
			// Forge rejects the WHOLE call pre-write when a full-object entry
			// omits its rationale (B-C3); reject here before returning any event.
			return nil, fmt.Errorf("task[%d] (%q): rationale is required (the per-task 'why this task in this chain' grain)", i, tk.Slug)
		}
		taskEv, err := buildTask(project, TaskInput{
			Slug:               tk.Slug,
			ChainSlug:          chainSlug,
			ProblemStatement:   tk.ProblemStatement,
			AcceptanceCriteria: tk.AcceptanceCriteria,
			ContextRequired:    tk.ContextRequired,
			Constraints:        tk.Constraints,
		})
		if err != nil {
			return nil, fmt.Errorf("task[%d]: %w", i, err)
		}
		taskEv.Rationale = tk.Rationale
		evs = append(evs, taskEv)
		taskSlugs = append(taskSlugs, tk.Slug)
		rationales = append(rationales, tk.Rationale)
	}
	// Optional grouping event (parity with forge's after_create hook). No fold
	// consumes it, so it never affects the chain/task projection rows.
	groupPayload, err := json.Marshal(events.ChainAndTasksForgedPayload{
		ChainSlug:         chainSlug,
		TaskSlugs:         taskSlugs,
		TaskCount:         len(taskSlugs),
		Mode:              "full_objects",
		PerTaskRationales: rationales,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ChainAndTasksForged payload: %w", err)
	}
	pid := project
	evs = append(evs, work.RecordEvent{
		Type:            "ChainAndTasksForged",
		EntitySlug:      chainSlug,
		EntityProjectID: &pid,
		Payload:         groupPayload,
	})
	return evs, nil
}

// ── Bucket 1 (edit arm): typed sparse-update inputs ─────────────────────────
//
// Edit shape differs from Create: omitted fields keep their prior value, and
// "" is a legal new value (set-to-empty), so the typed input uses *string /
// *[]string pointers — nil means "leave unchanged", non-nil means "set". The
// builders below convert the typed pointers into a fieldvalue.FieldValue map (the
// shape forge.ValidatePartial + the placeholder guard already understand),
// then marshal the UpdatedFields + UpdatedValues map into the typed *Edited
// event payload byte-identical to forge_edit(<schema>, …)'s.
//
// Set-by-lifecycle fields (e.g. bug.status, bug.resolution_note —
// set_by="bug_resolve") are NOT exposed on these inputs: the lifecycle action
// owns them, and a forge_edit / construct.Update attempt is rejected by B-ED2
// before reaching the builder. Routing fields (routed_chain_slug /
// routed_task_slug / routed_suggestion_slug) likewise ride on the lifecycle
// event payload (BugResolved.RoutedXSlug), not on a forge_edit, so they're
// not on the edit input. If a future caller needs them on a plain edit, add
// the *string pointer here + a guards.go set-by audit.

// BugEditInput is the typed sparse-update form of BugInput for
// construct.Update("bug", …). Slug identifies the row; pointer fields are
// "set when non-nil." nil pointers stay untouched in the projection.
type BugEditInput struct {
	Slug               string
	Title              *string
	ProblemStatement   *string
	Severity           *string
	Surface            *string
	Source             *string
	Tags               *string
	AcceptanceCriteria *[]string
	Constraints        *string
}

// fieldMap returns the fieldvalue.FieldValue map representing this edit's
// non-nil fields. Keys are the snake_case schema field names; values are
// SingleValue / ListValue per the forge convention. Empty map means "no
// updates supplied" — construct.Update treats that as a caller error,
// matching forge_edit's "no field updates supplied" rejection.
func (in BugEditInput) fieldMap() map[string]fieldvalue.FieldValue {
	out := map[string]fieldvalue.FieldValue{}
	if in.Title != nil {
		out["title"] = fieldvalue.SingleValue(*in.Title)
	}
	if in.ProblemStatement != nil {
		out["problem_statement"] = fieldvalue.SingleValue(*in.ProblemStatement)
	}
	if in.Severity != nil {
		out["severity"] = fieldvalue.SingleValue(*in.Severity)
	}
	if in.Surface != nil {
		out["surface"] = fieldvalue.SingleValue(*in.Surface)
	}
	if in.Source != nil {
		out["source"] = fieldvalue.SingleValue(*in.Source)
	}
	if in.Tags != nil {
		out["tags"] = fieldvalue.SingleValue(*in.Tags)
	}
	if in.AcceptanceCriteria != nil {
		out["acceptance_criteria"] = fieldvalue.ListValue(*in.AcceptanceCriteria)
	}
	if in.Constraints != nil {
		out["constraints"] = fieldvalue.SingleValue(*in.Constraints)
	}
	return out
}

// buildEditBug returns a BugEdited event with UpdatedFields/UpdatedValues
// byte-identical to forge_edit(bug, …)'s payload for equivalent fields.
// Package-private; construct.Update is the public entry. Pre-emit guards
// (B-G1 placeholder, B-ED1 partial-validate, B-ED2 set-by) run in the
// umbrella before this is called, so the validated map arrives ready to
// marshal.
func buildEditBug(project, slug string, validated map[string]fieldvalue.FieldValue) (work.RecordEvent, error) {
	updated, values := updatedFieldsAndValues(validated)
	payload, err := json.Marshal(events.BugEditedPayload{
		UpdatedFields: updated,
		UpdatedValues: values,
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal BugEdited payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "BugEdited",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// SuggestionEditInput is the sparse-update form of SuggestionInput.
type SuggestionEditInput struct {
	Slug               string
	Title              *string
	ProblemStatement   *string
	Priority           *string
	Surface            *string
	Source             *string
	Tags               *string
	AcceptanceCriteria *[]string
	Constraints        *string
}

func (in SuggestionEditInput) fieldMap() map[string]fieldvalue.FieldValue {
	out := map[string]fieldvalue.FieldValue{}
	if in.Title != nil {
		out["title"] = fieldvalue.SingleValue(*in.Title)
	}
	if in.ProblemStatement != nil {
		out["problem_statement"] = fieldvalue.SingleValue(*in.ProblemStatement)
	}
	if in.Priority != nil {
		out["priority"] = fieldvalue.SingleValue(*in.Priority)
	}
	if in.Surface != nil {
		out["surface"] = fieldvalue.SingleValue(*in.Surface)
	}
	if in.Source != nil {
		out["source"] = fieldvalue.SingleValue(*in.Source)
	}
	if in.Tags != nil {
		out["tags"] = fieldvalue.SingleValue(*in.Tags)
	}
	if in.AcceptanceCriteria != nil {
		out["acceptance_criteria"] = fieldvalue.ListValue(*in.AcceptanceCriteria)
	}
	if in.Constraints != nil {
		out["constraints"] = fieldvalue.SingleValue(*in.Constraints)
	}
	return out
}

// buildEditSuggestion returns a SuggestionEdited event byte-identical to
// forge_edit(suggestion, …) for equivalent fields.
func buildEditSuggestion(project, slug string, validated map[string]fieldvalue.FieldValue) (work.RecordEvent, error) {
	updated, values := updatedFieldsAndValues(validated)
	payload, err := json.Marshal(events.SuggestionEditedPayload{
		UpdatedFields: updated,
		UpdatedValues: values,
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal SuggestionEdited payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "SuggestionEdited",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// updatedFieldsAndValues converts a validated FieldValue map into the
// (UpdatedFields, UpdatedValues) pair every *EditedPayload carries. Sorting
// UpdatedFields keeps the payload deterministic across runs (the FieldValue
// map is iteration-random); UpdatedValues mirrors the same keys, with each
// value rendered via FieldValue.AsJoined() so lists become the "\n- "-joined
// storage form (the forge convention).
func updatedFieldsAndValues(validated map[string]fieldvalue.FieldValue) ([]string, map[string]string) {
	names := make([]string, 0, len(validated))
	for k := range validated {
		names = append(names, k)
	}
	sort.Strings(names)
	values := make(map[string]string, len(validated))
	for _, n := range names {
		values[n] = validated[n].AsJoined()
	}
	return names, values
}

// ── Bucket 1 (edit arm): chain + task ──────────────────────────────────────

// ChainEditInput is the sparse-update form of ChainInput. Slug identifies the
// row; pointer fields are "set when non-nil." design_decisions stays on the
// payload (the substrate's source of truth) even though the projection no
// longer folds it (the proj_chain_status.design_decisions column was retired
// in migration 065 — Phase 4 F2).
type ChainEditInput struct {
	Slug                string
	Output              *string
	DesignDecisions     *string
	CompletionCondition *string
}

func (in ChainEditInput) fieldMap() map[string]fieldvalue.FieldValue {
	out := map[string]fieldvalue.FieldValue{}
	if in.Output != nil {
		out["output"] = fieldvalue.SingleValue(*in.Output)
	}
	if in.DesignDecisions != nil {
		out["design_decisions"] = fieldvalue.SingleValue(*in.DesignDecisions)
	}
	if in.CompletionCondition != nil {
		out["completion_condition"] = fieldvalue.SingleValue(*in.CompletionCondition)
	}
	return out
}

// buildEditChain returns a ChainEdited event byte-identical to
// forge_edit(chain, …) for equivalent fields.
func buildEditChain(project, slug string, validated map[string]fieldvalue.FieldValue) (work.RecordEvent, error) {
	updated, values := updatedFieldsAndValues(validated)
	payload, err := json.Marshal(events.ChainEditedPayload{
		UpdatedFields: updated,
		UpdatedValues: values,
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal ChainEdited payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "ChainEdited",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// TaskEditInput is the sparse-update form of TaskInput. ChainSlug is
// REQUIRED on the edit path: forge_edit on tasks rejects without it
// (editTaskInTx surfaces "forge_edit on tasks requires chain_slug"),
// and the TaskEdited payload carries it for the anti-fanout fold guard
// (taskTargetWhere — bug `task-lifecycle-event-folds-fan-out-across-
// duplicate-task-slugs`).
type TaskEditInput struct {
	Slug               string
	ChainSlug          string
	ProblemStatement   *string
	AcceptanceCriteria *[]string
	ContextRequired    *string
	Constraints        *string
	HandoffOutput      *string
}

func (in TaskEditInput) fieldMap() map[string]fieldvalue.FieldValue {
	out := map[string]fieldvalue.FieldValue{}
	if in.ProblemStatement != nil {
		out["problem_statement"] = fieldvalue.SingleValue(*in.ProblemStatement)
	}
	if in.AcceptanceCriteria != nil {
		out["acceptance_criteria"] = fieldvalue.ListValue(*in.AcceptanceCriteria)
	}
	if in.ContextRequired != nil {
		out["context_required"] = fieldvalue.SingleValue(*in.ContextRequired)
	}
	if in.Constraints != nil {
		out["constraints"] = fieldvalue.SingleValue(*in.Constraints)
	}
	if in.HandoffOutput != nil {
		out["handoff_output"] = fieldvalue.SingleValue(*in.HandoffOutput)
	}
	return out
}

// buildEditTask returns a TaskEdited event with the chain-scoped anti-fanout
// disambiguator stamped on the payload, byte-identical to forge_edit(task, …).
func buildEditTask(project, slug, chainSlug string, validated map[string]fieldvalue.FieldValue) (work.RecordEvent, error) {
	if chainSlug == "" {
		return work.RecordEvent{}, fmt.Errorf("task: chain_slug is required on the edit path (anti-fanout disambiguator — see task-lifecycle-event-folds-fan-out-across-duplicate-task-slugs)")
	}
	updated, values := updatedFieldsAndValues(validated)
	payload, err := json.Marshal(events.TaskEditedPayload{
		ChainSlug:     chainSlug,
		UpdatedFields: updated,
		UpdatedValues: values,
	})
	if err != nil {
		return work.RecordEvent{}, fmt.Errorf("marshal TaskEdited payload: %w", err)
	}
	pid := project
	return work.RecordEvent{
		Type:            "TaskEdited",
		EntitySlug:      slug,
		EntityProjectID: &pid,
		Payload:         payload,
	}, nil
}

// editableSchemaFields returns the names of fields declared on this schema
// that are NOT owned by a lifecycle action (i.e. fd.SetBy == ""). Used by
// the set-by audit (B-ED2) and by tests as the source of truth for which
// fields a typed XEditInput is allowed to surface.
func editableSchemaFields(schema registry.Schema) []string {
	out := make([]string, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if f.SetBy != "" {
			continue
		}
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}
