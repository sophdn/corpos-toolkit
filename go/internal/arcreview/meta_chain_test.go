package arcreview

import (
	"context"
	"database/sql"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// Path-ratio unit tests — exercise isMetaSubstrateFileSet directly
// without the git shell-out. The positive/negative ratio boundary
// is the load-bearing logic; the git fetch is a thin shim covered
// at the integration layer by live arc-close fires.

func TestIsMetaSubstrateFileSet_AllInArcreviewMatches(t *testing.T) {
	files := []string{
		"go/internal/arcreview/validation.go",
		"go/internal/arcreview/validation_test.go",
		"go/internal/arcreview/compose.go",
	}
	if !isMetaSubstrateFileSet(files) {
		t.Errorf("all-arcreview file set should match; got false")
	}
}

func TestIsMetaSubstrateFileSet_AboveThresholdMatches(t *testing.T) {
	// 2 of 3 files arcreview (67%) — above 50% threshold.
	files := []string{
		"go/internal/arcreview/validation.go",
		"go/internal/arcreview/validation_test.go",
		"measure/arc-close-corpus-f4-replay.py",
	}
	if !isMetaSubstrateFileSet(files) {
		t.Errorf("2-of-3 arcreview should match (67%% > 50%%); got false")
	}
}

func TestIsMetaSubstrateFileSet_ExactlyHalfMatches(t *testing.T) {
	// 2 of 4 files arcreview (50%) — meets the >= 0.50 threshold.
	files := []string{
		"go/internal/arcreview/validation.go",
		"go/internal/arcreview/compose.go",
		"docs/ARC_CLOSE_FILING_REVIEW.md",
		"scripts/precommit.sh",
	}
	if !isMetaSubstrateFileSet(files) {
		t.Errorf("2-of-4 arcreview should match at 50%% threshold; got false")
	}
}

func TestIsMetaSubstrateFileSet_BelowThresholdDoesNotMatch(t *testing.T) {
	// 1 of 4 files arcreview (25%) — below threshold; commit
	// touches arcreview as a minor side-effect.
	files := []string{
		"go/internal/arcreview/imports.go",
		"go/internal/db/migrations/099_test.sql",
		"docs/CONVENTIONS.md",
		"scripts/precommit.sh",
	}
	if isMetaSubstrateFileSet(files) {
		t.Errorf("1-of-4 arcreview (25%%) should NOT match; got true")
	}
}

func TestIsMetaSubstrateFileSet_EmptyDoesNotMatch(t *testing.T) {
	if isMetaSubstrateFileSet(nil) {
		t.Errorf("nil file list should not match")
	}
	if isMetaSubstrateFileSet([]string{}) {
		t.Errorf("empty file list should not match")
	}
}

func TestIsMetaSubstrateFileSet_NonArcreviewFilesDoNotMatch(t *testing.T) {
	files := []string{
		"go/internal/work/task.go",
		"go/internal/events/payloads.go",
		"crates/shared-db/migrations/050_test.sql",
	}
	if isMetaSubstrateFileSet(files) {
		t.Errorf("zero arcreview files should not match; got true")
	}
}

// --- Public-function tests covering the early-return paths ----

// seedCommitLandedForMeta uses events.Emit so the events row lands
// through the same validator path production uses. Returns the
// event_id Emit assigned.
func seedCommitLandedForMeta(t *testing.T, pool *db.Pool, sha, projectID string) string {
	t.Helper()
	_, _ = pool.DB().Exec(`INSERT INTO projects (id, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, projectID, projectID)
	branch := "main"
	subject := "test commit"
	var eventID string
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		entity := events.NewEntityRef("commit", sha, projectID)
		payload := events.CommitLandedPayload{
			CommitSHA: sha,
			Branch:    &branch,
			Subject:   &subject,
		}
		id, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  entity,
			Payload: payload,
		})
		if err != nil {
			return err
		}
		eventID = id
		return nil
	})
	if err != nil {
		t.Fatalf("seedCommitLandedForMeta: %v", err)
	}
	return eventID
}

func seedBugReportedForMeta(t *testing.T, pool *db.Pool, projectID string) string {
	t.Helper()
	_, _ = pool.DB().Exec(`INSERT INTO projects (id, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, projectID, projectID)
	source := "test"
	tags := ""
	var eventID string
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		entity := events.NewEntityRef("bug", "test-meta-bug", projectID)
		payload := events.BugReportedPayload{
			Title:            "test",
			ProblemStatement: "test",
			Source:           &source,
			Tags:             &tags,
		}
		id, err := events.Emit(context.Background(), tx, events.EmitArgs{
			Entity:  entity,
			Payload: payload,
		})
		if err != nil {
			return err
		}
		eventID = id
		return nil
	})
	if err != nil {
		t.Fatalf("seedBugReportedForMeta: %v", err)
	}
	return eventID
}

func TestIsMetaSubstrateChainCommit_NonCommitEventReturnsFalse(t *testing.T) {
	pool := openTestPool(t)
	eventID := seedBugReportedForMeta(t, pool, "mcp-servers")
	if isMetaSubstrateChainCommitByEventLookup(context.Background(), pool, eventID) {
		t.Errorf("non-CommitLanded event should return false")
	}
}

func TestIsMetaSubstrateChainCommit_MissingEventReturnsFalse(t *testing.T) {
	pool := openTestPool(t)
	if isMetaSubstrateChainCommitByEventLookup(context.Background(), pool, "evt-does-not-exist") {
		t.Errorf("missing event id should return false")
	}
}

func TestIsMetaSubstrateChainCommit_InvalidSHAReturnsFalse(t *testing.T) {
	pool := openTestPool(t)
	// Use a 7-char hex SHA that won't resolve in any git repo
	// — defensive against a real 'git show' actually finding it.
	// The SHA validation passes (hex + length >= 7), but git show
	// will return non-zero, exercising the filesChangedInCommit
	// fail-open path.
	eventID := seedCommitLandedForMeta(t, pool, "deadbeef", "mcp-servers")
	if isMetaSubstrateChainCommitByEventLookup(context.Background(), pool, eventID) {
		t.Errorf("nonexistent SHA should return false (git show fails); got true")
	}
}

func TestIsMetaSubstrateChainCommit_EmptyEventIDReturnsFalse(t *testing.T) {
	pool := openTestPool(t)
	if isMetaSubstrateChainCommitByEventLookup(context.Background(), pool, "") {
		t.Errorf("empty event_id should return false")
	}
}

func TestIsMetaSubstrateChainCommitByEventLookup_NilPoolReturnsFalse(t *testing.T) {
	if isMetaSubstrateChainCommitByEventLookup(context.Background(), nil, "evt-whatever") {
		t.Errorf("nil pool should return false")
	}
}

// Envelope-based tests for the production path. The
// SubstrateReviewObserver passes the SubstrateTriggerEvent directly
// (not the event_id), so the events-fold-tx race that prevented
// the original DB-lookup variant from suppressing live is avoided.

func TestIsMetaSubstrateChainCommit_NonCommitEntityReturnsFalse(t *testing.T) {
	evt := SubstrateTriggerEvent{
		EventID:    "evt-bug-001",
		EventType:  "BugReported",
		EntityKind: "bug",
		EntitySlug: "some-bug-slug",
	}
	if isMetaSubstrateChainCommit(context.Background(), evt) {
		t.Errorf("non-commit entity should return false")
	}
}

func TestIsMetaSubstrateChainCommit_CommitEntityWithInvalidSHAReturnsFalse(t *testing.T) {
	evt := SubstrateTriggerEvent{
		EventID:    "evt-bad-sha",
		EventType:  "CommitLanded",
		EntityKind: "commit",
		EntitySlug: "zzz", // invalid SHA (non-hex) → git show errors → false
	}
	if isMetaSubstrateChainCommit(context.Background(), evt) {
		t.Errorf("invalid SHA should return false")
	}
}

func TestIsMetaSubstrateChainCommit_CommitEntityWithUnknownSHAReturnsFalse(t *testing.T) {
	evt := SubstrateTriggerEvent{
		EventID:    "evt-unknown-sha",
		EventType:  "CommitLanded",
		EntityKind: "commit",
		// Hex but not a real commit — git show fails → false.
		EntitySlug: "deadbeef0000000000000000000000000000beef",
	}
	if isMetaSubstrateChainCommit(context.Background(), evt) {
		t.Errorf("unknown SHA should return false (git show fails)")
	}
}
