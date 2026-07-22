package fs

// glob_test.go is the characterization net for fs.glob — the committed contract
// the ripgrep-backed implementation is built to pass. It pins recursive vs
// any-depth matching, the relative-to-search-root paths, no-match, and the
// 100-file cap with the truncated flag. See testdata/parity/GREP_GLOB_CONTRACT.md.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGlob_Recursive(t *testing.T) {
	requireRg(t)
	res, err := HandleGlob(context.Background(), mustJSON(t, GlobParams{Pattern: "**/*.go", Path: parityTree}))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	want := []string{"sub/helper.go", "sub/widget.go"}
	if got := sortedSet(res.Filenames); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("glob **/*.go = %v, want %v", got, want)
	}
	if res.NumFiles != 2 {
		t.Errorf("num_files = %d, want 2", res.NumFiles)
	}
	if res.Truncated {
		t.Error("truncated = true, want false")
	}
}

func TestGlob_AnyDepthBareStar(t *testing.T) {
	requireRg(t)
	// ripgrep glob dialect: a bare *.txt matches at any depth.
	res, err := HandleGlob(context.Background(), mustJSON(t, GlobParams{Pattern: "*.txt", Path: parityTree}))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	want := []string{"alpha.txt", "big.txt", "indent.txt", "notrail.txt"}
	if got := sortedSet(res.Filenames); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("glob *.txt = %v, want %v", got, want)
	}
}

func TestGlob_NoMatch(t *testing.T) {
	requireRg(t)
	res, err := HandleGlob(context.Background(), mustJSON(t, GlobParams{Pattern: "**/*.rs", Path: parityTree}))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if res.NumFiles != 0 || res.Truncated {
		t.Errorf("no-match: num_files=%d truncated=%v, want 0/false", res.NumFiles, res.Truncated)
	}
}

func TestGlob_TruncatesAt100(t *testing.T) {
	requireRg(t)
	dir := t.TempDir()
	for i := 0; i < 150; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)+".dat"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := HandleGlob(context.Background(), mustJSON(t, GlobParams{Pattern: "*.dat", Path: dir}))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if res.NumFiles != 100 {
		t.Errorf("num_files = %d, want 100 (cap)", res.NumFiles)
	}
	if !res.Truncated {
		t.Error("truncated = false, want true")
	}
}

func TestGlob_RequiresPattern(t *testing.T) {
	if _, err := HandleGlob(context.Background(), mustJSON(t, GlobParams{Path: parityTree})); err == nil {
		t.Error("glob without pattern = nil error, want error")
	}
}
