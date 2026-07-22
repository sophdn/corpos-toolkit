package fs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrep_ContextLines(t *testing.T) {
	requireRg(t)
	ctxN := 1
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "L3 needle here", Path: filepath.Join(parityTree, "big.txt"), OutputMode: "content", Context: &ctxN,
	}))
	if err != nil {
		t.Fatalf("grep -C1: %v", err)
	}
	// Context must include the neighbours L2 and L4, and stay relative-pathed
	// (the context-line "-" separator must not leak an absolute path).
	for _, want := range []string{"L2", "L4"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("context output missing %q:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "/big.txt") {
		t.Errorf("context line leaked an absolute path:\n%s", res.Content)
	}
}

func TestGrep_TypeFilter(t *testing.T) {
	requireRg(t)
	insensitive := true
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: parityTree, Type: "md", CaseInsensitive: &insensitive,
	}))
	if err != nil {
		t.Fatalf("grep --type md: %v", err)
	}
	if got := sortedSet(res.Filenames); strings.Join(got, ",") != "readme.md" {
		t.Errorf("type=md = %v, want [readme.md]", got)
	}
}

func TestGrep_Multiline(t *testing.T) {
	requireRg(t)
	// "Widget\sneedle" only matches across lines under multiline. widget.go has
	// "// Widget needle" on one line, so use a cross-line pattern in big.txt:
	// "needle\nL2" spans L1→L2.
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: `needle\nL2`, Path: filepath.Join(parityTree, "big.txt"), OutputMode: "content", Multiline: true,
	}))
	if err != nil {
		t.Fatalf("grep multiline: %v", err)
	}
	if res.NumLines == 0 {
		t.Errorf("multiline pattern matched nothing:\n%s", res.Content)
	}
}

func TestGrep_OffsetPaging(t *testing.T) {
	requireRg(t)
	insensitive := true
	// 3 matches in big.txt; offset 1 + content mode → 2 remaining lines.
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: filepath.Join(parityTree, "big.txt"), OutputMode: "content",
		CaseInsensitive: &insensitive, Offset: 1,
	}))
	if err != nil {
		t.Fatalf("grep offset: %v", err)
	}
	if res.AppliedOffset != 1 {
		t.Errorf("applied_offset = %d, want 1", res.AppliedOffset)
	}
	if res.NumLines != 2 {
		t.Errorf("num_lines = %d, want 2 after offset 1", res.NumLines)
	}
}

func TestGrep_HeadLimitZeroUnlimited(t *testing.T) {
	requireRg(t)
	insensitive := true
	zero := 0
	res, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{
		Pattern: "needle", Path: filepath.Join(parityTree, "big.txt"), OutputMode: "content",
		CaseInsensitive: &insensitive, HeadLimit: &zero,
	}))
	if err != nil {
		t.Fatalf("grep head_limit=0: %v", err)
	}
	if res.NumLines != 3 {
		t.Errorf("num_lines = %d, want 3 (unlimited)", res.NumLines)
	}
	if res.AppliedLimit != 0 {
		t.Errorf("applied_limit = %d, want 0 (unlimited)", res.AppliedLimit)
	}
}

func TestGrep_InvalidOutputMode(t *testing.T) {
	if _, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Pattern: "x", OutputMode: "bogus"})); err == nil {
		t.Error("invalid output_mode = nil error, want error")
	}
}

func TestGrep_NonexistentPath(t *testing.T) {
	if _, err := HandleGrep(context.Background(), mustJSON(t, GrepParams{Pattern: "x", Path: filepath.Join(t.TempDir(), "nope")})); err == nil {
		t.Error("nonexistent path = nil error, want error")
	}
}

func TestSplitGlobPatterns(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"*.go", []string{"*.go"}},
		{"*.ts,*.tsx", []string{"*.ts", "*.tsx"}},
		{"*.go *.md", []string{"*.go", "*.md"}},
		{"*.{ts,tsx}", []string{"*.{ts,tsx}"}}, // brace group preserved
	}
	for _, c := range cases {
		got := splitGlobPatterns(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitGlobPatterns(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestGlob_NonexistentPath(t *testing.T) {
	if _, err := HandleGlob(context.Background(), mustJSON(t, GlobParams{Pattern: "*", Path: filepath.Join(t.TempDir(), "nope")})); err == nil {
		t.Error("glob nonexistent path = nil error, want error")
	}
}

func TestGlob_PathIsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f.txt")
	mustWrite(t, f, "x")
	if _, err := HandleGlob(context.Background(), mustJSON(t, GlobParams{Pattern: "*", Path: f})); err == nil {
		t.Error("glob with file path = nil error, want error")
	}
}
