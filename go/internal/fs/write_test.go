package fs

// write_test.go is the characterization net for fs.write — the committed
// contract the implementation is built to pass: verbatim whole-file write with
// parent-dir creation, and the read/write/edit family precondition (an existing
// file must have been fully read, and unchanged since, before it is overwritten;
// a new file needs no prior read).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mtimeMs(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime().UnixMilli()
}

func TestWrite_NewFileCreatesParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "nested", "f.txt")
	reg := NewReadRegistry()
	got, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: "hello\nworld\n"}), reg)
	if err != nil {
		t.Fatalf("write new file: %v", err)
	}
	if !got.Created || got.BytesWritten != 12 || got.LineCount != 2 {
		t.Errorf("result: %+v", got)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "hello\nworld\n" {
		t.Errorf("on-disk content = %q", string(b))
	}
}

func TestWrite_VerbatimAndEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	content := "a\tb\r\nc\x00d" // tabs, CRLF, NUL — all verbatim
	reg := NewReadRegistry()
	if _, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: content}), reg); err != nil {
		t.Fatalf("write: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Errorf("not verbatim: %q", string(b))
	}
	// empty content writes an empty file
	empty := filepath.Join(dir, "empty.txt")
	got, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: empty, Content: ""}), reg)
	if err != nil || got.BytesWritten != 0 || got.LineCount != 0 {
		t.Errorf("empty write: %+v err=%v", got, err)
	}
}

func TestWrite_OverwriteRequiresRead(t *testing.T) {
	path := writeTemp(t, "f.txt", "original\n")
	reg := NewReadRegistry()

	// no prior read of the existing file → rejected
	_, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: "new\n"}), reg)
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected not-read rejection, got %v", err)
	}

	// after a full read is recorded → allowed
	reg.MarkRead(path, mtimeMs(t, path), true)
	if _, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: "new\n"}), reg); err != nil {
		t.Fatalf("write after read: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new\n" {
		t.Errorf("content = %q", string(b))
	}
}

func TestWrite_StaleReadRejected(t *testing.T) {
	path := writeTemp(t, "f.txt", "original\n")
	reg := NewReadRegistry()
	reg.MarkRead(path, 1, true) // read observed at an ancient mtime; the file is newer
	_, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: "new\n"}), reg)
	if err == nil || !strings.Contains(err.Error(), "modified since read") {
		t.Fatalf("expected stale-read rejection, got %v", err)
	}
}

func TestWrite_PartialReadDoesNotSatisfy(t *testing.T) {
	path := writeTemp(t, "f.txt", "original\n")
	reg := NewReadRegistry()
	reg.MarkRead(path, mtimeMs(t, path), false) // partial/ranged read
	_, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: "new\n"}), reg)
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("partial read must not satisfy the write precondition, got %v", err)
	}
}

func TestWrite_PostWriteAllowsFollowup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	reg := NewReadRegistry()
	if _, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: "v1\n"}), reg); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// the write recorded read-state, so an immediate overwrite needs no re-read
	if _, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: path, Content: "v2\n"}), reg); err != nil {
		t.Fatalf("follow-up write: %v", err)
	}
}

func TestWrite_Directory(t *testing.T) {
	dir := t.TempDir()
	_, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{FilePath: dir, Content: "x"}), NewReadRegistry())
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}

func TestWrite_RequiresFilePath(t *testing.T) {
	_, err := HandleWrite(context.Background(), mustJSON(t, WriteParams{Content: "x"}), NewReadRegistry())
	if err == nil || !strings.Contains(err.Error(), "requires file_path") {
		t.Fatalf("expected requires-file_path, got %v", err)
	}
}

func TestBuildTable_HasWriteEdit(t *testing.T) {
	tbl := BuildTable(Deps{})
	for _, a := range []string{"read", "write", "edit"} {
		if _, ok := tbl[a]; !ok {
			t.Errorf("BuildTable missing %q action", a)
		}
	}
}
