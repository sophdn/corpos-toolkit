package fs

// path_test.go is the regression net for fs-surface-does-not-expand-leading-tilde:
// a leading ~ must expand to $HOME, so a caller-supplied "~/x" lands at $HOME/x
// rather than silently creating a literal "~" directory under the daemon's CWD.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExpandUserPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", home},
		{"tilde slash", "~/x/y.go", filepath.Join(home, "x/y.go")},
		{"tilde root", "~/", home},
		{"absolute passthrough", "/etc/hosts", "/etc/hosts"},
		{"relative passthrough", "sub/f.go", "sub/f.go"},
		{"other-user tilde untouched", "~bob/x", "~bob/x"},
		{"mid-string tilde untouched", "/a/~/b", "/a/~/b"},
		{"empty passthrough", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expandUserPath(c.in); got != c.want {
				t.Errorf("expandUserPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// When the home directory cannot be resolved, a "~"-prefixed path is left
// untouched (surfaced downstream as a missing path) rather than guessed.
func TestExpandUserPath_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	for _, in := range []string{"~", "~/x"} {
		if got := expandUserPath(in); got != in {
			t.Errorf("expandUserPath(%q) with no HOME = %q, want unchanged", in, got)
		}
	}
}

// The end-to-end guard: fs.write of a tilde path lands under $HOME, NOT under a
// literal "~" directory in the process CWD, and a follow-up tilde read/write
// round-trips through the same expanded key (the read/write precondition holds).
func TestWrite_TildePathLandsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	reg := NewReadRegistry()
	const rel = "dev/run3/lru.go"
	res, err := HandleWrite(context.Background(),
		mustJSON(t, WriteParams{FilePath: "~/" + rel, Content: "package lru\n"}), reg)
	if err != nil {
		t.Fatalf("write tilde path: %v", err)
	}

	want := filepath.Join(home, rel)
	// The file is at $HOME/dev/run3/lru.go ...
	if b, rerr := os.ReadFile(want); rerr != nil || string(b) != "package lru\n" {
		t.Fatalf("expected file at %s; read err=%v content=%q", want, rerr, string(b))
	}
	if res.FilePath != want {
		t.Errorf("result FilePath = %q, want expanded %q", res.FilePath, want)
	}
	// ... and NOT under a literal "~" directory in the CWD (the bug's symptom).
	if _, serr := os.Stat("~"); serr == nil {
		t.Errorf("a literal '~' directory was created in CWD — tilde not expanded")
	}

	// A tilde read records read-state under the same expanded key, so an
	// overwrite (which requires a prior read of an existing file) is permitted.
	if _, rerr := HandleRead(context.Background(),
		mustJSON(t, ReadParams{FilePath: "~/" + rel})); rerr != nil {
		t.Fatalf("read tilde path: %v", rerr)
	}
	noteRead(reg, mustJSON(t, ReadParams{FilePath: "~/" + rel}))
	if _, werr := HandleWrite(context.Background(),
		mustJSON(t, WriteParams{FilePath: "~/" + rel, Content: "package lru // v2\n"}), reg); werr != nil {
		t.Fatalf("overwrite after tilde read should succeed, got: %v", werr)
	}
}
