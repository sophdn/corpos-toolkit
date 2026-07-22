package pointers

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

func seedPointer(t *testing.T, pool *db.Pool, question, invokeWhen, sourceRef string) int64 {
	t.Helper()
	id, err := Insert(context.Background(), pool, KnowledgePointer{
		ProjectID:  "test-proj",
		SourceType: "vault",
		SourceRef:  sourceRef,
		Question:   question,
		InvokeWhen: invokeWhen,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

// TestUpsert_LegacySlugFallsBackToSourceRefLookup pins the chain
// 617 T2 substrate fix: legacy vault pointer rows carry long-form
// slugs (`general/2026-05-07_<name>` from the early seeder) while
// post-rework forge writes produce the canonical bare `<name>` slug.
// Without the source_ref fallback in Upsert, a re-forge of a legacy
// row INSERTs in parallel and trips the UNIQUE constraint on
// (project_id, source_type, source_ref). The fix retries the lookup
// by source_ref when slug-lookup misses, then UPDATEs the row in
// place — normalising the slug to the canonical bare form in the
// same write.
func TestUpsert_LegacySlugFallsBackToSourceRefLookup(t *testing.T) {
	pool := testutil.NewTestDB(t)
	sourceRef := "learnings/general/2026-05-07_legacy-slug-target.md"
	legacySlug := "general/2026-05-07_legacy-slug-target"
	canonicalSlug := "legacy-slug-target"

	// Seed a legacy-shape pointer row mirroring what the early seeder
	// would have produced: long-form slug, vault project_id.
	legacyID, err := Insert(context.Background(), pool, KnowledgePointer{
		ProjectID:  "vault",
		SourceType: "vault",
		SourceRef:  sourceRef,
		Slug:       legacySlug,
		Question:   "old question",
		InvokeWhen: "old invoke_when",
		Tags:       []string{"legacy"},
	})
	if err != nil {
		t.Fatalf("seed legacy pointer: %v", err)
	}

	// Upsert with the canonical bare slug + same source_ref — must
	// resolve to the existing row via fallback and UPDATE in place
	// (not INSERT in parallel).
	res, err := Upsert(context.Background(), pool, KnowledgePointer{
		ProjectID:  "vault",
		SourceType: "vault",
		SourceRef:  sourceRef,
		Slug:       canonicalSlug,
		Question:   "new question",
		InvokeWhen: "new invoke_when",
		Tags:       []string{"normalised"},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.Action != "updated" {
		t.Errorf("action = %q, want 'updated' (source_ref fallback hit)", res.Action)
	}
	if res.ID != legacyID {
		t.Errorf("UPDATEd id = %d, want %d (original legacy row)", res.ID, legacyID)
	}

	// Row count for this source_ref must still be 1 — no parallel
	// row should have landed.
	var rowCount int
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM knowledge_pointers WHERE source_ref = ?`,
		sourceRef).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("knowledge_pointers row count for source_ref = %d, want 1", rowCount)
	}

	// Slug must be normalised to the canonical bare form.
	var gotSlug string
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT slug FROM knowledge_pointers WHERE id = ?`, legacyID).Scan(&gotSlug); err != nil {
		t.Fatalf("read slug: %v", err)
	}
	if gotSlug != canonicalSlug {
		t.Errorf("post-upsert slug = %q, want %q (normalisation)", gotSlug, canonicalSlug)
	}
}

// seedPointerWithSlug INSERTs a vault-shape pointer row with an explicit
// slug column value — the Insert helper above doesn't surface the slug
// column, so tests that need to pin two rows with distinct slugs
// pre-Upsert use raw SQL here.
func seedPointerWithSlug(t *testing.T, pool *db.Pool, projectID, sourceRef, slug, question, invokeWhen string) int64 {
	t.Helper()
	var id int64
	err := pool.WithWrite(context.Background(), func(tx *sql.Tx) error {
		row := tx.QueryRowContext(context.Background(),
			`INSERT INTO knowledge_pointers
				(project_id, source_type, source_ref, slug, question, invoke_when,
				 description, tags, status)
			 VALUES (?, 'vault', ?, ?, ?, ?, '', '[]', 'active')
			 RETURNING id`,
			projectID, sourceRef, slug, question, invokeWhen)
		if err := row.Scan(&id); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(),
			`INSERT INTO knowledge_pointers_fts (rowid, question, invoke_when) VALUES (?, ?, ?)`,
			id, question, invokeWhen)
		return err
	})
	if err != nil {
		t.Fatalf("seed pointer slug=%q: %v", slug, err)
	}
	return id
}

// TestUpsert_MergesConflictingSourceRefRow pins part 2 of bug
// forge-edit-non-declared-frontmatter-keys-cant-be-dropped-or-renamed-on-relocate.
// Setup: a canonical-bare-slug pointer for slug X at source_ref A and a
// legacy long-form-slug pointer at source_ref B both exist (e.g. a
// double-write the seeder + a post-rework re-forge produced). The
// caller then Upserts the canonical-slug row with source_ref=B (the
// physical file relocated). The slug-lookup HITS the canonical row;
// UPDATE source_ref=B would trip the table UNIQUE on
// (project_id, source_type, source_ref) because the legacy row still
// holds B. Fix: detect the conflict pre-UPDATE, delete the
// non-canonical (legacy) row in favor of the canonical-slug row,
// surface the merged id via UpsertResult.MergedConflictID.
func TestUpsert_MergesConflictingSourceRefRow(t *testing.T) {
	pool := testutil.NewTestDB(t)
	canonicalSlug := "merge-conflict-target"
	legacySlug := "general/2026-05-07_merge-conflict-target"
	originalSourceRef := "decisions/2026-05-20_merge-conflict-target.md"
	relocatedSourceRef := "learnings/general/2026-05-07_merge-conflict-target.md"

	// Seed the canonical-slug pointer at the original location (slug column
	// explicitly set so the slug-keyed lookup hits this row, not the legacy).
	canonicalID := seedPointerWithSlug(t, pool, "vault", originalSourceRef, canonicalSlug,
		"canonical question", "canonical invoke")
	// Seed the legacy long-form-slug pointer at the relocation target.
	legacyID := seedPointerWithSlug(t, pool, "vault", relocatedSourceRef, legacySlug,
		"legacy question", "legacy invoke")

	// Upsert the canonical-slug row with the relocated source_ref. The
	// pre-fix behavior tripped UNIQUE; the post-fix path merges the
	// legacy row away.
	res, err := Upsert(context.Background(), pool, KnowledgePointer{
		ProjectID:  "vault",
		SourceType: "vault",
		SourceRef:  relocatedSourceRef,
		Slug:       canonicalSlug,
		Question:   "post-relocate question",
		InvokeWhen: "post-relocate invoke",
		Tags:       []string{"merged"},
	})
	if err != nil {
		t.Fatalf("upsert (conflict path): %v", err)
	}
	if res.Action != "updated" {
		t.Errorf("action = %q, want 'updated'", res.Action)
	}
	if res.ID != canonicalID {
		t.Errorf("survived id = %d, want %d (canonical-slug row)", res.ID, canonicalID)
	}
	if res.MergedConflictID != legacyID {
		t.Errorf("MergedConflictID = %d, want %d (legacy long-form-slug row)", res.MergedConflictID, legacyID)
	}
	if res.PreviousSourceRef != originalSourceRef {
		t.Errorf("PreviousSourceRef = %q, want %q (canonical row's prior location)", res.PreviousSourceRef, originalSourceRef)
	}

	// Exactly one row should remain at relocatedSourceRef now, with the
	// canonical-slug.
	var rows int
	if err := pool.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM knowledge_pointers WHERE source_ref = ?`,
		relocatedSourceRef).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("post-merge row count for relocated source_ref = %d, want 1", rows)
	}
	// And zero rows for the original source_ref — the canonical row
	// moved, the legacy row at the new location was merged away.
	var origRows int
	pool.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM knowledge_pointers WHERE source_ref = ?`,
		originalSourceRef).Scan(&origRows)
	if origRows != 0 {
		t.Errorf("post-merge row count for original source_ref = %d, want 0", origRows)
	}
	// The legacy row's id must be gone.
	var legacyExists int
	pool.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM knowledge_pointers WHERE id = ?`, legacyID).Scan(&legacyExists)
	if legacyExists != 0 {
		t.Errorf("legacy row id=%d still present post-merge", legacyID)
	}
}

// TestUpsert_NoConflictDoesNotSetMergedConflictID guards the
// happy-path: when no row exists at the target source_ref, the
// merge logic is a no-op and MergedConflictID stays zero.
func TestUpsert_NoConflictDoesNotSetMergedConflictID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seededID := seedPointerWithSlug(t, pool, "vault",
		"decisions/2026-05-20_no-conflict.md", "no-conflict", "q", "w")
	res, err := Upsert(context.Background(), pool, KnowledgePointer{
		ProjectID:  "vault",
		SourceType: "vault",
		SourceRef:  "decisions/2026-05-20_no-conflict-moved.md",
		Slug:       "no-conflict",
		Question:   "q'",
		InvokeWhen: "w'",
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.ID != seededID {
		t.Errorf("id = %d, want %d", res.ID, seededID)
	}
	if res.MergedConflictID != 0 {
		t.Errorf("MergedConflictID = %d, want 0 on non-conflicting upsert", res.MergedConflictID)
	}
}

func TestInsert_SyncsFTSIndex(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedPointer(t, pool, "How do I run a retrieve?", "when planning retrieval calls", ".claude/vault/x.md")
	// FTS5 should match a content keyword.
	ids, err := FTSSearch(context.Background(), pool, "retrieve", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Errorf("expected FTS5 to surface id %d; got %v", id, ids)
	}
}

// Parity-pin: FTS5 OR tokenisation must split hyphenated terms (e.g. "two-pass")
// into "two OR pass" so MATCH doesn't trip on the "NOT may only appear right
// of AND" rule. See vault learning 2026-05-11_fts5-or-semantics-for-retrieval.
func TestFTSSearch_HyphenatedTermsSplitAndORed(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedPointer(t, pool, "How does two pass dispatch work?", "when reviewing retrieve cost", ".claude/vault/a.md")
	ids, err := FTSSearch(context.Background(), pool, "two-pass dispatch", 5)
	if err != nil {
		t.Fatalf("FTS5 with hyphen must not error; got: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("expected 1 hit, got %v", ids)
	}
}

func TestFTSSearch_EmptyQueryReturnsNil(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ids, err := FTSSearch(context.Background(), pool, "   ", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

func TestFTSSearch_OnlyNonAlphanumericTokensReturnsNil(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedPointer(t, pool, "x", "y", ".claude/vault/x.md")
	ids, err := FTSSearch(context.Background(), pool, "!!! ??? +++", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("punctuation-only query must return empty; got %v", ids)
	}
}

func TestGetByIDs_PreservesInputOrder(t *testing.T) {
	pool := testutil.NewTestDB(t)
	idA := seedPointer(t, pool, "alpha topic", "when alphabet", ".claude/vault/a.md")
	idB := seedPointer(t, pool, "beta topic", "when beta", ".claude/vault/b.md")
	idC := seedPointer(t, pool, "gamma topic", "when gamma", ".claude/vault/c.md")
	got, err := GetByIDs(context.Background(), pool, []int64{idC, idA, idB})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != idC || got[1].ID != idA || got[2].ID != idB {
		t.Errorf("input order not preserved; got %v", []int64{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestGetByIDs_SkipsMissing(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedPointer(t, pool, "q", "w", ".claude/vault/x.md")
	got, err := GetByIDs(context.Background(), pool, []int64{id, 99999})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Errorf("missing id should be silently skipped; got %v", got)
	}
}

func TestGetByIDs_EmptyInput(t *testing.T) {
	pool := testutil.NewTestDB(t)
	got, err := GetByIDs(context.Background(), pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestIncrementUsage_BumpsCounterAndTimestamp(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedPointer(t, pool, "q", "w", ".claude/vault/x.md")
	for i := 0; i < 3; i++ {
		if err := IncrementUsage(context.Background(), pool, id); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}
	got, _ := GetByIDs(context.Background(), pool, []int64{id})
	if got[0].UsageCount != 3 {
		t.Errorf("expected usage_count=3, got %d", got[0].UsageCount)
	}
	if got[0].LastUsedAt == nil {
		t.Errorf("last_used_at must be stamped")
	}
}

func TestRecordMiss_IncrementsCounterAndSetsHint(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedPointer(t, pool, "q", "w", ".claude/vault/x.md")
	reason := "moved"
	if err := RecordMiss(context.Background(), pool, id, &reason); err != nil {
		t.Fatalf("record_miss: %v", err)
	}
	got, _ := GetByIDs(context.Background(), pool, []int64{id})
	if got[0].NegativeFeedbackCount != 1 {
		t.Errorf("counter: %d", got[0].NegativeFeedbackCount)
	}
	if got[0].StalenessHint == nil || *got[0].StalenessHint != "moved" {
		t.Errorf("hint: %v", got[0].StalenessHint)
	}
}

// Parity-pin: a subsequent record_miss without a reason must not erase the
// hint set by a prior call (COALESCE preserves the existing value).
func TestRecordMiss_PreservesHintOnNilReason(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id := seedPointer(t, pool, "q", "w", ".claude/vault/x.md")
	first := "first"
	_ = RecordMiss(context.Background(), pool, id, &first)
	_ = RecordMiss(context.Background(), pool, id, nil)
	got, _ := GetByIDs(context.Background(), pool, []int64{id})
	if got[0].StalenessHint == nil || *got[0].StalenessHint != "first" {
		t.Errorf("hint must be preserved: %v", got[0].StalenessHint)
	}
	if got[0].NegativeFeedbackCount != 2 {
		t.Errorf("counter: %d", got[0].NegativeFeedbackCount)
	}
}

func TestFTSSearch_StripsBareLeadingDigits(t *testing.T) {
	// Vault path 2026-05-12_... starts with a digit; the question text is what
	// we search, so this only validates that paths with digits remain queryable.
	pool := testutil.NewTestDB(t)
	id := seedPointer(t, pool, "dated note about retrieval", "when retrieving", ".claude/vault/2026-05-12_x.md")
	ids, _ := FTSSearch(context.Background(), pool, "dated retrieval", 5)
	if !reflect.DeepEqual(ids, []int64{id}) {
		t.Errorf("expected [%d], got %v", id, ids)
	}
}
