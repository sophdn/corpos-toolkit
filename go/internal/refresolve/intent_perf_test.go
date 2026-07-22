package refresolve_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"toolkit/internal/events"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// Perf gate per T5 acceptance criteria: parse_context p50 resolution
// time on a 20-prompt representative fixture must not regress
// meaningfully because of intent detection (T4 §13.5 constraint:
// pattern-first because Qwen-rubric would add ~300-500ms per call).
//
// The detector is pure regex with no DB / network hits; sub-100μs
// typical. This test asserts the END-TO-END p50 (including the rest
// of parse_context, the catalogs load, and the cache split) stays
// under a generous 50ms ceiling — well above the ~10ms baseline.
// A regression here means either the intent pass started doing
// substantive work or one of the existing surfaces drifted; either
// way the on-call signal fires before the live envelope leaks the
// latency.
func TestHandleParseContext_IntentPerfGate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	deps := refresolve.HandlerDeps{
		Pool:             pool,
		Project:          "mcp-servers",
		Registry:         refresolve.NewRegistry(),
		Cache:            refresolve.NewParseContextCache(),
		DriftFireTracker: refresolve.NewDriftFireTracker(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "intent-perf-test")

	prompts := []string{
		"please verify the migration ran cleanly",
		"please implement that fix after filing it",
		"Any cleanup to do?",
		"I'd like the banner to work properly",
		"fix bug 1426",
		"verify cache invalidation",
		"explain how the fold hook works",
		"summarize recent commits",
		"list every open bug",
		"what's the status of chain X?",
		"audit the refresolve package",
		"thanks for the heads-up",
		"hello",
		"that's an interesting point",
		"sanity check the latency budget",
		"track it down please",
		"build the new resolver",
		"recap the last session",
		"are there any orphan stashes?",
		"show me all chains in the project",
	}

	durations := make([]time.Duration, 0, len(prompts))
	for _, prompt := range prompts {
		body, _ := json.Marshal(struct {
			MessageText string `json:"message_text"`
		}{MessageText: prompt})
		start := time.Now()
		if _, err := refresolve.HandleParseContext(ctx, deps, body); err != nil {
			t.Fatalf("call %q: %v", prompt, err)
		}
		durations = append(durations, time.Since(start))
	}

	sort.Slice(durations, func(i, j int) bool {
		return durations[i] < durations[j]
	})
	p50 := durations[len(durations)/2]
	p95 := durations[len(durations)*95/100]
	t.Logf("p50=%s p95=%s n=%d (intent detection active end-to-end)", p50, p95, len(durations))

	if p50 > 50*time.Millisecond {
		t.Errorf("p50 latency %s exceeds 50ms ceiling — intent detection or rest of envelope regressed", p50)
	}
}
