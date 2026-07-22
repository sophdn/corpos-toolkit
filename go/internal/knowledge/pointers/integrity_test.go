package pointers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

// seedVaultPointer inserts a source_type='vault' pointer with the given
// slug + source_ref. Used by the integrity-sweep tests below; the
// existing seedPointer helper omits the slug column which the sweep
// surfaces in its OrphanRow output.
func seedVaultPointer(t *testing.T, pool *db.Pool, projectID, slug, sourceRef string) int64 {
	t.Helper()
	p := KnowledgePointer{
		ProjectID:  projectID,
		SourceType: "vault",
		SourceRef:  sourceRef,
		Slug:       slug,
		Question:   "Q for " + slug,
		InvokeWhen: "when " + slug,
		Tags:       []string{},
	}
	res, err := Upsert(context.Background(), pool, p)
	if err != nil {
		t.Fatalf("upsert vault pointer %s: %v", slug, err)
	}
	return res.ID
}

func writeVaultFile(t *testing.T, vaultRoot, relative, body string) {
	t.Helper()
	abs := filepath.Join(vaultRoot, relative)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", relative, err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
}

func TestListVaultOrphans_PartitionsByFileExistence(t *testing.T) {
	pool := testutil.NewTestDB(t)
	vaultRoot := t.TempDir()

	// Two vault pointers: one with a file present, one orphaned.
	presentID := seedVaultPointer(t, pool, "vault", "present-decision", "decisions/2026-05-20_present.md")
	orphanID := seedVaultPointer(t, pool, "vault", "missing-decision", "decisions/2026-05-20_missing.md")
	writeVaultFile(t, vaultRoot, "decisions/2026-05-20_present.md", "ok")

	// Non-vault pointer must be ignored — chain/task/bug identity is
	// not file-backed and the sweep walks only source_type='vault'.
	if _, err := Upsert(context.Background(), pool, KnowledgePointer{
		ProjectID:  "mcp-servers",
		SourceType: "chain",
		SourceRef:  "some-chain-slug",
		Question:   "chain Q",
		InvokeWhen: "chain when",
		Tags:       []string{},
	}); err != nil {
		t.Fatalf("seed chain pointer: %v", err)
	}

	// Inactive vault pointer must be ignored — orphans-of-orphans is
	// not the sweep's job.
	inactiveID := seedVaultPointer(t, pool, "vault", "already-retired", "decisions/old.md")
	if err := RetireOrphan(context.Background(), pool, inactiveID); err != nil {
		t.Fatalf("pre-retire fixture: %v", err)
	}

	report, err := ListVaultOrphans(context.Background(), pool, vaultRoot)
	if err != nil {
		t.Fatalf("ListVaultOrphans: %v", err)
	}
	if report.TotalPointers != 2 {
		t.Errorf("total_pointers = %d; want 2 (present + orphan, chain + inactive excluded)", report.TotalPointers)
	}
	if report.FilesPresent != 1 {
		t.Errorf("files_present = %d; want 1", report.FilesPresent)
	}
	if report.OrphansFound != 1 {
		t.Errorf("orphans_found = %d; want 1", report.OrphansFound)
	}
	if len(report.Orphans) != 1 {
		t.Fatalf("orphans slice len = %d; want 1", len(report.Orphans))
	}
	got := report.Orphans[0]
	if got.ID != orphanID {
		t.Errorf("orphan id = %d; want %d", got.ID, orphanID)
	}
	if got.Slug != "missing-decision" {
		t.Errorf("orphan slug = %q; want missing-decision", got.Slug)
	}
	if got.SourceRef != "decisions/2026-05-20_missing.md" {
		t.Errorf("orphan source_ref = %q; want decisions/2026-05-20_missing.md", got.SourceRef)
	}
	// present pointer must not have moved.
	if got.ID == presentID {
		t.Errorf("present pointer id %d leaked into orphans slice", presentID)
	}
}

func TestListVaultOrphans_EmptyVaultRootIsError(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := ListVaultOrphans(context.Background(), pool, "")
	if err == nil {
		t.Fatal("expected error when vault root is empty; got nil")
	}
}

func TestRetireOrphan_TransitionsStatusAndRemovesFTSEntry(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedVaultPointer(t, pool, "vault", "to-retire", "decisions/x.md")

	// Pre-retire: FTS should surface the row.
	hits, err := FTSSearch(context.Background(), pool, "to retire", 5)
	if err != nil {
		t.Fatalf("pre-retire FTS: %v", err)
	}
	if len(hits) != 1 || hits[0] != id {
		t.Fatalf("pre-retire FTS hits = %v; want [%d]", hits, id)
	}

	if err := RetireOrphan(context.Background(), pool, id); err != nil {
		t.Fatalf("RetireOrphan: %v", err)
	}

	// Row preserved; status flipped to 'orphaned'.
	var status string
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT status FROM knowledge_pointers WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("post-retire SELECT: %v", err)
	}
	if status != "orphaned" {
		t.Errorf("status = %q; want orphaned (audit trail must survive retirement)", status)
	}

	// FTS entry removed so the row stops surfacing in knowledge_search.
	postHits, err := FTSSearch(context.Background(), pool, "to retire", 5)
	if err != nil {
		t.Fatalf("post-retire FTS: %v", err)
	}
	if len(postHits) != 0 {
		t.Errorf("post-retire FTS still surfaces id %v; want empty", postHits)
	}
}

func TestRetireOrphan_UnknownIDReturnsNotFound(t *testing.T) {
	pool := testutil.NewTestDB(t)
	err := RetireOrphan(context.Background(), pool, 99999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("RetireOrphan(unknown) err = %v; want ErrNotFound", err)
	}
}
