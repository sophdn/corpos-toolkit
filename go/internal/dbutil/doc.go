// Package dbutil hosts small SQL-construction helpers that compose with
// internal/db's Args accumulator.
//
// ## Intended use
//
// **Workflow served:** read-path handlers (work bug_list, observehttp
// bugs/tasks list endpoints) assemble conditional WHERE clauses with the
// same shape — N filters, each optional, joined with AND. Before this
// package each handler hand-rolled the `conds = append(...) +
// args.AddString(...)` pair per filter; WhereBuilder concentrates the
// pattern and keeps the bind order trivially correct.
//
// **Invocation pattern:** chain typed predicate methods, then read the
// SQL fragment + binds at query time:
//
//	wb := dbutil.NewWhereBuilder().
//	    Eq("b.project_id", project).
//	    Eq("b.status", status).
//	    Like("b.surface", "%"+surface+"%").
//	    GtEqString("b.filed_at", since)
//	rows, _ := pool.DB().QueryContext(ctx, prefix+" "+wb.Clause()+" "+suffix, wb.Args().Slice()...)
//
// **Success shape:** Clause returns "" when no filters were appended,
// otherwise `WHERE col1 = ? AND col2 = ? ...` (no leading/trailing
// space — matches the prior strings.Join shape). Args returns the
// underlying *db.Args in the order the predicates were added; call
// `.Slice()` on it to splat into QueryContext / ExecContext.
//
// **Non-goals:** not a general SQL builder (no OR, no sub-clauses, no
// JOIN — three call sites need none of those, and adding them would
// invite over-fitting); does not own the LIMIT / ORDER BY suffix
// (callers compose those inline so the optimisable column order
// stays visible); does not replace db.NewArgs — it composes with the
// same untyped-boundary discipline by holding its own private *db.Args.
package dbutil
