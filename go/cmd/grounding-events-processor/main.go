// Command grounding-events-processor reads Claude Code session JSONL files and
// records grounding_events (+ click_kind interactions + terminal resolutions) for
// each vault_search / kiwix_search / knowledge_search call found in the session.
//
// Invoked by the user-level Stop hook at
// ~/.claude/hooks/grounding-events-processor.sh on every session-end.
//
// Two modes:
//
//	--http-base http://localhost:3001   parse host-side, POST the parsed grounding
//	                                    to the container's ingest_grounding action
//	                                    (the post-cutover SINGLE-WRITER path; default
//	                                    for the Stop hook).
//	--db <path>                         open the DB directly and write locally. Only
//	                                    safe single-writer (e.g. a container-down
//	                                    one-shot or a backfill while nothing else
//	                                    holds the file). Opening the canonical DB this
//	                                    way while the container runs is the cross-
//	                                    mount-namespace WAL hazard.
//
// The parse + emit logic lives in internal/grounding so both this binary and the
// container action share one implementation (no cross-process drift).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/grounding"
	"toolkit/internal/projections"
	"toolkit/internal/telemetry"
)

const defaultDB = "/home/user/.local/share/toolkit/data/toolkit.db"

func main() {
	var (
		sessionPath             string
		dirPath                 string
		projectID               string
		dbPath                  string
		httpBase                string
		parentSpanID            string
		preserveTranscriptTimes bool
	)
	flag.StringVar(&sessionPath, "session", "", "single session JSONL file to process")
	flag.StringVar(&dirPath, "dir", "", "directory of session JSONL files to process")
	flag.StringVar(&projectID, "project-id", "", "project_id stamped onto every grounding_events row (inferred from --dir name when omitted)")
	flag.StringVar(&dbPath, "db", defaultDB, "path to toolkit.db — used only in --db fallback mode (single-writer; container-down one-shot)")
	flag.StringVar(&httpBase, "http-base", "", "when set (e.g. http://localhost:3001), POST parsed grounding to the container's ingest_grounding action instead of opening the DB directly (the post-cutover single-writer path)")
	flag.StringVar(&parentSpanID, "parent-span-id", "",
		"parent agent's span_id stamped on every grounding_events row in this run (sidechain → parent linkage). Empty leaves parent_span_id NULL.")
	flag.BoolVar(&preserveTranscriptTimes, "preserve-transcript-timestamps", false,
		"backfill mode: use each tool_use's transcript `timestamp` as the row's created_at rather than now(). Off in normal Stop-hook mode.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  grounding-events-processor --http-base <url> --session <path.jsonl> [--project-id <id>] [--parent-span-id <id>] [--preserve-transcript-timestamps]")
		fmt.Fprintln(os.Stderr, "  grounding-events-processor --db <path>     --dir <directory>      [--project-id <id>] (single-writer fallback)")
	}
	flag.Parse()

	files, err := collectFiles(sessionPath, dirPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		flag.Usage()
		os.Exit(1)
	}

	if projectID == "" {
		if dirPath != "" {
			projectID = grounding.InferProjectID(dirPath)
		}
		if projectID == "" {
			projectID = "unknown"
		}
	}

	ctx := context.Background()
	var totalEvents, totalGaps, totalInteractions, totalResolutions int
	tally := func(path string, r grounding.Result) {
		if r.Events > 0 || r.Interactions > 0 || r.Resolutions > 0 {
			fmt.Printf("%s: %d search call(s), %d gap(s), %d interaction(s), %d resolution(s)\n",
				filepath.Base(path), r.Events, r.Gaps, r.Interactions, r.Resolutions)
		}
		totalEvents += r.Events
		totalGaps += r.Gaps
		totalInteractions += r.Interactions
		totalResolutions += r.Resolutions
	}

	if httpBase != "" {
		// HTTP mode (default for the Stop hook): parse host-side, POST to the
		// container's ingest_grounding action so the container's single writer does
		// the emit + projection fold.
		for _, path := range files {
			r, perr := postGrounding(ctx, httpBase, projectID, parentSpanID, preserveTranscriptTimes, path)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", path, perr)
				continue
			}
			tally(path, r)
		}
	} else {
		// --db fallback: open the DB and write locally. ONLY safe as a single writer.
		pool, derr := db.Open(dbPath)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "open DB at %s: %v\n", dbPath, derr)
			os.Exit(2)
		}
		defer pool.Close()
		// Wire the read-side fold hook so EmitInteraction/EmitResolution fold the
		// query_* projections in the same tx (the container wires this at startup).
		telemetry.SetFoldHook(projections.FoldAllReadSide)
		for _, path := range files {
			r, perr := grounding.ProcessFile(ctx, pool, path, projectID, parentSpanID, preserveTranscriptTimes)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", path, perr)
				continue
			}
			tally(path, r)
		}
	}

	fmt.Printf("\nDone: %d file(s) | %d search events | %d zero-result gaps | %d interactions | %d resolutions\n",
		len(files), totalEvents, totalGaps, totalInteractions, totalResolutions)
}

// ingestEnvelope is the /mcp/work request body for the ingest_grounding action.
type ingestEnvelope struct {
	Action  string                  `json:"action"`
	Project string                  `json:"project"`
	Params  grounding.IngestRequest `json:"params"`
}

// postGrounding parses path host-side (no DB) and POSTs the events+entries to the
// container's ingest_grounding action at <httpBase>/mcp/work. Returns the action's
// Result. The MCP HTTP surface wraps the typed result in a CallToolResult envelope
// ({content:[{text:"<json>"}]}) or returns it bare — both are handled.
func postGrounding(ctx context.Context, httpBase, project, parentSpanID string, preserve bool, path string) (grounding.Result, error) {
	events, entries, err := grounding.Parse(path)
	if err != nil {
		return grounding.Result{}, err
	}
	body, err := json.Marshal(ingestEnvelope{
		Action:  "ingest_grounding",
		Project: project,
		Params: grounding.IngestRequest{
			ParentSpanID:            parentSpanID,
			PreserveTranscriptTimes: preserve,
			Events:                  events,
			Entries:                 entries,
		},
	})
	if err != nil {
		return grounding.Result{}, err
	}
	url := strings.TrimRight(httpBase, "/") + "/mcp/work"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return grounding.Result{}, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return grounding.Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return grounding.Result{}, fmt.Errorf("ingest_grounding HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	var r grounding.Result
	if json.Unmarshal(raw, &env) == nil && len(env.Content) > 0 && env.Content[0].Text != "" {
		_ = json.Unmarshal([]byte(env.Content[0].Text), &r)
	} else {
		_ = json.Unmarshal(raw, &r)
	}
	return r, nil
}

func collectFiles(sessionPath, dirPath string) ([]string, error) {
	switch {
	case sessionPath != "" && dirPath != "":
		return nil, fmt.Errorf("--session and --dir are mutually exclusive")
	case sessionPath != "":
		return []string{sessionPath}, nil
	case dirPath != "":
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			return nil, fmt.Errorf("read --dir %s: %w", dirPath, err)
		}
		var out []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if filepath.Ext(name) != ".jsonl" {
				continue
			}
			out = append(out, filepath.Join(dirPath, name))
		}
		sort.Strings(out)
		return out, nil
	default:
		return nil, fmt.Errorf("one of --session or --dir is required")
	}
}
