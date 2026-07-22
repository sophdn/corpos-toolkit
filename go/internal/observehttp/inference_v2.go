package observehttp

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/obs"
)

// Inference page v2 endpoints (chain telemetry-substrate-cleanup T3).
//
// The v1 page at /inference answers "did calls happen?" — task name,
// total calls, avg latency, linked bugs. That's a liveness signal
// masquerading as a health page. v2 separates the questions:
//
//   - is it alive?      → last_call_at + stale-threshold tinting
//   - is it healthy?    → p50/p95/p99 latency + success_rate
//   - is it improving?  → 7d sparklines (separate endpoint)
//   - is it costly?     → tokens_per_day + model_breakdown
//
// Per chain T3 acceptance criteria + vault learning
// 2026-05-12_telemetry-history-warmup-period: signals that depend on
// rolling history (p99, success_rate, sparklines) are marked WARMING_UP
// in the response when there aren't enough calls / days of data to
// compute a meaningful value, rather than silently returning a fallback.
// The dashboard renders the warming-up state visibly (vs. silently
// degrading to noise).

// Warmup thresholds — minima below which the corresponding signal is
// reported as warming-up rather than computed. Cross-validated with the
// vault warmup-period learning: the precondition belongs in the
// acceptance criteria, not the UI as an afterthought.
const (
	warmupMinCallsForP99         = 100
	warmupMinDaysForSparklines   = 3
	warmupMinCallsForSuccessRate = 20
	defaultWindowDays            = 7
	staleThresholdRedSeconds     = 86400 // ≥ 24h since last call → red
	staleThresholdYellowSeconds  = 3600  // ≥ 1h since last call → yellow
)

// HealthCard is the per-task record returned by /inference/health-cards.
// One card per discrete task_id observed in the window.
type HealthCard struct {
	TaskID           string         `json:"task_id"`
	LastCallAt       *string        `json:"last_call_at"` // ISO timestamp; nil for tasks with no calls in window
	CallCount        int64          `json:"call_count"`
	P50LatencyMS     *int64         `json:"p50_latency_ms"`     // nil when call_count == 0
	P95LatencyMS     *int64         `json:"p95_latency_ms"`     // nil when warming up
	P99LatencyMS     *int64         `json:"p99_latency_ms"`     // nil when warming up
	SuccessRate      *float64       `json:"success_rate"`       // nil when warming up
	SuccessRateBasis string         `json:"success_rate_basis"` // describes the predicate used
	BugCount         int64          `json:"bug_count"`          // joined from bugs.qwen_task_id
	TokensPerDay     *int64         `json:"tokens_per_day"`     // sum input+output / window days
	ModelBreakdown   []ModelStat    `json:"model_breakdown"`
	WarmingUp        WarmingUpFlags `json:"warming_up"` // per-signal warming-up state
}

// ModelStat is the per-model breakdown inside a HealthCard. Tasks served
// by multiple models surface here so the operator can decide whether to
// retarget at a cheaper / faster model.
type ModelStat struct {
	ModelName    string `json:"model_name"`
	CallCount    int64  `json:"call_count"`
	P95LatencyMS int64  `json:"p95_latency_ms"`
}

// WarmingUpFlags identifies which signals on the card are below their
// warmup threshold and therefore are NULL in the response rather than
// computed. Per vault learning 2026-05-12: the precondition belongs in
// the API response so the dashboard can render a "warming up" badge
// instead of silently degrading to a misleading default.
type WarmingUpFlags struct {
	P99         bool `json:"p99"`          // < warmupMinCallsForP99
	SuccessRate bool `json:"success_rate"` // < warmupMinCallsForSuccessRate
	Sparklines  bool `json:"sparklines"`   // applies to /inference/sparklines, surfaced here for the dashboard's panel-gating decision
}

// inferenceHealthCards is the new endpoint. It queries inference_invocations
// for per-task aggregates over a configurable window (default 7d),
// joins bug counts and the success-predicate registry, and returns one
// HealthCard per observed task_id.
func (s AppState) inferenceHealthCards(w http.ResponseWriter, r *http.Request) {
	windowDays := defaultWindowDays
	if v := r.URL.Query().Get("window_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			windowDays = n
		}
	}
	windowStart := time.Now().AddDate(0, 0, -windowDays)

	taskIDs, err := s.inferenceListTaskIDs(r, windowStart)
	if err != nil {
		dbErr(w, err)
		return
	}

	bugMap, err := s.inferenceBugCounts(r, projectFilter(r))
	if err != nil {
		dbErr(w, err)
		return
	}

	cards := make([]HealthCard, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		card, err := s.buildHealthCard(r, taskID, windowStart, windowDays)
		if err != nil {
			dbErr(w, fmt.Errorf("build card for %s: %w", taskID, err))
			return
		}
		card.BugCount = bugMap[taskID]
		cards = append(cards, card)
	}

	// Stale tasks (longest-since-last-call) first — operator wants to
	// see "what's broken" before "what's working." Within a stale tier,
	// alphabetical for determinism.
	sort.SliceStable(cards, func(i, j int) bool {
		ai, aj := cards[i].LastCallAt, cards[j].LastCallAt
		if ai == nil && aj == nil {
			return cards[i].TaskID < cards[j].TaskID
		}
		if ai == nil {
			return true // unknown-last-call ranks more stale than known
		}
		if aj == nil {
			return false
		}
		if *ai != *aj {
			return *ai < *aj // older last_call_at first
		}
		return cards[i].TaskID < cards[j].TaskID
	})

	writeJSON(w, http.StatusOK, cards)
}

// inferenceBugCounts joins bugs.qwen_task_id to produce a count of
// bugs filed against each task_id. Surfaces in the health-card's
// `bug_count` field. Project filter restricts to one project's bugs
// when set.
func (s AppState) inferenceBugCounts(r *http.Request, project string) (map[string]int64, error) {
	sqlStr := `SELECT qwen_task_id, COUNT(*) AS bug_count
		FROM proj_current_bugs WHERE qwen_task_id IS NOT NULL`
	binds := db.NewArgs()
	if project != "" {
		sqlStr += " AND project_id = ?"
		binds.AddString(project)
	}
	sqlStr += " GROUP BY qwen_task_id"
	rows, err := s.Pool.DB().QueryContext(r.Context(), sqlStr, binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var taskID string
		var count int64
		if err := rows.Scan(&taskID, &count); err != nil {
			return nil, err
		}
		out[taskID] = count
	}
	return out, rows.Err()
}

// inferenceListTaskIDs returns the distinct task_ids observed in the
// window. Sorted for determinism.
func (s AppState) inferenceListTaskIDs(r *http.Request, windowStart time.Time) ([]string, error) {
	rows, err := s.Pool.DB().QueryContext(r.Context(),
		`SELECT DISTINCT task_id FROM inference_invocations
		 WHERE created_at >= ?
		 ORDER BY task_id`,
		windowStart.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// buildHealthCard composes one card. Pulls latency percentiles and the
// success-predicate result in two queries (one aggregate scan; one bug
// count). The success predicate is interpolated server-side; the registry
// itself doesn't accept user-supplied SQL, so this is not an injection
// surface.
func (s AppState) buildHealthCard(r *http.Request, taskID string, windowStart time.Time, windowDays int) (HealthCard, error) {
	card := HealthCard{TaskID: taskID, ModelBreakdown: []ModelStat{}}

	pred, hadCustom := lookupSuccessPredicate(taskID)
	card.SuccessRateBasis = pred.Description
	if !hadCustom {
		// Log the gap so the operator can surface a predicate via PR.
		// Per the papercut-vs-structural-rework split, this is the
		// papercut layer — the default fires and the structural ask
		// (add a predicate) is visible in logs without blocking.
		obs.Logger(r.Context()).Info("inference v2: using default success predicate",
			slog.String("task_id", taskID),
			slog.String("basis", pred.Description),
		)
	}

	// Liveness + volume aggregates from the source table. Outcome success is
	// materialized in proj_inference_call_success and summed below only when
	// the warmup threshold is met (chain telemetry-success-model-unification —
	// the read-time predicate SQL that used to be interpolated here is gone).
	q := `SELECT
			COUNT(*) AS n,
			MAX(created_at) AS last_call_at,
			COALESCE(SUM(input_tokens), 0) + COALESCE(SUM(output_tokens), 0) AS total_tokens
		FROM inference_invocations
		WHERE task_id = ? AND created_at >= ?`

	var (
		n           int64
		lastCallAt  sql.NullString
		totalTokens int64
	)
	if err := s.Pool.DB().QueryRowContext(r.Context(), q, taskID, windowStart.UTC().Format(time.RFC3339)).
		Scan(&n, &lastCallAt, &totalTokens); err != nil {
		return card, err
	}
	card.CallCount = n
	if lastCallAt.Valid {
		s := lastCallAt.String
		card.LastCallAt = &s
	}

	if n == 0 {
		card.WarmingUp.P99 = true
		card.WarmingUp.SuccessRate = true
		return card, nil
	}

	// tokens_per_day surfaces only when we have at least one full day
	// of window; otherwise the average is too noisy to be useful.
	if windowDays > 0 {
		perDay := totalTokens / int64(windowDays)
		card.TokensPerDay = &perDay
	}

	// Latency percentiles: pull all latencies sorted, then index.
	latencies, err := s.inferenceLatencies(r, taskID, windowStart)
	if err != nil {
		return card, err
	}
	if len(latencies) > 0 {
		p50 := nearestRank(latencies, 50)
		card.P50LatencyMS = &p50
		p95 := nearestRank(latencies, 95)
		card.P95LatencyMS = &p95
	}

	// p99 is warmup-gated — too few calls and the value is one outlier
	// away from meaningless.
	if int64(len(latencies)) >= warmupMinCallsForP99 {
		p99 := nearestRank(latencies, 99)
		card.P99LatencyMS = &p99
	} else {
		card.WarmingUp.P99 = true
	}

	// success_rate is warmup-gated — too few calls and the rate is
	// either 1.0 or 0.0 with no useful resolution. The numerator is the
	// materialized Layer-2 outcome sum, read only now that it will be used.
	if n >= warmupMinCallsForSuccessRate {
		successCount, err := s.inferenceOutcomeSuccessCount(r, taskID, windowStart)
		if err != nil {
			return card, err
		}
		rate := float64(successCount) / float64(n)
		card.SuccessRate = &rate
	} else {
		card.WarmingUp.SuccessRate = true
	}

	// Per-model breakdown for tasks served by multiple models.
	models, err := s.inferenceModelBreakdown(r, taskID, windowStart)
	if err != nil {
		return card, err
	}
	card.ModelBreakdown = models

	// Sparklines warming-up state is computed per-task by inspecting
	// the per-day bucket count; surfaced here so the dashboard's expand
	// affordance can skip the sparkline render when the panel would
	// otherwise show a single point.
	dayCount, err := s.inferenceDayCount(r, taskID, windowStart)
	if err != nil {
		return card, err
	}
	card.WarmingUp.Sparklines = dayCount < warmupMinDaysForSparklines

	return card, nil
}

// inferenceOutcomeSuccessCount sums the materialized Layer-2 outcome success
// over the task's rows in the window — the numerator of health-cards
// success_rate, read from proj_inference_call_success instead of an
// interpolated read-time predicate (chain telemetry-success-model-unification).
func (s AppState) inferenceOutcomeSuccessCount(r *http.Request, taskID string, windowStart time.Time) (int64, error) {
	var n int64
	err := s.Pool.DB().QueryRowContext(r.Context(),
		`SELECT COALESCE(SUM(outcome_success), 0) FROM proj_inference_call_success
		 WHERE task_id = ? AND created_at >= ?`,
		taskID, windowStart.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}

func (s AppState) inferenceLatencies(r *http.Request, taskID string, windowStart time.Time) ([]int64, error) {
	rows, err := s.Pool.DB().QueryContext(r.Context(),
		`SELECT latency_ms FROM inference_invocations
		 WHERE task_id = ? AND created_at >= ?
		 ORDER BY latency_ms`,
		taskID, windowStart.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var l int64
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s AppState) inferenceModelBreakdown(r *http.Request, taskID string, windowStart time.Time) ([]ModelStat, error) {
	rows, err := s.Pool.DB().QueryContext(r.Context(),
		`SELECT model_name, latency_ms
		 FROM inference_invocations
		 WHERE task_id = ? AND created_at >= ?
		 ORDER BY model_name, latency_ms`,
		taskID, windowStart.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Bucket per-model in Go — keeping the SQL ungrouped lets us
	// compute p95 directly from the latency vector without a second
	// query, and avoids the SQLite-only "pick an arbitrary row when
	// COUNT(*) is mixed with per-row columns" behavior that strict-SQL
	// engines would reject outright.
	per := map[string][]int64{}
	order := []string{}
	for rows.Next() {
		var m string
		var l int64
		if err := rows.Scan(&m, &l); err != nil {
			return nil, err
		}
		if _, seen := per[m]; !seen {
			order = append(order, m)
		}
		per[m] = append(per[m], l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ModelStat, 0, len(order))
	for _, k := range order {
		ls := per[k]
		out = append(out, ModelStat{
			ModelName:    k,
			CallCount:    int64(len(ls)),
			P95LatencyMS: nearestRank(ls, 95),
		})
	}
	return out, nil
}

func (s AppState) inferenceDayCount(r *http.Request, taskID string, windowStart time.Time) (int, error) {
	var n int
	err := s.Pool.DB().QueryRowContext(r.Context(),
		`SELECT COUNT(DISTINCT date(created_at)) FROM inference_invocations
		 WHERE task_id = ? AND created_at >= ?`,
		taskID, windowStart.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}

// nearestRank is R-1 (matches admin/metrics.go::percentile). Operates on
// an already-sorted ascending slice. Returns 0 on empty input.
func nearestRank(sorted []int64, pct int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (pct*len(sorted) + 99) / 100 // ceil(pct/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// SparklineBucket is one day's aggregate for the sparkline visualization.
type SparklineBucket struct {
	Date         string   `json:"date"` // YYYY-MM-DD
	CallCount    int64    `json:"call_count"`
	P95LatencyMS *int64   `json:"p95_latency_ms"` // nil when call_count < 5 for the day
	SuccessRate  *float64 `json:"success_rate"`   // nil when call_count < 5 for the day
	TokensBurned int64    `json:"tokens_burned"`
}

// Sparkline is the per-task series. /inference/sparklines returns []Sparkline.
type Sparkline struct {
	TaskID  string            `json:"task_id"`
	Buckets []SparklineBucket `json:"buckets"`
}

// inferenceSparklines is the new endpoint returning per-task per-day
// buckets over the window. Used by the v2 page's expand-row sparkline.
func (s AppState) inferenceSparklines(w http.ResponseWriter, r *http.Request) {
	windowDays := defaultWindowDays
	if v := r.URL.Query().Get("window_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			windowDays = n
		}
	}
	windowStart := time.Now().AddDate(0, 0, -windowDays)
	taskFilter := strings.TrimSpace(r.URL.Query().Get("task_id"))

	taskIDs, err := s.inferenceListTaskIDs(r, windowStart)
	if err != nil {
		dbErr(w, err)
		return
	}

	out := make([]Sparkline, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		if taskFilter != "" && taskID != taskFilter {
			continue
		}
		buckets, err := s.inferenceSparklineBuckets(r, taskID, windowStart)
		if err != nil {
			dbErr(w, fmt.Errorf("buckets for %s: %w", taskID, err))
			return
		}
		out = append(out, Sparkline{TaskID: taskID, Buckets: buckets})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s AppState) inferenceSparklineBuckets(r *http.Request, taskID string, windowStart time.Time) ([]SparklineBucket, error) {
	// Scan ungrouped rows and bucket per-day in Go — keeps the p95
	// computation against the latency vector inline and avoids SQLite's
	// "pick an arbitrary row when COUNT(*) mixes with per-row columns" quirk.
	// Outcome success is the materialized Layer-2 column from
	// proj_inference_call_success (1:1 with inference_invocations by id),
	// joined in instead of interpolating a read-time predicate (chain
	// telemetry-success-model-unification).
	q := `SELECT
			date(qi.created_at) AS d,
			qi.latency_ms,
			COALESCE(qi.input_tokens, 0) + COALESCE(qi.output_tokens, 0) AS tokens,
			ics.outcome_success AS success
		FROM inference_invocations qi
		JOIN proj_inference_call_success ics ON ics.id = qi.id
		WHERE qi.task_id = ? AND qi.created_at >= ?
		ORDER BY d, qi.latency_ms`

	rows, err := s.Pool.DB().QueryContext(r.Context(), q, taskID, windowStart.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type dayAccum struct {
		latencies []int64
		successes int64
		tokens    int64
	}
	per := map[string]*dayAccum{}
	order := []string{}
	for rows.Next() {
		var d string
		var latency, tokens int64
		var success sql.NullInt64
		if err := rows.Scan(&d, &latency, &tokens, &success); err != nil {
			return nil, err
		}
		acc, exists := per[d]
		if !exists {
			acc = &dayAccum{}
			per[d] = acc
			order = append(order, d)
		}
		acc.latencies = append(acc.latencies, latency)
		acc.tokens += tokens
		if success.Valid {
			acc.successes += success.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SparklineBucket, 0, len(order))
	for _, d := range order {
		acc := per[d]
		callCount := int64(len(acc.latencies))
		bucket := SparklineBucket{
			Date:         d,
			CallCount:    callCount,
			TokensBurned: acc.tokens,
		}
		// Per-day p95 + success rate are only meaningful when the day
		// had enough calls. Below the floor we surface NULL — the
		// dashboard renders a gap rather than a misleading value.
		if callCount >= 5 {
			p95 := nearestRank(acc.latencies, 95)
			bucket.P95LatencyMS = &p95
			rate := float64(acc.successes) / float64(callCount)
			bucket.SuccessRate = &rate
		}
		out = append(out, bucket)
	}
	return out, nil
}

// Ensure db import keeps a referenced symbol so the compiler doesn't
// flag the import as unused when only via reflection downstream.
var _ = db.NewArgs
