package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/events"
)

// LifecycleStepParams is the wire shape for work.lifecycle_step. The
// sugar collapses the highest-frequency task-handoff seam (close T_n
// with sha + handoff_output + close_rationale, start T_n+1 with
// next_rationale) into one MCP call. Same-chain invariant: both tasks
// MUST belong to the same chain; cross-chain handoffs go through
// work.batch directly with explicit per-op chain_slug params.
type LifecycleStepParams struct {
	CloseTaskSlug      string `json:"close_task_slug"`
	CloseCommitSHA     string `json:"close_commit_sha,omitempty"`
	CloseHandoffOutput string `json:"close_handoff_output,omitempty"`
	CloseRationale     string `json:"close_rationale"`
	NextTaskSlug       string `json:"next_task_slug"`
	NextRationale      string `json:"next_rationale"`
}

// closeOpEnvelope is the typed shape for the task_complete sub-op's
// params. Marshalled directly into the batch's per-op Params field;
// kept as a typed struct so the dispatch-layer forbidigo gate (no bare
// `any` outside internal/db / internal/dispatch) is satisfied.
type closeOpEnvelope struct {
	Slug          string `json:"slug"`
	ChainSlug     string `json:"chain_slug"`
	CommitSHA     string `json:"commit_sha,omitempty"`
	HandoffOutput string `json:"handoff_output,omitempty"`
}

// startOpEnvelope is the typed shape for the task_start sub-op's
// params. Same forbidigo-driven reason as closeOpEnvelope.
type startOpEnvelope struct {
	Slug      string `json:"slug"`
	ChainSlug string `json:"chain_slug"`
}

// LifecycleStepResult is the response envelope. Wraps the underlying
// BatchResult plus the TaskHandoff event id and the inferred
// chain_slug — callers get one envelope back without having to walk
// the batch's per-op array.
type LifecycleStepResult struct {
	OK             bool        `json:"ok"`
	ChainSlug      string      `json:"chain_slug,omitempty"`
	CloseEventID   string      `json:"close_event_id,omitempty"`
	StartEventID   string      `json:"start_event_id,omitempty"`
	HandoffEventID string      `json:"handoff_event_id,omitempty"`
	Batch          BatchResult `json:"batch"`
	Error          string      `json:"error,omitempty"`
}

// HandleLifecycleStep dispatches the work.lifecycle_step action. The
// sugar's contract per the T1 design audit + the chain's T3 acceptance:
// ZERO duplicated logic vs work.batch — the handler builds a 2-op
// batch envelope ([task_complete, task_start]), validates the same-
// chain invariant, dispatches HandleBatch, and on success emits a
// composite TaskHandoff event that records the seam as a single fact.
//
// The composite TaskHandoff emits IN ADDITION TO the per-op
// TaskCompleted + TaskStarted cascade events (T1 audit Decision 2
// tightening) — downstream listeners subscribed to individual lifecycle
// events do not regress when callers switch to lifecycle_step.
//
// Same-chain invariant: close_task_slug uniquely identifies a chain
// (chain_id is FK on proj_current_tasks), and next_task_slug must
// resolve to the same chain. Cross-chain handoffs reject pre-dispatch
// with a clear error naming both chain slugs.
func HandleLifecycleStep(ctx context.Context, deps TableDeps, project string, params json.RawMessage) (LifecycleStepResult, error) {
	var p LifecycleStepParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return LifecycleStepResult{}, fmt.Errorf("parse lifecycle_step params: %w", err)
		}
	}
	if strings.TrimSpace(p.CloseTaskSlug) == "" {
		return LifecycleStepResult{Error: "lifecycle_step requires close_task_slug"}, nil
	}
	if strings.TrimSpace(p.NextTaskSlug) == "" {
		return LifecycleStepResult{Error: "lifecycle_step requires next_task_slug"}, nil
	}
	if strings.TrimSpace(p.CloseRationale) == "" {
		return LifecycleStepResult{Error: "lifecycle_step requires close_rationale (per-op rationale for the task_complete sub-op)"}, nil
	}
	if strings.TrimSpace(p.NextRationale) == "" {
		return LifecycleStepResult{Error: "lifecycle_step requires next_rationale (per-op rationale for the task_start sub-op)"}, nil
	}

	pool := deps.Pool
	if pool == nil {
		return LifecycleStepResult{Error: "lifecycle_step requires a DB pool"}, nil
	}

	// Same-chain invariant: resolve both slugs to their chains
	// before dispatching the batch. Cross-chain handoffs reject here
	// with both chain slugs named — that's the load-bearing
	// observability papercut so the caller can tell at a glance which
	// chain they grabbed the next slug from.
	closeChain, err := chainSlugForTask(ctx, pool.DB(), p.CloseTaskSlug)
	if err != nil {
		return LifecycleStepResult{Error: fmt.Sprintf("close_task_slug %q: %s", p.CloseTaskSlug, err.Error())}, nil
	}
	nextChain, err := chainSlugForTask(ctx, pool.DB(), p.NextTaskSlug)
	if err != nil {
		return LifecycleStepResult{Error: fmt.Sprintf("next_task_slug %q: %s", p.NextTaskSlug, err.Error())}, nil
	}
	if closeChain != nextChain {
		return LifecycleStepResult{
			Error: fmt.Sprintf("lifecycle_step: cross-chain handoff rejected — close_task_slug %q belongs to chain %q but next_task_slug %q belongs to chain %q. For cross-chain handoffs, call work.batch directly with explicit per-op chain_slug params.",
				p.CloseTaskSlug, closeChain, p.NextTaskSlug, nextChain),
		}, nil
	}

	// Build the 2-op batch envelope. Pass chain_slug explicitly on
	// both ops — same-chain invariant already validated, so this is
	// just disambiguation insurance against slug collisions across
	// other chains. Typed structs avoid the bare-any forbidigo gate
	// while keeping the omitempty contract on close_commit_sha /
	// close_handoff_output (zero-values stay off the wire so the
	// downstream task_complete handler's optional-field shape is
	// preserved).
	closeOpParams, err := json.Marshal(closeOpEnvelope{
		Slug:          p.CloseTaskSlug,
		ChainSlug:     closeChain,
		CommitSHA:     p.CloseCommitSHA,
		HandoffOutput: p.CloseHandoffOutput,
	})
	if err != nil {
		return LifecycleStepResult{}, fmt.Errorf("marshal close op params: %w", err)
	}
	startOpParams, err := json.Marshal(startOpEnvelope{
		Slug:      p.NextTaskSlug,
		ChainSlug: nextChain,
	})
	if err != nil {
		return LifecycleStepResult{}, fmt.Errorf("marshal start op params: %w", err)
	}
	batchParams := BatchParams{
		BatchRationale: fmt.Sprintf("lifecycle_step seam in chain %s: %s → %s", closeChain, p.CloseTaskSlug, p.NextTaskSlug),
		Ops: []BatchOp{
			{Op: "task_complete", Params: closeOpParams, Rationale: p.CloseRationale},
			{Op: "task_start", Params: startOpParams, Rationale: p.NextRationale},
		},
	}
	batchRaw, err := json.Marshal(batchParams)
	if err != nil {
		return LifecycleStepResult{}, fmt.Errorf("marshal batch envelope: %w", err)
	}

	batchResult, err := HandleBatch(ctx, deps, project, batchRaw)
	if err != nil {
		return LifecycleStepResult{Batch: batchResult}, err
	}
	out := LifecycleStepResult{
		ChainSlug: closeChain,
		Batch:     batchResult,
	}
	if !batchResult.OK {
		out.Error = batchResult.Error
		if out.Error == "" && batchResult.RolledBack {
			// Find the first failing op's error_message to bubble up
			// at the lifecycle_step envelope level — saves callers from
			// walking batch.ops[] for the actual rejection reason.
			for _, opRes := range batchResult.Ops {
				if !opRes.OK && opRes.ErrorMessage != nil {
					out.Error = fmt.Sprintf("lifecycle_step rolled back: %s op failed (%s): %s", opRes.Action, derefOr(opRes.ErrorKind, "unknown"), *opRes.ErrorMessage)
					break
				}
			}
		}
		return out, nil
	}

	// Capture cascade event ids before emitting the composite event.
	// In a successful 2-op batch, ops[0] is task_complete and ops[1]
	// is task_start; their EventIDs are the cascade event ids.
	if len(batchResult.Ops) >= 1 && batchResult.Ops[0].EventID != nil {
		out.CloseEventID = *batchResult.Ops[0].EventID
	}
	if len(batchResult.Ops) >= 2 && batchResult.Ops[1].EventID != nil {
		out.StartEventID = *batchResult.Ops[1].EventID
	}

	// Emit the composite TaskHandoff event. Lives in its own tx —
	// the batch's outer tx has already committed by the time we get
	// here. The envelope's entity is the closing task (the chain's
	// perspective: the seam belongs to the task that just finished).
	closeOpProjectID, err := lookupTaskProjectID(ctx, pool.DB(), p.CloseTaskSlug, closeChain)
	if err != nil {
		// The batch already committed; TaskHandoff emit failure is a
		// secondary effect. Surface it but don't fail the call.
		out.OK = true
		out.Error = fmt.Sprintf("lifecycle_step succeeded but TaskHandoff emit failed: lookup project_id: %s", err.Error())
		return out, nil
	}
	handoffPayload := events.TaskHandoffPayload{
		ChainSlug: closeChain,
		Closed: events.TaskHandoffClosed{
			TaskSlug:      p.CloseTaskSlug,
			CommitSHA:     strPtrIfNotEmpty(p.CloseCommitSHA),
			HandoffOutput: strPtrIfNotEmpty(p.CloseHandoffOutput),
			Rationale:     p.CloseRationale,
			EventID:       strPtrIfNotEmpty(out.CloseEventID),
		},
		Started: events.TaskHandoffStarted{
			TaskSlug:  p.NextTaskSlug,
			Rationale: p.NextRationale,
			EventID:   strPtrIfNotEmpty(out.StartEventID),
		},
	}
	emitErr := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		id, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("task", p.CloseTaskSlug, closeOpProjectID),
			Payload: handoffPayload,
		})
		if err != nil {
			return err
		}
		out.HandoffEventID = id
		return nil
	})
	if emitErr != nil {
		out.OK = true
		out.Error = fmt.Sprintf("lifecycle_step succeeded but TaskHandoff emit failed: %s", emitErr.Error())
		return out, nil
	}
	out.OK = true
	return out, nil
}

// chainSlugForTask resolves a task slug to its parent chain slug. Used
// by HandleLifecycleStep's same-chain invariant gate. Slug-only lookup
// (no chain disambiguator) — if the slug appears in multiple chains
// the function returns an explicit ambiguity error naming the chains
// so the caller can switch to work.batch with explicit chain_slug
// params.
func chainSlugForTask(ctx context.Context, q db.Queryer, slug string) (string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT c.slug FROM proj_current_tasks t
		 JOIN proj_chain_status c ON t.chain_id = c.id
		 WHERE t.slug = ? ORDER BY c.slug`, slug)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var chains []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		chains = append(chains, s)
	}
	switch len(chains) {
	case 0:
		return "", fmt.Errorf("task not found in any chain")
	case 1:
		return chains[0], nil
	default:
		return "", fmt.Errorf("task slug is ambiguous across chains: %s. Call work.batch directly with explicit chain_slug on each op", strings.Join(chains, ", "))
	}
}

// lookupTaskProjectID resolves the project_id for a (slug, chain_slug)
// pair. Used by HandleLifecycleStep to build the TaskHandoff event's
// EntityRef. Tasks inherit project_id from their parent chain.
func lookupTaskProjectID(ctx context.Context, q db.Queryer, slug, chainSlug string) (string, error) {
	var projectID string
	err := q.QueryRowContext(ctx,
		`SELECT c.project_id FROM proj_current_tasks t
		 JOIN proj_chain_status c ON t.chain_id = c.id
		 WHERE t.slug = ? AND c.slug = ?`, slug, chainSlug).
		Scan(&projectID)
	if err != nil {
		return "", err
	}
	return projectID, nil
}

// strPtrIfNotEmpty returns a *string pointing at s when non-empty,
// nil otherwise. Used by HandleLifecycleStep to populate the
// TaskHandoffPayload's optional fields without writing empty-string
// values into the events ledger.
func strPtrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// derefOr returns *p or fallback when p is nil. Small helper for the
// rolled-back error-summary branch.
func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// dispatchAdaptLifecycleStep wires HandleLifecycleStep through
// dispatch.Adapt. Mirrors dispatchAdaptBatch — kept here so the deps
// capture is local and the table.go entry stays one line.
func dispatchAdaptLifecycleStep(deps TableDeps) dispatch.Handler {
	return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (LifecycleStepResult, error) {
		return HandleLifecycleStep(ctx, deps, project, params)
	})
}

// Keep errors imported transitively visible — we use it in
// stringly-checked rollback branches above; the explicit import line
// avoids the unused-import gate when the codepath narrows.
var _ = errors.New

// ── Action-doc descriptor (parallel-run registry, action_doc.go) ────────

var lifecycleStepDoc = ActionDoc{
	Purpose: "Sugar over work.batch for the chain-task handoff seam: close T_n with sha + handoff_output + close_rationale AND start T_n+1 with next_rationale, all in one MCP round-trip. Builds a 2-op batch envelope and emits a composite TaskHandoff event after the underlying batch commits. Same-chain invariant — close_task_slug and next_task_slug MUST belong to the same chain (cross-chain handoffs reject pre-dispatch with both chain slugs named). For cross-chain handoffs use work.batch directly with explicit per-op chain_slug params.",
	Params: []DocParam{
		{Name: "close_task_slug", Required: true, Description: "Slug of the task to close. Inferred chain_slug comes from this task's parent chain."},
		{Name: "close_commit_sha", Required: false, Description: "Closing commit SHA. Optional (matches task_complete's gate). Accepts the literal sentinel 'unversioned'."},
		{Name: "close_handoff_output", Required: false, Description: "What artifacts the closing task produces for the next task. Operational color distinct from rationale."},
		{Name: "close_rationale", Required: true, Description: "Per-op rationale for the task_complete sub-op ('why this counts as done')."},
		{Name: "next_task_slug", Required: true, Description: "Slug of the next task to start. Must belong to the same chain as close_task_slug."},
		{Name: "next_rationale", Required: true, Description: "Per-op rationale for the task_start sub-op ('why pick this up next')."},
	},
	Example: `{"close_task_slug":"T2-work-batch","close_commit_sha":"ae702417","close_handoff_output":"work.batch landed with true ACID rollback","close_rationale":"T2 acceptance criteria all met","next_task_slug":"T3-work-lifecycle-step","next_rationale":"T3 sugar over batch is the natural next step"}`,
	Notes: "Same-chain invariant: close_task_slug uniquely identifies a chain via proj_current_tasks.chain_id. next_task_slug is looked up the same way; if it resolves to a different chain, the call rejects pre-dispatch with a clear error naming both chain slugs. Slug ambiguity (same slug across multiple chains) also rejects with the chains named so the caller can switch to work.batch with explicit chain_slug params per op.\n\n" +
		"Event fan-out: each lifecycle_step call produces FOUR events in the ledger — TaskCompleted (cascade), TaskTransitioned (cascade, pending→active), BatchExecuted (from the underlying work.batch), and TaskHandoff (composite). Listeners that want the affordance-level \"one event per chain-task seam\" view subscribe to TaskHandoff; listeners on the individual lifecycle events continue to receive them unchanged. The fan-out is intentional per the T1 design audit Decision 2 tightening — composite IN ADDITION to per-op, not in lieu of.\n\n" +
		"ZERO-duplicated-logic contract (T3 chain constraint): the handler is envelope construction + same-chain validation + HandleBatch dispatch + composite emit only. No re-implementation of task lifecycle logic. If a future change ever adds chain-state-mutation logic to lifecycle_step.go that isn't sugar over batch, that's a contract violation — refactor.",
	EnvelopeRequirements: []ActionEnvelopeReq{{
		Field:               "rationale",
		Required:            true,
		Reason:              "Dispatcher policy gate (action-manifests/dispatch-policy.toml). Envelope-level rationale is the 'why this MCP call' grain. The per-op close_rationale and next_rationale are separately required by HandleLifecycleStep at the validation gate; together all three land on the events ledger.",
		AppliesToActorKinds: []string{"agent"},
	}},
	Returns: &ActionReturn{Shape: "LifecycleStepResult", Description: "Envelope: ok, chain_slug (inferred from the closing task), close_event_id (cascade TaskCompleted event id), start_event_id (cascade TaskTransitioned event id), handoff_event_id (composite TaskHandoff event id), batch (the underlying BatchResult for full traceability). Error field populated on cross-chain rejection, missing rationale, batch rollback, or TaskHandoff emit failure (the secondary-effect case where the batch committed but the composite event emit failed)."},
}
