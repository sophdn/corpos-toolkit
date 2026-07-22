package registry_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/registry"
	"toolkit/internal/testutil"
)

// gitT runs a git command in dir, failing the test on error.
func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setupBareAndClone creates a local bare remote + a clone of it, returning the
// clone dir. A local bare remote needs no TLS, so the mirror round-trip is
// fully hermetic.
func setupBareAndClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "registry.git")
	clone := filepath.Join(root, "clone")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", bare).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "clone", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	return clone
}

func TestMirror_PushesNewEvents_Incremental(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ctx := context.Background()
	emitBug(t, pool, "mirror-1") // 2 events: BugReported + BugResolved

	clone := setupBareAndClone(t)
	opts := registry.MirrorOptions{Remote: "origin", Branch: "main"}

	// First mirror pushes the 2 events.
	res, err := registry.Mirror(ctx, pool, clone, opts)
	if err != nil {
		t.Fatalf("mirror 1: %v", err)
	}
	if !res.Pushed || res.NewEvents != 2 || res.TotalEvents != 2 {
		t.Fatalf("unexpected mirror 1 result: %+v", res)
	}

	// Second mirror with no new events pushes nothing.
	res2, err := registry.Mirror(ctx, pool, clone, opts)
	if err != nil {
		t.Fatalf("mirror 2: %v", err)
	}
	if res2.Pushed {
		t.Fatalf("mirror 2 should be a no-op, got: %+v", res2)
	}

	// A new event makes the next mirror push exactly that delta.
	emitBug(t, pool, "mirror-2")
	res3, err := registry.Mirror(ctx, pool, clone, opts)
	if err != nil {
		t.Fatalf("mirror 3: %v", err)
	}
	if !res3.Pushed || res3.NewEvents != 2 || res3.TotalEvents != 4 {
		t.Fatalf("unexpected mirror 3 result: %+v", res3)
	}

	// The pushed events landed on the remote: re-clone the bare repo and count.
	verify := filepath.Join(t.TempDir(), "verify")
	bareOut, err := exec.Command("git", "-C", clone, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("get origin url: %v", err)
	}
	bare := strings.TrimSpace(string(bareOut))
	if out, err := exec.Command("git", "clone", bare, verify).CombinedOutput(); err != nil {
		t.Fatalf("re-clone for verify: %v\n%s", err, out)
	}
	files, _ := filepath.Glob(filepath.Join(verify, "events", "*.json"))
	if len(files) != 4 {
		t.Fatalf("remote should have 4 event files after 2 mirrors, got %d", len(files))
	}
}
