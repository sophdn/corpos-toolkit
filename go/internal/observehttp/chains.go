package observehttp

import (
	"database/sql"
	"errors"
	"net/http"

	"toolkit/internal/db"
	"toolkit/internal/dbutil"
)

// ChainRow mirrors observe_http::handlers::chains::ChainRow. Counts are
// computed by correlated subqueries against the tasks table so the list
// endpoint is one round-trip.
type ChainRow struct {
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
	UpdatedAt  string `json:"updated_at"`
}

// Read path repointed to proj_chain_status (T4 of agent-first-substrate
// chain). Per-row task counts are folded at Chain*/Task* event emit
// time so the dashboard skips the five correlated subqueries per row
// the legacy chainListSelect carried.
const chainListSelect = `
SELECT c.id, c.project_id, c.slug, c.status,
    c.total_tasks, c.pending, c.active, c.blocked, c.closed, c.cancelled,
    c.updated_at
FROM proj_chain_status c
`

func (s AppState) chainsList(w http.ResponseWriter, r *http.Request) {
	includeClosed := boolParam(r, "include_closed", false)
	project := projectFilter(r)

	sqlStr := chainListSelect + " WHERE 1=1"
	binds := db.NewArgs()
	if !includeClosed {
		sqlStr += " AND c.status NOT IN ('closed', 'cancelled')"
	}
	if project != "" {
		sqlStr += " AND c.project_id = ?"
		binds.AddString(project)
	}
	sqlStr += " ORDER BY c.updated_at DESC"

	rows, err := s.Pool.DB().QueryContext(r.Context(), sqlStr, binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	out := []ChainRow{}
	for rows.Next() {
		var c ChainRow
		if err := rows.Scan(
			&c.ID, &c.ProjectID, &c.Slug, &c.Status,
			&c.TotalTasks, &c.Pending, &c.Active, &c.Blocked, &c.Closed, &c.Cancelled,
			&c.UpdatedAt,
		); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// chainsCounts is the aggregate counterpart of chainsList. Unlike the
// list endpoint, chainsCounts honors a `status` filter directly (no
// `include_closed` flag) so the dashboard's counts module can ask for
// any specific status bucket without two round-trips.
func (s AppState) chainsCounts(w http.ResponseWriter, r *http.Request) {
	wb := dbutil.NewWhereBuilder().
		Eq("project_id", projectFilter(r)).
		Eq("status", r.URL.Query().Get("status"))
	s.runCounts(w, r,
		"FROM proj_chain_status",
		wb,
		r.URL.Query().Get("group_by"),
		map[string]string{
			"status":     "status",
			"project_id": "project_id",
		})
}

// ChainDetail mirrors observe_http::handlers::chains::detail — returns
// the prose fields the list endpoint omits. design_decisions retired
// from this projection-side cache in migration 065 (Phase 4 F2); the
// EventTimeline reads it from ChainCreated/ChainEdited event payloads.
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

func (s AppState) chainsDetail(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	project := projectFilter(r)

	// Chain detail reads the projection cache; output / completion_condition
	// / closure_summary are denormalised onto proj_chain_status by every
	// Chain* event emit (see T4 fold). design_decisions retired in
	// migration 065 — it now flows only through the events ledger.
	var (
		row ChainDetail
		err error
	)
	if project != "" {
		err = s.Pool.DB().QueryRowContext(r.Context(),
			`SELECT id, project_id, slug, status, output,
                    completion_condition, closure_summary, created_at, updated_at
             FROM proj_chain_status WHERE project_id = ? AND slug = ?`,
			project, slug,
		).Scan(&row.ID, &row.ProjectID, &row.Slug, &row.Status,
			&row.Output, &row.CompletionCondition,
			&row.ClosureSummary, &row.CreatedAt, &row.UpdatedAt)
	} else {
		err = s.Pool.DB().QueryRowContext(r.Context(),
			`SELECT id, project_id, slug, status, output,
                    completion_condition, closure_summary, created_at, updated_at
             FROM proj_chain_status WHERE slug = ?`,
			slug,
		).Scan(&row.ID, &row.ProjectID, &row.Slug, &row.Status,
			&row.Output, &row.CompletionCondition,
			&row.ClosureSummary, &row.CreatedAt, &row.UpdatedAt)
	}

	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "chain '" + slug + "' not found",
		})
		return
	}
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}
