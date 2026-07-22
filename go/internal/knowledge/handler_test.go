package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
	"toolkit/internal/testutil"
)

// Build a temp vault populated with three fixture notes. Returns the canonical
// root path. Hermetic: never touches ~/.claude/vault.
func tempVault(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"decisions", "learnings/general", "reference"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	must := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("decisions/alpha.md",
		"---\ntitle: Alpha decision\ntags: [decisions, retrieval]\n---\n\nAlpha body text.\n")
	must("learnings/general/beta.md",
		"---\ntitle: Beta learning\ntags: [retrieval]\n---\n\nBeta body — discusses retrieval semantics in depth.\n")
	must("reference/gamma.md",
		"# Gamma reference\n\nGamma body content about something unrelated.\n")
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// mockLlama returns an httptest.Server that responds to /v1/chat/completions
// with the queued JSON-content responses. Returns the server and a pointer to
// the captured request-body slice (in call order).
func mockLlama(t *testing.T, responses []string) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	idx := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		text := responses[idx]
		if idx < len(responses)-1 {
			idx++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": text}},
			},
			"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 25},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func depsFor(t *testing.T, llamaURL, vaultRoot string) Deps {
	t.Helper()
	r := router.NewWithClients(llamacpp.New(llamaURL), nil, "qwen2.5-32b")
	pool := testutil.NewTestDB(t)
	return Deps{Pool: pool, Router: r, VaultRoot: vaultRoot}
}

// ── HandleVaultSearch ──────────────────────────────────────────────────

func TestHandleVaultSearch_MissingQueryReturnsError(t *testing.T) {
	deps := depsFor(t, "http://127.0.0.1:1", tempVault(t))
	resp, err := HandleVaultSearch(context.Background(), deps, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(resp.Error, "query is required") {
		t.Errorf("expected query-required error, got %q", resp.Error)
	}
}

func TestHandleVaultSearch_SinglePass_ReturnsRankedResults(t *testing.T) {
	root := tempVault(t)
	// One-result pass-1 → pass-2 skipped (<2 results, surfaces unchanged).
	srv, _ := mockLlama(t, []string{"decisions/alpha.md"})
	deps := depsFor(t, srv.URL, root)
	params, _ := json.Marshal(map[string]any{"query": "alpha", "top_k": 3, "vault_root": root})
	resp, err := HandleVaultSearch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d (%+v)", len(resp.Results), resp.Results)
	}
	if resp.Results[0].Path != "decisions/alpha.md" {
		t.Errorf("path wrong: %q", resp.Results[0].Path)
	}
	if resp.Results[0].Title != "Alpha decision" {
		t.Errorf("title wrong: %q", resp.Results[0].Title)
	}
	if resp.Pass2FellBack {
		t.Errorf("pass2_fell_back should be false on single-result pass-1")
	}
	if resp.VaultSize != 3 {
		t.Errorf("vault_size should be 3, got %d", resp.VaultSize)
	}
}

func TestHandleVaultSearch_TwoPass_RanksByPass2(t *testing.T) {
	root := tempVault(t)
	// Pass-1: two results. Pass-2: re-ranks them (beta first).
	srv, bodies := mockLlama(t, []string{
		"decisions/alpha.md\nlearnings/general/beta.md",
		"learnings/general/beta.md\ndecisions/alpha.md",
	})
	deps := depsFor(t, srv.URL, root)
	params, _ := json.Marshal(map[string]any{"query": "retrieval", "top_k": 5, "vault_root": root})
	resp, err := HandleVaultSearch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].Path != "learnings/general/beta.md" {
		t.Errorf("pass-2 must rank beta first; got %q", resp.Results[0].Path)
	}
	if resp.Pass2FellBack {
		t.Errorf("pass2_fell_back must be false on non-empty pass-2")
	}
	// Parity-pin: pass-2 prompt must include body excerpts (YAML-indented).
	if len(*bodies) < 2 || !strings.Contains((*bodies)[1], "body: |") {
		t.Errorf("pass-2 prompt must include body block; got %q", (*bodies)[1])
	}
}

// Parity-pin: empty pass-2 over-rejection falls back to pass-1 with
// pass2_fell_back=true; the search still returns useful results rather than
// pretending the vault is empty.
func TestHandleVaultSearch_Pass2EmptyFallsBack(t *testing.T) {
	root := tempVault(t)
	srv, _ := mockLlama(t, []string{
		"decisions/alpha.md\nlearnings/general/beta.md",
		"no match",
	})
	deps := depsFor(t, srv.URL, root)
	params, _ := json.Marshal(map[string]any{"query": "retrieval", "vault_root": root})
	resp, _ := HandleVaultSearch(context.Background(), deps, params)
	if !resp.Pass2FellBack {
		t.Errorf("expected fall-back, got pass2_fell_back=%v", resp.Pass2FellBack)
	}
	if len(resp.Results) != 2 {
		t.Errorf("expected 2 fall-back results, got %d", len(resp.Results))
	}
}

// DB-row verification (PARITY_STANDARD §2c). After a vault_search, exactly one
// grounding_events row with action='vault_search' must exist; pass1/pass2
// latency columns from migration 046 (chain telemetry-substrate-cleanup T2)
// must reflect the call's two-pass shape.
func TestHandleVaultSearch_WritesTelemetryRow(t *testing.T) {
	root := tempVault(t)
	srv, _ := mockLlama(t, []string{"decisions/alpha.md"})
	deps := depsFor(t, srv.URL, root)
	params, _ := json.Marshal(map[string]any{"query": "alpha", "top_k": 3, "vault_root": root})
	if _, err := HandleVaultSearch(context.Background(), deps, params); err != nil {
		t.Fatalf("call: %v", err)
	}

	var (
		queryText *string
		action    string
		results   int64
		pass1MS   *int64
		pass2MS   *int64
	)
	row := deps.Pool.DB().QueryRow(
		`SELECT query_text, action, results_count,
		        pass1_latency_ms, pass2_latency_ms
		 FROM grounding_events
		 WHERE action = 'vault_search'`,
	)
	if err := row.Scan(&queryText, &action, &results, &pass1MS, &pass2MS); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if queryText == nil || *queryText != "alpha" {
		t.Errorf("query_text: %v", queryText)
	}
	if action != "vault_search" {
		t.Errorf("action: %q", action)
	}
	if results != 1 {
		t.Errorf("results_count: %d", results)
	}
	if pass1MS == nil {
		t.Errorf("pass1_latency_ms must be set (not NULL) for a successful pass-1")
	}
	// Single-result pass-1 → pass-2 skipped, telemetry NULL.
	if pass2MS != nil {
		t.Errorf("pass2_latency_ms must be NULL when pass-2 skipped, got %v", *pass2MS)
	}
}

func TestHandleVaultSearch_VaultRootOverrideUsedNotEnv(t *testing.T) {
	// Override is explicit; the handler must NOT fall back to $HOME.
	root := tempVault(t)
	srv, _ := mockLlama(t, []string{"decisions/alpha.md"})
	deps := depsFor(t, srv.URL, root)
	// Pass an explicit vault_root via params; deps.VaultRoot stays empty.
	deps.VaultRoot = ""
	params, _ := json.Marshal(map[string]any{"query": "alpha", "vault_root": root})
	resp, err := HandleVaultSearch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.VaultRoot != root {
		t.Errorf("vault_root should be the override, got %q want %q", resp.VaultRoot, root)
	}
}

// Bug 1324 regression: when the vault is larger than MaxVaultCandidates,
// the response carries a structured `truncated_note_count` field and the
// candidates_hint no longer claims "75 most-recent" (which was the
// misleading premise that triggered the bug report). The hint must
// accurately describe the keyword-overlap algorithm and name the body
// term we now score against.
func TestHandleVaultSearch_TruncationFieldsAndAccurateHint(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "learnings/general"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed MaxVaultCandidates + 12 notes so we cross the truncation line
	// by a clean margin (a single-note overage would still trip the flag
	// but the count math is easier to read with daylight).
	total := MaxVaultCandidates + 12
	for i := 0; i < total; i++ {
		name := filepath.Join(root, "learnings/general",
			fmt.Sprintf("2026-05-%02d_note-%02d.md", (i%28)+1, i))
		body := fmt.Sprintf("---\ntitle: Note %02d\n---\n\nBody about retrieval %02d.\n", i, i)
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Mock Qwen — any non-empty response is fine; we're asserting on
	// the truncation envelope, not the rank output.
	srv, _ := mockLlama(t, []string{"learnings/general/2026-05-01_note-00.md"})
	deps := depsFor(t, srv.URL, root)
	params, _ := json.Marshal(map[string]any{"query": "retrieval", "vault_root": root})
	resp, err := HandleVaultSearch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleVaultSearch: %v", err)
	}

	if !resp.CandidatesTruncated {
		t.Errorf("truncated flag must be set when vault > MaxVaultCandidates")
	}
	if resp.CandidatesUsed != MaxVaultCandidates {
		t.Errorf("candidates_used: want %d, got %d", MaxVaultCandidates, resp.CandidatesUsed)
	}
	wantTruncatedCount := total - MaxVaultCandidates
	if resp.TruncatedNoteCount != wantTruncatedCount {
		t.Errorf("truncated_note_count: want %d (=%d − %d), got %d",
			wantTruncatedCount, total, MaxVaultCandidates, resp.TruncatedNoteCount)
	}
	if resp.VaultSize != total {
		t.Errorf("vault_size: want %d, got %d", total, resp.VaultSize)
	}

	// Hint must describe the actual algorithm. The previous wording
	// ("75 most-recent") was misleading because the prefilter scores
	// by keyword overlap, not by recency.
	bannedSubstrings := []string{
		"75 most-recent",
		"most-recent were reranked",
	}
	for _, banned := range bannedSubstrings {
		if strings.Contains(resp.CandidatesHint, banned) {
			t.Errorf("candidates_hint must not claim recency bias; found %q in %q",
				banned, resp.CandidatesHint)
		}
	}
	requiredSubstrings := []string{
		"keyword-overlap",
		"body",
		"older",
		// Bug 655: the hint must name the env-var knob so agents who
		// hit truncation have a documented way to raise the cap.
		"TOOLKIT_VAULT_MAX_CANDIDATES",
		// Bug 655: the hint includes "current cap is N" so the agent
		// sees the active value without having to grep the source.
		"current cap is",
	}
	for _, want := range requiredSubstrings {
		if !strings.Contains(resp.CandidatesHint, want) {
			t.Errorf("candidates_hint must mention %q; got %q", want, resp.CandidatesHint)
		}
	}
}

// Bug 1324 regression: a vault below the truncation cap must not set
// truncated_note_count (omitempty serialises it out of the wire form
// for a clean "no truncation" response).
func TestHandleVaultSearch_NoTruncationFieldsWhenUnderCap(t *testing.T) {
	root := tempVault(t) // 3 notes < 75
	srv, _ := mockLlama(t, []string{"decisions/alpha.md"})
	deps := depsFor(t, srv.URL, root)
	params, _ := json.Marshal(map[string]any{"query": "alpha", "vault_root": root})
	resp, err := HandleVaultSearch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("HandleVaultSearch: %v", err)
	}
	if resp.CandidatesTruncated {
		t.Errorf("candidates_truncated must be false for a sub-cap vault")
	}
	if resp.TruncatedNoteCount != 0 {
		t.Errorf("truncated_note_count must be 0 for sub-cap vault, got %d",
			resp.TruncatedNoteCount)
	}
	if resp.CandidatesHint != "" {
		t.Errorf("candidates_hint must be empty for sub-cap vault, got %q",
			resp.CandidatesHint)
	}
}

// ── HandleVaultRead ────────────────────────────────────────────────────

func TestHandleVaultRead_MissingPathReturnsError(t *testing.T) {
	deps := depsFor(t, "http://127.0.0.1:1", tempVault(t))
	resp, _ := HandleVaultRead(context.Background(), deps, json.RawMessage(`{}`))
	if !strings.Contains(resp.Error, "path is required") {
		t.Errorf("expected path-required error, got %q", resp.Error)
	}
}

func TestHandleVaultRead_ReturnsFrontmatterAndEditHint(t *testing.T) {
	root := tempVault(t)
	deps := depsFor(t, "http://127.0.0.1:1", root)
	params, _ := json.Marshal(map[string]any{"path": "decisions/alpha.md", "vault_root": root})
	resp, err := HandleVaultRead(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Path != "decisions/alpha.md" {
		t.Errorf("path: %q", resp.Path)
	}
	if resp.Frontmatter == nil {
		t.Fatalf("frontmatter unexpectedly nil")
	}
	if resp.Frontmatter.Title != "Alpha decision" {
		t.Errorf("title: %q", resp.Frontmatter.Title)
	}
	if !strings.Contains(resp.EditHint, "Read tool") || !strings.Contains(resp.EditHint, root) {
		t.Errorf("edit_hint must reference Read tool and absolute path; got %q", resp.EditHint)
	}
}

// Parity-pin: traversal must be rejected with a typed `path_traversal` error,
// not a generic 500 — callers depend on the error key to map UI hints.
func TestHandleVaultRead_TraversalReturnsTypedError(t *testing.T) {
	root := tempVault(t)
	deps := depsFor(t, "http://127.0.0.1:1", root)
	params, _ := json.Marshal(map[string]any{"path": "/etc/passwd", "vault_root": root})
	resp, _ := HandleVaultRead(context.Background(), deps, params)
	if resp.Error != "path_traversal" {
		t.Errorf("expected path_traversal, got %q", resp.Error)
	}
	if resp.Hint == "" {
		t.Errorf("hint should accompany path_traversal error")
	}
}

func TestHandleVaultRead_MissingNoteReturnsTypedError(t *testing.T) {
	root := tempVault(t)
	deps := depsFor(t, "http://127.0.0.1:1", root)
	params, _ := json.Marshal(map[string]any{"path": "decisions/no-such-file.md", "vault_root": root})
	resp, _ := HandleVaultRead(context.Background(), deps, params)
	if resp.Error != "note_not_found" {
		t.Errorf("expected note_not_found, got %q", resp.Error)
	}
}

// ── isTransientInferenceError ─────────────────────────────────────────

func TestIsTransientInferenceError(t *testing.T) {
	if !isTransientInferenceError(errString("router generate: empty choices in response")) {
		t.Error("must classify llamacpp empty-choices error as transient")
	}
	if isTransientInferenceError(errString("router generate: 500 internal error")) {
		t.Error("non-empty-choices errors must not be classified transient")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
