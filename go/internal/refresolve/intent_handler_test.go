package refresolve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"toolkit/internal/events"
	"toolkit/internal/refresolve"
	"toolkit/internal/testutil"
)

// HandleParseContext populates the envelope's top-level `intent`
// field (chain parse-context-lean-orienting T5; PARSE_CONTEXT.md §13.4).
func TestHandleParseContext_PopulatesIntent(t *testing.T) {
	pool := testutil.NewTestDB(t)
	registry := refresolve.NewRegistry()
	deps := refresolve.HandlerDeps{
		Pool:             pool,
		Project:          "mcp-servers",
		Registry:         registry,
		Cache:            refresolve.NewParseContextCache(),
		DriftFireTracker: refresolve.NewDriftFireTracker(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "intent-envelope-test")

	cases := []struct {
		text      string
		wantShape string
	}{
		{"please verify the migration ran cleanly", "verify"},
		{"please implement that fix after filing it", "implement"},
		{"Any cleanup to do?", "audit"},
		{"thanks", "none"},
	}
	for _, tc := range cases {
		body, _ := json.Marshal(struct {
			MessageText string `json:"message_text"`
		}{MessageText: tc.text})
		r, err := refresolve.HandleParseContext(ctx, deps, body)
		if err != nil {
			t.Fatalf("HandleParseContext(%q): %v", tc.text, err)
		}
		if r.Intent == nil {
			t.Errorf("%q: Intent is nil; want populated envelope field", tc.text)
			continue
		}
		if r.Intent.Shape != tc.wantShape {
			t.Errorf("%q: Intent.Shape = %q, want %q", tc.text, r.Intent.Shape, tc.wantShape)
		}
		if tc.wantShape != "none" && r.Intent.DetectedVia != "pattern" {
			t.Errorf("%q: DetectedVia = %q, want \"pattern\"", tc.text, r.Intent.DetectedVia)
		}
		if tc.wantShape == "none" && r.Intent.DetectedVia != "default" {
			t.Errorf("%q: DetectedVia = %q, want \"default\" for none", tc.text, r.Intent.DetectedVia)
		}
	}
}

// Ambiguous prompts (none intent) MUST NOT gate token-shape
// resolution. The §13.3 invariant: missing intent never blocks the
// existing resolvers.
func TestHandleParseContext_NoneIntentDoesntGateTokenResolvers(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedChainProj(t, pool, "mcp-servers", "intent-none-chain", "open")
	registry := refresolve.NewRegistry()
	registry.Register(stubResolver{
		shape: refresolve.ShapeChainSlug,
		hit: refresolve.HitSet{Candidates: []refresolve.Candidate{
			{ID: "intent-none-chain", Title: "test", Score: 1.0, SourceRef: "chain:intent-none-chain"},
		}},
	})
	deps := refresolve.HandlerDeps{
		Pool:             pool,
		Project:          "mcp-servers",
		Registry:         registry,
		Cache:            refresolve.NewParseContextCache(),
		DriftFireTracker: refresolve.NewDriftFireTracker(),
	}
	ctx := events.WithMCPSessionID(context.Background(), "intent-none-test")
	// Conversational shape ("thanks for X") → IntentNone, but the
	// chain slug must still resolve.
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "thanks for the heads-up about intent-none-chain"})
	r, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Intent == nil || r.Intent.Shape != "none" {
		t.Errorf("expected Intent.Shape=none; got %+v", r.Intent)
	}
	found := false
	for _, ref := range r.References {
		if ref.Token == "intent-none-chain" {
			found = true
		}
	}
	if !found {
		t.Errorf("token-shape resolver did not fire under IntentNone — invariant §13.3 broken; refs=%v", r.References)
	}
}

// T5→T9 backfill: a verify-intent message in a drift-detected session
// fires the drift Candidate via the intent-conditional path even
// after the bootstrap fire has been consumed.
func TestHandleParseContext_IntentVerifyUnlocksDriftSurface(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Synthesize a marker + fd-deleted PID so drift is detected.
	dir := t.TempDir()
	procRoot := filepath.Join(dir, "proc")
	markerPath := filepath.Join(dir, "marker")
	pid := 88888
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/path/to/binary (deleted)", filepath.Join(pidDir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("preserved stdio pid: 88888\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := refresolve.NewRegistry()
	deps := refresolve.HandlerDeps{
		Pool:                    pool,
		Project:                 "mcp-servers",
		Registry:                registry,
		Cache:                   refresolve.NewParseContextCache(),
		GitSHA:                  "newsha",
		DriftFireTracker:        refresolve.NewDriftFireTracker(),
		DriftMarkerPathOverride: markerPath,
		DriftProcRootOverride:   procRoot,
	}
	ctx := events.WithMCPSessionID(context.Background(), "intent-drift-backfill-test")

	// Conversational first call burns the bootstrap fire (IntentNone
	// still surfaces drift via path a — the first-call branch).
	body1, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "hello"})
	if _, err := refresolve.HandleParseContext(ctx, deps, body1); err != nil {
		t.Fatal(err)
	}
	// Verify-intent second call: bootstrap already fired, so the
	// intent-conditional path is the operative branch. T5 backfill
	// makes this fire instead of being inert.
	body2, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "verify the cache invalidation is firing"})
	r2, err := refresolve.HandleParseContext(ctx, deps, body2)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Intent == nil || r2.Intent.Shape != "verify" {
		t.Fatalf("expected Intent.Shape=verify; got %+v", r2.Intent)
	}
	foundDrift := false
	for _, ref := range r2.References {
		if ref.Token == "stdio-drift" {
			foundDrift = true
		}
	}
	if !foundDrift {
		t.Errorf("verify-intent did NOT surface drift Candidate via T5→T9 path; refs=%v", r2.References)
	}
}
