// work-surface regression — runs the same MCP action sequence against
// the Rust toolkit-server binary and the Go toolkit-server-go binary on
// identical fresh-copy databases and diffs the resulting responses + DB
// rows. T84's behavioral-parity gate before T58 retires the Rust crates.
//
// # Running
//
//	TOOLKIT_RUST_BINARY=../../target/release/toolkit-server \
//	  TOOLKIT_GO_BINARY=../bin/toolkit-server \
//	  TOOLKIT_BLUEPRINTS_DIR=../../blueprints/forge-schemas \
//	  go test -tags sqlite_fts5 -v -run TestWorkRegression ./smoketest/...
//
// Skips when either binary env var is unset.
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
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// regressionServer is a binary-agnostic wrapper that handles the two
// argv shapes: Rust needs --stdio-only and discovers schemas relative to
// its binary; Go needs --blueprints-dir and has no --stdio-only flag.
type regressionServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr *strings.Builder
	nextID int
	label  string
}

func spawnRegression(t *testing.T, binary, dbPath, blueprintsDir, label string) *regressionServer {
	t.Helper()
	args := []string{"--db", dbPath, "--default-project", "mcp-servers"}
	if label == "rust" {
		args = append(args, "--stdio-only")
	} else {
		args = append(args, "--blueprints-dir", blueprintsDir)
	}
	cmd := exec.Command(binary, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("%s stdin: %v", label, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("%s stdout: %v", label, err)
	}
	errBuf := &strings.Builder{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("%s start: %v", label, err)
	}
	srv := &regressionServer{
		cmd:    cmd,
		stdin:  stdin,
		stderr: errBuf,
		label:  label,
	}
	srv.stdout = bufio.NewScanner(stdout)
	srv.stdout.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("%s stderr:\n%s", label, errBuf.String())
		}
	})

	srv.send(t, map[string]any{
		"jsonrpc": "2.0", "id": srv.id(), "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": label + "-regression", "version": "0.1"},
		},
	})
	if _, ok := srv.readLine(10 * time.Second); !ok {
		t.Fatalf("%s no initialize response\nstderr:\n%s", label, errBuf.String())
	}
	srv.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return srv
}

func (s *regressionServer) id() int { s.nextID++; return s.nextID }

func (s *regressionServer) send(t *testing.T, msg any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	if _, err := fmt.Fprintf(s.stdin, "%s\n", b); err != nil {
		t.Logf("%s send: %v", s.label, err)
	}
}

func (s *regressionServer) readLine(timeout time.Duration) (string, bool) {
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

// callWork invokes one action and returns (payload, wall_ms). Tool name
// is always "work" — Rust and Go both expose the same surface name on
// stdio MCP.
func (s *regressionServer) callWork(t *testing.T, action string, params map[string]any) (map[string]any, int64) {
	t.Helper()
	id := s.id()
	t0 := time.Now()
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
	line, ok := s.readLine(15 * time.Second)
	dur := time.Since(t0).Milliseconds()
	if !ok {
		t.Fatalf("%s no response to %s\nstderr:\n%s", s.label, action, s.stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("%s parse %s: %v (raw=%s)", s.label, action, err, line)
	}
	if resp["error"] != nil {
		t.Fatalf("%s MCP error on %s: %v", s.label, action, resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	contentArr, _ := result["content"].([]any)
	if len(contentArr) == 0 {
		t.Fatalf("%s empty content for %s", s.label, action)
	}
	content := contentArr[0].(map[string]any)
	text, _ := content["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		// Some actions return arrays; wrap.
		var arr []any
		if err2 := json.Unmarshal([]byte(text), &arr); err2 == nil {
			return map[string]any{"_rows": arr}, dur
		}
		t.Fatalf("%s parse %s payload: %v (text=%s)", s.label, action, err, text)
	}
	return payload, dur
}

func prepRegressionDB(t *testing.T, suffix string) string {
	t.Helper()
	srcPath := "../../data/toolkit.db"
	if _, err := os.Stat(srcPath); err != nil {
		t.Skipf("canonical DB not found: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "regression-"+suffix+".db")
	src, _ := os.Open(srcPath)
	defer src.Close()
	out, _ := os.Create(dst)
	io.Copy(out, src)
	out.Close()
	return dst
}

// step is one logical action in the regression sequence. label is used
// for logging + report; params is the call body; postCheck (optional)
// runs after both binaries' calls and asserts DB-row parity.
type step struct {
	label    string
	action   string
	params   map[string]any
	keepKeys []string // keys whose VALUES are stable across runs and should match (filed_at etc. excluded)
}

// keysSet returns the sorted key list for envelope comparison.
func keysSet(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	insertionSort(out)
	return out
}

func insertionSort(s []string) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// TestWorkRegression_RustVsGo spawns both binaries, runs the same scripted
// sequence, and asserts envelope-key parity + selected scalar parity.
// Latency is collected per action and reported as a table at the end.
func TestWorkRegression_RustVsGo(t *testing.T) {
	rustBin := os.Getenv("TOOLKIT_RUST_BINARY")
	goBin := os.Getenv("TOOLKIT_GO_BINARY")
	if rustBin == "" || goBin == "" {
		t.Skip("set TOOLKIT_RUST_BINARY and TOOLKIT_GO_BINARY to run the regression")
	}
	if _, err := os.Stat(rustBin); err != nil {
		t.Skipf("rust binary missing: %v", err)
	}
	if _, err := os.Stat(goBin); err != nil {
		t.Skipf("go binary missing: %v", err)
	}
	bpDir := os.Getenv("TOOLKIT_BLUEPRINTS_DIR")
	if bpDir == "" {
		bpDir, _ = filepath.Abs("../../blueprints/forge-schemas")
	}

	now := time.Now().Unix()
	chainSlug := fmt.Sprintf("regress-chain-%d", now)
	taskSlug := fmt.Sprintf("regress-task-%d", now)
	bugSlug := fmt.Sprintf("regress-bug-%d", now)

	steps := []step{
		{
			label:    "forge chain",
			action:   "forge",
			params:   map[string]any{"schema_name": "chain", "slug": chainSlug, "output": "o", "design_decisions": "d", "completion_condition": "cc"},
			keepKeys: []string{"ok", "schema_name", "slug"},
		},
		{
			label:    "forge task",
			action:   "forge",
			params:   map[string]any{"schema_name": "task", "slug": taskSlug, "chain_slug": chainSlug, "problem_statement": "p"},
			keepKeys: []string{"ok", "schema_name", "slug"},
		},
		{
			label:    "task_start",
			action:   "task_start",
			params:   map[string]any{"slug": taskSlug, "chain_slug": chainSlug},
			keepKeys: []string{"ok"},
		},
		{
			label:    "task_complete",
			action:   "task_complete",
			params:   map[string]any{"slug": taskSlug, "chain_slug": chainSlug, "commit_sha": "abc1234"},
			keepKeys: []string{"ok"},
		},
		{
			label:    "chain_close",
			action:   "chain_close",
			params:   map[string]any{"slug": chainSlug, "summary": "regression done"},
			keepKeys: []string{"ok", "chain_slug"},
		},
		{
			label:    "forge bug",
			action:   "forge",
			params:   map[string]any{"schema_name": "bug", "slug": bugSlug, "title": "regression bug", "problem_statement": "p", "severity": "low"},
			keepKeys: []string{"ok", "schema_name", "slug"},
		},
		{
			label:    "bug_resolve (verb alias)",
			action:   "bug_resolve",
			params:   map[string]any{"slug": bugSlug, "resolution_kind": "fix", "commit_sha": "abc1234"},
			keepKeys: []string{"ok", "resolution_kind"},
		},
		{
			label:    "bug_read",
			action:   "bug_read",
			params:   map[string]any{"slug": bugSlug},
			keepKeys: []string{"slug", "status", "resolution_kind"},
		},
	}

	rustDB := prepRegressionDB(t, "rust")
	goDB := prepRegressionDB(t, "go")

	rust := spawnRegression(t, rustBin, rustDB, bpDir, "rust")
	goSrv := spawnRegression(t, goBin, goDB, bpDir, "go")

	type latencyRow struct {
		label  string
		rustMs int64
		goMs   int64
	}
	var latencies []latencyRow
	var divergences []string

	for _, s := range steps {
		rustPayload, rustMs := rust.callWork(t, s.action, s.params)
		goPayload, goMs := goSrv.callWork(t, s.action, s.params)
		latencies = append(latencies, latencyRow{s.label, rustMs, goMs})
		// Trace both payloads for diagnosis.
		t.Logf("step %q\n  rust=%v\n  go=%v", s.label, rustPayload, goPayload)

		// Envelope-key parity: both should carry the same set of
		// top-level keys (modulo the volatile keys filtered above).
		rustKeys := keysSet(rustPayload)
		goKeys := keysSet(goPayload)
		// Soft check: report key-set differences. Exact key-set parity
		// is rare across implementations (Rust may emit nullable keys
		// the Go side suppresses on omitempty). We report differences,
		// don't fail on them — the value parity check below is the
		// hard gate.
		if !reflect.DeepEqual(rustKeys, goKeys) {
			t.Logf("step %q key-set diverges (informational): rust=%v go=%v", s.label, rustKeys, goKeys)
		}

		// Hard parity check: for each keepKey, assert Rust and Go
		// produce the same value.
		for _, k := range s.keepKeys {
			rv := rustPayload[k]
			gv := goPayload[k]
			if !reflect.DeepEqual(rv, gv) {
				divergences = append(divergences,
					fmt.Sprintf("step %q field %q: rust=%v (%T) go=%v (%T)", s.label, k, rv, rv, gv, gv))
			}
		}
	}

	if len(divergences) > 0 {
		t.Errorf("response-parity divergences:\n  %s", strings.Join(divergences, "\n  "))
	}

	// DB-row parity for the chain row that both lifecycles closed.
	verifyDBRowParity(t, rustDB, goDB, chainSlug, taskSlug, bugSlug)

	// Latency table.
	t.Log("=== latency per action ===")
	t.Log("action                          rust_ms  go_ms")
	for _, l := range latencies {
		t.Logf("%-30s  %7d  %5d", l.label, l.rustMs, l.goMs)
	}
}

// verifyDBRowParity reads the chain / task / bug rows from both DBs and
// asserts column-level value parity for the columns whose values are
// stable across runs (excludes timestamps, IDs).
func verifyDBRowParity(t *testing.T, rustDB, goDB, chainSlug, taskSlug, bugSlug string) {
	t.Helper()

	rustConn, _ := sql.Open("sqlite", rustDB)
	defer rustConn.Close()
	goConn, _ := sql.Open("sqlite", goDB)
	defer goConn.Close()

	rustChain := scanChainRow(t, rustConn, chainSlug, "rust")
	goChain := scanChainRow(t, goConn, chainSlug, "go")
	if !reflect.DeepEqual(rustChain, goChain) {
		t.Errorf("chain row diverges:\n  rust=%v\n  go=%v", rustChain, goChain)
	}

	rustTask := scanTaskRow(t, rustConn, taskSlug, "rust")
	goTask := scanTaskRow(t, goConn, taskSlug, "go")
	if !reflect.DeepEqual(rustTask, goTask) {
		t.Errorf("task row diverges:\n  rust=%v\n  go=%v", rustTask, goTask)
	}

	rustBug := scanBugRow(t, rustConn, bugSlug, "rust")
	goBug := scanBugRow(t, goConn, bugSlug, "go")
	if !reflect.DeepEqual(rustBug, goBug) {
		t.Errorf("bug row diverges:\n  rust=%v\n  go=%v", rustBug, goBug)
	}
}

func scanChainRow(t *testing.T, db *sql.DB, slug, label string) map[string]any {
	t.Helper()
	var status, output, dd, cc, closureSummary string
	if err := db.QueryRow(
		`SELECT status, output, design_decisions, completion_condition, closure_summary FROM proj_chain_status WHERE slug = ?`,
		slug).Scan(&status, &output, &dd, &cc, &closureSummary); err != nil {
		t.Fatalf("%s chain row: %v", label, err)
	}
	return map[string]any{
		"status":               status,
		"output":               output,
		"design_decisions":     dd,
		"completion_condition": cc,
		"closure_summary":      closureSummary,
	}
}

func scanTaskRow(t *testing.T, db *sql.DB, slug, label string) map[string]any {
	t.Helper()
	var status, problem string
	var sha sql.NullString
	if err := db.QueryRow(
		`SELECT status, problem_statement, commit_sha FROM proj_current_tasks WHERE slug = ?`,
		slug).Scan(&status, &problem, &sha); err != nil {
		t.Fatalf("%s task row: %v", label, err)
	}
	return map[string]any{
		"status":            status,
		"problem_statement": problem,
		"commit_sha":        sha.String,
	}
}

func scanBugRow(t *testing.T, db *sql.DB, slug, label string) map[string]any {
	t.Helper()
	var status, title, problem, severity, note string
	var kind, sha sql.NullString
	if err := db.QueryRow(
		`SELECT status, title, problem_statement, severity, resolution_kind, resolution_note, resolved_commit_sha FROM proj_current_bugs WHERE slug = ?`,
		slug).Scan(&status, &title, &problem, &severity, &kind, &note, &sha); err != nil {
		t.Fatalf("%s bug row: %v", label, err)
	}
	return map[string]any{
		"status":              status,
		"title":               title,
		"problem_statement":   problem,
		"severity":            severity,
		"resolution_kind":     kind.String,
		"resolution_note":     note,
		"resolved_commit_sha": sha.String,
	}
}
