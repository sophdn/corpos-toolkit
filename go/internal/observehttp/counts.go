package observehttp

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"toolkit/internal/dbutil"
)

// countResponse is the shape returned by /{resource}/counts. When
// group_by is empty, only Total is populated (Buckets is nil and
// omitted from JSON). When group_by is set, Buckets is keyed by the
// group value and Total is the sum.
//
// This is the single source of truth the dashboard's counts module
// relies on for label / breakdown widgets — replacing the prior
// pattern of summing the (200-row-capped) list endpoint client-side,
// which silently undercounted any corpus larger than the cap.
type countResponse struct {
	Total   int64            `json:"total"`
	GroupBy string           `json:"group_by,omitempty"`
	Buckets map[string]int64 `json:"buckets,omitempty"`
}

// runCounts executes a COUNT query (optionally grouped) against the
// caller-supplied fromClause + WhereBuilder, validating group_by
// against the allowed-fields map.
//
// fromClause is the "FROM ..." tail (no WHERE), e.g.
// "FROM proj_current_bugs" or
// "FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id".
//
// allowedGroupBy maps the public query-string key (what callers send
// in ?group_by=X) to the SQL column expression embedded into the
// SELECT and GROUP BY (allows aliasing like
// "chain" → "c.slug AS chain"). Unrecognised keys return 400.
func (s AppState) runCounts(
	w http.ResponseWriter, r *http.Request,
	fromClause string, wb *dbutil.WhereBuilder,
	groupByKey string, allowedGroupBy map[string]string,
) {
	whereTail := ""
	if c := wb.Clause(); c != "" {
		whereTail = " " + c
	}

	if groupByKey == "" {
		sqlStr := fmt.Sprintf("SELECT COUNT(*) %s%s", fromClause, whereTail)
		var n int64
		if err := s.Pool.DB().QueryRowContext(r.Context(), sqlStr, wb.Args().Slice()...).Scan(&n); err != nil {
			dbErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, countResponse{Total: n})
		return
	}

	col, ok := allowedGroupBy[groupByKey]
	if !ok {
		allowed := make([]string, 0, len(allowedGroupBy))
		for k := range allowedGroupBy {
			allowed = append(allowed, k)
		}
		sort.Strings(allowed)
		http.Error(w,
			fmt.Sprintf("invalid group_by %q (allowed: %s)", groupByKey, strings.Join(allowed, ", ")),
			http.StatusBadRequest)
		return
	}

	sqlStr := fmt.Sprintf("SELECT %s, COUNT(*) %s%s GROUP BY %s",
		col, fromClause, whereTail, col)
	rows, err := s.Pool.DB().QueryContext(r.Context(), sqlStr, wb.Args().Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	buckets := map[string]int64{}
	var total int64
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			dbErr(w, err)
			return
		}
		buckets[key] = count
		total += count
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, countResponse{Total: total, GroupBy: groupByKey, Buckets: buckets})
}
