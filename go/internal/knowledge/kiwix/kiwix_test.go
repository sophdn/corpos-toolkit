package kiwix

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func rss(items string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Search</title>` + items + `</channel></rss>`
}

func TestParseSearchXML_TwoHits(t *testing.T) {
	zim := "wp_es_climate_2026-04"
	body := rss(fmt.Sprintf(
		`<item><title>Cambio climático</title><link>/content/%s/Cambio_clim%%C3%%A1tico</link><description>snippet one</description><wordCount>123</wordCount></item>`+
			`<item><title>Otro</title><link>/content/%s/Otro_art%%C3%%ADculo</link><description>snippet two</description></item>`,
		zim, zim,
	))
	hits, err := ParseSearchXML(body, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].ArticleRef.ZimID != zim {
		t.Errorf("zim: %s", hits[0].ArticleRef.ZimID)
	}
	if hits[0].ArticleRef.Slug != "Cambio_climático" {
		t.Errorf("slug: %q", hits[0].ArticleRef.Slug)
	}
	if hits[0].WordCount == nil || *hits[0].WordCount != 123 {
		t.Errorf("wordCount: %v", hits[0].WordCount)
	}
	if hits[1].ArticleRef.Slug != "Otro_artículo" {
		t.Errorf("slug 2: %q", hits[1].ArticleRef.Slug)
	}
}

func TestParseSearchXML_ZeroHits(t *testing.T) {
	hits, err := ParseSearchXML(rss(""), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("expected zero, got %d", len(hits))
	}
}

func TestParseSearchXML_CapsLongSnippet(t *testing.T) {
	long := strings.Repeat("x", 5000)
	body := rss(fmt.Sprintf(
		`<item><title>t</title><link>/content/z/slug</link><description>%s</description></item>`,
		long,
	))
	hits, err := ParseSearchXML(body, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits[0].Snippet) != 100 {
		t.Errorf("snippet len: %d", len(hits[0].Snippet))
	}
}

// Parity-pin: kiwix-serve's `?books=` filter is loose; each hit's ArticleRef
// must carry the actual ZIM the hit came from, not the requested ZIM. A Go
// port that hard-filters hits would silently lose cross-ZIM matches.
func TestParseSearchXML_AcceptsCrossZimHits(t *testing.T) {
	body := rss(
		`<item><title>requested-zim hit</title><link>/content/devdocs_en_rust_2026-04/reference/types/trait-object</link><description>d</description></item>` +
			`<item><title>sibling-zim hit</title><link>/content/rust-book_2026-04-29/doc.rust-lang.org/book/ch18-02-trait-objects.html</link><description>d</description></item>`,
	)
	hits, err := ParseSearchXML(body, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].ArticleRef.ZimID != "devdocs_en_rust_2026-04" {
		t.Errorf("zim[0]: %s", hits[0].ArticleRef.ZimID)
	}
	if hits[1].ArticleRef.ZimID != "rust-book_2026-04-29" {
		t.Errorf("zim[1]: %s", hits[1].ArticleRef.ZimID)
	}
}

func TestParseSearchXML_RejectsLinksNotUnderContent(t *testing.T) {
	body := rss(`<item><title>t</title><link>/admin/something</link><description>d</description></item>`)
	if _, err := ParseSearchXML(body, 1024); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseSearchXML_RejectsLinkWithEmptySlug(t *testing.T) {
	body := rss(`<item><title>t</title><link>/content/zim/</link><description>d</description></item>`)
	if _, err := ParseSearchXML(body, 1024); err == nil {
		t.Fatal("expected error")
	}
}

func TestCapSnippet_UTF8Safe(t *testing.T) {
	s := "a" + strings.Repeat("é", 50)
	capped := capSnippet(s, 5)
	if !isCharBoundary(capped, len(capped)) {
		t.Errorf("not on char boundary")
	}
	if len(capped) > 5 {
		t.Errorf("len > cap: %d", len(capped))
	}
}

// ── ParseCatalogXML ───────────────────────────────────────────────────

func TestParseCatalogXML_ExtractsZimFromNameTag(t *testing.T) {
	body := `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <title>Rust Docs</title>
    <name>devdocs_en_rust</name>
    <language>en</language>
    <articleCount>1234</articleCount>
    <flavour>maxi</flavour>
  </entry>
</feed>`
	books, err := ParseCatalogXML(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 {
		t.Fatalf("expected 1, got %d", len(books))
	}
	b := books[0]
	if b.ZimID != "devdocs_en_rust" {
		t.Errorf("zim: %s", b.ZimID)
	}
	if b.Title != "Rust Docs" {
		t.Errorf("title: %s", b.Title)
	}
	if b.Language == nil || *b.Language != "en" {
		t.Errorf("language: %v", b.Language)
	}
	if b.ArticleCount == nil || *b.ArticleCount != 1234 {
		t.Errorf("articleCount: %v", b.ArticleCount)
	}
}

func TestParseCatalogXML_FallsBackToLinkHref(t *testing.T) {
	body := `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <title>Wiki</title>
    <link href="/content/wikipedia_en_all_2026-04/A/index"/>
  </entry>
</feed>`
	books, err := ParseCatalogXML(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || books[0].ZimID != "wikipedia_en_all_2026-04" {
		t.Errorf("fallback parse failed: %+v", books)
	}
}

// ── StripBTags ────────────────────────────────────────────────────────

func TestStripBTags_RemovesHighlightMarkup(t *testing.T) {
	if got := StripBTags("foo <b>bar</b> baz"); got != "foo bar baz" {
		t.Errorf("got %q", got)
	}
	if got := StripBTags("plain"); got != "plain" {
		t.Errorf("plain string unchanged, got %q", got)
	}
}

// ── HTTP roundtrip ────────────────────────────────────────────────────

func mockKiwix(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSearch_RoundtripsRSSResponse(t *testing.T) {
	srv := mockKiwix(t, map[string]http.HandlerFunc{
		"/search": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("books") != "wp_en_2026-04" {
				t.Errorf("expected books=wp_en_2026-04, got %s", r.URL.Query().Get("books"))
			}
			if r.URL.Query().Get("format") != "xml" {
				t.Errorf("expected format=xml")
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(rss(`<item><title>T</title><link>/content/wp_en_2026-04/X</link><description>d</description></item>`)))
		},
	})
	c := New(srv.URL)
	hits, err := c.Search(context.Background(), "wp_en_2026-04", "x", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ArticleRef.ZimID != "wp_en_2026-04" {
		t.Errorf("unexpected: %+v", hits)
	}
}

func TestFetch_ReturnsBodyAndMime(t *testing.T) {
	srv := mockKiwix(t, map[string]http.HandlerFunc{
		"/content/wp_en_2026-04/Some_Article": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>body</html>"))
		},
	})
	c := New(srv.URL)
	art, err := c.Fetch(context.Background(), ArticleRef{ZimID: "wp_en_2026-04", Slug: "Some_Article"})
	if err != nil {
		t.Fatal(err)
	}
	if art.MIME != "text/html" {
		t.Errorf("mime: %s", art.MIME)
	}
	if !strings.Contains(art.Content, "body") {
		t.Errorf("content: %s", art.Content)
	}
}

func TestFetch_PercentEncodesSlugSpecialChars(t *testing.T) {
	captured := ""
	srv := mockKiwix(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) {
			captured = r.URL.RequestURI()
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok"))
		},
	})
	c := New(srv.URL)
	_, _ = c.Fetch(context.Background(), ArticleRef{ZimID: "z", Slug: "has space#hash?q"})
	// Expected: spaces → %20, # → %23, ? → %3F
	if !strings.Contains(captured, "%20") || !strings.Contains(captured, "%23") || !strings.Contains(captured, "%3F") {
		t.Errorf("special chars not encoded; captured=%q", captured)
	}
}

func TestSearch_HTTPErrorPropagates(t *testing.T) {
	srv := mockKiwix(t, map[string]http.HandlerFunc{
		"/search": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	c := New(srv.URL)
	if _, err := c.Search(context.Background(), "z", "x", 1); err == nil {
		t.Fatal("expected error")
	}
}

func TestListBooks_ForcesCountMinusOne(t *testing.T) {
	captured := ""
	srv := mockKiwix(t, map[string]http.HandlerFunc{
		"/catalog/v2/entries": func(w http.ResponseWriter, r *http.Request) {
			captured = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`))
		},
	})
	c := New(srv.URL)
	_, err := c.ListBooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(captured, "count=-1") {
		t.Errorf("expected count=-1 in query, got %q", captured)
	}
}

func TestNew_AppliesOptions(t *testing.T) {
	c := New("", WithSnippetCap(512), WithTimeout(time.Second*3))
	if c.SnippetCap() != 512 {
		t.Errorf("snippet cap: %d", c.SnippetCap())
	}
	if c.httpClient.Timeout != 3*time.Second {
		t.Errorf("timeout: %v", c.httpClient.Timeout)
	}
	if c.BaseURL() != DefaultBaseURL {
		t.Errorf("base url: %s", c.BaseURL())
	}
}
