package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
	"toolkit/internal/testutil"
)

// mockKiwixServer returns a kiwix-shaped HTTP test server backed by the
// provided per-path handlers. Closed via t.Cleanup.
func mockKiwixServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for p, h := range handlers {
		mux.HandleFunc(p, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func depsForKiwix(t *testing.T, kiwixURL, llamaURL string) Deps {
	t.Helper()
	r := router.NewWithClients(llamacpp.New(llamaURL), nil, "qwen2.5-32b")
	return Deps{
		Pool:         testutil.NewTestDB(t),
		Router:       r,
		KiwixBaseURL: kiwixURL,
	}
}

func searchRSS(items string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Search</title>` + items + `</channel></rss>`
}

// ── HandleKiwixSearch ────────────────────────────────────────────────

func TestHandleKiwixSearch_MissingParamsReturnsError(t *testing.T) {
	deps := depsForKiwix(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	resp, _ := HandleKiwixSearch(context.Background(), deps, json.RawMessage(`{"zim_id":"z"}`))
	if !strings.Contains(resp.Error, "zim_id and params.pattern are required") {
		t.Errorf("expected required-params error, got %q", resp.Error)
	}
}

func TestHandleKiwixSearch_ReranksByQwen(t *testing.T) {
	// kiwix returns two hits a/b in order; Qwen reranks to b/a.
	kiwixSrv := mockKiwixServer(t, map[string]http.HandlerFunc{
		"/search": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(searchRSS(
				`<item><title>A title</title><link>/content/z_2026/A</link><description>snippet A</description></item>` +
					`<item><title>B title</title><link>/content/z_2026/B</link><description>snippet B</description></item>`,
			)))
		},
	})
	llamaSrv, _ := mockLlama(t, []string{"z_2026/B\nz_2026/A"})
	deps := depsForKiwix(t, kiwixSrv.URL, llamaSrv.URL)

	params, _ := json.Marshal(map[string]any{"zim_id": "z_2026", "pattern": "x", "limit": 5})
	resp, err := HandleKiwixSearch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(resp.Hits))
	}
	if resp.Hits[0].ArticleRef.Slug != "B" {
		t.Errorf("expected B first after rerank; got %s", resp.Hits[0].ArticleRef.Slug)
	}
	if resp.QwenFellBack {
		t.Errorf("expected qwen_fell_back=false on successful rerank")
	}
	if resp.HitsIn != 2 || resp.HitsOut != 2 {
		t.Errorf("hits_in/out: %d / %d", resp.HitsIn, resp.HitsOut)
	}
}

// Parity-pin: Qwen inference failure must fall back to kiwix's original order
// with qwen_fell_back=true; the search must not surface as a hard error to the
// caller (matching Rust behaviour).
func TestHandleKiwixSearch_FallsBackOnQwenFailure(t *testing.T) {
	kiwixSrv := mockKiwixServer(t, map[string]http.HandlerFunc{
		"/search": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(searchRSS(
				`<item><title>A</title><link>/content/z_2026/A</link><description>d</description></item>` +
					`<item><title>B</title><link>/content/z_2026/B</link><description>d</description></item>`,
			)))
		},
	})
	llamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(llamaSrv.Close)
	deps := depsForKiwix(t, kiwixSrv.URL, llamaSrv.URL)

	params, _ := json.Marshal(map[string]any{"zim_id": "z_2026", "pattern": "x"})
	resp, err := HandleKiwixSearch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !resp.QwenFellBack {
		t.Errorf("expected qwen_fell_back=true on inference error")
	}
	if len(resp.Hits) != 2 || resp.Hits[0].ArticleRef.Slug != "A" {
		t.Errorf("expected original A/B order on fallback; got %+v", resp.Hits)
	}
}

// DB-row verification (PARITY_STANDARD §2c): one kiwix_offload_invocations row
// per call with the correct hits_in / hits_out / qwen_fell_back values
// on the grounding_events row (post chain telemetry-substrate-cleanup T2,
// migration 046).
func TestHandleKiwixSearch_WritesTelemetryRow(t *testing.T) {
	kiwixSrv := mockKiwixServer(t, map[string]http.HandlerFunc{
		"/search": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(searchRSS(
				`<item><title>A</title><link>/content/z/A</link><description>d</description></item>`,
			)))
		},
	})
	llamaSrv, _ := mockLlama(t, []string{"z/A"})
	deps := depsForKiwix(t, kiwixSrv.URL, llamaSrv.URL)
	params, _ := json.Marshal(map[string]any{"zim_id": "z", "pattern": "alpha", "limit": 5})
	if _, err := HandleKiwixSearch(context.Background(), deps, params); err != nil {
		t.Fatalf("call: %v", err)
	}

	var (
		queryText *string
		action    string
		results   int64
		hin, hout *int64
		fellBack  *int64
	)
	row := deps.Pool.DB().QueryRow(
		`SELECT query_text, action, results_count,
		        kiwix_hits_in, kiwix_hits_out, qwen_fell_back
		 FROM grounding_events
		 WHERE action = 'kiwix_search'`,
	)
	if err := row.Scan(&queryText, &action, &results, &hin, &hout, &fellBack); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if queryText == nil || *queryText != "alpha" {
		t.Errorf("query_text: %v", queryText)
	}
	if action != "kiwix_search" {
		t.Errorf("action: %q", action)
	}
	if results != 1 {
		t.Errorf("results_count: %d", results)
	}
	if hin == nil || hout == nil || *hin != 1 || *hout != 1 {
		t.Errorf("hits: in=%v out=%v", hin, hout)
	}
	if fellBack == nil || *fellBack != 0 {
		t.Errorf("qwen_fell_back should be 0 on successful rerank, got %v", fellBack)
	}
}

func TestHandleKiwixSearch_KiwixHTTPErrorSurfaces(t *testing.T) {
	kiwixSrv := mockKiwixServer(t, map[string]http.HandlerFunc{
		"/search": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	deps := depsForKiwix(t, kiwixSrv.URL, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"zim_id": "z", "pattern": "x"})
	resp, _ := HandleKiwixSearch(context.Background(), deps, params)
	if !strings.Contains(resp.Error, "HTTP 500") {
		t.Errorf("expected HTTP 500 error envelope, got %q", resp.Error)
	}
}

// ── HandleKiwixFetch ─────────────────────────────────────────────────

func TestHandleKiwixFetch_MissingParamsReturnsError(t *testing.T) {
	deps := depsForKiwix(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	resp, _ := HandleKiwixFetch(context.Background(), deps, json.RawMessage(`{}`))
	if !strings.Contains(resp.Error, "zim_id and params.slug are required") {
		t.Errorf("expected required-params error, got %q", resp.Error)
	}
}

func TestHandleKiwixFetch_ReturnsArticle(t *testing.T) {
	kiwixSrv := mockKiwixServer(t, map[string]http.HandlerFunc{
		"/content/z_2026/Some_Article": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>body</html>"))
		},
	})
	deps := depsForKiwix(t, kiwixSrv.URL, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"zim_id": "z_2026", "slug": "Some_Article"})
	resp, err := HandleKiwixFetch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Article == nil {
		t.Fatalf("article unexpectedly nil; resp.Error=%q", resp.Error)
	}
	if resp.Article.MIME != "text/html" || !strings.Contains(resp.Article.Content, "body") {
		t.Errorf("article wrong: %+v", resp.Article)
	}
}

// ── HandleKiwixListBooks ─────────────────────────────────────────────

func TestHandleKiwixListBooks_ReturnsBooksAndCounts(t *testing.T) {
	kiwixSrv := mockKiwixServer(t, map[string]http.HandlerFunc{
		"/catalog/v2/entries": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry><title>Rust Docs</title><name>devdocs_en_rust</name><language>en</language></entry>
  <entry><title>Kube</title><name>devdocs_en_kubernetes</name></entry>
</feed>`))
		},
	})
	deps := depsForKiwix(t, kiwixSrv.URL, "http://127.0.0.1:1")
	resp, err := HandleKiwixListBooks(context.Background(), deps, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.TotalCount != 2 || resp.ReturnedCount != 2 {
		t.Errorf("counts: total=%d returned=%d", resp.TotalCount, resp.ReturnedCount)
	}
	if resp.Filter != "" {
		t.Errorf("filter should be empty when not supplied; got %q", resp.Filter)
	}
	if !strings.Contains(resp.Note, "unversioned") {
		t.Errorf("note must mention unversioned ZIMs; got %q", resp.Note)
	}
}

func TestHandleKiwixListBooks_SubstringFilter(t *testing.T) {
	kiwixSrv := mockKiwixServer(t, map[string]http.HandlerFunc{
		"/catalog/v2/entries": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry><title>Rust Docs</title><name>devdocs_en_rust</name></entry>
  <entry><title>Kube</title><name>devdocs_en_kubernetes</name></entry>
</feed>`))
		},
	})
	deps := depsForKiwix(t, kiwixSrv.URL, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"q": "rust"})
	resp, _ := HandleKiwixListBooks(context.Background(), deps, params)
	if resp.TotalCount != 2 || resp.ReturnedCount != 1 {
		t.Errorf("counts: total=%d returned=%d", resp.TotalCount, resp.ReturnedCount)
	}
	if resp.Filter != "rust" {
		t.Errorf("filter: %q", resp.Filter)
	}
}
