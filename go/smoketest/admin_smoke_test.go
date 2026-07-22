// admin-surface smoke test — spawns the Go binary against a temp-copy
// of the canonical toolkit.db and exercises every admin meta-tool
// action through the MCP wire protocol. DB-row verifications query the
// temp copy via direct sqlite3 reads.
//
// # Running
//
//	TOOLKIT_SERVER_BINARY=../bin/toolkit-server \
//	  go test -tags sqlite_fts5 -v -run TestAdminSmoke ./smoketest/...
package smoketest_test

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type adminServer struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Scanner
	stderr  strings.Builder
	nextID  int
	dbPath  string
	binPath string
}

func spawnAdminServer(t *testing.T, binary, dbPath, blueprintsDir, rubricsDir string) *adminServer {
	t.Helper()
	args := []string{
		"--db", dbPath,
		"--default-project", "mcp-servers",
		"--blueprints-dir", blueprintsDir,
		"--rubrics-dir", rubricsDir,
	}
	cmd := exec.Command(binary, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	srv := &adminServer{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewScanner(stdout),
		dbPath:  dbPath,
		binPath: binary,
	}
	srv.stdout.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	srv.stderr = errBuf

	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("server stderr:\n%s", errBuf.String())
		}
	})

	srv.send(t, map[string]any{
		"jsonrpc": "2.0", "id": srv.id(), "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "admin-smoke", "version": "0.1"},
		},
	})
	if _, ok := srv.readLine(5 * time.Second); !ok {
		t.Fatalf("no response to initialize\nstderr:\n%s", errBuf.String())
	}
	srv.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return srv
}

func (s *adminServer) id() int { s.nextID++; return s.nextID }

func (s *adminServer) send(t *testing.T, msg any) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fmt.Fprintf(s.stdin, "%s\n", b); err != nil {
		t.Logf("send: %v", err)
	}
}

func (s *adminServer) readLine(timeout time.Duration) (string, bool) {
	ch := make(chan string, 1)
	go func() {
		if s.stdout.Scan() {
			ch <- s.stdout.Text()
		} else {
			close(ch)
		}
	}()
	select {
	case line, ok := <-ch:
		return line, ok
	case <-time.After(timeout):
		return "", false
	}
}

// callAdmin dispatches one admin action through the live server and
// returns the parsed payload. Wraps array payloads under {_rows: [...]}
// so callers always get a map[string]any.
func (s *adminServer) callAdmin(t *testing.T, action string, params map[string]any) map[string]any {
	t.Helper()
	id := s.id()
	s.send(t, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{
			"name": "admin",
			"arguments": map[string]any{
				"action": action,
				"params": params,
			},
		},
	})
	line, ok := s.readLine(10 * time.Second)
	if !ok {
		t.Fatalf("no response to %s\nstderr:\n%s", action, s.stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("parse %s response: %v (raw=%s)", action, err, line)
	}
	if resp["error"] != nil {
		t.Fatalf("MCP envelope error on %s: %v", action, resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	contentArr, _ := result["content"].([]any)
	if len(contentArr) == 0 {
		t.Fatalf("empty content for %s: %s", action, line)
	}
	content := contentArr[0].(map[string]any)
	text, _ := content["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		var arr []any
		if err2 := json.Unmarshal([]byte(text), &arr); err2 == nil {
			return map[string]any{"_rows": arr}
		}
		t.Fatalf("parse %s payload: %v (text=%s)", action, err, text)
	}
	return payload
}

// prepAdminDB copies the canonical DB into t.TempDir() so smoke writes
// don't pollute the live DB.
func prepAdminDB(t *testing.T) string {
	t.Helper()
	if src := os.Getenv("TOOLKIT_SMOKE_DB"); src != "" {
		return src
	}
	srcPath := "../../data/toolkit.db"
	if _, err := os.Stat(srcPath); err != nil {
		t.Skipf("canonical DB %s not found: %v", srcPath, err)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	dstPath := filepath.Join(t.TempDir(), "toolkit-admin-smoke.db")
	dst, err := os.Create(dstPath)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return dstPath
}

// TestAdminSmoke walks every admin action end-to-end through MCP.
// Single test rather than per-action so the MCP handshake amortises.
func TestAdminSmoke(t *testing.T) {
	binary := serverBinary(t)
	dbPath := prepAdminDB(t)
	srv := spawnAdminServer(t, binary,
		dbPath,
		"../../blueprints/forge-schemas",
		"../../blueprints/rubrics",
	)

	t.Run("health", func(t *testing.T) {
		got := srv.callAdmin(t, "health", nil)
		if got["server"] != "toolkit-server-go" {
			t.Errorf("server = %v, want toolkit-server-go", got["server"])
		}
		if got["db_ok"] != true {
			t.Errorf("db_ok = %v, want true", got["db_ok"])
		}
		if got["schema_head"] == nil {
			t.Error("schema_head missing")
		}
		// Rust parity: status / timestamp / schema_head must be present.
		for _, k := range []string{"status", "timestamp", "schema_head"} {
			if _, ok := got[k]; !ok {
				t.Errorf("missing Rust-parity field %q", k)
			}
		}
		// T61-extended: ok / uptime_seconds / version.
		for _, k := range []string{"ok", "uptime_seconds", "version"} {
			if _, ok := got[k]; !ok {
				t.Errorf("missing T61 field %q", k)
			}
		}
	})

	t.Run("server_version", func(t *testing.T) {
		got := srv.callAdmin(t, "server_version", nil)
		if got["package_version"] == nil {
			t.Error("package_version missing")
		}
	})

	t.Run("schema_version", func(t *testing.T) {
		got := srv.callAdmin(t, "schema_version", nil)
		if got["id"] == nil || got["name"] == nil || got["run_at"] == nil {
			t.Errorf("missing field in %+v", got)
		}
	})

	t.Run("project_list_includes_seed_packet", func(t *testing.T) {
		got := srv.callAdmin(t, "project_list", nil)
		rows, _ := got["_rows"].([]any)
		if len(rows) == 0 {
			t.Fatalf("empty project list: %+v", got)
		}
		seen := map[string]bool{}
		for _, r := range rows {
			m := r.(map[string]any)
			if id, ok := m["id"].(string); ok {
				seen[id] = true
			}
		}
		for _, want := range []string{"seed-packet", "mcp-servers"} {
			if !seen[want] {
				t.Errorf("project_list missing %q (got %v)", want, seen)
			}
		}
	})

	t.Run("schema_reload", func(t *testing.T) {
		got := srv.callAdmin(t, "schema_reload", nil)
		if got["ok"] != true {
			t.Errorf("ok = %v", got["ok"])
		}
		count, _ := got["schema_count"].(float64)
		if count < 1 {
			t.Errorf("schema_count = %v, want >= 1", got["schema_count"])
		}
	})

	t.Run("vault_search_metrics", func(t *testing.T) {
		got := srv.callAdmin(t, "vault_search_metrics", map[string]any{"recent_n": 5})
		if got["total_calls"] == nil {
			t.Error("total_calls missing")
		}
	})

	t.Run("host_register_then_remove", func(t *testing.T) {
		// Register a fresh test host slug.
		slug := "smoke-test-host"
		got := srv.callAdmin(t, "host_register", map[string]any{
			"id":                slug,
			"hostname":          "192.0.2.1", // TEST-NET-1
			"ssh_user":          "smoke",
			"ssh_port":          22,
			"description":       "smoke test host; removed at end of run",
			"passwordless_sudo": false,
		})
		if got["ok"] != true || got["host_id"] != slug {
			t.Fatalf("host_register: %+v", got)
		}
		// DB-row verification (PARITY_STANDARD §2.3).
		row, err := sqlAt(t, dbPath,
			"SELECT addr, ssh_user, ssh_port FROM hosts WHERE slug = ?", slug)
		if err != nil {
			t.Fatalf("sqlite verify: %v", err)
		}
		if row[0] != "192.0.2.1" || row[1] != "smoke" || row[2] != "22" {
			t.Errorf("host row wrong: %+v", row)
		}

		// host_list should include the registered host.
		got = srv.callAdmin(t, "host_list", nil)
		rows, _ := got["_rows"].([]any)
		found := false
		for _, r := range rows {
			m := r.(map[string]any)
			if m["slug"] == slug {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("host_list missing %q after register", slug)
		}

		// host_remove soft-deletes (stamps retired_at).
		got = srv.callAdmin(t, "host_remove", map[string]any{"id": slug})
		if got["ok"] != true || got["retired"] != true {
			t.Fatalf("host_remove: %+v", got)
		}
		row, err = sqlAt(t, dbPath,
			"SELECT retired_at IS NOT NULL FROM hosts WHERE slug = ?", slug)
		if err != nil {
			t.Fatalf("sqlite verify retire: %v", err)
		}
		if row[0] != "1" {
			t.Errorf("retired_at not set: %+v", row)
		}
	})

	t.Run("project_register_idempotent", func(t *testing.T) {
		_ = srv.callAdmin(t, "project_register", map[string]any{
			"id":   "smoke-test-project",
			"name": "Smoke Project v1",
			"path": "/tmp/smoke-test-project",
		})
		_ = srv.callAdmin(t, "project_register", map[string]any{
			"id":   "smoke-test-project",
			"name": "Smoke Project v2", // upsert should land
			"path": "/tmp/smoke-test-project",
		})
		row, err := sqlAt(t, dbPath,
			"SELECT name, path FROM projects WHERE id = ?", "smoke-test-project")
		if err != nil {
			t.Fatalf("sqlite verify: %v", err)
		}
		if row[0] != "Smoke Project v2" {
			t.Errorf("upsert didn't land: %+v", row)
		}
	})

	t.Run("apply_recipe_deferred_stub", func(t *testing.T) {
		got := srv.callAdmin(t, "apply_recipe", map[string]any{
			"slug": "anything", "project_id": "mcp-servers",
		})
		if got["error"] != "action_deferred" || got["action"] != "apply_recipe" {
			t.Errorf("expected deferred stub, got %+v", got)
		}
	})
}

// sqlAt opens the smoke DB read-only and returns the first row of the
// query as []string. Helps tests verify their writes landed.
func sqlAt(t *testing.T, dbPath, q string, args ...any) ([]string, error) {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, fmt.Errorf("no row")
	}
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	scan := make([]any, len(cols))
	for i := range vals {
		scan[i] = &vals[i]
	}
	if err := rows.Scan(scan...); err != nil {
		return nil, err
	}
	out := make([]string, len(cols))
	for i, v := range vals {
		switch x := v.(type) {
		case nil:
			out[i] = ""
		case []byte:
			out[i] = string(x)
		default:
			out[i] = fmt.Sprintf("%v", x)
		}
	}
	return out, nil
}
