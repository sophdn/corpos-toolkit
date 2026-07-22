package sys

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestProbeSandbox_None(t *testing.T) {
	p, err := ProbeSandbox(SandboxNone)
	if err != nil {
		t.Fatalf("ProbeSandbox(none): %v", err)
	}
	if p.Name() != SandboxNone {
		t.Errorf("Name = %q, want none", p.Name())
	}
	if !p.Available() {
		t.Error("none sandbox should always be available")
	}
	argv := p.Wrap("/bin/bash", "echo hi", "/work")
	want := []string{"/bin/bash", "-c", "echo hi"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("Wrap(none) = %v, want %v", argv, want)
	}
}

func TestSandbox_BwrapWrapShape(t *testing.T) {
	p := bwrapProvider{path: "/usr/bin/bwrap"}
	argv := p.Wrap("/bin/bash", "echo hi", "/work")
	if argv[0] != "/usr/bin/bwrap" {
		t.Errorf("argv[0] = %q, want bwrap path", argv[0])
	}
	joined := strings.Join(argv, " ")
	// Read-only host root, writable working dir, private tmp, namespace unshares.
	for _, frag := range []string{"--ro-bind / /", "--bind /work /work", "--tmpfs /tmp", "--unshare-pid", "--chdir /work", "/bin/bash -c echo hi"} {
		if !strings.Contains(joined, frag) {
			t.Errorf("bwrap argv missing %q; got: %s", frag, joined)
		}
	}
}

func TestSandbox_PodmanWrapShape(t *testing.T) {
	p := podmanProvider{path: "/usr/bin/podman", image: "docker.io/library/busybox:latest"}
	argv := p.Wrap("/bin/sh", "echo hi", "/work")
	if argv[0] != "/usr/bin/podman" {
		t.Errorf("argv[0] = %q, want podman path", argv[0])
	}
	joined := strings.Join(argv, " ")
	for _, frag := range []string{"run", "--rm", "--read-only", "-v /work:/work", "--workdir /work", "busybox", "echo hi"} {
		if !strings.Contains(joined, frag) {
			t.Errorf("podman argv missing %q; got: %s", frag, joined)
		}
	}
}

func TestProbeSandbox_UnknownKind(t *testing.T) {
	if _, err := ProbeSandbox(SandboxKind("nope")); err == nil {
		t.Error("ProbeSandbox(unknown) = nil error, want error")
	}
}

func TestRun_SelectUnavailableSandboxFailsClosed(t *testing.T) {
	r, _ := newTestRunner(t)
	// Force a backend that is definitely unavailable by pointing at a bogus
	// binary; the call must fail closed (error), never run unsandboxed.
	r.sandboxOverride = unavailableProvider{}
	_, err := r.Run(context.Background(), "echo hi", RunOptions{Sandbox: "stub-unavailable"})
	if err == nil {
		t.Fatal("Run with unavailable sandbox = nil error, want fail-closed error")
	}
}

// unavailableProvider is a test stand-in for a probed-but-unavailable backend.
type unavailableProvider struct{}

func (unavailableProvider) Name() SandboxKind                 { return "stub-unavailable" }
func (unavailableProvider) Available() bool                   { return false }
func (unavailableProvider) Wrap(sh, cmd, cwd string) []string { return []string{sh, "-c", cmd} }

// ---- live sandbox tests: opt-in (TOOLKIT_SANDBOX_LIVE_TEST=1) and skipped
// when the backend isn't actually runnable on this host. The gate run stays
// hermetic; run them manually to verify real isolation. ----

func liveSandboxEnabled() bool { return os.Getenv("TOOLKIT_SANDBOX_LIVE_TEST") == "1" }

func TestSandbox_PodmanLive(t *testing.T) {
	if !liveSandboxEnabled() {
		t.Skip("set TOOLKIT_SANDBOX_LIVE_TEST=1 to run live podman sandbox test")
	}
	p, err := ProbeSandbox(SandboxPodman)
	if err != nil || !p.Available() {
		t.Skipf("podman backend not available: %v", err)
	}
	r, _ := newTestRunner(t)
	// Root filesystem must be read-only inside the sandbox.
	res, err := r.Run(context.Background(), "touch /usr/zzz 2>/dev/null && echo RO_FAIL || echo RO_OK", RunOptions{Sandbox: SandboxPodman})
	if err != nil {
		t.Fatalf("Run sandboxed: %v", err)
	}
	if !strings.Contains(res.Output, "RO_OK") {
		t.Errorf("expected read-only root enforcement, got %q", res.Output)
	}
}

func TestSandbox_BwrapLive(t *testing.T) {
	if !liveSandboxEnabled() {
		t.Skip("set TOOLKIT_SANDBOX_LIVE_TEST=1 to run live bwrap sandbox test")
	}
	p, err := ProbeSandbox(SandboxBwrap)
	if err != nil || !p.Available() {
		t.Skipf("bwrap backend not available (load the bwrap-userns-restrict AppArmor profile): %v", err)
	}
	r, _ := newTestRunner(t)
	res, err := r.Run(context.Background(), "touch /usr/zzz 2>/dev/null && echo RO_FAIL || echo RO_OK", RunOptions{Sandbox: SandboxBwrap})
	if err != nil {
		t.Fatalf("Run sandboxed: %v", err)
	}
	if !strings.Contains(res.Output, "RO_OK") {
		t.Errorf("expected read-only root enforcement, got %q", res.Output)
	}
}
