package sys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Exec runner contract constants (see testdata/parity/EXEC_CONTRACT.md). These
// are model-agnostic: a model-coupled output token cap is intentionally absent —
// the character cap is the only output size guard.
const (
	// DefaultExecTimeoutMS is the wall-clock timeout applied when a call sets
	// none (or a non-positive value).
	DefaultExecTimeoutMS = 120_000 // 2 minutes
	// MaxExecTimeoutMS is the ceiling a requested timeout is clamped to.
	MaxExecTimeoutMS = 600_000 // 10 minutes
	// DefaultMaxOutputChars is the combined-output character budget when a call
	// sets none.
	DefaultMaxOutputChars = 30_000
	// MaxOutputCharsUpper is the ceiling the output budget is clamped to.
	MaxOutputCharsUpper = 150_000
	// TimeoutExitCode is the conventional exit code reported on timeout.
	TimeoutExitCode = 124
	// captureCeiling bounds in-memory capture so a runaway command cannot OOM
	// the server. Output beyond this is dropped before truncation accounting, so
	// the truncated-line count is exact only up to this size (a model-agnostic
	// safety guard, flagged in the contract).
	captureCeiling = 1 << 20 // 1 MiB
)

// RunOptions are the per-call knobs for Runner.Run. The zero value is valid:
// default timeout, default output budget, the runner's persistent working
// directory, inherited environment, and no sandbox.
type RunOptions struct {
	// Cwd overrides the working directory for this call only; it does not mutate
	// the runner's persistent directory unless the command itself cd's.
	Cwd string
	// Env entries are applied on top of the inherited process environment.
	Env map[string]string
	// TimeoutMS bounds wall-clock time (default DefaultExecTimeoutMS, clamped to
	// MaxExecTimeoutMS).
	TimeoutMS int64
	// MaxOutputChars bounds the combined output (default DefaultMaxOutputChars,
	// clamped to MaxOutputCharsUpper).
	MaxOutputChars int
	// Sandbox selects an OS isolation backend (default none).
	Sandbox SandboxKind
}

// RunResult is the outcome of a command. Output is the combined, blank-edge
// trimmed, truncation-capped stdout+stderr.
type RunResult struct {
	Output     string `json:"output"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	Cwd        string `json:"cwd"`
}

// Runner executes shell commands. Its only persistent state is the working
// directory, which a cd inside an (unsandboxed) command carries forward to the
// next call. A Runner is safe for concurrent use; calls serialize on the working
// directory.
type Runner struct {
	mu     sync.Mutex
	cwd    string
	origin string
	shell  string

	// sandboxOverride, when set, replaces the probed provider for a non-none
	// Sandbox selection. Test seam only.
	sandboxOverride SandboxProvider
}

// NewRunner returns a runner rooted at origin (the process working directory
// when origin is empty). origin is resolved to an absolute path.
func NewRunner(origin string) (*Runner, error) {
	if strings.TrimSpace(origin) == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("sys: resolve working directory: %w", err)
		}
		origin = wd
	}
	abs, err := filepath.Abs(origin)
	if err != nil {
		return nil, fmt.Errorf("sys: absolutize %q: %w", origin, err)
	}
	return &Runner{cwd: abs, origin: abs, shell: resolveShell()}, nil
}

// Cwd reports the runner's current persistent working directory.
func (r *Runner) Cwd() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cwd
}

// Run executes command and returns its combined output, exit code, and timing.
func (r *Runner) Run(ctx context.Context, command string, opts RunOptions) (RunResult, error) {
	if strings.TrimSpace(command) == "" {
		return RunResult{}, errors.New("sys.exec: empty command")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	sb, err := r.resolveSandbox(opts.Sandbox)
	if err != nil {
		return RunResult{}, err
	}
	if !sb.Available() {
		return RunResult{}, fmt.Errorf("sys.exec: sandbox backend %q is not available on this host", sb.Name())
	}
	sandboxed := sb.Name() != SandboxNone

	usedOverride := strings.TrimSpace(opts.Cwd) != ""
	cwd := r.cwd
	if usedOverride {
		cwd = opts.Cwd
	}
	cwd = r.recoverCwd(cwd)

	timeout := time.Duration(effectiveTimeoutMS(opts.TimeoutMS)) * time.Millisecond
	maxChars := effectiveMaxOutputChars(opts.MaxOutputChars)

	// Unsandboxed runs in the persistent directory append a pwd capture so a cd
	// inside the command carries forward. A per-call Cwd override is a one-shot
	// detour that never mutates the persistent directory, and a sandboxed run is
	// isolated/per-invocation — neither captures.
	persists := !sandboxed && !usedOverride
	var cwdFile string
	toRun := command
	if persists {
		f, ferr := os.CreateTemp("", "toolkit-sys-cwd-*")
		if ferr == nil {
			cwdFile = f.Name()
			_ = f.Close()
			defer func() { _ = os.Remove(cwdFile) }()
			toRun = command + "\n__toolkit_ec=$?\npwd -P > " + shellSingleQuote(cwdFile) + " 2>/dev/null\nexit $__toolkit_ec"
		}
	}

	argv := sb.Wrap(r.shell, toRun, cwd)

	cap := &cappedBuffer{max: captureCeiling}
	// The command is arbitrary by design; the sys.exec action gates it upstream
	// with an allowlist + required rationale + dispatch policy.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = buildEnv(cwd, opts.Env)
	cmd.Stdout = cap
	cmd.Stderr = cap
	// Own process group so a timeout kills the command's whole subtree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("sys.exec: start: %w", err)
	}
	pgid := cmd.Process.Pid

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	var timedOut bool
	select {
	case <-runCtx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done // reap
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
	case waitErr = <-done:
	}
	durationMS := time.Since(start).Milliseconds()

	output, truncated := truncateOutput(stripBlankEdges(cap.String()), maxChars)
	exitCode := exitCodeOf(timedOut, waitErr)

	if persists && !timedOut {
		r.adoptCapturedCwd(cwdFile)
	}

	return RunResult{
		Output:     output,
		ExitCode:   exitCode,
		TimedOut:   timedOut,
		Truncated:  truncated,
		DurationMS: durationMS,
		Cwd:        r.cwd,
	}, nil
}

// resolveSandbox picks the provider for a Sandbox selection, honoring the test
// override for non-none selections.
func (r *Runner) resolveSandbox(kind SandboxKind) (SandboxProvider, error) {
	if kind == "" || kind == SandboxNone {
		return noneProvider{}, nil
	}
	if r.sandboxOverride != nil {
		return r.sandboxOverride, nil
	}
	return ProbeSandbox(kind)
}

// recoverCwd returns cwd if it exists and is a directory; otherwise it falls
// back to the origin, resetting the persistent cwd when that is what vanished.
func (r *Runner) recoverCwd(cwd string) string {
	if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
		return cwd
	}
	if cwd == r.cwd {
		r.cwd = r.origin
	}
	return r.origin
}

// adoptCapturedCwd reads the pwd written by the wrapped command and adopts it as
// the persistent working directory when it names an existing directory.
func (r *Runner) adoptCapturedCwd(cwdFile string) {
	if cwdFile == "" {
		return
	}
	raw, err := os.ReadFile(cwdFile)
	if err != nil {
		return
	}
	dir := strings.TrimSpace(string(raw))
	if dir == "" {
		return
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		r.cwd = dir
	}
}

// exitCodeOf maps the wait outcome to the contract's exit code.
func exitCodeOf(timedOut bool, waitErr error) int {
	if timedOut {
		return TimeoutExitCode
	}
	if waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode() // -1 when terminated by a signal
	}
	return -1
}

// effectiveTimeoutMS applies the default and ceiling to a requested timeout.
func effectiveTimeoutMS(ms int64) int64 {
	if ms <= 0 {
		return DefaultExecTimeoutMS
	}
	if ms > MaxExecTimeoutMS {
		return MaxExecTimeoutMS
	}
	return ms
}

// effectiveMaxOutputChars applies the default and ceiling to a requested budget.
func effectiveMaxOutputChars(n int) int {
	if n <= 0 {
		return DefaultMaxOutputChars
	}
	if n > MaxOutputCharsUpper {
		return MaxOutputCharsUpper
	}
	return n
}

// stripBlankEdges removes leading and trailing lines that are entirely
// whitespace; interior blank lines and intra-line whitespace are preserved.
func stripBlankEdges(s string) string {
	lines := strings.Split(s, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := len(lines) - 1
	for end >= start && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if start > end {
		return ""
	}
	return strings.Join(lines[start:end+1], "\n")
}

// truncateOutput caps content at max characters, appending a marker that counts
// the discarded trailing lines. It returns the (possibly capped) text and
// whether truncation occurred.
func truncateOutput(content string, max int) (string, bool) {
	if len(content) <= max {
		return content, false
	}
	head := content[:max]
	remaining := strings.Count(content[max:], "\n") + 1
	return fmt.Sprintf("%s\n\n... [%d lines truncated] ...", head, remaining), true
}

// resolveShell picks the command shell: $SHELL when it names an executable
// bash/zsh, else /bin/bash, else /bin/sh.
func resolveShell() string {
	if sh := os.Getenv("SHELL"); sh != "" && (strings.Contains(sh, "bash") || strings.Contains(sh, "zsh")) && isExecutableFile(sh) {
		return sh
	}
	if isExecutableFile("/bin/bash") {
		return "/bin/bash"
	}
	return "/bin/sh"
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// buildEnv returns the child environment: the inherited process environment
// with PWD pinned to cwd and the caller's overrides applied on top.
func buildEnv(cwd string, extra map[string]string) []string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	m["PWD"] = cwd
	for k, v := range extra {
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// shellSingleQuote wraps s in single quotes, escaping embedded single quotes,
// so it is safe to interpolate into a /bin/sh command.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cappedBuffer is an io.Writer that retains at most max bytes and silently
// discards the rest (reporting full writes so the producer is never blocked or
// killed by a short write).
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if room >= len(p) {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
