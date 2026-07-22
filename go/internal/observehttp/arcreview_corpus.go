package observehttp

import (
	"context"
	"net/http"
)

// ArcCorpusBucket is one labeled histogram cell for a snapshot-corpus
// distribution chart (message-count or estimated-token ranges). Label is
// the human-facing range; Count is the row tally in that range. Buckets
// are emitted in a fixed order and zero-filled so the chart geometry is
// stable across corpus growth.
type ArcCorpusBucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// ArcCorpusStatsResponse is the arc-close snapshot-corpus telemetry payload
// (GET /telemetry/snapshot-corpus/stats). It summarizes
// arcreview_snapshot_corpus — the ML training substrate captured by the
// arc-close review path — so corpus readiness is legible at a glance
// instead of only via raw SQL.
//
// BySource is zero-filled to {live, recovered} so the source-split chart
// renders both cells even before live capture accrues. TupleCompleteRows
// counts rows whose event_id joins an ArcCloseFilingReviewed event carrying
// BOTH a non-empty decisions array and a non-empty arc_summary — the rows
// that assemble a full (snapshot, labels) training tuple. DistinctSessions
// is surfaced deliberately: the corpus is fire-rich but session-poor, and
// ML exporters MUST hold out by session (a fire-level split leaks), so the
// session count is the load-bearing readiness signal, not the row count.
type ArcCorpusStatsResponse struct {
	TotalRows         int               `json:"total_rows"`
	DistinctSessions  int               `json:"distinct_sessions"`
	BySource          map[string]int    `json:"by_source"`
	TruncatedRows     int               `json:"truncated_rows"`
	TupleCompleteRows int               `json:"tuple_complete_rows"`
	MessageCount      []ArcCorpusBucket `json:"message_count_buckets"`
	EstimatedTokens   []ArcCorpusBucket `json:"estimated_tokens_buckets"`
}

// arcCorpusSources is the closed set of source labels the by-source chart
// always renders (zero-filled). Mirrors the CHECK on the source column.
var arcCorpusSources = []string{"live", "recovered"}

// messageCountBucketOrder / messageCountBucketCase pair a fixed display
// order with the SQL CASE that assigns a row to its bucket. The two MUST
// agree on label strings; the test pins them together.
var messageCountBucketOrder = []string{"1-5", "6-10", "11-15", "16-19", "20"}

const messageCountBucketCase = `CASE
		WHEN message_count <= 5  THEN '1-5'
		WHEN message_count <= 10 THEN '6-10'
		WHEN message_count <= 15 THEN '11-15'
		WHEN message_count <= 19 THEN '16-19'
		ELSE '20'
	END`

var estimatedTokensBucketOrder = []string{"<1000", "1000-1999", "2000-2999", "3000-3999", "4000+"}

const estimatedTokensBucketCase = `CASE
		WHEN estimated_tokens < 1000 THEN '<1000'
		WHEN estimated_tokens < 2000 THEN '1000-1999'
		WHEN estimated_tokens < 3000 THEN '2000-2999'
		WHEN estimated_tokens < 4000 THEN '3000-3999'
		ELSE '4000+'
	END`

// arcCorpusStats handles GET /telemetry/snapshot-corpus/stats. Read-only
// aggregate over arcreview_snapshot_corpus (+ a join to events for the
// training-tuple-completeness count). No project filter: the corpus is
// session/event scoped and cross-project by construction.
func (s AppState) arcCorpusStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	resp := ArcCorpusStatsResponse{
		BySource: map[string]int{},
	}
	for _, src := range arcCorpusSources {
		resp.BySource[src] = 0
	}

	// Totals + truncated + session diversity in one pass.
	if err := s.Pool.DB().QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(DISTINCT session_id),
		       COALESCE(SUM(truncated), 0)
		FROM arcreview_snapshot_corpus`,
	).Scan(&resp.TotalRows, &resp.DistinctSessions, &resp.TruncatedRows); err != nil {
		dbErr(w, err)
		return
	}

	// Source split (zero-filled above).
	srcRows, err := s.Pool.DB().QueryContext(ctx,
		`SELECT source, COUNT(*) FROM arcreview_snapshot_corpus GROUP BY source`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer srcRows.Close()
	for srcRows.Next() {
		var src string
		var n int
		if err := srcRows.Scan(&src, &n); err != nil {
			dbErr(w, err)
			return
		}
		resp.BySource[src] = n
	}
	if err := srcRows.Err(); err != nil {
		dbErr(w, err)
		return
	}

	// Training-tuple completeness: the row's event must carry both a
	// non-empty decisions array and a non-empty arc_summary. nothing_to_file
	// fires (empty decisions) are valid labeled pairs but are NOT counted
	// here — "complete" means the full (snapshot, decisions, summary) tuple.
	if err := s.Pool.DB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM arcreview_snapshot_corpus c
		JOIN events e ON e.event_id = c.event_id
		WHERE e.type = 'ArcCloseFilingReviewed'
		  AND json_array_length(json_extract(e.payload, '$.decisions')) > 0
		  AND COALESCE(json_extract(e.payload, '$.arc_summary'), '') <> ''`,
	).Scan(&resp.TupleCompleteRows); err != nil {
		dbErr(w, err)
		return
	}

	if resp.MessageCount, err = s.arcCorpusBuckets(ctx, messageCountBucketCase, messageCountBucketOrder); err != nil {
		dbErr(w, err)
		return
	}
	if resp.EstimatedTokens, err = s.arcCorpusBuckets(ctx, estimatedTokensBucketCase, estimatedTokensBucketOrder); err != nil {
		dbErr(w, err)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

// arcCorpusBuckets runs one grouped count over the given CASE expression
// and returns the buckets in `order`, zero-filling any label the query did
// not produce so the chart always renders the full ordered set.
func (s AppState) arcCorpusBuckets(ctx context.Context, caseExpr string, order []string) ([]ArcCorpusBucket, error) {
	rows, err := s.Pool.DB().QueryContext(ctx,
		"SELECT "+caseExpr+" AS bucket, COUNT(*) FROM arcreview_snapshot_corpus GROUP BY bucket")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var label string
		var n int
		if err := rows.Scan(&label, &n); err != nil {
			return nil, err
		}
		counts[label] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ArcCorpusBucket, len(order))
	for i, label := range order {
		out[i] = ArcCorpusBucket{Label: label, Count: counts[label]}
	}
	return out, nil
}
