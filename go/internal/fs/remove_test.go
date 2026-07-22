package fs

// remove_test.go is the characterization net for fs.remove — file deletion,
// empty-dir deletion, the non-empty-dir recursive guard, the protected-root
// backstop, the missing-target error, and the path alias.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemove_File(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HandleRemove(context.Background(), mustJSON(t, RemoveParams{FilePath: p}))
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got.WasDir {
		t.Errorf("was_dir = true for a file")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file not removed")
	}
}

func TestRemove_EmptyDirWithoutRecursive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := HandleRemove(context.Background(), mustJSON(t, RemoveParams{FilePath: p}))
	if err != nil {
		t.Fatalf("remove empty dir: %v", err)
	}
	if !got.WasDir {
		t.Errorf("was_dir = false for a dir")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("empty dir not removed")
	}
}

func TestRemove_NonEmptyDirRefusedWithoutRecursive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "full")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := HandleRemove(context.Background(), mustJSON(t, RemoveParams{FilePath: p}))
	if err == nil || !strings.Contains(err.Error(), "non-empty directory") {
		t.Fatalf("expected non-empty refusal, got %v", err)
	}
	// Nothing deleted.
	if _, serr := os.Stat(filepath.Join(p, "f")); serr != nil {
		t.Errorf("contents removed on refused remove: %v", serr)
	}
}

func TestRemove_NonEmptyDirWithRecursive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(p, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "nested", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HandleRemove(context.Background(), mustJSON(t, RemoveParams{FilePath: p, Recursive: true}))
	if err != nil {
		t.Fatalf("recursive remove: %v", err)
	}
	if !got.WasDir {
		t.Errorf("was_dir = false")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("tree not removed")
	}
}

func TestRemove_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := HandleRemove(context.Background(), mustJSON(t, RemoveParams{FilePath: filepath.Join(dir, "nope")}))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing, got %v", err)
	}
}

func TestRemove_RequiresFilePath(t *testing.T) {
	_, err := HandleRemove(context.Background(), mustJSON(t, RemoveParams{}))
	if err == nil || !strings.Contains(err.Error(), "requires file_path") {
		t.Fatalf("expected requires-file_path, got %v", err)
	}
}

func TestRemove_RefusesProtectedRoot(t *testing.T) {
	for _, root := range []string{"/", "/home", "/etc"} {
		_, err := HandleRemove(context.Background(), mustJSON(t, RemoveParams{FilePath: root, Recursive: true}))
		if err == nil || !strings.Contains(err.Error(), "protected filesystem root") {
			t.Errorf("root %q: expected refusal, got %v", root, err)
		}
	}
}

func TestRemove_PathAlias(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"path":"` + p + `"}`)
	if _, err := HandleRemove(context.Background(), raw); err != nil {
		t.Fatalf("remove via path alias: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("not removed via alias")
	}
}
