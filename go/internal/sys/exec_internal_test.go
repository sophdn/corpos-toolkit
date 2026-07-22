package sys

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeSandbox_BwrapAndPodman(t *testing.T) {
	for _, k := range []SandboxKind{SandboxBwrap, SandboxPodman} {
		p, err := ProbeSandbox(k)
		if err != nil {
			t.Fatalf("ProbeSandbox(%s): %v", k, err)
		}
		if p.Name() != k {
			t.Errorf("Name() = %q, want %q", p.Name(), k)
		}
		_ = p.Available() // host-dependent; exercise the probe path
		if argv := p.Wrap("/bin/sh", "echo hi", "/w"); len(argv) == 0 {
			t.Errorf("%s.Wrap returned empty argv", k)
		}
	}
}

func TestProbePodman_ImageOverride(t *testing.T) {
	t.Setenv("TOOLKIT_SANDBOX_PODMAN_IMAGE", "example.com/custom:tag")
	p := probePodman()
	if p.path == "" {
		t.Skip("podman not installed on this host")
	}
	argv := p.Wrap("/bin/sh", "echo hi", "/w")
	found := false
	for _, a := range argv {
		if a == "example.com/custom:tag" {
			found = true
		}
	}
	if !found {
		t.Errorf("Wrap argv missing overridden image: %v", argv)
	}
}

func TestAdoptCapturedCwd(t *testing.T) {
	r, dir := newTestRunner(t)
	// empty path: no-op
	r.adoptCapturedCwd("")
	if r.cwd != dir {
		t.Errorf("cwd changed on empty file: %q", r.cwd)
	}
	// nonexistent file: no-op (read error)
	r.adoptCapturedCwd(filepath.Join(dir, "nope"))
	if r.cwd != dir {
		t.Errorf("cwd changed on missing file: %q", r.cwd)
	}
	// file holding a non-directory path: no-op
	bad := filepath.Join(dir, "cap")
	if err := os.WriteFile(bad, []byte("/this/does/not/exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.adoptCapturedCwd(bad)
	if r.cwd != dir {
		t.Errorf("cwd adopted a non-existent dir: %q", r.cwd)
	}
	// file holding a real dir: adopted
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(sub+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.adoptCapturedCwd(bad)
	if r.cwd != sub {
		t.Errorf("cwd = %q, want adopted %q", r.cwd, sub)
	}
}

func TestDefaultSandbox(t *testing.T) {
	p := DefaultSandbox()
	if p == nil {
		t.Fatal("DefaultSandbox returned nil")
	}
	// Whatever it picks must be available (or the no-op fallback, also available).
	if !p.Available() {
		t.Errorf("DefaultSandbox picked an unavailable provider %q", p.Name())
	}
}

func TestNewRunner_EmptyOriginUsesGetwd(t *testing.T) {
	r, err := NewRunner("")
	if err != nil {
		t.Fatalf("NewRunner(\"\"): %v", err)
	}
	wd, _ := os.Getwd()
	if r.Cwd() != wd {
		t.Errorf("Cwd() = %q, want process wd %q", r.Cwd(), wd)
	}
}

func TestNewRunner_RelativeOriginAbsolutized(t *testing.T) {
	r, err := NewRunner(".")
	if err != nil {
		t.Fatalf("NewRunner(\".\"): %v", err)
	}
	if !filepath.IsAbs(r.Cwd()) {
		t.Errorf("Cwd() = %q, want absolute", r.Cwd())
	}
}

func TestRun_PerCallInvalidCwdRecoversToOrigin(t *testing.T) {
	r, dir := newTestRunner(t)
	res, err := r.Run(context.Background(), "pwd", RunOptions{Cwd: filepath.Join(dir, "does-not-exist")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != dir {
		t.Errorf("pwd = %q, want recovery to origin %q", res.Output, dir)
	}
}

func TestRun_SignalExitReportsMinusOne(t *testing.T) {
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "kill -KILL $$", RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TimedOut {
		t.Error("TimedOut = true, want false (killed, not timed out)")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (terminated by signal)", res.ExitCode)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{max: 4}
	if n, _ := c.Write([]byte("ab")); n != 2 {
		t.Errorf("Write returned %d, want 2", n)
	}
	// room = 2, write 4 → only "cd" retained, but full length reported.
	if n, _ := c.Write([]byte("cdef")); n != 4 {
		t.Errorf("Write returned %d, want 4 (full length reported)", n)
	}
	if c.String() != "abcd" {
		t.Errorf("String() = %q, want %q", c.String(), "abcd")
	}
	// room = 0 → discarded, still reports full length.
	if n, _ := c.Write([]byte("x")); n != 1 {
		t.Errorf("Write returned %d, want 1", n)
	}
	if c.String() != "abcd" {
		t.Errorf("String() = %q, want unchanged %q", c.String(), "abcd")
	}
}

func TestIsExecutableFile(t *testing.T) {
	if isExecutableFile("/nonexistent/path/xyz") {
		t.Error("nonexistent path reported executable")
	}
	if isExecutableFile("/tmp") {
		t.Error("directory reported as executable file")
	}
	if !isExecutableFile("/bin/sh") {
		t.Error("/bin/sh not reported executable")
	}
}

func TestResolveShell_EnvBranches(t *testing.T) {
	// A non-bash/zsh SHELL is ignored → falls through to /bin/bash or /bin/sh.
	t.Setenv("SHELL", "/usr/bin/fish")
	sh := resolveShell()
	if sh != "/bin/bash" && sh != "/bin/sh" {
		t.Errorf("resolveShell with non-bash SHELL = %q, want /bin/bash or /bin/sh", sh)
	}
	// A bash SHELL that exists is honored.
	if isExecutableFile("/bin/bash") {
		t.Setenv("SHELL", "/bin/bash")
		if got := resolveShell(); got != "/bin/bash" {
			t.Errorf("resolveShell with SHELL=/bin/bash = %q, want /bin/bash", got)
		}
	}
}
