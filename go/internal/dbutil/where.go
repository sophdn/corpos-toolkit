package dbutil

import (
	"strings"

	"toolkit/internal/db"
)

// WhereBuilder accumulates conditional WHERE-clause predicates and
// their bind parameters in lockstep. Predicate methods chain.
//
// The struct holds a private *db.Args so callers don't need to
// thread two parallel accumulators (conds + args) by hand.
type WhereBuilder struct {
	conds []string
	args  *db.Args
}

// NewWhereBuilder returns an empty builder.
func NewWhereBuilder() *WhereBuilder {
	return &WhereBuilder{args: db.NewArgs()}
}

// Eq appends `col = ?` when val is non-empty. Empty strings skip
// entirely — matching the silent-default convention these handlers
// already use.
func (w *WhereBuilder) Eq(col, val string) *WhereBuilder {
	if val == "" {
		return w
	}
	w.conds = append(w.conds, col+" = ?")
	w.args.AddString(val)
	return w
}

// Like appends `col LIKE ?` when pattern is non-empty. The caller is
// responsible for wrapping the value with wildcards (e.g.
// "%"+surface+"%") so the bind shape stays explicit at the call site.
func (w *WhereBuilder) Like(col, pattern string) *WhereBuilder {
	if pattern == "" {
		return w
	}
	w.conds = append(w.conds, col+" LIKE ?")
	w.args.AddString(pattern)
	return w
}

// GtEqString appends `col >= ?` when val is non-empty. Used for
// TEXT-typed timestamp columns (e.g. b.filed_at) where the bind is
// a string in ISO-8601 form and the comparison is lexicographic.
func (w *WhereBuilder) GtEqString(col, val string) *WhereBuilder {
	if val == "" {
		return w
	}
	w.conds = append(w.conds, col+" >= ?")
	w.args.AddString(val)
	return w
}

// GtEqInt64 appends `col >= ?` when hasIt is true. Used for INTEGER
// timestamp columns where the bind is a Unix-epoch int64; the
// explicit hasIt flag lets `0` be a meaningful value (epoch start).
func (w *WhereBuilder) GtEqInt64(col string, val int64, hasIt bool) *WhereBuilder {
	if !hasIt {
		return w
	}
	w.conds = append(w.conds, col+" >= ?")
	w.args.AddInt64(val)
	return w
}

// Clause returns the assembled `WHERE col1 = ? AND col2 = ? ...`
// fragment, or "" if no predicates were appended. No leading or
// trailing space — callers compose space around the placeholder
// themselves (matches the prior `fmt.Sprintf("...%s ORDER BY...",
// whereClause)` shape).
func (w *WhereBuilder) Clause() string {
	if len(w.conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(w.conds, " AND ")
}

// Args returns the underlying *db.Args for splatting into QueryContext
// / ExecContext via its Slice() method. Exposing *db.Args (not []any)
// keeps the untyped-variadic boundary concentrated in internal/db per
// the forbidigo any-rule.
//
// Usage:
//
//	rows, err := pool.DB().QueryContext(ctx, sql, wb.Args().Slice()...)
func (w *WhereBuilder) Args() *db.Args {
	return w.args
}
