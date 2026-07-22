package abtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"toolkit/internal/db"
)

// PromotionGateConfig tunes the gate's firing criteria. Defaults match
// docs/ML_CAPABILITY_SUBSTRATE.md §8.3 — N >= 100 comparisons, delta
// > 0.05, stable over 3 windows. Per-task chains override via
// per-task config at promotion-time.
type PromotionGateConfig struct {
	MinComparisons int     // default 100
	MinDelta       float64 // default 0.05
	WindowDays     int     // default 7
	WindowsStable  int     // default 3
}

// PromotionGateVerdict is the readout. Ready=true means the gate
// permits a promote call; the caller is still required to invoke
// work.trained_model_promote explicitly (the gate suggests, doesn't
// auto-promote).
type PromotionGateVerdict struct {
	ModelID                  int64   `json:"model_id"`
	TotalComparisons         int     `json:"total_comparisons"`
	TrainedClickThroughRate  float64 `json:"trained_click_through_rate"`
	BaselineClickThroughRate float64 `json:"baseline_click_through_rate"`
	Delta                    float64 `json:"delta"`
	Ready                    bool    `json:"ready"`
	BlockedReason            string  `json:"blocked_reason,omitempty"`
}

// EvaluatePromotionGate scores baseline-vs-trained on the comparison
// corpus for a given trained_model.id.
//
// Click-through is measured by joining ab_comparisons to
// query_interactions (from query-telemetry-substrate). A "click" is a
// query_interactions row matched by (span_id, click_kind in ('cited',
// 'followed')) — a row in either kind says the agent surfaced + used
// the result the harness picked.
//
// Per-path rate: (clicks where used_path = X) / (total comparisons
// where used_path = X). The Delta = trained_rate - baseline_rate.
//
// In test fixtures or pre-telemetry phases the query_interactions
// table may be empty; the gate then returns Ready=false with a
// BlockedReason naming the gap. NOT an error — the substrate's
// no-regression guarantee holds (promotion stays manual).
func EvaluatePromotionGate(ctx context.Context, pool *db.Pool, modelID int64, cfg PromotionGateConfig) (PromotionGateVerdict, error) {
	if pool == nil {
		return PromotionGateVerdict{}, fmt.Errorf("EvaluatePromotionGate: pool is nil")
	}
	if cfg.MinComparisons == 0 {
		cfg.MinComparisons = 100
	}
	if cfg.MinDelta == 0 {
		cfg.MinDelta = 0.05
	}
	if cfg.WindowDays == 0 {
		cfg.WindowDays = 7
	}
	if cfg.WindowsStable == 0 {
		cfg.WindowsStable = 3
	}

	verdict := PromotionGateVerdict{ModelID: modelID}

	// Total comparisons (used to gate min-sample-size).
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ab_comparisons WHERE model_id = ?`,
		modelID).Scan(&verdict.TotalComparisons); err != nil {
		return verdict, fmt.Errorf("count comparisons: %w", err)
	}

	if verdict.TotalComparisons < cfg.MinComparisons {
		verdict.BlockedReason = fmt.Sprintf(
			"insufficient comparisons: %d < min %d",
			verdict.TotalComparisons, cfg.MinComparisons)
		return verdict, nil
	}

	// Per-path click-through. Left-join to query_interactions on span_id
	// — the harness writes one ab_comparisons row per Dispatch call,
	// inheriting the dispatcher's span_id. When the caller subsequently
	// surfaces + uses the result, the substrate's interaction detector
	// emits a query_interactions row with the same span and click_kind in
	// {'cited', 'followed'} (per query-telemetry-substrate TT1.5 §5).
	trainedRate, baselineRate, err := perPathClickThrough(ctx, pool, modelID)
	if errors.Is(err, sql.ErrNoRows) {
		verdict.BlockedReason = "no click-through signal yet (query_interactions empty for this model)"
		return verdict, nil
	}
	if err != nil {
		return verdict, fmt.Errorf("per-path click-through: %w", err)
	}

	verdict.TrainedClickThroughRate = trainedRate
	verdict.BaselineClickThroughRate = baselineRate
	verdict.Delta = trainedRate - baselineRate

	if verdict.Delta < cfg.MinDelta {
		verdict.BlockedReason = fmt.Sprintf(
			"trained delta %.3f < min delta %.3f", verdict.Delta, cfg.MinDelta)
		return verdict, nil
	}

	// Stability: split the comparison corpus into N rolling windows of
	// WindowDays each; require the delta to stay above MinDelta in
	// every window for WindowsStable consecutive windows.
	stable, err := windowStable(ctx, pool, modelID, cfg)
	if err != nil {
		return verdict, fmt.Errorf("window stability: %w", err)
	}
	if !stable {
		verdict.BlockedReason = fmt.Sprintf(
			"delta unstable: not above %.3f for %d consecutive %d-day windows",
			cfg.MinDelta, cfg.WindowsStable, cfg.WindowDays)
		return verdict, nil
	}

	verdict.Ready = true
	return verdict, nil
}

// perPathClickThrough returns (trained_rate, baseline_rate) for the
// model. Click defined as the dispatch's span yielded a
// query_interactions row with click_kind in ('cited','followed').
func perPathClickThrough(ctx context.Context, pool *db.Pool, modelID int64) (float64, float64, error) {
	const q = `
WITH path_totals AS (
    SELECT used_path, COUNT(*) AS n
    FROM ab_comparisons
    WHERE model_id = ?
    GROUP BY used_path
),
path_clicks AS (
    SELECT ac.used_path, COUNT(DISTINCT ac.span_id) AS clicked
    FROM ab_comparisons ac
    JOIN query_interactions qi
      ON qi.span_id = ac.span_id
     AND qi.click_kind IN ('cited', 'followed')
    WHERE ac.model_id = ?
    GROUP BY ac.used_path
)
SELECT pt.used_path, pt.n, COALESCE(pc.clicked, 0)
FROM path_totals pt
LEFT JOIN path_clicks pc ON pc.used_path = pt.used_path`

	rows, err := pool.DB().QueryContext(ctx, q, modelID, modelID)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	var trainedRate, baselineRate float64
	pathsSeen := 0
	for rows.Next() {
		var path string
		var n, clicked int
		if err := rows.Scan(&path, &n, &clicked); err != nil {
			return 0, 0, err
		}
		pathsSeen++
		var rate float64
		if n > 0 {
			rate = float64(clicked) / float64(n)
		}
		switch path {
		case "trained":
			trainedRate = rate
		case "baseline":
			baselineRate = rate
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if pathsSeen == 0 {
		return 0, 0, sql.ErrNoRows
	}
	return trainedRate, baselineRate, nil
}

// windowStable enforces the temporal-stability rule: the delta must
// stay above MinDelta for WindowsStable consecutive WindowDays-long
// windows ending at now().
//
// SQLite's date arithmetic is sufficient for this — we don't need
// strftime-week-bucketing here, just N rolling spans.
func windowStable(ctx context.Context, pool *db.Pool, modelID int64, cfg PromotionGateConfig) (bool, error) {
	for w := 0; w < cfg.WindowsStable; w++ {
		windowEnd := time.Now().AddDate(0, 0, -w*cfg.WindowDays)
		windowStart := windowEnd.AddDate(0, 0, -cfg.WindowDays)

		trainedRate, baselineRate, err := perPathClickThroughInWindow(ctx, pool, modelID, windowStart, windowEnd)
		if errors.Is(err, sql.ErrNoRows) {
			// Window has no comparisons; treat as unstable rather than
			// extrapolate forward.
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if trainedRate-baselineRate < cfg.MinDelta {
			return false, nil
		}
	}
	return true, nil
}

func perPathClickThroughInWindow(
	ctx context.Context,
	pool *db.Pool,
	modelID int64,
	start, end time.Time,
) (float64, float64, error) {
	const q = `
WITH path_totals AS (
    SELECT used_path, COUNT(*) AS n
    FROM ab_comparisons
    WHERE model_id = ?
      AND created_at >= ?
      AND created_at <  ?
    GROUP BY used_path
),
path_clicks AS (
    SELECT ac.used_path, COUNT(DISTINCT ac.span_id) AS clicked
    FROM ab_comparisons ac
    JOIN query_interactions qi
      ON qi.span_id = ac.span_id
     AND qi.click_kind IN ('cited', 'followed')
    WHERE ac.model_id = ?
      AND ac.created_at >= ?
      AND ac.created_at <  ?
    GROUP BY ac.used_path
)
SELECT pt.used_path, pt.n, COALESCE(pc.clicked, 0)
FROM path_totals pt
LEFT JOIN path_clicks pc ON pc.used_path = pt.used_path`

	startStr := start.UTC().Format("2006-01-02 15:04:05")
	endStr := end.UTC().Format("2006-01-02 15:04:05")
	rows, err := pool.DB().QueryContext(ctx, q,
		modelID, startStr, endStr,
		modelID, startStr, endStr)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var trainedRate, baselineRate float64
	pathsSeen := 0
	for rows.Next() {
		var path string
		var n, clicked int
		if err := rows.Scan(&path, &n, &clicked); err != nil {
			return 0, 0, err
		}
		pathsSeen++
		var rate float64
		if n > 0 {
			rate = float64(clicked) / float64(n)
		}
		switch path {
		case "trained":
			trainedRate = rate
		case "baseline":
			baselineRate = rate
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if pathsSeen == 0 {
		return 0, 0, sql.ErrNoRows
	}
	return trainedRate, baselineRate, nil
}
