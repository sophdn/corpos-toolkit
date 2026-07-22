package testutil_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"toolkit/internal/testutil"
)

// TestNewTestDB_SchemaApplied verifies that NewTestDB returns a DB with the
// full toolkit schema present (all migration tables exist and are queryable).
func TestNewTestDB_SchemaApplied(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		// Post-agent-substrate-crud-retirement T6: artifact CRUD
		// tables (chains/tasks/bugs/...) are dropped; reads flow
		// through projection tables, writes flow through the events
		// log. Smoke against the projection schema + the events log.
		{name: "proj_chain_status table", query: "SELECT COUNT(*) FROM proj_chain_status"},
		{name: "proj_current_tasks table", query: "SELECT COUNT(*) FROM proj_current_tasks"},
		{name: "proj_current_bugs table", query: "SELECT COUNT(*) FROM proj_current_bugs"},
		{name: "events table", query: "SELECT COUNT(*) FROM events"},
		{name: "knowledge_pointers table", query: "SELECT COUNT(*) FROM knowledge_pointers"},
		{name: "pointer_links table", query: "SELECT COUNT(*) FROM pointer_links"},
	}

	p := testutil.NewTestDB(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var count int
			if err := p.DB().QueryRow(tt.query).Scan(&count); err != nil {
				t.Errorf("query %q: %v", tt.query, err)
			}
		})
	}
}

// TestMockLlamaCPP_RespondsToRegisteredPaths demonstrates the table-driven
// test pattern used throughout the handler test suite.
func TestMockLlamaCPP_RespondsToRegisteredPaths(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantCode int
		wantBody string
	}{
		{name: "registered path returns 200", path: "/completion", wantCode: 200, wantBody: "token"},
		{name: "unknown path returns 404", path: "/unknown", wantCode: 404, wantBody: "no mock"},
	}

	srv := testutil.MockLlamaCPP(t, map[string]json.RawMessage{
		"/completion": testutil.JSON(t, map[string]string{"content": "token"}),
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantCode {
				t.Errorf("status: want %d, got %d", tt.wantCode, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tt.wantBody) {
				t.Errorf("body %q does not contain %q", body, tt.wantBody)
			}
		})
	}
}

// Bug 1323 regression: when SQLite reports `no such module: fts5`, the
// migration error is wrapped with an actionable hint naming the canonical
// `make -C go test` invocation. The raw SQLite error doesn't tell the
// reader they need a build tag; the wrapped message does.
//
// We can't easily simulate the missing-fts5 scenario inside a test
// binary that itself runs with sqlite_fts5 enabled, so the regression
// asserts on the matcher + format string directly. If a future refactor
// removes the hint, the matcher's required substrings will trip.
func TestNewTestDB_MissingFTS5HintMentionsBuildTag(t *testing.T) {
	tests := []struct {
		name    string
		errText string
		want    bool
	}{
		{"canonical sqlite error", "apply 020_knowledge_pointers.sql: no such module: fts5", true},
		{"capitalised variant", "No Such Module: FTS5", true},
		{"unrelated sqlite error", "apply 999_widget.sql: syntax error near WIDGET", false},
		{"nil-ish empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := testutil.IsMissingFTS5Error(stringError(tt.errText))
			if got != tt.want {
				t.Errorf("isMissingFTS5Error(%q) = %v, want %v", tt.errText, got, tt.want)
			}
		})
	}

	// The wrapped error text must include both invocations + the tag
	// name so the reader has at least two ways to fix the problem.
	hint := testutil.MissingFTS5HintFmt
	for _, want := range []string{
		"sqlite_fts5",
		"make -C go test",
		"-tags sqlite_fts5",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("missing-fts5 hint must mention %q; got %q", want, hint)
		}
	}
}

// stringError is a minimal error wrapper for the test above; using
// errors.New from the stdlib pulls a transitive dep just for one line.
type stringError string

func (s stringError) Error() string { return string(s) }

// TestMockAnthropic_RespondsToRegisteredPaths mirrors the above for the
// Anthropic mock, proving both mocks follow the same table-driven shape.
func TestMockAnthropic_RespondsToRegisteredPaths(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantCode int
		wantBody string
	}{
		{name: "registered path returns 200", path: "/v1/messages", wantCode: 200, wantBody: "content"},
		{name: "unknown path returns 404", path: "/v1/nope", wantCode: 404, wantBody: "not_found"},
	}

	srv := testutil.MockAnthropic(t, map[string]json.RawMessage{
		"/v1/messages": json.RawMessage(`{"type":"message","content":[]}`),
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantCode {
				t.Errorf("status: want %d, got %d", tt.wantCode, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tt.wantBody) {
				t.Errorf("body %q does not contain %q", body, tt.wantBody)
			}
		})
	}
}
