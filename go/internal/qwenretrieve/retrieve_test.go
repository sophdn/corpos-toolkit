package qwenretrieve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
)

func known(paths ...string) []string {
	return paths
}

func TestParseRetrieveResponse_StripsBulletsAndNumbers(t *testing.T) {
	resp := "- alpha\n2. beta\n`gamma`\n"
	got := ParseRetrieveResponse(resp, known("alpha", "beta", "gamma"))
	want := []string{"alpha", "beta", "gamma"}
	if !sliceEq(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseRetrieveResponse_DropsUnknownPaths(t *testing.T) {
	got := ParseRetrieveResponse("alpha\nzeta\nbeta\n", known("alpha", "beta"))
	if !sliceEq(got, []string{"alpha", "beta"}) {
		t.Fatalf("unknown paths must be dropped, got %v", got)
	}
}

func TestParseRetrieveResponse_ReturnsEmptyOnLeadingNoMatch(t *testing.T) {
	if got := ParseRetrieveResponse("no match\n", known("alpha")); len(got) != 0 {
		t.Fatalf("leading 'no match' must return empty, got %v", got)
	}
}

func TestParseRetrieveResponse_KeepsMatchesWhenNoMatchAfter(t *testing.T) {
	got := ParseRetrieveResponse("alpha\nno match\n", known("alpha"))
	if !sliceEq(got, []string{"alpha"}) {
		t.Fatalf("'no match' after matches must stop iteration but keep prior, got %v", got)
	}
}

func TestParseRetrieveResponse_HandlesQuotedNoMatch(t *testing.T) {
	if got := ParseRetrieveResponse("'no match'\n", known("alpha")); len(got) != 0 {
		t.Fatalf("quoted 'no match' must return empty, got %v", got)
	}
}

func TestParseRetrieveResponse_Dedups(t *testing.T) {
	got := ParseRetrieveResponse("alpha\nalpha\nbeta\n", known("alpha", "beta"))
	if !sliceEq(got, []string{"alpha", "beta"}) {
		t.Fatalf("duplicates must be dropped, got %v", got)
	}
}

func TestParseRetrieveResponse_StripsBacktickWrappedPaths(t *testing.T) {
	resp := "- `decisions/x.md`\n- learnings/y.md\n"
	got := ParseRetrieveResponse(resp, known("decisions/x.md", "learnings/y.md"))
	if !sliceEq(got, []string{"decisions/x.md", "learnings/y.md"}) {
		t.Fatalf("backtick wrapping must be stripped, got %v", got)
	}
}

// Parity-pin: trailing punctuation must be stripped without corrupting paths
// whose extensions contain '.' (e.g. `.md`).
func TestParseRetrieveResponse_StripsTrailingPunctuationNotPathChars(t *testing.T) {
	resp := "decisions/2026-05-08.md.\nlearnings/y.md,\n"
	got := ParseRetrieveResponse(resp, known("decisions/2026-05-08.md", "learnings/y.md"))
	if !sliceEq(got, []string{"decisions/2026-05-08.md", "learnings/y.md"}) {
		t.Fatalf("trailing . and , must be stripped without corrupting paths, got %v", got)
	}
}

// Parity-pin: digits/dots are LEFT-stripped only. Numbered prefixes vanish
// but date segments inside paths survive.
func TestParseRetrieveResponse_PreservesPathDigitsAndDots(t *testing.T) {
	resp := "1. decisions/2026-05-08.md\n"
	got := ParseRetrieveResponse(resp, known("decisions/2026-05-08.md"))
	if !sliceEq(got, []string{"decisions/2026-05-08.md"}) {
		t.Fatalf("numbered prefix must be stripped but path digits preserved, got %v", got)
	}
}

// ── SelectPass2OrFallback ──────────────────────────────────────────────

func TestSelectPass2OrFallback_UsesPass2WhenNonEmpty(t *testing.T) {
	pass1 := []string{"a.md", "b.md"}
	pass2 := []string{"b.md", "a.md"}
	got, fellBack := SelectPass2OrFallback(pass1, pass2)
	if !sliceEq(got, pass2) || fellBack {
		t.Fatalf("pass-2 must win when non-empty; got %v fellBack=%v", got, fellBack)
	}
}

func TestSelectPass2OrFallback_FallsBackOnEmptyPass2(t *testing.T) {
	pass1 := []string{"a.md", "b.md"}
	got, fellBack := SelectPass2OrFallback(pass1, nil)
	if !sliceEq(got, pass1) || !fellBack {
		t.Fatalf("empty pass-2 + non-empty pass-1 must fall back; got %v fellBack=%v", got, fellBack)
	}
}

func TestSelectPass2OrFallback_BothEmpty(t *testing.T) {
	got, fellBack := SelectPass2OrFallback(nil, nil)
	if len(got) != 0 || fellBack {
		t.Fatalf("genuine no-match: empty result, fellBack=false; got %v fellBack=%v", got, fellBack)
	}
}

// ── DispatchTwoPassRetrieve ────────────────────────────────────────────

func candidate(p string) RetrieveCandidate {
	return RetrieveCandidate{Path: p}
}

// mockLlama returns an httptest.Server that responds to /v1/chat/completions
// by returning the responses queue in order (or the last value on overflow).
// Returns the server and a recorder of received bodies.
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
		body := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": text}},
			},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func routerPointingAt(t *testing.T, url string) *router.Router {
	t.Helper()
	return router.NewWithClients(llamacpp.New(url), nil, "qwen2.5-32b")
}

func TestDispatchTwoPass_EmptyCandidatesShortCircuits(t *testing.T) {
	// No mock — if the call reaches inference it would error.
	r := routerPointingAt(t, "http://127.0.0.1:1")
	result, err := DispatchTwoPassRetrieve(context.Background(), r, "any", 5, nil, CorpusShapeVault, nil)
	if err != nil {
		t.Fatalf("empty candidates must Ok-short-circuit, got err: %v", err)
	}
	if len(result.RankedPaths) != 0 || result.FellBack || result.Pass1LatencyMS != 0 || result.Pass2LatencyMS != nil {
		t.Fatalf("expected zeroed result, got %+v", result)
	}
}

func TestDispatchTwoPass_SinglePass_UsesParsedOrderOnMatch(t *testing.T) {
	srv, _ := mockLlama(t, []string{"b.md\na.md"})
	r := routerPointingAt(t, srv.URL)
	cands := []RetrieveCandidate{candidate("a.md"), candidate("b.md")}
	result, err := DispatchTwoPassRetrieve(context.Background(), r, "find b", 5, cands, CorpusShapeKiwix, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !sliceEq(result.RankedPaths, []string{"b.md", "a.md"}) {
		t.Fatalf("parsed order, got %v", result.RankedPaths)
	}
	if result.FellBack {
		t.Fatalf("non-empty parse must not flip FellBack")
	}
	if result.Pass2LatencyMS != nil {
		t.Fatalf("single-pass mode: Pass2LatencyMS must be nil")
	}
}

func TestDispatchTwoPass_SinglePass_FallsBackOnEmptyParse(t *testing.T) {
	srv, _ := mockLlama(t, []string{"no match"})
	r := routerPointingAt(t, srv.URL)
	cands := []RetrieveCandidate{candidate("a.md"), candidate("b.md")}
	result, err := DispatchTwoPassRetrieve(context.Background(), r, "off-topic", 3, cands, CorpusShapeKiwix, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !sliceEq(result.RankedPaths, []string{"a.md", "b.md"}) {
		t.Fatalf("empty parse must fall back to input order, got %v", result.RankedPaths)
	}
	if !result.FellBack {
		t.Fatalf("empty parse + non-empty input must flip FellBack")
	}
}

func TestDispatchTwoPass_PropagatesPass1InferenceError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r := routerPointingAt(t, srv.URL)
	_, err := DispatchTwoPassRetrieve(context.Background(), r, "x", 3, []RetrieveCandidate{candidate("a.md")}, CorpusShapeVault, nil)
	if err == nil {
		t.Fatalf("pass-1 HTTP error must propagate as err")
	}
}

func TestDispatchTwoPass_TwoPass_SkipsPass2WhenPass1Returns1(t *testing.T) {
	srv, _ := mockLlama(t, []string{"a.md"})
	r := routerPointingAt(t, srv.URL)
	cands := []RetrieveCandidate{candidate("a.md"), candidate("b.md")}
	provider := BodyExcerptProvider(func(paths []string) []string {
		t.Fatalf("provider must not be called when pass-1 returns <2 paths; got %v", paths)
		return nil
	})
	result, err := DispatchTwoPassRetrieve(context.Background(), r, "narrow", 5, cands, CorpusShapeVault, provider)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !sliceEq(result.RankedPaths, []string{"a.md"}) || result.FellBack || result.Pass2LatencyMS != nil {
		t.Fatalf("expected pass-1 single result unchanged, got %+v", result)
	}
}

func TestDispatchTwoPass_TwoPass_RunsPass2WhenPass1HasMultiple(t *testing.T) {
	// Pass-1 returns 2 paths; pass-2 reranks them.
	srv, bodies := mockLlama(t, []string{"a.md\nb.md", "b.md\na.md"})
	r := routerPointingAt(t, srv.URL)
	cands := []RetrieveCandidate{candidate("a.md"), candidate("b.md")}
	providerCalled := false
	provider := BodyExcerptProvider(func(paths []string) []string {
		providerCalled = true
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i] = "body for " + p
		}
		return out
	})
	result, err := DispatchTwoPassRetrieve(context.Background(), r, "rerank", 5, cands, CorpusShapeVault, provider)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !providerCalled {
		t.Fatalf("body provider must be invoked")
	}
	if !sliceEq(result.RankedPaths, []string{"b.md", "a.md"}) {
		t.Fatalf("expected pass-2 order, got %v", result.RankedPaths)
	}
	if result.FellBack {
		t.Fatalf("non-empty pass-2 must not flip FellBack")
	}
	if result.Pass2LatencyMS == nil {
		t.Fatalf("two-pass executed: Pass2LatencyMS must be set")
	}
	// Verify pass-2 prompt carried body excerpts (parity with Rust YAML body block).
	if len(*bodies) < 2 {
		t.Fatalf("expected 2 inference calls, got %d", len(*bodies))
	}
	if !strings.Contains((*bodies)[1], "body:") || !strings.Contains((*bodies)[1], "body for") {
		t.Fatalf("pass-2 prompt must include body excerpts; got %q", (*bodies)[1])
	}
}

func TestDispatchTwoPass_TwoPass_FallsBackWhenPass2Empty(t *testing.T) {
	srv, _ := mockLlama(t, []string{"a.md\nb.md", "no match"})
	r := routerPointingAt(t, srv.URL)
	cands := []RetrieveCandidate{candidate("a.md"), candidate("b.md")}
	provider := BodyExcerptProvider(func(paths []string) []string {
		out := make([]string, len(paths))
		for i := range paths {
			out[i] = "body"
		}
		return out
	})
	result, err := DispatchTwoPassRetrieve(context.Background(), r, "over-reject", 5, cands, CorpusShapeVault, provider)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !sliceEq(result.RankedPaths, []string{"a.md", "b.md"}) {
		t.Fatalf("empty pass-2 must fall back to pass-1 order, got %v", result.RankedPaths)
	}
	if !result.FellBack {
		t.Fatalf("pass-2 over-rejection must flip FellBack")
	}
}

// ── ComposeRetrieve shape checks ───────────────────────────────────────

func TestComposeRetrieve_Pass1_VaultHeaderAndSystem(t *testing.T) {
	title := "Build.rs migration registry"
	summary := "Auto-discover migrations"
	cands := []RetrieveCandidate{
		{Path: "learnings/general/build-rs.md", Title: &title, Tags: []string{"rust", "build"}, Summary: &summary},
	}
	system, user := ComposeRetrieve(
		RetrieveTaskInput{Query: "migrations", TopK: 3},
		RetrieveContext{Candidates: cands, WithBody: false, CorpusShape: CorpusShapeVault},
	)
	if !strings.Contains(system, "rank notes from a personal knowledge vault") {
		t.Fatalf("pass-1 vault system prompt missing")
	}
	if !strings.HasPrefix(user, "Vault notes (relative paths under the vault root):") {
		t.Fatalf("pass-1 vault header missing; got %q", user[:80])
	}
	if !strings.Contains(user, "- learnings/general/build-rs.md") {
		t.Fatalf("user prompt must list candidate path; got %q", user)
	}
	if !strings.Contains(user, "title: Build.rs migration registry") {
		t.Fatalf("user prompt must include title; got %q", user)
	}
	if !strings.Contains(user, "tags: rust, build") {
		t.Fatalf("user prompt must include tags; got %q", user)
	}
	if !strings.Contains(user, "summary: Auto-discover migrations") {
		t.Fatalf("pass-1 must include summary; got %q", user)
	}
	if strings.Contains(user, "body:") {
		t.Fatalf("pass-1 must NOT include body excerpt; got %q", user)
	}
}

// Parity-pin: title that duplicates the path stem must be omitted (dated-kebab
// notes commonly have an uninformative auto-title equal to the filename).
func TestComposeRetrieve_SkipsRedundantTitleMatchingPathStem(t *testing.T) {
	title := "build-rs"
	cands := []RetrieveCandidate{
		{Path: "learnings/general/build-rs.md", Title: &title},
	}
	_, user := ComposeRetrieve(
		RetrieveTaskInput{Query: "x", TopK: 3},
		RetrieveContext{Candidates: cands, WithBody: false, CorpusShape: CorpusShapeVault},
	)
	if strings.Contains(user, "title: build-rs") {
		t.Fatalf("title equal to path stem must be omitted; got %q", user)
	}
}

func TestComposeRetrieve_Pass2_VaultIncludesBodies(t *testing.T) {
	body := "first line\nsecond line"
	cands := []RetrieveCandidate{
		{Path: "x.md", BodyExcerpt: &body},
	}
	system, user := ComposeRetrieve(
		RetrieveTaskInput{Query: "q", TopK: 2},
		RetrieveContext{Candidates: cands, WithBody: true, CorpusShape: CorpusShapeVault},
	)
	if !strings.Contains(system, "re-rank a small candidate list") {
		t.Fatalf("pass-2 vault system prompt missing")
	}
	if !strings.HasPrefix(user, "Candidate notes (with body excerpts):") {
		t.Fatalf("pass-2 vault header missing")
	}
	if !strings.Contains(user, "  body: |\n    first line\n    second line") {
		t.Fatalf("pass-2 must include YAML-indented body block; got %q", user)
	}
}

func TestComposeRetrieve_Kiwix_HeaderAndNouns(t *testing.T) {
	cands := []RetrieveCandidate{{Path: "wiki/x"}}
	_, user := ComposeRetrieve(
		RetrieveTaskInput{Query: "q", TopK: 5},
		RetrieveContext{Candidates: cands, WithBody: false, CorpusShape: CorpusShapeKiwix},
	)
	if !strings.HasPrefix(user, "Articles (paths shaped <zim_id>/<slug>):") {
		t.Fatalf("kiwix header missing")
	}
	if !strings.Contains(user, "most relevant articles") {
		t.Fatalf("kiwix instruction must use 'articles', got %q", user)
	}
}

// Parity-pin: query strings with embedded double quotes must be escaped in the
// JSON-style Task: "..." line (Rust uses .replace('"', '\\"')).
func TestComposeRetrieve_EscapesEmbeddedQuotes(t *testing.T) {
	cands := []RetrieveCandidate{{Path: "x.md"}}
	_, user := ComposeRetrieve(
		RetrieveTaskInput{Query: `she said "hello"`, TopK: 1},
		RetrieveContext{Candidates: cands, WithBody: false, CorpusShape: CorpusShapeVault},
	)
	if !strings.Contains(user, `Task: "she said \"hello\""`) {
		t.Fatalf("query quotes must be escaped; got %q", user)
	}
}

// ── helpers ───────────────────────────────────────────────────────────

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
