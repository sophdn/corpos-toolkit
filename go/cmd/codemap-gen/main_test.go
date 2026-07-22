package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRootForTest returns the workspace root by climbing from this test
// file's location. We don't shell out to `git rev-parse` because the
// test should remain hermetic and self-contained.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// wd is .../mcp-servers/go/cmd/codemap-gen → up three levels.
	root := filepath.Join(wd, "..", "..", "..")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// TestGenerate_Deterministic asserts two consecutive runs against the
// same tree produce byte-identical output. Drift here would mean the
// precommit --check gate flaps.
func TestGenerate_Deterministic(t *testing.T) {
	root := repoRootForTest(t)
	a, err := generate(root)
	if err != nil {
		t.Fatalf("generate (run 1): %v", err)
	}
	b, err := generate(root)
	if err != nil {
		t.Fatalf("generate (run 2): %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("non-deterministic output:\nrun1 len=%d, run2 len=%d", len(a), len(b))
	}
}

// TestLintGoDocs_PassesOnCleanTree confirms the live tree satisfies
// the convention. If this fails, an internal package is missing a
// doc.go four-field block.
func TestLintGoDocs_PassesOnCleanTree(t *testing.T) {
	root := repoRootForTest(t)
	if err := lintGoDocs(root); err != nil {
		t.Fatalf("lintGoDocs reported issues on the clean tree:\n%v", err)
	}
}

// TestLintGoDocs_DetectsMissingDocGo confirms the lint fails when a
// fresh package directory ships without a doc.go.
func TestLintGoDocs_DetectsMissingDocGo(t *testing.T) {
	root := newStagedTree(t)
	pkg := filepath.Join(root, "go", "internal", "_test_missing_doc")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir test pkg: %v", err)
	}
	// Drop a non-doc Go file so the directory is a real package.
	if err := os.WriteFile(filepath.Join(pkg, "stub.go"), []byte("package _test_missing_doc\n"), 0o644); err != nil {
		t.Fatalf("write stub.go: %v", err)
	}

	err := lintGoDocs(root)
	if err == nil {
		t.Fatalf("lintGoDocs returned nil; expected failure for missing doc.go")
	}
	if !strings.Contains(err.Error(), "missing doc.go") {
		t.Fatalf("error did not mention missing doc.go:\n%v", err)
	}
}

// TestLintGoDocs_DetectsCorruptedBlock confirms the lint fails when a
// doc.go ships with a renamed or dropped field in the four-field block.
func TestLintGoDocs_DetectsCorruptedBlock(t *testing.T) {
	root := newStagedTree(t)
	pkg := filepath.Join(root, "go", "internal", "_test_corrupted_doc")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Rename "Non-goals" → "Not for" (corruption).
	doc := `// Package _test_corrupted_doc tests the lint.
//
// ## Intended use
//
// **Workflow served:** stub
//
// **Invocation pattern:** stub
//
// **Success shape:** stub
//
// **Not for:** stub
package _test_corrupted_doc
`
	if err := os.WriteFile(filepath.Join(pkg, "doc.go"), []byte(doc), 0o644); err != nil {
		t.Fatalf("write doc.go: %v", err)
	}

	err := lintGoDocs(root)
	if err == nil {
		t.Fatalf("lintGoDocs returned nil; expected failure for corrupted block")
	}
	if !strings.Contains(err.Error(), "Non-goals") {
		t.Fatalf("error did not mention missing Non-goals field:\n%v", err)
	}
}

// TestGenerate_ForwardCompatPackageSurface confirms a stub package
// dropped under go/internal/ surfaces in CODEMAP without any code
// modification. This is the cross-chain forward-compat seam call-out
// from the chain spec.
func TestGenerate_ForwardCompatPackageSurface(t *testing.T) {
	root := newStagedTree(t)
	pkgName := "_test_forward_compat_pkg"
	pkg := filepath.Join(root, "go", "internal", pkgName)
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := fmt.Sprintf(`// Package %s is a stub used by codemap-gen tests.
//
// ## Intended use
//
// **Workflow served:** test stub
//
// **Invocation pattern:** test stub
//
// **Success shape:** test stub
//
// **Non-goals:** test stub
package %s
`, pkgName, pkgName)
	if err := os.WriteFile(filepath.Join(pkg, "doc.go"), []byte(doc), 0o644); err != nil {
		t.Fatalf("write doc.go: %v", err)
	}

	out, err := generate(root)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !bytes.Contains(out, []byte(pkgName)) {
		t.Fatalf("generated CODEMAP did not mention new package %s", pkgName)
	}
}

// TestFirstSentence asserts the heuristic doesn't truncate on `.md`,
// `etc.`, or `~/.path` patterns where the terminator isn't followed by
// whitespace.
func TestFirstSentence(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Reflex to consult ~/.claude/vault/ before starting a task.", "Reflex to consult ~/.claude/vault/ before starting a task."},
		{"Audit a codebase using the model. Second sentence is dropped.", "Audit a codebase using the model."},
		{"No terminator at all", "No terminator at all"},
		{"Includes (Wikipedia, etc.) inline. Drop after.", "Includes (Wikipedia, etc.) inline."},
		{"Path is /tmp/foo.md and persists.", "Path is /tmp/foo.md and persists."},
	}
	for _, c := range cases {
		got := firstSentence(c.in)
		if got != c.want {
			t.Errorf("firstSentence(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// newStagedTree copies the live repo into a temp dir so tests can mutate
// it (add stub packages, etc.) without contaminating the workspace.
// Symlinks under go/migrations and crates/shared-db are preserved so
// embedded SQL stays resolvable. This is fast enough because we only
// copy the directories codemap-gen actually reads.
func newStagedTree(t *testing.T) string {
	t.Helper()
	src := repoRootForTest(t)
	dst := t.TempDir()

	// Initialise an empty git repo so `git rev-parse --show-toplevel`
	// is happy if any helper shells out (none currently do, but the
	// codemap-gen binary's repoRoot() does — we test generate()
	// directly with an explicit root).
	if out, err := exec.Command("git", "init", "-q", dst).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// Copy only the directories codemap-gen reads, plus a few
	// adjacent files we may want to reference in CODEMAP.
	roots := []string{
		"action-manifests",
		"blueprints/forge-schemas",
		"go/internal",
		"crates",
		"benchmarks/src",
		"inference-clients/src",
		"skills",
		"scripts",
	}
	for _, r := range roots {
		srcDir := filepath.Join(src, r)
		dstDir := filepath.Join(dst, r)
		if _, err := os.Stat(srcDir); err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dstDir), 0o755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", dstDir, err)
		}
		if out, err := exec.Command("cp", "-r", srcDir, filepath.Dir(dstDir)).CombinedOutput(); err != nil {
			t.Fatalf("cp %s → %s: %v\n%s", srcDir, dstDir, err, out)
		}
	}
	return dst
}
