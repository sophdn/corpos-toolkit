package construct

import (
	"fmt"

	"toolkit/internal/forge/fieldvalue"
)

// ── ForgePrep → construct.Input conversion (T7 Stage 4) ────────────────
//
// The work-table `forge` MCP action accepts the full agent envelope (sugar or
// fields:{}), which forge.PrepareForge parses + validates into a ForgePrep. The
// Stage-4 adapter (cmd/toolkit-server) then routes the EVENT-SOURCED covered
// schemas through construct.Create instead of forge's native persistence. This
// file owns the validated-field-map → typed-Input conversion so the field-name
// knowledge lives next to the Input definitions (not scattered in the adapter),
// and reads the map through fieldvalue.StringField / fieldvalue.ListField so the
// extraction can't drift from forge's own.
//
// File schemas (memory/retro/report-card/migration) now route here (they set
// RoutingNote, which construct.CreateResult carries). bench joined them in
// chain 311 T7 Stage 6 P2-A (event-sourced via BenchmarkForged + the
// bench_harnesses fold). The remaining §15 deltas NOT routed here are
// vault-note (no event — re-homes at P2-C) and trained_model (still a forge
// direct-write — deferred to chain trained-model-event-source-migration).

// CreateRoutesToConstruct reports whether a prepared forge create can be served
// by construct.Create with full behavioral parity. True for the event-sourced
// schemas whose construct builders are field-complete: bug, suggestion, task,
// and chain. The one exception is a chain whose `tasks` carry PIPE-mode entries
// (slug|scope|status): construct.ChainWithTasksInput models only full-object
// tasks (per-task rationale required), so a pipe-mode fan-out stays on forge.
func CreateRoutesToConstruct(prep ForgePrep) bool {
	switch prep.SchemaName {
	case "bug", "suggestion", "task", "memory", "retrospective", "report-card", "migration", "bench", "vault-note":
		return true
	case "chain":
		if !prep.ChainTasksWerePeeled {
			return true
		}
		for _, e := range prep.ChainTaskEntries {
			if e.Mode != ChainTaskModeFull {
				return false // pipe-mode → forge owns it
			}
		}
		return true
	default:
		// §15 deltas not yet routed through construct's Create: vault-note
		// (no event — P1-B), bench/trained_model (direct-write — Stage 6).
		return false
	}
}

// chainAnchoredDocSections collects the section fields a forge-validated
// retrospective/report-card edit map carries into construct.ChainAnchoredDocInput.Sections
// — everything EXCEPT the chain_slug + title envelope keys. WriteChainAnchoredDoc
// skips empty entries, so passing the caller's provided sections verbatim is parity-safe.
func chainAnchoredDocSections(v map[string]fieldvalue.FieldValue) map[string]string {
	sections := make(map[string]string, len(v))
	for name := range v {
		if name == "chain_slug" || name == "title" {
			continue
		}
		sections[name] = fieldvalue.StringField(v, name)
	}
	return sections
}

// InputFromForge converts a PrepareForge result into the typed construct.Input
// for the covered event-sourced schemas. Call only when CreateRoutesToConstruct
// returned true. Reads prep.Validated through fieldvalue.StringField / fieldvalue.ListField
// (forge's own extraction) so the marshaled payload matches forge(<schema>)'s
// byte-for-byte.
func InputFromForge(prep ForgePrep) (Input, error) {
	v := prep.Validated
	switch prep.SchemaName {
	case "bug":
		return Input{Bug: &BugInput{
			Slug:                 prep.Slug,
			Title:                fieldvalue.StringField(v, "title"),
			ProblemStatement:     fieldvalue.StringField(v, "problem_statement"),
			Surface:              fieldvalue.StringField(v, "surface"),
			Severity:             fieldvalue.StringField(v, "severity"),
			Source:               fieldvalue.StringField(v, "source"),
			Tags:                 fieldvalue.StringField(v, "tags"),
			AcceptanceCriteria:   fieldvalue.ListField(v, "acceptance_criteria"),
			Constraints:          fieldvalue.StringField(v, "constraints"),
			QwenTaskID:           fieldvalue.StringField(v, "qwen_task_id"),
			RoutedSuggestionSlug: fieldvalue.StringField(v, "routed_suggestion_slug"),
		}}, nil

	case "suggestion":
		return Input{Suggestion: &SuggestionInput{
			Slug:               prep.Slug,
			Title:              fieldvalue.StringField(v, "title"),
			ProblemStatement:   fieldvalue.StringField(v, "problem_statement"),
			Surface:            fieldvalue.StringField(v, "surface"),
			Priority:           fieldvalue.StringField(v, "priority"),
			Source:             fieldvalue.StringField(v, "source"),
			Tags:               fieldvalue.StringField(v, "tags"),
			AcceptanceCriteria: fieldvalue.ListField(v, "acceptance_criteria"),
			Constraints:        fieldvalue.StringField(v, "constraints"),
		}}, nil

	case "task":
		return Input{Task: &TaskInput{
			Slug:               prep.Slug,
			ChainSlug:          fieldvalue.StringField(v, "chain_slug"),
			ProblemStatement:   fieldvalue.StringField(v, "problem_statement"),
			AcceptanceCriteria: fieldvalue.ListField(v, "acceptance_criteria"),
			ContextRequired:    fieldvalue.StringField(v, "context_required"),
			Constraints:        fieldvalue.StringField(v, "constraints"),
			HandoffOutput:      fieldvalue.StringField(v, "handoff_output"),
		}}, nil

	case "chain":
		ci := ChainInput{
			Slug:                prep.Slug,
			Output:              fieldvalue.StringField(v, "output"),
			DesignDecisions:     fieldvalue.StringField(v, "design_decisions"),
			CompletionCondition: fieldvalue.StringField(v, "completion_condition"),
		}
		if !prep.ChainTasksWerePeeled {
			return Input{Chain: &ci}, nil
		}
		tasks := make([]ChainTaskInput, 0, len(prep.ChainTaskEntries))
		for _, e := range prep.ChainTaskEntries {
			if e.Mode != ChainTaskModeFull {
				// Guarded by CreateRoutesToConstruct; defensive.
				return Input{}, fmt.Errorf("construct.InputFromForge: chain task %q is pipe-mode; not routable through construct (use forge)", e.Slug)
			}
			tasks = append(tasks, ChainTaskInput{
				Slug:               e.Slug,
				ProblemStatement:   e.ProblemStatement,
				AcceptanceCriteria: e.AcceptanceCriteria,
				ContextRequired:    e.ContextRequired,
				Constraints:        e.Constraints,
				Rationale:          e.Rationale,
			})
		}
		return Input{ChainWithTasks: &ChainWithTasksInput{ChainInput: ci, Tasks: tasks}}, nil

	case "memory":
		return Input{Memory: &MemoryInput{
			Slug:            prep.Slug,
			Kind:            fieldvalue.StringField(v, "memory_kind"),
			Description:     fieldvalue.StringField(v, "description"),
			Body:            fieldvalue.StringField(v, "body"),
			Source:          fieldvalue.StringField(v, "source"),
			ObservedFirst:   fieldvalue.StringField(v, "observed_first"),
			RecurrenceCount: fieldvalue.StringField(v, "recurrence_count"),
		}}, nil

	case "retrospective":
		return Input{Retrospective: &ChainAnchoredDocInput{
			Slug:      prep.Slug,
			ChainSlug: fieldvalue.StringField(v, "chain_slug"),
			Title:     fieldvalue.StringField(v, "title"),
			Sections:  chainAnchoredDocSections(v),
		}}, nil

	case "report-card":
		return Input{ReportCard: &ChainAnchoredDocInput{
			Slug:      prep.Slug,
			ChainSlug: fieldvalue.StringField(v, "chain_slug"),
			Title:     fieldvalue.StringField(v, "title"),
			Sections:  chainAnchoredDocSections(v),
		}}, nil

	case "migration":
		return Input{Migration: &MigrationInput{
			Slug:      prep.Slug,
			UpSQL:     fieldvalue.StringField(v, "up_sql"),
			Docstring: fieldvalue.StringField(v, "docstring"),
		}}, nil

	case "bench":
		return Input{Bench: &BenchInput{
			Slug:             prep.Slug,
			BinaryPath:       fieldvalue.StringField(v, "binary_path"),
			FlagSet:          fieldvalue.StringField(v, "flag_set"),
			BaselineJSONPath: fieldvalue.StringField(v, "baseline_json_path"),
			ParseOutputAs:    fieldvalue.StringField(v, "parse_output_as"),
			TimeoutMs:        fieldvalue.StringField(v, "timeout_ms"),
			GateMetrics:      fieldvalue.StringField(v, "gate_metrics"),
		}}, nil

	case "vault-note":
		// No-event file create — the validated forge field map rides straight
		// through (BY REFERENCE: createVaultNote mutates it with {subdir}/{date},
		// and the dispatch host's AfterCreate notifier reads the SAME map to build
		// the knowledge_pointer, mirroring forge's in-place prep.Validated thread).
		return Input{VaultNote: &VaultNoteInput{Slug: prep.Slug, Fields: v}}, nil

	default:
		return Input{}, fmt.Errorf("construct.InputFromForge: schema %q is not routed through construct (§15 delta — stays on forge)", prep.SchemaName)
	}
}
