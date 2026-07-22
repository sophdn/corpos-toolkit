package fs

// move_test.go is the characterization net for fs.move — rename/relocate of
// files and directories, the dir-into (mv) semantics, the no-clobber guard, the
// param aliases, and the cross-device copy-then-remove fallback (exercised via
// the copyTree helper directly, since a unit test cannot mount two filesystems).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMove_RenameFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "b.txt")
	got, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Source: src, Dest: dst}))
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if got.Dest != dst || got.IsDir || got.CrossDevice {
		t.Errorf("result = %+v", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still exists")
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello\n" {
		t.Errorf("dest content = %q", b)
	}
}

func TestMove_IntoExistingDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Source: src, Dest: destDir}))
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	want := filepath.Join(destDir, "a.txt")
	if got.Dest != want {
		t.Errorf("dest = %q want %q", got.Dest, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("entry not moved into dir: %v", err)
	}
}

func TestMove_RenameDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "d1")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "f"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "d2")
	got, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Source: src, Dest: dst}))
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if !got.IsDir {
		t.Errorf("is_dir = false, want true")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "nested", "f")); string(b) != "y" {
		t.Errorf("nested content lost: %q", b)
	}
}

func TestMove_CreatesDestParent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "new", "deep", "b.txt")
	if _, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Source: src, Dest: dst})); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dest not created: %v", err)
	}
}

func TestMove_RefusesClobber(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Source: src, Dest: dst}))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected clobber refusal, got %v", err)
	}
	// Source must be untouched after a refused move.
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("source removed on refused move: %v", serr)
	}
}

func TestMove_SourceMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Source: filepath.Join(dir, "nope"), Dest: filepath.Join(dir, "x")}))
	if err == nil || !strings.Contains(err.Error(), "source does not exist") {
		t.Fatalf("expected source-missing, got %v", err)
	}
}

func TestMove_RequiresBothParams(t *testing.T) {
	if _, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Dest: "/x"})); err == nil || !strings.Contains(err.Error(), "requires source") {
		t.Errorf("missing source: %v", err)
	}
	if _, err := HandleMove(context.Background(), mustJSON(t, MoveParams{Source: "/x"})); err == nil || !strings.Contains(err.Error(), "requires dest") {
		t.Errorf("missing dest: %v", err)
	}
}

func TestMove_ParamAliases(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "b.txt")
	// src/to aliases instead of source/dest.
	raw := []byte(`{"src":"` + src + `","to":"` + dst + `"}`)
	got, err := HandleMove(context.Background(), raw)
	if err != nil {
		t.Fatalf("move via aliases: %v", err)
	}
	if got.Dest != dst {
		t.Errorf("alias dest = %q", got.Dest)
	}
}

// TestMove_CrossDeviceCopyTree exercises the EXDEV fallback path directly: a
// unit test can't mount two filesystems, so we verify copyTree replicates a
// tree faithfully (the fallback = copyTree + RemoveAll(source)).
func TestMove_CrossDeviceCopyTree(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(src, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "f.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy")
	info, _ := os.Stat(src)
	if err := copyTree(src, dst, info); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "f.txt")); string(b) != "deep" {
		t.Errorf("nested file = %q", b)
	}
	fi, err := os.Stat(filepath.Join(dst, "a", "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode not preserved: %v", fi.Mode().Perm())
	}
}
