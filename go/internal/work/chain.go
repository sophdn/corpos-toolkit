// Package work implements the toolkit-server work meta-tool action handlers:
// chain CRUD (T52), task CRUD (T53), bug CRUD (T54), roadmap (T55), and
// forge_edit / forge_schemas / forge_schema introspection (T76, T78).
//
// Each action is a thin handler over internal/db queries; SQL is verbatim
// from the Rust work-lib equivalents (PARITY_STANDARD §1a). Response
// shapes mirror Rust serde output so the dashboard / CLI clients see
// the same JSON regardless of which binary serves the request.
//
// Result shape: every handler in this package returns a named result
// struct (see BugListResult, ChainStatusResult, etc. in types.go).
// Widening to `any` happens once per registration via dispatch.Adapt
// in table.go — that adapter is the single JSON-marshaling seam.
package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// ChainSummary mirrors Rust work-lib::types::ChainSummary. SQL columns
// listed in chainSummarySelect map to the JSON tags below.
type ChainSummary struct {
	ID         int64  `json:"id"`
	ProjectID  string `json:"project_id"`
	Slug       string `json:"slug"`
	Status     string `json:"status"`
	TotalTasks int64  `json:"total_tasks"`
	Pending    int64  `json:"pending"`
	Active     int64  `json:"active"`
	Blocked    int64  `json:"blocked"`
	Closed     int64  `json:"closed"`
	Cancelled  int64  `json:"cancelled"`
}

// ChainDetail mirrors Rust work-lib::types::ChainDetail. design_decisions
// retired in migration 065 (Phase 4 F2) — the value rides on
// ChainCreated / ChainEdited event payloads; readers wanting it walk
// the events ledger instead of reading this struct.
type ChainDetail struct {
	ID                  int64  `json:"id"`
	ProjectID           string `json:"project_id"`
	Slug                string `json:"slug"`
	Status              string `json:"status"`
	Output              string `json:"output"`
	CompletionCondition string `json:"completion_condition"`
	ClosureSummary      string `json:"closure_summary"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

// chainSummarySelect is the SQL select clause for every ChainSummary-shaped
// query. Verbatim port of work-lib::chains::CHAIN_SUMMARY_SELECT.
const chainSummarySelect = `SELECT c.id, c.project_id, c.slug, c.status,
    (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = c.id) as total_tasks,
    (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = c.id AND status = 'pending') as pending,
    (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = c.id AND status = 'active') as active,
    (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = c.id AND status = 'blocked') as blocked,
    (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = c.id AND status = 'closed') as closed,
    (SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = c.id AND status = 'cancelled') as cancelled
 FROM proj_chain_status c`

// listChains returns chains for the supplied project (or all if empty),
// excluding terminal-state chains (closed / cancelled).
func listChains(ctx context.Context, pool *db.Pool, project string) ([]ChainSummary, error) {
	var rows *sql.Rows
	var err error
	if project != "" {
		rows, err = pool.DB().QueryContext(ctx,
			chainSummarySelect+
				` WHERE c.project_id = ? AND c.status NOT IN ('closed', 'cancelled') ORDER BY pending DESC`,
			project)
	} else {
		rows, err = pool.DB().QueryContext(ctx,
			chainSummarySelect+
				` WHERE c.status NOT IN ('closed', 'cancelled') ORDER BY pending DESC`)
	}
	if err != nil {
		return nil, fmt.Errorf("list_chains: %w", err)
	}
	defer rows.Close()
	return scanChainSummaries(rows)
}

// findChains lists chains, optionally narrowed by a slug substring
// (pattern) and/or a project. Both filters are optional: an empty pattern
// drops the slug LIKE clause (list mode), an empty project drops the
// project filter (cross-project). Terminal-state chains are NOT excluded
// here — chain_find surfaces closed chains too, unlike chain_status.
func findChains(ctx context.Context, pool *db.Pool, pattern, project string, max int64) ([]ChainSummary, error) {
	var conds []string
	args := db.NewArgs()
	if pattern != "" {
		conds = append(conds, "c.slug LIKE ?")
		args.AddString("%" + pattern + "%")
	}
	if project != "" {
		conds = append(conds, "c.project_id = ?")
		args.AddString(project)
	}
	q := chainSummarySelect
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY pending DESC LIMIT ?"
	args.AddInt64(max)
	rows, err := pool.DB().QueryContext(ctx, q, args.Slice()...)
	if err != nil {
		return nil, fmt.Errorf("find_chains: %w", err)
	}
	defer rows.Close()
	return scanChainSummaries(rows)
}

func scanChainSummaries(rows *sql.Rows) ([]ChainSummary, error) {
	out := []ChainSummary{}
	for rows.Next() {
		var c ChainSummary
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Slug, &c.Status,
			&c.TotalTasks, &c.Pending, &c.Active, &c.Blocked, &c.Closed, &c.Cancelled); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrChainNotFound is returned by getChain / closeChain when the (project,
// slug) pair doesn't resolve.
var ErrChainNotFound = errors.New("chain not found")

// ErrChainHasOpenTasks is returned by closeChain when one or more tasks
// remain in a non-terminal state.
type ErrChainHasOpenTasks struct {
	Slug    string
	Count   int64
	Message string
}

func (e *ErrChainHasOpenTasks) Error() string { return e.Message }

func getChain(ctx context.Context, pool *db.Pool, project, slug string) (ChainDetail, error) {
	var d ChainDetail
	const cols = `id, project_id, slug, status, output,
		completion_condition, closure_summary, created_at, updated_at`
	var row *sql.Row
	if project != "" {
		row = pool.DB().QueryRowContext(ctx,
			`SELECT `+cols+` FROM proj_chain_status WHERE project_id = ? AND slug = ?`, project, slug)
	} else {
		row = pool.DB().QueryRowContext(ctx,
			`SELECT `+cols+` FROM proj_chain_status WHERE slug = ?`, slug)
	}
	if err := row.Scan(&d.ID, &d.ProjectID, &d.Slug, &d.Status, &d.Output,
		&d.CompletionCondition, &d.ClosureSummary, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChainDetail{}, fmt.Errorf("%w: %s", ErrChainNotFound, slug)
		}
		return ChainDetail{}, fmt.Errorf("get_chain: %w", err)
	}
	return d, nil
}

// getChainByID parallels getChain but looks up by the chains.id PK —
// the identifier chain_find returns as the row's `id` field.
func getChainByID(ctx context.Context, pool *db.Pool, project string, id int64) (ChainDetail, error) {
	var d ChainDetail
	const cols = `id, project_id, slug, status, output,
		completion_condition, closure_summary, created_at, updated_at`
	var row *sql.Row
	if project != "" {
		row = pool.DB().QueryRowContext(ctx,
			`SELECT `+cols+` FROM proj_chain_status WHERE project_id = ? AND id = ?`, project, id)
	} else {
		row = pool.DB().QueryRowContext(ctx,
			`SELECT `+cols+` FROM proj_chain_status WHERE id = ?`, id)
	}
	if err := row.Scan(&d.ID, &d.ProjectID, &d.Slug, &d.Status, &d.Output,
		&d.CompletionCondition, &d.ClosureSummary, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChainDetail{}, fmt.Errorf("%w: id=%d", ErrChainNotFound, id)
		}
		return ChainDetail{}, fmt.Errorf("get_chain_by_id: %w", err)
	}
	return d, nil
}

// lookupChainSlugByID resolves a chain id to its slug. Lets chain_close
// accept {id} the way bug_resolve / bug_stamp_sha already do (bug 1329);
// the rest of the handler operates on the slug so the write SQL and
// error paths stay identical regardless of how the caller named the chain.
func lookupChainSlugByID(ctx context.Context, pool *db.Pool, id int64) (string, error) {
	var slug string
	err := pool.DB().QueryRowContext(ctx, `SELECT slug FROM proj_chain_status WHERE id = ?`, id).Scan(&slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("chain id %d not found", id)
		}
		return "", err
	}
	return slug, nil
}

// closeChain transitions a chain to closed. All tasks must already be in
// terminal state. Mirrors work-lib's close_chain — including the
// roadmap cleanup DELETE.
func closeChain(ctx context.Context, pool *db.Pool, project, slug string, summary *string) error {
	var chainID int64
	var projectID string
	var row *sql.Row
	if project != "" {
		row = pool.DB().QueryRowContext(ctx,
			`SELECT id, project_id FROM proj_chain_status WHERE project_id = ? AND slug = ?`, project, slug)
	} else {
		row = pool.DB().QueryRowContext(ctx,
			`SELECT id, project_id FROM proj_chain_status WHERE slug = ?`, slug)
	}
	if err := row.Scan(&chainID, &projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrChainNotFound, slug)
		}
		return fmt.Errorf("close_chain lookup: %w", err)
	}

	var nonTerminal int64
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status NOT IN ('closed', 'cancelled')`,
		chainID).Scan(&nonTerminal); err != nil {
		return fmt.Errorf("close_chain count: %w", err)
	}
	if nonTerminal > 0 {
		return &ErrChainHasOpenTasks{
			Slug:    slug,
			Count:   nonTerminal,
			Message: fmt.Sprintf("chain '%s' has %d non-terminal tasks", slug, nonTerminal),
		}
	}

	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-chains: CRUD UPDATE dropped; fold for ChainClosed sets
		// status='closed' + closure_summary on proj_chain_status.
		_ = chainID
		if _, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("chain", slug, projectID),
			Payload: events.ChainClosedPayload{ClosureSummary: summary},
		}); err != nil {
			return err
		}
		// T5-roadmap: cascade roadmap removal moves to the projection
		// fold. projections/roadmap.go's Fold for ChainClosed DELETEs
		// proj_roadmap_view rows where ref_kind='chain' AND ref_slug=slug
		// from the event payload alone. No CRUD write here.
		return nil
	})
}

// ---------- handlers ----------

// chainSlugParams captures the aliased slug param accepted across chain
// handlers: slug / chain / chain_slug. ID and ChainID accept the numeric
// PK chain_find returns as the row identifier — chain_state honors
// either form (slug-side aliases or id-side aliases) so a chain_find →
// chain_state pivot doesn't have to round-trip back through the slug.
type chainSlugParams struct {
	Slug      string `json:"slug"`
	Chain     string `json:"chain"`
	ChainSlug string `json:"chain_slug"`
	ID        int64  `json:"id"`
	ChainID   int64  `json:"chain_id"`
}

func (p chainSlugParams) resolved() string {
	return firstNonEmpty(p.Slug, p.Chain, p.ChainSlug)
}

// resolvedID returns the first non-zero numeric chain identifier among
// id / chain_id. Zero means the caller did not supply an id form.
func (p chainSlugParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.ChainID
}

// UnmarshalJSON tolerates the numeric id aliases (id / chain_id) being
// supplied as EITHER a JSON number or a string. A numeric string is parsed to
// the int; a NON-numeric string is treated as a slug — the common case where a
// caller passes the chain's slug into chain_id because the slug is the handle
// they hold (e.g. the one parse_context surfaced). This stops the raw "cannot
// unmarshal string into int64" Go error from leaking and lets a slug-in-chain_id
// resolve, so chain_state accepts the same handle whichever field it lands in.
// Bug work-chain-identifier-handling-inconsistent-across-actions.
func (p *chainSlugParams) UnmarshalJSON(data []byte) error {
	var raw struct {
		Slug      string          `json:"slug"`
		Chain     string          `json:"chain"`
		ChainSlug string          `json:"chain_slug"`
		ID        json.RawMessage `json:"id"`
		ChainID   json.RawMessage `json:"chain_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Slug, p.Chain, p.ChainSlug = raw.Slug, raw.Chain, raw.ChainSlug
	var slugFromID, slugFromChainID string
	p.ID, slugFromID = coerceChainID(raw.ID)
	p.ChainID, slugFromChainID = coerceChainID(raw.ChainID)
	// A non-numeric string in an id field is really a slug — route it to the
	// slug aliases when the caller gave no explicit slug.
	if p.resolved() == "" {
		p.Slug = firstNonEmpty(slugFromID, slugFromChainID)
	}
	return nil
}

// coerceChainID parses a chain id/chain_id field that may be a JSON number or a
// string. Returns (id, "") for a number or numeric string, (0, slug) for a
// non-numeric string, and (0, "") for absent/null/unparseable (object/array) —
// the latter leaves both empty so the handler returns its typed
// missing-identifier envelope rather than leaking a raw unmarshal error.
func coerceChainID(rm json.RawMessage) (int64, string) {
	if len(rm) == 0 || string(rm) == "null" {
		return 0, ""
	}
	var n int64
	if err := json.Unmarshal(rm, &n); err == nil {
		return n, ""
	}
	var s string
	if err := json.Unmarshal(rm, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, ""
		}
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v, ""
		}
		return 0, s
	}
	return 0, ""
}

// HandleChainStatus implements work.chain_status. With slug → returns
// that one chain's summary or a structured chain_not_found error. Without
// slug → returns the full open-chain list for the project.
func HandleChainStatus(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (ChainStatusResult, error) {
	var p chainSlugParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainStatusResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	slug := p.resolved()
	chains, err := listChains(ctx, pool, project)
	if err != nil {
		return ChainStatusResult{}, err
	}
	if slug == "" {
		return ChainStatusResult{HasList: true, List: chains}, nil
	}
	for i := range chains {
		if chains[i].Slug == slug {
			c := chains[i]
			return ChainStatusResult{HasSingle: true, Single: &c}, nil
		}
	}
	return ChainStatusResult{Err: &ErrorEnvelope{
		Error: "chain_not_found",
		Hint:  "chain_status with `slug` filters the open-chain list; chain_state returns full chain detail (output, completion_condition, closure_summary, etc). For the chain's design_decisions rationale, walk the entity's event timeline — ChainCreated/ChainEdited event payloads carry it (retired from the projection cache in migration 065 / Phase 4 F2).",
	}}, nil
}

// HandleChainState implements work.chain_state. Accepts either the
// slug aliases (slug / chain / chain_slug) or the numeric id aliases
// (id / chain_id, the PK chain_find surfaces).
func HandleChainState(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (ChainStateResult, error) {
	var p chainSlugParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainStateResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	slug := p.resolved()
	id := p.resolvedID()
	if slug == "" && id == 0 {
		return ChainStateResult{Err: missingChainSlugEnvelope()}, nil
	}
	if slug != "" {
		chain, err := getChain(ctx, pool, project, slug)
		if err != nil {
			if errors.Is(err, ErrChainNotFound) {
				return ChainStateResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("chain '%s' not found", slug)}}, nil
			}
			return ChainStateResult{}, err
		}
		return ChainStateResult{Detail: &chain}, nil
	}
	chain, err := getChainByID(ctx, pool, project, id)
	if err != nil {
		if errors.Is(err, ErrChainNotFound) {
			return ChainStateResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("chain id=%d not found", id)}}, nil
		}
		return ChainStateResult{}, err
	}
	return ChainStateResult{Detail: &chain}, nil
}

// chainFindParams captures the chain_find search params. The pattern
// resolves through pattern/query/text/q/slug in that order.
type chainFindParams struct {
	Pattern    string `json:"pattern"`
	Query      string `json:"query"`
	Text       string `json:"text"`
	Q          string `json:"q"`
	Slug       string `json:"slug"`
	Project    string `json:"project"`
	Max        int64  `json:"max"`
	Limit      int64  `json:"limit"`
	MaxResults int64  `json:"max_results"`
}

// HandleChainFind implements work.chain_find — substring search on slug,
// optionally scoped to a project. Both pattern and project are OPTIONAL:
// with neither, chain_find lists chains cross-project (closes suggestion
// #60 — there was previously no way to list a project's chains). When
// present, pattern LIKE-matches the slug and project narrows to one
// project. Cross-project-by-default is preserved: an empty project applies
// no project filter. The project resolves from params.project, falling
// back to the dispatch envelope project.
func HandleChainFind(ctx context.Context, pool *db.Pool, envProject string, params json.RawMessage) (ChainFindResult, error) {
	var p chainFindParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainFindResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	pattern := firstNonEmpty(p.Pattern, p.Query, p.Text, p.Q, p.Slug)
	project := firstNonEmpty(p.Project, envProject)
	max := firstNonZeroInt64(p.Max, p.Limit, p.MaxResults, 10)
	chains, err := findChains(ctx, pool, pattern, project, max)
	if err != nil {
		return ChainFindResult{}, err
	}
	return ChainFindResult{List: chains}, nil
}

// chainCloseParams + HandleChainClose. Refuses if any task is still
// non-terminal. Echoes back closure_summary length when supplied.
//
// Two equivalent param names for the closing summary text:
//   - `summary` — the short canonical form
//   - `closure_summary` — the column name + chain.toml forge-schema field name
//
// Either is accepted; if both are supplied, `closure_summary` wins (it's
// the schema-aligned name and likely the more deliberate one). The
// alias exists because callers reach for the schema field name when
// closing a chain whose completion_condition mentions closure_summary,
// and silently dropping the param costs an agent-debug cycle to notice.
//
// Identifier aliases: chain may also be named by numeric id (chain.id PK,
// the form chain_find returns) — id / chain_id resolve to slug via
// lookupChainSlugByID. Bug 1329 parity for the chain surface.
type chainCloseParams struct {
	Slug      string `json:"slug"`
	Chain     string `json:"chain"`
	ChainSlug string `json:"chain_slug"`
	ID        int64  `json:"id"`
	ChainID   int64  `json:"chain_id"`
	Summary   string `json:"summary"`
	// Pointer presence (vs. empty string) distinguishes "summary
	// omitted" from "summary explicitly empty". The JSON unmarshal
	// pass populates this field whenever the `summary` or
	// `closure_summary` key appears.
	SummarySupplied bool `json:"-"`
}

func (p chainCloseParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.ChainID
}

// UnmarshalJSON tracks whether the caller supplied `summary` (or its
// alias `closure_summary`) so the handler can distinguish "not provided"
// (skip the closure_summary UPDATE) from "explicitly empty" (clear it).
func (p *chainCloseParams) UnmarshalJSON(b []byte) error {
	type alias chainCloseParams
	var raw struct {
		alias
		SummaryRaw        *json.RawMessage `json:"summary"`
		ClosureSummaryRaw *json.RawMessage `json:"closure_summary"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = chainCloseParams(raw.alias)
	// closure_summary wins over summary when both appear: it's the
	// schema-aligned name and the more deliberate choice.
	chosen := raw.ClosureSummaryRaw
	if chosen == nil {
		chosen = raw.SummaryRaw
	}
	if chosen != nil {
		p.SummarySupplied = true
		if err := json.Unmarshal(*chosen, &p.Summary); err != nil {
			return err
		}
	}
	return nil
}

// HandleChainCloseInTx is the tx-aware variant of HandleChainClose,
// designed for work.batch ops that need the close + chain-state UPDATE
// to flow through the outer batch tx (so a later op's failure correctly
// rolls back the close). Mirrors HandleChainClose's param parsing +
// pre-flight validation; only the write phase changes from a self-
// opened pool.WithWrite to events.Emit against the passed-in tx.
//
// Returns (result, eventID, err): eventID is the ChainClosed cascade
// event's id (empty on failure). T1 of batch-allowlist-widening.
//
// Reads (lookupChainSlugByID, the pre-flight count of non-terminal
// tasks) continue to use pool.DB() — same documented batch limitation
// HandleBugResolveInTx + HandleTaskCompleteInTx carry: cross-op read-
// after-write inside a batch sees pre-batch state. For the canonical
// batch([task_complete, chain_close]) shape that DOES NOT bite, because
// the task_complete write flows through the same tx and the
// non-terminal-tasks count then sees the just-closed task: SQLite WAL
// snapshot isolation in the same connection means reads inside a tx see
// that tx's own writes. (Verified by smoke (a) — task_complete + the
// matching chain_close both land cleanly with the count query seeing
// the just-completed task.)
func HandleChainCloseInTx(ctx context.Context, tx *sql.Tx, pool *db.Pool, project string, params json.RawMessage) (ChainCloseResult, string, error) {
	var p chainCloseParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainCloseResult{}, "", fmt.Errorf("parse params: %w", err)
		}
	}
	slug := firstNonEmpty(p.Slug, p.Chain, p.ChainSlug)
	if slug == "" {
		if id := p.resolvedID(); id > 0 {
			resolved, err := lookupChainSlugByID(ctx, pool, id)
			if err != nil {
				return ChainCloseResult{Error: err.Error()}, "", nil
			}
			slug = resolved
		}
	}
	if slug == "" {
		return ChainCloseResult{
			Error: IdentifierRequiredError("chain_close"),
			Hint:  "Pass {\"slug\": \"<chain-slug>\"} or {\"id\": <chain-id>}; 'chain' and 'chain_slug' are also accepted as slug aliases.",
		}, "", nil
	}
	var summary *string
	if p.SummarySupplied {
		s := p.Summary
		summary = &s
	}
	eventID, err := closeChainInTx(ctx, tx, pool, project, slug, summary)
	if err != nil {
		var openTasks *ErrChainHasOpenTasks
		if errors.As(err, &openTasks) {
			return ChainCloseResult{Error: openTasks.Message}, "", nil
		}
		if errors.Is(err, ErrChainNotFound) {
			return ChainCloseResult{
				Error: "chain_not_found",
				Hint:  "Accepted identifier params: 'slug' (with 'chain' / 'chain_slug' as aliases) or 'id' (with 'chain_id' as an alias).",
			}, "", nil
		}
		return ChainCloseResult{}, "", err
	}
	resp := ChainCloseResult{OK: true, ChainSlug: slug}
	if summary != nil {
		n := stringCharCount(*summary)
		resp.ClosureSummaryChars = &n
	}
	return resp, eventID, nil
}

// closeChainInTx mirrors closeChain but emits the ChainClosed event via
// the passed-in tx. Reads (chain lookup, non-terminal-tasks count) flow
// through tx-aware tx.QueryRowContext so the same-tx writes from a
// prior batch op (e.g. task_complete on the chain's last task) are
// visible to the count query — that visibility is the property that
// lets batch([task_complete, chain_close]) succeed on the chain's
// final task.
func closeChainInTx(ctx context.Context, tx *sql.Tx, pool *db.Pool, project, slug string, summary *string) (string, error) {
	var chainID int64
	var projectID string
	var row *sql.Row
	if project != "" {
		row = tx.QueryRowContext(ctx,
			`SELECT id, project_id FROM proj_chain_status WHERE project_id = ? AND slug = ?`, project, slug)
	} else {
		row = tx.QueryRowContext(ctx,
			`SELECT id, project_id FROM proj_chain_status WHERE slug = ?`, slug)
	}
	if err := row.Scan(&chainID, &projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrChainNotFound, slug)
		}
		return "", fmt.Errorf("close_chain lookup: %w", err)
	}

	var nonTerminal int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proj_current_tasks WHERE chain_id = ? AND status NOT IN ('closed', 'cancelled')`,
		chainID).Scan(&nonTerminal); err != nil {
		return "", fmt.Errorf("close_chain count: %w", err)
	}
	if nonTerminal > 0 {
		return "", &ErrChainHasOpenTasks{
			Slug:    slug,
			Count:   nonTerminal,
			Message: fmt.Sprintf("chain '%s' has %d non-terminal tasks", slug, nonTerminal),
		}
	}

	eventID, err := events.Emit(ctx, tx, events.EmitArgs{
		Entity:  events.NewEntityRef("chain", slug, projectID),
		Payload: events.ChainClosedPayload{ClosureSummary: summary},
	})
	if err != nil {
		return "", err
	}
	_ = pool // reserved for future read-through-tx migration of the pre-flight lookups
	return eventID, nil
}

func HandleChainClose(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (ChainCloseResult, error) {
	var p chainCloseParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ChainCloseResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	slug := firstNonEmpty(p.Slug, p.Chain, p.ChainSlug)
	if slug == "" {
		if id := p.resolvedID(); id > 0 {
			resolved, err := lookupChainSlugByID(ctx, pool, id)
			if err != nil {
				return ChainCloseResult{Error: err.Error()}, nil
			}
			slug = resolved
		}
	}
	if slug == "" {
		return ChainCloseResult{
			Error: IdentifierRequiredError("chain_close"),
			Hint:  "Pass {\"slug\": \"<chain-slug>\"} or {\"id\": <chain-id>}; 'chain' and 'chain_slug' are also accepted as slug aliases.",
		}, nil
	}
	var summary *string
	if p.SummarySupplied {
		s := p.Summary
		summary = &s
	}
	if err := closeChain(ctx, pool, project, slug, summary); err != nil {
		var openTasks *ErrChainHasOpenTasks
		if errors.As(err, &openTasks) {
			return ChainCloseResult{Error: openTasks.Message}, nil
		}
		if errors.Is(err, ErrChainNotFound) {
			return ChainCloseResult{
				Error: "chain_not_found",
				Hint:  "Accepted identifier params: 'slug' (with 'chain' / 'chain_slug' as aliases) or 'id' (with 'chain_id' as an alias).",
			}, nil
		}
		return ChainCloseResult{}, err
	}
	resp := ChainCloseResult{OK: true, ChainSlug: slug}
	if summary != nil {
		n := stringCharCount(*summary)
		resp.ClosureSummaryChars = &n
	}
	return resp, nil
}

func missingChainSlugEnvelope() *ErrorEnvelope {
	return &ErrorEnvelope{
		Error: "params.slug is required",
		Hint:  "Pass {\"slug\": \"<chain-slug>\"} or {\"id\": <chain-pk>}; 'chain' / 'chain_slug' alias the slug form and 'chain_id' aliases the id form.",
	}
}

func stringCharCount(s string) int {
	return len([]rune(s))
}

// ── Action-doc descriptors (parallel-run registry, action_doc.go) ───────
// Co-located with their handlers per the action-doc contract: the descriptor
// carries the authored semantics (purpose, param name/order/required/desc/
// alias-of, notes, …); param TYPE derives from the bound struct (action_doc.go
// deriveSpec). See docs/ACTION_DOC_CONTRACT.md.

var chainStatusDoc = ActionDoc{
	// slug is OPTIONAL: HandleChainStatus returns the full open-chain list when
	// slug is empty (TestChainStatus_NoSlugListsOpenChains), and one chain's
	// summary when a slug is given. The purpose leads with the list mode so a
	// weak/cheap model picks chain_status (not chain_find) for "list the open
	// chains"; the prior doc marked slug Required and omitted the list mode,
	// which contradicted the handler and mis-steered cheap-model tool choice.
	Purpose: "List all open chains with their task counts (no slug), or one chain's summary by slug. Open chains only — a closed or unknown slug returns chain_not_found. Use chain_state for full per-chain detail, or chain_find to search by name pattern.",
	Params: []DocParam{
		{Name: "slug", Required: false, Description: "Chain slug. Omit to list ALL open chains; pass a slug to return just that chain's summary (the chain must be open — a closed or unknown slug returns chain_not_found)."},
		{Name: "chain", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "chain_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
	},
	Example: `{"slug":"agent-first-substrate"}`,
}

var chainStateDoc = ActionDoc{
	Purpose: "Return a chain's full state including all tasks (verbose). Identify by `slug` (or its aliases `chain` / `chain_slug`) OR by the numeric `id` (or `chain_id`) — the id form lets a chain_find → chain_state pivot skip the slug round-trip.",
	Params: []DocParam{
		{Name: "slug", Required: false, Description: "Chain slug. Required unless an id form (id / chain_id) is supplied."},
		{Name: "chain", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "chain_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "id", Required: false, Description: "Chain id — the numeric PK chain_find surfaces. Resolves the chain directly; use instead of slug. A slug accidentally passed here (or in chain_id) is tolerated — routed to the slug resolver instead of erroring."},
		{Name: "chain_id", Required: false, Description: "Alias of id (accepts a numeric id or, tolerantly, a slug).", AliasOf: "id"},
	},
	Example: `{"slug":"agent-first-substrate"}`,
}

var chainFindDoc = ActionDoc{
	Purpose: "Search or LIST chains. With a `pattern` (or alias query/text/q/slug) it substring-matches the slug; OMIT the pattern to list chains (optionally project-scoped). Unlike chain_status, closed/cancelled chains are included. Returns compact rows.",
	Params: []DocParam{
		{Name: "pattern", Required: false, Description: "Substring to match against the slug (case-insensitive). Omit to list all chains. Aliases: query, text, q, slug."},
		{Name: "project", Required: false, Description: "Optional project to scope the list/search to. Omit for cross-project (the default). Falls back to the dispatch envelope project when set there."},
	},
	Example: `{"pattern":"substrate"}`,
}

var chainCloseDoc = ActionDoc{
	Purpose: "Close a chain, optionally with a closure summary.",
	Params: []DocParam{
		// id-OR-slug one-of: HandleChainClose resolves slug from id
		// (lookupChainSlugByID) and rejects only when BOTH are empty
		// (IdentifierRequiredError). Neither arm is individually Required.
		{Name: "id", Required: false, Description: "Chain id (preferred — globally unique)."},
		{Name: "chain_id", Required: false, Description: "Alias of id.", AliasOf: "id"},
		{Name: "slug", Required: false, Description: "Chain slug."},
		{Name: "chain", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "chain_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		// closure_summary has no backing struct field (chainCloseParams binds it
		// via custom UnmarshalJSON), so its Type is AUTHORED, not derived.
		{Name: "closure_summary", Required: false, Description: "Free-form closing note for the chain.", Type: "string"},
	},
	Example:              `{"slug":"agent-first-substrate","closure_summary":"All 8 tasks closed; retrospective at docs/..."}`,
	EnvelopeRequirements: rationaleEnv(),
}
