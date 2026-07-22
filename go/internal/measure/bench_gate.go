package measure

import (
	"fmt"
	"strings"

	"toolkit/internal/events"
)

// benchGateResult is the outcome of evaluating a harness's
// deterministic-metric gate over a completed diff.
//
//   - Configured: the harness declared a non-empty gate_metrics allowlist.
//     When false the bench is report-only (no pass/fail) — Passed is
//     meaningless and callers surface gate_passed as null.
//   - Passed: every matched gate metric had an exact-zero delta. Only
//     meaningful when Configured.
//   - MatchCount: how many of the diff's metrics matched a gate pattern.
//   - Failures: human-readable lines for the gate metrics that drifted (or
//     a single misconfiguration line when the patterns matched nothing).
type benchGateResult struct {
	Configured bool
	Passed     bool
	MatchCount int
	Failures   []string
}

// evaluateBenchGate computes the deterministic-metric gate. gate_metrics
// is a comma-separated allowlist of metric-name patterns; a metric
// matches a pattern when its name equals the pattern OR ends with
// "."+pattern (suffix gating for the "<shape>.<metric>" convention, so
// "n" gates every "<shape>.n"). A matched metric passes iff its delta_abs
// is exactly 0 — gate metrics are deterministic *by the harness author's
// declaration*, so any drift (or a nil delta from an appeared/disappeared
// metric) is a real regression. Jittery or live-data-dependent metrics
// (latency, most envelope sizes) are deliberately left OUT of the gate
// and remain in the diff as informational rows.
//
// Closes bug bench-diff-exact-equality-vs-live-data-defeats-regression-
// detection: the exact-equality diff couldn't gate a bench whose output
// drifts with live substrate; the allowlist narrows the pass/fail to the
// metrics that genuinely don't.
func evaluateBenchGate(gateMetrics string, metrics []events.BenchmarkMetricDiff) benchGateResult {
	patterns := splitGatePatterns(gateMetrics)
	if len(patterns) == 0 {
		return benchGateResult{Configured: false}
	}
	res := benchGateResult{Configured: true, Passed: true}
	for _, m := range metrics {
		if !metricMatchesGate(m.Name, patterns) {
			continue
		}
		res.MatchCount++
		if m.DeltaAbs == nil || *m.DeltaAbs != 0 {
			res.Passed = false
			res.Failures = append(res.Failures,
				fmt.Sprintf("%s (baseline=%s observed=%s)", m.Name, string(m.Baseline), string(m.Observed)))
		}
	}
	if res.MatchCount == 0 {
		// A non-empty gate that matches nothing is a misconfiguration — fail
		// loudly rather than vacuously pass (a silent no-op gate is worse
		// than no gate, because it reads green).
		res.Passed = false
		res.Failures = append(res.Failures,
			fmt.Sprintf("(gate_metrics %v matched none of the %d diff metrics — check the patterns)", patterns, len(metrics)))
	}
	return res
}

// splitGatePatterns parses the comma-separated gate_metrics into trimmed,
// non-empty pattern tokens.
func splitGatePatterns(gateMetrics string) []string {
	var out []string
	for _, p := range strings.Split(gateMetrics, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// metricMatchesGate reports whether a metric name is gated by any pattern:
// exact equality, or a "<shape>.<pattern>" suffix match.
func metricMatchesGate(name string, patterns []string) bool {
	for _, p := range patterns {
		if name == p || strings.HasSuffix(name, "."+p) {
			return true
		}
	}
	return false
}
