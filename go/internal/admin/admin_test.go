package admin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
	"toolkit/internal/testutil"
)

func mkDeps(t *testing.T) Deps {
	t.Helper()
	pool := testutil.NewTestDB(t)
	return Deps{
		Pool:        pool,
		StartedAt:   time.Now().Add(-5 * time.Second),
		GitSHA:      "test-sha",
		BuiltAtUnix: 1700000000,
		PackageVer:  "v0.0.0-test",
	}
}

func TestProjectRegister_Roundtrip(t *testing.T) {
	d := mkDeps(t)
	got, err := d.projectRegister(context.Background(),
		json.RawMessage(`{"id":"p1","name":"Project One","path":"/tmp/p1"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got.ProjectID != "p1" {
		t.Errorf("project_id = %v", got.ProjectID)
	}

	rows, err := d.projectList(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "p1" || rows[0].Path != "/tmp/p1" {
		t.Errorf("list returned %+v", rows)
	}
}

func TestProjectRegister_UpsertIdempotent(t *testing.T) {
	d := mkDeps(t)
	for i := 0; i < 2; i++ {
		if _, err := d.projectRegister(context.Background(),
			json.RawMessage(`{"id":"p1","name":"V1","path":"/tmp"}`)); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	// Second register with different name updates the row, not a new row.
	if _, err := d.projectRegister(context.Background(),
		json.RawMessage(`{"id":"p1","name":"V2","path":"/tmp"}`)); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	rows := mustListProjects(t, d)
	if len(rows) != 1 || rows[0].Name != "V2" {
		t.Errorf("upsert wrong: %+v", rows)
	}
}

func TestProjectRegister_MissingFields(t *testing.T) {
	d := mkDeps(t)
	_, err := d.projectRegister(context.Background(), json.RawMessage(`{"id":"only-id"}`))
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("missing-name should error: %v", err)
	}
}

func TestHostRegister_Roundtrip(t *testing.T) {
	d := mkDeps(t)
	_, err := d.hostRegister(context.Background(),
		json.RawMessage(`{"id":"h1","hostname":"10.0.0.1","ssh_user":"sophi","ssh_port":2222,"passwordless_sudo":true}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	hosts, err := d.hostList(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1", len(hosts))
	}
	if hosts[0].Slug != "h1" || hosts[0].SSHPort != 2222 || !hosts[0].PasswordlessSudo {
		t.Errorf("host row wrong: %+v", hosts[0])
	}
}

func TestHostList_HidesRetiredByDefault(t *testing.T) {
	d := mkDeps(t)
	_, err := d.hostRegister(context.Background(),
		json.RawMessage(`{"id":"h1","hostname":"a","ssh_user":"u"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.hostRemove(context.Background(),
		json.RawMessage(`{"id":"h1"}`)); err != nil {
		t.Fatal(err)
	}
	// Default list: empty.
	hosts := mustListHosts(t, d, "")
	if len(hosts) != 0 {
		t.Errorf("retired host leaked: %+v", hosts)
	}
	// With include_retired: visible.
	hosts = mustListHosts(t, d, `{"include_retired":true}`)
	if len(hosts) != 1 || hosts[0].RetiredAt == nil {
		t.Errorf("include_retired wrong: %+v", hosts)
	}
}

func TestHostRemove_UnknownReturnsError(t *testing.T) {
	d := mkDeps(t)
	_, err := d.hostRemove(context.Background(), json.RawMessage(`{"id":"ghost"}`))
	if err == nil || !strings.Contains(err.Error(), "host_not_found") {
		t.Errorf("missing-host should error: %v", err)
	}
}

func TestServerHealth_ReportsDBOKAndUptime(t *testing.T) {
	d := mkDeps(t)
	resp, err := d.serverHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !resp.DBOK {
		t.Errorf("db_ok = %v, want true", resp.DBOK)
	}
	if resp.Server != "toolkit-server-go" {
		t.Errorf("server = %v", resp.Server)
	}
	if resp.UptimeSeconds < 1 {
		t.Errorf("uptime_seconds = %d, want >= 1", resp.UptimeSeconds)
	}
}

func TestSchemaReload_ReportsDelta(t *testing.T) {
	dir := t.TempDir()
	must(t, writeFile(dir+"/a.toml", `supported_ops = ["create"]
[schema]
name = "a"
prefix = "A"
output_dir = "out"
filename_pattern = "{prefix}_{slug}_{date}.md"

[storage]
target = "db"
table = "t_a"
key_columns = ["slug"]

[[fields]]
name = "slug"
type = "string"
description = "x"
`))
	reg, err := registry.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := mkDeps(t)
	d.Schemas = reg

	// First reload: registry already has "a"; expect no delta.
	resp, err := d.schemaReload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.SchemaCount != 1 {
		t.Errorf("initial schema_count = %v, want 1", resp.SchemaCount)
	}

	// Add a schema on disk, reload, expect "b" in added.
	must(t, writeFile(dir+"/b.toml", `supported_ops = ["create"]
[schema]
name = "b"
prefix = "B"
output_dir = "out"
filename_pattern = "{prefix}_{slug}_{date}.md"

[storage]
target = "db"
table = "t_b"
key_columns = ["slug"]

[[fields]]
name = "slug"
type = "string"
description = "x"
`))
	resp, err = d.schemaReload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Added) != 1 || resp.Added[0] != "b" {
		t.Errorf("added = %v, want [b]", resp.Added)
	}
	if resp.SchemaCount != 2 {
		t.Errorf("after-add schema_count = %v, want 2", resp.SchemaCount)
	}
}

func TestVaultSearchMetrics_EmptyReturnsZeroes(t *testing.T) {
	d := mkDeps(t)
	resp, err := d.vaultSearchMetrics(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalCalls != 0 || resp.P50LatencyMs != nil {
		t.Errorf("empty metrics wrong: %+v", resp)
	}
}

func TestVaultSearchMetrics_ComputesPercentiles(t *testing.T) {
	d := mkDeps(t)
	// Post chain telemetry-substrate-cleanup T2 (migration 046), the
	// vault-search metrics handler reads from grounding_events with
	// latency = pass1_latency_ms + COALESCE(pass2_latency_ms, 0).
	// Stamp ten distinct call_ids so the unique (session_id, call_id)
	// index doesn't collapse the inserts into one.
	for i, ms := int64(0), int64(100); ms <= 1000; i, ms = i+1, ms+100 {
		if _, err := d.Pool.DB().Exec(
			`INSERT INTO grounding_events
				(project_id, session_id, call_id, action, results_count,
				 query_text, pass1_latency_ms)
			 VALUES ('p', 'sess', ?, 'vault_search', 3, 'q', ?)`,
			i, ms); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := d.vaultSearchMetrics(context.Background(),
		json.RawMessage(`{"recent_n":10}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalCalls != 10 {
		t.Errorf("total = %d, want 10", resp.TotalCalls)
	}
	if resp.P50LatencyMs == nil || *resp.P50LatencyMs != 500 {
		t.Errorf("p50 = %v, want 500", resp.P50LatencyMs)
	}
	if resp.P95LatencyMs == nil || *resp.P95LatencyMs != 1000 {
		t.Errorf("p95 = %v, want 1000", resp.P95LatencyMs)
	}
}

func TestPercentile_NearestRank(t *testing.T) {
	cases := []struct {
		sorted []int64
		pct    int
		want   int64
	}{
		{[]int64{}, 50, 0},
		{[]int64{42}, 50, 42},
		{[]int64{1, 2, 3, 4, 5}, 50, 3},
		{[]int64{1, 2, 3, 4, 5}, 95, 5},
		{[]int64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000}, 95, 1000},
	}
	for _, c := range cases {
		if got := percentile(c.sorted, c.pct); got != c.want {
			t.Errorf("percentile(%v, %d) = %d, want %d", c.sorted, c.pct, got, c.want)
		}
	}
}

func TestSchemaVersion_NoMigrationsErrors(t *testing.T) {
	pool, err := db.Open(t.TempDir() + "/empty.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	// db.Open populates _migrations via the embedded runner (bug 1326). To
	// exercise the empty-bookkeeping error path the handler is responsible
	// for, clear the table after Open and before invoking schemaVersion.
	if _, err := pool.DB().Exec(`DELETE FROM _migrations`); err != nil {
		t.Fatal(err)
	}
	d := Deps{Pool: pool, StartedAt: time.Now()}
	_, err = d.schemaVersion(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no migrations") {
		t.Errorf("expected 'no migrations' error, got %v", err)
	}
}

func TestRecipeStubs_ReturnDeferredEnvelope(t *testing.T) {
	stub := deferredRecipeStub("apply_recipe")
	if stub.Error != "action_deferred" || stub.Action != "apply_recipe" {
		t.Errorf("stub shape wrong: %+v", stub)
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func mustListProjects(t *testing.T, d Deps) []projectRow {
	t.Helper()
	out, err := d.projectList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustListHosts(t *testing.T, d Deps, params string) []hostRow {
	t.Helper()
	var raw json.RawMessage
	if params != "" {
		raw = json.RawMessage(params)
	}
	out, err := d.hostList(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
