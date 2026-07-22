package fs

// grep_test.go is the characterization net for fs.grep — the committed contract
// the ripgrep-backed implementation is built to pass. It pins the output modes
// (files_with_matches / content / count), case sensitivity, the glob filter,
// head_limit/offset paging, relative-to-search-root paths, and leading-dash
// pattern handling. See testdata/parity/GREP_GLOB_CONTRACT.md.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const parityTree = "testdata/parity/tree"

func requireRg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed; fs.grep/fs.glob require it")
	}
}

func sortedSet(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func TestGrep_FilesWithMatches_Default(t *testing.T) {
	requireRg(t)
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Pattern: "needle", Path: parityTree}))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if res.Mode != "files_with_matches" {
		t.Errorf("mode = %q, want files_with_matches", res.Mode)
	}
	// lowercase "needle": big.txt (L1,L3), readme.md, sub/widget.go.
	want := []string{"big.txt", "readme.md", "sub/widget.go"}
	if got := sortedSet(res.Filenames); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("filenames = %v, want %v", got, want)
	}
	if res.NumFiles != 3 {
		t.Errorf("num_files = %d, want 3", res.NumFiles)
	}
}

func TestGrep_CaseSensitiveVsInsensitive(t *testing.T) {
	requireRg(t)
	// "NEEDLE" case-sensitive only matches big.txt (L5).
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Pattern: "NEEDLE", Path: parityTree}))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if got := sortedSet(res.Filenames); strings.Join(got, ",") != "big.txt" {
		t.Errorf("case-sensitive NEEDLE = %v, want [big.txt]", got)
	}
	// case-insensitive widens to all three.
	insensitive := true
	res2, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Pattern: "NEEDLE", Path: parityTree, CaseInsensitive: &insensitive}))
	if err != nil {
		t.Fatalf("grep -i: %v", err)
	}
	want := []string{"big.txt", "readme.md", "sub/widget.go"}
	if got := sortedSet(res2.Filenames); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("case-insensitive = %v, want %v", got, want)
	}
}

func TestGrep_ContentMode_WithLineNumbers(t *testing.T) {
	requireRg(t)
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: filepath.Join(parityTree, "big.txt"), OutputMode: "content",
	}))
	if err != nil {
		t.Fatalf("grep content: %v", err)
	}
	if res.Mode != "content" {
		t.Errorf("mode = %q, want content", res.Mode)
	}
	for _, want := range []string{"big.txt:1:L1 needle", "big.txt:3:L3 needle here"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
	if res.NumLines != 2 {
		t.Errorf("num_lines = %d, want 2", res.NumLines)
	}
}

func TestGrep_ContentMode_NoLineNumbers(t *testing.T) {
	requireRg(t)
	no := false
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: filepath.Join(parityTree, "big.txt"), OutputMode: "content", ShowLineNumbers: &no,
	}))
	if err != nil {
		t.Fatalf("grep content -n=false: %v", err)
	}
	if strings.Contains(res.Content, "big.txt:1:") {
		t.Errorf("line numbers present despite -n=false:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "big.txt:L1 needle") {
		t.Errorf("content missing path:text form:\n%s", res.Content)
	}
}

func TestGrep_CountMode(t *testing.T) {
	requireRg(t)
	insensitive := true
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: parityTree, OutputMode: "count", CaseInsensitive: &insensitive,
	}))
	if err != nil {
		t.Fatalf("grep count: %v", err)
	}
	if res.Mode != "count" {
		t.Errorf("mode = %q, want count", res.Mode)
	}
	// big.txt:3 (L1,L3,L5), readme.md:1, sub/widget.go:2 → total 6 across 3 files.
	if res.NumMatches != 6 {
		t.Errorf("num_matches = %d, want 6\n%s", res.NumMatches, res.Content)
	}
	if res.NumFiles != 3 {
		t.Errorf("num_files = %d, want 3", res.NumFiles)
	}
}

func TestGrep_GlobFilter(t *testing.T) {
	requireRg(t)
	insensitive := true
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: parityTree, Glob: "*.md", CaseInsensitive: &insensitive,
	}))
	if err != nil {
		t.Fatalf("grep glob: %v", err)
	}
	if got := sortedSet(res.Filenames); strings.Join(got, ",") != "readme.md" {
		t.Errorf("glob *.md = %v, want [readme.md]", got)
	}
}

func TestGrep_NoMatches(t *testing.T) {
	requireRg(t)
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Pattern: "zzz-no-such-token", Path: parityTree}))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if res.NumFiles != 0 || len(res.Filenames) != 0 {
		t.Errorf("expected no matches, got %d files %v", res.NumFiles, res.Filenames)
	}
}

func TestGrep_HeadLimitTruncates(t *testing.T) {
	requireRg(t)
	// content mode over big.txt, case-insensitive "needle" → 3 lines; cap at 2.
	insensitive := true
	limit := 2
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: filepath.Join(parityTree, "big.txt"), OutputMode: "content",
		CaseInsensitive: &insensitive, HeadLimit: &limit,
	}))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if res.NumLines != 2 {
		t.Errorf("num_lines = %d, want 2 (capped)", res.NumLines)
	}
	if res.AppliedLimit != 2 {
		t.Errorf("applied_limit = %d, want 2 (truncation occurred)", res.AppliedLimit)
	}
}

func TestGrep_LeadingDashPattern(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a line with -needle in it\nplain line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Pattern: "-needle", Path: dir}))
	if err != nil {
		t.Fatalf("grep leading-dash: %v", err)
	}
	if got := sortedSet(res.Filenames); strings.Join(got, ",") != "f.txt" {
		t.Errorf("leading-dash pattern = %v, want [f.txt]", got)
	}
}

func TestGrep_RequiresPattern(t *testing.T) {
	if _, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Path: parityTree})); err == nil {
		t.Error("grep without pattern = nil error, want error")
	}
}
