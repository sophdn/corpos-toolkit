package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingGit is a fake gitFn that answers a fixed map of git queries
// and records every call so config-mutating calls can be asserted. Any
// unmatched query returns "" (the benign answer for `git config …`).
type recordingGit struct {
	answers map[string]string
	calls   []string
}

func (g *recordingGit) fn(dir string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	g.calls = append(g.calls, key)
	if v, ok := g.answers[key]; ok {
		return v, nil
	}
	return "", nil
}

func (g *recordingGit) called(substr string) bool {
	for _, c := range g.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// mainCheckoutGit builds a fake git for a MAIN checkout rooted at root.
func mainCheckoutGit(root string) *recordingGit {
	return &recordingGit{answers: map[string]string{
		"rev-parse --show-toplevel":                         root,
		"rev-parse --path-format=absolute --git-common-dir": filepath.Join(root, ".git"),
		"rev-parse --path-format=absolute --git-path hooks": filepath.Join(root, ".git", "hooks"),
	}}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestInitGoMainCheckout proves init on a Go main checkout writes a
// go-stack starter gate.yml + both hooks into .git/hooks, and is
// idempotent (second run keeps gate.yml, rewrites hooks).
func TestInitGoMainCheckout(t *testing.T) {
	root := t.TempDir()
	// A go.mod at the root → stack go, unit ".".
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	g := mainCheckoutGit(root)
	var out bytes.Buffer
	if err := initRepo(root, g.fn, &out); err != nil {
		t.Fatalf("initRepo: %v", err)
	}

	cfg := readFile(t, filepath.Join(root, "gate.yml"))
	if !strings.Contains(cfg, "stack: go") || !strings.Contains(cfg, "dir: .") {
		t.Fatalf("go starter gate.yml wrong:\n%s", cfg)
	}
	if !strings.Contains(cfg, "tier: pre-commit") || !strings.Contains(cfg, "tier: pre-push") {
		t.Fatalf("starter should tier checks:\n%s", cfg)
	}

	hooksDir := filepath.Join(root, ".git", "hooks")
	pc := readFile(t, filepath.Join(hooksDir, "pre-commit"))
	pp := readFile(t, filepath.Join(hooksDir, "pre-push"))
	if !strings.Contains(pc, "run --tier=pre-commit") || !strings.Contains(pp, "run --tier=pre-push") {
		t.Fatalf("hooks call wrong tier:\npre-commit=%s\npre-push=%s", pc, pp)
	}
	if !strings.Contains(pc, managedMarker) {
		t.Fatalf("hook missing managed marker:\n%s", pc)
	}
	// git-native bypass must be documented and NOT defeated.
	if !strings.Contains(pc, "--no-verify") {
		t.Fatalf("hook should document --no-verify escape:\n%s", pc)
	}
	if !strings.Contains(pc, "command -v corpos-gate") {
		t.Fatalf("hook should guard on corpos-gate being on PATH:\n%s", pc)
	}
	// A main checkout must NOT set a shared core.hooksPath.
	if g.called("core.hooksPath") {
		t.Fatalf("main checkout must not set core.hooksPath: %v", g.calls)
	}

	// Idempotent second run: gate.yml kept, hooks rewritten, no error.
	out.Reset()
	if err := initRepo(root, mainCheckoutGit(root).fn, &out); err != nil {
		t.Fatalf("second initRepo: %v", err)
	}
	if !strings.Contains(out.String(), "gate.yml exists, keeping it") {
		t.Fatalf("second run should keep gate.yml:\n%s", out.String())
	}
	if readFile(t, filepath.Join(hooksDir, "pre-commit")) != pc {
		t.Fatalf("second run should reproduce identical managed hook")
	}
}

// TestInitLinkedWorktree proves init on a linked worktree installs
// gate-only hooks into the worktree's private git dir and sets a
// PER-WORKTREE core.hooksPath (never a shared config), mirroring
// scripts/worktree-setup.sh.
func TestInitLinkedWorktree(t *testing.T) {
	mainRoot := t.TempDir()
	wt := t.TempDir() // the linked worktree root (a different dir)
	gitDir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
	g := &recordingGit{answers: map[string]string{
		"rev-parse --show-toplevel":                         wt,
		"rev-parse --path-format=absolute --git-common-dir": filepath.Join(mainRoot, ".git"),
		"rev-parse --absolute-git-dir":                      gitDir,
	}}
	var out bytes.Buffer
	if err := initRepo(wt, g.fn, &out); err != nil {
		t.Fatalf("initRepo worktree: %v", err)
	}
	hooksDir := filepath.Join(gitDir, "gate-only-hooks")
	if _, err := os.Stat(filepath.Join(hooksDir, "pre-commit")); err != nil {
		t.Fatalf("worktree hook not written to private git dir: %v", err)
	}
	if !g.called("extensions.worktreeConfig true") {
		t.Fatalf("worktree init should enable worktreeConfig: %v", g.calls)
	}
	if !g.called("config --worktree core.hooksPath") {
		t.Fatalf("worktree init should set per-worktree core.hooksPath: %v", g.calls)
	}
	if !strings.Contains(out.String(), "linked worktree") {
		t.Fatalf("summary should note the linked-worktree path:\n%s", out.String())
	}
}

// TestStarterConfigDetectsStack proves stack auto-detection: go (nested
// module), ts (package.json), and shell (fallback).
func TestStarterConfigDetectsStack(t *testing.T) {
	// Nested go.mod (like this repo's go/ layout) → unit "go".
	goRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(goRoot, "go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goRoot, "go", "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if stack, body := starterConfig(goRoot); stack != "go" || !strings.Contains(body, "dir: go") {
		t.Fatalf("nested go detection: stack=%s body=%s", stack, body)
	}

	tsRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(tsRoot, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if stack, body := starterConfig(tsRoot); stack != "ts" || !strings.Contains(body, "stack: ts") {
		t.Fatalf("ts detection: stack=%s", stack)
	}

	shRoot := t.TempDir()
	if stack, body := starterConfig(shRoot); stack != "shell" || !strings.Contains(body, "stack: shell") {
		t.Fatalf("shell fallback: stack=%s", stack)
	}
}

// TestInitNotGitRepo proves init fails cleanly when the target is not a
// git repo.
func TestInitNotGitRepo(t *testing.T) {
	badGit := func(string, ...string) (string, error) {
		return "", os.ErrNotExist
	}
	var out bytes.Buffer
	if err := initRepo(t.TempDir(), badGit, &out); err == nil {
		t.Fatalf("expected error for non-git dir")
	}
}

// TestCmdInitCLI drives the CLI dispatch against a REAL git repo so
// gitExec + real worktree detection are exercised end-to-end.
func TestCmdInitCLI(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		if _, err2 := os.Stat("/bin/git"); err2 != nil {
			t.Skip("git not available")
		}
	}
	root := t.TempDir()
	mustGit(t, root, "init")
	mustGit(t, root, "config", "user.email", "t@t")
	mustGit(t, root, "config", "user.name", "t")

	var out, errb bytes.Buffer
	if code := runCLI([]string{"init", root}, &out, &errb); code != 0 {
		t.Fatalf("init CLI exit=%d err=%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "gate.yml")); err != nil {
		t.Fatalf("gate.yml not written: %v", err)
	}
	// The real hooks dir (.git/hooks) should hold the managed pre-commit.
	pc := filepath.Join(root, ".git", "hooks", "pre-commit")
	if body := readFile(t, pc); !strings.Contains(body, "run --tier=pre-commit") {
		t.Fatalf("real hook wrong:\n%s", body)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitExec(dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}
