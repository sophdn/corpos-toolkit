// Package db provides the SQLite connection pool, migration runner,
// and a typed accumulator for heterogeneous query arguments.
//
// ## Intended use
//
// **Workflow served:** every Go handler that touches toolkit.db needs a
// migrated `*Pool` plus a way to build the `args ...any` lists that
// `database/sql` requires; this package owns the pool open path, the
// embedded-migration runner, FTS5 missing-module detection, and the
// `Args` accumulator that boundary-isolates the stdlib's untyped variadic.
//
// **Invocation pattern:** `pool, err := db.Open(path)` once at startup;
// `pool.WithRead(ctx, fn)` / `pool.WithWrite(ctx, fn)` scope a read /
// write transaction with serialized write access; build parameter lists
// via `args := db.NewArgs(); args.AddString(x); args.AddInt64(y)` then
// splat with `args.Slice()...` into QueryContext / ExecContext.
//
// **Success shape:** Open returns `*Pool` after applying every pending
// migration; in-transaction callbacks see `*sql.Tx`; helpers return
// well-typed Go structs and propagate sqlite errors wrapped with
// per-call context.
//
// **Non-goals:** does not implement domain logic, does not export the
// raw `*sql.DB` (callers must use WithRead/WithWrite to participate in
// pool discipline). Migration content lives in `migrations/` (canonical
// post-T6) with a real-copy mirror under
// go/internal/testutil/migrations/ for the Go embed.
package db
