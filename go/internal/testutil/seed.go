package testutil

import (
	"database/sql"
	"testing"

	"toolkit/internal/db"
)

// nullStringIf returns a [sql.NullString] wrapping s when s != "",
// otherwise an invalid (SQL NULL) NullString. Used by the seed
// helpers to map empty-string opts to NULL columns without bare
// `any` parameter binding (forbidden in this repo outside the
// concentrated stdlib boundaries — see go-conventions).
func nullStringIf(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// Migration-066 invariant defaults. These are the fixture values used
// when a caller selects a status that requires the associated column
// to be non-NULL but doesn't override the value:
//
//   - bug.status != 'open'  ⇒ resolved_at must be non-NULL.
//   - task.status == 'closed' ⇒ commit_sha must be non-NULL.
//   - chain.status == 'closed' ⇒ closure_summary must be non-NULL
//     (empty string passes — the CHECK is `IS NOT NULL`, not `!= "`).
//
// The defaults match the values that the inlined seed helpers across the
// repo had converged on before consolidation (T2 of
// harvest-the-consolidation, 2026-05-22).
const (
	defaultFixtureResolvedAt     = "2026-05-22T00:00:00Z"
	defaultFixtureCommitSHA      = "0000000000000000000000000000000000000000"
	defaultFixtureClosureSummary = ""
)

// SeedBugOpts customizes the row written by SeedBug. All fields are
// optional; empty/zero values take the helper's defaults.
//
// Migration 066 invariants stay enforced regardless of opts: non-'open'
// statuses always get a non-NULL resolved_at (opts.ResolvedAt if set,
// otherwise [defaultFixtureResolvedAt]); 'open' rows always get NULL.
type SeedBugOpts struct {
	// Title defaults to "T:<slug>" when empty so list-endpoint
	// assertions can match on slug.
	Title string
	// Severity defaults to "medium".
	Severity string
	// ProblemStatement defaults to "" (column default).
	ProblemStatement string
	// Surface defaults to "" (column default).
	Surface string
	// Source defaults to "" (column default).
	Source string
	// ResolutionKind: empty string is written as NULL (the COALESCE()
	// in bugResolver mirrors the CRUD-row shape).
	ResolutionKind string
	// RoutedChainSlug, RoutedTaskSlug, RoutedSuggestionSlug default to
	// "" (column defaults).
	RoutedChainSlug      string
	RoutedTaskSlug       string
	RoutedSuggestionSlug string
	// ResolvedCommitSHA: empty string is written as NULL (the column
	// is NULLable; bugResolver expects NULL not "").
	ResolvedCommitSHA string
	// ResolvedAt overrides [defaultFixtureResolvedAt] for non-'open'
	// statuses. Ignored when status == "open" (always NULL).
	ResolvedAt string
	// FiledAt overrides datetime('now'). RFC3339 string expected.
	FiledAt string
}

// SeedBug inserts a row into proj_current_bugs and returns the assigned
// id. The id is derived from `COALESCE(MAX(id), 0) + 1` against the
// table so the helper is safe to call concurrently from the same test
// (each insert sees the prior row's id committed).
//
// Migration 066's biconditional `status='open' XNOR resolved_at IS NULL`
// is enforced by the helper. The CHECK on the table is the backstop;
// the helper just keeps the inline INSERT-INTO sites from each
// re-deriving the same logic.
func SeedBug(t *testing.T, pool *db.Pool, project, slug, status string, opts SeedBugOpts) int64 {
	t.Helper()
	title := opts.Title
	if title == "" {
		title = "T:" + slug
	}
	severity := opts.Severity
	if severity == "" {
		severity = "medium"
	}
	var resolvedAt sql.NullString
	if status != "open" {
		if opts.ResolvedAt != "" {
			resolvedAt = sql.NullString{String: opts.ResolvedAt, Valid: true}
		} else {
			resolvedAt = sql.NullString{String: defaultFixtureResolvedAt, Valid: true}
		}
	}
	// NULL-vs-empty-string for the NULLable columns (resolution_kind,
	// resolved_commit_sha). The historical resolver code uses COALESCE
	// over NULL not '', so the test fixture must distinguish. COALESCE
	// in the SQL VALUES clause translates the (?, datetime('now'))
	// pattern for filed_at / updated_at — a [sql.NullString] with
	// Valid=false binds as SQL NULL.
	resolutionKind := nullStringIf(opts.ResolutionKind)
	resolvedSHA := nullStringIf(opts.ResolvedCommitSHA)
	filedAtArg := nullStringIf(opts.FiledAt)
	res, err := pool.DB().Exec(
		`INSERT INTO proj_current_bugs
		    (id, slug, project_id, title, problem_statement, surface, severity, source,
		     status, resolution_kind, routed_chain_slug, routed_task_slug,
		     routed_suggestion_slug, resolved_commit_sha,
		     resolved_at, filed_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_bugs),
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         COALESCE(?, datetime('now')), COALESCE(?, datetime('now')))`,
		slug, project, title, opts.ProblemStatement, opts.Surface, severity, opts.Source,
		status, resolutionKind, opts.RoutedChainSlug, opts.RoutedTaskSlug,
		opts.RoutedSuggestionSlug, resolvedSHA, resolvedAt,
		filedAtArg, filedAtArg,
	)
	if err != nil {
		t.Fatalf("testutil.SeedBug %q: %v", slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("testutil.SeedBug %q LastInsertId: %v", slug, err)
	}
	return id
}

// SeedChainOpts customizes the row written by SeedChain. All fields
// optional.
type SeedChainOpts struct {
	// Output defaults to "" (column default).
	Output string
	// CompletionCondition defaults to "" (column default).
	CompletionCondition string
	// ClosureSummary overrides [defaultFixtureClosureSummary] for
	// closed chains. Ignored on status='open' (writes NULL).
	ClosureSummary string
	// CreatedAt overrides datetime('now'). RFC3339 string expected.
	// Use this to backdate fixtures for diff-window/recency tests.
	CreatedAt string
}

// SeedChain inserts a row into proj_chain_status and returns the
// assigned id. Migration 066's `status='closed' implies closure_summary
// IS NOT NULL` is enforced: closed chains write an empty-string default
// when no override is supplied (the CHECK is IS NOT NULL, not != ").
func SeedChain(t *testing.T, pool *db.Pool, project, slug, status string, opts SeedChainOpts) int64 {
	t.Helper()
	var closureSummary sql.NullString
	if status == "closed" {
		if opts.ClosureSummary != "" {
			closureSummary = sql.NullString{String: opts.ClosureSummary, Valid: true}
		} else {
			closureSummary = sql.NullString{String: defaultFixtureClosureSummary, Valid: true}
		}
	}
	createdAtArg := nullStringIf(opts.CreatedAt)
	res, err := pool.DB().Exec(
		`INSERT INTO proj_chain_status
		    (id, slug, project_id, status, output, completion_condition, closure_summary,
		     created_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM proj_chain_status),
		         ?, ?, ?, ?, ?, COALESCE(?, ''),
		         COALESCE(?, datetime('now')), COALESCE(?, datetime('now')))`,
		slug, project, status, opts.Output, opts.CompletionCondition, closureSummary,
		createdAtArg, createdAtArg,
	)
	if err != nil {
		t.Fatalf("testutil.SeedChain %q: %v", slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("testutil.SeedChain %q LastInsertId: %v", slug, err)
	}
	// SQLite stores '' for non-closed chains (column NOT NULL DEFAULT
	// ''); the COALESCE(?, '') in the INSERT keeps the closed-status
	// path NULL-safe while preserving the empty-string default.
	return id
}

// SeedTaskOpts customizes the row written by SeedTask. All fields optional.
type SeedTaskOpts struct {
	// Position defaults to the next ascending position within the
	// chain (derived from MAX(position)+1). Set explicitly to override.
	Position int64
	// ProblemStatement defaults to "" (column default).
	ProblemStatement string
	// AcceptanceCriteria defaults to "" (column default).
	AcceptanceCriteria string
	// ContextRequired defaults to "" (column default).
	ContextRequired string
	// Constraints defaults to "" (column default).
	Constraints string
	// HandoffOutput defaults to "" (column default).
	HandoffOutput string
	// CommitSHA overrides [defaultFixtureCommitSHA] for closed tasks.
	// Ignored on non-closed statuses (writes NULL).
	CommitSHA string
}

// SeedTask inserts a row into proj_current_tasks scoped to chainID and
// returns the assigned id. Migration 066's `status='closed' implies
// commit_sha IS NOT NULL` is enforced: closed tasks default to
// [defaultFixtureCommitSHA] when no override is supplied.
//
// SeedTask does NOT refresh proj_chain_status counter columns
// (total_tasks/pending/active/...). Tests that assert on those columns
// should call [RefreshChainCounters] after the SeedTask cluster, or
// use a per-package wrapper that bundles the refresh.
func SeedTask(t *testing.T, pool *db.Pool, chainID int64, slug, status string, opts SeedTaskOpts) int64 {
	t.Helper()
	var commitSHA sql.NullString
	if status == "closed" {
		if opts.CommitSHA != "" {
			commitSHA = sql.NullString{String: opts.CommitSHA, Valid: true}
		} else {
			commitSHA = sql.NullString{String: defaultFixtureCommitSHA, Valid: true}
		}
	}
	position := opts.Position
	if position == 0 {
		// Default to next slot within this chain so the
		// (chain_id, position) ordering tests stay coherent without
		// callers having to track their own per-chain counter.
		_ = pool.DB().QueryRow(
			`SELECT COALESCE(MAX(position), 0) + 1 FROM proj_current_tasks WHERE chain_id = ?`,
			chainID,
		).Scan(&position)
	}
	res, err := pool.DB().Exec(
		`INSERT INTO proj_current_tasks
		    (id, chain_id, slug, position, status, problem_statement, acceptance_criteria,
		     context_required, constraints, handoff_output, commit_sha,
		     created_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_tasks),
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         datetime('now'), datetime('now'))`,
		chainID, slug, position, status, opts.ProblemStatement, opts.AcceptanceCriteria,
		opts.ContextRequired, opts.Constraints, opts.HandoffOutput, commitSHA,
	)
	if err != nil {
		t.Fatalf("testutil.SeedTask %q: %v", slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("testutil.SeedTask %q LastInsertId: %v", slug, err)
	}
	return id
}

// RefreshChainCounters recomputes the proj_chain_status
// total_tasks/pending/active/blocked/closed/cancelled columns for
// chainID from the live proj_current_tasks rows. Call after seeding a
// cluster of tasks when the test asserts on chain-row counter values
// (the observehttp /chains list endpoint reads them; refresolve
// resolvers do not).
func RefreshChainCounters(t *testing.T, pool *db.Pool, chainID int64) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`UPDATE proj_chain_status SET
		    total_tasks = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ?),
		    pending     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'pending'),
		    active      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'active'),
		    blocked     = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'blocked'),
		    closed      = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'closed'),
		    cancelled   = (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status = 'cancelled')
		 WHERE id = ?`,
		chainID, chainID, chainID, chainID, chainID, chainID, chainID,
	); err != nil {
		t.Fatalf("testutil.RefreshChainCounters %d: %v", chainID, err)
	}
}

// SeedProject inserts an `INSERT OR IGNORE` row into the projects table.
// Several packages had a local seedProject helper duplicating this
// exact shape (observehttp, measure as inline). Consolidated here so
// the projects-row prerequisite doesn't get re-derived per package.
func SeedProject(t *testing.T, pool *db.Pool, id string) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT OR IGNORE INTO projects (id, name) VALUES (?, ?)`, id, id,
	); err != nil {
		t.Fatalf("testutil.SeedProject %q: %v", id, err)
	}
}
