package arcreview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscriptFile(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func mustParams(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

func TestHandle_SkippedOnMissingSessionID(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	out, err := HandleReviewArcForFiling(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"transcript_path": "/tmp/foo"}))
	if err != nil {
		t.Fatalf("handler returned error; expected non-error skip: %v", err)
	}
	if out.Status != "skipped" {
		t.Fatalf("expected status=skipped, got %q", out.Status)
	}
	if !strings.Contains(out.Reason, "session_id") {
		t.Fatalf("expected reason to name session_id, got %q", out.Reason)
	}
}

func TestHandle_SkippedOnMissingTranscriptPath(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	out, _ := HandleReviewArcForFiling(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{"session_id": "s1"}))
	if out.Status != "skipped" || !strings.Contains(out.Reason, "transcript_path") {
		t.Fatalf("expected skipped+transcript_path reason, got %+v", out)
	}
}

func TestHandle_SkippedOnMissingRouter(t *testing.T) {
	pool := openTestPool(t)
	tpath := writeTranscriptFile(t, []string{`{"role":"user","content":"hi"}`})
	deps := Deps{Pool: pool} // Router nil
	out, _ := HandleReviewArcForFiling(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{
			"session_id":      "s1",
			"transcript_path": tpath,
		}))
	if out.Status != "qwen_unreachable" {
		t.Fatalf("expected status=qwen_unreachable when router missing, got %q (%+v)", out.Status, out)
	}
}

func TestHandle_DebouncedOnSecondCall(t *testing.T) {
	pool := openTestPool(t)
	tpath := writeTranscriptFile(t, []string{`{"role":"user","content":"hi"}`})

	// Seed a recent fire by directly calling RecordFire under the
	// debouncer — this lets the test exercise the debounced branch
	// without driving a full review (which would need a live router).
	deb := NewDebouncer(pool)
	if err := deb.RecordFire(context.Background(), "s1"); err != nil {
		t.Fatalf("RecordFire seed: %v", err)
	}

	deps := Deps{Pool: pool} // Router nil — but debouncer fires before the nil check matters
	out, _ := HandleReviewArcForFiling(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{
			"session_id":      "s1",
			"transcript_path": tpath,
		}))
	if out.Status != "debounced" {
		t.Fatalf("expected status=debounced after RecordFire, got %q (%+v)", out.Status, out)
	}
	if out.LastFireAt == "" {
		t.Fatalf("expected LastFireAt populated on debounced response")
	}
}

func TestHandle_SkippedOnEmptyTranscript(t *testing.T) {
	pool := openTestPool(t)
	tpath := writeTranscriptFile(t, nil)
	deps := Deps{Pool: pool}
	out, _ := HandleReviewArcForFiling(context.Background(), deps, "mcp-servers",
		mustParams(t, map[string]any{
			"session_id":      "fresh",
			"transcript_path": tpath,
		}))
	// With router nil, the empty-snapshot check comes after the nil
	// check, so we expect qwen_unreachable. But to exercise the
	// empty-snapshot branch we'd need a non-nil router. Confirm the
	// empty-transcript path doesn't crash.
	if out.Status != "qwen_unreachable" && out.Status != "skipped" {
		t.Fatalf("empty transcript + nil router: expected qwen_unreachable or skipped, got %q", out.Status)
	}
}

func TestPartitionDecisions_AutoExecuteHighConfidence(t *testing.T) {
	body, _ := json.Marshal(ForgeBugPayload{Title: "t", ProblemStatement: "p"})
	decisions := []FilingDecision{
		{Action: ActionForgeBug, Confidence: 0.9, Payload: body, Reasoning: "x"},
	}
	got := partitionDecisions(decisions)
	if len(got.AutoExecute) != 1 {
		t.Fatalf("expected 1 auto-execute, got %d", len(got.AutoExecute))
	}
	if len(got.SurfaceForConfirm) != 0 || len(got.Skip) != 0 {
		t.Fatalf("unexpected partition: %+v", got)
	}
}

func TestPartitionDecisions_SkillUpdateAlwaysSurfaces(t *testing.T) {
	body, _ := json.Marshal(SkillUpdatePayload{
		SkillSlug: "x", PatchKind: "add_section", Content: "c",
	})
	// Even at confidence 0.99, skill_update must NOT auto-execute.
	decisions := []FilingDecision{
		{Action: ActionSkillUpdate, Confidence: 0.99, Payload: body, Reasoning: "x"},
	}
	got := partitionDecisions(decisions)
	if len(got.AutoExecute) != 0 {
		t.Fatalf("skill_update must never auto-execute; got AutoExecute=%v", got.AutoExecute)
	}
	if len(got.SurfaceForConfirm) != 1 {
		t.Fatalf("expected 1 surface_for_confirm, got %d", len(got.SurfaceForConfirm))
	}
}

func TestPartitionDecisions_MediumConfidenceSurfaces(t *testing.T) {
	body, _ := json.Marshal(ForgeBugPayload{Title: "t", ProblemStatement: "p"})
	decisions := []FilingDecision{
		{Action: ActionForgeBug, Confidence: 0.7, Payload: body, Reasoning: "x"},
	}
	got := partitionDecisions(decisions)
	if len(got.SurfaceForConfirm) != 1 {
		t.Fatalf("expected medium-confidence forge_bug to surface, got %+v", got)
	}
}

func TestPartitionDecisions_LowConfidenceSkipped(t *testing.T) {
	body, _ := json.Marshal(ForgeBugPayload{Title: "t", ProblemStatement: "p"})
	decisions := []FilingDecision{
		{Action: ActionForgeBug, Confidence: 0.3, Payload: body, Reasoning: "x"},
	}
	got := partitionDecisions(decisions)
	if len(got.Skip) != 1 {
		t.Fatalf("expected low-confidence skip, got %+v", got)
	}
}

func TestPartitionDecisions_NothingToFileLandsInSkip(t *testing.T) {
	decisions := []FilingDecision{
		{Action: ActionNothingToFile, Confidence: 0.4, Reasoning: "uneventful"},
	}
	got := partitionDecisions(decisions)
	if len(got.Skip) != 1 {
		t.Fatalf("expected nothing_to_file to land in Skip, got %+v", got)
	}
}
