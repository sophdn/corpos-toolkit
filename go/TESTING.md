# Testing the Go toolkit-server

## IMPORTANT — the `sqlite_fts5` build tag is required

All Go tests in this repo require the `sqlite_fts5` build tag. Several
migrations (`020_knowledge_pointers.sql`, etc.) create FTS5 virtual
tables, and the FTS5 module is compiled into the `mattn/go-sqlite3`
driver only when this tag is present. A raw `go test ./...` fails with
the cryptic SQLite-layer error:

    apply 020_knowledge_pointers.sql: no such module: fts5

If you see this, you forgot the tag — the SQLite driver is otherwise
healthy.

## Canonical invocations

Use the Makefile when possible:

```
make -C go test          # all package tests (passes -tags sqlite_fts5)
make -C go vet           # vet  (also passes the tag)
make -C go smoke         # smoke harness against the built binary
make -C go build-all     # compile every package (not just cmd/toolkit-server)
```

Or invoke `go test` directly with the tag:

```
go test -tags sqlite_fts5 ./...
go test -tags sqlite_fts5 -run 'TestVaultSearch_' ./internal/knowledge/
```

The `testutil.NewTestDB` helper detects the missing-module error and
replaces it with a hint that names this file — see
`internal/testutil/testutil.go:NewTestDB` (bug 1323's fix path).

## Package layout

- `cmd/toolkit-server/` — the binary entry-point. `make build` outputs
  to `bin/toolkit-server`.
- `internal/` — production handlers and infrastructure (per Go's
  `internal/` rule, importable only by code rooted at `go/`).
- `smoketest/` — out-of-tree smoke harness that exercises the built
  binary's MCP surface. Pulled in via `make -C go smoke`.

## Mocks

`testutil.MockLlamaCPP` and `testutil.MockAnthropic` start httptest
servers with table-driven response maps. See `testutil_test.go` for
examples. Tests must never reach external HTTP services.
