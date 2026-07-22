package fs

// read_test.go is the characterization net for fs.read — the committed contract
// the implementation is built to pass. The expectations pin the read contract:
// the numbered-line format (unpadded "<n>\t<content>", split on newline, no
// trailing newline), the offset/limit range semantics, BOM/CRLF handling, the
// trailing-empty-line rule, the byte cap, and the empty/short-offset warnings.
// See testdata/parity/OBSERVED_HARNESS_CONTRACT.md for the contract narrative.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

func TestRead_FullFile(t *testing.T) {
	// File ends in "\n" ⇒ trailing empty numbered line (split-on-"\n"), unpadded
	// line numbers, NO trailing newline on the joined output.
	path := writeTemp(t, "f.txt", "alpha\nbeta\ngamma\n")
	got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "1\talpha\n2\tbeta\n3\tgamma\n4\t"
	if got.Content != want {
		t.Errorf("content mismatch:\n got %q\nwant %q", got.Content, want)
	}
	if got.LineCount != 4 || got.StartLine != 1 || got.TotalLines != 4 {
		t.Errorf("metadata: %+v", got)
	}
}

func TestRead_NoTrailingNewline(t *testing.T) {
	path := writeTemp(t, "f.txt", "one\ntwo")
	got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "1\tone\n2\ttwo"
	if got.Content != want {
		t.Errorf("content mismatch:\n got %q\nwant %q", got.Content, want)
	}
	if got.TotalLines != 2 {
		t.Errorf("expected 2 total lines, got %+v", got)
	}
}

func TestRead_CRLF_and_BOM(t *testing.T) {
	// CRLF is normalized to LF (trailing "\r" stripped per line); a leading UTF-8
	// BOM is stripped.
	path := writeTemp(t, "f.txt", "\ufeffa\r\nb\r\n")
	got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "1\ta\n2\tb\n3\t"
	if got.Content != want {
		t.Errorf("content mismatch:\n got %q\nwant %q", got.Content, want)
	}
}

func TestRead_LineRange(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "line%d\n", i)
	}
	path := writeTemp(t, "f.txt", sb.String())

	got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path, Offset: 3, Limit: 4}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "3\tline3\n4\tline4\n5\tline5\n6\tline6"
	if got.Content != want {
		t.Errorf("content mismatch:\n got %q\nwant %q", got.Content, want)
	}
	if got.StartLine != 3 || got.LineCount != 4 || got.TotalLines != 11 {
		t.Errorf("metadata: %+v", got)
	}
}

func TestRead_OffsetPastEOF_Warns(t *testing.T) {
	path := writeTemp(t, "f.txt", "a\nb\n") // 3 parts incl trailing empty
	got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path, Offset: 99}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Content != "" {
		t.Errorf("expected empty content past EOF, got %q", got.Content)
	}
	want := "<system-reminder>Warning: the file exists but is shorter than the provided offset (99). The file has 3 lines.</system-reminder>"
	if got.Warning != want {
		t.Errorf("warning mismatch:\n got %q\nwant %q", got.Warning, want)
	}
}

func TestRead_EmptyFile_Warns(t *testing.T) {
	// Empty file: the split yields one empty part (totalLines=1), content is ""
	// ⇒ the shorter-than-offset warning is emitted.
	path := writeTemp(t, "f.txt", "")
	got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "<system-reminder>Warning: the file exists but is shorter than the provided offset (1). The file has 1 lines.</system-reminder>"
	if got.Warning != want {
		t.Errorf("warning mismatch:\n got %q\nwant %q", got.Warning, want)
	}
}

func TestRead_ByteCap(t *testing.T) {
	big := strings.Repeat("x", 400*1024) // 400KB, single line, > 256KB cap
	path := writeTemp(t, "big.txt", big)

	// Whole-file read trips the byte cap.
	_, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path}))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size (256KB)") {
		t.Fatalf("expected byte-cap error mentioning 256KB, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "(400KB)") {
		t.Errorf("expected actual size 400KB in message, got %v", err)
	}

	// A ranged read (limit set) skips the cap.
	if _, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path, Offset: 1, Limit: 1})); err != nil {
		t.Errorf("ranged read should skip byte cap, got %v", err)
	}
}

func TestRead_MissingFile(t *testing.T) {
	_, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: "/no/such/file/xyz"}))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected does-not-exist error, got %v", err)
	}
}

func TestRead_Directory(t *testing.T) {
	dir := t.TempDir()
	_, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: dir}))
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}

func TestRead_MissingFilePath(t *testing.T) {
	_, err := HandleRead(context.Background(), mustJSON(t, ReadParams{}))
	if err == nil || !strings.Contains(err.Error(), "requires file_path") {
		t.Fatalf("expected requires-file_path error, got %v", err)
	}
}

func TestHumanByteSize(t *testing.T) {
	cases := map[int64]string{
		0:               "0 bytes",
		512:             "512 bytes",
		1024:            "1KB",
		1536:            "1.5KB",
		256 * 1024:      "256KB",
		400 * 1024:      "400KB",
		1024 * 1024:     "1MB",
		3 * 1024 * 1024: "3MB",
	}
	for in, want := range cases {
		if got := humanByteSize(in); got != want {
			t.Errorf("humanByteSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildTable_HasRead(t *testing.T) {
	tbl := BuildTable(Deps{})
	if _, ok := tbl["read"]; !ok {
		t.Fatal("BuildTable missing read action")
	}
}

// TestRead_HarnessParity is the committed parity oracle: fs.read's Content for
// the fixtures in testdata/parity/tree/ must equal the output the harness Read
// produces for the same files, per the source contract in
// OBSERVED_HARNESS_CONTRACT.md. The deny-list swap leans on this — if someone
// "fixes" the format back toward cat -n (padding / trailing newline), it fails.
func TestRead_HarnessParity(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"alpha.txt", "1\talpha\n2\tbeta\n3\tgamma\n4\t"},
		{"notrail.txt", "1\tone\n2\ttwo"},
		{"indent.txt", "1\tnoindent\n2\t    fourspaces\n3\t\t\ttabbed\n4\t"},
		{
			"big.txt",
			"1\tL1 needle\n2\tL2\n3\tL3 needle here\n4\tL4\n5\tL5 NEEDLE upper\n" +
				"6\tL6\n7\tL7\n8\tL8\n9\tL9\n10\tL10\n11\tL11\n12\tL12 last\n13\t",
		},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			path := filepath.Join("testdata", "parity", "tree", c.file)
			got, err := HandleRead(context.Background(), mustJSON(t, ReadParams{FilePath: path}))
			if err != nil {
				t.Fatalf("read %s: %v", c.file, err)
			}
			if got.Content != c.want {
				t.Errorf("harness parity mismatch for %s:\n got %q\nwant %q", c.file, got.Content, c.want)
			}
		})
	}
}
