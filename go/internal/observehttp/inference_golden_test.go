package observehttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// Characterization-net golden harness for the /inference/* endpoints
// (chain per-tool-per-model-observability T9 — the refactor GATE).
//
// These goldens pin the EXACT JSON the three inference endpoints emit
// today (reading inference_invocations + the read-time success-predicate SQL)
// over a single deterministic, combinatorially-complete fixture. They are
// the parity oracle the later relocation steps (T11 inference_invocations
// write path, T12 inference_tool_model_performance projection + endpoint
// repoint) must reproduce byte-for-byte. If a relocation changes a number,
// the golden mismatch is the proof — exactly the "consolidation loses no
// metric" guarantee the program plan (docs/TELEMETRY_CONSOLIDATION.md §6)
// promises a non-analyst reviewer.
//
// Determinism strategy. The endpoints window on time.Now() and emit
// absolute timestamps (last_call_at) and per-day bucket dates, so a raw
// byte-golden would drift every run. Two moves make it reproducible:
//   - Fixtures seed relative to a captured base, and day-groups land at
//     exact base.AddDate(0,0,-k) — in UTC that shifts the calendar date by
//     exactly k days regardless of wall-clock time-of-day, so date() bucket
//     boundaries never straddle midnight (the one flakiness the existing
//     per-day tests sidestep by asserting counts, not dates).
//   - Before comparison, every RFC3339 timestamp and bare YYYY-MM-DD date
//     in the response is replaced by a stable rank token (<TS0> = most
//     recent, <DATE0> = most recent day, …). Rank encodes the only
//     semantic content these fields carry for the endpoints (recency /
//     ordering); the absolute instant is wall-clock noise.
//
// Regenerate after an intentional, reviewed behavior change with
// UPDATE_GOLDEN=1 go test -tags sqlite_fts5 ./internal/observehttp/ -run Golden

var (
	goldenTSRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$`)
	goldenDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// collectTimeStrings walks a decoded JSON value and records every leaf
// string that looks like an RFC3339 timestamp or a bare date.
func collectTimeStrings(v any, ts, dates map[string]struct{}) {
	switch x := v.(type) {
	case map[string]any:
		for _, val := range x {
			collectTimeStrings(val, ts, dates)
		}
	case []any:
		for _, val := range x {
			collectTimeStrings(val, ts, dates)
		}
	case string:
		switch {
		case goldenTSRe.MatchString(x):
			ts[x] = struct{}{}
		case goldenDateRe.MatchString(x):
			dates[x] = struct{}{}
		}
	}
}

// assignRankTokens maps each distinct value to "<prefix><rank>" where rank
// 0 is the most recent. RFC3339 (all UTC 'Z' here) and ISO dates sort
// lexically == chronologically, so a descending string sort yields
// recency order.
func assignRankTokens(set map[string]struct{}, prefix string, out map[string]string) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })
	for i, k := range keys {
		out[k] = fmt.Sprintf("<%s%d>", prefix, i)
	}
}

// substituteStrings returns a copy of v with every string leaf present in
// repl swapped for its token.
func substituteStrings(v any, repl map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = substituteStrings(val, repl)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = substituteStrings(val, repl)
		}
		return out
	default:
		if s, ok := v.(string); ok {
			if r, ok := repl[s]; ok {
				return r
			}
		}
		return v
	}
}

// canonicalizeGolden decodes raw endpoint JSON, replaces time-relative
// fields with rank tokens, and re-marshals it indented for a stable,
// reviewable golden.
func canonicalizeGolden(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonicalize: unmarshal: %v\nbody=%s", err, raw)
	}
	ts := map[string]struct{}{}
	dates := map[string]struct{}{}
	collectTimeStrings(v, ts, dates)
	repl := map[string]string{}
	assignRankTokens(ts, "TS", repl)
	assignRankTokens(dates, "DATE", repl)
	v = substituteStrings(v, repl)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("canonicalize: marshal: %v", err)
	}
	return out
}

// assertGolden compares canonicalized endpoint JSON to a committed golden
// file. UPDATE_GOLDEN=1 (re)writes it instead — the deliberate, reviewed
// "behavior intentionally changed" path.
func assertGolden(t *testing.T, name string, raw []byte) {
	t.Helper()
	got := canonicalizeGolden(t, raw)
	path := filepath.Join("testdata", "inference_golden", name+".json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("UPDATE_GOLDEN: wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(got, "\n")) {
		t.Errorf("golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

// getRawJSON issues the GET and returns the raw body, asserting 200.
func getRawJSON(t *testing.T, srv *httptest.Server, path string) []byte {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body=%s", path, resp.StatusCode, body)
	}
	return body
}

// seedBugWithQwenTask inserts a proj_current_bugs row attributed to a
// qwen_task_id so the health-cards bug_count join has something to count.
// SeedBug doesn't expose qwen_task_id, and the join is the whole point of
// the field, so the fixture writes the column directly.
func seedBugWithQwenTask(t *testing.T, pool *db.Pool, project, slug, qwenTaskID string) {
	t.Helper()
	if _, err := pool.DB().Exec(
		`INSERT INTO proj_current_bugs
			(id, slug, project_id, title, status, qwen_task_id, filed_at, updated_at)
		 VALUES ((SELECT COALESCE(MAX(id), 0) + 1 FROM proj_current_bugs),
		         ?, ?, ?, 'open', ?, datetime('now'), datetime('now'))`,
		slug, project, "T:"+slug, qwenTaskID,
	); err != nil {
		t.Fatalf("seedBugWithQwenTask %q: %v", slug, err)
	}
}

// seedBenchmarkAccuracy seeds the provenance + proj_benchmark_results rows
// the classify_* success predicate joins against, with a given accuracy.
func seedBenchmarkAccuracy(t *testing.T, pool *db.Pool, project, taskID string, accuracy float64, runAt time.Time) {
	t.Helper()
	provID := "prov-" + taskID
	if _, err := pool.DB().Exec(`INSERT INTO benchmark_provenance
		(id, run_id, model_id, model_version, prompt_template_hash, corpus_hash,
		 retriever_version, retriever_config_hash, seed, env_hash, started_event_id)
		VALUES (?, ?, 'qwen', 'v1', 'p', 'c', 'r', 'rc', 0, 'e', ?)`,
		provID, "run-"+taskID, "ev-"+taskID); err != nil {
		t.Fatalf("seed provenance %q: %v", taskID, err)
	}
	if _, err := pool.DB().Exec(`INSERT INTO proj_benchmark_results
		(id, project_id, scenario_id, tool_name, model_name, run_at, wall_clock_ms,
		 invocation_ok, task_id, accuracy_score, provenance_id)
		VALUES (?, ?, 's', ?, 'qwen', ?, 100, 1, ?, ?, ?)`,
		"br-"+taskID, project, taskID, runAt.Unix(), taskID, accuracy, provID); err != nil {
		t.Fatalf("seed proj_benchmark_results %q: %v", taskID, err)
	}
}

// TestGolden_InferenceHealthCards pins the health-cards endpoint over a
// fixture that crosses the endpoint's input equivalence classes in one
// shot: call-count tiers (warming <20, computed ≥20, p99-computed ≥100),
// single vs multi model, tokens present vs absent, every success-predicate
// class that produces a card (default mixed, classify hit, vault-rerank
// hit), the bug_count join, the multi-day sparkline-warming flag, and the
// cross-task staleness sort. Rejection predicate classes (classify miss /
// no-benchmark, vault-rerank miss) and the exact warmup boundaries are
// pinned in focused tests below.
func TestGolden_InferenceHealthCards(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p")

	vaultNow := time.Now().UTC()
	// base sits a clear hour before vaultNow so every day(k) group is
	// strictly older than the now-anchored vault-rerank rows (which must
	// stay within the predicate's proximity window of their grounding
	// row), giving an unambiguous staleness order.
	base := vaultNow.Add(-time.Hour).Truncate(time.Minute)
	day := func(k int) time.Time { return base.AddDate(0, 0, -k) }

	// vault-rerank-retrieve: 25 rows at ~now + one proximate vault_search
	// grounding row → predicate hit, success_rate 1.0. Newest task.
	for i := 0; i < 25; i++ {
		seedQwenWithTime(t, pool, "vault-rerank-retrieve", "qwen2.5-32b", 1000, nil, nil, vaultNow)
	}
	testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingAction("vault_search"),
		testutil.WithGroundingResultsCount(3),
	)

	// bravo-healthy-100: 100 all-success rows spread over 3 distinct days
	// → p99 computed (≥100) AND sparkline-warming false (dayCount ≥ 3).
	in, out := int64(20), int64(50)
	for i := 0; i < 100; i++ {
		seedQwenWithTime(t, pool, "bravo-healthy-100", "qwen2.5-32b", int64(100+i), &in, &out, day(1+i%3))
	}

	// aaa-default-mixed: 25 rows, default predicate, even success / odd
	// fail (latency 0, no output) → success_rate 13/25, p99 warming. Two
	// bugs attributed → bug_count 2.
	for i := 0; i < 25; i++ {
		if i%2 == 0 {
			seedQwenWithTime(t, pool, "aaa-default-mixed", "qwen2.5-32b", int64(100+i), &in, &out, day(4))
		} else {
			seedQwenWithTime(t, pool, "aaa-default-mixed", "qwen2.5-32b", 0, nil, nil, day(4))
		}
	}
	seedBugWithQwenTask(t, pool, "p", "bug-aaa-1", "aaa-default-mixed")
	seedBugWithQwenTask(t, pool, "p", "bug-aaa-2", "aaa-default-mixed")

	// charlie-multimodel: 5 rows, two models → multi-model breakdown with
	// per-model p95; <20 calls → both warmup flags set.
	seedQwenWithTime(t, pool, "charlie-multimodel", "qwen2.5-32b", 100, nil, nil, day(5))
	seedQwenWithTime(t, pool, "charlie-multimodel", "qwen2.5-32b", 150, nil, nil, day(5))
	seedQwenWithTime(t, pool, "charlie-multimodel", "qwen2.5-32b", 200, nil, nil, day(5))
	seedQwenWithTime(t, pool, "charlie-multimodel", "claude-opus", 500, nil, nil, day(5))
	seedQwenWithTime(t, pool, "charlie-multimodel", "claude-opus", 600, nil, nil, day(5))

	// classify_delta: 25 rows + a 0.9-accuracy benchmark → classify
	// predicate hit, success_rate 1.0. Oldest task.
	seedBenchmarkAccuracy(t, pool, "p", "classify_delta", 0.9, vaultNow)
	for i := 0; i < 25; i++ {
		seedQwenWithTime(t, pool, "classify_delta", "qwen2.5-32b", int64(100+i), nil, nil, day(6))
	}

	srv := newTestServer(t, pool)
	assertGolden(t, "health_cards", getRawJSON(t, srv, "/inference/health-cards"))
}

// TestGolden_InferenceSparklines pins the per-day bucket series: a task
// with three days of activity crossing the 5-call/day floor (≥5 →
// p95+success_rate populated; <5 → both NULL), mixed success so the
// per-bucket success_rate is a non-trivial fraction, summed tokens_burned,
// and a second task so the multi-series shape (ordered by task_id) is
// pinned.
func TestGolden_InferenceSparklines(t *testing.T) {
	pool := testutil.NewTestDB(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	day := func(k int) time.Time { return base.AddDate(0, 0, -k) }
	in, out := int64(10), int64(40)

	// multi-day-task:
	//   day0: 10 calls, 7 success / 3 fail → success_rate 0.7, p95 set.
	//   day1: 6 calls, all success → success_rate 1.0, p95 set.
	//   day5: 3 calls (<5 floor) → p95 + success_rate NULL.
	for i := 0; i < 10; i++ {
		if i < 7 {
			seedQwenWithTime(t, pool, "multi-day-task", "qwen2.5-32b", int64(100+i), &in, &out, day(0))
		} else {
			seedQwenWithTime(t, pool, "multi-day-task", "qwen2.5-32b", 0, nil, nil, day(0))
		}
	}
	for i := 0; i < 6; i++ {
		seedQwenWithTime(t, pool, "multi-day-task", "qwen2.5-32b", int64(200+i), &in, &out, day(1))
	}
	for i := 0; i < 3; i++ {
		seedQwenWithTime(t, pool, "multi-day-task", "qwen2.5-32b", int64(300+i), &in, &out, day(5))
	}

	// zzz-second-task: a single day, 5 calls → exercises the multi-series
	// list (sorted by task_id, so it follows multi-day-task).
	for i := 0; i < 5; i++ {
		seedQwenWithTime(t, pool, "zzz-second-task", "qwen2.5-32b", int64(50+i), &in, &out, day(0))
	}

	srv := newTestServer(t, pool)
	assertGolden(t, "sparklines", getRawJSON(t, srv, "/inference/sparklines?window_days=7"))
}

// TestGolden_InferenceRetrievalHealth pins the retrieval-health aggregate
// across all three retrieval actions and all four click-kind tiers
// (including resolved-from, which no existing test exercises), the
// weighted-score arithmetic, the warmup floor, the most-active-first
// action sort, and the followed→cited→mentioned→resolved-from by_kind
// order. This endpoint emits no timestamps, so the golden is fully literal.
func TestGolden_InferenceRetrievalHealth(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()

	// kiwix_search: 30 grounding rows (most active → sorts first), a few
	// followed interactions.
	for i := 0; i < 30; i++ {
		span := fmt.Sprintf("span-ks-%d", i)
		gID := seedGroundingForRetrieval(t, pool, "kiwix_search", span, now.Add(-time.Duration(i)*time.Minute))
		if i < 10 {
			seedInteraction(t, pool, gID, span, "ref-"+span, "followed", 1.0)
		}
	}

	// vault_search: 20 grounding rows, all four click-kind tiers.
	for i := 0; i < 20; i++ {
		span := fmt.Sprintf("span-vs-%d", i)
		gID := seedGroundingForRetrieval(t, pool, "vault_search", span, now.Add(-time.Duration(i)*time.Minute))
		switch {
		case i < 8:
			seedInteraction(t, pool, gID, span, "ref-"+span, "followed", 1.0)
		case i < 9:
			seedInteraction(t, pool, gID, span, "ref-"+span, "cited", 0.8)
		case i < 12:
			seedInteraction(t, pool, gID, span, "ref-"+span, "mentioned", 0.4)
		case i < 14:
			seedInteraction(t, pool, gID, span, "ref-"+span, "resolved-from", 1.0)
		}
	}

	// knowledge_search: 12 grounding rows → exercises the third action
	// (untested before today). Above the 10-row floor → not warming.
	for i := 0; i < 12; i++ {
		span := fmt.Sprintf("span-kn-%d", i)
		gID := seedGroundingForRetrieval(t, pool, "knowledge_search", span, now.Add(-time.Duration(i)*time.Minute))
		if i < 4 {
			seedInteraction(t, pool, gID, span, "ref-"+span, "cited", 0.8)
		}
	}

	srv := newTestServer(t, pool)
	assertGolden(t, "retrieval_health", getRawJSON(t, srv, "/inference/retrieval-health"))
}
