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
	"toolkit/internal/stdiodrift"
	"toolkit/internal/testutil"
)

// shouldSurface bootstrap fires once per session regardless of intent.
// Direct DriftFireTracker test: pure logic, no DB.
func TestDriftFireTracker_BootstrapFiresOnce(t *testing.T) {
	tracker := refresolve.NewDriftFireTracker()
	state := stdiodrift.State{
		DriftDetected: true,
		StdioProcesses: []stdiodrift.StdioProcess{
			{PID: 1, DriftKind: stdiodrift.DriftKindStdioFDPinned},
		},
	}
	// First call: bootstrap path fires.
	surface, bootstrap, suppressed := refresolve.ExportShouldSurface(tracker, "sess-A", state, "")
	if !surface || !bootstrap || suppressed {
		t.Fatalf("first call: surface=%v bootstrap=%v suppressed=%v; want true,true,false",
			surface, bootstrap, suppressed)
	}
	// Second call with no intent: bootstrap already fired, intent empty
	// → no surface, no count increment beyond the first fire.
	surface, bootstrap, suppressed = refresolve.ExportShouldSurface(tracker, "sess-A", state, "")
	if surface || bootstrap || suppressed {
		t.Errorf("second call (no intent): surface=%v bootstrap=%v suppressed=%v; want false,false,false",
			surface, bootstrap, suppressed)
	}
}

// Intent-conditional path fires on verify / fix / implement / audit.
func TestDriftFireTracker_IntentConditionalFires(t *testing.T) {
	tracker := refresolve.NewDriftFireTracker()
	state := stdiodrift.State{
		DriftDetected:  true,
		StdioProcesses: []stdiodrift.StdioProcess{{PID: 1, DriftKind: stdiodrift.DriftKindStdioFDPinned}},
	}
	// Bootstrap consumed first.
	refresolve.ExportShouldSurface(tracker, "sess-B", state, "")

	for _, intent := range []string{"verify", "fix", "implement", "audit"} {
		t.Run(intent, func(t *testing.T) {
			// Sub-test uses its own session to keep the bootstrap path
			// separate from the surfacing counter under test.
			localTracker := refresolve.NewDriftFireTracker()
			sess := "sess-" + intent
			// Burn the bootstrap fire first.
			refresolve.ExportShouldSurface(localTracker, sess, state, "")
			// Now an intent-shape fire on the SAME session.
			surface, bootstrap, suppressed := refresolve.ExportShouldSurface(localTracker, sess, state, intent)
			if !surface {
				t.Errorf("intent=%q: surface=false, want true", intent)
			}
			if bootstrap {
				t.Errorf("intent=%q: bootstrap=true on subsequent fire, want false", intent)
			}
			if suppressed {
				t.Errorf("intent=%q: suppressed=true on second fire, want false (cap is 3)", intent)
			}
		})
	}
}

// Suppression kicks in after the third fire.
func TestDriftFireTracker_SuppressionAfterThreeFires(t *testing.T) {
	tracker := refresolve.NewDriftFireTracker()
	state := stdiodrift.State{
		DriftDetected:  true,
		StdioProcesses: []stdiodrift.StdioProcess{{PID: 1, DriftKind: stdiodrift.DriftKindStdioFDPinned}},
	}
	sess := "sess-suppress"
	// Three fires under the verify intent (after bootstrap).
	refresolve.ExportShouldSurface(tracker, sess, state, "verify") // 1 (bootstrap + intent both match; first call ticks)
	refresolve.ExportShouldSurface(tracker, sess, state, "verify") // 2
	refresolve.ExportShouldSurface(tracker, sess, state, "verify") // 3
	// Fourth fire: cap reached → suppressed.
	surface, _, suppressed := refresolve.ExportShouldSurface(tracker, sess, state, "verify")
	if surface {
		t.Error("fourth fire: surface=true, want false (cap=3)")
	}
	if !suppressed {
		t.Error("fourth fire: suppressed=false, want true")
	}
}

// No drift → no surface, no suppression, no bootstrap consumed.
func TestDriftFireTracker_NoDriftDoesNothing(t *testing.T) {
	tracker := refresolve.NewDriftFireTracker()
	state := stdiodrift.State{DriftDetected: false}
	surface, bootstrap, suppressed := refresolve.ExportShouldSurface(tracker, "sess-C", state, "verify")
	if surface || bootstrap || suppressed {
		t.Errorf("no drift: surface=%v bootstrap=%v suppressed=%v; want all false", surface, bootstrap, suppressed)
	}
}

// End-to-end through HandleParseContext: simulate the post-commit
// advisor marker + a preserved-PID /proc symlink with the deleted
// suffix; assert parse_context surfaces the drift Candidate.
//
// Live-verify acceptance criterion: at commit a4e21c5 the stdio process
// was on 0363dac and parse_context did NOT surface drift. This test
// pins the post-T9 expected behavior — same shape, just under
// controlled fixtures.
func TestParseContext_SurfacesStdioDriftCandidate(t *testing.T) {
	pool := testutil.NewTestDB(t)

	dir := t.TempDir()
	procRoot := filepath.Join(dir, "proc")
	markerPath := filepath.Join(dir, "marker")
	pid := 33333
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/path/to/binary (deleted)", filepath.Join(pidDir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("preserved stdio pid: 33333\n"), 0o644); err != nil {
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
	ctx := events.WithMCPSessionID(context.Background(), "stdio-drift-test")
	body, _ := json.Marshal(struct {
		MessageText string `json:"message_text"`
	}{MessageText: "hello"})

	r, err := refresolve.HandleParseContext(ctx, deps, body)
	if err != nil {
		t.Fatalf("HandleParseContext: %v", err)
	}
	found := false
	for _, ref := range r.References {
		if ref.Token == "stdio-drift" && ref.Shape == refresolve.ShapeDisciplineSkill {
			found = true
			if ref.PresentedAs == "" {
				t.Errorf("drift ref has empty presented_as")
			}
		}
	}
	if !found {
		t.Errorf("expected a stdio-drift discipline_skill Candidate in envelope; got refs=%v", r.References)
	}
}
