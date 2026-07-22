package registry

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"toolkit/internal/db"
)

// MirrorOptions tunes a [Mirror] push.
type MirrorOptions struct {
	// Remote + Branch name the push target on the registry checkout. Default
	// origin/main.
	Remote string
	Branch string
	// InsecureTLS adds `-c http.sslVerify=false` to every git invocation —
	// needed for the homelab Gitea's self-signed cert. Off for the hermetic
	// test (a local bare remote has no TLS).
	InsecureTLS bool
}

// MirrorResult reports the outcome of a mirror push.
type MirrorResult struct {
	Pushed      bool   `json:"pushed"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	NewEvents   int    `json:"new_events"`   // event files added since the last mirror (the delta)
	TotalEvents int    `json:"total_events"` // events in the local ledger after export
	Message     string `json:"message"`
}

// Mirror is the async-tail step of the forge-v2 record surface (chain
// emit-surface-forge-v2 T5): it serializes the local hot-draft ledger to the
// registry checkout, and if there are new events, commits and pushes them to
// the canonical Gitea registry — which triggers the CI validity-stamp gate.
//
// It is INCREMENTAL for free: [ExportFromDB] is deterministic + idempotent,
// so re-exporting an unchanged ledger produces byte-identical files (a git
// no-op); only genuinely-new events show up as staged adds. A mirror with no
// new events pushes nothing (Pushed=false).
//
// This NEVER runs on the record() critical path — the local validate → append
// → fold → return stays synchronous and fast (§2). Mirror runs on a save
// boundary (a commit / Stop / flush hook invokes `event-registry mirror`),
// the durability + CI tail riding behind the agent's fast local return.
//
// Mirror does not block on CI: it pushes and returns the commit SHA. The CI
// verdict for that SHA is fetched separately by [PollVerdict] (the completion
// ping), so a slow or red CI never stalls the mirror.
func Mirror(ctx context.Context, pool *db.Pool, registryDir string, opts MirrorOptions) (MirrorResult, error) {
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	if opts.Branch == "" {
		opts.Branch = "main"
	}

	total, err := ExportFromDB(ctx, pool, registryDir)
	if err != nil {
		return MirrorResult{}, fmt.Errorf("export for mirror: %w", err)
	}

	if _, err := gitRun(ctx, registryDir, opts, "add", "-A"); err != nil {
		return MirrorResult{}, err
	}

	porcelain, err := gitRun(ctx, registryDir, opts, "status", "--porcelain")
	if err != nil {
		return MirrorResult{}, err
	}
	if strings.TrimSpace(porcelain) == "" {
		return MirrorResult{Pushed: false, TotalEvents: total, Message: "no new events to mirror"}, nil
	}

	// Count the newly-added event files — the delta size (for the message and
	// for the CI strict-list, which validates exactly these).
	added, err := gitRun(ctx, registryDir, opts, "diff", "--cached", "--name-only", "--diff-filter=A", "--", eventsDir)
	if err != nil {
		return MirrorResult{}, err
	}
	newCount := countNonEmptyLines(added)

	msg := fmt.Sprintf("mirror: +%d event(s) (%d total) @ %s", newCount, total, time.Now().UTC().Format(time.RFC3339))
	// Identity supplied inline so the registry checkout needs no pre-config.
	if _, err := gitRun(ctx, registryDir, opts,
		"-c", "user.name=toolkit-mirror", "-c", "user.email=mirror@toolkit.local",
		"commit", "-m", msg); err != nil {
		return MirrorResult{}, err
	}

	if _, err := gitRun(ctx, registryDir, opts, "push", opts.Remote, "HEAD:"+opts.Branch); err != nil {
		return MirrorResult{}, err
	}

	sha, err := gitRun(ctx, registryDir, opts, "rev-parse", "HEAD")
	if err != nil {
		return MirrorResult{}, err
	}
	return MirrorResult{
		Pushed:      true,
		CommitSHA:   sha,
		NewEvents:   newCount,
		TotalEvents: total,
		Message:     fmt.Sprintf("mirrored %d new event(s) → %s/%s @ %s", newCount, opts.Remote, opts.Branch, sha[:min(12, len(sha))]),
	}, nil
}

// gitRun runs a git command in dir, prepending the -C dir and (when the
// homelab self-signed cert is in play) the sslVerify=false config. Returns
// trimmed stdout; wraps the error with stderr for diagnosis.
func gitRun(ctx context.Context, dir string, opts MirrorOptions, args ...string) (string, error) {
	full := []string{"-C", dir}
	if opts.InsecureTLS {
		full = append(full, "-c", "http.sslVerify=false")
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func countNonEmptyLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
