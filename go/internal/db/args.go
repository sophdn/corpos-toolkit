package db

// Args is the codebase's typed accumulator for conditional SQL arguments.
//
// `database/sql.QueryContext` and `ExecContext` accept `args ...any` because
// SQL parameter lists are inherently heterogeneous (string + int64 + nil +
// *string in a single row). That `any` boundary is structural — it cannot be
// removed from the stdlib signature.
//
// Args concentrates that boundary into this one type. Callers add typed
// values via per-type methods; the internal []any slice is the driver
// hand-off and never escapes this package's API in untyped form. Use
// [Args.Slice] to splat into a QueryContext / ExecContext variadic call.
//
// The forbidigo lint rule excludes internal/db precisely because of this
// type — Args is the *only* legitimate place in the codebase where the
// SQL driver's untyped variadic touches our code.
type Args struct {
	vals []any
}

// NewArgs returns an empty accumulator.
func NewArgs() *Args { return &Args{} }

// AddString appends a string-valued SQL parameter.
func (a *Args) AddString(s string) *Args { a.vals = append(a.vals, s); return a }

// AddInt64 appends an int64-valued SQL parameter.
func (a *Args) AddInt64(n int64) *Args { a.vals = append(a.vals, n); return a }

// AddBool appends a bool-valued SQL parameter.
func (a *Args) AddBool(b bool) *Args { a.vals = append(a.vals, b); return a }

// AddFloat appends a float64-valued SQL parameter.
func (a *Args) AddFloat(f float64) *Args { a.vals = append(a.vals, f); return a }

// AddNullableString appends a *string. A nil pointer becomes SQL NULL
// (database/sql dereferences non-nil pointers; nil pointers serialise as NULL).
func (a *Args) AddNullableString(s *string) *Args { a.vals = append(a.vals, s); return a }

// AddNullableInt appends a *int64; nil becomes NULL.
func (a *Args) AddNullableInt(n *int64) *Args { a.vals = append(a.vals, n); return a }

// AddNullableFloat appends a *float64; nil becomes NULL.
func (a *Args) AddNullableFloat(f *float64) *Args { a.vals = append(a.vals, f); return a }

// Len returns the number of accumulated arguments.
func (a *Args) Len() int { return len(a.vals) }

// Slice returns the accumulated args for splatting into QueryContext /
// ExecContext. The returned slice shares storage with the builder; callers
// should not mutate it.
//
// Usage:
//
//	args := db.NewArgs().AddString(project).AddInt64(since)
//	rows, err := pool.DB().QueryContext(ctx, sql, args.Slice()...)
func (a *Args) Slice() []any { return a.vals }

// Scanner is the minimum interface satisfied by both *sql.Row and *sql.Rows.
// Declared here so callers that abstract over single-row vs multi-row scans
// can name `db.Scanner` instead of inlining `interface { Scan(...any) error }`
// — the `any` in the stdlib Scan signature is unavoidable, so concentrating
// the boundary in this package keeps the forbidigo exemption in one place.
type Scanner interface {
	Scan(dest ...any) error
}
