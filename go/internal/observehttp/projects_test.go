package observehttp

import (
	"net/http"
	"testing"

	"toolkit/internal/testutil"
)

func TestProjectsList_ReturnsAllRegisteredProjectsSortedByName(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "zebra-proj")
	seedProject(t, pool, "alpha-proj")
	seedProject(t, pool, "mcp-servers")

	srv := newTestServer(t, pool)
	var got []projectRow
	if code := getJSON(t, srv, "/projects", &got); code != http.StatusOK {
		t.Fatalf("got status %d", code)
	}
	if len(got) != 3 {
		t.Fatalf("got %d projects, want 3: %+v", len(got), got)
	}
	wantOrder := []string{"alpha-proj", "mcp-servers", "zebra-proj"}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Errorf("row %d: id=%q, want %q", i, got[i].ID, want)
		}
		if got[i].Name == "" {
			t.Errorf("row %d: empty name", i)
		}
	}
}

func TestProjectsList_EmptyWhenNoProjects(t *testing.T) {
	pool := testutil.NewTestDB(t)
	srv := newTestServer(t, pool)
	var got []projectRow
	if code := getJSON(t, srv, "/projects", &got); code != http.StatusOK {
		t.Fatalf("got status %d", code)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want empty: %+v", len(got), got)
	}
}
