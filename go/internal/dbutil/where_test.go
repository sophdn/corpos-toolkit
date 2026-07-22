package dbutil

import (
	"reflect"
	"testing"
)

func TestWhereBuilder_Empty(t *testing.T) {
	wb := NewWhereBuilder()
	if got := wb.Clause(); got != "" {
		t.Errorf("empty clause: got %q, want \"\"", got)
	}
	if got := wb.Args().Slice(); len(got) != 0 {
		t.Errorf("empty args: got %v, want empty slice", got)
	}
}

func TestWhereBuilder_EqSkipsEmpty(t *testing.T) {
	wb := NewWhereBuilder().Eq("b.status", "")
	if got := wb.Clause(); got != "" {
		t.Errorf("Eq with empty val should skip; got %q", got)
	}
	if len(wb.Args().Slice()) != 0 {
		t.Errorf("Eq with empty val should not bind; got args %v", wb.Args().Slice())
	}
}

func TestWhereBuilder_SingleEq(t *testing.T) {
	wb := NewWhereBuilder().Eq("b.status", "open")
	if got, want := wb.Clause(), "WHERE b.status = ?"; got != want {
		t.Errorf("clause: got %q, want %q", got, want)
	}
	if got, want := wb.Args().Slice(), []any{"open"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args: got %v, want %v", got, want)
	}
}

func TestWhereBuilder_MultiFilterAndJoin(t *testing.T) {
	wb := NewWhereBuilder().
		Eq("b.project_id", "mcp-servers").
		Eq("b.status", "open").
		Eq("b.severity", "high").
		Like("b.surface", "%vault%").
		GtEqString("b.filed_at", "2026-05-16")
	want := "WHERE b.project_id = ? AND b.status = ? AND b.severity = ? AND b.surface LIKE ? AND b.filed_at >= ?"
	if got := wb.Clause(); got != want {
		t.Errorf("clause: got %q, want %q", got, want)
	}
	wantArgs := []any{"mcp-servers", "open", "high", "%vault%", "2026-05-16"}
	if got := wb.Args().Slice(); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("args: got %v, want %v", got, wantArgs)
	}
}

func TestWhereBuilder_GtEqInt64HasItFlag(t *testing.T) {
	skipped := NewWhereBuilder().GtEqInt64("run_at", 12345, false)
	if got := skipped.Clause(); got != "" {
		t.Errorf("hasIt=false should skip; got %q", got)
	}

	taken := NewWhereBuilder().GtEqInt64("run_at", 0, true)
	if got, want := taken.Clause(), "WHERE run_at >= ?"; got != want {
		t.Errorf("hasIt=true with val=0 should still bind; got %q want %q", got, want)
	}
	if got, want := taken.Args().Slice(), []any{int64(0)}; !reflect.DeepEqual(got, want) {
		t.Errorf("args: got %v, want %v", got, want)
	}
}

func TestWhereBuilder_LikeSkipsEmptyPattern(t *testing.T) {
	wb := NewWhereBuilder().Like("b.surface", "")
	if got := wb.Clause(); got != "" {
		t.Errorf("empty Like pattern should skip; got %q", got)
	}
}

// TestWhereBuilder_BugListSnapshot pins the byte-exact WHERE clause + bind
// slice that the work.HandleBugList migration produces for the canonical
// status+severity+since filter set. If this changes, the migration is no
// longer behaviour-preserving relative to the pre-WhereBuilder code path.
func TestWhereBuilder_BugListSnapshot(t *testing.T) {
	wb := NewWhereBuilder().
		Eq("b.project_id", "mcp-servers").
		Eq("b.status", "open").
		Eq("b.severity", "high").
		Like("b.surface", "").
		GtEqString("b.filed_at", "1700000000")
	const want = "WHERE b.project_id = ? AND b.status = ? AND b.severity = ? AND b.filed_at >= ?"
	if got := wb.Clause(); got != want {
		t.Errorf("bug_list snapshot clause:\n  got:  %q\n  want: %q", got, want)
	}
	wantArgs := []any{"mcp-servers", "open", "high", "1700000000"}
	if got := wb.Args().Slice(); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("bug_list snapshot args:\n  got:  %v\n  want: %v", got, wantArgs)
	}
}

func TestWhereBuilder_BindOrderMatchesAppendOrder(t *testing.T) {
	wb := NewWhereBuilder().
		GtEqString("b.filed_at", "2026-05-01").
		Eq("b.status", "open").
		Like("b.surface", "%vault%")
	want := "WHERE b.filed_at >= ? AND b.status = ? AND b.surface LIKE ?"
	if got := wb.Clause(); got != want {
		t.Errorf("clause: got %q, want %q", got, want)
	}
	if got, want := wb.Args().Slice(), []any{"2026-05-01", "open", "%vault%"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args: got %v, want %v", got, want)
	}
}
