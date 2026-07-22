package curation_test

import (
	"context"
	"errors"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
	"toolkit/internal/testutil"
)

func validInsert() curation.CandidateInsert {
	return curation.CandidateInsert{
		ProjectID:   "mcp-servers",
		SourceType:  "task",
		SourceRef:   "mcp-servers::sample-task",
		Question:    "What does sample-task ship?",
		InvokeWhen:  "When investigating sample-task scope.",
		Description: "Sample task body for tests.",
		Tags:        []string{"test"},
		Origin:      "task_handoff",
	}
}

func TestAddCandidate_HappyPath(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, err := curation.AddCandidate(context.Background(), pool, validInsert())
	if err != nil {
		t.Fatalf("AddCandidate: %v", err)
	}
	if id == 0 {
		t.Fatal("AddCandidate returned id=0")
	}

	got, err := curation.ReadCandidate(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("ReadCandidate: %v", err)
	}
	if got.Question != "What does sample-task ship?" {
		t.Errorf("Question: got %q", got.Question)
	}
	if got.Status != "pending" {
		t.Errorf("Status: want pending, got %q", got.Status)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "test" {
		t.Errorf("Tags: got %v", got.Tags)
	}
}

func TestAddCandidate_RejectsEmptyQuestion(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ins := validInsert()
	ins.Question = "  "
	_, err := curation.AddCandidate(context.Background(), pool, ins)
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

func TestAddCandidate_RejectsEmptyInvokeWhen(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ins := validInsert()
	ins.InvokeWhen = ""
	_, err := curation.AddCandidate(context.Background(), pool, ins)
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

func TestAddCandidate_RejectsEmptyDescription(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ins := validInsert()
	ins.Description = ""
	_, err := curation.AddCandidate(context.Background(), pool, ins)
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

func TestAddCandidate_RejectsInvalidOrigin(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ins := validInsert()
	ins.Origin = "not_a_real_origin"
	_, err := curation.AddCandidate(context.Background(), pool, ins)
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

func TestAddCandidate_RejectsEmptySourceRef(t *testing.T) {
	pool := testutil.NewTestDB(t)
	ins := validInsert()
	ins.SourceRef = ""
	_, err := curation.AddCandidate(context.Background(), pool, ins)
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

func TestReadCandidate_NotFound(t *testing.T) {
	pool := testutil.NewTestDB(t)
	_, err := curation.ReadCandidate(context.Background(), pool, 99999)
	if !errors.Is(err, curation.ErrCandidateNotFound) {
		t.Fatalf("want ErrCandidateNotFound, got %v", err)
	}
}

func TestListPending_FiltersAndOrdering(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedSeveral(t, pool)

	all, err := curation.ListPending(context.Background(), pool, curation.ListFilter{})
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListPending all pending: want 3, got %d", len(all))
	}
	// Quality DESC NULLS LAST: 0.9 then 0.5 then NULL.
	if all[0].QualityScore == nil || *all[0].QualityScore != 0.9 {
		t.Errorf("first row should be 0.9, got %+v", all[0].QualityScore)
	}
	if all[2].QualityScore != nil {
		t.Errorf("last row should be unscored, got %+v", all[2].QualityScore)
	}
}

func TestListPending_UnscoredOnly(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedSeveral(t, pool)

	unscored, err := curation.ListPending(context.Background(), pool, curation.ListFilter{
		UnscoredOnly: true,
	})
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(unscored) != 1 {
		t.Fatalf("UnscoredOnly: want 1, got %d", len(unscored))
	}
	if unscored[0].QualityScore != nil {
		t.Errorf("unscored row has score: %v", *unscored[0].QualityScore)
	}
}

func TestListPending_ProjectFilter(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedSeveral(t, pool)
	// Insert one for a different project.
	ins := validInsert()
	ins.ProjectID = "other"
	ins.SourceRef = "other::candidate-x"
	if _, err := curation.AddCandidate(context.Background(), pool, ins); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	mcp, err := curation.ListPending(context.Background(), pool, curation.ListFilter{
		ProjectID: "mcp-servers",
	})
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(mcp) != 3 {
		t.Errorf("project-scoped: want 3, got %d", len(mcp))
	}

	other, err := curation.ListPending(context.Background(), pool, curation.ListFilter{
		ProjectID: "other",
	})
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(other) != 1 {
		t.Errorf("project-scoped 'other': want 1, got %d", len(other))
	}
}

func TestUpdateCandidateScoring_HappyPath(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, err := curation.AddCandidate(context.Background(), pool, validInsert())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = curation.UpdateCandidateScoring(context.Background(), pool, id, curation.ExtractedMeta{
		Question:    "Updated question",
		InvokeWhen:  "Updated invoke_when",
		Description: "Updated description",
	}, 0.92)
	if err != nil {
		t.Fatalf("UpdateCandidateScoring: %v", err)
	}

	got, err := curation.ReadCandidate(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Question != "Updated question" {
		t.Errorf("Question not updated: %q", got.Question)
	}
	if got.QualityScore == nil || *got.QualityScore != 0.92 {
		t.Errorf("QualityScore: got %v", got.QualityScore)
	}
}

func TestUpdateCandidateScoring_PreservesDescriptionWhenEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, err := curation.AddCandidate(context.Background(), pool, validInsert())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = curation.UpdateCandidateScoring(context.Background(), pool, id, curation.ExtractedMeta{
		Question:    "Updated question",
		InvokeWhen:  "Updated invoke_when",
		Description: "",
	}, 0.5)
	if err != nil {
		t.Fatalf("UpdateCandidateScoring: %v", err)
	}

	got, _ := curation.ReadCandidate(context.Background(), pool, id)
	if got.Description == "" {
		t.Errorf("Description erased when meta.Description was empty (should preserve)")
	}
}

func TestUpdateCandidateScoring_RefusesOnNonPending(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, _ := curation.AddCandidate(context.Background(), pool, validInsert())
	// Flip status to rejected.
	if _, err := pool.DB().Exec(
		`UPDATE curation_candidates SET status='rejected' WHERE id=?`, id); err != nil {
		t.Fatalf("flip status: %v", err)
	}

	err := curation.UpdateCandidateScoring(context.Background(), pool, id, curation.ExtractedMeta{
		Question:   "Q",
		InvokeWhen: "W",
	}, 0.5)
	if !errors.Is(err, curation.ErrCandidateNotFound) {
		t.Fatalf("want ErrCandidateNotFound, got %v", err)
	}
}

func TestPromoteCandidate_HappyPath(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, _ := curation.AddCandidate(context.Background(), pool, validInsert())

	pointerID, err := curation.PromoteCandidate(context.Background(), pool, id, true)
	if err != nil {
		t.Fatalf("PromoteCandidate: %v", err)
	}
	if pointerID == 0 {
		t.Fatal("PromoteCandidate returned pointer id=0")
	}

	// Candidate marked promoted.
	got, _ := curation.ReadCandidate(context.Background(), pool, id)
	if got.Status != "promoted" {
		t.Errorf("status: want promoted, got %q", got.Status)
	}
	if !got.PromotedAutomatically {
		t.Errorf("PromotedAutomatically: want true (passed true), got false")
	}

	// Pointer row exists with the expected source_ref.
	var srcRef string
	if err := pool.DB().QueryRow(
		`SELECT source_ref FROM knowledge_pointers WHERE id=?`, pointerID,
	).Scan(&srcRef); err != nil {
		t.Fatalf("pointer query: %v", err)
	}
	if srcRef != "mcp-servers::sample-task" {
		t.Errorf("pointer source_ref: got %q", srcRef)
	}
}

func TestPromoteCandidate_RefusesOnNonPending(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, _ := curation.AddCandidate(context.Background(), pool, validInsert())
	if _, err := pool.DB().Exec(
		`UPDATE curation_candidates SET status='rejected' WHERE id=?`, id); err != nil {
		t.Fatalf("flip status: %v", err)
	}
	_, err := curation.PromoteCandidate(context.Background(), pool, id, false)
	if !errors.Is(err, curation.ErrCandidateNotFound) {
		t.Fatalf("want ErrCandidateNotFound, got %v", err)
	}
}

func TestRejectCandidate_HappyPath(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, _ := curation.AddCandidate(context.Background(), pool, validInsert())

	err := curation.RejectCandidate(context.Background(), pool, id, "off-topic — session noise")
	if err != nil {
		t.Fatalf("RejectCandidate: %v", err)
	}

	got, _ := curation.ReadCandidate(context.Background(), pool, id)
	if got.Status != "rejected" {
		t.Errorf("status: want rejected, got %q", got.Status)
	}
	// Reason recorded in tags (per the schema-compatible piggyback).
	foundReason := false
	for _, tag := range got.Tags {
		if tag == "rejected_reason: off-topic — session noise" {
			foundReason = true
			break
		}
	}
	if !foundReason {
		t.Errorf("rejected_reason not in tags: %v", got.Tags)
	}
}

func TestRejectCandidate_RefusesEmptyReason(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, _ := curation.AddCandidate(context.Background(), pool, validInsert())
	err := curation.RejectCandidate(context.Background(), pool, id, "   ")
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

func TestRejectCandidate_RefusesOnNonPending(t *testing.T) {
	pool := testutil.NewTestDB(t)
	id, _ := curation.AddCandidate(context.Background(), pool, validInsert())
	if _, err := pool.DB().Exec(
		`UPDATE curation_candidates SET status='promoted' WHERE id=?`, id); err != nil {
		t.Fatalf("flip status: %v", err)
	}
	err := curation.RejectCandidate(context.Background(), pool, id, "too late")
	if !errors.Is(err, curation.ErrCandidateNotFound) {
		t.Fatalf("want ErrCandidateNotFound, got %v", err)
	}
}

func TestAddPointerLink_HappyPath(t *testing.T) {
	pool := testutil.NewTestDB(t)
	// Need two pointers.
	a := seedPointer(t, pool, "A")
	b := seedPointer(t, pool, "B")

	err := curation.AddPointerLink(context.Background(), pool, a, b, "see-also", false)
	if err != nil {
		t.Fatalf("AddPointerLink: %v", err)
	}

	var rel string
	var confirmed int
	if err := pool.DB().QueryRow(
		`SELECT relationship, confirmed FROM pointer_links WHERE pointer_id=? AND related_id=?`,
		a, b).Scan(&rel, &confirmed); err != nil {
		t.Fatalf("query: %v", err)
	}
	if rel != "see-also" {
		t.Errorf("relationship: got %q", rel)
	}
	if confirmed != 0 {
		t.Errorf("confirmed: got %d", confirmed)
	}
}

func TestAddPointerLink_RejectsSamePointerToItself(t *testing.T) {
	pool := testutil.NewTestDB(t)
	a := seedPointer(t, pool, "self")
	err := curation.AddPointerLink(context.Background(), pool, a, a, "see-also", false)
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

func TestAddPointerLink_RejectsInvalidRelationship(t *testing.T) {
	pool := testutil.NewTestDB(t)
	a := seedPointer(t, pool, "a")
	b := seedPointer(t, pool, "b")
	err := curation.AddPointerLink(context.Background(), pool, a, b, "is-friends-with", false)
	if !errors.Is(err, curation.ErrInvalidCandidate) {
		t.Fatalf("want ErrInvalidCandidate, got %v", err)
	}
}

// --- helpers ---

func seedSeveral(t *testing.T, pool *db.Pool) {
	t.Helper()
	// One scored 0.9, one scored 0.5, one unscored.
	high := validInsert()
	high.SourceRef = "mcp-servers::high"
	score := 0.9
	high.QualityScore = &score
	if _, err := curation.AddCandidate(context.Background(), pool, high); err != nil {
		t.Fatalf("seed high: %v", err)
	}

	mid := validInsert()
	mid.SourceRef = "mcp-servers::mid"
	midScore := 0.5
	mid.QualityScore = &midScore
	if _, err := curation.AddCandidate(context.Background(), pool, mid); err != nil {
		t.Fatalf("seed mid: %v", err)
	}

	low := validInsert()
	low.SourceRef = "mcp-servers::low"
	if _, err := curation.AddCandidate(context.Background(), pool, low); err != nil {
		t.Fatalf("seed low: %v", err)
	}
}

func seedPointer(t *testing.T, pool *db.Pool, label string) int64 {
	t.Helper()
	var id int64
	err := pool.DB().QueryRow(
		`INSERT INTO knowledge_pointers
		    (project_id, source_type, source_ref, question, invoke_when, tags, status)
		 VALUES ('test', 'vault', ?, ?, 'when', '[]', 'active')
		 RETURNING id`,
		"vault/"+label+".md", "Q for "+label,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedPointer: %v", err)
	}
	return id
}
