// Package testutil provides the shared test infrastructure every Go
// handler test in this repo relies on — hermetic DB, HTTP test server
// harness, and fixture path discipline.
//
// ## Intended use
//
// **Workflow served:** every Go test in this repo needs a hermetic
// toolkit.db with every migration applied, an httptest.Server harness
// for HTTP-shaped tests, and the same fixture-path discipline so tests
// don't drift across packages. All Go tests in this repo require the
// `sqlite_fts5` build tag (run via `make -C go test` or
// `go test -tags sqlite_fts5 ./...`); a raw `go test ./...` produces
// "no such module: fts5" because several migrations create FTS5
// virtual tables.
//
// **Invocation pattern:** `db := testutil.OpenDB(t)` for a temp-dir
// hermetic database; `srv := testutil.HTTPTestServer(t, handler)` for
// an httptest.Server wired with the same middleware production uses;
// cleanup is automatic via `t.Cleanup`.
//
// **Success shape:** tests receive a fully-migrated `*db.Pool`, an
// addressable `*httptest.Server`, and embedded fixtures parsed into
// typed Go structs.
//
// **Non-goals:** not a fixture authoring tool, does not seed domain
// data (each test seeds what it needs), not production code — the
// `internal/testutil` import path means testutil exports are visible
// only to test binaries.
package testutil
