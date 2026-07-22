package sys

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SandboxKind names an OS-level isolation backend for the exec runner.
type SandboxKind string

const (
	// SandboxNone runs the command as a plain child process (the default,
	// parity floor — no isolation).
	SandboxNone SandboxKind = "none"
	// SandboxBwrap wraps the command in bubblewrap: read-only host root, a
	// writable working dir, private /proc + /dev + tmpfs /tmp, unshared
	// PID/IPC/UTS namespaces. Lightweight; shares the host. Availability is
	// host-dependent (unprivileged user namespaces may be AppArmor-restricted).
	SandboxBwrap SandboxKind = "bwrap"
	// SandboxPodman wraps the command in a rootless container with a read-only
	// rootfs and the working dir bind-mounted writable. Heavier; full isolation.
	SandboxPodman SandboxKind = "podman"
)

// defaultPodmanImage is the base image podman sandboxing runs commands in. It is
// intentionally minimal; the working tree is bind-mounted over it.
const defaultPodmanImage = "docker.io/library/busybox:latest"

// SandboxProvider builds the argv that runs a command under a backend's
// isolation, and reports whether the backend can actually run on this host.
type SandboxProvider interface {
	// Name is the backend kind.
	Name() SandboxKind
	// Available reports whether the backend is runnable here (binary present and,
	// where relevant, the host permits it). A provider that is not Available must
	// never be used to run a command — the runner fails closed instead.
	Available() bool
	// Wrap returns the argv to execute (argv[0] is the binary) that runs
	// `command` via `shell` with `cwd` as the working directory, under this
	// backend's isolation.
	Wrap(shell, command, cwd string) []string
}

// ProbeSandbox returns the provider for a kind, probing host availability.
// SandboxNone (and the empty string) always returns an available no-op provider.
func ProbeSandbox(kind SandboxKind) (SandboxProvider, error) {
	switch kind {
	case "", SandboxNone:
		return noneProvider{}, nil
	case SandboxBwrap:
		return probeBwrap(), nil
	case SandboxPodman:
		return probePodman(), nil
	default:
		return nil, fmt.Errorf("sys.exec: unknown sandbox backend %q (want none|bwrap|podman)", kind)
	}
}

// DefaultSandbox returns the first available isolation backend (bwrap preferred
// for weight, then podman), or the no-op provider if neither is runnable.
func DefaultSandbox() SandboxProvider {
	if p := probeBwrap(); p.Available() {
		return p
	}
	if p := probePodman(); p.Available() {
		return p
	}
	return noneProvider{}
}

// noneProvider runs the command with no isolation.
type noneProvider struct{}

func (noneProvider) Name() SandboxKind { return SandboxNone }
func (noneProvider) Available() bool   { return true }
func (noneProvider) Wrap(shell, command, _ string) []string {
	return []string{shell, "-c", command}
}

// bwrapProvider isolates via bubblewrap.
type bwrapProvider struct {
	path      string // resolved bwrap binary, "" when absent
	available bool
}

func probeBwrap() bwrapProvider {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return bwrapProvider{}
	}
	// Binary present; confirm it can actually create a user namespace here.
	// On AppArmor-restricted hosts (Ubuntu 23.10+) this fails until a profile
	// granting bwrap `userns` is loaded — report unavailable rather than letting
	// a command silently run unsandboxed.
	probe := exec.Command(path, "--ro-bind", "/", "/", "--unshare-user", "true")
	available := probe.Run() == nil
	return bwrapProvider{path: path, available: available}
}

func (p bwrapProvider) Name() SandboxKind { return SandboxBwrap }
func (p bwrapProvider) Available() bool   { return p.available && p.path != "" }

func (p bwrapProvider) Wrap(shell, command, cwd string) []string {
	return []string{
		p.path,
		"--ro-bind", "/", "/", // read-only view of the host
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp", // private writable scratch
		"--bind", cwd, cwd, // working dir is writable
		"--chdir", cwd,
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--die-with-parent",
		shell, "-c", command,
	}
}

// podmanProvider isolates via a rootless container.
type podmanProvider struct {
	path      string
	image     string
	available bool
}

func probePodman() podmanProvider {
	path, err := exec.LookPath("podman")
	if err != nil {
		return podmanProvider{}
	}
	image := defaultPodmanImage
	if v := strings.TrimSpace(os.Getenv("TOOLKIT_SANDBOX_PODMAN_IMAGE")); v != "" {
		image = v
	}
	return podmanProvider{path: path, image: image, available: true}
}

func (p podmanProvider) Name() SandboxKind { return SandboxPodman }
func (p podmanProvider) Available() bool   { return p.available && p.path != "" }

func (p podmanProvider) Wrap(shell, command, cwd string) []string {
	// The container shell is the image's /bin/sh; the host shell path is not
	// meaningful inside the container, so command runs via `sh -c`.
	_ = shell
	return []string{
		p.path, "run", "--rm",
		"--read-only",
		"--network", "none",
		"-v", cwd + ":" + cwd,
		"--workdir", cwd,
		p.image,
		"/bin/sh", "-c", command,
	}
}
