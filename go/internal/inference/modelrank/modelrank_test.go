package modelrank

import "testing"

const dflt = "qwen2.5-32b"

// Basic rank() sanity (chain data-driven-model-routing T4). The exhaustive
// boundary matrix (warmup edge, margin edge, latency tie-break) + the cache +
// DB-backed Select live in the T5 densification net.

// Cold start: the default model has no warmed row → stay on the default.
func TestRank_ColdStartStaysDefault(t *testing.T) {
	// One non-default model warmed, but the default itself is below warmup.
	rows := []ModelStat{
		{ModelName: dflt, CallCount: 5, OutcomeSuccessCount: 5},                  // below warmup
		{ModelName: "claude-sonnet-4-6", CallCount: 50, OutcomeSuccessCount: 50}, // warmed, perfect
	}
	if got, switched := rank(rows, dflt); got != dflt || switched {
		t.Errorf("rank = (%q,%v), want (%q,false) — default not warmed is cold-start", got, switched, dflt)
	}
}

// Empty rows (no telemetry at all) → default, not switched.
func TestRank_NoRowsStaysDefault(t *testing.T) {
	if got, switched := rank(nil, dflt); got != dflt || switched {
		t.Errorf("rank(nil) = (%q,%v), want (%q,false)", got, switched, dflt)
	}
}

// A warmed candidate that MATERIALLY beats the warmed default switches.
func TestRank_MaterialWinnerSwitches(t *testing.T) {
	rows := []ModelStat{
		{ModelName: dflt, CallCount: 100, OutcomeSuccessCount: 70},                // 0.70
		{ModelName: "claude-sonnet-4-6", CallCount: 100, OutcomeSuccessCount: 90}, // 0.90 ≥ 0.70+0.10
	}
	if got, switched := rank(rows, dflt); got != "claude-sonnet-4-6" || !switched {
		t.Errorf("rank = (%q,%v), want (claude-sonnet-4-6,true)", got, switched)
	}
}

// A warmed candidate that beats the default by LESS than the margin does NOT
// switch — the cost-asymmetry guard keeps the free local default.
func TestRank_ThinEdgeStaysDefault(t *testing.T) {
	rows := []ModelStat{
		{ModelName: dflt, CallCount: 100, OutcomeSuccessCount: 80},                // 0.80
		{ModelName: "claude-sonnet-4-6", CallCount: 100, OutcomeSuccessCount: 88}, // 0.88 < 0.80+0.10
	}
	if got, switched := rank(rows, dflt); got != dflt || switched {
		t.Errorf("rank = (%q,%v), want (%q,false) — thin edge must not displace the free default", got, switched, dflt)
	}
}

// ── T5 densification: the data-present ranking boundaries ─────────────────

// Warmup boundary on the DEFAULT model: at exactly warmupMinCalls (20) the
// default is warmed and a material candidate displaces it; at 19 the default is
// cold so there is no trustworthy basis to route away — stay default.
func TestRank_DefaultWarmupBoundary(t *testing.T) {
	winner := ModelStat{ModelName: "claude-sonnet-4-6", CallCount: 100, OutcomeSuccessCount: 100} // 1.0
	t.Run("default_warmed_at_20_switches", func(t *testing.T) {
		rows := []ModelStat{{ModelName: dflt, CallCount: 20, OutcomeSuccessCount: 10}, winner} // default 0.50
		if got, switched := rank(rows, dflt); got != "claude-sonnet-4-6" || !switched {
			t.Errorf("rank = (%q,%v), want (claude-sonnet-4-6,true) — default warmed at exactly 20", got, switched)
		}
	})
	t.Run("default_below_warmup_at_19_stays", func(t *testing.T) {
		rows := []ModelStat{{ModelName: dflt, CallCount: 19, OutcomeSuccessCount: 10}, winner}
		if got, switched := rank(rows, dflt); got != dflt || switched {
			t.Errorf("rank = (%q,%v), want (%q,false) — default below warmup is cold-start", got, switched, dflt)
		}
	})
}

// Quality-margin boundary: a candidate at EXACTLY dQ+0.10 switches (the `>=`);
// one a hair below stays. Default quality is 0.70 (70/100).
func TestRank_QualityMarginBoundary(t *testing.T) {
	def := ModelStat{ModelName: dflt, CallCount: 100, OutcomeSuccessCount: 70} // 0.70
	t.Run("exactly_at_margin_switches", func(t *testing.T) {
		rows := []ModelStat{def, {ModelName: "claude-sonnet-4-6", CallCount: 100, OutcomeSuccessCount: 80}} // 0.80 == 0.70+0.10
		if got, switched := rank(rows, dflt); got != "claude-sonnet-4-6" || !switched {
			t.Errorf("rank = (%q,%v), want (claude-sonnet-4-6,true) — 0.80 meets the >= 0.80 margin", got, switched)
		}
	})
	t.Run("just_below_margin_stays", func(t *testing.T) {
		rows := []ModelStat{def, {ModelName: "claude-sonnet-4-6", CallCount: 100, OutcomeSuccessCount: 79}} // 0.79 < 0.80
		if got, switched := rank(rows, dflt); got != dflt || switched {
			t.Errorf("rank = (%q,%v), want (%q,false) — 0.79 is below the margin", got, switched, dflt)
		}
	})
}

// Among multiple margin-clearing candidates, the highest quality wins.
func TestRank_HighestQualityAmongClearers(t *testing.T) {
	rows := []ModelStat{
		{ModelName: dflt, CallCount: 100, OutcomeSuccessCount: 60},      // 0.60
		{ModelName: "model-a", CallCount: 100, OutcomeSuccessCount: 75}, // 0.75 ≥ 0.70
		{ModelName: "model-b", CallCount: 100, OutcomeSuccessCount: 95}, // 0.95 — best
		{ModelName: "model-c", CallCount: 100, OutcomeSuccessCount: 72}, // 0.72 ≥ 0.70
	}
	if got, switched := rank(rows, dflt); got != "model-b" || !switched {
		t.Errorf("rank = (%q,%v), want (model-b,true) — highest quality among margin-clearers", got, switched)
	}
}

// Tie on quality among margin-clearers → lower average latency wins.
func TestRank_LatencyTieBreak(t *testing.T) {
	rows := []ModelStat{
		{ModelName: dflt, CallCount: 100, OutcomeSuccessCount: 50},                                  // 0.50
		{ModelName: "model-slow", CallCount: 100, OutcomeSuccessCount: 80, TotalLatencyMS: 200_000}, // 0.80, avg 2000ms
		{ModelName: "model-fast", CallCount: 100, OutcomeSuccessCount: 80, TotalLatencyMS: 100_000}, // 0.80, avg 1000ms
	}
	if got, switched := rank(rows, dflt); got != "model-fast" || !switched {
		t.Errorf("rank = (%q,%v), want (model-fast,true) — equal quality breaks to lower latency", got, switched)
	}
}

// A high-quality candidate BELOW warmup is ignored (not yet trustworthy).
func TestRank_SubWarmupCandidateIgnored(t *testing.T) {
	rows := []ModelStat{
		{ModelName: dflt, CallCount: 100, OutcomeSuccessCount: 70},               // 0.70 warmed
		{ModelName: "claude-sonnet-4-6", CallCount: 10, OutcomeSuccessCount: 10}, // 1.0 but only 10 calls
	}
	if got, switched := rank(rows, dflt); got != dflt || switched {
		t.Errorf("rank = (%q,%v), want (%q,false) — sub-warmup candidate must be ignored", got, switched, dflt)
	}
}

// Degenerate outcome data (a task with no ground-truth join → all-zero
// outcome_success_count) yields quality 0 for every model, so the margin never
// clears and the free default holds — the safe behavior.
func TestRank_AllZeroOutcomeStaysDefault(t *testing.T) {
	rows := []ModelStat{
		{ModelName: dflt, CallCount: 100, OutcomeSuccessCount: 0},
		{ModelName: "claude-sonnet-4-6", CallCount: 100, OutcomeSuccessCount: 0},
	}
	if got, switched := rank(rows, dflt); got != dflt || switched {
		t.Errorf("rank = (%q,%v), want (%q,false) — all-zero outcome must hold the default", got, switched, dflt)
	}
}
