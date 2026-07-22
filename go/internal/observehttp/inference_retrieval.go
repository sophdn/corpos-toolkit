package observehttp

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Retrieval-health endpoint (chain telemetry-substrate-cleanup T3c).
//
// Per vault learning 2026-05-17_tiered-implicit-feedback-for-rag-telemetry:
// the substrate's `click_kind` enum is tiered with default weights so a
// consumer can choose between high-precision ('followed', 'resolved-from')
// and high-recall ('mentioned', any-kind) views. The retrieval-health
// panel respects that — separate per-kind rates AND a weighted aggregate
// score, NOT a single "any click" rate that would collapse the tiering.
//
// Default weights (mirroring the vault learning's table):
//   followed      = 1.0  — agent navigated to the source_ref deliberately
//   cited         = 0.8  — assistant text quoted ≥40 chars / file-line ref
//   mentioned     = 0.4  — source_ref string appeared but no follow-up
//   resolved-from = 1.0  — terminal resolution rationale named the source
//
// Reads `query_interactions` joined to `grounding_events` for the three
// actions that emit search calls (vault_search / kiwix_search /
// knowledge_search). Returns an empty array — and the dashboard hides
// the panel — when the substrate isn't populated yet (zero
// query_interactions rows in the window).

// clickKindWeight maps each tier to its default weight. Held in code
// rather than the SQL because the dashboard might choose to render
// alternative weights for an installation (future per-deployment
// override; not implemented today).
var clickKindWeight = map[string]float64{
	"followed":      1.0,
	"cited":         0.8,
	"mentioned":     0.4,
	"resolved-from": 1.0,
}

// retrievalActions are the three grounding_events.action values that
// query_interactions rows can fan out from. Keeps SQL simple — IN clause
// has a fixed shape.
var retrievalActions = []string{"vault_search", "kiwix_search", "knowledge_search"}

// RetrievalKindStat is one (action, click_kind) bucket.
type RetrievalKindStat struct {
	ClickKind string  `json:"click_kind"`
	Count     int64   `json:"count"`
	Rate      float64 `json:"rate"`   // count / grounding_count (capped at 1.0 for display)
	Weight    float64 `json:"weight"` // surfaces the default weight so the dashboard doesn't have to re-derive it
}

// RetrievalHealthAction is the per-action aggregate that the panel
// renders as one row.
type RetrievalHealthAction struct {
	Action           string              `json:"action"`
	GroundingCount   int64               `json:"grounding_count"`
	InteractionCount int64               `json:"interaction_count"`
	ByKind           []RetrievalKindStat `json:"by_kind"`
	// WeightedScore: sum over kinds of (rate * weight). Captures the
	// per-search "expected weight of feedback signal" — values closer
	// to 1.0 mean every search produced a high-tier interaction; values
	// near 0.0 mean most searches resulted in no useful feedback.
	WeightedScore float64 `json:"weighted_score"`
	// WarmingUp is true when grounding_count is below the sample floor
	// for this panel (≥10 grounding rows). Below the floor the rates
	// are too noisy to be useful; the dashboard renders a badge instead
	// of the bars.
	WarmingUp bool `json:"warming_up"`
}

const retrievalHealthMinGrounding = 10

// inferenceRetrievalHealth aggregates per-action click_kind rates over
// the configured window (default 7d, max 90d). Always returns an array
// — empty when the substrate has no interaction rows for any retrieval
// action in the window.
func (s AppState) inferenceRetrievalHealth(w http.ResponseWriter, r *http.Request) {
	windowDays := defaultWindowDays
	if v := r.URL.Query().Get("window_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			windowDays = n
		}
	}
	windowStart := time.Now().AddDate(0, 0, -windowDays).UTC().Format(time.RFC3339)

	results := make([]RetrievalHealthAction, 0, len(retrievalActions))
	for _, action := range retrievalActions {
		stat, err := s.fetchRetrievalAction(r, action, windowStart)
		if err != nil {
			dbErr(w, fmt.Errorf("aggregate %s: %w", action, err))
			return
		}
		// Skip actions with zero grounding rows in window — the panel
		// degrades gracefully when an action has no traffic yet.
		if stat.GroundingCount == 0 {
			continue
		}
		results = append(results, stat)
	}

	// Most-active action first; alphabetical tie-break for determinism.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].GroundingCount != results[j].GroundingCount {
			return results[i].GroundingCount > results[j].GroundingCount
		}
		return results[i].Action < results[j].Action
	})

	writeJSON(w, http.StatusOK, results)
}

// fetchRetrievalAction does the two queries needed for one action: count
// of grounding_events rows (the denominator) and the per-click_kind
// histogram (the numerators). One pass each — small data; the JOIN to
// grounding_events restricts query_interactions to the right action.
func (s AppState) fetchRetrievalAction(r *http.Request, action, windowStart string) (RetrievalHealthAction, error) {
	out := RetrievalHealthAction{Action: action, ByKind: []RetrievalKindStat{}}

	// Denominator: count of grounding_events rows for this action in window.
	if err := s.Pool.DB().QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM grounding_events
		 WHERE action = ? AND created_at >= ?`,
		action, windowStart,
	).Scan(&out.GroundingCount); err != nil {
		return out, err
	}
	if out.GroundingCount == 0 {
		return out, nil
	}

	// Per-kind histogram. Joins query_interactions to grounding_events
	// so the action filter applies to the source rows the interactions
	// fanned out from.
	rows, err := s.Pool.DB().QueryContext(r.Context(),
		`SELECT qi.click_kind, COUNT(*) AS n
		 FROM query_interactions qi
		 JOIN grounding_events ge ON qi.grounding_event_id = ge.id
		 WHERE ge.action = ? AND ge.created_at >= ?
		 GROUP BY qi.click_kind`,
		action, windowStart,
	)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	var totalInteractions int64
	var weightedSum float64
	denom := float64(out.GroundingCount)
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return out, err
		}
		weight := clickKindWeight[kind] // 0.0 for unknown tiers — safe default
		rate := float64(count) / denom
		out.ByKind = append(out.ByKind, RetrievalKindStat{
			ClickKind: kind,
			Count:     count,
			Rate:      rate,
			Weight:    weight,
		})
		totalInteractions += count
		weightedSum += rate * weight
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.InteractionCount = totalInteractions
	out.WeightedScore = weightedSum
	out.WarmingUp = out.GroundingCount < retrievalHealthMinGrounding

	// Render order: followed → cited → mentioned → resolved-from. Same
	// as the vault learning's table; preserves a natural strong→weak→
	// strong narrative the panel reads from left to right.
	kindOrder := map[string]int{"followed": 0, "cited": 1, "mentioned": 2, "resolved-from": 3}
	sort.SliceStable(out.ByKind, func(i, j int) bool {
		oi, ok1 := kindOrder[out.ByKind[i].ClickKind]
		oj, ok2 := kindOrder[out.ByKind[j].ClickKind]
		if !ok1 {
			oi = 99
		}
		if !ok2 {
			oj = 99
		}
		if oi != oj {
			return oi < oj
		}
		return out.ByKind[i].ClickKind < out.ByKind[j].ClickKind
	})
	return out, nil
}

// dbErr suppression: this file uses the package-local dbErr / writeJSON
// helpers defined in router-adjacent files; no additional plumbing
// needed.
var _ = sql.ErrNoRows
