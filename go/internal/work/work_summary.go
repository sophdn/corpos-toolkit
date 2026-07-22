// Package work's work_summary.go adds the one-call open-work portfolio
// rollup. Where chain_status / bug_list / task_list / suggestion_list each
// answer one slice of "what's open", work_summary collapses the four counts
// into a single read so a resume briefing doesn't need four round-trips.
// Read-only; respects the optional project filter and can break the totals
// down per-project.
package work

import (
	"context"
	"encoding/json"
	"sort"

	"toolkit/internal/db"
)

// workSummaryParams captures work.work_summary. Both fields optional:
// empty project → cross-project totals; by_project → per-project breakdown
// alongside the top-level totals.
type workSummaryParams struct {
	Project   string `json:"project"`
	ByProject bool   `json:"by_project"`
}

// WorkSummaryTasks holds the non-terminal task counts (tasks whose parent
// chain is itself non-terminal), split by task status.
type WorkSummaryTasks struct {
	Pending int64 `json:"pending"`
	Active  int64 `json:"active"`
	Blocked int64 `json:"blocked"`
}

// WorkSummaryResult is the open-work rollup. The top-level value carries the
// portfolio totals (Project empty); when by_project is set, Projects holds
// one WorkSummaryResult per project (each with Project populated, Projects
// itself nil).
type WorkSummaryResult struct {
	Project         string              `json:"project,omitempty"`
	OpenBugs        int64               `json:"open_bugs"`
	OpenChains      int64               `json:"open_chains"`
	Tasks           WorkSummaryTasks    `json:"tasks"`
	OpenSuggestions int64               `json:"open_suggestions"`
	Projects        []WorkSummaryResult `json:"projects,omitempty"`
}

// HandleWorkSummary implements work.work_summary. Cross-project when project
// is empty (mirrors bug_list / task_list / chain_status); scoped when set.
// When by_project=true, the same counts are additionally grouped per project.
func HandleWorkSummary(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (WorkSummaryResult, error) {
	var p workSummaryParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return WorkSummaryResult{}, err
		}
	}
	// params.project (bug-1070 nested) wins over the envelope only when set;
	// otherwise fall back to the envelope project.
	scope := firstNonEmpty(p.Project, project)

	total, err := workSummaryTotals(ctx, pool, scope)
	if err != nil {
		return WorkSummaryResult{}, err
	}

	if p.ByProject {
		rows, err := workSummaryByProject(ctx, pool, scope)
		if err != nil {
			return WorkSummaryResult{}, err
		}
		total.Projects = rows
	}
	return total, nil
}

// workSummaryTotals computes the four open-work counts for one scope (empty
// scope → cross-project). Project is left empty on the returned value.
func workSummaryTotals(ctx context.Context, pool *db.Pool, scope string) (WorkSummaryResult, error) {
	var out WorkSummaryResult
	dbh := pool.DB()

	openBugsQ := `SELECT COUNT(*) FROM proj_current_bugs WHERE status = 'open'`
	openChainsQ := `SELECT COUNT(*) FROM proj_chain_status WHERE status NOT IN ('closed', 'cancelled')`
	openSuggQ := `SELECT COUNT(*) FROM proj_current_suggestions WHERE status = 'open'`
	tasksQ := `SELECT t.status, COUNT(*)
		FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
		WHERE c.status NOT IN ('closed', 'cancelled')
		  AND t.status IN ('pending', 'active', 'blocked')`

	bugArgs, chainArgs, suggArgs, taskArgs := db.NewArgs(), db.NewArgs(), db.NewArgs(), db.NewArgs()
	if scope != "" {
		openBugsQ += ` AND project_id = ?`
		openChainsQ += ` AND project_id = ?`
		openSuggQ += ` AND project_id = ?`
		tasksQ += ` AND c.project_id = ?`
		bugArgs.AddString(scope)
		chainArgs.AddString(scope)
		suggArgs.AddString(scope)
		taskArgs.AddString(scope)
	}
	tasksQ += ` GROUP BY t.status`

	if err := dbh.QueryRowContext(ctx, openBugsQ, bugArgs.Slice()...).Scan(&out.OpenBugs); err != nil {
		return WorkSummaryResult{}, err
	}
	if err := dbh.QueryRowContext(ctx, openChainsQ, chainArgs.Slice()...).Scan(&out.OpenChains); err != nil {
		return WorkSummaryResult{}, err
	}
	if err := dbh.QueryRowContext(ctx, openSuggQ, suggArgs.Slice()...).Scan(&out.OpenSuggestions); err != nil {
		return WorkSummaryResult{}, err
	}

	rows, err := dbh.QueryContext(ctx, tasksQ, taskArgs.Slice()...)
	if err != nil {
		return WorkSummaryResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return WorkSummaryResult{}, err
		}
		out.Tasks = applyTaskCount(out.Tasks, status, n)
	}
	if err := rows.Err(); err != nil {
		return WorkSummaryResult{}, err
	}
	return out, nil
}

// workSummaryByProject computes the per-project breakdown. When scope is
// set, the breakdown contains just that project; otherwise every project
// with open work. Rows are sorted by project id for a deterministic shape.
func workSummaryByProject(ctx context.Context, pool *db.Pool, scope string) ([]WorkSummaryResult, error) {
	byProj := map[string]*WorkSummaryResult{}
	get := func(pid string) *WorkSummaryResult {
		r, ok := byProj[pid]
		if !ok {
			r = &WorkSummaryResult{Project: pid}
			byProj[pid] = r
		}
		return r
	}

	scopeArgs := db.NewArgs()
	if scope != "" {
		scopeArgs.AddString(scope)
	}

	// open bugs per project
	q := `SELECT project_id, COUNT(*) FROM proj_current_bugs WHERE status = 'open'`
	if scope != "" {
		q += ` AND project_id = ?`
	}
	q += ` GROUP BY project_id`
	if err := scanCountByProject(ctx, pool, q, scopeArgs, func(pid string, n int64) {
		get(pid).OpenBugs = n
	}); err != nil {
		return nil, err
	}

	// open chains per project
	q = `SELECT project_id, COUNT(*) FROM proj_chain_status WHERE status NOT IN ('closed', 'cancelled')`
	if scope != "" {
		q += ` AND project_id = ?`
	}
	q += ` GROUP BY project_id`
	if err := scanCountByProject(ctx, pool, q, scopeArgs, func(pid string, n int64) {
		get(pid).OpenChains = n
	}); err != nil {
		return nil, err
	}

	// open suggestions per project
	q = `SELECT project_id, COUNT(*) FROM proj_current_suggestions WHERE status = 'open'`
	if scope != "" {
		q += ` AND project_id = ?`
	}
	q += ` GROUP BY project_id`
	if err := scanCountByProject(ctx, pool, q, scopeArgs, func(pid string, n int64) {
		get(pid).OpenSuggestions = n
	}); err != nil {
		return nil, err
	}

	// non-terminal tasks per project, split by task status
	q = `SELECT c.project_id, t.status, COUNT(*)
		FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
		WHERE c.status NOT IN ('closed', 'cancelled')
		  AND t.status IN ('pending', 'active', 'blocked')`
	if scope != "" {
		q += ` AND c.project_id = ?`
	}
	q += ` GROUP BY c.project_id, t.status`
	rows, err := pool.DB().QueryContext(ctx, q, scopeArgs.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid, status string
		var n int64
		if err := rows.Scan(&pid, &status, &n); err != nil {
			return nil, err
		}
		r := get(pid)
		r.Tasks = applyTaskCount(r.Tasks, status, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]WorkSummaryResult, 0, len(byProj))
	for _, r := range byProj {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

// scanCountByProject runs a (project_id, count) grouped query and invokes
// set for each row.
func scanCountByProject(ctx context.Context, pool *db.Pool, query string, args *db.Args, set func(pid string, n int64)) error {
	rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var n int64
		if err := rows.Scan(&pid, &n); err != nil {
			return err
		}
		set(pid, n)
	}
	return rows.Err()
}

// applyTaskCount routes a (task-status, count) pair onto the right field of
// a WorkSummaryTasks. Unknown statuses are ignored (the query only asks for
// pending/active/blocked).
func applyTaskCount(t WorkSummaryTasks, status string, n int64) WorkSummaryTasks {
	switch status {
	case "pending":
		t.Pending = n
	case "active":
		t.Active = n
	case "blocked":
		t.Blocked = n
	}
	return t
}

var workSummaryDoc = ActionDoc{
	Purpose: "One-call open-work portfolio rollup: open_bugs, open_chains, non-terminal task counts (pending/active/blocked), and open_suggestions. Cross-project by default; pass `project` to scope, or `by_project:true` for a per-project breakdown alongside the totals. Read-only — collapses the four separate list verbs into a single resume-briefing read.",
	Params: []DocParam{
		{Name: "project", Required: false, Description: "Optional project to scope the rollup to. Omit for cross-project totals."},
		{Name: "by_project", Required: false, Description: "When true, adds a `projects` array with the same counts grouped per project, alongside the top-level totals."},
	},
	Example: `{"by_project":true}`,
}
