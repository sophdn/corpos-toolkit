package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"time"

	"toolkit/internal/forge/registry"
	"toolkit/internal/inference/llamacpp"
)

// HealthResult is the response shape for the health / server_health action.
type HealthResult struct {
	Status            string  `json:"status"`
	Timestamp         string  `json:"timestamp"`
	SchemaHead        *string `json:"schema_head"`
	OK                bool    `json:"ok"`
	DBOK              bool    `json:"db_ok"`
	LlamaCPPReachable bool    `json:"llamacpp_reachable"`
	UptimeSeconds     int64   `json:"uptime_seconds"`
	Server            string  `json:"server"`
	Version           string  `json:"version"`
}

// SchemaVersionResult is the response shape for schema_version: the head
// row of _migrations.
type SchemaVersionResult struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	RunAt string `json:"run_at"`
}

// SchemaReloadResult is the response shape for schema_reload.
type SchemaReloadResult struct {
	OK          bool          `json:"ok"`
	SchemaCount int           `json:"schema_count"`
	Added       []string      `json:"added"`
	Removed     []string      `json:"removed"`
	ParseErrors []ParseErrRow `json:"parse_errors"`
}

// ParseErrRow is one parse-error entry inside a schema_reload response.
type ParseErrRow struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// ServerVersionResult is the response shape for server_version.
type ServerVersionResult struct {
	GitSHA         string `json:"git_sha"`
	BuiltAtUnix    int64  `json:"built_at_unix"`
	PackageVersion string `json:"package_version"`
}

// serverHealth mirrors crates/toolkit-server/src/dispatch/admin.rs's
// server_health AND extends the response with the additional fields
// T61 named (db_ok / llamacpp_reachable / uptime_seconds / server /
// version). The Rust fields stay so any existing reader keeps working.
func (d Deps) serverHealth(ctx context.Context) (HealthResult, error) {
	// status / db_ok come from a trivial SELECT 1.
	var one int64
	dbErr := d.Pool.DB().QueryRowContext(ctx, `SELECT 1`).Scan(&one)
	dbOK := dbErr == nil

	// schema_head from _migrations (matches Rust shape).
	var head *string
	var name string
	if err := d.Pool.DB().QueryRowContext(ctx,
		`SELECT name FROM _migrations ORDER BY id DESC LIMIT 1`).Scan(&name); err == nil {
		head = &name
	}

	// llamacpp_reachable: 1-second HEAD/GET to the configured base URL.
	llamaOK := llamaReachable(ctx)

	status := "ok"
	if !dbOK {
		status = "degraded"
	}

	return HealthResult{
		Status:            status,
		Timestamp:         time.Now().UTC().Format(time.RFC3339Nano),
		SchemaHead:        head,
		OK:                dbOK,
		DBOK:              dbOK,
		LlamaCPPReachable: llamaOK,
		UptimeSeconds:     int64(time.Since(d.StartedAt).Seconds()),
		Server:            "toolkit-server-go",
		Version:           d.PackageVer,
	}, nil
}

func llamaReachable(ctx context.Context) bool {
	// One source of truth with the inference router + the -llama-url flag, so the
	// reachability signal probes the SAME URL inference actually dispatches to.
	base := llamacpp.BaseURLFromEnv()
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// schemaVersion mirrors the Rust schema_version dispatch action —
// returns the migration head row (id, name, run_at) or an error
// envelope when _migrations is empty.
func (d Deps) schemaVersion(ctx context.Context) (SchemaVersionResult, error) {
	var out SchemaVersionResult
	err := d.Pool.DB().QueryRowContext(ctx,
		`SELECT id, name, run_at FROM _migrations ORDER BY id DESC LIMIT 1`,
	).Scan(&out.ID, &out.Name, &out.RunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SchemaVersionResult{}, errors.New("no migrations applied")
	}
	if err != nil {
		return SchemaVersionResult{}, err
	}
	return out, nil
}

// schemaReload mirrors ToolkitServer.reload_schemas in
// crates/toolkit-server/src/lib.rs: re-scans the registry's source
// dirs, atomically replaces the in-memory registry, and emits a
// summary including added/removed names + parse errors.
func (d Deps) schemaReload(ctx context.Context) (SchemaReloadResult, error) {
	if d.Schemas == nil {
		return SchemaReloadResult{}, errors.New("schema registry not configured")
	}
	beforeNames := sortedNames(d.Schemas)
	if err := d.Schemas.Reload(); err != nil {
		return SchemaReloadResult{}, err
	}
	afterNames := sortedNames(d.Schemas)
	parseErrors := []ParseErrRow{}
	for _, e := range d.Schemas.ParseErrors() {
		parseErrors = append(parseErrors, ParseErrRow{File: e.SourceFile, Error: e.Err})
	}
	return SchemaReloadResult{
		OK:          true,
		SchemaCount: d.Schemas.Len(),
		Added:       diffSorted(afterNames, beforeNames),
		Removed:     diffSorted(beforeNames, afterNames),
		ParseErrors: parseErrors,
	}, nil
}

// serverVersion returns the build metadata for the running binary.
func (d Deps) serverVersion(_ context.Context) (ServerVersionResult, error) {
	return ServerVersionResult{
		GitSHA:         d.GitSHA,
		BuiltAtUnix:    d.BuiltAtUnix,
		PackageVersion: d.PackageVer,
	}, nil
}

func sortedNames(r *registry.Registry) []string {
	out := append([]string(nil), r.Names()...)
	sort.Strings(out)
	return out
}

// diffSorted returns elements in `a` not in `b`. Output preserves a's
// ordering. Both arguments don't actually need to be sorted; the name
// is historical from the Rust BTreeSet difference call.
func diffSorted(a, b []string) []string {
	out := []string{}
	bs := map[string]struct{}{}
	for _, s := range b {
		bs[s] = struct{}{}
	}
	for _, s := range a {
		if _, present := bs[s]; !present {
			out = append(out, s)
		}
	}
	return out
}
