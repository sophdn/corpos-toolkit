package stdiodrift

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MarkerPath is the canonical location of the post-commit-restart-advisor's
// marker file. Both the advisor (scripts/post-commit-restart-advisor.sh
// lines 492 / 525) and this package treat the path as fixed; relocating
// it would break the cross-process contract.
const MarkerPath = "/tmp/toolkit-server-restart-needed"

// DriftKind enumerates the drift shapes a stdio process can exhibit.
// Returned in the State response and emitted on the
// ParseContextStdioDriftSurfaced event payload.
type DriftKind string

const (
	DriftKindNone             DriftKind = "none"
	DriftKindStdioFDPinned    DriftKind = "stdio_fd_pinned"
	DriftKindCompileTimeStale DriftKind = "compile_time_stale"
	DriftKindBoth             DriftKind = "both"
)

// StdioProcess describes one toolkit-server stdio process the advisor
// preserved (or that this package's enumeration found by other means).
type StdioProcess struct {
	PID             int       `json:"pid"`
	ReportedGitSHA  string    `json:"reported_git_sha"`
	OnDiskBinarySHA string    `json:"on_disk_binary_sha"`
	FDDeleted       bool      `json:"fd_deleted"`
	DriftKind       DriftKind `json:"drift_kind"`
}

// State is the typed wire shape /admin/stdio-drift-state returns AND
// the in-process struct refresolve consults to decide whether to
// surface a Candidate. drift_detected is true when ANY listed
// stdio_process has a non-none drift_kind.
type State struct {
	HeadSHA        string         `json:"head_sha"`
	StdioProcesses []StdioProcess `json:"stdio_processes"`
	DriftDetected  bool           `json:"drift_detected"`
	// CheckedAt is filled by the helper that calls Snapshot; not
	// emitted on the wire by default (server-side cache decisions
	// can stamp it before serializing).
	CheckedAt time.Time `json:"-"`
}

// MarkerInfo is the parsed form of /tmp/toolkit-server-restart-needed.
// The advisor's marker is plain text; we extract structured fields
// (preserved PIDs) plus carry the raw body forward for tests / debug.
type MarkerInfo struct {
	Present       bool
	PreservedPIDs []int
	Raw           string
}

// ReadMarker loads the advisor's marker file. When the file doesn't
// exist, returns MarkerInfo{Present: false, …} with no error — the
// no-marker case is the dominant happy path (no preserved stdio = no
// drift) and must not surface as an error.
func ReadMarker(path string) (MarkerInfo, error) {
	if path == "" {
		path = MarkerPath
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MarkerInfo{Present: false}, nil
		}
		return MarkerInfo{}, fmt.Errorf("read marker %s: %w", path, err)
	}
	info := MarkerInfo{Present: true, Raw: string(body)}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "preserved stdio pid:") {
			continue
		}
		pidStr := strings.TrimSpace(strings.TrimPrefix(line, "preserved stdio pid:"))
		if pid, perr := strconv.Atoi(pidStr); perr == nil {
			info.PreservedPIDs = append(info.PreservedPIDs, pid)
		}
	}
	return info, nil
}

// ProcessFDDeleted reports whether /proc/<pid>/exe resolves to a path
// ending in " (deleted)" — Linux's signal that the binary's inode was
// unlinked while the process held it open. Returns (false, nil) when
// the process isn't found (already exited).
//
// procRoot is "/proc" in production; tests pass a tempdir with mocked
// symlinks for the fd-deletion path.
func ProcessFDDeleted(procRoot string, pid int) (bool, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	link := filepath.Join(procRoot, strconv.Itoa(pid), "exe")
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("readlink %s: %w", link, err)
	}
	return strings.HasSuffix(target, " (deleted)"), nil
}

// GitHEAD returns the current `git rev-parse HEAD` for repoRoot.
// Empty repoRoot defaults to the process's working directory. Uses a
// 2-second timeout so a hung git invocation never stalls a parse_context
// call.
func GitHEAD(ctx context.Context, repoRoot string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	args := []string{"rev-parse", "HEAD"}
	cmd := exec.CommandContext(pctx, "git", args...)
	if repoRoot != "" {
		cmd.Dir = repoRoot
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ClassifyKind composes the two drift signals into one DriftKind:
//
//	fd_deleted && shaStale → DriftKindBoth
//	fd_deleted              → DriftKindStdioFDPinned
//	shaStale                → DriftKindCompileTimeStale
//	(neither)               → DriftKindNone
//
// shaStale is true when reported != onDisk AND both are non-empty
// (empty reported means we couldn't observe the running SHA — handled
// as "not stale" rather than guessing).
func ClassifyKind(fdDeleted bool, reported, onDisk string) DriftKind {
	shaStale := reported != "" && onDisk != "" && reported != onDisk
	switch {
	case fdDeleted && shaStale:
		return DriftKindBoth
	case fdDeleted:
		return DriftKindStdioFDPinned
	case shaStale:
		return DriftKindCompileTimeStale
	default:
		return DriftKindNone
	}
}

// SnapshotInputs are the per-call parameters Snapshot needs. Pull
// most of these from AppState / HandlerDeps at the call site.
type SnapshotInputs struct {
	// MarkerPathOverride defaults to MarkerPath when empty.
	MarkerPathOverride string
	// ProcRootOverride defaults to "/proc" when empty (tests override).
	ProcRootOverride string
	// RepoRoot is the working directory passed to `git rev-parse HEAD`.
	// Empty means "the process cwd."
	RepoRoot string
	// OnDiskGitSHA is the daemon/process's own compile-time gitSHA
	// (from ldflags). The endpoint compares each preserved PID's
	// reported SHA against this value.
	OnDiskGitSHA string
	// ReportedSHAByPID is an optional map from PID → that process's
	// gitSHA. Populated when an out-of-band channel (marker
	// augmentation, sibling startup-file, in-process registration)
	// can supply per-PID identity. Empty → reported_git_sha left
	// blank; classification relies on fd_deleted alone.
	ReportedSHAByPID map[int]string
}

// Snapshot produces the typed State the endpoint serializes and the
// parse_context handler evaluates. Pure function modulo filesystem
// reads + the `git rev-parse HEAD` shell-out.
//
// No-marker case: returns State{DriftDetected: false, StdioProcesses: []}
// even if git HEAD is reachable — drift requires a preserved stdio,
// which the marker is the canonical signal for.
func Snapshot(ctx context.Context, in SnapshotInputs) (State, error) {
	out := State{
		StdioProcesses: []StdioProcess{},
		CheckedAt:      time.Now().UTC(),
	}

	markerPath := in.MarkerPathOverride
	if markerPath == "" {
		markerPath = MarkerPath
	}
	marker, err := ReadMarker(markerPath)
	if err != nil {
		return out, fmt.Errorf("read marker: %w", err)
	}

	headSHA, headErr := GitHEAD(ctx, in.RepoRoot)
	if headErr == nil {
		out.HeadSHA = headSHA
	}
	// HEAD lookup failure isn't fatal — the marker may still indicate
	// drift via fd_deleted. Leave HeadSHA empty in that case.

	if !marker.Present {
		return out, nil
	}

	for _, pid := range marker.PreservedPIDs {
		fdDeleted, _ := ProcessFDDeleted(in.ProcRootOverride, pid)
		reported := ""
		if in.ReportedSHAByPID != nil {
			reported = in.ReportedSHAByPID[pid]
		}
		kind := ClassifyKind(fdDeleted, reported, in.OnDiskGitSHA)
		out.StdioProcesses = append(out.StdioProcesses, StdioProcess{
			PID:             pid,
			ReportedGitSHA:  reported,
			OnDiskBinarySHA: in.OnDiskGitSHA,
			FDDeleted:       fdDeleted,
			DriftKind:       kind,
		})
		if kind != DriftKindNone {
			out.DriftDetected = true
		}
	}
	return out, nil
}
