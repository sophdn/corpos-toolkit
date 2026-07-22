package fs

// artifact_events_test.go is the integration net for the OPT-IN provenance
// stamping on fs.write / fs.edit. It pins two invariants: the default mutation
// emits nothing (byte-parity), and record mode emits an artifact event that
// fs.read provenance mode then surfaces — the full write->read loop.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/testutil"
)

func writeViaTable(t *testing.T, tbl dispatch.Table, params WriteParams) WriteResult {
	t.Helper()
	res, err := tbl["write"](context.Background(), "", mustJSON(t, params))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	wr, ok := res.(WriteResult)
	if !ok {
		t.Fatalf("write result type = %T", res)
	}
	return wr
}

func TestWriteRecord_DefaultEmitsNothing(t *testing.T) {
	pool := testutil.NewTestDB(t)
	tbl := BuildTable(Deps{Pool: pool})
	dir := t.TempDir()
	res := writeViaTable(t, tbl, WriteParams{FilePath: filepath.Join(dir, "f.txt"), Content: "hi\n"})
	if res.Event != nil {
		t.Fatalf("default write must not emit an artifact event, got %+v", res.Event)
	}
	if n := countEvents(t, pool, "ArtifactWritten"); n != 0 {
		t.Errorf("expected 0 ArtifactWritten events, got %d", n)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), `"event"`) {
		t.Errorf("default write JSON leaked event key: %s", b)
	}
}

func TestWriteRecord_EmitsAndProvenanceSurfaces(t *testing.T) {
	pool := testutil.NewTestDB(t)
	tbl := BuildTable(Deps{Pool: pool})
	dir := t.TempDir()
	path := filepath.Join(dir, "tracked.go")

	res := writeViaTable(t, tbl, WriteParams{
		FilePath: path,
		Content:  "package p\n",
		Record:   true,
		Intent:   "seed the tracked file deliberately",
	})
	if res.Event == nil || res.Event.Type != "ArtifactWritten" || res.Event.EventID == "" {
		t.Fatalf("record write should attach an ArtifactWritten receipt, got %+v", res.Event)
	}
	if !res.Created {
		t.Errorf("expected created=true")
	}
	if n := countEvents(t, pool, "ArtifactWritten"); n != 1 {
		t.Fatalf("expected exactly 1 ArtifactWritten event, got %d", n)
	}

	// The write half of the loop is now visible to the read half: provenance
	// mode on the same path surfaces the event with its intent.
	prov, err := handleReadMode(context.Background(), Deps{Pool: pool}, mustJSON(t, ReadParams{FilePath: path, Provenance: true}))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if prov.Provenance == nil || len(prov.Provenance.Events) != 1 {
		t.Fatalf("provenance should surface the artifact event, got %+v", prov.Provenance)
	}
	e := prov.Provenance.Events[0]
	if e.Type != "ArtifactWritten" || e.Rationale != "seed the tracked file deliberately" {
		t.Errorf("surfaced event mismatch: %+v", e)
	}
}

func TestWriteRecord_NoPoolIsFailOpen(t *testing.T) {
	// record=true with no pool must still succeed (fail-open), just without a receipt.
	tbl := BuildTable(Deps{})
	dir := t.TempDir()
	res := writeViaTable(t, tbl, WriteParams{FilePath: filepath.Join(dir, "f.txt"), Content: "x\n", Record: true, Intent: "nope"})
	if res.Event != nil {
		t.Errorf("no-pool record write should attach no receipt, got %+v", res.Event)
	}
}

func TestEditRecord_DefaultEmitsNothing(t *testing.T) {
	pool := testutil.NewTestDB(t)
	tbl := BuildTable(Deps{Pool: pool, Reads: NewReadRegistry()})
	path := writeTemp(t, "f.txt", "alpha beta\n")
	tbl["read"](context.Background(), "", mustJSON(t, ReadParams{FilePath: path})) // satisfy the edit precondition

	res, err := tbl["edit"](context.Background(), "", mustJSON(t, EditParams{FilePath: path, OldString: "beta", NewString: "BETA"}))
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if er := res.(EditResult); er.Event != nil {
		t.Fatalf("default edit must not emit an artifact event, got %+v", er.Event)
	}
	if n := countEvents(t, pool, "ArtifactEdited"); n != 0 {
		t.Errorf("expected 0 ArtifactEdited events, got %d", n)
	}
}

func TestEditRecord_EmitsAndProvenanceSurfaces(t *testing.T) {
	pool := testutil.NewTestDB(t)
	reg := NewReadRegistry()
	tbl := BuildTable(Deps{Pool: pool, Reads: reg})
	path := writeTemp(t, "f.txt", "alpha beta gamma\n")
	tbl["read"](context.Background(), "", mustJSON(t, ReadParams{FilePath: path}))

	res, err := tbl["edit"](context.Background(), "", mustJSON(t, EditParams{
		FilePath: path, OldString: "beta", NewString: "BETA",
		Record: true, Intent: "rename the middle token",
	}))
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	er := res.(EditResult)
	if er.Event == nil || er.Event.Type != "ArtifactEdited" || er.Event.EventID == "" {
		t.Fatalf("record edit should attach an ArtifactEdited receipt, got %+v", er.Event)
	}
	if n := countEvents(t, pool, "ArtifactEdited"); n != 1 {
		t.Fatalf("expected 1 ArtifactEdited event, got %d", n)
	}

	prov, err := handleReadMode(context.Background(), Deps{Pool: pool}, mustJSON(t, ReadParams{FilePath: path, Provenance: true}))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if prov.Provenance == nil || len(prov.Provenance.Events) != 1 {
		t.Fatalf("provenance should surface the edit event, got %+v", prov.Provenance)
	}
	if e := prov.Provenance.Events[0]; e.Type != "ArtifactEdited" || e.Rationale != "rename the middle token" {
		t.Errorf("surfaced event mismatch: %+v", e)
	}
}

func countEvents(t *testing.T, pool *db.Pool, typ string) int {
	t.Helper()
	var n int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE type = ?`, typ).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func moveViaTable(t *testing.T, tbl dispatch.Table, params MoveParams) MoveResult {
	t.Helper()
	res, err := tbl["move"](context.Background(), "", mustJSON(t, params))
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	mr, ok := res.(MoveResult)
	if !ok {
		t.Fatalf("move result type = %T", res)
	}
	return mr
}

func TestMoveRecord_DefaultEmitsNothing(t *testing.T) {
	pool := testutil.NewTestDB(t)
	tbl := BuildTable(Deps{Pool: pool})
	dir := t.TempDir()
	src := writeTemp(t, "a.txt", "x\n")
	res := moveViaTable(t, tbl, MoveParams{Source: src, Dest: filepath.Join(dir, "b.txt")})
	if res.Event != nil {
		t.Fatalf("default move must not emit, got %+v", res.Event)
	}
	if n := countEvents(t, pool, "ArtifactMoved"); n != 0 {
		t.Errorf("expected 0 ArtifactMoved events, got %d", n)
	}
}

func TestMoveRecord_EmitsAndProvenanceSurfaces(t *testing.T) {
	pool := testutil.NewTestDB(t)
	tbl := BuildTable(Deps{Pool: pool})
	src := writeTemp(t, "from.go", "package p\n")
	dst := filepath.Join(filepath.Dir(src), "to.go")

	res := moveViaTable(t, tbl, MoveParams{Source: src, Dest: dst, Record: true, Intent: "relocate the tracked file deliberately"})
	if res.Event == nil || res.Event.Type != "ArtifactMoved" || res.Event.EventID == "" {
		t.Fatalf("record move should attach an ArtifactMoved receipt, got %+v", res.Event)
	}
	if n := countEvents(t, pool, "ArtifactMoved"); n != 1 {
		t.Fatalf("expected exactly 1 ArtifactMoved event, got %d", n)
	}

	// The relocation is visible to a reader of the destination: provenance mode
	// on the dest path surfaces the move event with its intent.
	prov, err := handleReadMode(context.Background(), Deps{Pool: pool}, mustJSON(t, ReadParams{FilePath: dst, Provenance: true}))
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if prov.Provenance == nil || len(prov.Provenance.Events) != 1 {
		t.Fatalf("provenance should surface the move event, got %+v", prov.Provenance)
	}
	if e := prov.Provenance.Events[0]; e.Type != "ArtifactMoved" || e.Rationale != "relocate the tracked file deliberately" {
		t.Errorf("surfaced event mismatch: %+v", e)
	}
}

func TestRemoveRecord_DefaultEmitsNothingAndRecordEmits(t *testing.T) {
	pool := testutil.NewTestDB(t)
	tbl := BuildTable(Deps{Pool: pool})

	// Default remove: no event.
	p1 := writeTemp(t, "gone.txt", "x")
	res1, err := tbl["remove"](context.Background(), "", mustJSON(t, RemoveParams{FilePath: p1}))
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if res1.(RemoveResult).Event != nil {
		t.Fatalf("default remove must not emit, got %+v", res1.(RemoveResult).Event)
	}
	if n := countEvents(t, pool, "ArtifactRemoved"); n != 0 {
		t.Errorf("expected 0 ArtifactRemoved events, got %d", n)
	}

	// Record remove: one ArtifactRemoved event with the intent as rationale.
	p2 := writeTemp(t, "gone2.txt", "y")
	res2, err := tbl["remove"](context.Background(), "", mustJSON(t, RemoveParams{FilePath: p2, Record: true, Intent: "delete the stale tracked file"}))
	if err != nil {
		t.Fatalf("record remove: %v", err)
	}
	ev := res2.(RemoveResult).Event
	if ev == nil || ev.Type != "ArtifactRemoved" || ev.EventID == "" {
		t.Fatalf("record remove should attach an ArtifactRemoved receipt, got %+v", ev)
	}
	if n := countEvents(t, pool, "ArtifactRemoved"); n != 1 {
		t.Errorf("expected exactly 1 ArtifactRemoved event, got %d", n)
	}
}
