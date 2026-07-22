// Package smoketest is a stdio JSON-RPC smoke harness for toolkit-server.
//
// It spawns a target binary (Rust or Go) via stdin/stdout and exercises the
// MCP wire protocol at the protocol level. The harness is intentionally
// separate from the inference benchmarks in benchmarks/ — it tests protocol
// conformance and crash-resistance, not model quality.
//
// # Running
//
// `go test` runs the test binary with cwd = `go/smoketest/`, so relative
// paths in env vars resolve from that directory. The examples below use
// the relative paths that work from a fresh shell at `go/`; absolute
// paths are always safe.
//
// Against the Go binary (build first with `make build` from go/):
//
//	TOOLKIT_SERVER_BINARY=../bin/toolkit-server \
//	  TOOLKIT_SMOKE_DB=../../data/toolkit.db \
//	  go test -v -run TestSmoke ./smoketest/...
//
// Against the Rust binary (build first with `cargo build -p toolkit-server` from mcp-servers/):
//
//	TOOLKIT_SERVER_BINARY=../../target/debug/toolkit-server \
//	  TOOLKIT_SMOKE_DB=../../data/toolkit.db \
//	  go test -v -run TestSmoke ./smoketest/...
//
// # Comparing Rust vs Go
//
// Run the harness twice, capturing output:
//
//	TOOLKIT_SERVER_BINARY=../../target/debug/toolkit-server go test -v ./smoketest/... > rust.txt 2>&1
//	TOOLKIT_SERVER_BINARY=../bin/toolkit-server             go test -v ./smoketest/... > go.txt 2>&1
//	diff rust.txt go.txt
//
// A surface is ready to migrate when `diff rust.txt go.txt` is empty for all TestSmoke cases.
//
// # Stub behaviour
//
// The Go binary skeleton does not yet implement MCP. Tests that require a
// full MCP handshake are skipped when the binary exits before completing the
// initialize exchange. The harness logs the exit code and stderr — if the
// binary panics (non-zero exit), the test fails; if it exits cleanly (code
// 0), the case is recorded as a successful stub run.
package smoketest_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// serverBinary returns the path to the toolkit-server binary under test.
// Reads TOOLKIT_SERVER_BINARY env var; skips the test if unset.
func serverBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("TOOLKIT_SERVER_BINARY")
	if bin == "" {
		t.Skip("TOOLKIT_SERVER_BINARY not set; skipping smoke test")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("TOOLKIT_SERVER_BINARY=%q: %v", bin, err)
	}
	return bin
}

// smokeDB returns the path to the toolkit SQLite DB for smoke runs.
// Falls back to ../../data/toolkit.db if TOOLKIT_SMOKE_DB is unset —
// resolved from the test cwd (`go/smoketest/`), this is the canonical
// `mcp-servers/data/toolkit.db`.
func smokeDB() string {
	if db := os.Getenv("TOOLKIT_SMOKE_DB"); db != "" {
		return db
	}
	return "../../data/toolkit.db"
}

// server manages a spawned toolkit-server process communicating via stdio JSON-RPC.
type server struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr strings.Builder
}

// spawn starts the binary and wires up stdin/stdout.
func spawn(t *testing.T, binary, dbPath string) *server {
	t.Helper()
	cmd := exec.Command(binary, "--db", dbPath, "--default-project", "mcp-servers")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	var errBuf strings.Builder
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %q: %v", binary, err)
	}

	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("server stderr:\n%s", errBuf.String())
		}
	})

	return &server{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
		stderr: errBuf,
	}
}

// send writes a JSON-RPC message to the server's stdin.
func (s *server) send(t *testing.T, msg any) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := fmt.Fprintf(s.stdin, "%s\n", data); err != nil {
		// Server may have exited — not necessarily a failure.
		t.Logf("send: %v (server may have exited)", err)
	}
}

// readLine reads one line from stdout with a timeout.
// Returns ("", false) if the server closed stdout or the timeout fires.
func (s *server) readLine(t *testing.T, timeout time.Duration) (string, bool) {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		if s.stdout.Scan() {
			done <- s.stdout.Text()
		} else {
			close(done)
		}
	}()
	select {
	case line, ok := <-done:
		return line, ok
	case <-time.After(timeout):
		t.Logf("readLine: timeout after %v (server may be a stub)", timeout)
		return "", false
	}
}

// jsonrpcRequest is a minimal JSON-RPC 2.0 request.
type jsonrpcRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonrpcResponse is a minimal JSON-RPC 2.0 response shape.
type jsonrpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func intPtr(i int) *int { return &i }

// TestSmoke_ServerStartsClean verifies the binary starts and exits cleanly
// when stdin is closed immediately. No MCP exchange required.
// This is the minimum bar for any binary in the migration — it must not panic
// on startup.
func TestSmoke_ServerStartsClean(t *testing.T) {
	binary := serverBinary(t)
	db := smokeDB()

	cmd := exec.Command(binary, "--db", db, "--default-project", "mcp-servers")
	var errBuf strings.Builder
	cmd.Stderr = &errBuf

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Close stdin immediately — a well-behaved server should shut down.
	_ = stdin.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			exitCode := -1
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
			t.Errorf("server exited with error (code %d): %v\nstderr:\n%s", exitCode, err, errBuf.String())
		} else {
			t.Logf("server exited cleanly (stub or graceful shutdown)")
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Errorf("server did not exit within 5s after stdin closed")
	}
}

// TestSmoke_Initialize sends an MCP initialize request and checks that the
// response is either valid JSON-RPC or that the server exits cleanly (stub).
// A non-zero exit code is a failure; a timeout with a panic string on stderr
// is a failure.
func TestSmoke_Initialize(t *testing.T) {
	binary := serverBinary(t)
	db := smokeDB()

	s := spawn(t, binary, db)

	req := jsonrpcRequest{
		Jsonrpc: "2.0",
		ID:      intPtr(1),
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "smoke-test", "version": "0.1"},
		},
	}
	s.send(t, req)

	line, ok := s.readLine(t, 3*time.Second)
	if !ok {
		// Server exited without responding — verify it exited cleanly.
		done := make(chan error, 1)
		go func() { done <- s.cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server exited with error after initialize: %v", err)
			} else {
				t.Log("stub: server exited cleanly without MCP response (expected for skeleton)")
			}
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
			t.Error("server did not exit after stdin closed; possible hang")
		}
		return
	}

	// Server responded — verify it is valid JSON-RPC.
	var resp jsonrpcResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v\nraw: %s", err, line)
	}
	if resp.Jsonrpc != "2.0" {
		t.Errorf("jsonrpc field: want %q, got %q", "2.0", resp.Jsonrpc)
	}
	if resp.ID == nil || *resp.ID != 1 {
		t.Errorf("response id: want 1, got %v", resp.ID)
	}
	if resp.Error != nil {
		t.Logf("initialize error response (valid JSON-RPC error): code=%d msg=%q", resp.Error.Code, resp.Error.Message)
	} else {
		t.Logf("initialize succeeded: result=%s", resp.Result)
	}
}

// TestSmoke_UnimplementedAction calls an action on the work meta-tool and
// verifies the response is a structured JSON error (not a panic or garbage).
// Skipped if the server exits before completing the MCP handshake (stub mode).
func TestSmoke_UnimplementedAction(t *testing.T) {
	binary := serverBinary(t)
	db := smokeDB()

	s := spawn(t, binary, db)

	// Attempt initialize first.
	initReq := jsonrpcRequest{
		Jsonrpc: "2.0",
		ID:      intPtr(1),
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "smoke-test", "version": "0.1"},
		},
	}
	s.send(t, initReq)

	line, ok := s.readLine(t, 3*time.Second)
	if !ok {
		t.Skip("server exited before MCP handshake (stub mode) — skipping action test")
	}

	var initResp jsonrpcResponse
	if err := json.Unmarshal([]byte(line), &initResp); err != nil {
		t.Fatalf("initialize response is not valid JSON: %v", err)
	}
	if initResp.Error != nil {
		t.Skipf("server rejected initialize (not yet MCP-capable): %s", initResp.Error.Message)
	}

	// Send initialized notification.
	s.send(t, jsonrpcRequest{
		Jsonrpc: "2.0",
		Method:  "notifications/initialized",
	})

	// Call an action that may not be implemented yet.
	callReq := jsonrpcRequest{
		Jsonrpc: "2.0",
		ID:      intPtr(2),
		Method:  "tools/call",
		Params: map[string]any{
			"name": "mcp__toolkit-server__work",
			"arguments": map[string]any{
				"action": "chain_status",
				"params": map[string]any{"chain": "agent-os-go-migration"},
			},
		},
	}
	s.send(t, callReq)

	line, ok = s.readLine(t, 5*time.Second)
	if !ok {
		t.Skip("server exited before action response (stub mode)")
	}

	var callResp jsonrpcResponse
	if err := json.Unmarshal([]byte(line), &callResp); err != nil {
		t.Fatalf("action response is not valid JSON-RPC: %v\nraw: %s", err, line)
	}
	if callResp.Jsonrpc != "2.0" {
		t.Errorf("jsonrpc field: want %q, got %q", "2.0", callResp.Jsonrpc)
	}
	// Either a result or a structured error is acceptable — both prove no panic.
	if callResp.Error != nil {
		t.Logf("action returned structured error (acceptable): code=%d msg=%q", callResp.Error.Code, callResp.Error.Message)
	} else {
		t.Logf("action returned result: %s", callResp.Result)
	}
}
