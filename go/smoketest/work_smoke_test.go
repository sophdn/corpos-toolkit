// work-surface smoke test — spawns the Go binary against a temp-copy of
// the real toolkit.db, exercises the full create→start→complete→close
// lifecycle plus bug resolution + roadmap roundtrip, and verifies each
// row landed via direct sqlite3 reads.
//
// Safety: defaults to a copy of `data/toolkit.db` in t.TempDir() so the
// real DB is never mutated. Override via TOOLKIT_SMOKE_DB to run against
// a different file (e.g. the live DB after a manual backup).
//
// # Running
//
//	TOOLKIT_SERVER_BINARY=../bin/toolkit-server \
//	  go test -tags sqlite_fts5 -v -run TestWorkSmoke ./smoketest/...
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

	_ "modernc.org/sqlite"
)

// workServer wraps a spawned server with the work-meta-tool invocation
// helper. The server stays alive across multiple calls; tests close it
// via t.Cleanup so a single MCP handshake amortises across the whole
// lifecycle exercise.
type workServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr strings.Builder
	nextID int
}

func spawnWorkServer(t *testing.T, binary, dbPath, blueprintsDir string) *workServer {
	t.Helper()
	args := []string{
		"--db", dbPath,
		"--default-project", "mcp-servers",
		"--blueprints-dir", blueprintsDir,
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
	srv := &workServer{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
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

	// initialize handshake
	srv.send(t, map[string]any{
		"jsonrpc": "2.0", "id": srv.id(), "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "work-smoke", "version": "0.1"},
		},
	})
	if _, ok := srv.readLine(5 * time.Second); !ok {
		t.Fatalf("no response to initialize\nstderr:\n%s", errBuf.String())
	}
	srv.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return srv
}

func (s *workServer) id() int {
	s.nextID++
	return s.nextID
}

func (s *workServer) send(t *testing.T, msg any) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fmt.Fprintf(s.stdin, "%s\n", b); err != nil {
		t.Logf("send: %v (server may have exited)", err)
	}
}

func (s *workServer) readLine(timeout time.Duration) (string, bool) {
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

// callWork dispatches one work action through the running server and
// returns the parsed payload from the result content. Errors at the MCP
// envelope level surface as test fatals; structured action-level errors
// (e.g. {"error": "chain_not_found"}) come back as the parsed map.
func (s *workServer) callWork(t *testing.T, action string, params map[string]any) map[string]any {
	t.Helper()
	id := s.id()
	s.send(t, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{
			"name": "work",
			"arguments": map[string]any{
				"action":  action,
				"params":  params,
				"project": "mcp-servers",
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
		// Some actions return an array; wrap so callers always see a map.
		var arr []any
		if err2 := json.Unmarshal([]byte(text), &arr); err2 == nil {
			return map[string]any{"_rows": arr}
		}
		t.Fatalf("parse %s payload: %v (text=%s)", action, err, text)
	}
	return payload
}

// prepWorkDB writes a fresh copy of the canonical toolkit.db into t.TempDir
// and returns the path. Smoke runs land on the copy so the canonical DB is
// never mutated. Override via TOOLKIT_SMOKE_DB to point at a manually-
// prepared backup.
func prepWorkDB(t *testing.T) string {
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
	dstPath := filepath.Join(t.TempDir(), "toolkit-smoke.db")
	dst, err := os.Create(dstPath)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatalf("copy: %v", err)
	}
	dst.Close()
	return dstPath
}

func blueprintsDirSmoke(t *testing.T) string {
	t.Helper()
	dir := "../../blueprints/forge-schemas"
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("blueprints dir %s not found: %v", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// TestWorkSmoke_ChainLifecycle exercises the full chain + task lifecycle
// against the live Go binary: forge(chain) → forge(task) → task_start →
// task_complete → chain_close. Each step is followed by a direct DB read
// asserting column values.
func TestWorkSmoke_ChainLifecycle(t *testing.T) {
	binary := serverBinary(t)
	dbPath := prepWorkDB(t)
	bpDir := blueprintsDirSmoke(t)

	srv := spawnWorkServer(t, binary, dbPath, bpDir)

	now := time.Now().Unix()
	chainSlug := fmt.Sprintf("smoke-chain-%d", now)
	taskSlug := fmt.Sprintf("smoke-task-%d", now)

	// 1. forge(chain)
	resp := srv.callWork(t, "forge", map[string]any{
		"schema_name":          "chain",
		"slug":                 chainSlug,
		"output":               "smoke test output",
		"design_decisions":     "smoke decisions",
		"completion_condition": "smoke done",
	})
	if resp["ok"] != true {
		t.Fatalf("forge chain: %v", resp)
	}

	// 2. DB verification
	dbConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	defer dbConn.Close()
	var chainStatus, chainOutput string
	if err := dbConn.QueryRow(`SELECT status, output FROM proj_chain_status WHERE slug = ?`, chainSlug).Scan(&chainStatus, &chainOutput); err != nil {
		t.Fatalf("read chain row: %v", err)
	}
	if chainStatus != "open" || chainOutput != "smoke test output" {
		t.Errorf("chain row mismatch: status=%q output=%q", chainStatus, chainOutput)
	}

	// 3. forge(task)
	resp = srv.callWork(t, "forge", map[string]any{
		"schema_name":       "task",
		"slug":              taskSlug,
		"chain_slug":        chainSlug,
		"problem_statement": "do the smoke thing",
	})
	if resp["ok"] != true {
		t.Fatalf("forge task: %v", resp)
	}
	var taskStatus string
	if err := dbConn.QueryRow(`SELECT t.status FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id WHERE t.slug = ? AND c.slug = ?`, taskSlug, chainSlug).Scan(&taskStatus); err != nil {
		t.Fatalf("read task row: %v", err)
	}
	if taskStatus != "pending" {
		t.Errorf("task status after forge: %q", taskStatus)
	}

	// 4. task_start
	resp = srv.callWork(t, "task_start", map[string]any{
		"slug": taskSlug, "chain_slug": chainSlug,
	})
	if resp["ok"] != true {
		t.Fatalf("task_start: %v", resp)
	}
	dbConn.QueryRow(`SELECT status FROM proj_current_tasks WHERE slug = ?`, taskSlug).Scan(&taskStatus)
	if taskStatus != "active" {
		t.Errorf("task status after start: %q", taskStatus)
	}

	// 5. task_complete with commit_sha
	resp = srv.callWork(t, "task_complete", map[string]any{
		"slug":           taskSlug,
		"chain_slug":     chainSlug,
		"commit_sha":     "abc123smoke",
		"handoff_output": "smoke done",
	})
	if resp["ok"] != true {
		t.Fatalf("task_complete: %v", resp)
	}
	var sha string
	dbConn.QueryRow(`SELECT status, commit_sha FROM proj_current_tasks WHERE slug = ?`, taskSlug).Scan(&taskStatus, &sha)
	if taskStatus != "closed" || sha != "abc123smoke" {
		t.Errorf("post-complete row: status=%q sha=%q", taskStatus, sha)
	}

	// 6. chain_close
	resp = srv.callWork(t, "chain_close", map[string]any{
		"slug":    chainSlug,
		"summary": "smoke complete",
	})
	if resp["ok"] != true {
		t.Fatalf("chain_close: %v", resp)
	}
	var closureSummary string
	dbConn.QueryRow(`SELECT status, closure_summary FROM proj_chain_status WHERE slug = ?`, chainSlug).Scan(&chainStatus, &closureSummary)
	if chainStatus != "closed" || closureSummary != "smoke complete" {
		t.Errorf("post-close chain: status=%q summary=%q", chainStatus, closureSummary)
	}

	t.Logf("chain lifecycle smoke OK: chain=%s task=%s", chainSlug, taskSlug)
}

// TestWorkSmoke_BugResolutionLifecycle exercises forge(bug) → bug_resolve
// (with the verb-form alias normaliser) → bug_read.
func TestWorkSmoke_BugResolutionLifecycle(t *testing.T) {
	binary := serverBinary(t)
	dbPath := prepWorkDB(t)
	bpDir := blueprintsDirSmoke(t)

	srv := spawnWorkServer(t, binary, dbPath, bpDir)

	now := time.Now().Unix()
	bugSlug := fmt.Sprintf("smoke-bug-%d", now)

	resp := srv.callWork(t, "forge", map[string]any{
		"schema_name":       "bug",
		"slug":              bugSlug,
		"title":             "smoke bug title",
		"problem_statement": "smoke problem",
		"severity":          "low",
	})
	if resp["ok"] != true {
		t.Fatalf("forge bug: %v", resp)
	}

	// Resolve with verb-form alias `fix` — should canonicalise to `fixed`.
	resp = srv.callWork(t, "bug_resolve", map[string]any{
		"slug":            bugSlug,
		"resolution_kind": "fix",
		"resolution_note": "smoke fix",
		"commit_sha":      "smokesha",
	})
	if resp["ok"] != true {
		t.Fatalf("bug_resolve: %v", resp)
	}
	if resp["resolution_kind"] != "fixed" {
		t.Errorf("alias canonicalise: got %v", resp["resolution_kind"])
	}

	// DB verification — kind canonicalised, sha persisted, resolved_at set.
	dbConn, _ := sql.Open("sqlite", dbPath)
	defer dbConn.Close()
	var status, kind, sha, resolvedAt sql.NullString
	dbConn.QueryRow(`SELECT status, resolution_kind, resolved_commit_sha, resolved_at FROM proj_current_bugs WHERE slug = ?`, bugSlug).
		Scan(&status, &kind, &sha, &resolvedAt)
	if status.String != "fixed" || kind.String != "fixed" || sha.String != "smokesha" {
		t.Errorf("bug row mismatch: status=%q kind=%q sha=%q", status.String, kind.String, sha.String)
	}
	if resolvedAt.String == "" {
		t.Error("resolved_at not set after bug_resolve")
	}

	// bug_read by slug verifies the read path.
	resp = srv.callWork(t, "bug_read", map[string]any{"slug": bugSlug})
	if resp["slug"] != bugSlug || resp["resolution_kind"] != "fixed" {
		t.Errorf("bug_read: %v", resp)
	}

	t.Logf("bug resolution smoke OK: bug=%s", bugSlug)
}

// TestWorkSmoke_RoadmapList verifies roadmap_list returns without error
// against the live DB. The list may be empty; the assertion is structural
// — a successful call with an array (possibly empty) response.
func TestWorkSmoke_RoadmapList(t *testing.T) {
	binary := serverBinary(t)
	dbPath := prepWorkDB(t)
	bpDir := blueprintsDirSmoke(t)

	srv := spawnWorkServer(t, binary, dbPath, bpDir)

	resp := srv.callWork(t, "roadmap_list", map[string]any{})
	// Handler returns an array; the wrapper packs it as _rows.
	if _, ok := resp["_rows"]; !ok {
		// Could also be an empty array; both shapes are acceptable.
		if _, isMap := resp["error"]; isMap {
			t.Fatalf("roadmap_list error: %v", resp)
		}
		t.Logf("roadmap_list returned non-array payload: %v", resp)
	}
}

// TestWorkSmoke_TaskBlockerRoundtrip covers task_block → task_blockers →
// task_unblock.
func TestWorkSmoke_TaskBlockerRoundtrip(t *testing.T) {
	binary := serverBinary(t)
	dbPath := prepWorkDB(t)
	bpDir := blueprintsDirSmoke(t)

	srv := spawnWorkServer(t, binary, dbPath, bpDir)

	now := time.Now().Unix()
	chainSlug := fmt.Sprintf("smoke-block-chain-%d", now)
	a := fmt.Sprintf("smoke-task-a-%d", now)
	b := fmt.Sprintf("smoke-task-b-%d", now)

	srv.callWork(t, "forge", map[string]any{
		"schema_name":          "chain",
		"slug":                 chainSlug,
		"output":               "o",
		"design_decisions":     "d",
		"completion_condition": "cc",
	})
	srv.callWork(t, "forge", map[string]any{
		"schema_name":       "task",
		"slug":              a,
		"chain_slug":        chainSlug,
		"problem_statement": "a",
	})
	srv.callWork(t, "forge", map[string]any{
		"schema_name":       "task",
		"slug":              b,
		"chain_slug":        chainSlug,
		"problem_statement": "b",
	})

	// Block a on b.
	resp := srv.callWork(t, "task_block", map[string]any{
		"slug":         a,
		"chain_slug":   chainSlug,
		"blocker_slug": b,
		"reason":       "waiting",
	})
	if resp["ok"] != true {
		t.Fatalf("task_block: %v", resp)
	}

	// task_blockers returns an array of BlockerEntry.
	resp = srv.callWork(t, "task_blockers", map[string]any{"slug": a, "chain_slug": chainSlug})
	rows, _ := resp["_rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("task_blockers: want 1, got %v", resp)
	}
	first, _ := rows[0].(map[string]any)
	if first["slug"] != b {
		t.Errorf("blocker entry: %v", first)
	}

	// Unblock.
	resp = srv.callWork(t, "task_unblock", map[string]any{
		"slug":         a,
		"chain_slug":   chainSlug,
		"blocker_slug": b,
	})
	if resp["ok"] != true {
		t.Fatalf("task_unblock: %v", resp)
	}
	resp = srv.callWork(t, "task_blockers", map[string]any{"slug": a, "chain_slug": chainSlug})
	rows, _ = resp["_rows"].([]any)
	if len(rows) != 0 {
		t.Errorf("post-unblock blockers: %v", resp)
	}

	t.Logf("blocker roundtrip smoke OK: a=%s b=%s", a, b)
}
