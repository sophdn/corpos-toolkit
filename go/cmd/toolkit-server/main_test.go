package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBlueprintsDir_ExplicitPathPassesThrough(t *testing.T) {
	if got := resolveBlueprintsDir("/some/explicit/path"); got != "/some/explicit/path" {
		t.Errorf("explicit path: got %q, want %q", got, "/some/explicit/path")
	}
}

func TestResolveBlueprintsDir_EmptyMeansOptOut(t *testing.T) {
	// An empty flag value is the explicit opt-out per bug 1266's constraint.
	// It must NOT trigger auto-discovery.
	if got := resolveBlueprintsDir(""); got != "" {
		t.Errorf("empty (opt-out): got %q, want \"\"", got)
	}
}

func TestAutoDiscoverBlueprintsDir_FindsCanonicalLayout(t *testing.T) {
	// Build a temp tree matching the deployed layout:
	//   <root>/go/bin/toolkit-server  (fake binary)
	//   <root>/blueprints/forge-schemas/  (schemas)
	root := t.TempDir()
	binDir := filepath.Join(root, "go", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bindir: %v", err)
	}
	exe := filepath.Join(binDir, "toolkit-server")
	if err := os.WriteFile(exe, []byte{}, 0o755); err != nil {
		t.Fatalf("touch exe: %v", err)
	}
	schemasDir := filepath.Join(root, "blueprints", "forge-schemas")
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		t.Fatalf("mkdir schemas: %v", err)
	}

	got := autoDiscoverBlueprintsDir(exe)
	// Resolve symlinks on both sides since t.TempDir on macOS returns /var
	// which may resolve to /private/var.
	want, _ := filepath.EvalSymlinks(schemasDir)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != want {
		t.Errorf("got %q, want %q", gotResolved, want)
	}
}

func TestAutoDiscoverBlueprintsDir_ReturnsEmptyWhenAbsent(t *testing.T) {
	// Same binary layout, but no blueprints/ dir present — must return "".
	root := t.TempDir()
	binDir := filepath.Join(root, "go", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bindir: %v", err)
	}
	exe := filepath.Join(binDir, "toolkit-server")
	if err := os.WriteFile(exe, []byte{}, 0o755); err != nil {
		t.Fatalf("touch exe: %v", err)
	}

	if got := autoDiscoverBlueprintsDir(exe); got != "" {
		t.Errorf("missing schemas: got %q, want \"\"", got)
	}
}

func TestAutoDiscoverRubricsDir_FindsCanonicalLayout(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "go", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bindir: %v", err)
	}
	exe := filepath.Join(binDir, "toolkit-server")
	if err := os.WriteFile(exe, []byte{}, 0o755); err != nil {
		t.Fatalf("touch exe: %v", err)
	}
	rubricsDir := filepath.Join(root, "blueprints", "rubrics")
	if err := os.MkdirAll(rubricsDir, 0o755); err != nil {
		t.Fatalf("mkdir rubrics: %v", err)
	}

	got := autoDiscoverRubricsDir(exe)
	want, _ := filepath.EvalSymlinks(rubricsDir)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != want {
		t.Errorf("got %q, want %q", gotResolved, want)
	}
}

func TestAutoDiscoverRubricsDir_ReturnsEmptyWhenAbsent(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "go", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bindir: %v", err)
	}
	exe := filepath.Join(binDir, "toolkit-server")
	if err := os.WriteFile(exe, []byte{}, 0o755); err != nil {
		t.Fatalf("touch exe: %v", err)
	}
	if got := autoDiscoverRubricsDir(exe); got != "" {
		t.Errorf("missing rubrics: got %q, want \"\"", got)
	}
}

// Resolver preference-order + HasPathPrefix coverage moved to
// internal/dispatch/resolve_test.go alongside the implementation, which is now
// shared by the native stdio and HTTP transports.
