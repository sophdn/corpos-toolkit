package measure

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBenchPath(t *testing.T) {
	cwd, _ := os.Getwd()
	cases := []struct {
		name string
		p    string
		root string
		want string
	}{
		{"empty stays empty", "", "/proj", ""},
		{"absolute unchanged (root ignored)", "/abs/bin", "/proj", "/abs/bin"},
		{"relative joins project root", "go/bin/x", "/proj", "/proj/go/bin/x"},
		{"relative with empty root falls back to cwd", "go/bin/x", "", filepath.Join(cwd, "go/bin/x")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveBenchPath(c.p, c.root); got != c.want {
				t.Fatalf("resolveBenchPath(%q, %q) = %q, want %q", c.p, c.root, got, c.want)
			}
		})
	}
}
