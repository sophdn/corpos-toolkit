package sys

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestRunner builds a Runner rooted at a fresh temp dir.
func newTestRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	// Resolve symlinks so pwd comparisons (which see the realpath, e.g. /tmp ->
	// /private/tmp on macOS, or /tmp symlinks on some Linux) line up.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	r, err := NewRunner(real)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r, real
}

func TestRun_SimpleEcho(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "echo hi", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "hi" {
		t.Errorf("Output = %q, want %q", res.Output, "hi")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("TimedOut = true, want false")
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
}

func TestRun_ExitCode(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "exit 7", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestRun_StderrCaptured(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "echo boom 1>&2", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "boom" {
		t.Errorf("Output = %q, want %q (stderr captured)", res.Output, "boom")
	}
}

func TestRun_CombinedInterleaved(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "echo out; echo err 1>&2", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "out\nerr" {
		t.Errorf("Output = %q, want interleaved %q", res.Output, "out\nerr")
	}
}

func TestRun_CwdDefault(t *testing.T) {
	r, dir := newTestRunner(t)
	res, err := r.Run(context.Background(), "pwd", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != dir {
		t.Errorf("pwd = %q, want runner cwd %q", res.Output, dir)
	}
}

func TestRun_CwdPersistsAcrossCalls(t *testing.T) {
	r, dir := newTestRunner(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := r.Run(context.Background(), "cd sub", RunOptions{}); err != nil {
		t.Fatalf("Run cd: %v", err)
	}
	want := filepath.Join(dir, "sub")
	if r.Cwd() != want {
		t.Errorf("runner.Cwd() = %q, want %q (cd persisted)", r.Cwd(), want)
	}
	res, err := r.Run(context.Background(), "pwd", RunOptions{})
	if err != nil {
		t.Fatalf("Run pwd: %v", err)
	}
	if res.Output != want {
		t.Errorf("pwd after cd = %q, want %q", res.Output, want)
	}
}

func TestRun_PerCallCwdOverrideDoesNotPersist(t *testing.T) {
	r, dir := newTestRunner(t)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	res, err := r.Run(context.Background(), "pwd", RunOptions{Cwd: sub})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != sub {
		t.Errorf("pwd with per-call Cwd = %q, want %q", res.Output, sub)
	}
	// The override must not have mutated the persistent cwd.
	if r.Cwd() != dir {
		t.Errorf("runner.Cwd() = %q, want unchanged %q", r.Cwd(), dir)
	}
}

func TestRun_EnvOverride(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "echo $FOO", RunOptions{Env: map[string]string{"FOO": "bar"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "bar" {
		t.Errorf("Output = %q, want %q", res.Output, "bar")
	}
}

func TestRun_EnvInherited(t *testing.T) {
	t.Setenv("SYS_EXEC_TEST_INHERIT", "inherited-value")
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "echo $SYS_EXEC_TEST_INHERIT", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "inherited-value" {
		t.Errorf("Output = %q, want inherited %q", res.Output, "inherited-value")
	}
}

func TestRun_Timeout(t *testing.T) {
	r, _ := newTestRunner(t)
	start := time.Now()
	res, err := r.Run(context.Background(), "sleep 10", RunOptions{TimeoutMS: 250})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if res.ExitCode != TimeoutExitCode {
		t.Errorf("ExitCode = %d, want %d (timeout)", res.ExitCode, TimeoutExitCode)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("elapsed %v, want the timeout to fire well before the sleep finished", elapsed)
	}
}

func TestRun_TimeoutKillsProcessGroup(t *testing.T) {
	r, _ := newTestRunner(t)
	start := time.Now()
	// A backgrounded grandchild must die with the group, not outlive the kill.
	res, err := r.Run(context.Background(), "sleep 30 & sleep 30", RunOptions{TimeoutMS: 300})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("elapsed %v, want process-group kill to return promptly", elapsed)
	}
}

func TestRun_CwdRecovery(t *testing.T) {
	r, dir := newTestRunner(t)
	gone := filepath.Join(dir, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := r.Run(context.Background(), "cd gone", RunOptions{}); err != nil {
		t.Fatalf("Run cd: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// The runner's cwd no longer exists; the next call must recover to origin.
	res, err := r.Run(context.Background(), "pwd", RunOptions{})
	if err != nil {
		t.Fatalf("Run after cwd removed: %v", err)
	}
	if res.Output != dir {
		t.Errorf("pwd after cwd removed = %q, want recovery to origin %q", res.Output, dir)
	}
}

func TestRun_OutputTruncationIntegration(t *testing.T) {
	r, _ := newTestRunner(t)
	// 200 lines "L<n>"; cap to a small char budget so truncation triggers.
	res, err := r.Run(context.Background(), "for i in $(seq 1 200); do echo L$i; done", RunOptions{MaxOutputChars: 40})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if !strings.Contains(res.Output, "lines truncated]") {
		t.Errorf("Output missing truncation marker: %q", res.Output)
	}
	if len(res.Output) > 40+len("\n\n... [999 lines truncated] ...") {
		t.Errorf("Output longer than cap+marker: %d", len(res.Output))
	}
}

func TestRun_DurationRecorded(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "true", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.DurationMS < 0 {
		t.Errorf("DurationMS = %d, want >= 0", res.DurationMS)
	}
}

func TestRun_EmptyCommandIsError(t *testing.T) {
	r, _ := newTestRunner(t)
	if _, err := r.Run(context.Background(), "   ", RunOptions{}); err == nil {
		t.Error("Run(empty) = nil error, want error")
	}
}

// ---- unit nets over the formatting + clamping helpers ----

func TestStripBlankEdges(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hi", "hi"},
		{"\n\n  \nreal\n  \n\n", "real"},
		{"a\n\nb", "a\n\nb"}, // interior blank preserved
		{"   ", ""},
		{"", ""},
		{"\n\n", ""},
		{"  lead\ntrail  ", "  lead\ntrail  "}, // intra-line whitespace preserved
	}
	for _, c := range cases {
		if got := stripBlankEdges(c.in); got != c.want {
			t.Errorf("stripBlankEdges(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "abc"
	if out, tr := truncateOutput(short, 30000); out != short || tr {
		t.Errorf("short: got (%q,%v), want (%q,false)", out, tr, short)
	}
	// 10 lines, cut after 5 chars: tail is the discarded remainder.
	in := "l1\nl2\nl3\nl4\nl5\nl6"
	out, tr := truncateOutput(in, 5)
	if !tr {
		t.Fatal("expected truncated=true")
	}
	if !strings.HasPrefix(out, in[:5]) {
		t.Errorf("truncated output should start with first 5 chars; got %q", out)
	}
	if !strings.Contains(out, "lines truncated]") {
		t.Errorf("missing marker: %q", out)
	}
}

func TestEffectiveTimeoutMS(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, DefaultExecTimeoutMS},
		{-5, DefaultExecTimeoutMS},
		{5000, 5000},
		{MaxExecTimeoutMS + 1, MaxExecTimeoutMS},
		{MaxExecTimeoutMS, MaxExecTimeoutMS},
	}
	for _, c := range cases {
		if got := effectiveTimeoutMS(c.in); got != c.want {
			t.Errorf("effectiveTimeoutMS(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEffectiveMaxOutputChars(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultMaxOutputChars},
		{-1, DefaultMaxOutputChars},
		{500, 500},
		{MaxOutputCharsUpper + 1, MaxOutputCharsUpper},
	}
	for _, c := range cases {
		if got := effectiveMaxOutputChars(c.in); got != c.want {
			t.Errorf("effectiveMaxOutputChars(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestResolveShell(t *testing.T) {
	sh := resolveShell()
	if sh == "" {
		t.Fatal("resolveShell returned empty")
	}
	if _, err := os.Stat(sh); err != nil {
		t.Errorf("resolveShell returned non-existent path %q: %v", sh, err)
	}
}
