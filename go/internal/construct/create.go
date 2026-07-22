package construct

import (
	"context"
	"encoding/json"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
	"toolkit/internal/work"
)

// ── construct.Create — the agent-facing umbrella ────────────────────────────
//
// One call per create operation, schema-name dispatched, internally
// orchestrating the kit-of-parts forge.HandleForge orchestrates for its
// callers (validate → dup-check → build → write/emit → index-notify).
// Mirrors forge(schema, params)'s one-call ergonomics on top of the record
// substrate. Stage 4 callers (arcreview ForgeFn, work.batch's forge ops, the
// work table forge action, the CLIs) route through Create instead of
// assembling the kit themselves; Stage 5 layers a forge-shaped MCP
// affordance on this so the agent's external call shape doesn't change when
// forge archives at Stage 6.

// Deps carries the dependencies Create needs across schemas: the DB Pool
// (record submit + dup-check + index-sync) and the forge schema registry
// (file-schema dispatch + index-sync).
type Deps struct {
	Pool    *db.Pool
	Schemas *registry.Registry
	// OnCreate / OnEdit / BurstTracker are consumed only by the finalize tail
	// (FinalizeForgeCreate / FinalizeForgeEdit), relocated from forge.Deps in
	// chain 311 T7 Stage 6 P2-C.2. Optional — nil disables the SSE/index
	// notifier (OnCreate/OnEdit) or the work.batch burst nudge (BurstTracker).
	// The create/update orchestration (construct.Create / Update) does NOT read
	// them; the dispatch host wires them when it needs the post-persist tail.
	OnCreate     AfterCreateNotifier
	OnEdit       AfterEditNotifier
	BurstTracker *ForgeBurstTracker
}

// Input is the discriminated-union payload for Create. Each schema's typed
// per-schema input rides its own field; the dispatch reads the field matching
// the schema name. Exactly ONE field must be set for the call to be well-
// formed. The "chain" schema accepts either Chain (bare) or ChainWithTasks
// (atomic fan-out) — set exactly one.
//
// Typed end-to-end (the canonical Go pattern over bare any — see vault
// reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md): callers
// construct e.g. Input{Bug: &BugInput{...}} and the dispatcher pulls the
// right typed input out by schema name; the schema↔input cross-check rejects
// mismatched pairs with a clear error.
type Input struct {
	Bug            *BugInput
	Suggestion     *SuggestionInput
	Chain          *ChainInput
	ChainWithTasks *ChainWithTasksInput
	Task           *TaskInput
	Memory         *MemoryInput
	Retrospective  *ChainAnchoredDocInput
	ReportCard     *ChainAnchoredDocInput
	Migration      *MigrationInput
	Bench          *BenchInput
	VaultNote      *VaultNoteInput
}

// CreateResult is the umbrella's return shape: the schema dispatched, the
// entity's slug, the events submitted through record, and (for file schemas)
// the on-disk file path. EntitySlug is the head entity — for the chain+tasks
// fan-out it's the chain's slug; the task slugs ride EventsEmitted.
type CreateResult struct {
	Schema        string
	EntitySlug    string
	EventsEmitted []work.RecordEvent
	FilePath      string // empty for non-file schemas
	// RoutingNote is the human-readable "routed to …" hint the file schemas
	// (memory/retro/report-card/migration) produce, byte-identical to forge's,
	// so the agent-facing response carries it when a create routes through the
	// layer. Empty for the event-sourced schemas (forge sets none there either).
	RoutingNote string
}

// Create takes a schema name + typed Input + project, runs the full
// orchestration, and returns the result. Mismatched (schema, Input) — e.g.
// schema="bug" with Input.Memory set — returns a clear error.
//
// Orchestration (per schema arm):
//  1. Validate the Input has exactly the field matching the schema set.
//  2. Dispatch to the per-schema builder (validates inputs, applies forge
//     sugar, marshals the typed event payload; file schemas also write the
//     artifact via re-homed forge helpers).
//  3. Run B-D1 duplicate-slug reject for the event-sourced creates
//     (bug/suggestion/chain/task). File schemas have their own update
//     semantics; migration is idempotent by design.
//  4. Submit through work.HandleRecord — strict_all_or_nothing on for the
//     chain+tasks fan-out (atomic semantics matching forge).
//  5. Run B-F3 knowledge-index sync for the Indexed DB schemas
//     (bug, chain, task — not suggestion, not memory; file schemas build
//     their pointer at file-write time inside forge.WriteChainAnchoredDoc).
//     For chain+tasks fan-out, also sync each TaskCreated's pointer.
func Create(ctx context.Context, deps Deps, schema, project string, in Input) (CreateResult, error) {
	if deps.Pool == nil {
		return CreateResult{}, fmt.Errorf("construct.Create: Deps.Pool is required")
	}
	if err := validateInputMatchesSchema(schema, in); err != nil {
		return CreateResult{}, err
	}

	// Bucket 5: no-event file create (vault-note). The markdown file IS the
	// artifact; there is no typed event and no record submit, so it skips the
	// event-centric orchestration below. The knowledge_pointer upsert (+ the
	// same-slug-reforge "updated" verb + scope-change orphan cleanup) is the
	// dispatch host's full AfterCreate notifier's job, exactly as it was when
	// forge served vault-note via vaultNoteStrategy.Create. (P2-C.2 survivor.)
	if schema == "vault-note" {
		return createVaultNote(ctx, deps, project, in.VaultNote.Slug, in.VaultNote.Fields)
	}

	events, filePath, routingNote, err := dispatchBuild(ctx, deps, schema, project, in)
	if err != nil {
		return CreateResult{}, err
	}
	if len(events) == 0 {
		return CreateResult{}, fmt.Errorf("construct.Create: dispatch produced no events for schema %q", schema)
	}

	// (3) B-D1 once-only-create reject (pre-emit).
	if shouldDupCheck(schema) {
		chainSlug := chainSlugFromInput(in)
		headSlug := events[0].EntitySlug
		if err := RejectDuplicateCreate(ctx, deps.Pool.DB(), schema, project, chainSlug, headSlug); err != nil {
			return CreateResult{}, err
		}
	}

	// (4) Record submit. Strict mode iff fan-out (forge's atomic semantics).
	strict := len(events) > 1
	params, err := json.Marshal(work.RecordParams{Events: events, StrictAllOrNothing: strict})
	if err != nil {
		return CreateResult{}, fmt.Errorf("construct.Create: marshal record params: %w", err)
	}
	res, err := work.HandleRecord(ctx, work.TableDeps{Pool: deps.Pool}, project, params)
	if err != nil {
		return CreateResult{}, fmt.Errorf("construct.Create: record submit: %w", err)
	}
	if !res.OK || res.Recorded != len(events) {
		return CreateResult{}, fmt.Errorf("construct.Create: record submit incomplete: ok=%v recorded=%d want=%d", res.OK, res.Recorded, len(events))
	}

	// (5) B-F3 knowledge-index sync for Indexed DB schemas.
	if needsIndexSync(schema) {
		if err := SyncCreateIndex(ctx, deps.Pool, deps.Schemas, schema, project, events[0].EntitySlug); err != nil {
			return CreateResult{}, fmt.Errorf("construct.Create: index sync %s: %w", schema, err)
		}
		// chain+tasks fan-out: also sync each task pointer.
		if schema == "chain" {
			for _, ev := range events {
				if ev.Type == "TaskCreated" {
					if err := SyncCreateIndex(ctx, deps.Pool, deps.Schemas, "task", project, ev.EntitySlug); err != nil {
						return CreateResult{}, fmt.Errorf("construct.Create: index sync task %q: %w", ev.EntitySlug, err)
					}
				}
			}
		}
	}

	return CreateResult{
		Schema:        schema,
		EntitySlug:    events[0].EntitySlug,
		EventsEmitted: events,
		FilePath:      filePath,
		RoutingNote:   routingNote,
	}, nil
}

// dispatchBuild routes a (schema, Input) pair to the right per-schema builder
// and returns the events + (for file schemas) the file path extracted from
// the event payload. The schema↔Input cross-check happens in
// validateInputMatchesSchema before this is called, so each arm dereferences
// the typed field directly.
func dispatchBuild(ctx context.Context, deps Deps, schema, project string, in Input) ([]work.RecordEvent, string, string, error) {
	switch schema {
	case "bug":
		ev, err := buildBug(project, *in.Bug)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, "", "", nil

	case "suggestion":
		ev, err := buildSuggestion(project, *in.Suggestion)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, "", "", nil

	case "chain":
		if in.Chain != nil {
			ev, err := buildChain(project, *in.Chain)
			if err != nil {
				return nil, "", "", err
			}
			return []work.RecordEvent{ev}, "", "", nil
		}
		// in.ChainWithTasks != nil (validated above)
		evs, err := buildChainWithTasks(project, *in.ChainWithTasks)
		if err != nil {
			return nil, "", "", err
		}
		return evs, "", "", nil

	case "task":
		ev, err := buildTask(project, *in.Task)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, "", "", nil

	case "memory":
		memSchema, ok := deps.Schemas.Get("memory")
		if !ok {
			return nil, "", "", fmt.Errorf("construct.Create: memory schema not in registry")
		}
		ev, note, err := buildMemory(ctx, deps.Pool.DB(), memSchema, project, *in.Memory)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, vaultPathFromPayload(ev.Payload), note, nil

	case "retrospective":
		s, ok := deps.Schemas.Get("retrospective")
		if !ok {
			return nil, "", "", fmt.Errorf("construct.Create: retrospective schema not in registry")
		}
		ev, note, err := buildRetrospective(ctx, deps.Pool, s, project, *in.Retrospective)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, filePathFromPayload(ev.Payload), note, nil

	case "report-card":
		s, ok := deps.Schemas.Get("report-card")
		if !ok {
			return nil, "", "", fmt.Errorf("construct.Create: report-card schema not in registry")
		}
		ev, note, err := buildReportCard(ctx, deps.Pool, s, project, *in.ReportCard)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, filePathFromPayload(ev.Payload), note, nil

	case "migration":
		s, ok := deps.Schemas.Get("migration")
		if !ok {
			return nil, "", "", fmt.Errorf("construct.Create: migration schema not in registry")
		}
		ev, note, err := buildMigration(ctx, deps.Pool, s, project, *in.Migration)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, firstFilePathFromPayload(ev.Payload), note, nil

	case "bench":
		ev, note, err := buildBench(ctx, deps.Pool.DB(), project, *in.Bench)
		if err != nil {
			return nil, "", "", err
		}
		return []work.RecordEvent{ev}, "", note, nil

	default:
		return nil, "", "", fmt.Errorf("construct.Create: unknown schema %q (supported: bug, suggestion, chain, task, memory, retrospective, report-card, migration, bench)", schema)
	}
}

// ── orchestration helpers ───────────────────────────────────────────────────

// validateInputMatchesSchema enforces the union discipline: exactly the
// Input field matching the schema name must be non-nil, with no other field
// set. The "chain" schema accepts either Chain or ChainWithTasks (mutually
// exclusive). Unknown schema → unknown-schema error here, before dispatch.
func validateInputMatchesSchema(schema string, in Input) error {
	switch schema {
	case "bug":
		return requireExactly(in, "Bug", in.Bug != nil)
	case "suggestion":
		return requireExactly(in, "Suggestion", in.Suggestion != nil)
	case "chain":
		// either Chain or ChainWithTasks, not both.
		if in.Chain == nil && in.ChainWithTasks == nil {
			return fmt.Errorf("construct.Create: schema %q requires Input.Chain or Input.ChainWithTasks", schema)
		}
		if in.Chain != nil && in.ChainWithTasks != nil {
			return fmt.Errorf("construct.Create: schema %q: set exactly one of Input.Chain or Input.ChainWithTasks, not both", schema)
		}
		if extras := setInputFields(in, "Chain", "ChainWithTasks"); len(extras) > 0 {
			return fmt.Errorf("construct.Create: schema %q: unexpected Input fields set: %v", schema, extras)
		}
		return nil
	case "task":
		return requireExactly(in, "Task", in.Task != nil)
	case "memory":
		return requireExactly(in, "Memory", in.Memory != nil)
	case "retrospective":
		return requireExactly(in, "Retrospective", in.Retrospective != nil)
	case "report-card":
		return requireExactly(in, "ReportCard", in.ReportCard != nil)
	case "migration":
		return requireExactly(in, "Migration", in.Migration != nil)
	case "bench":
		return requireExactly(in, "Bench", in.Bench != nil)
	case "vault-note":
		return requireExactly(in, "VaultNote", in.VaultNote != nil)
	default:
		return fmt.Errorf("construct.Create: unknown schema %q (supported: bug, suggestion, chain, task, memory, retrospective, report-card, migration, bench, vault-note)", schema)
	}
}

// requireExactly checks that the named Input field is set and no other field is.
func requireExactly(in Input, expected string, isSet bool) error {
	if !isSet {
		return fmt.Errorf("construct.Create: missing Input.%s", expected)
	}
	if extras := setInputFields(in, expected); len(extras) > 0 {
		return fmt.Errorf("construct.Create: unexpected Input fields set alongside Input.%s: %v", expected, extras)
	}
	return nil
}

// setInputFields returns the names of Input fields that are set but NOT in
// the allowed list. Used for the union discipline.
func setInputFields(in Input, allowed ...string) []string {
	allowSet := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowSet[a] = true
	}
	var extras []string
	check := func(name string, isSet bool) {
		if isSet && !allowSet[name] {
			extras = append(extras, name)
		}
	}
	check("Bug", in.Bug != nil)
	check("Suggestion", in.Suggestion != nil)
	check("Chain", in.Chain != nil)
	check("ChainWithTasks", in.ChainWithTasks != nil)
	check("Task", in.Task != nil)
	check("Memory", in.Memory != nil)
	check("Retrospective", in.Retrospective != nil)
	check("ReportCard", in.ReportCard != nil)
	check("Migration", in.Migration != nil)
	check("Bench", in.Bench != nil)
	check("VaultNote", in.VaultNote != nil)
	return extras
}

// shouldDupCheck reports whether B-D1 once-only-create applies to the schema.
// File schemas have their own update semantics; migration is idempotent.
func shouldDupCheck(schema string) bool {
	switch schema {
	case "bug", "suggestion", "chain", "task":
		return true
	default:
		return false
	}
}

// needsIndexSync reports whether B-F3 knowledge-index sync applies. Only the
// Indexed DB schemas (bug, chain, task). Suggestion + memory are not Indexed
// (forge.Schema.Indexed()=false); file schemas build their pointer at
// file-write time inside forge.WriteChainAnchoredDoc.
func needsIndexSync(schema string) bool {
	switch schema {
	case "bug", "chain", "task":
		return true
	default:
		return false
	}
}

// chainSlugFromInput pulls TaskInput.ChainSlug for the task-arm dup-check
// (whose key is (chain_id, slug), not (project_id, slug)). Empty otherwise.
func chainSlugFromInput(in Input) string {
	if in.Task != nil {
		return in.Task.ChainSlug
	}
	return ""
}

// vaultPathFromPayload extracts the file path a MemoryWritten event records.
func vaultPathFromPayload(payload json.RawMessage) string {
	var p struct {
		VaultPath string `json:"vault_path"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.VaultPath
}

// filePathFromPayload extracts file_path from a RetrospectiveForged /
// ReportCardForged event payload.
func filePathFromPayload(payload json.RawMessage) string {
	var p struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.FilePath
}

// firstFilePathFromPayload returns the canonical path of a MigrationForged
// event (file_paths[0] = canonical, [1] = testutil mirror).
func firstFilePathFromPayload(payload json.RawMessage) string {
	var p struct {
		FilePaths []string `json:"file_paths"`
	}
	_ = json.Unmarshal(payload, &p)
	if len(p.FilePaths) == 0 {
		return ""
	}
	return p.FilePaths[0]
}
