package refresolve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"toolkit/internal/db"
)

// chainResolver wraps the chains table; the detector has already
// confirmed Token ∈ chains.slug, so Resolve returns a single
// high-confidence Candidate with the chain's status / task counts
// as DebugNotes. Reads via a focused SELECT to avoid spinning up
// the full chain_find handler.
type chainResolver struct{ pool *db.Pool }

func (chainResolver) Shape() ShapeCategory   { return ShapeChainSlug }
func (chainResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 10} }

func (r chainResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	if r.pool == nil {
		return HitSet{ConfidenceTier: TierNoHit, Err: errors.New("chain resolver: db pool nil")}, nil
	}
	const q = `SELECT id, project_id, status,
        (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = chains.id) AS total_tasks,
        (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = chains.id AND status = 'pending') AS pending,
        (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = chains.id AND status = 'blocked') AS blocked
        FROM proj_chain_status AS chains WHERE slug = ?`
	var id, total, pending, blocked int64
	var project, status string
	err := r.pool.DB().QueryRowContext(ctx, q, ref.Token).Scan(&id, &project, &status, &total, &pending, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return HitSet{}, nil
	}
	if err != nil {
		return HitSet{Err: fmt.Errorf("chain lookup: %w", err)}, nil
	}
	return HitSet{
		Candidates: []Candidate{{
			ID:        ref.Token,
			Title:     fmt.Sprintf("chain %s in %s", ref.Token, project),
			Score:     1.0,
			SourceRef: "chain:" + ref.Token,
			DebugNotes: fmt.Sprintf("status=%s tasks=%d pending=%d blocked=%d",
				status, total, pending, blocked),
		}},
	}, nil
}

// taskResolver wraps the tasks table. A task slug may appear in
// multiple chains; Resolve returns one Candidate per match (most
// of the time exactly one) and lets the dispatcher classify the
// tier.
//
// DebugNotes carry both the task's own status AND the parent chain's
// status — so an agent referencing a task by slug can scan-check
// whether the chain is still active without a second round-trip
// (closes the task-side of bug
// `task-chain-bug-state-not-glanceable-from-id-or-slug-in-conversation-prose`).
type taskResolver struct{ pool *db.Pool }

func (taskResolver) Shape() ShapeCategory   { return ShapeTaskSlug }
func (taskResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 10} }

func (r taskResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	if r.pool == nil {
		return HitSet{ConfidenceTier: TierNoHit, Err: errors.New("task resolver: db pool nil")}, nil
	}
	const q = `SELECT t.id, t.status, t.position, c.slug, c.status
        FROM proj_current_tasks t JOIN proj_chain_status c ON c.id = t.chain_id
        WHERE t.slug = ?`
	rows, err := r.pool.DB().QueryContext(ctx, q, ref.Token)
	if err != nil {
		return HitSet{Err: fmt.Errorf("task lookup: %w", err)}, nil
	}
	defer rows.Close()
	cands := []Candidate{}
	for rows.Next() {
		var id, position int64
		var status, chainSlug, chainStatus string
		if err := rows.Scan(&id, &status, &position, &chainSlug, &chainStatus); err != nil {
			return HitSet{Err: fmt.Errorf("task scan: %w", err)}, nil
		}
		cands = append(cands, Candidate{
			ID:        ref.Token,
			Title:     fmt.Sprintf("task %s in chain %s", ref.Token, chainSlug),
			Score:     1.0,
			SourceRef: "task:" + chainSlug + "/" + ref.Token,
			DebugNotes: fmt.Sprintf("status=%s position=%d chain=%s:%s task_id=%d",
				status, position, chainSlug, chainStatus, id),
		})
	}
	if err := rows.Err(); err != nil {
		return HitSet{Err: fmt.Errorf("task rows: %w", err)}, nil
	}
	return HitSet{Candidates: cands}, nil
}

// bugResolver wraps the bugs table. Returns one Candidate per
// matching bug (slug is unique per project, so typically one
// match unless the same slug exists across projects).
//
// DebugNotes carry the bug's status, severity, and (when resolved)
// the resolution kind + routed_chain_slug / routed_task_slug /
// resolved_commit_sha so an agent referencing a closed bug in prose
// can scan-check the terminal disposition without a second round-trip
// (closes the bug-side of bug
// `task-chain-bug-state-not-glanceable-from-id-or-slug-in-conversation-prose`).
type bugResolver struct{ pool *db.Pool }

func (bugResolver) Shape() ShapeCategory   { return ShapeBugSlug }
func (bugResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 10} }

func (r bugResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	if r.pool == nil {
		return HitSet{ConfidenceTier: TierNoHit, Err: errors.New("bug resolver: db pool nil")}, nil
	}
	const q = `SELECT project_id, title, status, severity,
        COALESCE(resolution_kind, ''), routed_chain_slug, routed_task_slug,
        COALESCE(resolved_commit_sha, '')
        FROM proj_current_bugs WHERE slug = ?`
	rows, err := r.pool.DB().QueryContext(ctx, q, ref.Token)
	if err != nil {
		return HitSet{Err: fmt.Errorf("bug lookup: %w", err)}, nil
	}
	defer rows.Close()
	cands := []Candidate{}
	for rows.Next() {
		var project, title, status, severity, resolutionKind string
		var routedChainSlug, routedTaskSlug, resolvedCommitSHA string
		if err := rows.Scan(&project, &title, &status, &severity,
			&resolutionKind, &routedChainSlug, &routedTaskSlug, &resolvedCommitSHA); err != nil {
			return HitSet{Err: fmt.Errorf("bug scan: %w", err)}, nil
		}
		cands = append(cands, Candidate{
			ID:        ref.Token,
			Title:     fmt.Sprintf("bug %s in %s: %s", ref.Token, project, title),
			Score:     1.0,
			SourceRef: "bug:" + project + "/" + ref.Token,
			DebugNotes: composeBugDebugNotes(status, severity, resolutionKind,
				routedChainSlug, routedTaskSlug, resolvedCommitSHA),
		})
	}
	if err := rows.Err(); err != nil {
		return HitSet{Err: fmt.Errorf("bug rows: %w", err)}, nil
	}
	return HitSet{Candidates: cands}, nil
}

// composeBugDebugNotes renders the bug's status fields as the
// DebugNotes string. For open bugs the line stays compact
// (status+severity); for resolved bugs the resolution detail is
// appended so the agent's first lookup carries the terminal-state
// signal it would otherwise have to fetch via a second bug_read.
func composeBugDebugNotes(status, severity, resolutionKind, routedChainSlug, routedTaskSlug, resolvedCommitSHA string) string {
	base := fmt.Sprintf("status=%s severity=%s", status, severity)
	if status == "open" {
		return base
	}
	if resolutionKind != "" {
		base += " kind=" + resolutionKind
	}
	if routedChainSlug != "" {
		base += " routed_chain=" + routedChainSlug
		if routedTaskSlug != "" {
			base += "/" + routedTaskSlug
		}
	}
	if resolvedCommitSHA != "" {
		short := resolvedCommitSHA
		if len(short) > 7 {
			short = short[:7]
		}
		base += " sha=" + short
	}
	return base
}
