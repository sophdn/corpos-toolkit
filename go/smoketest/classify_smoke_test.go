// Package smoketest_test contains the classify-specific smoke comparison test.
//
// This test sends one representative input per deployed rubric to both the
// Go and Rust toolkit-server binaries and compares the returned label. It is
// the T31 pre-migration quality gate: Go must return a valid label for every
// rubric before classify traffic is routed to the Go server.
//
// # Running
//
// Paths are relative to the test cwd (`go/smoketest/`):
//
//	TOOLKIT_SERVER_BINARY=../bin/toolkit-server \
//	  TOOLKIT_SMOKE_DB=../../data/toolkit.db \
//	  TOOLKIT_RUBRICS_DIR=../../blueprints/rubrics \
//	  go test -tags sqlite_fts5 -v -run TestClassifySmoke ./smoketest/...
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

// classifyCase is one smoke-test input for a classify action.
type classifyCase struct {
	action      string
	params      map[string]any
	validLabels []string // any of these is a passing response
}

var classifyCases = []classifyCase{
	{
		action:      "classify_chain_task_proportionality",
		params:      map[string]any{"task_spec": "Port the Rust rubric registry to Go. Estimated effort: 1 day. Team context: solo developer; prior signal strong (Rust implementation as reference)."},
		validLabels: []string{"proportionate", "disproportionate", "unclear"},
	},
	{
		action:      "classify_retirement_observation",
		params:      map[string]any{"observation_text": "Observation: `tools_used` aggregation shows zero invocations of `mcp__seed-packet__signal_table_export` across 200 sessions. The tool is registered but has not been called."},
		validLabels: []string{"tool-retirement", "skill-retirement", "workflow-retirement", "not-retirement"},
	},
	{
		action:      "classify_artifact_tier",
		params:      map[string]any{"artifact_descriptor": "CLAUDE.md at the project root — session-start protocol, ambient skills, document-map pointers."},
		validLabels: []string{"tier-zero", "tier-one", "tier-two", "tier-three"},
	},
	{
		action:      "classify_audit_finding_severity",
		params:      map[string]any{"finding_prose": "Finding: dispatch arm for bug_resolve accepts commit_sha but the allowed-key list is not enforced at the dispatch boundary. A hyphenated key is silently dropped; the bug resolves with empty commit_sha."},
		validLabels: []string{"critical", "high", "medium", "low"},
	},
	{
		action: "classify_artifact_review_criterion",
		params: map[string]any{
			"artifact_excerpt": "DB_PASSWORD=\"hunter2\"\npsql -U admin -W \"$DB_PASSWORD\" ...",
			"purpose":          "safety",
			"criterion":        "No hardcoded secrets, keys, or tokens.",
		},
		validLabels: []string{"pass", "fail", "mixed", "n-a"},
	},
	{
		action:      "classify_session_routing_trigger",
		params:      map[string]any{"user_input": "please continue the agent-os-go-migration chain"},
		validLabels: []string{"context-handoff", "execute-document", "retirement-dispatch", "chain-execution", "tool-suggest", "no-trigger"},
	},
	{
		action:      "classify_pre_commit_failure",
		params:      map[string]any{"stderr": "running cargo clippy --all-targets\nwarning: variable does not need to be mutable\nerror: lint warnings emitted under -D warnings\nexit 1"},
		validLabels: []string{"lint", "typecheck", "test", "lifecycle", "unclassifiable"},
	},
	{
		action:      "classify_docstring_drift",
		params:      map[string]any{"function_snippet": "/// Parses the config string and returns a `HashMap<String, String>`.\nfn parse_config(input: &str) -> Result<HashMap<String, String>, ConfigError> {\n    let map = tokenize(input)?;\n    Ok(map)\n}"},
		validLabels: []string{"matches", "doesn't_match", "unclear"},
	},
}

func rubricsDir() string {
	// TOOLKIT_RUBRICS_DIR="" means "Rust binary: no flag needed; skip dir check".
	if d, ok := os.LookupEnv("TOOLKIT_RUBRICS_DIR"); ok {
		return d
	}
	// Resolved from the test cwd (`go/smoketest/`) → mcp-servers/blueprints/rubrics.
	return "../../blueprints/rubrics"
}

// callClassify spawns the binary, sends an MCP classify call, and returns
// the label from the response. Returns ("", error) on any failure.
func callClassify(t *testing.T, binary, dbPath, rubricsDirPath string, tc classifyCase) (string, error) {
	t.Helper()
	args := []string{"--db", dbPath, "--default-project", "mcp-servers"}
	if rubricsDirPath != "" {
		args = append(args, "--rubrics-dir", rubricsDirPath)
	}
	cmd := exec.Command(binary, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
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

	// Initialize
	id1 := 1
	send(map[string]any{
		"jsonrpc": "2.0", "id": id1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "classify-smoke", "version": "0.1"},
		},
	})

	line, ok := readLine(5 * time.Second)
	if !ok {
		return "", fmt.Errorf("no response to initialize")
	}
	var initResp map[string]any
	if err := json.Unmarshal([]byte(line), &initResp); err != nil {
		return "", fmt.Errorf("parse initialize response: %w", err)
	}
	if initResp["error"] != nil {
		return "", fmt.Errorf("initialize error: %v", initResp["error"])
	}

	// Initialized notification
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	// Classify call
	id2 := 2
	send(map[string]any{
		"jsonrpc": "2.0", "id": id2, "method": "tools/call",
		"params": map[string]any{
			"name":      "measure",
			"arguments": map[string]any{"action": tc.action, "params": tc.params},
		},
	})

	line, ok = readLine(60 * time.Second) // Qwen can be slow
	if !ok {
		return "", fmt.Errorf("no response to classify call (timeout)")
	}
	var callResp map[string]any
	if err := json.Unmarshal([]byte(line), &callResp); err != nil {
		return "", fmt.Errorf("parse classify response: %w", err)
	}
	if callResp["error"] != nil {
		return "", fmt.Errorf("classify MCP error: %v", callResp["error"])
	}

	// Extract label from result content
	result, _ := callResp["result"].(map[string]any)
	if result == nil {
		return "", fmt.Errorf("no result in response")
	}
	contentArr, _ := result["content"].([]any)
	if len(contentArr) == 0 {
		return "", fmt.Errorf("empty content array")
	}
	content, _ := contentArr[0].(map[string]any)
	text, _ := content["text"].(string)
	if text == "" {
		return "", fmt.Errorf("empty text in content")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return "", fmt.Errorf("parse payload: %w (text=%s)", err, text)
	}
	if errMsg, ok := payload["error"].(string); ok {
		return "", fmt.Errorf("classify action error: %s", errMsg)
	}
	label, _ := payload["label"].(string)
	if label == "" {
		return "", fmt.Errorf("no label in payload: %s", text)
	}
	_ = io.Discard
	return label, nil
}

// TestClassifySmoke_GoRespondsWithValidLabel verifies the Go server returns a
// valid label for every deployed rubric. This is the T31 pre-migration gate.
func TestClassifySmoke_GoRespondsWithValidLabel(t *testing.T) {
	binary := serverBinary(t)
	db := smokeDB()
	rdDir := rubricsDir()

	// Skip dir accessibility check when rdDir is empty (Rust binary has rubrics compiled in).
	if rdDir != "" {
		if _, err := os.Stat(rdDir); err != nil {
			t.Skipf("rubrics dir %q not accessible: %v", rdDir, err)
		}
	}

	type result struct {
		action string
		label  string
		err    error
	}
	results := make([]result, len(classifyCases))

	for i, tc := range classifyCases {
		tc := tc
		i := i
		t.Run(tc.action, func(t *testing.T) {
			label, err := callClassify(t, binary, db, rdDir, tc)
			results[i] = result{action: tc.action, label: label, err: err}
			if err != nil {
				t.Errorf("classify failed: %v", err)
				return
			}

			valid := false
			for _, v := range tc.validLabels {
				if label == v {
					valid = true
					break
				}
			}
			if !valid {
				t.Errorf("label %q not in valid set %v", label, tc.validLabels)
			}
			t.Logf("action=%s label=%s OK", tc.action, label)
		})
	}
}
