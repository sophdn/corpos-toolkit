// Package smoketest_test — measure-surface smoke comparison.
//
// The measure surface in Go is the survivor of T42+T43 retirements:
// only benchmark_record + benchmark_query carry over; session_journal
// and emotive_battery are wiped, not ported. This test exercises the
// two remaining actions against the live Go binary with the real
// toolkit.db and verifies (a) the response shape, (b) the row landed
// in benchmark_results, (c) benchmark_query returns the row with all
// fields populated.
//
// # Running
//
// Paths are relative to the test cwd (`go/smoketest/`):
//
//	TOOLKIT_SERVER_BINARY=../bin/toolkit-server \
//	  TOOLKIT_SMOKE_DB=../../data/toolkit.db \
//	  go test -tags sqlite_fts5 -v -run TestMeasureSmoke ./smoketest/...
package smoketest_test

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// callMeasure spawns the Go binary, performs the MCP handshake, sends one
// measure tools/call, and returns the parsed payload from the result content.
func callMeasure(t *testing.T, binary, dbPath, action string, params map[string]any) (map[string]any, error) {
	t.Helper()
	args := []string{"--db", dbPath, "--default-project", "mcp-servers"}
	cmd := exec.Command(binary, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	readLine := func(d time.Duration) (string, bool) {
		ch := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				ch <- scanner.Text()
			} else {
				close(ch)
			}
		}()
		select {
		case line, ok := <-ch:
			return line, ok
		case <-time.After(d):
			return "", false
		}
	}
	send := func(msg any) {
		b, _ := json.Marshal(msg)
		fmt.Fprintf(stdin, "%s\n", b)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()

	send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "measure-smoke", "version": "0.1"},
		},
	})
	line, ok := readLine(5 * time.Second)
	if !ok {
		return nil, fmt.Errorf("no response to initialize\nstderr:\n%s", errBuf.String())
	}
	var initResp map[string]any
	if err := json.Unmarshal([]byte(line), &initResp); err != nil {
		return nil, fmt.Errorf("parse initialize response: %w", err)
	}
	if initResp["error"] != nil {
		return nil, fmt.Errorf("initialize error: %v", initResp["error"])
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	send(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "measure",
			"arguments": map[string]any{"action": action, "params": params},
		},
	})
	line, ok = readLine(10 * time.Second)
	if !ok {
		return nil, fmt.Errorf("no response to %s\nstderr:\n%s", action, errBuf.String())
	}
	var callResp map[string]any
	if err := json.Unmarshal([]byte(line), &callResp); err != nil {
		return nil, fmt.Errorf("parse %s response: %w (raw=%s)", action, err, line)
	}
	if callResp["error"] != nil {
		return nil, fmt.Errorf("MCP error: %v", callResp["error"])
	}
	result, _ := callResp["result"].(map[string]any)
	if result == nil {
		return nil, fmt.Errorf("no result in response: %s", line)
	}
	contentArr, _ := result["content"].([]any)
	if len(contentArr) == 0 {
		return nil, fmt.Errorf("empty content array: %s", line)
	}
	content, _ := contentArr[0].(map[string]any)
	text, _ := content["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("empty text in content: %s", line)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		// Some actions (benchmark_query) return an array. Re-unmarshal into a wrapper.
		var arr []any
		if err2 := json.Unmarshal([]byte(text), &arr); err2 == nil {
			return map[string]any{"_rows": arr}, nil
		}
		return nil, fmt.Errorf("parse payload: %w (text=%s)", err, text)
	}
	return payload, nil
}

// TestMeasureSmoke_BenchmarkRecordRoundtrip exercises benchmark_record +
// benchmark_query end-to-end against the live Go binary and the real
// toolkit.db. After the write, queries the row directly via sqlite3 to
// verify column values (PARITY_STANDARD §2.3 DB-row verification).
func TestMeasureSmoke_BenchmarkRecordRoundtrip(t *testing.T) {
	binary := serverBinary(t)
	dbPath := smokeDB()

	// Unique per-run identifiers so concurrent runs / leftover rows from
	// prior runs do not collide.
	now := time.Now().Unix()
	scenarioID := fmt.Sprintf("smoke-measure-%d", now)
	runID := fmt.Sprintf("smoke-run-%d", now)

	// 1. benchmark_record
	recordPayload, err := callMeasure(t, binary, dbPath, "benchmark_record", map[string]any{
		"scenario_id":   scenarioID,
		"tool_name":     "smoke.benchmark_record",
		"model_name":    "smoke-test-model",
		"run_id":        runID,
		"run_at":        now,
		"wall_clock_ms": 42,
		"invocation_ok": true,
		"input_tokens":  128,
		"output_tokens": 8,
		"notes":         "T45 measure smoke",
		"task_shape":    "Classify",
	})
	if err != nil {
		t.Fatalf("benchmark_record: %v", err)
	}
	if recordPayload["ok"] != true {
		t.Fatalf("benchmark_record: expected ok=true, got %v", recordPayload)
	}
	id, _ := recordPayload["id"].(string)
	if id == "" {
		t.Fatalf("benchmark_record: missing id in payload %v", recordPayload)
	}

	// 2. DB-row verification — query the row directly and assert columns.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db for verification: %v", err)
	}
	defer db.Close()

	var (
		gotScenario, gotTool, gotModel, gotRunID, gotNotes, gotTaskShape sql.NullString
		gotRunAt, gotWall, gotInTok, gotOutTok, gotInvocationOK          sql.NullInt64
		gotInvokedCtx                                                    sql.NullInt64
	)
	err = db.QueryRow(`SELECT scenario_id, tool_name, model_name, run_id, run_at,
		wall_clock_ms, input_tokens, output_tokens, invocation_ok,
		invoked_contextually, notes, task_shape
		FROM proj_benchmark_results WHERE id = ?`, id).Scan(
		&gotScenario, &gotTool, &gotModel, &gotRunID, &gotRunAt,
		&gotWall, &gotInTok, &gotOutTok, &gotInvocationOK,
		&gotInvokedCtx, &gotNotes, &gotTaskShape,
	)
	if err != nil {
		t.Fatalf("readback row %s: %v", id, err)
	}
	if gotScenario.String != scenarioID {
		t.Errorf("scenario_id: want %q got %q", scenarioID, gotScenario.String)
	}
	if gotTool.String != "smoke.benchmark_record" {
		t.Errorf("tool_name: %q", gotTool.String)
	}
	if gotModel.String != "smoke-test-model" {
		t.Errorf("model_name: %q", gotModel.String)
	}
	if gotRunID.String != runID {
		t.Errorf("run_id: want %q got %q", runID, gotRunID.String)
	}
	if gotRunAt.Int64 != now {
		t.Errorf("run_at: want %d got %d", now, gotRunAt.Int64)
	}
	if gotWall.Int64 != 42 {
		t.Errorf("wall_clock_ms: %d", gotWall.Int64)
	}
	if gotInTok.Int64 != 128 {
		t.Errorf("input_tokens: %d", gotInTok.Int64)
	}
	if gotOutTok.Int64 != 8 {
		t.Errorf("output_tokens: %d", gotOutTok.Int64)
	}
	if gotInvocationOK.Int64 != 1 {
		t.Errorf("invocation_ok: bool true should map to 1, got %d", gotInvocationOK.Int64)
	}
	if gotInvokedCtx.Int64 != 1 {
		t.Errorf("invoked_contextually default: %d", gotInvokedCtx.Int64)
	}
	if gotNotes.String != "T45 measure smoke" {
		t.Errorf("notes: %q", gotNotes.String)
	}
	if gotTaskShape.String != "Classify" {
		t.Errorf("task_shape: %q", gotTaskShape.String)
	}

	// 3. benchmark_query — filter by run_id (uniquely identifies this row).
	queryPayload, err := callMeasure(t, binary, dbPath, "benchmark_query", map[string]any{
		"run_id": runID,
	})
	if err != nil {
		t.Fatalf("benchmark_query: %v", err)
	}
	rowsAny, ok := queryPayload["_rows"].([]any)
	if !ok {
		t.Fatalf("benchmark_query: expected array result, got %v", queryPayload)
	}
	if len(rowsAny) != 1 {
		t.Fatalf("benchmark_query: expected 1 row for run_id=%s, got %d", runID, len(rowsAny))
	}
	row, _ := rowsAny[0].(map[string]any)
	if row["id"] != id {
		t.Errorf("query row id: want %s got %v", id, row["id"])
	}
	if row["scenario_id"] != scenarioID {
		t.Errorf("query row scenario_id: %v", row["scenario_id"])
	}
	if row["notes"] != "T45 measure smoke" {
		t.Errorf("query row notes: %v", row["notes"])
	}

	// 4. Cleanup — delete the projection row so the toolkit.db isn't
	// polluted. The smoke-test event in events stays (events is append-
	// only by trigger; rebuild would resurrect the projection row, which
	// is fine for an idempotent smoke).
	if _, err := db.Exec("DELETE FROM proj_benchmark_results WHERE id = ?", id); err != nil {
		t.Logf("cleanup delete failed (non-fatal): %v", err)
	}

	t.Logf("measure smoke OK: id=%s scenario_id=%s run_id=%s", id, scenarioID, runID)
}
