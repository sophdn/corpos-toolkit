// Package smoketest_test — knowledge surface smoke comparison test.
//
// Exercises vault_search, kiwix_search, library_add/get/retire roundtrip, and
// knowledge_search against the live Go binary + real services (toolkit.db,
// kiwix-serve on localhost:8889, Qwen on localhost:8081). This is the T39
// pre-migration quality gate: Go must produce well-shaped responses on every
// knowledge action before the routing cutover.
//
// # Running
//
// Paths are relative to the test cwd (`go/smoketest/`):
//
//	TOOLKIT_SERVER_BINARY=../bin/toolkit-server \
//	  TOOLKIT_SMOKE_DB=../../data/toolkit.db \
//	  go test -tags sqlite_fts5 -v -run TestKnowledgeSmoke ./smoketest/...
package smoketest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// knowledgeCall spawns the binary, opens an MCP session, and runs one
// tools/call against the knowledge meta-tool. Returns the decoded payload
// or an error.
func knowledgeCall(t *testing.T, binary, dbPath, project, action string, params map[string]any) (map[string]any, error) {
	t.Helper()
	args := []string{"--db", dbPath, "--default-project", project}
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
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	readLine := func(timeout time.Duration) (string, bool) {
		done := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				done <- scanner.Text()
			} else {
				close(done)
			}
		}()
		select {
		case line, ok := <-done:
			return line, ok
		case <-time.After(timeout):
			return "", false
		}
	}
	send := func(msg any) {
		data, _ := json.Marshal(msg)
		fmt.Fprintf(stdin, "%s\n", data)
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()

	// Initialize.
	send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "knowledge-smoke", "version": "0.1"},
		},
	})
	line, ok := readLine(5 * time.Second)
	if !ok {
		return nil, fmt.Errorf("no response to initialize; stderr:\n%s", errBuf.String())
	}
	var initResp map[string]any
	if err := json.Unmarshal([]byte(line), &initResp); err != nil {
		return nil, fmt.Errorf("parse initialize: %w", err)
	}
	if initResp["error"] != nil {
		return nil, fmt.Errorf("initialize error: %v", initResp["error"])
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	// Call action on knowledge meta-tool.
	send(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "knowledge",
			"arguments": map[string]any{"action": action, "params": params, "project": project},
		},
	})
	// Allow long timeout: Qwen rerank + DB writes can be slow.
	line, ok = readLine(90 * time.Second)
	if !ok {
		return nil, fmt.Errorf("no response to %s (timeout); stderr:\n%s", action, errBuf.String())
	}
	var callResp map[string]any
	if err := json.Unmarshal([]byte(line), &callResp); err != nil {
		return nil, fmt.Errorf("parse call response: %w", err)
	}
	if callResp["error"] != nil {
		return nil, fmt.Errorf("call %s error: %v", action, callResp["error"])
	}
	result, _ := callResp["result"].(map[string]any)
	if result == nil {
		return nil, fmt.Errorf("no result in response: %s", line)
	}
	contentArr, _ := result["content"].([]any)
	if len(contentArr) == 0 {
		return nil, fmt.Errorf("empty content array")
	}
	content, _ := contentArr[0].(map[string]any)
	text, _ := content["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("empty text in content")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("parse payload: %w (text=%s)", err, text)
	}
	return payload, nil
}

// kiwixUp checks whether the local kiwix-serve is reachable. The smoke test
// skips kiwix_search when kiwix is down rather than failing the whole suite.
func kiwixUp() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8889/catalog/v2/entries?count=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// vaultUp checks whether the user's vault root is present.
func vaultUp() bool {
	home, ok := os.LookupEnv("HOME")
	if !ok {
		return false
	}
	if _, err := os.Stat(home + "/.claude/vault"); err != nil {
		return false
	}
	return true
}

// TestKnowledgeSmoke_VaultSearch runs vault_search against the real vault.
// Skipped when the vault directory is absent.
func TestKnowledgeSmoke_VaultSearch(t *testing.T) {
	if !vaultUp() {
		t.Skip("vault not present at $HOME/.claude/vault")
	}
	binary := serverBinary(t)
	db := smokeDB()
	payload, err := knowledgeCall(t, binary, db, "mcp-servers", "vault_search", map[string]any{
		"query": "go migration toolkit-server",
		"top_k": 3,
	})
	if err != nil {
		t.Fatalf("vault_search: %v", err)
	}
	if errMsg, ok := payload["error"].(string); ok {
		t.Fatalf("vault_search returned error: %s", errMsg)
	}
	// Response shape: results, vault_root, vault_size, latency_ms, pass2_fell_back.
	for _, key := range []string{"results", "vault_root", "vault_size", "latency_ms", "pass2_fell_back"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("vault_search missing key %q (response=%v)", key, payload)
		}
	}
	results, _ := payload["results"].([]any)
	if len(results) == 0 {
		t.Errorf("vault_search returned zero results for an on-topic query; payload=%v", payload)
	} else {
		t.Logf("vault_search OK: %d results, vault_size=%v, pass2_fell_back=%v",
			len(results), payload["vault_size"], payload["pass2_fell_back"])
		first := results[0].(map[string]any)
		t.Logf("  top: %v (%v)", first["path"], first["title"])
	}
}

// TestKnowledgeSmoke_KiwixSearch runs kiwix_search against the live kiwix.
// Skipped when kiwix is down.
func TestKnowledgeSmoke_KiwixSearch(t *testing.T) {
	if !kiwixUp() {
		t.Skip("kiwix-serve not reachable at localhost:8889")
	}
	binary := serverBinary(t)
	db := smokeDB()
	payload, err := knowledgeCall(t, binary, db, "mcp-servers", "kiwix_search", map[string]any{
		"zim_id":  "devdocs_en_rust_2026-04",
		"pattern": "trait object",
		"limit":   5,
	})
	if err != nil {
		t.Fatalf("kiwix_search: %v", err)
	}
	if errMsg, ok := payload["error"].(string); ok {
		t.Fatalf("kiwix_search returned error: %s", errMsg)
	}
	for _, key := range []string{"hits", "qwen_fell_back", "hits_in", "hits_out"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("kiwix_search missing key %q (response=%v)", key, payload)
		}
	}
	hits, _ := payload["hits"].([]any)
	if len(hits) == 0 {
		t.Errorf("kiwix_search returned zero hits for an on-topic query; payload=%v", payload)
	} else {
		t.Logf("kiwix_search OK: hits_in=%v hits_out=%v qwen_fell_back=%v",
			payload["hits_in"], payload["hits_out"], payload["qwen_fell_back"])
	}
}

// TestKnowledgeSmoke_LibraryRoundtrip exercises library_add → library_get →
// library_retire on a throwaway dewey number. Uses a smoke-only project to
// avoid polluting real project libraries.
func TestKnowledgeSmoke_LibraryRoundtrip(t *testing.T) {
	binary := serverBinary(t)
	db := smokeDB()
	// Use a unique dewey to avoid clashes if the test re-runs without cleanup.
	dewey := fmt.Sprintf("999.%d", time.Now().Unix()%99999)
	project := "knowledge-smoke-test"

	// Add.
	add, err := knowledgeCall(t, binary, db, project, "library_add", map[string]any{
		"dewey":           dewey,
		"primary_author":  "Smoke",
		"year":            2026,
		"citation_raw":    "Smoke, T. (2026). Throwaway test entry.",
		"establishes":     "Smoke-test verification of library_add roundtrip.",
		"what_it_answers": "Does library_add work end-to-end?",
		"invoke_when":     "during the knowledge smoke test only",
	})
	if err != nil {
		t.Fatalf("library_add: %v", err)
	}
	if errMsg, ok := add["error"].(string); ok {
		t.Fatalf("library_add returned error: %s", errMsg)
	}
	if add["ok"] != true || add["dewey"] != dewey {
		t.Errorf("library_add unexpected payload: %v", add)
	}

	// Get.
	got, err := knowledgeCall(t, binary, db, project, "library_get", map[string]any{"dewey": dewey})
	if err != nil {
		t.Fatalf("library_get: %v", err)
	}
	if errMsg, ok := got["error"].(string); ok {
		t.Fatalf("library_get returned error: %s", errMsg)
	}
	if got["dewey"] != dewey {
		t.Errorf("library_get unexpected dewey: %v", got["dewey"])
	}
	citation, _ := got["citation"].(map[string]any)
	if citation == nil || citation["primary_author"] != "Smoke" {
		t.Errorf("library_get unexpected citation: %v", got["citation"])
	}
	t.Logf("library_get OK: dewey=%s author=%v year=%v", dewey, citation["primary_author"], citation["year"])

	// Retire (cleanup; harmless on a fresh DB if it can't run).
	retire, err := knowledgeCall(t, binary, db, project, "library_retire", map[string]any{
		"dewey":  dewey,
		"reason": "smoke-test cleanup",
	})
	if err != nil {
		t.Fatalf("library_retire: %v", err)
	}
	if retire["ok"] != true {
		t.Errorf("library_retire payload: %v", retire)
	}
}

// TestKnowledgeSmoke_KnowledgeSearch runs the unified FTS5 + Qwen rerank flow
// end-to-end against the live DB. Asserts the response shape; tolerates an
// empty result set (the index may not contain rows on every project).
func TestKnowledgeSmoke_KnowledgeSearch(t *testing.T) {
	binary := serverBinary(t)
	db := smokeDB()
	payload, err := knowledgeCall(t, binary, db, "mcp-servers", "knowledge_search", map[string]any{
		"query": "go migration parity vault search",
		"top_k": 5,
	})
	if err != nil {
		t.Fatalf("knowledge_search: %v", err)
	}
	if errMsg, ok := payload["error"].(string); ok {
		t.Fatalf("knowledge_search returned error: %s", errMsg)
	}
	for _, key := range []string{"results", "results_count", "query"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("knowledge_search missing key %q (response=%v)", key, payload)
		}
	}
	count, _ := payload["results_count"].(float64)
	if count > 0 {
		t.Logf("knowledge_search OK: %v results, qwen_fell_back=%v", count, payload["qwen_fell_back"])
		results, _ := payload["results"].([]any)
		first := results[0].(map[string]any)
		// Verify hit shape carries the expected source-attribution fields.
		for _, key := range []string{"id", "source_type", "source_ref", "question", "invoke_when", "usage_count", "negative_feedback_count"} {
			if _, ok := first[key]; !ok {
				t.Errorf("knowledge_search hit missing key %q (hit=%v)", key, first)
			}
		}
	} else {
		// Empty corpus is acceptable in smoke — we still validated shape.
		t.Logf("knowledge_search returned 0 results (corpus may be empty); shape OK")
	}
}
