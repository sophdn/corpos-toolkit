package observehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"toolkit/internal/testutil"
)

// Bug 1452: GET /admin/parse-context-skip-rate is the structural
// visibility surface for the parse-context-first-call reflex compliance.
// These tests pin the wire shape and the metric semantics:
//   - observed_prompts = distinct prompt_ids with any activity in window
//   - with_parse_context = distinct prompt_ids that also had a
//     reference_resolution row (the sole query_source the resolution
//     path emits, for both the parse_context and resolve_references
//     actions)
//   - skip_rate = 1 - (with_parse_context / observed_prompts)
//   - skipped_prompt_ids = the cohort of observed-without-parse,
//     capped at 20 for response-size sanity

func TestParseContextSkipRate_ZeroPromptsZeroRate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)

	var resp parseContextSkipRateResponse
	if code := getJSON(t, srv, "/admin/parse-context-skip-rate", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.ObservedPrompts != 0 {
		t.Errorf("observed_prompts = %d, want 0", resp.ObservedPrompts)
	}
	if resp.SkipRate != 0 {
		t.Errorf("skip_rate = %v, want 0 (nothing observed → no skip)", resp.SkipRate)
	}
	if resp.WindowHours != 24 {
		t.Errorf("window_hours = %d, want 24 (default)", resp.WindowHours)
	}
}

func TestParseContextSkipRate_MixedSkipAndFire(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Three prompts in the last hour:
	//   p-skipped-1 has only agent_initiated activity (skip)
	//   p-skipped-2 has only harness_reminder_interception (skip)
	//   p-fired    has a reference_resolution fire (no skip)
	//
	// The resolution path emits query_source='reference_resolution' for
	// both the parse_context and resolve_references actions
	// (refresolve/grounding_emit.go). 'parse_context' is the action NAME,
	// not a query_source value — the grounding_events.query_source CHECK
	// does not admit it — so the skip-rate SQL filters on
	// 'reference_resolution' alone.
	_, err := pool.DB().Exec(`
		INSERT INTO grounding_events
		  (project_id, session_id, call_id, action, results_count, source_refs,
		   next_turn_has_output, used, span_id, prompt_id, query_source, created_at)
		VALUES
		  ('p', 's1', 'c1', 'kiwix_search',  0, '[]', 0, NULL, '', 'p-skipped-1', 'agent_initiated', datetime('now', '-10 minutes')),
		  ('p', 's1', 'c2', 'kiwix_search',  0, '[]', 0, NULL, '', 'p-skipped-2', 'harness_reminder_interception', datetime('now', '-5 minutes')),
		  ('p', 's1', 'c3', 'parse_context', 0, '[]', 0, NULL, '', 'p-fired',     'reference_resolution', datetime('now', '-1 minutes')),
		  ('p', 's1', 'c4', 'kiwix_search',  0, '[]', 0, NULL, '', 'p-fired',     'agent_initiated', datetime('now', '-30 seconds'))
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)

	var resp parseContextSkipRateResponse
	if code := getJSON(t, srv, "/admin/parse-context-skip-rate", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.ObservedPrompts != 3 {
		t.Errorf("observed_prompts = %d, want 3", resp.ObservedPrompts)
	}
	if resp.WithParseContext != 1 {
		t.Errorf("with_parse_context = %d, want 1", resp.WithParseContext)
	}
	wantRate := 2.0 / 3.0
	if resp.SkipRate < wantRate-0.001 || resp.SkipRate > wantRate+0.001 {
		t.Errorf("skip_rate = %v, want ~%v", resp.SkipRate, wantRate)
	}
	if len(resp.SkippedPromptIDs) != 2 {
		t.Errorf("skipped_prompt_ids = %v, want 2 entries", resp.SkippedPromptIDs)
	}
	skipped := map[string]bool{}
	for _, p := range resp.SkippedPromptIDs {
		skipped[p] = true
	}
	if !skipped["p-skipped-1"] || !skipped["p-skipped-2"] {
		t.Errorf("missing expected skipped prompts in %v", resp.SkippedPromptIDs)
	}
	if skipped["p-fired"] {
		t.Errorf("p-fired listed as skipped — it had a parse_context row")
	}
}

func TestParseContextSkipRate_ReferenceResolutionCounted(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// reference_resolution is the query_source the resolution path stamps
	// for both the parse_context and resolve_references actions. The
	// metric must count it as a "parse-context fired" signal.
	_, err := pool.DB().Exec(`
		INSERT INTO grounding_events
		  (project_id, session_id, call_id, action, results_count, source_refs,
		   next_turn_has_output, used, span_id, prompt_id, query_source, created_at)
		VALUES
		  ('p', 's', 'c1', 'resolve_references', 0, '[]', 0, NULL, '', 'alias-test', 'reference_resolution', datetime('now', '-1 minutes'))
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)

	var resp parseContextSkipRateResponse
	if code := getJSON(t, srv, "/admin/parse-context-skip-rate", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.WithParseContext != 1 {
		t.Errorf("reference_resolution row should count as parse-context fired; with_parse_context=%d", resp.WithParseContext)
	}
	if resp.SkipRate != 0 {
		t.Errorf("skip_rate = %v, want 0 (only prompt had a fire)", resp.SkipRate)
	}
}

func TestParseContextSkipRate_WindowParameterClamps(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Row older than 1 hour, within 24 hours.
	_, err := pool.DB().Exec(`
		INSERT INTO grounding_events
		  (project_id, session_id, call_id, action, results_count, source_refs,
		   next_turn_has_output, used, span_id, prompt_id, query_source, created_at)
		VALUES
		  ('p', 's', 'c1', 'kiwix_search', 0, '[]', 0, NULL, '', 'old', 'agent_initiated', datetime('now', '-3 hours'))
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(BuildRouter(AppState{Pool: pool}))
	t.Cleanup(srv.Close)

	// 1-hour window excludes the 3-hour-old row.
	var narrow parseContextSkipRateResponse
	getJSON(t, srv, "/admin/parse-context-skip-rate?hours=1", &narrow)
	if narrow.ObservedPrompts != 0 {
		t.Errorf("hours=1: observed = %d, want 0 (row is 3h old)", narrow.ObservedPrompts)
	}
	if narrow.WindowHours != 1 {
		t.Errorf("hours=1: window_hours echoed as %d", narrow.WindowHours)
	}

	// 24-hour window includes it.
	var wide parseContextSkipRateResponse
	getJSON(t, srv, "/admin/parse-context-skip-rate?hours=24", &wide)
	if wide.ObservedPrompts != 1 {
		t.Errorf("hours=24: observed = %d, want 1", wide.ObservedPrompts)
	}
}

// Pool-disabled boots don't register the route — the ServeMux returns
// 404. Mirrors every other DB-backed endpoint's degraded behaviour
// (the router gates all Pool-dependent routes behind `if state.Pool !=
// nil` so degraded boots fail closed at the routing layer, not in each
// handler).
func TestParseContextSkipRate_404WhenPoolMissing(t *testing.T) {
	srv := httptest.NewServer(BuildRouter(AppState{}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/admin/parse-context-skip-rate")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
