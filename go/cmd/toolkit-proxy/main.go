// Command toolkit-proxy is the stdio→HTTP bridge that lets a Claude Code
// session reach the containerized toolkit-server WITHOUT opening the
// canonical SQLite DB itself.
//
// Why this exists (chain auto-startup-dev-services T7, "the CRUX"): every
// session's .mcp.json historically mounted the toolkit as a stdio command
// running the native binary with --db <canonical>, so each session opened
// the canonical SQLite file directly. That is safe only while every opener
// is a same-host process under POSIX advisory locks. To make the toolkit
// CONTAINER the single owner of the canonical DB (no cross-mount-namespace
// WAL hazard), the per-session access path must stop opening the file. This
// proxy is the replacement .mcp.json command: it speaks MCP stdio to Claude
// Code and forwards every tool call to the container's POST /mcp/<surface>
// HTTP route. It holds NO database handle, runs no migrations, and starts
// no background jobs — the container is the sole DB opener.
//
// Fidelity: the proxy registers the SAME surface meta-tools as the
// native server (work/measure/knowledge/admin/ml/sys — fs retired in T6), with
// the SAME descriptions (actiondocs.*Description) and the SAME input schema
// (dispatch.MetaToolInputSchema), imported from this module so they cannot
// drift. The stdio handshake's per-session attribution (actor, session id)
// and the per-session --default-project are forwarded to the container as
// X-MCP-Actor / X-MCP-Session / X-MCP-Default-Project headers, which the
// HTTP /mcp route stamps back onto the dispatch context (see
// internal/observehttp/mcp_dispatch.go). Per-call rationale rides inside the
// JSON body (dispatch.Args.Rationale), so it travels for free.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolkit/internal/actiondocs"
	"toolkit/internal/dispatch"
)

// surface pairs a meta-tool name with its description. The input schema is
// identical across surfaces (dispatch.MetaToolInputSchema), so it is not
// stored here.
type surface struct {
	name        string
	description string
}

// surfaces is the full set the native toolkit-server registers as stdio
// meta-tools (cmd/toolkit-server/main.go). Keep in lockstep: a surface the
// proxy omits is simply unreachable from a proxied session.
// gitSHA and builtAtUnix are populated at build time via -ldflags -X (see
// scripts/install-proxy.sh), mirroring toolkit-server's stamp. They let a
// staleness check compare the INSTALLED proxy against the repo HEAD — the proxy is
// a hand-installed SPOF (~/.local/bin/toolkit-proxy) every .mcp.json depends on, so
// detectable drift is the whole point (chain finish-sophdn-repo-split T5). Defaults
// mark an un-stamped build (a bare `go build` without the ldflags).
var (
	gitSHA      = "unversioned"
	builtAtUnix = "0"
)

var surfaces = []surface{
	{"work", actiondocs.WorkDescription},
	{"measure", actiondocs.MeasureDescription},
	{"knowledge", actiondocs.KnowledgeDescription},
	{"admin", actiondocs.AdminDescription},
	{"ml", actiondocs.MLDescription},
	// fs retired from the toolkit (T6) — corpos owns it natively, Claude Code
	// uses its own Read/Write/Edit. sys stays (introspection only).
	{"sys", actiondocs.SysDescription},
	{"ecosystem", actiondocs.EcosystemDescription},
}

func main() {
	var httpBase, defaultProject string
	var showVersion bool
	flag.StringVar(&httpBase, "http-base", envOr("TOOLKIT_HTTP_BASE", "http://localhost:3001"),
		"base URL of the containerized toolkit-server HTTP surface (POST {base}/mcp/<surface>)")
	flag.StringVar(&defaultProject, "default-project", "",
		"per-session default project, forwarded as X-MCP-Default-Project (mirrors the native binary's --default-project)")
	flag.BoolVar(&showVersion, "version", false,
		"print the build SHA (the ldflags-stamped gitSHA) + build time and exit — the staleness-check input")
	flag.Parse()

	// -version prints to STDOUT and exits BEFORE the stdio JSON-RPC server starts,
	// so it is safe to read from a script (scripts/check-proxy-staleness.sh). Once
	// the server is running, stdout is the JSON-RPC stream and must carry nothing else.
	if showVersion {
		fmt.Printf("toolkit-proxy %s built %s\n", gitSHA, builtAtUnix)
		return
	}

	httpBase = strings.TrimRight(httpBase, "/")

	// stderr-only logging: stdout is the JSON-RPC stream to Claude Code and
	// must carry nothing else.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client := &http.Client{Timeout: 120 * time.Second}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "toolkit-server",
		Version: gitSHA, // the ldflags-stamped build SHA, so the handshake reports drift
	}, nil)

	metaSchema := dispatch.MetaToolInputSchema()
	for _, s := range surfaces {
		s := s // capture per iteration for the closure
		mcp.AddTool(server, &mcp.Tool{
			Name:        s.name,
			Description: s.description,
			InputSchema: metaSchema,
		}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
			return forward(ctx, client, httpBase, defaultProject, s.name, req, args, logger)
		})
	}

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Error("proxy server error", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// forward marshals the dispatch.Args, POSTs them to the container's
// /mcp/<surface> route with the per-session attribution headers, and wraps
// the response body back into a CallToolResult. The HTTP route returns the
// UNWRAPPED action JSON (its mcpDispatch unwraps the TextContent before
// writing), so the body is re-wrapped here into the single-TextContent shape
// every dispatch result uses (dispatch.jsonResult), which is exactly what a
// native stdio handler would have returned.
func forward(
	ctx context.Context,
	client *http.Client,
	httpBase, defaultProject, surfaceName string,
	req *mcp.CallToolRequest,
	args dispatch.Args,
	logger *slog.Logger,
) (*mcp.CallToolResult, any, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal args: %w", err)
	}

	url := httpBase + "/mcp/" + surfaceName
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if actor := actorID(req); actor != "" {
		httpReq.Header.Set("X-MCP-Actor", actor)
	}
	if sess := sessionID(req); sess != "" {
		httpReq.Header.Set("X-MCP-Session", sess)
	}
	if defaultProject != "" {
		httpReq.Header.Set("X-MCP-Default-Project", defaultProject)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		// Transport failure (container down, network) — surface it as a
		// tool error so the session sees a clear signal rather than a
		// silent empty result. The native fallback path (CLAUDE.md) is the
		// recovery.
		return nil, nil, fmt.Errorf("reach toolkit container at %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	// 200 is the normal path: the body is the action-specific JSON (success
	// shape OR the {error: ...} envelope for policy rejects / not-implemented
	// — both of which the native stdio path also returns as TextContent).
	// Non-200 is a transport-level rejection (404 unknown surface, 400 bad
	// body, 503 not ready, 500 dispatch error); pass the body through under a
	// tool error so the discriminating shape is still visible.
	if resp.StatusCode != http.StatusOK {
		logger.Warn("toolkit container returned non-200",
			slog.String("surface", surfaceName),
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(respBody)))
		return nil, nil, fmt.Errorf("toolkit container %s -> HTTP %d: %s", surfaceName, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(respBody)}},
	}, nil, nil
}

// actorID mirrors the native server's stampMCPActor: derive a stable agent
// identity from the MCP initialize handshake's ClientInfo. Returns "" when
// the handshake is missing so forward() omits the header and the container
// falls back to its dispatch-default actor.
func actorID(req *mcp.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return ""
	}
	if init := req.Session.InitializeParams(); init != nil && init.ClientInfo != nil {
		ci := init.ClientInfo
		switch {
		case ci.Name != "" && ci.Version != "":
			return ci.Name + "-" + ci.Version
		case ci.Name != "":
			return ci.Name
		}
	}
	return "unknown-stdio-client"
}

// sessionID mirrors the native server's stampMCPSessionID: use the
// transport session id, falling back to the stable per-connection pointer
// for stdio transports (which report no id).
func sessionID(req *mcp.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return ""
	}
	if id := req.Session.ID(); id != "" {
		return id
	}
	return fmt.Sprintf("stdio-%p", req.Session)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
