package knowledge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
	"toolkit/internal/knowledge/pointers"
	"toolkit/internal/testutil"
)

func searchDeps(t *testing.T, llamaURL string) Deps {
	t.Helper()
	r := router.NewWithClients(llamacpp.New(llamaURL), nil, "qwen2.5-32b")
	return Deps{Pool: testutil.NewTestDB(t), Router: r}
}

func seedSearchPointer(t *testing.T, deps Deps, question, invokeWhen, sourceRef string) int64 {
	t.Helper()
	id, err := pointers.Insert(context.Background(), deps.Pool, pointers.KnowledgePointer{
		ProjectID:  "test-proj",
		SourceType: "vault",
		SourceRef:  sourceRef,
		Question:   question,
		InvokeWhen: invokeWhen,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHandleKnowledgeSearch_MissingQueryReturnsError(t *testing.T) {
	deps := searchDeps(t, "http://127.0.0.1:1")
	resp, _ := HandleKnowledgeSearch(context.Background(), deps, "", json.RawMessage(`{}`))
	if !strings.Contains(resp.Error, "query is required") {
		t.Errorf("expected query-required, got %q", resp.Error)
	}
}

func TestHandleKnowledgeSearch_EmptyCorpusReturnsEmpty(t *testing.T) {
	deps := searchDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"query": "x"})
	resp, _ := HandleKnowledgeSearch(context.Background(), deps, "", params)
	if resp.ResultsCount != 0 {
		t.Errorf("expected results_count=0, got %d", resp.ResultsCount)
	}
}

// Parity-pin: pointer ids are bare integers; their candidate path must be
// "ptr/<id>" so the retrieve-response parser doesn't strip the digits as a
// numbered-list prefix.
func TestHandleKnowledgeSearch_QwenReceivesPtrPrefixedPaths(t *testing.T) {
	deps := searchDeps(t, "")
	id := seedSearchPointer(t, deps, "How does retrieve work?", "when planning retrieve", ".claude/vault/x.md")
	// Hook a Qwen mock that captures the request body so we can assert the prompt.
	srv, captured := mockLlama(t, []string{"ptr/" + i64Str(id)})
	deps.Router = router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")

	params, _ := json.Marshal(map[string]any{"query": "retrieve"})
	resp, err := HandleKnowledgeSearch(context.Background(), deps, "", params)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].ID != id {
		t.Errorf("expected single result with id %d; got %+v", id, resp.Results)
	}
	// Verify the prompt body included the prefixed path.
	if len(*captured) < 1 {
		t.Fatal("Qwen was not called")
	}
	if !strings.Contains((*captured)[0], "ptr/"+i64Str(id)) {
		t.Errorf("expected Qwen prompt to carry ptr/<id> path; got %q", (*captured)[0])
	}
}

// Parity-pin: when Qwen fails, FTS5 candidate order is preserved (capped at
// top_k) and qwen_fell_back=true.
func TestHandleKnowledgeSearch_QwenFailureFallsBackToFTSOrder(t *testing.T) {
	deps := searchDeps(t, "http://127.0.0.1:1") // dead port → inference error
	a := seedSearchPointer(t, deps, "How does retrieve A work?", "when A", ".claude/vault/a.md")
	b := seedSearchPointer(t, deps, "How does retrieve B work?", "when B", ".claude/vault/b.md")

	params, _ := json.Marshal(map[string]any{"query": "retrieve", "top_k": 5})
	resp, _ := HandleKnowledgeSearch(context.Background(), deps, "", params)
	if !resp.QwenFellBack {
		t.Errorf("expected qwen_fell_back=true on inference failure; got %v", resp.QwenFellBack)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 fallback results, got %+v", resp.Results)
	}
	// FTS5 OR rank: both pointers match; both surface. IDs from input order.
	if resp.Results[0].ID != a && resp.Results[0].ID != b {
		t.Errorf("unexpected first result: %+v", resp.Results[0])
	}
}

// DB-row verification (PARITY_STANDARD §2c): increment_usage bumps the row's
// counter for each result returned.
func TestHandleKnowledgeSearch_IncrementsUsagePerResult(t *testing.T) {
	deps := searchDeps(t, "")
	id := seedSearchPointer(t, deps, "How does retrieve work?", "when planning retrieve", ".claude/vault/x.md")
	srv, _ := mockLlama(t, []string{"ptr/" + i64Str(id)})
	deps.Router = router.NewWithClients(llamacpp.New(srv.URL), nil, "qwen2.5-32b")

	params, _ := json.Marshal(map[string]any{"query": "retrieve"})
	if _, err := HandleKnowledgeSearch(context.Background(), deps, "", params); err != nil {
		t.Fatalf("call: %v", err)
	}
	got, _ := pointers.GetByIDs(context.Background(), deps.Pool, []int64{id})
	if got[0].UsageCount != 1 {
		t.Errorf("usage_count: %d", got[0].UsageCount)
	}
	if got[0].LastUsedAt == nil {
		t.Errorf("last_used_at must be stamped after a successful surfaced result")
	}
}

// ── HandleKnowledgeReportMiss ─────────────────────────────────────

func TestHandleKnowledgeReportMiss_MissingIDReturnsError(t *testing.T) {
	deps := searchDeps(t, "http://127.0.0.1:1")
	resp, _ := HandleKnowledgeReportMiss(context.Background(), deps, "", json.RawMessage(`{}`))
	if !strings.Contains(resp.Error, "pointer_id") {
		t.Errorf("expected pointer_id-required, got %q", resp.Error)
	}
}

func TestHandleKnowledgeReportMiss_IncrementsCounter(t *testing.T) {
	deps := searchDeps(t, "http://127.0.0.1:1")
	id := seedSearchPointer(t, deps, "q", "w", ".claude/vault/x.md")
	params, _ := json.Marshal(map[string]any{"pointer_id": id, "staleness_reason": "moved"})
	resp, _ := HandleKnowledgeReportMiss(context.Background(), deps, "", params)
	if !resp.OK || resp.PointerID != id {
		t.Errorf("response: %+v", resp)
	}
	got, _ := pointers.GetByIDs(context.Background(), deps.Pool, []int64{id})
	if got[0].NegativeFeedbackCount != 1 {
		t.Errorf("counter: %d", got[0].NegativeFeedbackCount)
	}
	if got[0].StalenessHint == nil || *got[0].StalenessHint != "moved" {
		t.Errorf("hint: %v", got[0].StalenessHint)
	}
}

func i64Str(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
