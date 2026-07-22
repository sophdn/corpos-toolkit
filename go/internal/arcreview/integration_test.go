package arcreview

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
)

// llamaServerProbeTimeout caps the integration health check. Local
// llama-server should respond well under a second when up; the longer
// cap lets the probe survive a marginal local network.
const llamaServerProbeTimeout = 2 * time.Second

// llamaServerReachable returns the base URL and true when a real
// llama-server is responding to /v1/models within the timeout.
// Returns "", false otherwise; the caller skips the test on false.
//
// Honors the TOOLKIT_LOCAL_URL env var (the same one
// llamacpp.New / router.New consult) so a non-default port works.
func llamaServerReachable() (string, bool) {
	base := os.Getenv(llamacpp.EnvLocalURL)
	if base == "" {
		base = "http://localhost:8081"
	}
	client := &http.Client{Timeout: llamaServerProbeTimeout}
	resp, err := client.Get(base + "/v1/models")
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	return base, true
}

// TestIntegration_ReviewFiresAgainstLiveQwen exercises the full Stage 1
// + Stage 2 stack against a real llama-server instance. Skipped when
// the server isn't reachable so CI runs clean.
//
// What it covers:
//   - Snapshot extraction from a fixture transcript.
//   - DispatchReview making the two real Qwen calls (arc-summary +
//     main review).
//   - ParseReviewResponse handling whatever shape Qwen returns.
//   - ValidateDecision dropping out-of-schema decisions.
//   - The handler partitioning the kept decisions.
//   - emitFilingReviewedEvent landing an ArcCloseFilingReviewed row.
//
// What it does NOT cover (deferred to per-stage tests):
//   - Specific decision content. Qwen output is non-deterministic; we
//     assert the shape and reachability, not the filing accuracy.
//   - Substrate-side firing path (see listener.go — substrate-side
//     firing is the follow-up task).
//
// To run locally:
//
//	# Ensure llama-server is up at $TOOLKIT_LOCAL_URL (default :8081)
//	make -C go test ./internal/arcreview/...
func TestIntegration_ReviewFiresAgainstLiveQwen(t *testing.T) {
	base, ok := llamaServerReachable()
	if !ok {
		t.Skip("llama-server not reachable at TOOLKIT_LOCAL_URL (or default localhost:8081); skipping integration test")
	}
	t.Logf("using llama-server at %s", base)

	pool := openTestPool(t)

	// Seed a project row so the event emission's project-scoped
	// entity validates.
	if _, err := pool.DB().Exec(
		`INSERT INTO projects (id, name) VALUES ('mcp-servers', 'mcp-servers')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Build a minimal but realistic transcript: a small bug-fix arc
	// with one workaround comment — the kind of arc the discipline
	// surfaces as bug-worthy.
	transcript := []string{
		`{"role":"user","content":"the cli is exiting 0 even when the subcommand returns 2 — what's going on?"}`,
		`{"role":"assistant","content":"looking at the wrapper, it captures the inner subprocess status but always exits 0 after writing the report. that's the bug — the wrapper swallows the exit code. workaround for now: grep the report for ERROR and exit 1 manually from the caller. should probably fix the wrapper to propagate."}`,
		`{"role":"user","content":"yes, please fix the wrapper to propagate; the grep workaround is fragile"}`,
		`{"role":"assistant","content":"updated wrapper.sh to forward $? through the exit. tests pass; the original exit-0 path is gone."}`,
	}
	tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(tpath, []byte(strings.Join(transcript, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	r, err := router.New(base)
	if err != nil {
		t.Fatalf("router.New(%q): %v", base, err)
	}

	deps := Deps{Pool: pool, Router: r, BackoffSeconds: 1}
	params, _ := json.Marshal(map[string]any{
		"session_id":      "integration-test-session",
		"transcript_path": tpath,
		"triggers":        []string{"user_shape_done", "counter_user_turns_5"},
		"fired_at":        time.Now().UTC().Format(time.RFC3339),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := HandleReviewArcForFiling(ctx, deps, "mcp-servers", params)
	if err != nil {
		t.Fatalf("handler returned error (should fail-open with status field): %v", err)
	}

	switch out.Status {
	case "fired":
		// Happy path. Assert structural invariants we expect from any
		// successful fire.
		if len(out.Decisions) == 0 {
			t.Logf("warning: Qwen returned 0 valid decisions; that's accepted (parse drift / over-rejection)")
		}
		if got := len(out.Partition.AutoExecute) + len(out.Partition.SurfaceForConfirm) + len(out.Partition.Skip); got != len(out.Decisions) {
			t.Errorf("partition counts (%d) must equal decision count (%d)", got, len(out.Decisions))
		}
		// Confirm the telemetry row landed.
		var rowCount int
		if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'ArcCloseFilingReviewed' AND entity_slug = ?`, "integration-test-session").Scan(&rowCount); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if rowCount != 1 {
			t.Errorf("expected exactly 1 ArcCloseFilingReviewed event, got %d", rowCount)
		}
		t.Logf("fired: decisions=%d auto_execute=%d surface=%d skip=%d latency_ms=%d",
			len(out.Decisions), len(out.Partition.AutoExecute),
			len(out.Partition.SurfaceForConfirm), len(out.Partition.Skip), out.LatencyMS)
	case "qwen_unreachable":
		// Probe passed but the actual generate call failed — could be
		// a cold-start timeout or an unusual error. Not a hard fail;
		// log and skip the structural assertions.
		t.Skipf("qwen_unreachable from real call (probe succeeded; generate failed): %s", out.Reason)
	default:
		t.Fatalf("unexpected status %q (reason: %s)", out.Status, out.Reason)
	}
}
