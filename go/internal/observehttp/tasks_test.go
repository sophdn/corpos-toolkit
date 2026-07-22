package observehttp

import (
	"net/http"
	"strings"
	"testing"

	"toolkit/internal/testutil"
)

func TestTasksList_FiltersByChain(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	c1 := seedChain(t, pool, "test", "c1", "open")
	c2 := seedChain(t, pool, "test", "c2", "open")
	seedTask(t, pool, c1, "t1", "pending")
	seedTask(t, pool, c2, "t2", "pending")

	srv := newTestServer(t, pool)
	var got []TaskRow
	getJSON(t, srv, "/tasks?chain_slug=c1", &got)
	if len(got) != 1 || got[0].Slug != "t1" || got[0].ChainSlug != "c1" {
		t.Fatalf("chain_slug filter wrong: %+v", got)
	}
}

func TestTasksList_FiltersByStatus(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	c := seedChain(t, pool, "test", "c", "open")
	seedTask(t, pool, c, "p1", "pending")
	seedTask(t, pool, c, "p2", "pending")
	seedTask(t, pool, c, "c1", "closed")

	srv := newTestServer(t, pool)
	var got []TaskRow
	getJSON(t, srv, "/tasks?status=closed", &got)
	if len(got) != 1 || got[0].Slug != "c1" {
		t.Fatalf("status filter wrong: %+v", got)
	}
}

func TestTasksList_OrdersByChainThenPosition(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	cb := seedChain(t, pool, "test", "b-chain", "open")
	ca := seedChain(t, pool, "test", "a-chain", "open")
	// Insert in position-shuffled order; expected output is a-chain
	// then b-chain, each by ascending position.
	testutil.SeedTask(t, pool, cb, "b2", "pending", testutil.SeedTaskOpts{Position: 2})
	testutil.SeedTask(t, pool, cb, "b1", "pending", testutil.SeedTaskOpts{Position: 1})
	testutil.SeedTask(t, pool, ca, "a2", "pending", testutil.SeedTaskOpts{Position: 2})
	testutil.SeedTask(t, pool, ca, "a1", "pending", testutil.SeedTaskOpts{Position: 1})

	srv := newTestServer(t, pool)
	var got []TaskRow
	getJSON(t, srv, "/tasks", &got)
	slugs := make([]string, len(got))
	for i, r := range got {
		slugs[i] = r.ChainSlug + "/" + r.Slug
	}
	want := []string{"a-chain/a1", "a-chain/a2", "b-chain/b1", "b-chain/b2"}
	for i, s := range want {
		if i >= len(slugs) || slugs[i] != s {
			t.Errorf("order = %v, want %v", slugs, want)
			break
		}
	}
}

func TestTasksSearch_EmptyPatternReturns400(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	code := getJSON(t, srv, "/tasks/search", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestTasksSearch_MatchesAndSnippets(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	c := seedChain(t, pool, "test", "search-chain", "open")
	testutil.SeedTask(t, pool, c, "has-needle", "pending", testutil.SeedTaskOpts{
		Position:         1,
		ProblemStatement: "Long prefix text and the keyword NEEDLE appears here in the body",
		HandoffOutput:    "NEEDLE in handoff too",
	})
	testutil.SeedTask(t, pool, c, "no-match", "pending", testutil.SeedTaskOpts{
		Position:         2,
		ProblemStatement: "nothing relevant",
	})

	srv := newTestServer(t, pool)
	var got SearchResponse
	if code := getJSON(t, srv, "/tasks/search?pattern=needle", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Pattern != "needle" {
		t.Errorf("pattern = %q", got.Pattern)
	}
	// Two field-level matches on the same task (problem_statement + handoff_output).
	if got.Count != 2 {
		t.Fatalf("count = %d, want 2; matches=%+v", got.Count, got.Matches)
	}
	for _, m := range got.Matches {
		if m.TaskSlug != "has-needle" {
			t.Errorf("unexpected task in match: %+v", m)
		}
		if !strings.Contains(strings.ToLower(m.Snippet), "needle") {
			t.Errorf("snippet missing needle: %q", m.Snippet)
		}
	}
}

func TestTasksSearch_TruncatedFlag(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "test")
	c := seedChain(t, pool, "test", "c", "open")
	for i := 0; i < 3; i++ {
		testutil.SeedTask(t, pool, c, "t"+string(rune('0'+i)), "pending", testutil.SeedTaskOpts{
			Position:         int64(i),
			ProblemStatement: "NEEDLE",
		})
	}
	srv := newTestServer(t, pool)
	var got SearchResponse
	getJSON(t, srv, "/tasks/search?pattern=NEEDLE&max_results=2", &got)
	if !got.Truncated {
		t.Errorf("truncated = false, want true (got count=%d)", got.Count)
	}
}

func TestExtractSnippet_EllipsesAroundLongBody(t *testing.T) {
	body := strings.Repeat("x", 150) + "NEEDLE" + strings.Repeat("y", 150)
	snip, ok := extractSnippet(body, "needle")
	if !ok {
		t.Fatal("extractSnippet not ok")
	}
	if !strings.HasPrefix(snip, "…") || !strings.HasSuffix(snip, "…") {
		t.Errorf("missing ellipses: %q", snip)
	}
	if !strings.Contains(strings.ToLower(snip), "needle") {
		t.Errorf("missing match: %q", snip)
	}
}

func TestExtractSnippet_AbsentReturnsNotOK(t *testing.T) {
	if _, ok := extractSnippet("hello world", "absent"); ok {
		t.Error("ok = true for absent pattern")
	}
}
