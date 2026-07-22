// Package testutil provides shared test infrastructure for toolkit-server handler tests.
//
// **All Go tests in this repo require the `sqlite_fts5` build tag.** The
// FTS5 module is compiled into the mattn/go-sqlite3 driver only when this
// tag is present, and several migrations (knowledge_pointers, etc.) create
// FTS5 virtual tables. Run tests via `make -C go test` (canonical) or
// `go test -tags sqlite_fts5 ./...`. A raw `go test ./...` produces
// cryptic "no such module: fts5" errors when db.Open runs migrations.
//
// db.Open detects the missing-fts5 SQLite error and wraps it with an
// actionable hint via [db.MissingFTS5HintFmt] (originally bug 1323;
// migrated from testutil into the db package as part of bug 1326 when
// the migration runner moved into db.Open).
package testutil

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"toolkit/internal/db"
)

// migrationFS keeps a hermetic copy of the schema migrations under this
// package. It is NOT consumed by NewTestDB — db.Open runs migrations
// from its own embed — but a handful of regression tests in
// testutil_test.go read individual migration files to assert documentation
// comments. Keep this fixture in sync with crates/shared-db/migrations/
// (the canonical source-of-truth) and go/internal/db/migrations/.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// Compile-time use so the embed isn't flagged as unused by the linter when
// no test file in this package reads it directly.
var _ = migrationFS

// MissingFTS5HintFmt re-exports [db.MissingFTS5HintFmt] for tests that
// still reference the testutil-scoped name. Prefer the db-package
// constant in new code.
const MissingFTS5HintFmt = db.MissingFTS5HintFmt

// IsMissingFTS5Error re-exports [db.IsMissingFTS5Error] for the same
// backwards-compatibility reason.
func IsMissingFTS5Error(err error) bool { return db.IsMissingFTS5Error(err) }

// NewTestDB creates a temporary SQLite database with the full toolkit schema applied.
// The database is closed and the temp directory removed via t.Cleanup.
//
// As of bug 1326 the schema is applied by db.Open itself; this helper
// simply opens a temp-path pool. A failure here from a missing FTS5
// build tag surfaces as a wrapped error via [db.MissingFTS5HintFmt].
//
// Post-agent-substrate-crud-retirement T6: there's no
// InstallProjectionMirrorTriggers hop — the retired CRUD tables
// (bugs/tasks/chains/task_blockers/benchmark_results/roadmap_items/
// suggestions) are gone, so tests seed projection rows directly
// (proj_current_bugs, proj_current_tasks, proj_chain_status, ...) or
// emit events through the production handlers.
func NewTestDB(t *testing.T) *db.Pool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolkit_test.db")
	p, err := db.Open(path)
	if err != nil {
		t.Fatalf("testutil.NewTestDB: open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// JSON marshals v into a json.RawMessage and fails the test on error.
// Convenience for constructing mock-response maps (the Mock* helpers
// take map[string]json.RawMessage so each entry is pre-marshaled).
//
// Generic over T so call sites pass concrete typed payloads; the `any`
// constraint is the encoding/json stdlib boundary (same shape as
// dispatch.Adapt[T any] / observehttp.writeJSON[T any]). Per the
// typed-returns reference doc, generic constraints are the right way
// to name this boundary.
func JSON[T any](t *testing.T, v T) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("testutil.JSON: %v", err)
	}
	return data
}

// MockLlamaCPP starts an httptest.Server that returns configurable JSON responses
// for llama.cpp-shaped requests. Keys are URL paths; values are pre-marshaled
// JSON bodies (use [JSON] to encode a typed Go value). The server is closed
// via t.Cleanup.
func MockLlamaCPP(t *testing.T, responses map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp, ok := responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			resp = json.RawMessage(`{"error":"no mock for ` + r.URL.Path + `"}`)
		}
		_, _ = w.Write(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// MockAnthropic starts an httptest.Server that returns configurable JSON responses
// for Anthropic API-shaped requests. Same shape as [MockLlamaCPP].
func MockAnthropic(t *testing.T, responses map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp, ok := responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			resp = json.RawMessage(`{"error":{"type":"not_found","message":"no mock for ` + r.URL.Path + `"}}`)
		}
		_, _ = w.Write(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
