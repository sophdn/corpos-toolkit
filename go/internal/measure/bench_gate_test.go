package measure

import (
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/events"
)

func fp(f float64) *float64 { return &f }

// metric builds a BenchmarkMetricDiff with a numeric baseline/observed +
// delta. A nil delta models a missing/non-numeric side.
func metric(name string, delta *float64) events.BenchmarkMetricDiff {
	return events.BenchmarkMetricDiff{
		Name:     name,
		Baseline: json.RawMessage("0"),
		Observed: json.RawMessage("0"),
		DeltaAbs: delta,
	}
}

func TestEvaluateBenchGate_Unconfigured_NotGated(t *testing.T) {
	res := evaluateBenchGate("", []events.BenchmarkMetricDiff{metric("audit.n", fp(0))})
	if res.Configured {
		t.Fatalf("empty gate_metrics must be Configured=false, got %+v", res)
	}
}

func TestEvaluateBenchGate_AllZeroDelta_Passes(t *testing.T) {
	metrics := []events.BenchmarkMetricDiff{
		metric("audit.n", fp(0)),
		metric("fix.n", fp(0)),
		metric("none.envelope_bytes_p50", fp(0)),
		// a drifting NON-gate metric must NOT affect the gate
		metric("audit.envelope_bytes_p50", fp(1660)),
		metric("audit.http_p50_ms", fp(5)),
	}
	res := evaluateBenchGate("n,none.envelope_bytes_p50", metrics)
	if !res.Configured || !res.Passed {
		t.Fatalf("want Configured+Passed, got %+v", res)
	}
	if res.MatchCount != 3 {
		t.Errorf("want 3 matched (audit.n, fix.n, none.envelope_bytes_p50), got %d", res.MatchCount)
	}
	if len(res.Failures) != 0 {
		t.Errorf("want no failures, got %v", res.Failures)
	}
}

func TestEvaluateBenchGate_GateMetricDrifts_Fails(t *testing.T) {
	metrics := []events.BenchmarkMetricDiff{
		metric("audit.n", fp(0)),
		metric("fix.n", fp(-1)), // a shape lost a prompt — corpus regression
		metric("audit.envelope_bytes_p50", fp(1660)),
	}
	res := evaluateBenchGate("n", metrics)
	if !res.Configured || res.Passed {
		t.Fatalf("want Configured + NOT passed, got %+v", res)
	}
	if res.MatchCount != 2 {
		t.Errorf("want 2 matched (*.n), got %d", res.MatchCount)
	}
	if len(res.Failures) != 1 || !strings.Contains(res.Failures[0], "fix.n") {
		t.Errorf("failures should name fix.n, got %v", res.Failures)
	}
}

func TestEvaluateBenchGate_MissingDelta_Fails(t *testing.T) {
	// A gate metric present in only one side (delta nil) means the shape
	// appeared/disappeared — a structural regression the gate must catch.
	metrics := []events.BenchmarkMetricDiff{metric("audit.n", nil)}
	res := evaluateBenchGate("n", metrics)
	if res.Passed {
		t.Fatalf("nil-delta gate metric must fail the gate, got %+v", res)
	}
}

func TestEvaluateBenchGate_SuffixVsExactMatching(t *testing.T) {
	metrics := []events.BenchmarkMetricDiff{
		metric("audit.n", fp(0)),                  // matches suffix "n"
		metric("audit.http_p50_ms", fp(99)),       // must NOT match "n"
		metric("none.envelope_bytes_p50", fp(0)),  // matches exact
		metric("audit.envelope_bytes_p50", fp(7)), // must NOT match the exact none.* pattern
	}
	res := evaluateBenchGate("n,none.envelope_bytes_p50", metrics)
	if res.MatchCount != 2 {
		t.Errorf("want exactly 2 matched (audit.n + none.envelope_bytes_p50), got %d", res.MatchCount)
	}
	if !res.Passed {
		t.Errorf("the two matched gate metrics are zero-delta; want pass, got %+v", res)
	}
}

func TestEvaluateBenchGate_PatternsMatchNothing_FailsLoudly(t *testing.T) {
	// A non-empty gate that matches zero metrics is a misconfiguration —
	// fail loudly rather than vacuously pass (silent no-op gate).
	metrics := []events.BenchmarkMetricDiff{metric("audit.envelope_bytes_p50", fp(0))}
	res := evaluateBenchGate("nonexistent_metric", metrics)
	if res.Passed {
		t.Fatalf("zero-match gate must NOT pass, got %+v", res)
	}
	if res.MatchCount != 0 || len(res.Failures) == 0 {
		t.Errorf("want MatchCount=0 + a failure explaining the misconfig, got %+v", res)
	}
}
