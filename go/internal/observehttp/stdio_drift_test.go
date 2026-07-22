package observehttp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"toolkit/internal/stdiodrift"
)

// GET /admin/stdio-drift-state returns drift_detected=false with an
// empty stdio_processes array when no marker is present.
func TestStdioDriftState_NoMarkerReturnsClean(t *testing.T) {
	srv := httptest.NewServer(BuildRouter(AppState{
		DriftMarkerPathOverride: filepath.Join(t.TempDir(), "no-marker"),
	}))
	t.Cleanup(srv.Close)

	var resp stdiodrift.State
	if code := getJSON(t, srv, "/admin/stdio-drift-state", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.DriftDetected {
		t.Error("DriftDetected = true, want false (no marker)")
	}
	if len(resp.StdioProcesses) != 0 {
		t.Errorf("StdioProcesses non-empty: %v", resp.StdioProcesses)
	}
}

// Marker plus an fd-deleted symlink → drift_detected=true with one
// entry tagged stdio_fd_pinned.
func TestStdioDriftState_MarkerPlusFDDeletedSurfacesDrift(t *testing.T) {
	dir := t.TempDir()
	procRoot := filepath.Join(dir, "proc")
	markerPath := filepath.Join(dir, "marker")
	pid := 77777
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/bin/toolkit-server (deleted)", filepath.Join(pidDir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("preserved stdio pid: 77777\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(BuildRouter(AppState{
		GitSHA:                  "newsha",
		DriftMarkerPathOverride: markerPath,
		DriftProcRootOverride:   procRoot,
	}))
	t.Cleanup(srv.Close)

	var resp stdiodrift.State
	if code := getJSON(t, srv, "/admin/stdio-drift-state", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.DriftDetected {
		t.Fatal("DriftDetected = false, want true")
	}
	if len(resp.StdioProcesses) != 1 {
		t.Fatalf("StdioProcesses len = %d, want 1", len(resp.StdioProcesses))
	}
	p := resp.StdioProcesses[0]
	if p.PID != pid {
		t.Errorf("PID = %d, want %d", p.PID, pid)
	}
	if !p.FDDeleted {
		t.Error("FDDeleted = false, want true")
	}
	if p.DriftKind != stdiodrift.DriftKindStdioFDPinned {
		t.Errorf("DriftKind = %v, want %v", p.DriftKind, stdiodrift.DriftKindStdioFDPinned)
	}
	if p.OnDiskBinarySHA != "newsha" {
		t.Errorf("OnDiskBinarySHA = %q, want %q", p.OnDiskBinarySHA, "newsha")
	}
}
