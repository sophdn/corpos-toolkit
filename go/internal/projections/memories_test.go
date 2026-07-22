package projections_test

import (
	"testing"

	"toolkit/internal/testutil"
)

// TestCurrentMemories_RebuildFromEmpty seeds three MemoryWritten events
// directly into the events table and asserts a fresh rebuild produces
// three proj_memories rows with deterministic content, and that running
// the rebuild twice yields the same checksum (idempotency). Memories have
// no CRUD table — events ARE the source — so this exercises the only
// population path. Chain substrate-health-audit-projections T7.
func TestCurrentMemories_RebuildFromEmpty(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7a00-0001-7000-8000-000000000001', '2026-05-21 00:00:01', 'system', 'test', 'MemoryWritten', 'memory', 'mem-alpha', 'p1',
		 '{"name":"mem-alpha","kind":"feedback","description":"alpha description","vault_path":"/v/feedback/mem-alpha.md","body_length_bytes":120}', '019e7a00-0001-7000-8000-000000000001', 1),
		('019e7a00-0002-7000-8000-000000000002', '2026-05-21 00:00:02', 'system', 'test', 'MemoryWritten', 'memory', 'mem-beta', 'p1',
		 '{"name":"mem-beta","kind":"user","description":"beta description","vault_path":"/v/user/mem-beta.md","body_length_bytes":0}', '019e7a00-0002-7000-8000-000000000002', 1),
		('019e7a00-0003-7000-8000-000000000003', '2026-05-21 00:00:03', 'system', 'test', 'MemoryWritten', 'memory', 'mem-gamma', 'p1',
		 '{"name":"mem-gamma","kind":"project","description":"gamma description","vault_path":"/v/projects/mem-gamma.md","body_length_bytes":80}', '019e7a00-0003-7000-8000-000000000003', 1)`)

	mustExec(t, pool, `DELETE FROM proj_memories`)
	mustRebuild(t, pool, []string{"memories"})
	reference := tableChecksum(t, pool, "proj_memories")

	mustExec(t, pool, `DELETE FROM proj_memories`)
	mustRebuild(t, pool, []string{"memories"})
	after := tableChecksum(t, pool, "proj_memories")
	if reference != after {
		t.Fatalf("proj_memories checksum drift: reference=%s after=%s", reference, after)
	}

	if got := tableCount(t, pool, "proj_memories"); got != 3 {
		t.Errorf("proj_memories rows = %d, want 3", got)
	}
}

// TestCurrentMemories_LastWriteWins pins the global-namespace fold
// semantics: the same memory name re-filed (under a different project
// context, as happens for cross-project memories like linguistic-tics) is
// ONE row, not two. The most-recent write's fields win; filed_at preserves
// the FIRST write's ts; last_event_ts tracks the latest.
func TestCurrentMemories_LastWriteWins(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedProject(t, pool, "p2")
	// Two MemoryWritten events for the same name, ascending ts, different
	// project + body length. Replay order is ts-ascending.
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7a00-00aa-7000-8000-0000000000a1', '2026-05-21 00:00:01', 'system', 'test', 'MemoryWritten', 'memory', 'mem-dup', 'p1',
		 '{"name":"mem-dup","kind":"feedback","description":"first filing","vault_path":"/v/feedback/mem-dup.md","body_length_bytes":50}', '019e7a00-00aa-7000-8000-0000000000a1', 1),
		('019e7a00-00bb-7000-8000-0000000000b2', '2026-05-22 00:00:02', 'system', 'test', 'MemoryWritten', 'memory', 'mem-dup', 'p2',
		 '{"name":"mem-dup","kind":"feedback","description":"second filing reworded","vault_path":"/v/feedback/mem-dup.md","body_length_bytes":90}', '019e7a00-00bb-7000-8000-0000000000b2', 1)`)

	mustExec(t, pool, `DELETE FROM proj_memories`)
	mustRebuild(t, pool, []string{"memories"})

	if got := tableCount(t, pool, "proj_memories"); got != 1 {
		t.Fatalf("proj_memories rows = %d, want 1 (last-write-wins on name)", got)
	}

	var description, projectID, filedAt, lastEventTs string
	var bodyLen int
	if err := pool.DB().QueryRow(
		`SELECT description, project_id, body_length_bytes, filed_at, last_event_ts
		   FROM proj_memories WHERE name = 'mem-dup'`,
	).Scan(&description, &projectID, &bodyLen, &filedAt, &lastEventTs); err != nil {
		t.Fatalf("query mem-dup: %v", err)
	}
	if description != "second filing reworded" {
		t.Errorf("description = %q, want latest write %q", description, "second filing reworded")
	}
	if projectID != "p2" {
		t.Errorf("project_id = %q, want latest write %q", projectID, "p2")
	}
	if bodyLen != 90 {
		t.Errorf("body_length_bytes = %d, want latest write 90", bodyLen)
	}
	if filedAt != "2026-05-21 00:00:01" {
		t.Errorf("filed_at = %q, want FIRST write ts (preserved across re-writes)", filedAt)
	}
	if lastEventTs != "2026-05-22 00:00:02" {
		t.Errorf("last_event_ts = %q, want LATEST write ts", lastEventTs)
	}
}

// TestCurrentMemories_UnknownEventTypeIgnored confirms the fold tolerates a
// future memory-kind event type (MemoryEdited / MemoryDeleted) without
// erroring — it falls through to a no-op until a handler lands, per the T7
// constraint to accommodate future kinds without requiring them.
func TestCurrentMemories_UnknownEventTypeIgnored(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	mustExec(t, pool, `INSERT INTO events
		(event_id, ts, actor_kind, actor_id, type, entity_kind, entity_slug, entity_project_id, payload, span_id, schema_version)
		VALUES
		('019e7a00-00cc-7000-8000-0000000000c1', '2026-05-21 00:00:01', 'system', 'test', 'MemoryWritten', 'memory', 'mem-x', 'p1',
		 '{"name":"mem-x","kind":"user","description":"present","vault_path":"/v/user/mem-x.md","body_length_bytes":10}', '019e7a00-00cc-7000-8000-0000000000c1', 1),
		('019e7a00-00dd-7000-8000-0000000000d2', '2026-05-22 00:00:02', 'system', 'test', 'MemoryEdited', 'memory', 'mem-x', 'p1',
		 '{"name":"mem-x","kind":"user","description":"edited","vault_path":"/v/user/mem-x.md","body_length_bytes":20}', '019e7a00-00dd-7000-8000-0000000000d2', 1)`)

	mustExec(t, pool, `DELETE FROM proj_memories`)
	// Must not error on the unknown MemoryEdited type.
	mustRebuild(t, pool, []string{"memories"})

	if got := tableCount(t, pool, "proj_memories"); got != 1 {
		t.Fatalf("proj_memories rows = %d, want 1", got)
	}
	// The MemoryEdited is a no-op today, so the MemoryWritten state stands.
	var description string
	if err := pool.DB().QueryRow(
		`SELECT description FROM proj_memories WHERE name = 'mem-x'`,
	).Scan(&description); err != nil {
		t.Fatalf("query mem-x: %v", err)
	}
	if description != "present" {
		t.Errorf("description = %q, want %q (MemoryEdited is a no-op until handled)", description, "present")
	}
}
