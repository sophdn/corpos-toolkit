// Package modelrank is the data-driven model selector for the inference
// router (chain data-driven-model-routing, Chain 3 of the telemetry-
// consolidation program). It reads the read-side projection
// proj_inference_tool_model_performance (per (task_id, model_name): call_count,
// success_count, outcome_success_count, total_latency_ms) through a short
// in-process cache and ranks the candidate models for a task, falling back to
// a static default at cold start.
//
// It lives outside the router package so the router stays db-free: the router
// holds an injected router.ModelSelectorFunc (wired in main.go to
// Ranker.Select), exactly as it holds an injected telemetry recorder.
//
// The ranking rule is deliberately conservative + cost-aware: it stays on the
// static default (the free local model) unless another model MATERIALLY beats
// it, so it never shifts a free local call to a paid remote model on a thin
// quality edge. See docs/CHAIN3_ROUTING_TARGET.md §1.3 (the user-vettable rule).
package modelrank

import (
	"context"
	"sync"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/qwenctx"
)

const (
	// warmupMinCalls is the per-(task,model) call_count below which a row is
	// not yet trustworthy and the model is treated as cold (mirrors the
	// health-cards warmupMinCallsForSuccessRate=20 precedent).
	warmupMinCalls = 20
	// qualityMargin is the absolute outcome-success-rate edge a non-default
	// model must clear to displace the static default — the cost-asymmetry
	// guard (Finding-R1): the free local default is only abandoned when a
	// (possibly paid) alternative is materially better, never on a thin edge.
	qualityMargin = 0.10
	// cacheTTL bounds projection reads to at most one per task per minute.
	cacheTTL = 60 * time.Second
)

// ModelStat is one (model) row of a task's per-model performance, the input to
// the pure ranking function.
type ModelStat struct {
	ModelName           string
	CallCount           int64
	OutcomeSuccessCount int64
	SuccessCount        int64
	TotalLatencyMS      int64
}

func quality(m ModelStat) float64 {
	if m.CallCount == 0 {
		return 0
	}
	return float64(m.OutcomeSuccessCount) / float64(m.CallCount)
}

func avgLatency(m ModelStat) float64 {
	if m.CallCount == 0 {
		return 0
	}
	return float64(m.TotalLatencyMS) / float64(m.CallCount)
}

// rank chooses the model for a task from its per-model stats and the static
// default. Returns (chosen, switched); switched=false means "use the default"
// — either cold start (the default model itself is not warmed) or no candidate
// cleared the cost-asymmetry margin. Pure (no I/O): the vettable core, pinned
// exhaustively by the step-7 net.
func rank(rows []ModelStat, defaultModel string) (string, bool) {
	var dStat *ModelStat
	warmed := make([]ModelStat, 0, len(rows))
	for _, m := range rows {
		if m.CallCount < warmupMinCalls {
			continue
		}
		warmed = append(warmed, m)
		if m.ModelName == defaultModel {
			cp := m
			dStat = &cp
		}
	}
	// Cold start: the default model has no warmed row to compare against, so
	// there is no trustworthy basis to route elsewhere — stay on the default.
	if dStat == nil {
		return defaultModel, false
	}
	dQ := quality(*dStat)

	bestName := defaultModel
	bestQ := dQ
	var bestLat float64
	switched := false
	for _, m := range warmed {
		if m.ModelName == defaultModel {
			continue
		}
		q := quality(m)
		if q < dQ+qualityMargin {
			continue // does not materially beat the free default
		}
		lat := avgLatency(m)
		// Among margin-clearing candidates, prefer higher quality; tie-break
		// on lower average latency.
		if !switched || q > bestQ || (q == bestQ && lat < bestLat) {
			bestName, bestQ, bestLat, switched = m.ModelName, q, lat, true
		}
	}
	return bestName, switched
}

type cacheEntry struct {
	model    string
	ok       bool
	expireAt time.Time
}

// Ranker is the cached, best-effort selector wired into the router. It is safe
// for concurrent use (the daemon hosts concurrent MCP sessions).
type Ranker struct {
	pool         *db.Pool
	defaultModel string
	ttl          time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
	now   func() time.Time // injectable clock for tests
}

// NewRanker builds a Ranker over the given pool with the static default model
// (the local model identifier, e.g. "qwen2.5-32b") used as the cold-start
// fallback. main.go wires inferRouter.SetModelSelector(ranker.Select).
func NewRanker(pool *db.Pool, defaultModel string) *Ranker {
	return &Ranker{
		pool:         pool,
		defaultModel: defaultModel,
		ttl:          cacheTTL,
		cache:        make(map[string]cacheEntry),
		now:          time.Now,
	}
}

// Select implements router.ModelSelectorFunc. It returns the model to use for
// the task on ctx and ok=true when a data-driven choice was made, or
// ("", false) to let the router fall back to its static default — for an
// unattributed task, a cache/query path that yields no switch, or ANY read
// error (best-effort: a telemetry-read outage must never block or misroute the
// inference call; it degrades to the static default).
func (rk *Ranker) Select(ctx context.Context) (string, bool) {
	taskID := qwenctx.TaskID(ctx)
	if taskID == "" || taskID == qwenctx.Unattributed {
		return "", false
	}
	now := rk.now()

	rk.mu.Lock()
	if e, ok := rk.cache[taskID]; ok && now.Before(e.expireAt) {
		rk.mu.Unlock()
		return e.model, e.ok
	}
	rk.mu.Unlock()

	rows, err := rk.queryStats(ctx, taskID)
	if err != nil {
		// Best-effort: do not cache an error; degrade to the static default.
		return "", false
	}
	model, switched := rank(rows, rk.defaultModel)

	rk.mu.Lock()
	rk.cache[taskID] = cacheEntry{model: model, ok: switched, expireAt: now.Add(rk.ttl)}
	rk.mu.Unlock()
	return model, switched
}

// queryStats reads the per-model performance rows for one task from the
// read-side projection. Returns the rows (possibly empty) or an error.
func (rk *Ranker) queryStats(ctx context.Context, taskID string) ([]ModelStat, error) {
	rows, err := rk.pool.DB().QueryContext(ctx,
		`SELECT model_name, call_count, outcome_success_count, success_count, total_latency_ms
		   FROM proj_inference_tool_model_performance
		  WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelStat
	for rows.Next() {
		var m ModelStat
		if err := rows.Scan(&m.ModelName, &m.CallCount, &m.OutcomeSuccessCount, &m.SuccessCount, &m.TotalLatencyMS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
