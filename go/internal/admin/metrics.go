package admin

import (
	"context"
	"encoding/json"
	"math"
	"sort"

	"toolkit/internal/db"
)

// recentQueryRow mirrors shared_db::vault_telemetry::RecentQueryRow.
type recentQueryRow struct {
	Query        string `json:"query"`
	ResultsCount int64  `json:"results_count"`
	LatencyMs    int64  `json:"latency_ms"`
	CreatedAt    string `json:"created_at"`
}

// vaultSearchMetrics mirrors shared_db::vault_telemetry::VaultSearchMetrics.
// Nullable scalars surface as *T so the encoder writes `null` (matching
// Rust Option<T>) instead of dropping the keys.
type vaultSearchMetricsOut struct {
	WindowStart      *string          `json:"window_start"`
	WindowEnd        *string          `json:"window_end"`
	TotalCalls       int64            `json:"total_calls"`
	P50LatencyMs     *int64           `json:"p50_latency_ms"`
	P95LatencyMs     *int64           `json:"p95_latency_ms"`
	MeanResultsCount *float64         `json:"mean_results_count"`
	RecentQueries    []recentQueryRow `json:"recent_queries"`
}

// vaultSearchMetricsParams is the typed param struct for vault_search_metrics.
// Hoisted from an inline anonymous struct to a named, co-located type so the
// action-doc registry can reflect its field kinds (chain
// migrate-admin-action-docs-to-derive-contract): `since` derives to
// optional_string (string field, not required), `recent_n` to integer (int64
// field). The Unmarshal behavior is unchanged — same json tags, same tolerant
// decode (a malformed payload leaves the zero value, matching the prior
// `_ = json.Unmarshal` discard).
type vaultSearchMetricsParams struct {
	Since   string `json:"since"`
	RecentN int64  `json:"recent_n"`
}

func (d Deps) vaultSearchMetrics(ctx context.Context, params json.RawMessage) (vaultSearchMetricsOut, error) {
	var p vaultSearchMetricsParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	recentN := p.RecentN
	if recentN <= 0 {
		recentN = 50
	}
	if recentN > 1000 {
		recentN = 1000
	}

	var rows *recentQueryRowSet
	var err error
	if p.Since != "" {
		rows, err = d.fetchVaultRows(ctx, p.Since, recentN)
	} else {
		rows, err = d.fetchVaultRows(ctx, "", recentN)
	}
	if err != nil {
		return vaultSearchMetricsOut{}, err
	}

	out := vaultSearchMetricsOut{RecentQueries: []recentQueryRow{}}
	if len(rows.all) == 0 {
		return out, nil
	}

	latencies := make([]int64, len(rows.all))
	var sumResults float64
	for i, r := range rows.all {
		latencies[i] = r.LatencyMs
		sumResults += float64(r.ResultsCount)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := percentile(latencies, 50)
	p95 := percentile(latencies, 95)
	mean := sumResults / float64(len(rows.all))

	// Rust took the first-row created_at as window_end and last-row as
	// window_start (rows sorted DESC). Mirror.
	winStart := rows.all[len(rows.all)-1].CreatedAt
	winEnd := rows.all[0].CreatedAt

	limit := len(rows.all)
	if limit > 20 {
		limit = 20
	}
	for _, r := range rows.all[:limit] {
		out.RecentQueries = append(out.RecentQueries, r)
	}
	out.WindowStart = &winStart
	out.WindowEnd = &winEnd
	out.TotalCalls = int64(len(rows.all))
	out.P50LatencyMs = &p50
	out.P95LatencyMs = &p95
	out.MeanResultsCount = &mean
	return out, nil
}

type recentQueryRowSet struct {
	all []recentQueryRow
}

func (d Deps) fetchVaultRows(ctx context.Context, since string, limit int64) (*recentQueryRowSet, error) {
	// Reads from grounding_events post chain telemetry-substrate-cleanup
	// T2 (migration 046). Latency is reconstructed as pass1_latency_ms +
	// COALESCE(pass2_latency_ms, 0); query_text + results_count are
	// already on the row. action filter restricts to vault_search.
	q := `SELECT COALESCE(query_text, ''),
	             results_count,
	             COALESCE(pass1_latency_ms, 0) + COALESCE(pass2_latency_ms, 0) AS latency_ms,
	             created_at
	      FROM grounding_events
	      WHERE action = 'vault_search'`
	args := db.NewArgs()
	if since != "" {
		q += ` AND created_at >= ?`
		args.AddString(since)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args.AddInt64(limit)
	rows, err := d.Pool.DB().QueryContext(ctx, q, args.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &recentQueryRowSet{}
	for rows.Next() {
		var r recentQueryRow
		if err := rows.Scan(&r.Query, &r.ResultsCount, &r.LatencyMs, &r.CreatedAt); err != nil {
			return nil, err
		}
		out.all = append(out.all, r)
	}
	return out, rows.Err()
}

// percentile uses the nearest-rank method (R-1), matching Rust's
// shared_db::vault_telemetry::percentile. p95 over 50 rows → 48th
// element of the sorted slice. The sorted slice must be in ascending order.
func percentile(sorted []int64, pct int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(pct) / 100.0 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	idx := rank - 1
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
