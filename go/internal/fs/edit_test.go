package fs

// edit_test.go is the characterization net for fs.edit — exact-string replace
// with uniqueness/replace_all semantics, CRLF→LF normalization, the empty
// old_string create/fill cases, and the read/write/edit family precondition.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readEdit marks path as fully read at its current mtime, the precondition an
// existing-file edit requires.
func markFullRead(t *testing.T, reg *ReadRegistry, path string) {
	t.Helper()
	reg.MarkRead(path, mtimeMs(t, path), true)
}

func TestEdit_SingleReplace(t *testing.T) {
	path := writeTemp(t, "f.txt", "alpha beta gamma\n")
	reg := NewReadRegistry()
	markFullRead(t, reg, path)
	got, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "beta", NewString: "BETA"}), reg)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got.Replacements != 1 {
		t.Errorf("replacements = %d", got.Replacements)
	}
	if b, _ := os.ReadFile(path); string(b) != "alpha BETA gamma\n" {
		t.Errorf("content = %q", string(b))
	}
}

func TestEdit_RequiresRead(t *testing.T) {
	path := writeTemp(t, "f.txt", "x\n")
	_, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "x", NewString: "y"}), NewReadRegistry())
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("expected not-read rejection, got %v", err)
	}
}

func TestEdit_NotFound(t *testing.T) {
	path := writeTemp(t, "f.txt", "hello\n")
	reg := NewReadRegistry()
	markFullRead(t, reg, path)
	_, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "absent", NewString: "x"}), reg)
	if err == nil || !strings.Contains(err.Error(), "not found in file") {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestEdit_AmbiguousWithoutReplaceAll(t *testing.T) {
	path := writeTemp(t, "f.txt", "x x x\n")
	reg := NewReadRegistry()
	markFullRead(t, reg, path)
	_, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "x", NewString: "y"}), reg)
	if err == nil || !strings.Contains(err.Error(), "Found 3 matches") {
		t.Fatalf("expected ambiguity error naming the count, got %v", err)
	}
}

func TestEdit_ReplaceAll(t *testing.T) {
	path := writeTemp(t, "f.txt", "x x x\n")
	reg := NewReadRegistry()
	markFullRead(t, reg, path)
	got, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "x", NewString: "y", ReplaceAll: true}), reg)
	if err != nil {
		t.Fatalf("replace_all: %v", err)
	}
	if got.Replacements != 3 {
		t.Errorf("replacements = %d", got.Replacements)
	}
	if b, _ := os.ReadFile(path); string(b) != "y y y\n" {
		t.Errorf("content = %q", string(b))
	}
}

func TestEdit_NoChanges(t *testing.T) {
	path := writeTemp(t, "f.txt", "x\n")
	reg := NewReadRegistry()
	markFullRead(t, reg, path)
	_, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "x", NewString: "x"}), reg)
	if err == nil || !strings.Contains(err.Error(), "No changes to make") {
		t.Fatalf("expected no-changes error, got %v", err)
	}
}

func TestEdit_CRLFNormalizedToLF(t *testing.T) {
	path := writeTemp(t, "f.txt", "one\r\ntwo\r\nthree\r\n")
	reg := NewReadRegistry()
	markFullRead(t, reg, path)
	if _, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "two", NewString: "TWO"}), reg); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// matched against LF-normalized content; written back with LF endings
	if b, _ := os.ReadFile(path); string(b) != "one\nTWO\nthree\n" {
		t.Errorf("content = %q (CRLF should normalize to LF)", string(b))
	}
}

func TestEdit_NewFileViaEmptyOldString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	got, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "", NewString: "created body\n"}), NewReadRegistry())
	if err != nil {
		t.Fatalf("create via empty old_string: %v", err)
	}
	if !got.Created {
		t.Errorf("expected created=true, got %+v", got)
	}
	if b, _ := os.ReadFile(path); string(b) != "created body\n" {
		t.Errorf("content = %q", string(b))
	}
}

func TestEdit_EmptyOldStringOnNonEmptyFile(t *testing.T) {
	path := writeTemp(t, "f.txt", "already here\n")
	reg := NewReadRegistry()
	markFullRead(t, reg, path)
	_, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: path, OldString: "", NewString: "x"}), reg)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
}

func TestEdit_NonexistentNonEmptyOld(t *testing.T) {
	_, err := HandleEdit(context.Background(), mustJSON(t, EditParams{FilePath: "/no/such/file/zzz", OldString: "x", NewString: "y"}), NewReadRegistry())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected does-not-exist, got %v", err)
	}
}

// TestFamily_ReadThenEditSelfConsistent is the load-bearing test for the
// family: an edit to an existing file is REJECTED until fs.read (through the
// surface) records it, then SUCCEEDS — proving the owned read satisfies the
// owned edit precondition with no harness read-state involved.
func TestFamily_ReadThenEditSelfConsistent(t *testing.T) {
	reg := NewReadRegistry()
	tbl := BuildTable(Deps{Reads: reg})
	path := writeTemp(t, "f.txt", "hello world\n")
	ctx := context.Background()
	editParams := mustJSON(t, EditParams{FilePath: path, OldString: "world", NewString: "there"})

	// edit before any read → rejected
	if _, err := tbl["edit"](ctx, "", editParams); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("edit before read should be rejected, got %v", err)
	}
	// read through the surface (records into the shared registry)
	if _, err := tbl["read"](ctx, "", mustJSON(t, ReadParams{FilePath: path})); err != nil {
		t.Fatalf("read: %v", err)
	}
	// now the edit is allowed
	res, err := tbl["edit"](ctx, "", editParams)
	if err != nil {
		t.Fatalf("edit after read: %v", err)
	}
	if er, ok := res.(EditResult); !ok || er.Replacements != 1 {
		t.Errorf("result: %+v ok=%v", res, ok)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello there\n" {
		t.Errorf("content = %q", string(b))
	}
}
