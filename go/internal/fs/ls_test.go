package fs

// ls_test.go is the net for fs.ls — a self-defined action (no harness LS tool),
// so this pins the first-principles contract: immediate children, sorted by
// name, typed rows with sizes, dotfiles gated by `all`. See testdata/LS_CONTRACT.md.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLS_Basic(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "b.txt"), "bb")
	mustWrite(t, filepath.Join(dir, "a.txt"), "a")
	if err := os.Mkdir(filepath.Join(dir, "zsub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := HandleLS(context.Background(), mustJSON(t, LSParams{Path: dir}))
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if res.Count != 3 || len(res.Entries) != 3 {
		t.Fatalf("count = %d, want 3", res.Count)
	}
	// sorted by name: a.txt, b.txt, zsub
	if res.Entries[0].Name != "a.txt" || res.Entries[1].Name != "b.txt" || res.Entries[2].Name != "zsub" {
		t.Errorf("order = %v, want [a.txt b.txt zsub]", names(res.Entries))
	}
	if res.Entries[0].Type != "file" || res.Entries[2].Type != "dir" {
		t.Errorf("types = %q/%q, want file/dir", res.Entries[0].Type, res.Entries[2].Type)
	}
	if res.Entries[0].Size != 1 {
		t.Errorf("a.txt size = %d, want 1", res.Entries[0].Size)
	}
	if res.Entries[2].Size != 0 {
		t.Errorf("dir size = %d, want 0", res.Entries[2].Size)
	}
}

func TestLS_HiddenGatedByAll(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "visible.txt"), "v")
	mustWrite(t, filepath.Join(dir, ".hidden"), "h")

	res, err := HandleLS(context.Background(), mustJSON(t, LSParams{Path: dir}))
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if res.Count != 1 || res.Entries[0].Name != "visible.txt" {
		t.Errorf("default hid dotfiles wrong: %v", names(res.Entries))
	}

	all := true
	res2, err := HandleLS(context.Background(), mustJSON(t, LSParams{Path: dir, All: &all}))
	if err != nil {
		t.Fatalf("ls all: %v", err)
	}
	if res2.Count != 2 {
		t.Errorf("all=true count = %d, want 2 (%v)", res2.Count, names(res2.Entries))
	}
}

func TestLS_Symlink(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "target.txt"), "t")
	if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	res, err := HandleLS(context.Background(), mustJSON(t, LSParams{Path: dir}))
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	var linkType string
	for _, e := range res.Entries {
		if e.Name == "link" {
			linkType = e.Type
		}
	}
	if linkType != "symlink" {
		t.Errorf("link type = %q, want symlink", linkType)
	}
}

func TestLS_NotExist(t *testing.T) {
	if _, err := HandleLS(context.Background(), mustJSON(t, LSParams{Path: filepath.Join(t.TempDir(), "nope")})); err == nil {
		t.Error("ls nonexistent = nil error, want error")
	}
}

func TestLS_FileNotDir(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	mustWrite(t, f, "x")
	if _, err := HandleLS(context.Background(), mustJSON(t, LSParams{Path: f})); err == nil {
		t.Error("ls of a file = nil error, want error")
	}
}

func TestLS_DefaultsToWorkingDir(t *testing.T) {
	res, err := HandleLS(context.Background(), mustJSON(t, LSParams{}))
	if err != nil {
		t.Fatalf("ls default: %v", err)
	}
	// The package dir contains this test file's sources; just assert it listed
	// something and reported the directory.
	if res.Count == 0 {
		t.Error("ls of working dir returned no entries")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func names(es []LSEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name
	}
	return out
}
