package work_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/work"
)

// End-to-end coverage for bug `task-stamp-sha-accepts-foreign-chain-
// commit`: exercises the full guard path through HandleTaskStampSHA —
// real git read (gitCommitMessage) + lookupProjectPath + lookupChainSlug
// + commitChainMismatch — against a real temp git repo. The pure
// decision logic is unit-tested in stamp_validate_test.go (internal
// package); this proves the wiring + git I/O work together.
func TestTaskStampSHA_RejectsForeignChainCommit(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	// The 882 shape: a commit that declares it closes a DIFFERENT chain.
	foreignSHA := gitCommit(t, repo, "chain(arc-close-filing-review): T7 — retrospective + closing audit event")
	// A commit that correctly names this task's chain.
	matchingSHA := gitCommit(t, repo, "chain(train-skill-auto-loader-v1): T7 — retrospective")

	pool := openTaskTestPool(t)
	if _, err := pool.DB().Exec(`UPDATE projects SET path = ? WHERE id = 'mcp-servers'`, repo); err != nil {
		t.Fatalf("set project path: %v", err)
	}
	seedChain(t, pool, "mcp-servers", "train-skill-auto-loader-v1")
	seedTask(t, pool, "train-skill-auto-loader-v1", "t7-retrospective", "active")

	// Foreign-chain commit → REJECTED (pre-fix this silently closed the
	// wrong task).
	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t7-retrospective",
		"commit_sha": foreignSHA,
	}))
	if resp.OK {
		t.Fatalf("expected rejection for foreign-chain commit, got OK: %+v", resp)
	}
	if !strings.Contains(resp.Error, "arc-close-filing-review") || !strings.Contains(resp.Error, "train-skill-auto-loader-v1") {
		t.Errorf("rejection should name both chains; got %q", resp.Error)
	}
	// The task must remain un-stamped / un-closed.
	var status string
	pool.DB().QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = 't7-retrospective'`).Scan(&status)
	if status == "closed" {
		t.Errorf("task was closed despite the rejection — guard did not prevent the write")
	}

	// Matching-chain commit → ACCEPTED.
	resp2, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t7-retrospective",
		"commit_sha": matchingSHA,
	}))
	if !resp2.OK {
		t.Fatalf("expected matching-chain commit to be accepted; got %+v", resp2)
	}
}

// TestTaskStampSHA_AllowsWhenCommitUnreadable proves the best-effort
// degradation: an unknown SHA (not in the repo) can't be read, so the
// guard allows the stamp rather than blocking on an infra gap.
func TestTaskStampSHA_AllowsWhenCommitUnreadable(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	gitCommit(t, repo, "chain(some-chain): T1 — seed")

	pool := openTaskTestPool(t)
	if _, err := pool.DB().Exec(`UPDATE projects SET path = ? WHERE id = 'mcp-servers'`, repo); err != nil {
		t.Fatalf("set project path: %v", err)
	}
	seedChain(t, pool, "mcp-servers", "other-chain")
	seedTask(t, pool, "other-chain", "t1", "active")

	// A syntactically valid SHA that isn't in the repo → unreadable →
	// allow (don't block legit work on a fetch gap).
	resp, _ := work.HandleTaskStampSHA(context.Background(), pool, "mcp-servers", mustJSON(t, map[string]any{
		"slug":       "t1",
		"commit_sha": "deadbeef",
	}))
	if !resp.OK {
		t.Fatalf("expected allow when commit is unreadable; got %+v", resp)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
}

// TestHermeticGitCommit_SurvivesAmbientConfigInjection is the regression pin
// for bug 937 (hermetic-git-spawning-tests-inherit-git-config-parameters-
// poisoned-by-c-injection). The gate-only worktree commit path commits via
// `git -c core.hooksPath=… commit`, which exports GIT_CONFIG_PARAMETERS into
// every descendant git. Before the fix, a hermetic test's `git commit`
// inherited that overridden core.hooksPath, tried to exec a hook absent under
// its /tmp repo, and failed. Here we plant exactly that ambient state — a
// core.hooksPath pointing at a pre-commit hook that always fails — and assert
// the helper-spawned commit still succeeds because gitCmd scrubs the injection.
func TestHermeticGitCommit_SurvivesAmbientConfigInjection(t *testing.T) {
	// A hooks dir whose pre-commit always fails — the poison.
	hooks := t.TempDir()
	hookPath := filepath.Join(hooks, "pre-commit")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho 'poison hook fired' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write poison hook: %v", err)
	}
	// Inject it the way `git -c core.hooksPath=<hooks>` would.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath="+hooks+"'")

	repo := t.TempDir()
	gitInit(t, repo)
	// pre-commit hooks fire even on --allow-empty commits, so this would
	// exec the poison hook and fail if gitCmd didn't scrub the injection.
	gitCommit(t, repo, "chain(some-chain): T1 — seed")
}

// gitCommit makes an empty commit with the given subject and returns its
// short SHA.
func gitCommit(t *testing.T, dir, subject string) string {
	t.Helper()
	runGit(t, dir, "commit", "--allow-empty", "-q", "-m", subject)
	out, err := gitCmd(dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := gitCmd(dir, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitCmd builds a `git -C dir …` command whose environment has the git
// configuration-injection channels stripped, so these hermetic temp-repo
// commits start from a clean git environment regardless of how the test
// binary was invoked. Two ambient channels would otherwise leak in:
//
//   - GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE (bug 921): set when the test
//     suite runs inside the pre-commit hook of a *worktree* checkout.
//   - GIT_CONFIG_PARAMETERS / GIT_CONFIG_COUNT (bug 937): exported by the
//     gate-only worktree commit path's `git -c core.hooksPath=… commit`, the
//     value every descendant git inherits. An inherited core.hooksPath makes
//     this commit exec a hook that doesn't exist under the temp repo → fail.
//
// precommit.sh scrubs the same vars for the `make -C go test` subprocess;
// doing it here too makes the tests self-hermetic under a direct `go test`.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = hermeticGitEnv()
	return cmd
}

func hermeticGitEnv() []string {
	strip := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_COUNT": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		// GIT_CONFIG_COUNT gates the GIT_CONFIG_KEY_<n>/VALUE_<n> array form;
		// stripping count alone neutralizes it, but drop the indexed vars too
		// so nothing lingers for a tool that reads them directly.
		if strip[k] || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
