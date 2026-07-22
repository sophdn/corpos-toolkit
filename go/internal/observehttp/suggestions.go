package observehttp

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/dbutil"
)

// SuggestionRow mirrors the bugRow shape but reads native suggestion
// vocabulary (priority, routed_bug_slug) and skips qwen_task_id —
// suggestions never run through the Qwen filing-decision telemetry
// surface that field captures for bugs. resolved_at is nullable so the
// encoder emits explicit null for unresolved suggestions, matching the
// bugRow convention.
//
// Per chain `agent-suggestion-box` design, this handler queries the
// canonical suggestions table directly rather than going through a
// projection view; the audit-ledger integration that bugs accumulated
// via proj_current_bugs lands later if/when suggestions also need that
// shape. For now the suggestions read surface is event-driven via the
// SuggestionReported / SuggestionResolved / SuggestionReopened /
// SuggestionStamped payloads and the canonical CRUD table.
type SuggestionRow struct {
	ID                int64   `json:"id"`
	ProjectID         string  `json:"project_id"`
	Slug              string  `json:"slug"`
	Title             string  `json:"title"`
	Surface           string  `json:"surface"`
	Priority          string  `json:"priority"`
	Status            string  `json:"status"`
	RoutedChainSlug   string  `json:"routed_chain_slug"`
	RoutedTaskSlug    string  `json:"routed_task_slug"`
	RoutedBugSlug     string  `json:"routed_bug_slug"`
	ResolvedCommitSHA *string `json:"resolved_commit_sha" tstype:"string | null,required"`
	FiledAt           string  `json:"filed_at"`
	ResolvedAt        *string `json:"resolved_at" tstype:"string | null,required"`
}

// validSuggestionStatuses / validSuggestionPriorities are the
// suggestion-side vocab enforced at the HTTP boundary. Invalid filter
// values reject with 400 rather than silently returning an empty list —
// caller code that mis-spells a kind ("adopted_at" vs "adopted") sees
// the error immediately.
var (
	validSuggestionStatuses   = map[string]struct{}{"open": {}, "adopted": {}, "deferred": {}, "rejected": {}}
	validSuggestionPriorities = map[string]struct{}{"low": {}, "medium": {}, "high": {}}
)

func (s AppState) suggestionsList(w http.ResponseWriter, r *http.Request) {
	project := projectFilter(r)
	status := r.URL.Query().Get("status")
	priority := r.URL.Query().Get("priority")
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

	if status != "" {
		if _, ok := validSuggestionStatuses[status]; !ok {
			http.Error(w, fmt.Sprintf("invalid status %q (allowed: open, adopted, deferred, rejected)", status), http.StatusBadRequest)
			return
		}
	}
	if priority != "" {
		if _, ok := validSuggestionPriorities[priority]; !ok {
			http.Error(w, fmt.Sprintf("invalid priority %q (allowed: low, medium, high)", priority), http.StatusBadRequest)
			return
		}
	}

	var b strings.Builder
	b.WriteString(`
SELECT id, project_id, slug, title, surface, priority, status,
       routed_chain_slug, routed_task_slug, routed_bug_slug,
       resolved_commit_sha, filed_at, resolved_at
FROM proj_current_suggestions WHERE 1=1`)
	binds := db.NewArgs()
	if project != "" {
		b.WriteString(" AND project_id = ?")
		binds.AddString(project)
	}
	if status != "" {
		b.WriteString(" AND status = ?")
		binds.AddString(status)
	}
	if priority != "" {
		b.WriteString(" AND priority = ?")
		binds.AddString(priority)
	}
	if surface != "" {
		b.WriteString(" AND surface LIKE ?")
		binds.AddString("%" + surface + "%")
	}
	b.WriteString(fmt.Sprintf(" ORDER BY filed_at DESC LIMIT %d", limit))

	rows, err := s.Pool.DB().QueryContext(r.Context(), b.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	out := []SuggestionRow{}
	for rows.Next() {
		var sg SuggestionRow
		if err := rows.Scan(
			&sg.ID, &sg.ProjectID, &sg.Slug, &sg.Title, &sg.Surface,
			&sg.Priority, &sg.Status,
			&sg.RoutedChainSlug, &sg.RoutedTaskSlug, &sg.RoutedBugSlug,
			&sg.ResolvedCommitSHA, &sg.FiledAt, &sg.ResolvedAt,
		); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, sg)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// suggestionsCounts is the aggregate counterpart of suggestionsList —
// see bugsCounts for the architectural note about list-endpoint caps
// silently undercapping client-side counts.
func (s AppState) suggestionsCounts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	priority := r.URL.Query().Get("priority")
	surface := r.URL.Query().Get("surface")

	if status != "" {
		if _, ok := validSuggestionStatuses[status]; !ok {
			http.Error(w, fmt.Sprintf("invalid status %q (allowed: open, adopted, deferred, rejected)", status), http.StatusBadRequest)
			return
		}
	}
	if priority != "" {
		if _, ok := validSuggestionPriorities[priority]; !ok {
			http.Error(w, fmt.Sprintf("invalid priority %q (allowed: low, medium, high)", priority), http.StatusBadRequest)
			return
		}
	}

	likePattern := ""
	if surface != "" {
		likePattern = "%" + surface + "%"
	}
	wb := dbutil.NewWhereBuilder().
		Eq("project_id", projectFilter(r)).
		Eq("status", status).
		Eq("priority", priority).
		Like("surface", likePattern)
	s.runCounts(w, r,
		"FROM proj_current_suggestions",
		wb,
		r.URL.Query().Get("group_by"),
		map[string]string{
			"status":     "status",
			"priority":   "priority",
			"surface":    "surface",
			"project_id": "project_id",
		})
}
