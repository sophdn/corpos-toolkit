package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the cross-spelling param tolerance added for bug
// fs-surface-param-name-inconsistency-read-filepath-vs-ls-path: the
// file-operating actions (read/write/edit) canonically take `file_path`, while
// the directory/search actions (ls/glob/grep) canonically take `path`. A caller
// (esp. a small local model) that guesses the OTHER family's spelling should
// still succeed — `path` is accepted as an alias of `file_path` on read/write/
// edit, and `file_path` as an alias of `path` on ls/glob/grep. Canonical-name
// behavior (and the missing-required-param errors) is unchanged.

func raw(s string) json.RawMessage { return json.RawMessage([]byte(s)) }

func TestRead_AcceptsPathAliasForFilePath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(f, []byte("hi there\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// `path` instead of the canonical `file_path`.
	got, err := HandleRead(context.Background(), raw(`{"path":`+jsonStr(f)+`}`))
	if err != nil {
		t.Fatalf("read via path alias: %v", err)
	}
	if !strings.Contains(got.Content, "hi there") {
		t.Fatalf("content = %q, want it to contain the file body", got.Content)
	}
}

func TestWrite_AcceptsPathAliasForFilePath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "out.txt")
	_, err := HandleWrite(context.Background(), raw(`{"path":`+jsonStr(f)+`,"content":"body"}`), NewReadRegistry())
	if err != nil {
		t.Fatalf("write via path alias: %v", err)
	}
	b, err := os.ReadFile(f)
	if err != nil || string(b) != "body" {
		t.Fatalf("file = %q err=%v, want %q", string(b), err, "body")
	}
}

func TestEdit_AcceptsPathAliasForFilePath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "e.txt")
	if err := os.WriteFile(f, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewReadRegistry()
	// edit requires a prior read of the file; do it via the canonical path.
	if _, err := HandleRead(context.Background(), raw(`{"file_path":`+jsonStr(f)+`}`)); err != nil {
		t.Fatal(err)
	}
	noteRead(reg, raw(`{"file_path":`+jsonStr(f)+`}`))
	_, err := HandleEdit(context.Background(), raw(`{"path":`+jsonStr(f)+`,"old_string":"alpha","new_string":"beta"}`), reg)
	if err != nil {
		t.Fatalf("edit via path alias: %v", err)
	}
	b, _ := os.ReadFile(f)
	if string(b) != "beta" {
		t.Fatalf("file = %q, want beta", string(b))
	}
}

func TestLS_AcceptsFilePathAliasForPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// `file_path` instead of the canonical `path`.
	got, err := HandleLS(context.Background(), raw(`{"file_path":`+jsonStr(dir)+`}`))
	if err != nil {
		t.Fatalf("ls via file_path alias: %v", err)
	}
	if got.Path != dir || got.Count != 1 {
		t.Fatalf("ls = %+v, want path=%s count=1", got, dir)
	}
}

func TestGlob_AcceptsFilePathAliasForPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "z.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HandleGlob(context.Background(), raw(`{"pattern":"*.txt","file_path":`+jsonStr(dir)+`}`))
	if err != nil {
		t.Fatalf("glob via file_path alias: %v", err)
	}
	if len(got.Filenames) != 1 {
		t.Fatalf("glob filenames = %v, want exactly one", got.Filenames)
	}
}

func TestGrep_AcceptsFilePathAliasForPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte("findme here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := HandleGrep(context.Background(), raw(`{"pattern":"findme","file_path":`+jsonStr(dir)+`}`))
	if err != nil {
		t.Fatalf("grep via file_path alias: %v", err)
	}
	if got.NumFiles == 0 || len(got.Filenames) == 0 {
		t.Fatalf("grep found no matches via file_path alias: %+v", got)
	}
}

// jsonStr quotes s as a JSON string literal for embedding in a raw payload.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
