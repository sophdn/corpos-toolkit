package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cmdInit handles `corpos-gate init [dir]`: it wires gate-only git hooks
// (pre-commit → the fast tier, pre-push → the slow tier) and, if absent,
// a starter gate.yml into the repo containing dir (default: the current
// directory). Re-running is idempotent — the managed hooks are rewritten,
// an existing gate.yml is left untouched.
func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "corpos-gate init [dir]\n\n"+
			"Installs gate-only git hooks + a starter gate.yml into the repo that\n"+
			"contains dir (default: the current directory). Idempotent: re-running\n"+
			"rewrites the managed hooks and never clobbers an existing gate.yml.\n\n"+
			"Requires the target to be a git repo (run 'git init' first) and the\n"+
			"corpos-gate binary to be on PATH for the hooks to fire (install it once\n"+
			"with 'make -C go corpos-gate-install').\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := "."
	if fs.NArg() >= 1 {
		dir = fs.Arg(0)
	}
	if err := initRepo(dir, gitExec, stdout); err != nil {
		fmt.Fprintf(stderr, "corpos-gate init: %v\n", err)
		return 1
	}
	return 0
}

// gitFn runs a git command in dir and returns its trimmed stdout. It is
// injected so initRepo's repo/worktree resolution is hermetically
// testable without a real git process.
type gitFn func(dir string, args ...string) (string, error)

// gitExec is the production gitFn: a real `git -C dir …` invocation.
func gitExec(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// initRepo performs the wiring for `init`, printing a per-action summary
// to out. All git access flows through git (injectable). Filesystem
// writes hit the real repo the resolved paths point at.
func initRepo(dir string, git gitFn, out io.Writer) error {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return fmt.Errorf("%s is not inside a git repository (run `git init` first)", dir)
	}

	if err := writeStarterConfig(root, out); err != nil {
		return err
	}
	if err := installHooks(root, git, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "corpos-gate: init complete for %s\n", root)
	return nil
}

// ── starter gate.yml ────────────────────────────────────────────────────

// writeStarterConfig writes a minimal stack-appropriate gate.yml at the
// repo root IFF none exists. An existing gate.yml is never clobbered.
func writeStarterConfig(root string, out io.Writer) error {
	path := filepath.Join(root, "gate.yml")
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(out, "corpos-gate: gate.yml exists, keeping it\n")
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat gate.yml: %w", err)
	}
	stack, body := starterConfig(root)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write starter gate.yml: %w", err)
	}
	fmt.Fprintf(out, "corpos-gate: wrote starter gate.yml (stack=%s)\n", stack)
	return nil
}

// starterConfig auto-detects the repo's stack and returns its name plus a
// minimal gate.yml body. Detection order: a Go module (go.mod at the root
// or one directory down) → go; package.json → ts; otherwise → shell. The
// go starter drives the real go adapter; ts/shell use custom checks since
// those adapters aren't wired yet.
func starterConfig(root string) (stack, body string) {
	if unit, ok := detectGoUnit(root); ok {
		return "go", fmt.Sprintf(`# corpos-gate config — starter written by `+"`corpos-gate init`"+`.
# Tiers are a superset: ci ⊃ pre-push ⊃ pre-commit. pre-commit is the
# FAST gate (keep it well under ~20s); push slow checks to pre-push/ci.
stack: go
units:
  - dir: %s
checks:
  format: { enabled: true,  tier: pre-commit }
  vet:    { enabled: true,  tier: pre-commit }
  build:  { enabled: true,  tier: pre-commit }
  test:   { enabled: true,  tier: pre-push }
`, unit)
	}
	if fileExists(filepath.Join(root, "package.json")) {
		return "ts", `# corpos-gate config — starter written by ` + "`corpos-gate init`" + `.
# Tiers are a superset: ci ⊃ pre-push ⊃ pre-commit. Edit the commands to
# match this project (the ts adapter is not wired yet, so these are
# custom shell checks).
stack: ts
units:
  - dir: .
custom:
  - { name: build, cmd: "npm run build --if-present", tier: pre-commit }
  - { name: test,  cmd: "npm test --if-present",      tier: pre-push }
`
	}
	return "shell", `# corpos-gate config — starter written by ` + "`corpos-gate init`" + `.
# Tiers are a superset: ci ⊃ pre-push ⊃ pre-commit. Edit the commands to
# match this project (shell stack: everything is a custom check).
stack: shell
units:
  - dir: .
custom:
  - { name: shellcheck, cmd: "find . -name '*.sh' -print0 | xargs -0 -r shellcheck", tier: pre-commit }
`
}

// detectGoUnit returns the unit dir for a Go module: "." when go.mod sits
// at the repo root, else the first immediate subdirectory that holds a
// go.mod (matching the go/-nested layout this repo itself uses).
func detectGoUnit(root string) (string, bool) {
	if fileExists(filepath.Join(root, "go.mod")) {
		return ".", true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(root, e.Name(), "go.mod")) {
			return e.Name(), true
		}
	}
	return "", false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ── hooks ───────────────────────────────────────────────────────────────

// managedMarker tags a hook file corpos-gate wrote, so re-running init
// knows the file is its own to overwrite.
const managedMarker = "# corpos-gate-managed hook — safe to overwrite via `corpos-gate init`."

// installHooks writes the pre-commit + pre-push gate hooks, honoring the
// worktree workflow: in a LINKED worktree they go into that worktree's
// private git dir + a per-worktree core.hooksPath (never touching the
// main checkout or a shared config); in a MAIN checkout they go straight
// into the repo's hooks dir (.git/hooks) so no shared core.hooksPath is
// set that a worktree-discipline guard could collide with.
func installHooks(root string, git gitFn, out io.Writer) error {
	hooksDir, worktree, err := hookDir(root, git)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir %s: %w", hooksDir, err)
	}
	for hook, tier := range map[string]string{"pre-commit": "pre-commit", "pre-push": "pre-push"} {
		p := filepath.Join(hooksDir, hook)
		if err := os.WriteFile(p, []byte(hookScript(tier)), 0o755); err != nil {
			return fmt.Errorf("write %s hook: %w", hook, err)
		}
	}
	if worktree {
		// Point THIS worktree's hooks at the gate-only dir without
		// disturbing the main checkout (mirrors scripts/worktree-setup.sh).
		if _, err := git(root, "config", "extensions.worktreeConfig", "true"); err != nil {
			return fmt.Errorf("enable worktreeConfig: %w", err)
		}
		if _, err := git(root, "config", "--worktree", "core.hooksPath", hooksDir); err != nil {
			return fmt.Errorf("set per-worktree core.hooksPath: %w", err)
		}
		fmt.Fprintf(out, "corpos-gate: installed gate hooks for linked worktree at %s (per-worktree core.hooksPath)\n", hooksDir)
	} else {
		fmt.Fprintf(out, "corpos-gate: installed gate hooks at %s\n", hooksDir)
	}
	return nil
}

// hookDir resolves where the gate hooks go and whether root is a linked
// worktree. For a linked worktree it returns "<private-git-dir>/gate-only-hooks"
// (invisible to the main checkout, removed with the worktree). For a main
// checkout it returns the repo's real hooks dir (git rev-parse --git-path
// hooks, typically .git/hooks).
func hookDir(root string, git gitFn) (dir string, worktree bool, err error) {
	common, err := git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", false, fmt.Errorf("resolve git common dir: %w", err)
	}
	mainRoot := ""
	if strings.HasSuffix(common, string(filepath.Separator)+".git") || filepath.Base(common) == ".git" {
		mainRoot = filepath.Dir(common)
	}
	// A linked worktree's root differs from the main checkout that owns
	// the common git dir.
	if mainRoot != "" && mainRoot != root {
		gitDir, gerr := git(root, "rev-parse", "--absolute-git-dir")
		if gerr != nil {
			return "", false, fmt.Errorf("resolve worktree git dir: %w", gerr)
		}
		return filepath.Join(gitDir, "gate-only-hooks"), true, nil
	}
	// Main checkout: install into the real hooks dir.
	hooksPath, herr := git(root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if herr != nil {
		return "", false, fmt.Errorf("resolve hooks dir: %w", herr)
	}
	return hooksPath, false, nil
}

// hookScript is the body of a managed gate hook for the given tier. It
// calls corpos-gate from PATH (with a clear error if absent) and relies
// on git's native --no-verify for emergency bypass — it does nothing that
// would defeat it.
func hookScript(tier string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
%s
# Runs the corpos-gate %s tier. Emergency bypass is git-native:
#   git commit --no-verify     (or: git push --no-verify)
# This hook does nothing to defeat that.
set -euo pipefail
if ! command -v corpos-gate >/dev/null 2>&1; then
    echo "corpos-gate: binary not on PATH — install it once with" >&2
    echo "  make -C go corpos-gate-install   (from the corpos-toolkit repo)" >&2
    echo "then retry, or bypass this hook with --no-verify." >&2
    exit 1
fi
exec corpos-gate run --tier=%s
`, managedMarker, tier, tier)
}
