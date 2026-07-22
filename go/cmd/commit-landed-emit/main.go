// Command commit-landed-emit is the one-shot tool that lands a
// CommitLanded event after every git commit. Invoked from
// scripts/post-commit-restart-advisor.sh per chain
// arc-close-filing-review-substrate-listener-wiring T6.
//
// IMPORTANT: this binary POSTs to the daemon's HTTP MCP endpoint
// (work.emit_commit_landed) rather than opening its own DB pool. The
// reason is fold-hook locality — events.SetFoldHook installs the
// SubstrateReviewObserver in the daemon's process, NOT in this
// one-shot binary's process. An emit done through a binary-owned pool
// would land the row but bypass the listener that triggers the
// substrate-side review. Routing through the daemon keeps the
// goroutine-fires-review pipeline intact.
//
// Reads commit metadata directly from `git` (so no env-var
// assumptions), builds the typed payload, posts to /mcp/work, and
// exits. Fail-open: every error path logs to stderr and exits 0 (the
// post-commit advisor must NEVER block the commit). Exit code 1 is
// reserved for argument-parsing failures since those indicate a
// wiring bug rather than runtime conditions.
//
// Usage:
//
//	commit-landed-emit --project mcp-servers
//	commit-landed-emit --project mcp-servers --port 3000
//	commit-landed-emit --project mcp-servers --sha HEAD
//
// The advisor invokes the binary without --sha (defaults to HEAD,
// which post-commit guarantees points at the new commit) and without
// --port (defaults to the TOOLKIT_HTTP_PORT env var or 3000).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	port := flag.String("port", "", "toolkit HTTP daemon port (default $TOOLKIT_HTTP_PORT or 3000)")
	project := flag.String("project", "", "project_id for the entity reference (required)")
	sha := flag.String("sha", "HEAD", "commit ref to inspect (default HEAD)")
	flag.Parse()
	if *project == "" {
		log.Printf("commit-landed-emit: --project is required")
		os.Exit(1)
	}
	resolvedPort := *port
	if resolvedPort == "" {
		resolvedPort = os.Getenv("TOOLKIT_HTTP_PORT")
	}
	if resolvedPort == "" {
		resolvedPort = "3000"
	}

	payload, err := gatherCommitPayload(*sha)
	if err != nil {
		log.Printf("commit-landed-emit: gather commit details: %v — fail-open", err)
		return
	}

	if err := postEmit(resolvedPort, *project, payload); err != nil {
		log.Printf("commit-landed-emit: post: %v — fail-open", err)
		return
	}
}

// commitPayload mirrors arcreview.EmitCommitLandedParams JSON shape so
// the binary can construct the request body without importing the
// arcreview package (the binary's job is purely message-passing).
type commitPayload struct {
	CommitSHA         string  `json:"commit_sha"`
	Branch            *string `json:"branch,omitempty"`
	FilesChangedCount *int    `json:"files_changed_count,omitempty"`
	Author            *string `json:"author,omitempty"`
	Subject           *string `json:"subject,omitempty"`
}

func gatherCommitPayload(ref string) (commitPayload, error) {
	fullSHA, err := gitOutput("rev-parse", ref)
	if err != nil {
		return commitPayload{}, fmt.Errorf("rev-parse %s: %w", ref, err)
	}
	payload := commitPayload{CommitSHA: fullSHA}

	if branch, err := gitOutput("rev-parse", "--abbrev-ref", "HEAD"); err == nil && branch != "" && branch != "HEAD" {
		payload.Branch = &branch
	}
	if stat, err := gitOutput("show", "--stat", "--format=", fullSHA); err == nil {
		if n, ok := parseFilesChanged(stat); ok {
			payload.FilesChangedCount = &n
		}
	}
	if author, err := gitOutput("show", "-s", "--format=%an <%ae>", fullSHA); err == nil && author != "" {
		payload.Author = &author
	}
	if subj, err := gitOutput("show", "-s", "--format=%s", fullSHA); err == nil && subj != "" {
		payload.Subject = &subj
	}
	return payload, nil
}

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// parseFilesChanged extracts the integer count from the `--stat` summary
// line `N files changed, ...`. Returns (count, true) on success;
// (0, false) when the line isn't parseable so the caller can leave
// FilesChangedCount nil and surface NULL in the schema.
func parseFilesChanged(stat string) (int, bool) {
	for _, line := range strings.Split(stat, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "file changed") && !strings.Contains(line, "files changed") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}

// mcpRequest mirrors the dispatch.Args JSON shape the HTTP MCP endpoint
// expects. Typed so the binary stays free of bare-any.
type mcpRequest struct {
	Action  string        `json:"action"`
	Project string        `json:"project"`
	Params  commitPayload `json:"params"`
}

// postEmit sends the emit_commit_landed request to the daemon. The
// request is best-effort; transport errors retry up to ~5s (the
// post-commit advisor relaunches the HTTP daemon asynchronously via
// nohup, so a fresh-daemon-not-yet-bound window of ~1-2s is normal).
// After retries exhaust, the caller logs the error and absorbs it
// (the advisor must not block the commit on a daemon glitch).
func postEmit(port, project string, payload commitPayload) error {
	body, err := json.Marshal(mcpRequest{
		Action:  "emit_commit_landed",
		Project: project,
		Params:  payload,
	})
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/mcp/work", port)
	client := &http.Client{Timeout: 3 * time.Second}

	// Retry schedule: 10 attempts at 500ms intervals = ~5s total wall
	// time. Sized to cover the typical post-restart daemon boot window
	// (~1-2s observed locally) plus headroom for slow boots.
	const maxAttempts = 10
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("post status %d", resp.StatusCode)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return nil
	}
	return fmt.Errorf("post after %d attempts: %w", maxAttempts, lastErr)
}
