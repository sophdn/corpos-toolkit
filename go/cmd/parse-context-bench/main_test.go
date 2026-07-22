package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fixedResults is a deterministic two-shape sample used across the
// aggregate + byte-stability tests. "alpha" has one prompt (percentiles
// collapse to the single value); "beta" has two (exercises the linear-
// interpolation path).
func fixedResults() []promptResult {
	return []promptResult{
		{Shape: "alpha", Prompt: "a1", ResolutionTimeMs: 5, HTTPLatencyMs: 3, EnvelopeBytes: 100, HTTPStatus: 200},
		{Shape: "beta", Prompt: "b1", ResolutionTimeMs: 10, HTTPLatencyMs: 4, EnvelopeBytes: 200, HTTPStatus: 200},
		{Shape: "beta", Prompt: "b2", ResolutionTimeMs: 20, HTTPLatencyMs: 6, EnvelopeBytes: 400, HTTPStatus: 200},
	}
}

func TestPercentile(t *testing.T) {
	cases := []struct {
		name string
		in   []int
		p    int
		want int
	}{
		{"empty", nil, 50, 0},
		{"single", []int{42}, 99, 42},
		{"two-p50", []int{10, 20}, 50, 15},
		{"two-p99", []int{10, 20}, 99, 20},
		{"unsorted-input", []int{20, 10}, 50, 15},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := percentile(c.in, c.p); got != c.want {
				t.Fatalf("percentile(%v, %d) = %d, want %d", c.in, c.p, got, c.want)
			}
		})
	}
}

func TestComputeShapeStatsSortedByShape(t *testing.T) {
	stats := computeShapeStats(fixedResults())
	if len(stats) != 2 {
		t.Fatalf("want 2 shapes, got %d", len(stats))
	}
	if stats[0].Shape != "alpha" || stats[1].Shape != "beta" {
		t.Fatalf("shapes not sorted: %q, %q", stats[0].Shape, stats[1].Shape)
	}
	if stats[1].EnvelopeBytesP50 != 300 || stats[1].EnvelopeBytesMax != 400 {
		t.Fatalf("beta bytes p50/max = %d/%d, want 300/400", stats[1].EnvelopeBytesP50, stats[1].EnvelopeBytesMax)
	}
}

func TestBuildAggregateMap(t *testing.T) {
	got := buildAggregateMap(fixedResults())
	want := map[string]int{
		"alpha.n": 1, "alpha.resolution_p50_ms": 5, "alpha.resolution_p99_ms": 5,
		"alpha.http_p50_ms": 3, "alpha.http_p99_ms": 3,
		"alpha.envelope_bytes_p50": 100, "alpha.envelope_bytes_max": 100,
		"beta.n": 2, "beta.resolution_p50_ms": 15, "beta.resolution_p99_ms": 20,
		"beta.http_p50_ms": 5, "beta.http_p99_ms": 6,
		"beta.envelope_bytes_p50": 300, "beta.envelope_bytes_max": 400,
	}
	if len(got) != len(want) {
		t.Fatalf("key count = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
}

// TestWriteAggregateJSONGolden pins the exact bytes the aggregate file
// carries — this is the shape forge(bench) registers as a baseline and
// measure.bench_run diffs against, so drift here is a contract break. The
// 2-space indent + trailing newline match measure.bench_run's
// update_baseline writer.
func TestWriteAggregateJSONGolden(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agg.json")
	if err := writeAggregateJSON(path, fixedResults()); err != nil {
		t.Fatalf("writeAggregateJSON: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := `{
  "alpha.envelope_bytes_max": 100,
  "alpha.envelope_bytes_p50": 100,
  "alpha.http_p50_ms": 3,
  "alpha.http_p99_ms": 3,
  "alpha.n": 1,
  "alpha.resolution_p50_ms": 5,
  "alpha.resolution_p99_ms": 5,
  "beta.envelope_bytes_max": 400,
  "beta.envelope_bytes_p50": 300,
  "beta.http_p50_ms": 5,
  "beta.http_p99_ms": 6,
  "beta.n": 2,
  "beta.resolution_p50_ms": 15,
  "beta.resolution_p99_ms": 20
}
`
	if string(got) != want {
		t.Fatalf("aggregate bytes mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWriteJSONByteStable guards the existing --out-json contract: the
// per-prompt array shape must not drift as the aggregate feature is added.
func TestWriteJSONByteStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raw.json")
	if err := writeJSON(path, fixedResults()[:1]); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := `[
  {
    "shape": "alpha",
    "prompt": "a1",
    "envelope_bytes": 100,
    "resolution_time_ms": 5,
    "http_latency_ms": 3,
    "http_status": 200
  }
]`
	if string(got) != want {
		t.Fatalf("raw json bytes mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
