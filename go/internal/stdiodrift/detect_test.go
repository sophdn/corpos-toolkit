package stdiodrift

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestReadMarker_AbsentReturnsNoErrorNoPresent(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "no-such-marker")
	info, err := ReadMarker(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Present {
		t.Error("expected Present=false for missing marker")
	}
	if len(info.PreservedPIDs) != 0 {
		t.Errorf("expected zero PreservedPIDs; got %v", info.PreservedPIDs)
	}
}

func TestReadMarker_ParsesPreservedPIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")
	body := `stdio toolkit-server rebuilt at 2026-05-21T01:00:00Z
the active session retained the OLD binary on disk fd; /mcp reconnect to pick up the new one
preserved stdio pid: 12345
preserved stdio pid: 67890
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := ReadMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Present {
		t.Fatal("expected Present=true")
	}
	want := []int{12345, 67890}
	if len(info.PreservedPIDs) != len(want) {
		t.Fatalf("PreservedPIDs len=%d, want %d (%v)", len(info.PreservedPIDs), len(want), info.PreservedPIDs)
	}
	for i, got := range info.PreservedPIDs {
		if got != want[i] {
			t.Errorf("PreservedPIDs[%d] = %d, want %d", i, got, want[i])
		}
	}
}

func TestProcessFDDeleted_DetectsDeletedSuffix(t *testing.T) {
	procRoot := t.TempDir()
	pid := 99
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Symlink target with " (deleted)" suffix — Linux kernel writes
	// this on /proc/<pid>/exe when the original inode is unlinked.
	if err := os.Symlink("/usr/local/bin/toolkit-server (deleted)", filepath.Join(pidDir, "exe")); err != nil {
		t.Fatal(err)
	}
	deleted, err := ProcessFDDeleted(procRoot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected fd_deleted=true for symlink with ' (deleted)' suffix")
	}
}

func TestProcessFDDeleted_LivingProcessIsNotDeleted(t *testing.T) {
	procRoot := t.TempDir()
	pid := 100
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/bin/toolkit-server", filepath.Join(pidDir, "exe")); err != nil {
		t.Fatal(err)
	}
	deleted, err := ProcessFDDeleted(procRoot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected fd_deleted=false for symlink without ' (deleted)' suffix")
	}
}

func TestProcessFDDeleted_NonexistentPIDIsNotDeleted(t *testing.T) {
	procRoot := t.TempDir()
	deleted, err := ProcessFDDeleted(procRoot, 42424242)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected fd_deleted=false for nonexistent PID (no error)")
	}
}

func TestClassifyKind_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		deleted  bool
		reported string
		onDisk   string
		want     DriftKind
	}{
		{"clean", false, "abc123", "abc123", DriftKindNone},
		{"fd-pinned only", true, "abc123", "abc123", DriftKindStdioFDPinned},
		{"sha-stale only", false, "abc123", "def456", DriftKindCompileTimeStale},
		{"both", true, "abc123", "def456", DriftKindBoth},
		{"fd-pinned, unknown reported", true, "", "def456", DriftKindStdioFDPinned},
		{"reported empty isn't stale", false, "", "def456", DriftKindNone},
		{"onDisk empty isn't stale", false, "abc123", "", DriftKindNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyKind(tc.deleted, tc.reported, tc.onDisk)
			if got != tc.want {
				t.Errorf("ClassifyKind(%v, %q, %q) = %v, want %v",
					tc.deleted, tc.reported, tc.onDisk, got, tc.want)
			}
		})
	}
}

func TestSnapshot_NoMarkerNoDrift(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "no-marker")
	state, err := Snapshot(context.Background(), SnapshotInputs{
		MarkerPathOverride: tmp,
		OnDiskGitSHA:       "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.DriftDetected {
		t.Error("expected DriftDetected=false without marker")
	}
	if len(state.StdioProcesses) != 0 {
		t.Errorf("expected zero StdioProcesses; got %d", len(state.StdioProcesses))
	}
}

func TestSnapshot_MarkerPlusFDDeletedReportsDrift(t *testing.T) {
	dir := t.TempDir()
	procRoot := filepath.Join(dir, "proc")
	markerPath := filepath.Join(dir, "marker")

	pid := 12345
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/path/to/binary (deleted)", filepath.Join(pidDir, "exe")); err != nil {
		t.Fatal(err)
	}
	markerBody := "stdio toolkit-server rebuilt at X\npreserved stdio pid: 12345\n"
	if err := os.WriteFile(markerPath, []byte(markerBody), 0o644); err != nil {
		t.Fatal(err)
	}

	state, err := Snapshot(context.Background(), SnapshotInputs{
		MarkerPathOverride: markerPath,
		ProcRootOverride:   procRoot,
		OnDiskGitSHA:       "newsha",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !state.DriftDetected {
		t.Fatal("expected DriftDetected=true")
	}
	if len(state.StdioProcesses) != 1 {
		t.Fatalf("expected 1 StdioProcess; got %d", len(state.StdioProcesses))
	}
	p := state.StdioProcesses[0]
	if p.PID != pid {
		t.Errorf("PID = %d, want %d", p.PID, pid)
	}
	if !p.FDDeleted {
		t.Error("expected FDDeleted=true")
	}
	if p.DriftKind != DriftKindStdioFDPinned {
		t.Errorf("DriftKind = %v, want %v", p.DriftKind, DriftKindStdioFDPinned)
	}
	if p.OnDiskBinarySHA != "newsha" {
		t.Errorf("OnDiskBinarySHA = %q, want %q", p.OnDiskBinarySHA, "newsha")
	}
}

func TestSnapshot_ReportedSHAByPIDPopulatesField(t *testing.T) {
	dir := t.TempDir()
	procRoot := filepath.Join(dir, "proc")
	markerPath := filepath.Join(dir, "marker")
	pid := 222
	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/path/to/binary", filepath.Join(pidDir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("preserved stdio pid: 222\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	state, err := Snapshot(context.Background(), SnapshotInputs{
		MarkerPathOverride: markerPath,
		ProcRootOverride:   procRoot,
		OnDiskGitSHA:       "newsha",
		ReportedSHAByPID:   map[int]string{222: "oldsha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !state.DriftDetected {
		t.Fatal("expected DriftDetected=true (sha mismatch is itself drift)")
	}
	p := state.StdioProcesses[0]
	if p.ReportedGitSHA != "oldsha" {
		t.Errorf("ReportedGitSHA = %q, want %q", p.ReportedGitSHA, "oldsha")
	}
	if p.DriftKind != DriftKindCompileTimeStale {
		t.Errorf("DriftKind = %v, want %v (not fd-pinned, sha mismatch only)",
			p.DriftKind, DriftKindCompileTimeStale)
	}
}
