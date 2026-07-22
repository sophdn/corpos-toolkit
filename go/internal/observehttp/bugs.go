package observehttp

import (
	"fmt"
	"net/http"
	"strconv"

	"toolkit/internal/dbutil"
)

// BugRow mirrors observe_http::handlers::bugs::BugRow. resolved_at and
// qwen_task_id are nullable in the schema; we use *string so the encoder
// emits explicit `null` (matching Rust's Option<String> serialization)
// instead of dropping the keys.
//
// The `tstype:"string | null,required"` override on the *string fields
// pins the TS-side shape to `string | null` (not `string | undefined`);
// without omitempty, Go marshals a nil pointer to JSON null, and the
// TS consumer reads `null`, not a missing key. Same shape as
// ModelMetrics' *float64 score fields, established in chain T8.
type BugRow struct {
	ID                   int64   `json:"id"`
	ProjectID            string  `json:"project_id"`
	Slug                 string  `json:"slug"`
	Title                string  `json:"title"`
	Surface              string  `json:"surface"`
	Severity             string  `json:"severity"`
	Status               string  `json:"status"`
	RoutedSuggestionSlug string  `json:"routed_suggestion_slug"`
	FiledAt              string  `json:"filed_at"`
	ResolvedAt           *string `json:"resolved_at" tstype:"string | null,required"`
	QwenTaskID           *string `json:"qwen_task_id" tstype:"string | null,required"`
}

func (s AppState) bugsList(w http.ResponseWriter, r *http.Request) {
	project := projectFilter(r)
	status := r.URL.Query().Get("status")
	severity := r.URL.Query().Get("severity")
	surface := r.URL.Query().Get("surface")
	limit := int64(200)
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}

	// Read path repointed to proj_current_bugs (T4 of agent-first-substrate
	// chain). The projection is refreshed inside every Bug* event emit's
	// tx; the dashboard sees materialised state without re-running joins
	// per request. CRUD bugs table remains as the write target.
	wb := dbutil.NewWhereBuilder().
		Eq("project_id", project).
		Eq("status", status).
		Eq("severity", severity).
		Eq("surface", surface)
	// LIMIT is interpolated directly because the value is clamped above;
	// keeping it out of the bind list matches the Rust handler's query
	// plan and avoids the parameter-count drift in sqlite.
	query := fmt.Sprintf(`
SELECT id, project_id, slug, title, surface, severity, status,
       routed_suggestion_slug, filed_at, resolved_at, qwen_task_id
FROM proj_current_bugs %s ORDER BY filed_at DESC LIMIT %d`, wb.Clause(), limit)

	rows, err := s.Pool.DB().QueryContext(r.Context(), query, wb.Args().Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	out := []BugRow{}
	for rows.Next() {
		var bg BugRow
		if err := rows.Scan(
			&bg.ID, &bg.ProjectID, &bg.Slug, &bg.Title, &bg.Surface,
			&bg.Severity, &bg.Status, &bg.RoutedSuggestionSlug,
			&bg.FiledAt, &bg.ResolvedAt, &bg.QwenTaskID,
		); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, bg)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// bugsCounts surfaces true bug counts for the dashboard — the list
// endpoint above is capped at 1000 rows for payload-size reasons, so
// any client-side `rows.length`-style count silently undercaps for
// a corpus larger than that. /bugs/counts runs an aggregate query
// against proj_current_bugs and returns the true total or a grouped
// breakdown (status / severity / surface / project_id).
func (s AppState) bugsCounts(w http.ResponseWriter, r *http.Request) {
	wb := dbutil.NewWhereBuilder().
		Eq("project_id", projectFilter(r)).
		Eq("status", r.URL.Query().Get("status")).
		Eq("severity", r.URL.Query().Get("severity")).
		Eq("surface", r.URL.Query().Get("surface"))
	s.runCounts(w, r,
		"FROM proj_current_bugs",
		wb,
		r.URL.Query().Get("group_by"),
		map[string]string{
			"status":     "status",
			"severity":   "severity",
			"surface":    "surface",
			"project_id": "project_id",
		})
}
