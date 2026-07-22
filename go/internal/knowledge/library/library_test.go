package library

import (
	"context"
	"errors"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/testutil"
)

const testProject = "test-proj"

// setupProject registers a project so the library_entries.project_id FK
// is satisfied. The current Go pool doesn't enforce FK by default, but
// the SQL is correct and forward-portable.
func setupProject(t *testing.T, pool *db.Pool) {
	t.Helper()
	_, err := pool.DB().Exec(`INSERT OR IGNORE INTO projects (id, slug) VALUES (?, ?)`, testProject, testProject)
	if err != nil {
		// The projects table schema may differ; fall through silently if so —
		// the FK is OFF in test pools so missing rows don't block insert.
		_ = err
	}
}

func ptrU32(n uint32) *uint32 { return &n }

func sampleEntry(dewey string) LibraryEntry {
	return LibraryEntry{
		Dewey: dewey,
		Citation: Citation{
			Raw:           "Doe, J. (2025). Foo bar.",
			PrimaryAuthor: "Doe",
			Year:          ptrU32(2025),
		},
		Status:        EntryStatus{Type: "active"},
		Establishes:   "Establishes baseline foo behaviour.",
		WhatItAnswers: "What is foo?",
		InvokeWhen:    "When deciding how to foo",
		Tags:          []string{"foo", "baseline"},
		IndexPointers: []IndexPointer{{Section: "FoundationalDecisions", Question: "How does foo work?", Role: "primary"}},
	}
}

// ── ValidateDewey ────────────────────────────────────────────────────

func TestValidateDewey_AcceptsValidShapes(t *testing.T) {
	for _, ok := range []string{"500", "510.5", "999.0001"} {
		if err := ValidateDewey(ok); err != nil {
			t.Errorf("expected %q valid, got %v", ok, err)
		}
	}
}

func TestValidateDewey_RejectsInvalidShapes(t *testing.T) {
	for _, bad := range []string{"", "1", "12", "abc", "1234", "12.3", "500.", "500.a"} {
		if err := ValidateDewey(bad); !errors.Is(err, ErrInvalidDewey) {
			t.Errorf("expected %q rejected, got %v", bad, err)
		}
	}
}

// ── Add + Get roundtrip ─────────────────────────────────────────────

func TestAddGet_Roundtrip(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	in := sampleEntry("500.42")
	if err := Add(ctx, pool, testProject, in); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := Get(ctx, pool, testProject, "500.42")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Dewey != in.Dewey || got.Citation.PrimaryAuthor != "Doe" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.Citation.Year == nil || *got.Citation.Year != 2025 {
		t.Errorf("year roundtrip: %v", got.Citation.Year)
	}
	if got.Establishes != in.Establishes || got.InvokeWhen != in.InvokeWhen {
		t.Errorf("string fields drift")
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags roundtrip: %v", got.Tags)
	}
	if len(got.IndexPointers) != 1 || got.IndexPointers[0].Section != "FoundationalDecisions" {
		t.Errorf("index_pointers roundtrip: %v", got.IndexPointers)
	}
	if got.Status.Type != "active" {
		t.Errorf("status: %v", got.Status)
	}
	if got.LastUpdated == "" {
		t.Errorf("last_updated should be set by datetime('now')")
	}
}

func TestAdd_RejectsDuplicateDewey(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	in := sampleEntry("500.42")
	if err := Add(ctx, pool, testProject, in); err != nil {
		t.Fatalf("add: %v", err)
	}
	err := Add(ctx, pool, testProject, in)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestAdd_ValidationErrors(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()

	bad := sampleEntry("500.42")
	bad.Citation.PrimaryAuthor = ""
	if err := Add(ctx, pool, testProject, bad); !errors.Is(err, ErrValidation) {
		t.Errorf("missing primary_author should fail validation; got %v", err)
	}

	bad = sampleEntry("500.42")
	bad.Citation.Raw = "  "
	if err := Add(ctx, pool, testProject, bad); !errors.Is(err, ErrValidation) {
		t.Errorf("missing citation.raw should fail validation; got %v", err)
	}

	bad = sampleEntry("500.42")
	bad.IndexPointers = []IndexPointer{{Section: "S", Question: "", Role: "r"}}
	if err := Add(ctx, pool, testProject, bad); !errors.Is(err, ErrValidation) {
		t.Errorf("empty pointer question should fail validation; got %v", err)
	}

	bad = sampleEntry("bad")
	if err := Add(ctx, pool, testProject, bad); !errors.Is(err, ErrInvalidDewey) {
		t.Errorf("bad dewey should fail; got %v", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	_, err := Get(context.Background(), pool, testProject, "999")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ── Update + Retire ─────────────────────────────────────────────────

func TestUpdate_PartialFieldsChanged(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	in := sampleEntry("500.42")
	_ = Add(ctx, pool, testProject, in)
	newEstablishes := "Revised baseline."
	tags := []string{"updated"}
	result, err := Update(ctx, pool, testProject, "500.42", EntryUpdate{
		Establishes: &newEstablishes,
		Tags:        &tags,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(result.FieldsChanged) != 2 {
		t.Errorf("fields_changed: %v", result.FieldsChanged)
	}
	got, _ := Get(ctx, pool, testProject, "500.42")
	if got.Establishes != newEstablishes {
		t.Errorf("establishes not updated: %q", got.Establishes)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "updated" {
		t.Errorf("tags not updated: %v", got.Tags)
	}
}

func TestRetire_FlipsStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	_ = Add(ctx, pool, testProject, sampleEntry("500.42"))
	if err := Retire(ctx, pool, testProject, "500.42", "superseded"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	got, _ := Get(ctx, pool, testProject, "500.42")
	if got.Status.Type != "retired" {
		t.Errorf("status: %v", got.Status)
	}
}

func TestRetire_RejectsAlreadyRetired(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	_ = Add(ctx, pool, testProject, sampleEntry("500.42"))
	_ = Retire(ctx, pool, testProject, "500.42", "first")
	err := Retire(ctx, pool, testProject, "500.42", "second")
	if !errors.Is(err, ErrValidation) {
		t.Errorf("expected ErrValidation on double-retire, got %v", err)
	}
}

// ── List / ListSections / ListDewey ─────────────────────────────────

func TestListActive_ExcludesRetired(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	a := sampleEntry("500.1")
	b := sampleEntry("500.2")
	_ = Add(ctx, pool, testProject, a)
	_ = Add(ctx, pool, testProject, b)
	_ = Retire(ctx, pool, testProject, "500.2", "out")
	entries, _ := ListActive(ctx, pool, testProject)
	if len(entries) != 1 || entries[0].Dewey != "500.1" {
		t.Errorf("expected only active; got %v", entries)
	}
}

func TestListSections_DedupesAcrossEntries(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	a := sampleEntry("500.1")
	b := sampleEntry("500.2")
	b.IndexPointers = []IndexPointer{
		{Section: "OtherSection", Question: "Why?", Role: "primary"},
		{Section: "FoundationalDecisions", Question: "Already exists?", Role: "primary"},
	}
	_ = Add(ctx, pool, testProject, a)
	_ = Add(ctx, pool, testProject, b)
	sections, _ := ListSections(ctx, pool, testProject)
	want := []string{"FoundationalDecisions", "OtherSection"}
	if len(sections) != len(want) || sections[0] != want[0] || sections[1] != want[1] {
		t.Errorf("got %v want %v", sections, want)
	}
}

func TestListDeweyByPrefix(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	for _, d := range []string{"500.1", "500.2", "510.1"} {
		_ = Add(ctx, pool, testProject, sampleEntry(d))
	}
	got, _ := ListDeweyByPrefix(ctx, pool, testProject, "500")
	if len(got) != 2 || got[0] != "500.1" || got[1] != "500.2" {
		t.Errorf("prefix 500 mismatch: %v", got)
	}
	all, _ := ListDeweyByPrefix(ctx, pool, testProject, "")
	if len(all) != 3 {
		t.Errorf("empty prefix should return all 3: %v", all)
	}
}

// ── Find ────────────────────────────────────────────────────────────

func TestFindKeyword_MatchesPriorityField(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	e := sampleEntry("500.42")
	e.Establishes = "Establishes the cromulent baseline."
	_ = Add(ctx, pool, testProject, e)
	matches, _ := FindKeyword(ctx, pool, testProject, "cromulent")
	if len(matches) != 1 || matches[0].MatchedField != "establishes" {
		t.Errorf("priority field mismatch: %+v", matches)
	}
}

func TestFindKeyword_FallsThroughToTagsAndPointers(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	e := sampleEntry("500.42")
	e.Establishes = "x"
	e.InvokeWhen = "y"
	e.Citation.PrimaryAuthor = "z"
	e.Tags = []string{"unique-tag"}
	_ = Add(ctx, pool, testProject, e)
	matches, _ := FindKeyword(ctx, pool, testProject, "unique-tag")
	if len(matches) != 1 || matches[0].MatchedField != "tags" {
		t.Errorf("expected tags match; got %+v", matches)
	}
}

func TestFindSemantic_FiltersBySection(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	a := sampleEntry("500.1")
	b := sampleEntry("500.2")
	b.IndexPointers = []IndexPointer{{Section: "OtherSection", Question: "?", Role: "primary"}}
	_ = Add(ctx, pool, testProject, a)
	_ = Add(ctx, pool, testProject, b)
	res, _ := FindSemantic(ctx, pool, testProject, "FoundationalDecisions")
	if len(res) != 1 || res[0].Dewey != "500.1" {
		t.Errorf("semantic mode filter mismatch: %v", res)
	}
}

func TestFindManifest_CapsSummaryAt160Chars(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	e := sampleEntry("500.1")
	e.WhatItAnswers = long
	_ = Add(ctx, pool, testProject, e)
	res, _ := FindManifest(ctx, pool, testProject, "FoundationalDecisions")
	if len(res) != 1 {
		t.Fatalf("expected 1, got %v", res)
	}
	runes := []rune(res[0].WhatItAnswersSummary)
	if len(runes) != 161 { // 160 + ellipsis
		t.Errorf("summary length: %d", len(runes))
	}
}

// ── CrossReference ──────────────────────────────────────────────────

func TestCrossReference_SectionMode(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	a := sampleEntry("500.1")
	b := sampleEntry("500.2")
	b.IndexPointers = []IndexPointer{
		{Section: "FoundationalDecisions", Question: "different", Role: "secondary"},
	}
	c := sampleEntry("500.3")
	c.IndexPointers = []IndexPointer{{Section: "OtherSection", Question: "?", Role: "primary"}}
	_ = Add(ctx, pool, testProject, a)
	_ = Add(ctx, pool, testProject, b)
	_ = Add(ctx, pool, testProject, c)
	res, err := CrossReference(ctx, pool, testProject, "500.1", CrossRefModeSection)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.BySection["FoundationalDecisions"]) != 1 {
		t.Errorf("section group: %v", res.BySection)
	}
	if len(res.BySection["OtherSection"]) != 0 {
		t.Errorf("other section should not appear when target lacks that section")
	}
}

func TestCrossReference_QuestionMode(t *testing.T) {
	pool := testutil.NewTestDB(t)
	setupProject(t, pool)
	ctx := context.Background()
	a := sampleEntry("500.1")
	b := sampleEntry("500.2")
	b.IndexPointers = []IndexPointer{
		{Section: "FoundationalDecisions", Question: "How does foo work?", Role: "secondary"},
	}
	c := sampleEntry("500.3")
	c.IndexPointers = []IndexPointer{
		{Section: "FoundationalDecisions", Question: "Other question", Role: "primary"},
	}
	_ = Add(ctx, pool, testProject, a)
	_ = Add(ctx, pool, testProject, b)
	_ = Add(ctx, pool, testProject, c)
	res, err := CrossReference(ctx, pool, testProject, "500.1", CrossRefModeQuestion)
	if err != nil {
		t.Fatal(err)
	}
	key := "FoundationalDecisions::How does foo work?"
	if len(res.ByQuestion[key]) != 1 {
		t.Errorf("question group: %v", res.ByQuestion)
	}
	// Question-mode entries carry role.
	if got := res.ByQuestion[key]; len(got) > 0 && (got[0].Role == nil || *got[0].Role != "secondary") {
		t.Errorf("role missing or wrong: %v", got)
	}
}
