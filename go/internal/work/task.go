package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/jsonutil"
)

// Task mirrors Rust work-lib::types::Task. Full row from the tasks table
// plus the chain_id (FK) and audit timestamps.
type Task struct {
	ID                 int64  `json:"id"`
	ChainID            int64  `json:"chain_id"`
	Slug               string `json:"slug"`
	Position           int64  `json:"position"`
	Status             string `json:"status"`
	ProblemStatement   string `json:"problem_statement"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	ContextRequired    string `json:"context_required"`
	Constraints        string `json:"constraints"`
	HandoffOutput      string `json:"handoff_output"`
	CommitSHA          string `json:"commit_sha,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// TaskListItem is the compact projection task_list / task_search return
// when verbose=false (the default).
type TaskListItem struct {
	ID        int64  `json:"id"`
	ChainID   int64  `json:"chain_id"`
	ChainSlug string `json:"chain_slug"`
	Slug      string `json:"slug"`
	Position  int64  `json:"position"`
	Status    string `json:"status"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// BlockerEntry mirrors Rust work-lib::types::BlockerEntry. Kind is
// omitted on structural blockers (the historical wire shape) and set
// to "status_only" on the synthetic entry the handler appends when a
// task carries `status='blocked'` with NO structural edge at all
// (intra- OR cross-chain). Bug 1379's disambiguation is between
// "blocked by a real structural edge" (kind omitted, slug/chain_slug
// populated) and "blocked-flag-without-any-edge" (kind=status_only).
// ChainSlug always names the BLOCKER's chain, which may differ from
// the blocked task's chain when the edge was created cross-chain via
// task_block's blocker_chain_slug parameter. See bug 1413 for the
// docs-vs-behavior correction.
type BlockerEntry struct {
	Slug      string `json:"slug"`
	ChainSlug string `json:"chain_slug"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
	Kind      string `json:"kind,omitempty"`
}

// ErrAmbiguousSlug is returned when a task slug resolves to multiple
// non-cancelled rows across different chains.
type ErrAmbiguousSlug struct {
	Slug   string
	Chains []string
}

func (e *ErrAmbiguousSlug) Error() string {
	hints := make([]string, 0, len(e.Chains))
	for _, c := range e.Chains {
		hints = append(hints, fmt.Sprintf("chain_slug=%q", c))
	}
	return fmt.Sprintf(
		"task slug %q is ambiguous across chains: %s — disambiguate by passing %s (the `chain` shorthand is also accepted)",
		e.Slug,
		strings.Join(e.Chains, ", "),
		strings.Join(hints, " or "),
	)
}

var ErrTaskNotFound = errors.New("task not found")

// resolveTaskChain finds the (chain_id, current_status) pair for the
// supplied slug. Same disambiguation discipline as Rust work-lib:
// chain-supplied → scoped; chain-absent + 1 candidate → that one;
// chain-absent + ≥2 candidates → prefer single non-cancelled, else
// ErrAmbiguousSlug. Non-Tx wrapper delegates to resolveTaskChainOn(pool.DB()).
func resolveTaskChain(ctx context.Context, pool *db.Pool, slug, chainSlug string) (int64, string, error) {
	return resolveTaskChainOn(ctx, pool.DB(), slug, chainSlug)
}

// resolveTaskChainOn is the db.Queryer-parameterised inner body. Pass
// pool.DB() for caller-isolated reads or *sql.Tx for reads that must
// see the outer write tx's pending writes (the work.HandleBatch
// composition case).
func resolveTaskChainOn(ctx context.Context, q db.Queryer, slug, chainSlug string) (int64, string, error) {
	if chainSlug != "" {
		var chainID int64
		var status string
		err := q.QueryRowContext(ctx,
			`SELECT t.chain_id, t.status FROM proj_current_tasks t
			 JOIN proj_chain_status c ON t.chain_id = c.id
			 WHERE t.slug = ? AND c.slug = ?`, slug, chainSlug).
			Scan(&chainID, &status)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, "", fmt.Errorf("%w: task '%s' not found in chain '%s'", ErrTaskNotFound, slug, chainSlug)
			}
			return 0, "", err
		}
		return chainID, status, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT t.chain_id, c.slug, t.status FROM proj_current_tasks t
		 JOIN proj_chain_status c ON t.chain_id = c.id
		 WHERE t.slug = ? ORDER BY c.slug`, slug)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	type cand struct {
		chainID int64
		chain   string
		status  string
	}
	var all []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.chainID, &c.chain, &c.status); err != nil {
			return 0, "", err
		}
		all = append(all, c)
	}
	switch len(all) {
	case 0:
		return 0, "", fmt.Errorf("%w: task '%s' not found", ErrTaskNotFound, slug)
	case 1:
		return all[0].chainID, all[0].status, nil
	default:
		nonCancelled := make([]cand, 0, len(all))
		for _, c := range all {
			if c.status != "cancelled" {
				nonCancelled = append(nonCancelled, c)
			}
		}
		if len(nonCancelled) == 1 {
			return nonCancelled[0].chainID, nonCancelled[0].status, nil
		}
		names := make([]string, 0, len(all))
		for _, c := range all {
			names = append(names, c.chain)
		}
		return 0, "", &ErrAmbiguousSlug{Slug: slug, Chains: names}
	}
}

// transitionTask runs the state-machine + DB UPDATE. Mirrors work-lib's
// transition_task, including the pending → closed gate on non-empty
// handoff_output, the SHA-stamp branches, and the terminal-state side
// effects (roadmap cleanup + stale-blocker handling). Emits the
// corresponding event type per toStatus — TaskCompleted for closed,
// TaskCancelled for cancelled, TaskTransitioned for the non-terminal
// states (pending / active / blocked); blockerSlug is optional and only
// meaningful for toStatus="blocked".
func transitionTask(ctx context.Context, pool *db.Pool, slug, toStatus, chainSlug string, handoffOutput, commitSHA *string) error {
	return transitionTaskWithBlockerEdges(ctx, pool, slug, toStatus, chainSlug, handoffOutput, commitSHA, "", "")
}

// transitionTaskWithBlocker is the underlying implementation; the
// blockerSlug parameter is consumed only by HandleTaskBlock so the
// emitted TaskTransitioned event records which task pinned the block.
func transitionTaskWithBlocker(ctx context.Context, pool *db.Pool, slug, toStatus, chainSlug string, handoffOutput, commitSHA *string, blockerSlug string) error {
	return transitionTaskWithBlockerEdges(ctx, pool, slug, toStatus, chainSlug, handoffOutput, commitSHA, blockerSlug, "")
}

// transitionTaskWithBlockerEdges is the underlying implementation that
// supports both addedBlockerSlug (the existing TaskTransitioned.blocker_slug
// semantics — names the edge ADDED by this transition) and
// removedBlockerSlug (added 2026-05-20 via T3 of agent-substrate-crud-
// retirement, §9.1 audit finding — names the edge REMOVED). Both are
// optional and mutually independent; HandleTaskUnblock emits with only
// removedBlockerSlug populated, HandleTaskBlock emits with only
// addedBlockerSlug, and transitionTask (terminal-state cleanup) emits
// with neither.
func transitionTaskWithBlockerEdges(ctx context.Context, pool *db.Pool, slug, toStatus, chainSlug string, handoffOutput, commitSHA *string, addedBlockerSlug, removedBlockerSlug string) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := transitionTaskWithBlockerEdgesInTx(ctx, tx, pool, slug, toStatus, chainSlug, handoffOutput, commitSHA, addedBlockerSlug, removedBlockerSlug)
		return err
	})
}

// transitionTaskWithBlockerEdgesInTx is the tx-aware inner body of
// transitionTaskWithBlockerEdges. Reads use the supplied tx (rather
// than pool.DB()) so cascade reads within the same transaction see
// the tx's own pending writes. This matters when one batch op modifies
// state another batch op reads — e.g., the auto-clear blocked→pending
// transition followed by pending→active inside HandleTaskStartInTx.
// Returns the event_id of the emitted transition event for callers
// (work.HandleBatch) that record cascade-event ids in their payload.
func transitionTaskWithBlockerEdgesInTx(ctx context.Context, tx *sql.Tx, pool *db.Pool, slug, toStatus, chainSlug string, handoffOutput, commitSHA *string, addedBlockerSlug, removedBlockerSlug string) (string, error) {
	chainID, fromStatus, err := resolveTaskChainOn(ctx, tx, slug, chainSlug)
	if err != nil {
		return "", err
	}
	if err := checkTaskTransition(slug, fromStatus, toStatus, handoffOutput); err != nil {
		return "", err
	}
	projectID, err := lookupChainProjectOn(ctx, tx, chainID)
	if err != nil {
		return "", err
	}
	// Canonical chain slug stamped on the event so the projection fold
	// targets exactly this task and doesn't fan out across same-slug tasks
	// in other chains (bug `task-lifecycle-event-folds-fan-out-across-
	// duplicate-task-slugs`). Resolve from chainID rather than trusting the
	// caller-passed chainSlug (which is empty when the task was named by id).
	canonicalChainSlug := chainSlug
	if canonicalChainSlug == "" {
		_ = tx.QueryRowContext(ctx, `SELECT slug FROM proj_chain_status WHERE id = ?`, chainID).Scan(&canonicalChainSlug)
	}
	payload := buildTaskTransitionPayload(canonicalChainSlug, fromStatus, toStatus, handoffOutput, commitSHA, addedBlockerSlug, removedBlockerSlug)
	if toStatus == "closed" || toStatus == "cancelled" {
		if err := cleanupBlockersAfterClose(ctx, tx, chainID, slug); err != nil {
			return "", err
		}
	}
	eventID, err := events.Emit(ctx, tx, events.EmitArgs{
		Entity:  events.NewEntityRef("task", slug, projectID),
		Payload: payload,
	})
	if err != nil {
		return "", err
	}
	// Sweep AFTER the close event has folded. The TaskCompleted fold
	// (foldTaskBlockersCleanupOnClose) deletes every proj_task_blockers edge
	// referencing the just-closed task, so a dependent whose ONLY blocker was
	// this task is now edgeless and the sweep's NOT EXISTS(edge) filter catches
	// it. Running the sweep before the emit (the prior order) left such
	// dependents stranded blocked-with-no-edge. Bug
	// `prose-only-sweep-runs-before-edge-cleanup-fold-strands-structurally-blocked-task-on-blocker-close`.
	if toStatus == "closed" {
		if err := sweepProseOnlyBlockedAfterClose(ctx, tx, chainID, projectID, slug); err != nil {
			return "", err
		}
	}
	return eventID, nil
}

// buildTaskTransitionPayload picks the right event payload struct for a
// given toStatus. Terminal states get their dedicated types
// (TaskCompleted / TaskCancelled); non-terminal states share
// TaskTransitioned. Centralised here so HandleTaskBlock / HandleTaskUnblock
// only need to thread the blocker edge slugs; the picker handles the rest.
// removedBlockerSlug (added 2026-05-20 via T3 of agent-substrate-crud-
// retirement, §9.1 audit finding) names the blocker edge being removed
// in an unblock transition; addedBlockerSlug retains its existing
// semantics (the edge being added on a block transition).
func buildTaskTransitionPayload(chainSlug, fromStatus, toStatus string, handoffOutput, commitSHA *string, addedBlockerSlug, removedBlockerSlug string) events.Payload {
	switch toStatus {
	case "closed":
		return events.TaskCompletedPayload{
			ChainSlug:      chainSlug,
			CommitSHA:      commitSHA,
			ClosureSummary: handoffOutput,
		}
	case "cancelled":
		return events.TaskCancelledPayload{ChainSlug: chainSlug, Reason: nil}
	default:
		var added, removed *string
		if addedBlockerSlug != "" {
			b := addedBlockerSlug
			added = &b
		}
		if removedBlockerSlug != "" {
			b := removedBlockerSlug
			removed = &b
		}
		return events.TaskTransitionedPayload{
			ChainSlug:          chainSlug,
			FromStatus:         fromStatus,
			ToStatus:           toStatus,
			BlockerSlug:        added,
			RemovedBlockerSlug: removed,
		}
	}
}

// lookupChainProject reads a chain's project_id by chain row id. Task
// handlers use this to populate the EntityRef on emitted events; tasks
// inherit project_id from their parent chain rather than carrying it
// directly on the row.
func lookupChainProject(ctx context.Context, pool *db.Pool, chainID int64) (string, error) {
	return lookupChainProjectOn(ctx, pool.DB(), chainID)
}

// lookupChainProjectOn is the db.Queryer-parameterised inner body. Use
// tx when the call must see pending writes from the same outer
// transaction.
func lookupChainProjectOn(ctx context.Context, q db.Queryer, chainID int64) (string, error) {
	var projectID string
	err := q.QueryRowContext(ctx,
		`SELECT project_id FROM proj_chain_status WHERE id = ?`, chainID,
	).Scan(&projectID)
	if err != nil {
		return "", fmt.Errorf("lookup chain project_id: %w", err)
	}
	return projectID, nil
}

// lookupChainSlug returns the canonical slug for a chain by id. Used by
// the stamp-commit guard (bug `task-stamp-sha-accepts-foreign-chain-
// commit`) to compare against the chain the commit declares it belongs
// to. Returns "" on lookup failure — the guard treats that as "can't
// verify, allow".
func lookupChainSlug(ctx context.Context, pool *db.Pool, chainID int64) string {
	var slug string
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT slug FROM proj_chain_status WHERE id = ?`, chainID,
	).Scan(&slug); err != nil {
		return ""
	}
	return slug
}

// lookupProjectPath returns the registered filesystem path for a project
// (projects.path). Empty when unset or unknown. The stamp-commit guard
// uses it to read the commit message via git; an empty path makes the
// guard a no-op (best-effort).
func lookupProjectPath(ctx context.Context, pool *db.Pool, projectID string) string {
	var path string
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT path FROM projects WHERE id = ?`, projectID,
	).Scan(&path); err != nil {
		return ""
	}
	return path
}

func cleanupBlockersAfterClose(ctx context.Context, tx *sql.Tx, chainID int64, slug string) error {
	var taskID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM proj_current_tasks WHERE slug = ? AND chain_id = ?`,
		slug, chainID).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT blocked_task_id FROM proj_task_blockers WHERE blocker_task_id = ?`, taskID.Int64)
	if err != nil {
		return err
	}
	var formerly []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		formerly = append(formerly, id)
	}
	rows.Close()
	// T5-tasks: CRUD writes (DELETE FROM task_blockers + UPDATE tasks)
	// dropped. The closing TaskCompleted/TaskCancelled event has already
	// landed by this point; downstream blocked tasks are unblocked via
	// the prose-only sweep below (which emits its own TaskTransitioned
	// events). Structural task_blockers edges pointing at the closing
	// task are handled lazily — the fold treats them as stale once the
	// blocker task closes; T6's drop removes the table entirely.
	_ = taskID
	_ = formerly
	return nil
}

// sweepProseOnlyBlockedAfterClose unblocks every task in the same chain
// that has status='blocked' but no structural task_blockers row. Used
// after a successful task_complete to handle the prose-only intra-chain
// dependency pattern where chain authors record "X blocks Y" in
// design_decisions prose without creating structural edges via
// task_block. cleanupBlockersAfterClose handles structural edges; this
// sweep handles the prose-only case those edges miss.
//
// Heuristic risk: a task that's blocked-with-no-edges in this chain may
// have a real prose dep on a sibling that's still open. The sweep
// optimistically flips it to 'pending'; the user can re-block via
// task_block if the dep needs to be structural. Soft false-positive
// (task lands in pending instead of blocked) is preferred over silent
// staleness (task stays blocked indefinitely after its prose-dep
// closed). See bug task-complete-doesnt-auto-unblock-intra-chain-prose-deps.
//
// Runs inside the same tx as the closing UPDATE + Emit. Each swept task
// gets its own TaskTransitioned event so the audit ledger records the
// flip alongside the closing event that triggered it.
func sweepProseOnlyBlockedAfterClose(ctx context.Context, tx *sql.Tx, chainID int64, projectID, justClosedSlug string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.slug
		FROM proj_current_tasks t
		WHERE t.chain_id = ?
		  AND t.status = 'blocked'
		  AND t.slug != ?
		  AND NOT EXISTS (
		      SELECT 1 FROM proj_task_blockers WHERE blocked_task_id = t.id
		  )
		ORDER BY t.position`, chainID, justClosedSlug)
	if err != nil {
		return fmt.Errorf("sweep prose-only blocked: query: %w", err)
	}
	var sweepSlugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			rows.Close()
			return fmt.Errorf("sweep prose-only blocked: scan: %w", err)
		}
		sweepSlugs = append(sweepSlugs, slug)
	}
	rows.Close()
	if len(sweepSlugs) == 0 {
		return nil
	}
	// Stamp the canonical chain slug on every swept transition so the fold
	// targets exactly this chain's task and doesn't fan out across same-slug
	// tasks in other chains (bug `task-lifecycle-event-folds-fan-out-across-
	// duplicate-task-slugs`; generic chain-step slugs like audit-eight-axes /
	// triage-gate recur across many chains, so an empty chain_slug here
	// wrongly unblocks sibling chains' same-slug tasks). All swept tasks
	// belong to chainID by construction, so one lookup serves the batch.
	var canonicalChainSlug string
	_ = tx.QueryRowContext(ctx, `SELECT slug FROM proj_chain_status WHERE id = ?`, chainID).Scan(&canonicalChainSlug)
	for _, slug := range sweepSlugs {
		// T5-tasks: CRUD UPDATE dropped; fold applies status='pending'.
		if _, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity: events.NewEntityRef("task", slug, projectID),
			Payload: events.TaskTransitionedPayload{
				ChainSlug:  canonicalChainSlug,
				FromStatus: "blocked",
				ToStatus:   "pending",
			},
		}); err != nil {
			return fmt.Errorf("sweep prose-only blocked: emit %s: %w", slug, err)
		}
	}
	return nil
}

// taskTransitions is the typed state machine table. Mirrors Rust
// transitions::TASK_TRANSITIONS exactly. The pending → closed entry is
// gated on non-empty handoff_output.
var taskTransitions = []struct {
	From string
	To   string
	Gate string // "" = always; field name = NonEmptyField gate
}{
	{"pending", "active", ""},
	{"pending", "blocked", ""},
	{"pending", "cancelled", ""},
	{"pending", "closed", "handoff_output"},
	{"active", "blocked", ""},
	{"active", "closed", ""},
	{"active", "cancelled", ""},
	// active → pending is the mid-flight pause/revert path (bug
	// `task-lifecycle-no-clean-mid-flight-pause-or-revert`, 1461):
	// task_start was previously one-way; multi-session cold-pickup
	// arcs had no clean way to revert a paused task. task_unstart
	// drives this transition. The TaskTransitioned event carries
	// from_status=active to_status=pending so audit-ledger consumers
	// see the revert.
	{"active", "pending", ""},
	{"blocked", "pending", ""},
	{"blocked", "cancelled", ""},
	// Reopen lands a terminal task back in the PENDING backlog (not
	// active) per task_reopen's documented contract — so a wrongly-
	// closed/cancelled task becomes pickup-eligible by a lowest-position-
	// pending heuristic instead of masquerading as in-progress. Bug
	// `task-reopen-lands-in-active-not-documented-pending`. (active is
	// reached from pending via task_start — the explicit two-step.)
	{"closed", "pending", ""},
	{"cancelled", "pending", ""},
}

func checkTaskTransition(slug, from, to string, handoffOutput *string) error {
	for _, t := range taskTransitions {
		if t.From != from || t.To != to {
			continue
		}
		if t.Gate == "" {
			return nil
		}
		if t.Gate == "handoff_output" {
			if handoffOutput != nil && *handoffOutput != "" {
				return nil
			}
			return fmt.Errorf("task '%s': pending → closed is a two-step transition — call task_start first to move the task to active, then task_complete. The task's pre-set handoff_output is informational at task_complete time; the active-state hop stamps started_at and is required (from='%s' to='%s')",
				slug, from, to)
		}
	}
	return fmt.Errorf("task '%s': transition '%s' → '%s' not allowed", slug, from, to)
}

// ---------- handlers ----------

// taskReadParams accepts slug or id (id wins when both present).
// Aliases (bug 1441): task_id / task_slug match the sibling spelling
// schema-driven callers reach for; chain matches the shorthand
// task_search + transition params already accept.
type taskReadParams struct {
	Slug      string `json:"slug"`
	TaskSlug  string `json:"task_slug"`
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"`
	ChainSlug string `json:"chain_slug"`
	Chain     string `json:"chain"`
}

func (p taskReadParams) resolvedSlug() string  { return firstNonEmpty(p.Slug, p.TaskSlug) }
func (p taskReadParams) resolvedChain() string { return firstNonEmpty(p.ChainSlug, p.Chain) }
func (p taskReadParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.TaskID
}

func HandleTaskRead(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskReadResult, error) {
	var p taskReadParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskReadResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if id := p.resolvedID(); id > 0 {
		return readTaskByID(ctx, pool, id)
	}
	slug := p.resolvedSlug()
	if slug == "" {
		return TaskReadResult{Err: &ErrorEnvelope{Error: IdentifierRequiredError("task_read")}}, nil
	}
	return readTaskBySlug(ctx, pool, slug, p.resolvedChain())
}

func readTaskBySlug(ctx context.Context, pool *db.Pool, slug, chainSlug string) (TaskReadResult, error) {
	chainID, _, err := resolveTaskChain(ctx, pool, slug, chainSlug)
	if err != nil {
		var amb *ErrAmbiguousSlug
		if errors.As(err, &amb) {
			return TaskReadResult{Err: &ErrorEnvelope{Error: amb.Error(), Chains: amb.Chains}}, nil
		}
		if errors.Is(err, ErrTaskNotFound) {
			return TaskReadResult{Err: &ErrorEnvelope{Error: err.Error()}}, nil
		}
		return TaskReadResult{}, err
	}
	var t Task
	var sha sql.NullString
	if err := pool.DB().QueryRowContext(ctx, `
		SELECT id, chain_id, slug, position, status,
		problem_statement, acceptance_criteria, context_required,
		constraints, handoff_output, commit_sha, created_at, updated_at
		FROM proj_current_tasks WHERE slug = ? AND chain_id = ?`, slug, chainID,
	).Scan(&t.ID, &t.ChainID, &t.Slug, &t.Position, &t.Status,
		&t.ProblemStatement, &t.AcceptanceCriteria, &t.ContextRequired,
		&t.Constraints, &t.HandoffOutput, &sha, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return TaskReadResult{}, err
	}
	if sha.Valid {
		t.CommitSHA = sha.String
	}
	return TaskReadResult{Task: &t}, nil
}

func readTaskByID(ctx context.Context, pool *db.Pool, id int64) (TaskReadResult, error) {
	var t Task
	var sha sql.NullString
	err := pool.DB().QueryRowContext(ctx, `
		SELECT id, chain_id, slug, position, status,
		problem_statement, acceptance_criteria, context_required,
		constraints, handoff_output, commit_sha, created_at, updated_at
		FROM proj_current_tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.ChainID, &t.Slug, &t.Position, &t.Status,
		&t.ProblemStatement, &t.AcceptanceCriteria, &t.ContextRequired,
		&t.Constraints, &t.HandoffOutput, &sha, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskReadResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("task id=%d not found", id)}}, nil
		}
		return TaskReadResult{}, err
	}
	if sha.Valid {
		t.CommitSHA = sha.String
	}
	return TaskReadResult{Task: &t}, nil
}

// taskSearchParams captures both task_search and task_list. Aliases:
// chain / chain_slug (slug) and chain_id (numeric PK, accepted so the same
// chain handle works on task_list as on chain_state — bug
// work-chain-identifier-handling-inconsistent-across-actions); pattern / query
// / text / q / slug.
type taskSearchParams struct {
	Chain      string `json:"chain"`
	ChainSlug  string `json:"chain_slug"`
	ChainID    int64  `json:"chain_id"`
	Pattern    string `json:"pattern"`
	Query      string `json:"query"`
	Text       string `json:"text"`
	Q          string `json:"q"`
	Slug       string `json:"slug"`
	Status     string `json:"status"`
	Max        int64  `json:"max"`
	Limit      int64  `json:"limit"`
	MaxResults int64  `json:"max_results"`
}

// UnmarshalJSON lets task_list/task_search accept a chain by numeric chain_id
// as well as by slug, and tolerates a slug mistakenly passed in chain_id (a
// non-numeric string routes to the chain slug) — the same number-or-string
// coercion chain_state uses, so the chain handle is interchangeable across the
// two actions. Bug work-chain-identifier-handling-inconsistent-across-actions.
func (p *taskSearchParams) UnmarshalJSON(data []byte) error {
	var raw struct {
		Chain      string          `json:"chain"`
		ChainSlug  string          `json:"chain_slug"`
		ChainID    json.RawMessage `json:"chain_id"`
		Pattern    string          `json:"pattern"`
		Query      string          `json:"query"`
		Text       string          `json:"text"`
		Q          string          `json:"q"`
		Slug       string          `json:"slug"`
		Status     string          `json:"status"`
		Max        int64           `json:"max"`
		Limit      int64           `json:"limit"`
		MaxResults int64           `json:"max_results"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = taskSearchParams{
		Chain: raw.Chain, ChainSlug: raw.ChainSlug, Pattern: raw.Pattern,
		Query: raw.Query, Text: raw.Text, Q: raw.Q, Slug: raw.Slug,
		Status: raw.Status, Max: raw.Max, Limit: raw.Limit, MaxResults: raw.MaxResults,
	}
	id, slug := coerceChainID(raw.ChainID)
	p.ChainID = id
	if slug != "" && p.Chain == "" && p.ChainSlug == "" {
		p.Chain = slug
	}
	return nil
}

// HandleTaskSearch / HandleTaskList share an implementation since the
// Rust dispatch routes both to compact-projection rows. Chain filter
// optional; pattern filter optional with chain.
func HandleTaskSearch(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TaskListResult, error) {
	var p taskSearchParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskListResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	chain := firstNonEmpty(p.Chain, p.ChainSlug)
	// Accept a numeric chain_id by resolving it to its slug, so the same chain
	// handle works here as on chain_state (bug work-chain-identifier-handling-
	// inconsistent-across-actions).
	if chain == "" && p.ChainID != 0 {
		slug, err := chainSlugByID(ctx, pool, p.ChainID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return TaskListResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("chain id=%d not found", p.ChainID)}}, nil
			}
			return TaskListResult{}, err
		}
		chain = slug
	}
	pattern := firstNonEmpty(p.Pattern, p.Query, p.Text, p.Q, p.Slug)
	if chain == "" && pattern == "" {
		return TaskListResult{Err: &ErrorEnvelope{Error: "task_search: provide chain (slug or chain_id) or pattern"}}, nil
	}
	max := firstNonZeroInt64(p.Max, p.Limit, p.MaxResults, 20)

	if chain != "" {
		// Chain-scoped — order by position; pattern optional.
		return listTasksByChain(ctx, pool, chain, pattern, p.Status)
	}
	return searchTasksSummary(ctx, pool, pattern, project, max)
}

// taskListParams captures work.task_list — the cross-cutting "list open
// tasks" verb. Unlike task_search (which dead-ends when given neither a
// chain nor a pattern), task_list defaults to listing every task, with
// optional narrowing by status / project / chain / since. Decoded through
// the strict decodeListParams generic, so unknown filters are rejected
// (the anti-silent-narrowing guarantee shared with bug_list / suggestion_list).
//
// Chain narrowing is slug-only (chain / chain_slug): the strict decoder
// can't accept the numeric-or-string chain_id coercion taskSearchParams
// does via custom UnmarshalJSON without weakening the unknown-field
// rejection, and chain-scoped listing already works through task_search.
type taskListParams struct {
	Status    string `json:"status"`
	State     string `json:"state"` // alias → status
	Since     string `json:"since"` // filters on t.created_at >=
	Verbose   bool   `json:"verbose"`
	All       bool   `json:"all"` // legacy no-op, accepted for shape parity
	Limit     int64  `json:"limit"`
	Offset    int64  `json:"offset"`
	Chain     string `json:"chain"`      // optional chain narrowing (slug)
	ChainSlug string `json:"chain_slug"` // alias → chain
}

// HandleTaskList implements work.task_list as a real cross-cutting list
// verb (closes bug task-list-aliases-task-search-dead-ends-no-cross-cutting-
// open-task-verb). Empty params → all tasks (bounded by the default limit
// of 50). Cross-project when project is empty; scoped when set. `open` is a
// convenience pseudo-status meaning the non-terminal set (NOT IN
// closed/cancelled), mirroring bug/suggestion "open"; any other status is an
// exact match. Verbose is a no-op — TaskListItem is the only task-list
// projection, so there is no richer shape to switch to.
func HandleTaskList(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TaskListResult, error) {
	p, err := decodeListParams[taskListParams](params, "task_list",
		"status, since, verbose, all, limit, offset, chain (status alias: state; chain alias: chain_slug)")
	if err != nil {
		return TaskListResult{}, err
	}
	status := firstNonEmpty(p.Status, p.State)
	chain := firstNonEmpty(p.Chain, p.ChainSlug)
	limit, offset := normalizeLimitOffset(p.Limit, p.Offset, 50)

	args := db.NewArgs()
	var conds []string
	if project != "" {
		conds = append(conds, "c.project_id = ?")
		args.AddString(project)
	}
	switch status {
	case "":
		// no status filter — every task
	case "open":
		// convenience alias: the non-terminal set (pending + active + blocked)
		conds = append(conds, "t.status NOT IN ('closed', 'cancelled')")
	default:
		conds = append(conds, "t.status = ?")
		args.AddString(status)
	}
	if chain != "" {
		conds = append(conds, "c.slug = ?")
		args.AddString(chain)
	}
	if p.Since != "" {
		conds = append(conds, "t.created_at >= ?")
		args.AddString(p.Since)
	}
	whereClause := ""
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
	}

	limitClause := ""
	if limit >= 0 {
		if offset > 0 {
			limitClause = fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
		} else {
			limitClause = fmt.Sprintf("LIMIT %d", limit)
		}
	}

	query := fmt.Sprintf(`SELECT t.id, t.chain_id, c.slug, t.slug, t.position, t.status,
	      t.problem_statement, t.created_at, t.updated_at
	      FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
	      %s ORDER BY c.slug, t.position %s`, whereClause, limitClause)
	rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
	if err != nil {
		return TaskListResult{}, err
	}
	defer rows.Close()
	items, err := scanTaskListItems(rows)
	if err != nil {
		return TaskListResult{}, err
	}
	return TaskListResult{List: items}, nil
}

// chainSlugByID resolves a chain's numeric PK to its slug so task_list can
// accept a chain_id and route through the existing slug-keyed query path.
func chainSlugByID(ctx context.Context, pool *db.Pool, chainID int64) (string, error) {
	var slug string
	err := pool.DB().QueryRowContext(ctx, `SELECT slug FROM proj_chain_status WHERE id = ?`, chainID).Scan(&slug)
	return slug, err
}

func listTasksByChain(ctx context.Context, pool *db.Pool, chainSlug, pattern, statusFilter string) (TaskListResult, error) {
	var chainID int64
	err := pool.DB().QueryRowContext(ctx, `SELECT id FROM proj_chain_status WHERE slug = ?`, chainSlug).Scan(&chainID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskListResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("chain '%s' not found", chainSlug)}}, nil
		}
		return TaskListResult{}, err
	}

	args := db.NewArgs()
	q := `SELECT t.id, t.chain_id, c.slug, t.slug, t.position, t.status,
	      t.problem_statement, t.created_at, t.updated_at
	      FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id WHERE c.slug = ?`
	args.AddString(chainSlug)
	if statusFilter != "" {
		q += ` AND t.status = ?`
		args.AddString(statusFilter)
	}
	if pattern != "" {
		like := "%" + pattern + "%"
		q += ` AND (t.problem_statement LIKE ? OR t.acceptance_criteria LIKE ?
			 OR t.context_required LIKE ? OR t.constraints LIKE ?
			 OR t.handoff_output LIKE ? OR t.slug LIKE ?)`
		args.AddString(like).AddString(like).AddString(like).AddString(like).AddString(like).AddString(like)
	}
	q += ` ORDER BY t.position`
	rows, err := pool.DB().QueryContext(ctx, q, args.Slice()...)
	if err != nil {
		return TaskListResult{}, err
	}
	defer rows.Close()
	items, err := scanTaskListItems(rows)
	if err != nil {
		return TaskListResult{}, err
	}
	return TaskListResult{List: items}, nil
}

func searchTasksSummary(ctx context.Context, pool *db.Pool, pattern, project string, max int64) (TaskListResult, error) {
	like := "%" + pattern + "%"
	q := `SELECT t.id, t.chain_id, c.slug, t.slug, t.position, t.status,
	      t.problem_statement, t.created_at, t.updated_at
	      FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
	      WHERE (t.problem_statement LIKE ? OR t.acceptance_criteria LIKE ?
	             OR t.context_required LIKE ? OR t.constraints LIKE ?
	             OR t.handoff_output LIKE ? OR t.slug LIKE ?)`
	args := db.NewArgs().AddString(like).AddString(like).AddString(like).AddString(like).AddString(like).AddString(like)
	if project != "" {
		q += ` AND c.project_id = ?`
		args.AddString(project)
	}
	q += ` LIMIT ?`
	args.AddInt64(max)
	rows, err := pool.DB().QueryContext(ctx, q, args.Slice()...)
	if err != nil {
		return TaskListResult{}, err
	}
	defer rows.Close()
	items, err := scanTaskListItems(rows)
	if err != nil {
		return TaskListResult{}, err
	}
	return TaskListResult{List: items}, nil
}

func scanTaskListItems(rows *sql.Rows) ([]TaskListItem, error) {
	// Non-nil zero-length slice so JSON marshals as `[]`, not `null`.
	out := []TaskListItem{}
	for rows.Next() {
		var it TaskListItem
		var ps string
		if err := rows.Scan(&it.ID, &it.ChainID, &it.ChainSlug, &it.Slug,
			&it.Position, &it.Status, &ps, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		it.Title = deriveTaskTitle(ps)
		out = append(out, it)
	}
	return out, rows.Err()
}

// deriveTaskTitle: first non-empty line of problem_statement, truncated to
// 120 chars with ellipsis. Mirrors Rust derive_task_title.
func deriveTaskTitle(ps string) string {
	for _, line := range strings.Split(ps, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) <= 120 {
			return line
		}
		return string(r[:120]) + "…"
	}
	return ""
}

// ---------- transition handlers ----------

// taskTransitionParams captures task_start / task_cancel / task_reopen.
// task_complete has its own params with handoff_output + commit_sha.
//
// Identifier discipline: either {slug, chain_slug?} or {id}. `chain` is
// accepted as an alias for `chain_slug` so callers reaching for the same
// shorthand task_search uses don't get silently ignored.
type taskTransitionParams struct {
	Slug      string `json:"slug"`
	TaskSlug  string `json:"task_slug"` // bug 1441 alias
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"` // bug 1441 alias
	ChainSlug string `json:"chain_slug"`
	Chain     string `json:"chain"` // alias for chain_slug — silently mapped
}

func (p taskTransitionParams) resolvedSlug() string { return firstNonEmpty(p.Slug, p.TaskSlug) }
func (p taskTransitionParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.TaskID
}

// resolveTransitionIdentifier returns the canonical (slug, chain_slug) for
// the task the transition handlers should act on. When {id} is supplied it
// wins (mirrors task_read's bug-1329-parity discipline); otherwise the
// (slug, chain_slug) pair is passed through, with `chain` accepted as an
// alias for chain_slug.
func resolveTransitionIdentifier(ctx context.Context, pool *db.Pool, p taskTransitionParams) (slug, chainSlug string, err error) {
	if id := p.resolvedID(); id > 0 {
		var s, cs string
		e := pool.DB().QueryRowContext(ctx,
			`SELECT t.slug, c.slug FROM proj_current_tasks t
			 JOIN proj_chain_status c ON t.chain_id = c.id
			 WHERE t.id = ?`, id).Scan(&s, &cs)
		if e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return "", "", fmt.Errorf("task id=%d not found", id)
			}
			return "", "", e
		}
		return s, cs, nil
	}
	cs := firstNonEmpty(p.ChainSlug, p.Chain)
	return p.resolvedSlug(), cs, nil
}

func HandleTaskStart(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TaskTransitionResult, error) {
	var result TaskTransitionResult
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		r, _, err := HandleTaskStartInTx(ctx, tx, pool, project, params)
		result = r
		return err
	})
	if err != nil && result.Error == "" {
		return TaskTransitionResult{}, err
	}
	return result, nil
}

// HandleTaskStartInTx is the tx-aware variant of HandleTaskStart. Used
// by work.HandleBatch so task_start sub-ops join the outer batch tx.
// Returns (result, eventID, err) — eventID is the cascade
// TaskTransitioned event id (the final pending→active event; the
// blocked→pending auto-clear emits its own cascade event that is NOT
// returned to the caller because batch records one event per op).
func HandleTaskStartInTx(ctx context.Context, tx *sql.Tx, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskTransitionResult, string, error) {
	var p taskTransitionParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskTransitionResult{}, "", fmt.Errorf("parse params: %w", err)
		}
	}
	if p.resolvedID() == 0 && p.resolvedSlug() == "" {
		return TaskTransitionResult{Error: IdentifierRequiredError("task_start")}, "", nil
	}
	slug, chainSlug, err := resolveTransitionIdentifier(ctx, pool, p)
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, "", nil
	}
	if err := autoClearStatusOnlyBlockedIfNeededInTx(ctx, tx, pool, slug, chainSlug); err != nil {
		return TaskTransitionResult{Error: err.Error()}, "", nil
	}
	eventID, err := transitionTaskWithBlockerEdgesInTx(ctx, tx, pool, slug, "active", chainSlug, nil, nil, "", "")
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, "", nil
	}
	return TaskTransitionResult{OK: true, Slug: slug, Status: "active"}, eventID, nil
}

// autoClearStatusOnlyBlockedIfNeededInTx is the tx-aware variant of
// autoClearStatusOnlyBlockedIfNeeded used by the batch-aware Tx
// handlers. Same semantics; emits the cascade blocked→pending event on
// the supplied tx instead of opening a new one.
func autoClearStatusOnlyBlockedIfNeededInTx(ctx context.Context, tx *sql.Tx, pool *db.Pool, slug, chainSlug string) error {
	_, fromStatus, err := resolveTaskChainOn(ctx, tx, slug, chainSlug)
	if err != nil {
		return err
	}
	if fromStatus != "blocked" {
		return nil
	}
	hasEdges, err := taskHasStructuralBlockersOn(ctx, tx, slug, chainSlug)
	if err != nil {
		return err
	}
	if hasEdges {
		return nil
	}
	_, err = transitionTaskWithBlockerEdgesInTx(ctx, tx, pool, slug, "pending", chainSlug, nil, nil, "", "")
	return err
}

// autoClearStatusOnlyBlockedIfNeeded inspects the task's current
// status; if it's 'blocked' AND no structural task_blockers edges
// exist, emits a TaskTransitioned(blocked→pending) event to clear the
// prose-only block. No-op if the task is in any other state or has
// structural edges. Errors are surfaced to the caller; the caller
// (HandleTaskStart) treats this auto-clear as advisory — if it
// no-ops because of structural edges, the subsequent pending→active
// call will still hit the natural state-machine rejection at
// checkTaskTransition.
func autoClearStatusOnlyBlockedIfNeeded(ctx context.Context, pool *db.Pool, slug, chainSlug string) error {
	_, fromStatus, err := resolveTaskChain(ctx, pool, slug, chainSlug)
	if err != nil {
		return err
	}
	if fromStatus != "blocked" {
		return nil
	}
	hasEdges, err := taskHasStructuralBlockers(ctx, pool, slug, chainSlug)
	if err != nil {
		return err
	}
	if hasEdges {
		// Real blocker. Let the downstream state-machine gate reject
		// the blocked→active transition naturally — the resulting
		// error message names the structural blocker, which is the
		// signal the caller needs.
		return nil
	}
	return transitionTask(ctx, pool, slug, "pending", chainSlug, nil, nil)
}

// taskHasStructuralBlockers reports whether the named task has any
// structural task_blockers edges (proj_task_blockers rows where
// blocked_task_id is this task's id). Resolves the task id via
// (slug, chain_slug) for disambiguation when slugs aren't globally
// unique.
func taskHasStructuralBlockers(ctx context.Context, pool *db.Pool, slug, chainSlug string) (bool, error) {
	return taskHasStructuralBlockersOn(ctx, pool.DB(), slug, chainSlug)
}

// taskHasStructuralBlockersOn is the db.Queryer-parameterised variant.
func taskHasStructuralBlockersOn(ctx context.Context, q db.Queryer, slug, chainSlug string) (bool, error) {
	var n int64
	if chainSlug == "" {
		err := q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM proj_task_blockers b
			JOIN proj_current_tasks t ON t.id = b.blocked_task_id
			WHERE t.slug = ?`, slug).Scan(&n)
		if err != nil {
			return false, fmt.Errorf("query proj_task_blockers: %w", err)
		}
		return n > 0, nil
	}
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM proj_task_blockers b
		JOIN proj_current_tasks t ON t.id = b.blocked_task_id
		JOIN proj_chain_status c   ON c.id = t.chain_id
		WHERE t.slug = ? AND c.slug = ?`, slug, chainSlug).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("query proj_task_blockers: %w", err)
	}
	return n > 0, nil
}

// taskCompleteParams accepts handoff_output + commit_sha (sha alias).
// Identifier discipline matches taskTransitionParams: slug+chain_slug or id.
type taskCompleteParams struct {
	Slug          string  `json:"slug"`
	TaskSlug      string  `json:"task_slug"` // bug 1441 alias
	ID            int64   `json:"id"`
	TaskID        int64   `json:"task_id"` // bug 1441 alias
	ChainSlug     string  `json:"chain_slug"`
	Chain         string  `json:"chain"`
	HandoffOutput *string `json:"handoff_output"`
	CommitSHA     string  `json:"commit_sha"`
	SHA           string  `json:"sha"`
}

func (p taskCompleteParams) resolvedSlug() string { return firstNonEmpty(p.Slug, p.TaskSlug) }
func (p taskCompleteParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.TaskID
}

func HandleTaskComplete(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TaskTransitionResult, error) {
	var result TaskTransitionResult
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		r, _, err := HandleTaskCompleteInTx(ctx, tx, pool, project, params)
		result = r
		return err
	})
	if err != nil && result.Error == "" {
		return TaskTransitionResult{}, err
	}
	return result, nil
}

// HandleTaskCompleteInTx is the tx-aware variant of HandleTaskComplete.
// Used by work.HandleBatch so task_complete sub-ops join the outer
// batch tx. Returns (result, eventID, err) where eventID is the
// cascade TaskCompleted event id.
func HandleTaskCompleteInTx(ctx context.Context, tx *sql.Tx, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskTransitionResult, string, error) {
	var p taskCompleteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskTransitionResult{}, "", fmt.Errorf("parse params: %w", err)
		}
	}
	if p.resolvedID() == 0 && p.resolvedSlug() == "" {
		return TaskTransitionResult{Error: IdentifierRequiredError("task_complete")}, "", nil
	}
	slug, chainSlug, err := resolveTransitionIdentifier(ctx, pool, taskTransitionParams{
		Slug: p.resolvedSlug(), ID: p.resolvedID(), ChainSlug: p.ChainSlug, Chain: p.Chain,
	})
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, "", nil
	}
	sha := firstNonEmpty(p.CommitSHA, p.SHA)
	var shaPtr *string
	if sha != "" {
		if !isValidCommitSHAOrSentinel(sha) {
			return TaskTransitionResult{
				Error:  shaValidationError(sha, false),
				Action: "task_complete",
			}, "", nil
		}
		shaPtr = &sha
	}
	eventID, err := transitionTaskWithBlockerEdgesInTx(ctx, tx, pool, slug, "closed", chainSlug, p.HandoffOutput, shaPtr, "", "")
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, "", nil
	}
	resp := TaskTransitionResult{OK: true, Slug: slug, Status: "closed"}
	if shaPtr != nil {
		resp.CommitSHA = *shaPtr
	}
	return resp, eventID, nil
}

func HandleTaskCancel(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskTransitionResult, error) {
	var p taskTransitionParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskTransitionResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.resolvedID() == 0 && p.resolvedSlug() == "" {
		return TaskTransitionResult{Error: IdentifierRequiredError("task_cancel")}, nil
	}
	slug, chainSlug, err := resolveTransitionIdentifier(ctx, pool, p)
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	if err := transitionTask(ctx, pool, slug, "cancelled", chainSlug, nil, nil); err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	return TaskTransitionResult{OK: true, Slug: slug, Status: "cancelled"}, nil
}

// HandleTaskUnstart reverts an in-flight task from status=active back
// to status=pending. Closes bug 1461
// (task-lifecycle-no-clean-mid-flight-pause-or-revert): task_start was
// previously one-way; multi-session cold-pickup arcs had no clean way
// to surface "this task was started but the work paused mid-flight"
// to the next agent — the workaround was to enrich problem_statement
// with a "TASK STATUS WILL SHOW ACTIVE; IGNORE THAT" prose block.
// task_unstart is the substrate-side revert: emits a TaskTransitioned
// event (from=active, to=pending) so chain_state surfaces the task in
// the pending bucket rather than the active one. The transition is
// idempotent in spirit — calling it on an already-pending task errors
// with the standard transition-not-allowed message rather than
// silently no-op'ing.
func HandleTaskUnstart(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskTransitionResult, error) {
	var p taskTransitionParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskTransitionResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.resolvedID() == 0 && p.resolvedSlug() == "" {
		return TaskTransitionResult{Error: IdentifierRequiredError("task_unstart")}, nil
	}
	slug, chainSlug, err := resolveTransitionIdentifier(ctx, pool, p)
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	if err := transitionTask(ctx, pool, slug, "pending", chainSlug, nil, nil); err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	return TaskTransitionResult{OK: true, Slug: slug, Status: "pending"}, nil
}

func HandleTaskReopen(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskTransitionResult, error) {
	var p taskTransitionParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskTransitionResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.resolvedID() == 0 && p.resolvedSlug() == "" {
		return TaskTransitionResult{Error: IdentifierRequiredError("task_reopen")}, nil
	}
	slug, chainSlug, err := resolveTransitionIdentifier(ctx, pool, p)
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	// Land in pending (the backlog), NOT active — see TASK_TRANSITIONS
	// note + bug task-reopen-lands-in-active-not-documented-pending.
	if err := transitionTask(ctx, pool, slug, "pending", chainSlug, nil, nil); err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	return TaskTransitionResult{OK: true, Slug: slug, Status: "pending"}, nil
}

// taskStampParams accepts slug + commit_sha (sha alias) + optional chain_slug
// — or {id}, same parity as the transition handlers.
type taskStampParams struct {
	Slug      string `json:"slug"`
	TaskSlug  string `json:"task_slug"` // bug 1441 alias
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"` // bug 1441 alias
	ChainSlug string `json:"chain_slug"`
	Chain     string `json:"chain"`
	CommitSHA string `json:"commit_sha"`
	SHA       string `json:"sha"`
	// Bug 975: optional closure note captured by the same atomic stamp
	// call. Only meaningful when the stamp drives the pending/active →
	// closed transition (it rides into TaskCompletedPayload.ClosureSummary);
	// on an already-closed task it's rejected with a task_edit hint rather
	// than silently dropped.
	HandoffOutput *string `json:"handoff_output"`
}

func (p taskStampParams) resolvedSlug() string { return firstNonEmpty(p.Slug, p.TaskSlug) }
func (p taskStampParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.TaskID
}

func HandleTaskStampSHA(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (ShaStampResult, error) {
	var p taskStampParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ShaStampResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	sha := firstNonEmpty(p.CommitSHA, p.SHA)
	if (p.resolvedID() == 0 && p.resolvedSlug() == "") || sha == "" {
		return ShaStampResult{Error: IdentifierRequiredError("task_stamp_sha")}, nil
	}
	if !isValidCommitSHAOrSentinel(sha) {
		return ShaStampResult{
			Error:  shaValidationError(sha, false),
			Action: "task_stamp_sha",
		}, nil
	}
	slug, chainSlug, err := resolveTransitionIdentifier(ctx, pool, taskTransitionParams{
		Slug: p.resolvedSlug(), ID: p.resolvedID(), ChainSlug: p.ChainSlug, Chain: p.Chain,
	})
	if err != nil {
		return ShaStampResult{Error: err.Error()}, nil
	}
	chainID, status, err := resolveTaskChain(ctx, pool, slug, chainSlug)
	if err != nil {
		return ShaStampResult{Error: err.Error()}, nil
	}
	projectID, err := lookupChainProject(ctx, pool, chainID)
	if err != nil {
		return ShaStampResult{}, err
	}
	// Canonical chain slug — stamped on the emitted event so the fold
	// targets exactly this task (anti-fanout, bug `task-lifecycle-event-
	// folds-fan-out-across-duplicate-task-slugs`) AND used by the 882
	// chain-match guard below.
	canonicalChainSlug := lookupChainSlug(ctx, pool, chainID)
	if canonicalChainSlug == "" {
		canonicalChainSlug = chainSlug
	}
	// Bug `task-stamp-sha-accepts-foreign-chain-commit`: guard against a
	// mistyped task id stamping a commit that closes a DIFFERENT chain's
	// task (silent chain-state corruption). Best-effort — reads the
	// commit message via git and rejects only on the high-confidence
	// signal that the commit declares a different chain. Skips the
	// 'unversioned' sentinel and any case where the commit can't be read
	// (no repo path, git unavailable, commit not in local history) so an
	// infra gap never blocks a legitimate stamp.
	if sha != unversionedSentinel {
		if repoPath := lookupProjectPath(ctx, pool, projectID); repoPath != "" {
			if body, ok := gitCommitMessage(repoPath, sha); ok {
				if reason := commitChainMismatch(body, canonicalChainSlug, slug); reason != "" {
					return ShaStampResult{Error: reason, Action: "task_stamp_sha"}, nil
				}
			}
		}
	}
	// Bug 1402: when the task is already in flight (pending or
	// active), task_stamp_sha atomically completes and stamps it.
	// The verb-shape signals "this SHA captures the closure" — the
	// handoff_output gate that task_complete normally enforces on
	// pending→closed is bypassed by design (callers who want to
	// record a prose closure should use task_complete instead).
	// Blocked / cancelled / any other status still errors: those
	// states need explicit unblock / reopen first.
	// Bug 975: an optional handoff_output rides into the closure when the
	// stamp drives the pending/active → closed transition, so the natural
	// "stamp SHA + record handoff + close" flow is a single call instead of
	// the old stamp-then-task_complete sequence that errored closed→closed
	// and dropped the handoff. On an already-closed task the close has
	// already happened, so a supplied handoff would have nowhere coherent to
	// land here — reject with a hint at task_edit (the canonical
	// post-closure handoff-attach path) rather than silently dropping it.
	handoffSupplied := p.HandoffOutput != nil && *p.HandoffOutput != ""
	switch status {
	case "closed":
		if handoffSupplied {
			return ShaStampResult{
				Error:  fmt.Sprintf("task '%s' is already closed; task_stamp_sha cannot also record a handoff on it. Hint: stamp the SHA, then attach the handoff with task_edit(handoff_output=…) (or handoff_output_append to extend an existing one).", slug),
				Action: "task_stamp_sha",
			}, nil
		}
		err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
			// T5-tasks: CRUD UPDATE dropped; fold stamps the SHA on proj_current_tasks.
			_, err := events.Emit(ctx, tx, events.EmitArgs{
				Entity:  events.NewEntityRef("task", slug, projectID),
				Payload: events.TaskStampedPayload{ChainSlug: canonicalChainSlug, CommitSHA: sha},
			})
			return err
		})
	case "pending", "active":
		err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
			// T5-tasks: CRUD UPDATE dropped; fold for TaskCompleted sets status='closed' + commit_sha.
			if err := cleanupBlockersAfterClose(ctx, tx, chainID, slug); err != nil {
				return err
			}
			shaCopy := sha
			// Emit the close BEFORE sweeping so the TaskCompleted fold deletes
			// edges referencing this task; the sweep then catches dependents
			// whose only blocker was this task (same fix as
			// transitionTaskWithBlockerEdgesInTx).
			if _, err := events.Emit(ctx, tx, events.EmitArgs{
				Entity: events.NewEntityRef("task", slug, projectID),
				Payload: events.TaskCompletedPayload{
					ChainSlug:      canonicalChainSlug,
					CommitSHA:      &shaCopy,
					ClosureSummary: p.HandoffOutput,
				},
			}); err != nil {
				return err
			}
			return sweepProseOnlyBlockedAfterClose(ctx, tx, chainID, projectID, slug)
		})
	default:
		return ShaStampResult{
			Error: fmt.Sprintf("task '%s' is '%s'; cannot stamp a SHA from this state. Hint: task_reopen (or task_unblock) first, then call task_stamp_sha to complete and stamp in one call.", slug, status),
		}, nil
	}
	if err != nil {
		return ShaStampResult{}, err
	}
	return ShaStampResult{OK: true, Slug: slug, CommitSHA: sha}, nil
}

// ---------- blockers ----------

// taskBlockParams captures task_block + task_unblock — block adds the
// blocker_slug + reason; unblock can omit blocker_slug to drop all.
//
// Identifier aliases: a task may be named by slug (+ optional chain_slug
// to disambiguate cross-chain duplicates) OR by numeric id. Same applies
// to blockers (blocker_slug + blocker_chain_slug OR blocker_id). ID wins
// when both are supplied. The `task_slug` field is accepted as a doc-
// compatibility alias for `slug` — earlier versions of block-task.md
// named the field that way, and stale callers should not see a hard
// failure. The action manifest now documents `slug` as canonical.
//
// Action-doc-canonical aliases for the blocker fields: the action doc
// at go/internal/actiondocs/corpus/work/task_block.toml advertises `blocked_by`
// as the canonical name (with `blocker_slug` as the FROM-side alias),
// and `blocked_by_chain` as the canonical for the cross-chain hint
// (with `blocker_chain_slug` as the FROM-side alias). The Go struct's
// JSON tags historically only matched the FROM-side names, which meant
// agents calling with the documented canonical names hit a silent-
// success-no-edge path: default JSON unmarshal dropped the unknown
// field, the blocker block fell through, the row was never inserted.
// The BlockedBy / BlockedByChain fields below catch the canonical names
// at unmarshal time; HandleTaskBlock copies them into the FROM-side
// fields before doing any work. Both name shapes are now first-class.
type taskBlockParams struct {
	Slug             string `json:"slug"`
	TaskSlug         string `json:"task_slug"`
	ID               int64  `json:"id"`
	TaskID           int64  `json:"task_id"`
	ChainSlug        string `json:"chain_slug"`
	BlockerSlug      string `json:"blocker_slug"`
	BlockedBy        string `json:"blocked_by"`
	BlockerID        int64  `json:"blocker_id"`
	BlockerChainSlug string `json:"blocker_chain_slug"`
	BlockedByChain   string `json:"blocked_by_chain"`
	Reason           string `json:"reason"`
}

// normalizeAliases copies action-doc-canonical alias fields into the
// FROM-side fields the handler reads downstream. Called once at the top
// of HandleTaskBlock / HandleTaskUnblock so the rest of those functions
// see one shape regardless of which name the caller used. A non-empty
// FROM-side value wins over a non-empty TO-side value — if a caller
// somehow sent both, the older / original name is the safer fallback
// (and they should be equal anyway). Idempotent.
func (p *taskBlockParams) normalizeAliases() {
	if p.BlockerSlug == "" && p.BlockedBy != "" {
		p.BlockerSlug = p.BlockedBy
	}
	if p.BlockerChainSlug == "" && p.BlockedByChain != "" {
		p.BlockerChainSlug = p.BlockedByChain
	}
}

// resolveTaskBySlugOrID locates a task by id (preferred) or slug + optional
// chain_slug. Returns (id, chain_id, status). The error envelope-style hint
// uses the same field names the caller supplied so the agent can correct
// the call without re-reading docs.
func resolveTaskBySlugOrID(ctx context.Context, pool *db.Pool, slug string, id int64, chainSlug string) (int64, int64, string, error) {
	if id > 0 {
		var chainID int64
		var status string
		err := pool.DB().QueryRowContext(ctx,
			`SELECT chain_id, status FROM proj_current_tasks WHERE id = ?`, id).Scan(&chainID, &status)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, 0, "", fmt.Errorf("%w: task id=%d not found", ErrTaskNotFound, id)
			}
			return 0, 0, "", err
		}
		return id, chainID, status, nil
	}
	chainID, status, err := resolveTaskChain(ctx, pool, slug, chainSlug)
	if err != nil {
		return 0, 0, "", err
	}
	var tid int64
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT id FROM proj_current_tasks WHERE slug = ? AND chain_id = ?`, slug, chainID).Scan(&tid); err != nil {
		return 0, 0, "", err
	}
	return tid, chainID, status, nil
}

// taskBlockMissingTaskError builds the friendly error for a task_block /
// task_unblock call that named no identifier for the task being acted on.
// Names the actual accepted params so the agent can correct without
// re-reading docs.
func taskBlockMissingTaskError(action string, params json.RawMessage) string {
	keys := jsonTopLevelKeys(params)
	return fmt.Sprintf(
		"%s requires the task being %sed — pass `slug` (+ optional `chain_slug`) or `task_id`. Blocker is named via `blocker_slug` (+ optional `blocker_chain_slug`) or `blocker_id`. Received keys: %v",
		action, strings.TrimSuffix(strings.TrimPrefix(action, "task_"), "_task"), keys,
	)
}

// jsonTopLevelKeys returns the top-level keys of a JSON object params blob.
// Returns nil on parse failure or non-object input. Used in error messages
// so the caller sees which keys their malformed call did include.
func jsonTopLevelKeys(params json.RawMessage) []string {
	if len(params) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func HandleTaskBlock(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskTransitionResult, error) {
	var p taskBlockParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskTransitionResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	p.normalizeAliases()
	slug := firstNonEmpty(p.Slug, p.TaskSlug)
	id := p.ID
	if id == 0 {
		id = p.TaskID
	}
	if slug == "" && id == 0 {
		return TaskTransitionResult{Error: taskBlockMissingTaskError("task_block", params)}, nil
	}

	blockedID, _, current, err := resolveTaskBySlugOrID(ctx, pool, slug, id, p.ChainSlug)
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	// Recover the slug for downstream transition + response when only id supplied.
	if slug == "" {
		if err := pool.DB().QueryRowContext(ctx, `SELECT slug FROM proj_current_tasks WHERE id = ?`, blockedID).Scan(&slug); err != nil {
			return TaskTransitionResult{}, err
		}
	}

	if p.BlockerSlug != "" || p.BlockerID > 0 {
		bc := p.BlockerChainSlug
		if bc == "" {
			bc = p.ChainSlug
		}
		blockerID, _, blockerStatus, err := resolveTaskBySlugOrID(ctx, pool, p.BlockerSlug, p.BlockerID, bc)
		if err != nil {
			return TaskTransitionResult{Error: err.Error()}, nil
		}
		blockerLabel := p.BlockerSlug
		if blockerLabel == "" {
			blockerLabel = fmt.Sprintf("id=%d", p.BlockerID)
		}
		// Resolve blockerSlug from blockerID when only id was supplied —
		// the fold needs the slug to populate proj_task_blockers via
		// TaskTransitioned.BlockerSlug.
		blockerSlugForEmit := p.BlockerSlug
		if blockerSlugForEmit == "" {
			_ = pool.DB().QueryRowContext(ctx,
				`SELECT slug FROM proj_current_tasks WHERE id = ?`, blockerID).Scan(&blockerSlugForEmit)
		}
		if blockerID == blockedID {
			return TaskTransitionResult{Error: fmt.Sprintf("task '%s' cannot block itself", slug)}, nil
		}
		if blockerStatus == "closed" || blockerStatus == "cancelled" {
			return TaskTransitionResult{Error: fmt.Sprintf("task '%s' is already %s and cannot act as a blocker", blockerLabel, blockerStatus)}, nil
		}
		// INSERT OR IGNORE the structural edge. RowsAffected reports
		// whether a new edge actually landed (1) or the row was already
		// present (0) — used below to decide whether to emit a
		// TaskTransitioned. Pre-T3 the emit branched on `current !=
		// "blocked"` which silently swallowed every 2nd+ blocker INSERT.
		// T3 of agent-substrate-crud-retirement (§9.1) lifts that guard:
		// the audit ledger now records every new edge so payload-only
		// fold reconstruction (T5's contract) can rebuild
		// proj_task_blockers without joining the CRUD table.
		// T5-tasks: CRUD INSERT INTO task_blockers dropped. The fold
		// for TaskTransitioned with BlockerSlug set INSERTs the edge
		// into proj_task_blockers. Existence-check via projection
		// preserves the "was-already-present" no-emit branch below.
		var inserted int64
		var alreadyEdgeExists bool
		_ = pool.DB().QueryRowContext(ctx,
			`SELECT 1 FROM proj_task_blockers WHERE blocked_task_id = ? AND blocker_task_id = ?`,
			blockedID, blockerID).Scan(new(int))
		// If a row exists, alreadyEdgeExists stays true via the Scan.
		// Simpler: query COUNT.
		var existingEdges int64
		_ = pool.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM proj_task_blockers WHERE blocked_task_id = ? AND blocker_task_id = ?`,
			blockedID, blockerID).Scan(&existingEdges)
		if existingEdges == 0 {
			inserted = 1
		}
		_ = alreadyEdgeExists
		_ = p.Reason
		if current == "blocked" && inserted > 0 {
			// 2nd+ blocker case: status stays blocked but a new edge
			// landed. Pre-T3 this emitted no event; T3 (§9.1) requires
			// an explicit (blocked → blocked) self-transition carrying
			// the added blocker_slug so projection rebuild can see the
			// edge ADD without reading task_blockers. Emit directly via
			// events.Emit because the regular state-machine gate
			// (checkTaskTransition) rejects blocked → blocked as a
			// no-op transition — but the rejection is about the STATUS
			// flip, not about edge mutations, so we bypass and emit
			// the audit-only record.
			var projectID string
			var chainID int64
			if err := pool.DB().QueryRowContext(ctx, `SELECT chain_id FROM proj_current_tasks WHERE id = ?`, blockedID).Scan(&chainID); err == nil {
				projectID, _ = lookupChainProject(ctx, pool, chainID)
			}
			addedSlug := blockerSlugForEmit
			// Stamp the chain slug so the edge fold (foldTaskBlockersTransitioned)
			// resolves the blocked task chain-scoped, not by ambiguous (project,
			// slug). Bug task-blocker-edge-fold-resolves-by-project-slug-ignoring-chain.
			blockChainSlug := lookupChainSlug(ctx, pool, chainID)
			err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
				_, err := events.Emit(ctx, tx, events.EmitArgs{
					Entity: events.NewEntityRef("task", slug, projectID),
					Payload: events.TaskTransitionedPayload{
						ChainSlug:   blockChainSlug,
						FromStatus:  "blocked",
						ToStatus:    "blocked",
						BlockerSlug: &addedSlug,
					},
				})
				return err
			})
			if err != nil {
				return TaskTransitionResult{}, err
			}
			return TaskTransitionResult{OK: true, Slug: slug, Status: "blocked"}, nil
		}
	}
	if current != "blocked" {
		// Use the resolved blockerSlugForEmit so the fold can populate
		// proj_task_blockers when the user supplied blocker_id (not slug).
		blockerSlugForEmit := p.BlockerSlug
		if blockerSlugForEmit == "" && p.BlockerID > 0 {
			_ = pool.DB().QueryRowContext(ctx,
				`SELECT slug FROM proj_current_tasks WHERE id = ?`, p.BlockerID).Scan(&blockerSlugForEmit)
		}
		if err := transitionTaskWithBlocker(ctx, pool, slug, "blocked", p.ChainSlug, nil, nil, blockerSlugForEmit); err != nil {
			return TaskTransitionResult{Error: err.Error()}, nil
		}
	}
	return TaskTransitionResult{OK: true, Slug: slug, Status: "blocked"}, nil
}

func HandleTaskUnblock(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskTransitionResult, error) {
	var p taskBlockParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskTransitionResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	p.normalizeAliases()
	slug := firstNonEmpty(p.Slug, p.TaskSlug)
	id := p.ID
	if id == 0 {
		id = p.TaskID
	}
	if slug == "" && id == 0 {
		return TaskTransitionResult{Error: taskBlockMissingTaskError("task_unblock", params)}, nil
	}

	blockedID, _, _, err := resolveTaskBySlugOrID(ctx, pool, slug, id, p.ChainSlug)
	if err != nil {
		return TaskTransitionResult{Error: err.Error()}, nil
	}
	if slug == "" {
		if err := pool.DB().QueryRowContext(ctx, `SELECT slug FROM proj_current_tasks WHERE id = ?`, blockedID).Scan(&slug); err != nil {
			return TaskTransitionResult{}, err
		}
	}

	// removedBlockerSlugs collects which blocker edges were actually
	// deleted by this call, in deterministic order. Per T3 of agent-
	// substrate-crud-retirement (§9.1 audit finding), one TaskTransitioned
	// event must emit per removed edge so payload-only fold reconstruction
	// (T5's contract) can rebuild proj_task_blockers without joining the
	// CRUD table. Single-edge unblock: at most one entry; multi-edge
	// unblock-all (no blocker_slug supplied): one entry per existing edge.
	var removedBlockerSlugs []string
	if p.BlockerSlug != "" || p.BlockerID > 0 {
		bc := p.BlockerChainSlug
		if bc == "" {
			bc = p.ChainSlug
		}
		blockerID, _, _, err := resolveTaskBySlugOrID(ctx, pool, p.BlockerSlug, p.BlockerID, bc)
		if err != nil {
			return TaskTransitionResult{Error: err.Error()}, nil
		}
		blockerLabel := p.BlockerSlug
		if blockerLabel == "" {
			blockerLabel = fmt.Sprintf("id=%d", p.BlockerID)
		}
		// Recover the canonical blocker slug for the emit — when the
		// caller supplied an id-only blocker reference, we still want the
		// slug on the event payload so payload-only fold can match the
		// CRUD-side proj_task_blockers row by slug.
		blockerSlugForEmit := p.BlockerSlug
		if blockerSlugForEmit == "" {
			_ = pool.DB().QueryRowContext(ctx, `SELECT slug FROM proj_current_tasks WHERE id = ?`, blockerID).Scan(&blockerSlugForEmit)
		}
		var affected int64
		// T5-tasks: CRUD DELETE dropped. The fold for TaskTransitioned
		// with RemovedBlockerSlug DELETEs the proj_task_blockers edge.
		// Existence check via projection preserves the "not-blocked-by"
		// error path.
		var existing int64
		_ = pool.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM proj_task_blockers WHERE blocked_task_id = ? AND blocker_task_id = ?`,
			blockedID, blockerID).Scan(&existing)
		affected = existing
		if affected == 0 {
			return TaskTransitionResult{Error: fmt.Sprintf("task '%s' is not blocked by '%s'", slug, blockerLabel)}, nil
		}
		removedBlockerSlugs = append(removedBlockerSlugs, blockerSlugForEmit)
	} else {
		// Multi-edge unblock-all: read every existing edge's blocker slug
		// before the DELETE, then emit one TaskTransitioned per removed
		// edge inside the same tx so the audit ledger captures each edge
		// individually. Pre-T3 this collapsed every removal into at most
		// one event (the terminal pending-transition); T3 (§9.1) makes
		// each edge-drop addressable on the events ledger.
		// T5-tasks: multi-edge unblock — collect every edge's blocker
		// slug from proj_task_blockers (CRUD DELETE dropped; each edge's
		// removal is folded individually via the per-edge TaskTransitioned
		// emit below).
		rows, qerr := pool.DB().QueryContext(ctx,
			`SELECT bt.slug FROM proj_task_blockers tb
			 JOIN proj_current_tasks bt ON tb.blocker_task_id = bt.id
			 WHERE tb.blocked_task_id = ?
			 ORDER BY tb.created_at ASC, tb.blocker_task_id ASC`, blockedID)
		if qerr != nil {
			return TaskTransitionResult{}, qerr
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return TaskTransitionResult{}, err
			}
			removedBlockerSlugs = append(removedBlockerSlugs, s)
		}
		rows.Close()
	}

	// T5-tasks: edges aren't yet deleted from proj_task_blockers (fold
	// fires per-emit below). Subtract the about-to-be-removed count
	// from the current count to get the post-unblock remaining edges.
	var remaining int64
	if err := pool.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM proj_task_blockers WHERE blocked_task_id = ?`, blockedID).Scan(&remaining); err != nil {
		return TaskTransitionResult{}, err
	}
	remaining -= int64(len(removedBlockerSlugs))
	if remaining < 0 {
		remaining = 0
	}
	var nowStatus string
	if err := pool.DB().QueryRowContext(ctx, `SELECT status FROM proj_current_tasks WHERE id = ?`, blockedID).Scan(&nowStatus); err != nil {
		return TaskTransitionResult{}, err
	}
	// Per-edge audit emit. If the last edge cleared and the task is still
	// flagged blocked, the FINAL emit instead drives the status flip back
	// to pending — that emit carries removed_blocker_slug for the last
	// edge alongside the from=blocked → to=pending transition. Other
	// removed edges emit a (blocked → blocked) self-transition each so
	// the events ledger names every dropped edge.
	if len(removedBlockerSlugs) > 0 {
		// Recover projectID via the blocked task's chain so the emitted
		// TaskTransitioned events carry a full EntityRef. Empty project
		// is acceptable — events.Emit accepts cross-cutting entity refs.
		var projectID string
		var chainID int64
		if err := pool.DB().QueryRowContext(ctx, `SELECT chain_id FROM proj_current_tasks WHERE id = ?`, blockedID).Scan(&chainID); err == nil {
			projectID, _ = lookupChainProject(ctx, pool, chainID)
		}
		// Chain slug for the edge-removal emits so the fold resolves this
		// chain's task, not a same-slug sibling (bug task-blocker-edge-fold-
		// resolves-by-project-slug-ignoring-chain).
		unblockChainSlug := lookupChainSlug(ctx, pool, chainID)
		emitTerminalPending := remaining == 0 && nowStatus == "blocked"
		lastIdx := len(removedBlockerSlugs) - 1
		for i, bs := range removedBlockerSlugs {
			if emitTerminalPending && i == lastIdx {
				// transitionTaskWithBlockerEdges handles the (blocked
				// → pending) status flip + emit, threading the removed
				// slug into the payload.
				if err := transitionTaskWithBlockerEdges(ctx, pool, slug, "pending", p.ChainSlug, nil, nil, "", bs); err != nil {
					return TaskTransitionResult{Error: err.Error()}, nil
				}
				continue
			}
			// Edge-removal-only emit. No DB status flip; just the audit
			// ledger record. We invoke events.Emit directly because the
			// transition helpers gate on checkTaskTransition (blocked →
			// blocked would be rejected as a no-op).
			rb := bs
			err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
				_, err := events.Emit(ctx, tx, events.EmitArgs{
					Entity: events.NewEntityRef("task", slug, projectID),
					Payload: events.TaskTransitionedPayload{
						ChainSlug:          unblockChainSlug,
						FromStatus:         "blocked",
						ToStatus:           "blocked",
						RemovedBlockerSlug: &rb,
					},
				})
				return err
			})
			if err != nil {
				return TaskTransitionResult{}, err
			}
		}
	} else if nowStatus == "blocked" {
		// Status-only block: the task is flagged blocked but has no
		// structural task_blockers edge (the prose-only chain-creation
		// pattern forge(chain, tasks=[...]) produces for inline status=blocked
		// entries). There's no edge to remove, but the unblock request still
		// means "the prose dep cleared" — so drive the blocked→pending flip
		// directly. This makes task_unblock authoritative for status-only
		// blocks, honoring the advice the task_blockers status_only
		// diagnostic emits ("Run task_unblock if the dep cleared"). Before
		// this branch the flip was gated entirely on len(removedBlockerSlugs)
		// > 0, so a status-only unblock returned {ok:true} and silently did
		// nothing. transitionTaskWithBlockerEdges stamps the canonical chain
		// slug so the fold targets this task, not same-slug siblings. Bug
		// `task-unblock-noops-on-status-only-block-and-complete-sweep-doesnt-fire`.
		if err := transitionTaskWithBlockerEdges(ctx, pool, slug, "pending", p.ChainSlug, nil, nil, "", ""); err != nil {
			return TaskTransitionResult{Error: err.Error()}, nil
		}
	}
	return TaskTransitionResult{OK: true, Slug: slug}, nil
}

// taskBlockersParams reads the blockers list for slug + optional chain_slug,
// or id (bug 1387: id-by-default parity with task_read / task_block).
type taskBlockersParams struct {
	Slug      string `json:"slug"`
	TaskSlug  string `json:"task_slug"` // bug 1441 alias
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"` // bug 1441 alias
	ChainSlug string `json:"chain_slug"`
	Chain     string `json:"chain"` // bug 1441 alias
}

func (p taskBlockersParams) resolvedSlug() string  { return firstNonEmpty(p.Slug, p.TaskSlug) }
func (p taskBlockersParams) resolvedChain() string { return firstNonEmpty(p.ChainSlug, p.Chain) }
func (p taskBlockersParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.TaskID
}

func HandleTaskBlockers(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (TaskBlockersResult, error) {
	var p taskBlockersParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TaskBlockersResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.resolvedSlug() == "" && p.resolvedID() == 0 {
		return TaskBlockersResult{Err: &ErrorEnvelope{Error: IdentifierRequiredError("task_blockers")}}, nil
	}
	blockedID, _, _, err := resolveTaskBySlugOrID(ctx, pool, p.resolvedSlug(), p.resolvedID(), p.resolvedChain())
	if err != nil {
		return TaskBlockersResult{Err: &ErrorEnvelope{Error: err.Error()}}, nil
	}
	var taskStatus string
	if err := pool.DB().QueryRowContext(ctx, `SELECT status FROM proj_current_tasks WHERE id = ?`, blockedID).Scan(&taskStatus); err != nil {
		return TaskBlockersResult{}, err
	}
	rows, err := pool.DB().QueryContext(ctx,
		`SELECT bt.slug, bc.slug, tb.reason, tb.created_at
		 FROM proj_task_blockers tb
		 JOIN proj_current_tasks bt ON tb.blocker_task_id = bt.id
		 JOIN proj_chain_status bc ON bt.chain_id = bc.id
		 WHERE tb.blocked_task_id = ?
		 ORDER BY tb.created_at ASC, tb.blocker_task_id ASC`, blockedID)
	if err != nil {
		return TaskBlockersResult{}, err
	}
	defer rows.Close()
	out := []BlockerEntry{}
	for rows.Next() {
		var e BlockerEntry
		if err := rows.Scan(&e.Slug, &e.ChainSlug, &e.Reason, &e.CreatedAt); err != nil {
			return TaskBlockersResult{}, err
		}
		out = append(out, e)
	}
	// Bug 1379 + 1413: when task.status='blocked' but no structural
	// edge exists at all (intra- OR cross-chain), the empty list is
	// ambiguous — the task could be "unblocked but flag stale" OR
	// "blocked by a prose-only dependency the chain's design_decisions
	// records but no task_block call ever made structural". Surface
	// the gap with a synthetic kind="status_only" entry; callers
	// reading slug/chain_slug/reason stay backward-compatible (the
	// synthetic entry's slug/chain are empty), and kind-aware callers
	// learn to audit the chain's design_decisions for the prose dep.
	// Note: cross-chain task_blockers edges (written via task_block's
	// blocker_chain_slug parameter) DO surface in the structural list
	// above — they are not the trigger for this synthetic entry. Pre-
	// bug-1413 the message claimed cross-chain edges weren't tracked;
	// that was always a documentation error, never a code one.
	if len(out) == 0 && taskStatus == "blocked" {
		out = append(out, BlockerEntry{
			Kind:   "status_only",
			Reason: "task has status='blocked' but no structural task_blockers edge (intra- or cross-chain) — likely a prose-only dep the chain's design_decisions records that no task_block call ever made structural. Run task_unblock if the dep cleared, or task_block with the missing blocker to make it structural.",
		})
	}
	return TaskBlockersResult{List: out}, nil
}

// taskEditFieldShape classifies the accepted JSON shape for one content
// field. Mirrors the forge-schema field-type vocabulary on the matching
// task.toml entries so task_edit accepts the same encodings forge-create
// does — bug 1319 was the asymmetry between create (string-or-list) and
// edit (string-only).
type taskEditFieldShape int

const (
	// taskEditShapeString accepts a JSON string only. Used for fields
	// declared as `string` / `optional_string` in task.toml
	// (problem_statement, constraints, handoff_output).
	taskEditShapeString taskEditFieldShape = iota
	// taskEditShapeStringOrList accepts a JSON string OR an array of
	// strings. A list is rendered to the storage form by joining items
	// on "\n- " — the same shape forge's AsJoined produces and the
	// shape task_read returns for inline bullet rendering.
	taskEditShapeStringOrList
)

// taskEditOp classifies how a supplied field value combines with the
// existing column value. Bug 1423 added the append variant for
// handoff_output addenda (sanity-check audits etc.) so a closed task's
// handoff record can be extended without a read-then-write round-trip.
type taskEditOp int

const (
	// opSet replaces the column value with the supplied value.
	opSet taskEditOp = iota
	// opAppend concatenates the supplied value onto the existing column
	// value via SQLite `||`. Caller responsibility: include any leading
	// newline / separator they want; idempotence (re-appending the same
	// addendum produces duplicate text) is the caller's concern too.
	opAppend
)

// taskEditFieldSpec lists every editable input key with the shape it
// accepts, the column it writes, and how it combines (set vs append).
// Order is stable so the EditableFields envelope renders the same
// canonical list across calls — and so SQL set-list ordering is
// deterministic in tests.
var taskEditFieldSpec = []struct {
	name   string // top-level JSON key callers use
	column string // db column name
	shape  taskEditFieldShape
	op     taskEditOp
}{
	{"problem_statement", "problem_statement", taskEditShapeString, opSet},
	{"acceptance_criteria", "acceptance_criteria", taskEditShapeStringOrList, opSet},
	{"context_required", "context_required", taskEditShapeStringOrList, opSet},
	{"constraints", "constraints", taskEditShapeString, opSet},
	{"handoff_output", "handoff_output", taskEditShapeString, opSet},
	{"handoff_output_append", "handoff_output", taskEditShapeString, opAppend},
}

// taskEditableFields returns the canonical field-name list used in the
// no-fields and unknown-field error envelopes. Derived from
// taskEditFieldSpec so the doc and accept-set never drift.
func taskEditableFields() []string {
	out := make([]string, len(taskEditFieldSpec))
	for i, s := range taskEditFieldSpec {
		out[i] = s.name
	}
	return out
}

// taskEditRoutingKeys lists the top-level keys task_edit accepts that
// are NOT field updates — routing identifiers and the dispatch-layer
// rationale (which an agent may also nest into params by mistake; the
// dispatch layer detects that and surfaces a structured error before
// the handler runs). Used by the unknown-field check so legitimate
// routing keys don't trigger "unknown field" envelopes. Bug 1422.
var taskEditRoutingKeys = map[string]struct{}{
	"slug":       {},
	"task_slug":  {}, // bug 1441 alias parity
	"chain_slug": {},
	"chain":      {}, // bug 1441 alias parity
	"id":         {},
	"task_id":    {}, // bug 1441 alias parity
	"rationale":  {},
}

// decodeTaskEditField parses one field's raw JSON into its storage-form
// string per the declared shape. Mirrors forge's coerceFields so an
// array passed to acceptance_criteria stores the same value forge-create
// would have stored for the same input.
func decodeTaskEditField(raw json.RawMessage, shape taskEditFieldShape) (string, error) {
	// JSON null collapses to empty string for both shapes — matches the
	// "field present but emptied" semantic the pointer-string version used.
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	// String is the common path; try it first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	if shape == taskEditShapeString {
		return "", fmt.Errorf("expected string, got %s", jsonShapeOf(raw))
	}
	// taskEditShapeStringOrList: accept homogeneous string list and
	// heterogeneous (string|number|bool) list, the same permissive
	// shapes forge.FieldsFromJSON parses for optional_string_or_list.
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, "\n- "), nil
	}
	var mixed []json.RawMessage
	if err := json.Unmarshal(raw, &mixed); err == nil {
		items := make([]string, 0, len(mixed))
		for _, item := range mixed {
			items = append(items, jsonutil.ScalarToString(item))
		}
		return strings.Join(items, "\n- "), nil
	}
	return "", fmt.Errorf("expected string or array of strings, got %s", jsonShapeOf(raw))
}

// jsonShapeOf names the top-level JSON kind in raw — used in error
// messages so the caller can see why their value didn't fit (e.g.
// "got array of objects" beats raw json.Unmarshal error text).
func jsonShapeOf(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "empty"
	}
	switch trimmed[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// HandleTaskEdit updates the mutable content fields on an existing
// task. Per vault learning reference_task_edit_vs_forge_edit: task_edit
// accepts FLAT field names at the top level, not the field_name + value
// tuple that forge_edit uses.
//
// Per-field shapes follow the forge-schema declarations on task.toml:
// acceptance_criteria and context_required are optional_string_or_list
// (string or []string accepted; list renders to "\n- "-joined storage).
// problem_statement / constraints / handoff_output are string-shaped.
// Bug 1319 fixed the asymmetry that made forge-create accept lists but
// task_edit reject them with a parse-stage error.
//
// handoff_output_append (bug 1423) is a parallel input key that
// concatenates onto the existing handoff_output via SQLite `||` rather
// than replacing — sized for post-closure addenda (sanity-check audits,
// late telemetry) that previously required a read-then-write
// round-trip. Mutually exclusive with handoff_output in the same call.
//
// Per-field validation is independent: a single bad field doesn't abort
// the call, it returns a field_errors map alongside fields_written for
// the fields that did validate. Bug 1319 acceptance §4.
//
// Envelope-level validation (bug 1422) aggregates structural problems
// — missing slug, unknown field names, mutually-exclusive combinations
// — into a single `errors` list so a caller can fix the call shape in
// one revision instead of iterating. The dispatch-layer rationale gate
// fires before this handler runs and remains a separate envelope; that
// boundary is structural.
func HandleTaskEdit(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TaskEditResult, error) {
	var top map[string]json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &top); err != nil {
			return TaskEditResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	var slug, chainSlug string
	if raw, ok := top["slug"]; ok {
		_ = json.Unmarshal(raw, &slug)
	}
	if slug == "" {
		if raw, ok := top["task_slug"]; ok {
			_ = json.Unmarshal(raw, &slug)
		}
	}
	if raw, ok := top["chain_slug"]; ok {
		_ = json.Unmarshal(raw, &chainSlug)
	}
	if chainSlug == "" {
		if raw, ok := top["chain"]; ok {
			_ = json.Unmarshal(raw, &chainSlug)
		}
	}
	// Bug 1441 alias parity: id / task_id resolves to slug+chain_slug via
	// the same lookup the transition handlers use. When the caller passes
	// id-only (no slug), recover both routing values so the rest of the
	// handler operates on the canonical pair.
	if slug == "" {
		var id int64
		if raw, ok := top["id"]; ok {
			_ = json.Unmarshal(raw, &id)
		}
		if id == 0 {
			if raw, ok := top["task_id"]; ok {
				_ = json.Unmarshal(raw, &id)
			}
		}
		if id > 0 {
			var s, cs string
			e := pool.DB().QueryRowContext(ctx,
				`SELECT t.slug, c.slug FROM proj_current_tasks t
				 JOIN proj_chain_status c ON t.chain_id = c.id
				 WHERE t.id = ?`, id).Scan(&s, &cs)
			if e == nil {
				slug = s
				if chainSlug == "" {
					chainSlug = cs
				}
			} else if errors.Is(e, sql.ErrNoRows) {
				return TaskEditResult{Error: fmt.Sprintf("task id=%d not found", id)}, nil
			} else {
				return TaskEditResult{}, e
			}
		}
	}

	// --- envelope-level aggregation (bug 1422) ---
	//
	// Collect every structural problem we can detect before we decide
	// to short-circuit. The dispatch-layer rationale gate has already
	// passed by the time control reaches here, so missing rationale
	// is not in scope; everything else is.
	var envErrors []string
	if slug == "" {
		envErrors = append(envErrors, IdentifierRequiredError("task_edit"))
	}
	// Unknown-field check: a key in params that isn't a routing key
	// and isn't in the editable spec is almost always a typo
	// (`handoff_output_append` before bug 1423; misspellings of real
	// fields). Naming it explicitly avoids the silent-drop where the
	// call returned "no field updates supplied" with no hint about
	// the actually-supplied key.
	specNames := map[string]struct{}{}
	for _, s := range taskEditFieldSpec {
		specNames[s.name] = struct{}{}
	}
	unknownKeys := make([]string, 0)
	for k := range top {
		if _, isRouting := taskEditRoutingKeys[k]; isRouting {
			continue
		}
		if _, isField := specNames[k]; isField {
			continue
		}
		unknownKeys = append(unknownKeys, k)
	}
	sort.Strings(unknownKeys)
	for _, k := range unknownKeys {
		envErrors = append(envErrors, fmt.Sprintf("unknown field: %s", k))
	}
	// Mutually-exclusive combination check (bug 1423): set + append on
	// the same column has no obvious composition order, so reject
	// rather than guessing.
	if _, hasSet := top["handoff_output"]; hasSet {
		if _, hasApp := top["handoff_output_append"]; hasApp {
			envErrors = append(envErrors,
				"handoff_output and handoff_output_append are mutually exclusive in a single call")
		}
	}
	if len(envErrors) > 0 {
		return TaskEditResult{
			Error:          "validation failed",
			Errors:         envErrors,
			EditableFields: taskEditableFields(),
		}, nil
	}

	// Per-field decode. Each field is independent — one bad field
	// surfaces in field_errors while the rest still write.
	type pending struct {
		name   string // input key, for fields_written + field_errors
		column string // db column the value lands on
		value  string
		op     taskEditOp
	}
	var pendings []pending
	fieldErrors := map[string]string{}
	for _, spec := range taskEditFieldSpec {
		raw, ok := top[spec.name]
		if !ok {
			continue
		}
		val, err := decodeTaskEditField(raw, spec.shape)
		if err != nil {
			fieldErrors[spec.name] = err.Error()
			continue
		}
		pendings = append(pendings, pending{
			name:   spec.name,
			column: spec.column,
			value:  val,
			op:     spec.op,
		})
	}
	if len(pendings) == 0 && len(fieldErrors) == 0 {
		return TaskEditResult{
			Error:          "no field updates supplied",
			EditableFields: taskEditableFields(),
		}, nil
	}
	if len(pendings) == 0 {
		// Every supplied field failed validation. Don't open a write
		// transaction; surface the field_errors envelope.
		return TaskEditResult{
			Error:          "no fields could be parsed; see field_errors",
			FieldErrors:    fieldErrors,
			EditableFields: taskEditableFields(),
		}, nil
	}

	chainID, _, err := resolveTaskChain(ctx, pool, slug, chainSlug)
	if err != nil {
		return TaskEditResult{Error: err.Error()}, nil
	}

	sets := make([]string, 0, len(pendings)+1)
	args := db.NewArgs()
	written := make([]string, 0, len(pendings))
	// updatedValues captures the post-edit value for each changed column
	// so the TaskEdited payload's updated_values map (T3 of agent-
	// substrate-crud-retirement, §9.4) lets payload-only fold
	// reconstruction recover the new state without re-reading the row.
	// For opAppend the projection fold needs the appended-fragment value
	// alongside its column key — readers must consult the prior
	// updated_values entry for the same column to reconstruct the
	// concatenation result. updated_values[<name>] records exactly what
	// was sent on this call; the column-vs-input-key distinction is
	// preserved in the input-key form so handoff_output_append stays
	// distinguishable from a handoff_output replace at fold time.
	updatedValues := make(map[string]string, len(pendings))
	// Pre-resolve current column values for append operations so the
	// updated_values map carries the FINAL post-concatenation string
	// (the fold can't replicate SQLite's `||` operator from the input
	// alone). For replace operations the value goes through as-is.
	for _, u := range pendings {
		written = append(written, u.name)
		if u.op == opAppend {
			var existing sql.NullString
			_ = pool.DB().QueryRowContext(ctx,
				fmt.Sprintf(`SELECT %s FROM proj_current_tasks WHERE slug = ? AND chain_id = ?`, u.column),
				slug, chainID).Scan(&existing)
			final := existing.String + u.value
			updatedValues[u.column] = final
			// Also record the input-key form so the fold's
			// UpdatedFields-driven lookup matches (fold iterates
			// UpdatedFields which uses input-key form like
			// "handoff_output_append" not "handoff_output").
			updatedValues[u.name] = final
		} else {
			updatedValues[u.column] = u.value
			updatedValues[u.name] = u.value
		}
	}
	_ = sets
	_ = args
	_ = project
	projectID, err := lookupChainProject(ctx, pool, chainID)
	if err != nil {
		return TaskEditResult{}, err
	}
	// Anti-fanout chain disambiguation (bug `task-lifecycle-event-folds-
	// fan-out-across-duplicate-task-slugs`).
	chainSlugForEmit := lookupChainSlug(ctx, pool, chainID)
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-tasks: CRUD UPDATE dropped; fold applies updated_values
		// to proj_current_tasks.
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("task", slug, projectID),
			Payload: events.TaskEditedPayload{ChainSlug: chainSlugForEmit, UpdatedFields: written, UpdatedValues: updatedValues},
		})
		return err
	})
	if err != nil {
		return TaskEditResult{}, err
	}
	resp := TaskEditResult{OK: true, Slug: slug, FieldsWritten: written}
	if len(fieldErrors) > 0 {
		resp.FieldErrors = fieldErrors
	}
	return resp, nil
}

// ── Action-doc descriptors (parallel-run registry, action_doc.go) ───────

var taskReadDoc = ActionDoc{
	Purpose: "Return a task's full content. Identify by `id` (preferred — globally unique) OR `slug` + `chain_slug`.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id. Preferred when known — slug alone is not globally unique."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug to disambiguate."},
		{Name: "chain_slug", Required: false, Description: "Chain slug. Required when identifying by slug."},
	},
	Example: `{"slug":"design-telemetry-substrate","chain_slug":"query-telemetry-substrate"}`,
	Notes:   "task_read accepts {slug} or {id} (bug 1329 parity).",
}

var taskSearchDoc = ActionDoc{
	Purpose: "Search tasks by slug/title pattern. Returns compact rows. With `chain` filter, pattern is optional and results return in position order.",
	Params: []DocParam{
		{Name: "chain", Required: false, Description: "Chain slug to scope to. Alias: chain_slug."},
		{Name: "chain_id", Required: false, Description: "Numeric chain PK to scope to (resolved to the slug) — the same chain handle chain_state accepts, so a chain_find result works on either action. A slug accidentally passed here is tolerated (routed to the chain slug)."},
		{Name: "pattern", Required: false, Description: "Substring to match (case-insensitive). Aliases: query, text, q."},
		// verbose has no backing field on taskSearchParams (the handler reads it
		// separately), so its Type is AUTHORED, not derived.
		{Name: "verbose", Required: false, Description: "Return full Task records instead of compact rows.", Type: "bool"},
	},
	Example: `{"chain":"agent-first-substrate","verbose":true}`,
}

var taskListDoc = ActionDoc{
	Purpose: "List tasks across chains (the cross-cutting open-task verb). With NO params, returns every task (bounded by the default limit of 50); cross-project when no project is set, project-scoped when one is. Use `status:\"open\"` for the non-terminal set (pending+active+blocked). For a single chain's tasks in position order, or a title/slug substring search, use task_search instead.",
	Params: []DocParam{
		{Name: "status", Required: false, Description: "Filter by task status. `open` is a convenience alias for the non-terminal set (NOT closed/cancelled — i.e. pending+active+blocked); pending / active / blocked / closed / cancelled match exactly. Omit for all statuses. Alias: state."},
		{Name: "state", Required: false, Description: "Alias of status.", AliasOf: "status"},
		{Name: "since", Required: false, Description: "ISO timestamp; only tasks created at or after this time (t.created_at >=)."},
		{Name: "verbose", Required: false, Description: "Accepted for shape parity with bug_list/suggestion_list; currently a no-op (task_list has one compact projection)."},
		{Name: "all", Required: false, Description: "Legacy no-op, accepted for shape parity."},
		{Name: "limit", Required: false, Description: "Max rows (default 50)."},
		{Name: "offset", Required: false, Description: "Row offset for pagination."},
		{Name: "chain", Required: false, Description: "Optional chain slug to narrow the list to one chain. Alias: chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Alias of chain.", AliasOf: "chain"},
	},
	Example: `{"status":"open"}`,
	SeeAlso: "task_search",
}

var taskStartDoc = ActionDoc{
	Purpose: "Transition a task to status=active.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred)."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug."},
	},
	Example:              `{"id":6326}`,
	EnvelopeRequirements: rationaleEnv(),
}

var taskCompleteDoc = ActionDoc{
	Purpose: "Transition a task to status=closed with a closure note + optional commit SHA.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred)."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug."},
		{Name: "commit_sha", Required: false, Description: "SHA the work landed in. Accepts 'unversioned' for non-repo fixes (see skill:bug-filing-discipline §Unversioned-artifact exception)."},
		{Name: "sha", Required: false, Description: "Alias of commit_sha.", AliasOf: "commit_sha"},
	},
	Example: `{"id":6326,"commit_sha":"abc1234"}`,
	Notes: "Companion task_stamp_sha(slug, commit_sha) stamps a commit SHA on an already-closed task — mirrors bug_stamp_sha for when the SHA wasn't available at task_complete time; rejects non-closed tasks.\n\n" +
		"Post-close side effects: (1) structural task_blockers edges pointing AT the closing task are deleted; any task whose last structural blocker was just removed flips from 'blocked' to 'pending' automatically (cleanupBlockersAfterClose). (2) Prose-only-blocked tasks in the same chain — tasks with status='blocked' but NO structural task_blockers rows — also flip to 'pending' automatically (sweepProseOnlyBlockedAfterClose). This handles the chain-authoring pattern where 'X blocks Y' is recorded in design_decisions prose without a structural task_block call. Each swept task emits its own TaskTransitioned event so the audit ledger captures the state change. The sweep does NOT fire on task_cancel — cancelled tasks weren't completed in the satisfies-prose-dep sense.",
	EnvelopeRequirements: rationaleEnv(),
}

var taskCancelDoc = ActionDoc{
	Purpose: "Transition a task to status=cancelled.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred)."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug."},
	},
	Example:              `{"id":6326}`,
	EnvelopeRequirements: rationaleEnv(),
}

var taskReopenDoc = ActionDoc{
	Purpose: "Reopen a closed or cancelled task, returning it to status=pending (the backlog) so it becomes pickup-eligible again. Use task_start to then move it to active.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred)."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug."},
	},
	Example:              `{"id":6326}`,
	EnvelopeRequirements: rationaleEnv(),
}

var taskUnstartDoc = ActionDoc{
	Purpose: "Revert an in-flight task from status=active back to status=pending. Use for the mid-flight pause/revert case (multi-session cold-pickup, context-budget exhaustion) where task_start was called but the work paused without landing.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred)."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug."},
	},
	Example: `{"id":6326}`,
}

var taskStampShaDoc = ActionDoc{
	Purpose: "Stamp a commit SHA on a task. On an already-closed task this records a late-discovered SHA. On a PENDING or ACTIVE task it ATOMICALLY CLOSES the task and stamps the SHA in one call (the stamp verb signals 'this commit captures the closure'); pass handoff_output to record the closure note in that same call. Blocked/cancelled tasks are rejected — reopen/unblock first.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred)."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug."},
		{Name: "commit_sha", Required: true, Description: "SHA to stamp. Alias: sha. Accepts 'unversioned' for non-repo fixes."},
		{Name: "sha", Required: false, Description: "Alias of commit_sha.", AliasOf: "commit_sha"},
		{Name: "handoff_output", Required: false, Description: "Closure note recorded when this stamp closes a pending/active task (rides into the same TaskCompleted as the SHA, so 'stamp + record handoff + close' is one call — no follow-up task_complete, which would error closed→closed). Rejected on an already-closed task: stamp the SHA, then attach the handoff via task_edit(handoff_output=…)."},
	},
	Example:              `{"id":6326,"sha":"abc1234"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var taskBlockDoc = ActionDoc{
	Purpose: "Add a blocker to a task: the blocked task waits until the blocker resolves. Cross-chain blockers work by passing blocker_chain_slug alongside blocker_slug.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Blocked task id (preferred)."},
		{Name: "task_id", Required: false, Description: "Alias of id.", AliasOf: "id"},
		{Name: "slug", Required: false, Description: "Blocked task slug. Pair with chain_slug."},
		{Name: "task_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "chain_slug", Required: false, Description: "Chain slug of the blocked task."},
		{Name: "blocker_id", Required: false, Description: "Blocker task id (preferred)."},
		{Name: "blocker_slug", Required: false, Description: "Blocker task slug. Pair with blocker_chain_slug for cross-chain blockers."},
		{Name: "blocker_chain_slug", Required: false, Description: "Chain slug of the blocker task. Required when blocker lives in a different chain than the blocked task."},
		{Name: "reason", Required: false, Description: "Free-form note explaining why the blocker applies."},
	},
	Example:              `{"slug":"telemetry-cleanup","chain_slug":"telemetry-substrate-cleanup","blocker_slug":"chain-retrospective","blocker_chain_slug":"agent-first-substrate","reason":"deferred until foundation closes"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var taskUnblockDoc = ActionDoc{
	Purpose: "Remove a previously-set blocker from a task.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Blocked task id (preferred)."},
		{Name: "slug", Required: false, Description: "Blocked task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug of the blocked task."},
		{Name: "blocker_slug", Required: true, Description: "Slug of the blocker to remove."},
		{Name: "blocker_chain_slug", Required: false, Description: "Chain slug for cross-chain blockers."},
	},
	Example:              `{"slug":"telemetry-cleanup","blocker_slug":"chain-retrospective","blocker_chain_slug":"agent-first-substrate"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var taskBlockersDoc = ActionDoc{
	Purpose: "List the structural task_blockers edges pointing at a task. Both intra-chain AND cross-chain edges surface — any task_block call that wrote an edge (whether or not blocker_chain_slug pointed at the same chain as the blocked task) appears in the response. Each entry's chain_slug field names the BLOCKER's chain, so callers can tell intra- from cross-chain at a glance. If a task has status='blocked' with NO structural edge at all (intra or cross), the response includes one synthetic entry with kind='status_only' — that's the prose-only signal: the chain's design_decisions records a wait that no task_block call ever made structural. Bug 1413 corrected this description from an earlier text that claimed cross-chain edges were not tracked; the underlying SQL has always returned them, only the docs lagged.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred — id-by-default flows like chain_state → task_blockers avoid the extra slug lookup)."},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug."},
		{Name: "chain_slug", Required: false, Description: "Chain slug."},
	},
	Example: `{"slug":"interactions-and-resolutions-tables","chain_slug":"query-telemetry-substrate"}`,
	Notes:   "Reports structural intra-chain task_blockers edges only. When a task carries status='blocked' but has no structural edges, the result includes a synthetic blocker entry of kind='status_only' (per bug 1379 fix at 0b8c29e) — that signals either a cross-chain prose wait OR a state that's about to be cleaned up by a sibling task_complete's auto-sweep (cleanupBlockersAfterClose for structural edges, sweepProseOnlyBlockedAfterClose for prose-only entries). After bug task-complete-doesnt-auto-unblock-intra-chain-prose-deps's fix, the prose-only intra-chain case auto-resolves on the next task_complete in the chain; status_only sentinels are now most often cross-chain prose waits.",
}

// task_edit has no typed param struct (ParamStruct == nil) — every param Type
// is AUTHORED here, not derived.
var taskEditDoc = ActionDoc{
	Purpose: "Edit a task's content fields (problem_statement, acceptance_criteria, context_required, constraints, handoff_output). Pass handoff_output_append to concatenate onto the existing handoff_output (atomic, sized for post-closure addenda — sanity audits, late telemetry) rather than rewriting; mutually exclusive with handoff_output in the same call. Envelope-level validation problems (missing slug, unknown field names, mutually-exclusive combinations) aggregate into a single `errors` list. For richer schema-driven edits, prefer forge_edit on schema=task.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Task id (preferred).", Type: "int64"},
		{Name: "slug", Required: false, Description: "Task slug. Pair with chain_slug.", Type: "string"},
		{Name: "chain_slug", Required: false, Description: "Chain slug — needed because slug is unique only within a chain.", Type: "string"},
		{Name: "problem_statement", Required: false, Description: "Rewrite the task's problem statement.", Type: "string"},
		{Name: "acceptance_criteria", Required: false, Description: "Rewrite the task's acceptance criteria.", Type: "string"},
		{Name: "context_required", Required: false, Description: "Rewrite the task's required-context field.", Type: "string"},
		{Name: "constraints", Required: false, Description: "Rewrite the task's constraints field.", Type: "string"},
		{Name: "handoff_output", Required: false, Description: "Rewrite the task's handoff_output. Mutually exclusive with handoff_output_append.", Type: "string"},
		{Name: "handoff_output_append", Required: false, Description: "Append-mode addendum for handoff_output. Concatenated via SQLite `||`; caller controls leading separator (newline etc.). Mutually exclusive with handoff_output.", Type: "string"},
	},
	SeeAlso:              "forge_edit",
	Notes:                "forge_edit on a task accepts the same {chain_slug, slug, <fields…>} top-level shape as task_edit (the composite key is synthesized from chain_slug + slug); the structured form on tasks needs key: {chain_slug, slug}.",
	EnvelopeRequirements: rationaleEnv(),
}
