package observehttp

import (
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// These tests close the one seed+load coverage gap the latent-inert-arm-audit
// left open: the vault-rerank-retrieve success predicate, whose Expr filters
// grounding_events on action = 'vault_search'. That is a free-text column with
// no CHECK to validate against, so per CONVENTIONS.md "Literal-filter contract"
// it needs a seed+load DB test — the CHECK-membership guard
// (telemetry.TestLiteralFilterArms_StayWithinCheckSet) cannot cover it.
//
// The predicate marks a inference_invocations row successful iff a proximate
// grounding_events row with action='vault_search' AND results_count>0 exists.
// The positive test makes the writer/reader contract executable: if the
// emitter ever stopped writing the literal 'vault_search', or the predicate's
// literal drifted, success_rate would flip from 1.0 to 0.0 and the test would
// fail — the silently-inert-arm class made loud.
//
// Seeding note: all rows share ~now, so the predicate's proximity window
// (latency_ms/1000 + 2s, here 3s) always spans them. The literal is what's
// under test here; the proximity-window time math is pinned by
// TestCharacterization_VaultRerankProximityWindowBoundary (bug 949 corrected
// the prior claim that inference_retrieval_test covered it — that file tests
// the /inference/retrieval-health aggregate, which shares no code with this
// predicate's latency-scaled window).

// TestLookupSuccessPredicate_Registry pins the registry dispatch itself —
// the pure routing logic that decides which SQL fragment a task_id gets,
// independent of any DB. The relocation (T12) materializes these
// predicates into a projection, so the exact dispatch (exact-match first,
// then the `classify_` 9-char prefix, else default + hadCustom=false) is
// behavior that must survive the move. The 8-vs-9-char cases pin the
// prefix boundary precisely.
func TestLookupSuccessPredicate_Registry(t *testing.T) {
	cases := []struct {
		taskID     string
		wantDesc   string
		wantCustom bool
	}{
		{"vault-rerank-retrieve", vaultRerankRetrievePredicate.Description, true}, // exact registry hit
		{"classify_x", classifyPredicate.Description, true},                       // prefix hit
		{"classify_", classifyPredicate.Description, true},                        // exactly 9 chars — boundary IN
		{"classify", defaultSuccessPredicate.Description, false},                  // 8 chars — boundary OUT
		{"classify-dash", defaultSuccessPredicate.Description, false},             // 9th char is '-', not '_'
		{"knowledge-search", defaultSuccessPredicate.Description, false},          // unregistered → default
		{"", defaultSuccessPredicate.Description, false},                          // empty → default
	}
	for _, c := range cases {
		t.Run(c.taskID, func(t *testing.T) {
			pred, hadCustom := lookupSuccessPredicate(c.taskID)
			if pred.Description != c.wantDesc {
				t.Errorf("desc = %q, want %q", pred.Description, c.wantDesc)
			}
			if hadCustom != c.wantCustom {
				t.Errorf("hadCustom = %v, want %v", hadCustom, c.wantCustom)
			}
		})
	}
}

const vaultRerankTaskID = "vault-rerank-retrieve"

// seedVaultRerankInvocations seeds n inference_invocations for the predicate's task
// at ~now with a 1s latency (→ 3s proximity tolerance), clearing the
// success-rate warmup threshold (20).
func seedVaultRerankInvocations(t *testing.T, pool *db.Pool, n int, now time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedQwenWithTime(t, pool, vaultRerankTaskID, "qwen2.5-32b", 1000, nil, nil, now)
	}
}

// TestVaultRerankPredicate_HitsOnProximateVaultSearchGrounding is the seed+load
// guard: seed the WRITER's literal (a vault_search grounding row with
// results_count>0) and assert it reaches the predicate.
func TestVaultRerankPredicate_HitsOnProximateVaultSearchGrounding(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	seedVaultRerankInvocations(t, pool, 25, now)
	testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingAction("vault_search"),
		testutil.WithGroundingResultsCount(3),
	)

	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)

	if len(got) != 1 {
		t.Fatalf("expected 1 card for %s, got %d", vaultRerankTaskID, len(got))
	}
	if got[0].SuccessRate == nil {
		t.Fatal("success_rate should populate at 25 calls (≥ warmup threshold 20)")
	}
	if *got[0].SuccessRate != 1.0 {
		t.Errorf("success_rate = %v, want 1.0 — every invocation sees the proximate action='vault_search' grounding row; a 0 here means the predicate literal no longer matches what the writer emits",
			*got[0].SuccessRate)
	}
}

// TestVaultRerankPredicate_IgnoresNonVaultSearchGrounding isolates the action
// literal as load-bearing: a proximate grounding row that is NOT a vault_search
// must not satisfy the predicate. Without the 'vault_search' filter this would
// falsely count as success.
func TestVaultRerankPredicate_IgnoresNonVaultSearchGrounding(t *testing.T) {
	pool := testutil.NewTestDB(t)
	now := time.Now()
	seedVaultRerankInvocations(t, pool, 25, now)
	testutil.SeedGroundingEvent(t, pool,
		testutil.WithGroundingAction("kiwix_search"),
		testutil.WithGroundingResultsCount(3),
	)

	srv := newTestServer(t, pool)
	var got []HealthCard
	getJSON(t, srv, "/inference/health-cards", &got)

	if len(got) != 1 {
		t.Fatalf("expected 1 card, got %d", len(got))
	}
	if got[0].SuccessRate == nil {
		t.Fatal("success_rate should populate at 25 calls")
	}
	if *got[0].SuccessRate != 0.0 {
		t.Errorf("success_rate = %v, want 0.0 — only action='vault_search' grounding rows satisfy the predicate, not kiwix_search",
			*got[0].SuccessRate)
	}
}
