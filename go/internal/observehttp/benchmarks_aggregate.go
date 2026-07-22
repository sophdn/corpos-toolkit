package observehttp

import "sort"

// ModelMetrics mirrors observe_http::handlers::benchmarks::ModelMetrics.
// All score fields are *float64 so omitted-data rows serialise as
// `null` (matching Rust Option<f64>); verdictDistribution is omitted
// entirely when no rows in the window carried a detected_tool label.
type ModelMetrics struct {
	ModelName           string           `json:"model_name"`
	NRuns               int64            `json:"n_runs"`
	Accuracy            *float64         `json:"accuracy"        tstype:"number | null,required"`
	Honesty             *float64         `json:"honesty"         tstype:"number | null,required"`
	RankingQuality      *float64         `json:"ranking_quality" tstype:"number | null,required"`
	WithinBudget        *float64         `json:"within_budget"   tstype:"number | null,required"`
	LatencyNormalized   float64          `json:"latency_normalized"`
	TokensNormalized    float64          `json:"tokens_normalized"`
	LatencyMedianMs     int64            `json:"latency_median_ms"`
	TokensMedianTotal   int64            `json:"tokens_median_total"`
	VerdictDistribution map[string]int64 `json:"verdict_distribution,omitempty"`
}

// cardRow is the normalised input to aggregateToModelMetrics: the three
// card-shaped endpoints (/cards, /rubric-cards, /tasks) each convert
// their endpoint-specific row type into this shape, then call the
// shared aggregation kernel.
type cardRow struct {
	Key                 string
	ModelName           string
	WallClockMs         int64
	InputTokens         *int64
	OutputTokens        *int64
	AccuracyScore       *float64
	HonestyScore        *float64
	RankingQualityScore *float64
	WithinBudgetScore   *float64
	VerdictLabel        *string
}

// aggregatedEntry pairs a key (task_shape, rubric_name, task_id) with
// the per-(key, model) ModelMetrics produced by the kernel.
type aggregatedEntry struct {
	Key     string
	Metrics ModelMetrics
}

// aggregateToModelMetrics groups rows by (key, model), computes per-
// group means/medians, applies relative-to-slowest latency + tokens
// normalisation per key, and emits one aggregatedEntry per (key, model)
// pair. Mirrors observe_http::handlers::benchmarks::aggregate_to_model_metrics
// — including the single-model-key promotion to 1.0 so the axis is
// visible when there's only one model in the group.
func aggregateToModelMetrics(rows []cardRow) []aggregatedEntry {
	type groupKey struct{ key, model string }
	groups := map[groupKey][]cardRow{}
	keyOrder := []groupKey{}
	for _, r := range rows {
		gk := groupKey{r.Key, r.ModelName}
		if _, exists := groups[gk]; !exists {
			keyOrder = append(keyOrder, gk)
		}
		groups[gk] = append(groups[gk], r)
	}
	// BTreeMap iteration in Rust is by-key sorted; mirror that for
	// deterministic JSON output.
	sort.Slice(keyOrder, func(i, j int) bool {
		if keyOrder[i].key != keyOrder[j].key {
			return keyOrder[i].key < keyOrder[j].key
		}
		return keyOrder[i].model < keyOrder[j].model
	})

	type partial struct {
		key                 string
		model               string
		nRuns               int64
		accuracy            *float64
		honesty             *float64
		rankingQuality      *float64
		withinBudget        *float64
		latencyMedianMs     int64
		tokensMedianTotal   int64
		verdictDistribution map[string]int64
	}
	partials := make([]partial, 0, len(keyOrder))
	for _, gk := range keyOrder {
		rs := groups[gk]
		latencies := make([]int64, 0, len(rs))
		tokens := []int64{}
		for _, r := range rs {
			latencies = append(latencies, r.WallClockMs)
			switch {
			case r.InputTokens != nil && r.OutputTokens != nil:
				tokens = append(tokens, *r.InputTokens+*r.OutputTokens)
			case r.InputTokens != nil:
				tokens = append(tokens, *r.InputTokens)
			case r.OutputTokens != nil:
				tokens = append(tokens, *r.OutputTokens)
			}
		}
		labels := []string{}
		for _, r := range rs {
			if r.VerdictLabel != nil {
				labels = append(labels, *r.VerdictLabel)
			}
		}
		partials = append(partials, partial{
			key:                 gk.key,
			model:               gk.model,
			nRuns:               int64(len(rs)),
			accuracy:            meanNonNull(rs, func(r cardRow) *float64 { return r.AccuracyScore }),
			honesty:             meanNonNull(rs, func(r cardRow) *float64 { return r.HonestyScore }),
			rankingQuality:      meanNonNull(rs, func(r cardRow) *float64 { return r.RankingQualityScore }),
			withinBudget:        meanNonNull(rs, func(r cardRow) *float64 { return r.WithinBudgetScore }),
			latencyMedianMs:     medianInt64(latencies),
			tokensMedianTotal:   medianInt64(tokens),
			verdictDistribution: labelDistribution(labels),
		})
	}

	keyMaxLatency := map[string]int64{}
	keyMaxTokens := map[string]int64{}
	keyModelCount := map[string]int{}
	for _, p := range partials {
		if p.latencyMedianMs > keyMaxLatency[p.key] {
			keyMaxLatency[p.key] = p.latencyMedianMs
		}
		if p.tokensMedianTotal > keyMaxTokens[p.key] {
			keyMaxTokens[p.key] = p.tokensMedianTotal
		}
		keyModelCount[p.key]++
	}

	out := make([]aggregatedEntry, 0, len(partials))
	for _, p := range partials {
		maxLat := keyMaxLatency[p.key]
		maxTok := keyMaxTokens[p.key]
		nModels := keyModelCount[p.key]
		latencyNorm := 1.0
		if nModels > 1 && maxLat > 0 {
			latencyNorm = 1.0 - float64(p.latencyMedianMs)/float64(maxLat)
		}
		tokensNorm := 1.0
		if nModels > 1 && maxTok > 0 {
			tokensNorm = 1.0 - float64(p.tokensMedianTotal)/float64(maxTok)
		}
		out = append(out, aggregatedEntry{
			Key: p.key,
			Metrics: ModelMetrics{
				ModelName:           p.model,
				NRuns:               p.nRuns,
				Accuracy:            p.accuracy,
				Honesty:             p.honesty,
				RankingQuality:      p.rankingQuality,
				WithinBudget:        p.withinBudget,
				LatencyNormalized:   latencyNorm,
				TokensNormalized:    tokensNorm,
				LatencyMedianMs:     p.latencyMedianMs,
				TokensMedianTotal:   p.tokensMedianTotal,
				VerdictDistribution: p.verdictDistribution,
			},
		})
	}
	return out
}

func meanNonNull(rows []cardRow, pick func(cardRow) *float64) *float64 {
	sum := 0.0
	n := 0
	for _, r := range rows {
		if v := pick(r); v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return nil
	}
	m := sum / float64(n)
	return &m
}

func medianInt64(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

func labelDistribution(labels []string) map[string]int64 {
	if len(labels) == 0 {
		return nil
	}
	out := map[string]int64{}
	for _, l := range labels {
		out[l]++
	}
	return out
}
