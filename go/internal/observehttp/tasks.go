package observehttp

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/dbutil"
)

// tasksCounts surfaces cross-chain task counts for the dashboard. The
// list endpoint above is unbounded, so client-side counting works in
// principle, but the single-source-of-truth counts module on the
// frontend calls this for consistency with bugs/suggestions/chains.
//
// Filters mirror tasksList (chain_slug / chain_status / status /
// project). group_by accepts status / chain_status / chain_slug /
// project_id.
func (s AppState) tasksCounts(w http.ResponseWriter, r *http.Request) {
	wb := dbutil.NewWhereBuilder().
		Eq("c.slug", r.URL.Query().Get("chain_slug")).
		Eq("c.project_id", projectFilter(r)).
		Eq("c.status", r.URL.Query().Get("chain_status")).
		Eq("t.status", r.URL.Query().Get("status"))
	s.runCounts(w, r,
		"FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id",
		wb,
		r.URL.Query().Get("group_by"),
		map[string]string{
			"status":       "t.status",
			"chain_status": "c.status",
			"chain_slug":   "c.slug",
			"project_id":   "c.project_id",
		})
}

type TaskRow struct {
	ID               int64  `json:"id"`
	ChainID          int64  `json:"chain_id"`
	ChainSlug        string `json:"chain_slug"`
	ProjectID        string `json:"project_id"`
	Slug             string `json:"slug"`
	Position         int64  `json:"position"`
	Status           string `json:"status"`
	ProblemStatement string `json:"problem_statement"`
	UpdatedAt        string `json:"updated_at"`
}

func (s AppState) tasksList(w http.ResponseWriter, r *http.Request) {
	chainSlug := r.URL.Query().Get("chain_slug")
	project := projectFilter(r)
	status := r.URL.Query().Get("status")

	wb := dbutil.NewWhereBuilder().
		Eq("c.slug", chainSlug).
		Eq("c.project_id", project).
		Eq("t.status", status)
	query := fmt.Sprintf(`
SELECT t.id, t.chain_id, c.slug AS chain_slug, c.project_id,
       t.slug, t.position, t.status, t.problem_statement, t.updated_at
FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
%s ORDER BY c.slug, t.position`, wb.Clause())

	rows, err := s.Pool.DB().QueryContext(r.Context(), query, wb.Args().Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	out := []TaskRow{}
	for rows.Next() {
		var t TaskRow
		if err := rows.Scan(
			&t.ID, &t.ChainID, &t.ChainSlug, &t.ProjectID,
			&t.Slug, &t.Position, &t.Status, &t.ProblemStatement, &t.UpdatedAt,
		); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// TaskContentMatch mirrors work_lib::tasks::TaskContentMatch — one row
// per (task, field) pair that matched the search pattern, with a
// ~200-char snippet centred on the first occurrence.
type TaskContentMatch struct {
	ChainSlug   string `json:"chain_slug"`
	ChainStatus string `json:"chain_status"`
	TaskSlug    string `json:"task_slug"`
	TaskStatus  string `json:"task_status"`
	Field       string `json:"field"`
	Snippet     string `json:"snippet"`
}

type SearchResponse struct {
	Count     int                `json:"count"`
	Truncated bool               `json:"truncated"`
	Pattern   string             `json:"pattern"`
	Matches   []TaskContentMatch `json:"matches"`
}

func (s AppState) tasksSearch(w http.ResponseWriter, r *http.Request) {
	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "params.pattern required",
		})
		return
	}
	chainSlug := r.URL.Query().Get("chain_slug")
	chainStatus := r.URL.Query().Get("chain_status")
	project := projectFilter(r)
	maxResults := int64(50)
	if v := r.URL.Query().Get("max_results"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			maxResults = parsed
		}
	}
	if maxResults < 1 {
		maxResults = 1
	}
	if maxResults > 200 {
		maxResults = 200
	}

	matches, err := searchTaskSnippets(r, s, pattern, chainSlug, chainStatus, project, maxResults)
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, SearchResponse{
		Count:     len(matches),
		Truncated: int64(len(matches)) >= maxResults,
		Pattern:   pattern,
		Matches:   matches,
	})
}

func searchTaskSnippets(
	r *http.Request,
	s AppState,
	pattern, chainSlug, chainStatus, project string,
	maxResults int64,
) ([]TaskContentMatch, error) {
	like := "%" + pattern + "%"
	var b strings.Builder
	b.WriteString(`
SELECT c.slug AS chain_slug, c.status AS chain_status,
       t.slug AS task_slug, t.status AS task_status,
       t.problem_statement, t.acceptance_criteria, t.context_required,
       t.constraints, t.handoff_output
FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
WHERE (
    t.problem_statement LIKE ? OR t.acceptance_criteria LIKE ?
    OR t.context_required LIKE ? OR t.constraints LIKE ?
    OR t.handoff_output LIKE ? OR t.slug LIKE ?
)`)
	binds := db.NewArgs().
		AddString(like).AddString(like).AddString(like).
		AddString(like).AddString(like).AddString(like)
	if chainSlug != "" {
		b.WriteString(" AND c.slug = ?")
		binds.AddString(chainSlug)
	}
	if chainStatus != "" {
		b.WriteString(" AND c.status = ?")
		binds.AddString(chainStatus)
	}
	if project != "" {
		b.WriteString(" AND c.project_id = ?")
		binds.AddString(project)
	}
	b.WriteString(" ORDER BY c.slug, t.position")

	rows, err := s.Pool.DB().QueryContext(r.Context(), b.String(), binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	matches := []TaskContentMatch{}
	for rows.Next() {
		var (
			row struct {
				ChainSlug, ChainStatus, TaskSlug, TaskStatus  string
				ProblemStatement, AcceptanceCriteria, Context string
				Constraints, HandoffOutput                    string
			}
		)
		if err := rows.Scan(
			&row.ChainSlug, &row.ChainStatus, &row.TaskSlug, &row.TaskStatus,
			&row.ProblemStatement, &row.AcceptanceCriteria, &row.Context,
			&row.Constraints, &row.HandoffOutput,
		); err != nil {
			return nil, err
		}
		fields := []struct {
			name, body string
		}{
			{"problem_statement", row.ProblemStatement},
			{"acceptance_criteria", row.AcceptanceCriteria},
			{"context_required", row.Context},
			{"constraints", row.Constraints},
			{"handoff_output", row.HandoffOutput},
		}
		for _, f := range fields {
			if snip, ok := extractSnippet(f.body, pattern); ok {
				matches = append(matches, TaskContentMatch{
					ChainSlug:   row.ChainSlug,
					ChainStatus: row.ChainStatus,
					TaskSlug:    row.TaskSlug,
					TaskStatus:  row.TaskStatus,
					Field:       f.name,
					Snippet:     snip,
				})
				if int64(len(matches)) >= maxResults {
					return matches, nil
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}

// extractSnippet mirrors work_lib::tasks::extract_snippet: find the
// first case-insensitive occurrence of pattern in body and return a
// ~200-rune window centred on it, with leading/trailing ellipses when
// truncated. ok=false when pattern is absent.
func extractSnippet(body, pattern string) (string, bool) {
	if body == "" || pattern == "" {
		return "", false
	}
	idx := strings.Index(strings.ToLower(body), strings.ToLower(pattern))
	if idx < 0 {
		return "", false
	}
	// Rune-aware window: count runes up to idx (byte index from
	// strings.Index), then take 100 runes either side of the match.
	bodyRunes := []rune(body)
	patRunes := []rune(pattern)
	charIdx := len([]rune(body[:idx]))
	const half = 100
	start := charIdx - half
	if start < 0 {
		start = 0
	}
	end := charIdx + len(patRunes) + half
	if end > len(bodyRunes) {
		end = len(bodyRunes)
	}
	var sb strings.Builder
	if start > 0 {
		sb.WriteRune('…')
	}
	sb.WriteString(string(bodyRunes[start:end]))
	if end < len(bodyRunes) {
		sb.WriteRune('…')
	}
	return sb.String(), true
}
