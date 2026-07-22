package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// Meta-substrate-chain detection — suggestion
// `meta-session-arc-close-downweighting-when-chain-targets-arcreview-itself`.
//
// When a chain DEVELOPS the arc-close-review substrate itself (e.g.
// chain 618), each commit on that chain triggers an arc-close fire
// that reviews the chain's own commits. Qwen produces paraphrase-
// shape filings of the chain's own work; F4 catches some but not
// all; the agent spends session time recognising noise.
//
// Detection: if >=50% of files in the trigger CommitLanded's commit
// are under `go/internal/arcreview/`, the commit is meta-substrate
// and the arc-close fire should be suppressed.
//
// Distinct from a CHAIN-LEVEL flag (proposed in the suggestion as
// path (a)). The chain-flag path requires schema changes; this
// file-path heuristic is operator-transparent and ships standalone.
//
// FOLLOW-ON (deferred): a chain-level meta_substrate_chain frontmatter
// flag would be a more precise signal than the file-path heuristic
// when a chain modifies arcreview as one of several adjacent
// substrates (e.g. a refactor that touches arcreview + work + events
// proportionally). The file-path 50% threshold would miss those
// cases. Add the chain-flag path when this gap surfaces in
// retrospective measurement.

const (
	// metaSubstratePathPrefix is the source-tree prefix that signals
	// the commit modifies the arc-close substrate ITSELF rather than
	// using it as a consumer. Changes here mean the next arc-close
	// fire would review the substrate's own code.
	metaSubstratePathPrefix = "go/internal/arcreview/"

	// metaSubstrateFileRatio is the threshold above which a commit
	// is classified as meta-substrate. Per the suggestion: ">=50%"
	// avoids over-suppressing commits that touch arcreview as a
	// minor side-effect (e.g. a refactor renaming a struct that
	// arcreview imports).
	metaSubstrateFileRatio = 0.50
)

// isMetaSubstrateChainCommit returns true when the trigger event
// corresponds to a CommitLanded whose commit modifies primarily
// substrate-self code (>= metaSubstrateFileRatio of files-changed
// under metaSubstratePathPrefix).
//
// Reads the commit SHA from the trigger event envelope directly
// (entity_kind="commit" + entity_slug=<sha>) — the SubstrateReviewObserver
// fires INSIDE the events-fold transaction, so the events-table
// row isn't yet visible to external readers at the moment of the
// call. Using the envelope avoids the race that the (otherwise
// equivalent) events-table lookup would hit.
//
// Fail-open: any error (entity_kind not commit, git binary missing,
// git show fails) returns false. The arc-close fire proceeds as
// normal rather than blocking on a detection failure.
//
// Returns false for non-CommitLanded triggers (e.g. BugResolved,
// TaskCompleted) — meta-substrate detection only applies to
// commit-driven fires.
func isMetaSubstrateChainCommit(ctx context.Context, evt SubstrateTriggerEvent) bool {
	if evt.EntityKind != "commit" {
		return false
	}
	sha := evt.EntitySlug
	files, err := filesChangedInCommit(ctx, sha)
	if err != nil || len(files) == 0 {
		return false
	}
	return isMetaSubstrateFileSet(files)
}

// isMetaSubstrateChainCommitByEventLookup is the events-table-
// lookup variant used by tests (where the trigger event envelope
// isn't available — only the persisted event_id). Production code
// uses the envelope-based isMetaSubstrateChainCommit above.
func isMetaSubstrateChainCommitByEventLookup(ctx context.Context, pool *db.Pool, eventID string) bool {
	if pool == nil || eventID == "" {
		return false
	}
	sha, ok := commitSHAForEvent(ctx, pool, eventID)
	if !ok {
		return false
	}
	files, err := filesChangedInCommit(ctx, sha)
	if err != nil || len(files) == 0 {
		return false
	}
	return isMetaSubstrateFileSet(files)
}

// isMetaSubstrateFileSet is the pure path-ratio logic, separated
// from the git shell-out so unit tests can exercise it without
// fixtures. Returns true when >= metaSubstrateFileRatio of the
// input files sit under metaSubstratePathPrefix.
func isMetaSubstrateFileSet(files []string) bool {
	if len(files) == 0 {
		return false
	}
	matched := 0
	for _, f := range files {
		if strings.HasPrefix(f, metaSubstratePathPrefix) {
			matched++
		}
	}
	ratio := float64(matched) / float64(len(files))
	return ratio >= metaSubstrateFileRatio
}

// commitSHAForEvent looks up the named event row, decodes its
// payload as CommitLanded, returns commit_sha. Returns (sha, true)
// on success, ("", false) on any failure path. Reads payload via
// the same json column the projection folds use.
func commitSHAForEvent(ctx context.Context, pool *db.Pool, eventID string) (string, bool) {
	var (
		eventType  string
		payloadRaw []byte
	)
	err := pool.DB().QueryRowContext(ctx, `
		SELECT type, payload FROM events WHERE event_id = ? LIMIT 1`,
		eventID).Scan(&eventType, &payloadRaw)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false
		}
		return "", false
	}
	if eventType != "CommitLanded" {
		return "", false
	}
	var p events.CommitLandedPayload
	if err := json.Unmarshal(payloadRaw, &p); err != nil {
		return "", false
	}
	if p.CommitSHA == "" {
		return "", false
	}
	return p.CommitSHA, true
}

// filesChangedInCommit shells to `git show --name-only --format=
// <sha>` to fetch the file list. The observer process's CWD is
// the repo root (set by the launcher); no explicit -C flag needed.
func filesChangedInCommit(ctx context.Context, sha string) ([]string, error) {
	// SHA validation: only hex chars + length sane bounds. Defensive
	// against any shell-injection paranoia even though exec.Command
	// doesn't go through a shell.
	if len(sha) < 7 || len(sha) > 64 {
		return nil, fmt.Errorf("invalid sha length: %d", len(sha))
	}
	for _, r := range sha {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return nil, fmt.Errorf("invalid sha char: %q", r)
		}
	}
	cmd := exec.CommandContext(ctx, "git", "show", "--name-only", "--format=", sha)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	files := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}
