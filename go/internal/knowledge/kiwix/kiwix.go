// Package kiwix is a typed client for kiwix-serve's three endpoints:
//   - /search?format=xml → RSS-shaped XML, parsed to []SearchHit.
//   - /content/<zim>/<slug> → article body.
//   - /catalog/v2/entries?count=-1 → OPDS-shaped XML, parsed to []BookInfo.
//
// Mirrors knowledge_lib::kiwix on the Rust side. Drops the observability
// integration — wire back when the unified observe-layer lands.
package kiwix

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultSnippetCap caps search snippets at 2 KiB so agent context stays
	// bounded by hit count, not per-hit snippet bloat.
	DefaultSnippetCap = 2 * 1024
	// DefaultTimeout sized for the worst-case fulltext query under disk-cache
	// misses; typical responses are sub-second.
	DefaultTimeout = 15 * time.Second
	// DefaultBaseURL is the local kiwix-serve endpoint.
	DefaultBaseURL = "http://localhost:8889"
)

// ArticleRef identifies one article: the (versioned) ZIM ID and the article
// slug. Slug is the percent-decoded form as it appears under /content/<zim>/.
type ArticleRef struct {
	ZimID string `json:"zim_id"`
	Slug  string `json:"slug"`
}

// SearchHit is one result from kiwix-serve's /search XML response. Score is
// reserved for future ranker integration; it is always nil from kiwix-serve.
type SearchHit struct {
	ArticleRef ArticleRef `json:"article_ref"`
	Title      string     `json:"title"`
	Snippet    string     `json:"snippet"`
	WordCount  *uint32    `json:"word_count,omitempty"`
	Score      *float64   `json:"score,omitempty"`
}

// Article carries one fetched article body plus its declared MIME type.
type Article struct {
	ArticleRef ArticleRef `json:"article_ref"`
	MIME       string     `json:"mime"`
	Content    string     `json:"content"`
}

// BookInfo summarises one ZIM book from the OPDS catalog.
type BookInfo struct {
	ZimID        string  `json:"zim_id"`
	Title        string  `json:"title"`
	Language     *string `json:"language,omitempty"`
	ArticleCount *uint64 `json:"article_count,omitempty"`
	Flavour      *string `json:"flavour,omitempty"`
}

// ErrHTTP wraps a non-success kiwix HTTP response.
type ErrHTTP struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *ErrHTTP) Error() string {
	return fmt.Sprintf("kiwix %s: HTTP %d: %s", e.Op, e.StatusCode, e.Body)
}

// Client calls kiwix-serve.
type Client struct {
	baseURL    string
	httpClient *http.Client
	snippetCap int
}

// Option configures a Client.
type Option func(*Client)

// WithSnippetCap overrides the default snippet cap (DefaultSnippetCap).
func WithSnippetCap(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.snippetCap = n
		}
	}
}

// WithTimeout overrides the default request timeout (DefaultTimeout).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.httpClient.Timeout = d
		}
	}
}

// New constructs a Client targeting baseURL. Empty string uses DefaultBaseURL.
func New(baseURL string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: DefaultTimeout},
		snippetCap: DefaultSnippetCap,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// BaseURL returns the configured kiwix-serve endpoint.
func (c *Client) BaseURL() string { return c.baseURL }

// SnippetCap returns the configured snippet byte cap.
func (c *Client) SnippetCap() int { return c.snippetCap }

// Search runs a fulltext query against zimID, returning up to limit hits.
//
// kiwix-serve treats `?books=<zim>` as a loose filter — the response can carry
// hits from sibling ZIMs (versioned variants or unrelated ZIMs whose articles
// match strongly). Each hit's ArticleRef carries the actual ZIM the hit came
// from; callers post-filter if they need hard per-ZIM scope.
func (c *Client) Search(ctx context.Context, zimID, pattern string, limit int) ([]SearchHit, error) {
	if limit < 1 {
		limit = 1
	}
	q := url.Values{}
	q.Set("books", zimID)
	q.Set("pattern", pattern)
	q.Set("format", "xml")
	q.Set("pageLength", strconv.Itoa(limit))
	endpoint := c.baseURL + "/search?" + q.Encode()

	body, err := c.get(ctx, endpoint, "search")
	if err != nil {
		return nil, err
	}
	return ParseSearchXML(body, c.snippetCap)
}

// Fetch retrieves one article by ZIM ID + slug. Slug is percent-encoded for
// the URL path; the response is returned verbatim along with its Content-Type.
func (c *Client) Fetch(ctx context.Context, ar ArticleRef) (Article, error) {
	endpoint := c.baseURL + "/content/" + ar.ZimID + "/" + percentEncodeSlug(ar.Slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Article{}, fmt.Errorf("kiwix fetch build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Article{}, fmt.Errorf("kiwix fetch: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return Article{}, fmt.Errorf("kiwix fetch read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Article{}, &ErrHTTP{Op: "fetch", StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return Article{ArticleRef: ar, MIME: mime, Content: string(bodyBytes)}, nil
}

// ListBooks queries the OPDS catalog and returns all loaded ZIMs.
//
// Forces ?count=-1 so kiwix-serve returns the full catalog rather than its
// 10-entry default page (was bug #1142 on the Rust side).
func (c *Client) ListBooks(ctx context.Context) ([]BookInfo, error) {
	endpoint := c.baseURL + "/catalog/v2/entries?count=-1"
	body, err := c.get(ctx, endpoint, "catalog")
	if err != nil {
		return nil, err
	}
	return ParseCatalogXML(body)
}

func (c *Client) get(ctx context.Context, endpoint, op string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("kiwix %s build request: %w", op, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("kiwix %s: %w", op, err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("kiwix %s read body: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &ErrHTTP{Op: op, StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}
	return string(bodyBytes), nil
}

// ── XML parsing ───────────────────────────────────────────────────────

// rssItem mirrors the kiwix RSS shape: <title>, <link> (a /content path),
// <description> (the snippet), <wordCount> (optional).
type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	WordCount   string `xml:"wordCount"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssRoot struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

// ParseSearchXML parses a kiwix /search?format=xml response into hits. snippetCap
// truncates each hit's snippet to ≤ snippetCap bytes on a UTF-8 char boundary.
func ParseSearchXML(body string, snippetCap int) ([]SearchHit, error) {
	var root rssRoot
	if err := xml.Unmarshal([]byte(body), &root); err != nil {
		return nil, fmt.Errorf("kiwix search: bad xml: %w", err)
	}
	hits := make([]SearchHit, 0, len(root.Channel.Items))
	for _, item := range root.Channel.Items {
		if item.Title == "" {
			return nil, errors.New("kiwix search: item missing <title>")
		}
		if item.Link == "" {
			return nil, errors.New("kiwix search: item missing <link>")
		}
		ar, err := parseContentLink(item.Link)
		if err != nil {
			return nil, err
		}
		snippet := capSnippet(item.Description, snippetCap)
		var wc *uint32
		if item.WordCount != "" {
			if n, err := strconv.ParseUint(item.WordCount, 10, 32); err == nil {
				v := uint32(n)
				wc = &v
			}
		}
		hits = append(hits, SearchHit{
			ArticleRef: ar,
			Title:      item.Title,
			Snippet:    snippet,
			WordCount:  wc,
		})
	}
	return hits, nil
}

// parseContentLink parses a `/content/<zim>/<percent-encoded-slug>` link into
// an ArticleRef. The ZIM is whatever the link carries; no validation against
// any expected ZIM since kiwix-serve's `?books=` filter is loose.
func parseContentLink(link string) (ArticleRef, error) {
	stripped := strings.TrimPrefix(link, "/content/")
	if stripped == link {
		return ArticleRef{}, fmt.Errorf("kiwix search: link not under /content: %s", link)
	}
	slash := strings.IndexByte(stripped, '/')
	if slash < 0 {
		return ArticleRef{}, fmt.Errorf("kiwix search: link missing slug: %s", link)
	}
	zimPart := stripped[:slash]
	slugPart := stripped[slash+1:]
	if slugPart == "" {
		return ArticleRef{}, fmt.Errorf("kiwix search: link slug empty: %s", link)
	}
	decoded, err := url.PathUnescape(slugPart)
	if err != nil {
		// Match Rust's decode_utf8_lossy: fall back to the raw form on decode
		// failure rather than rejecting the hit.
		decoded = slugPart
	}
	return ArticleRef{ZimID: zimPart, Slug: decoded}, nil
}

// capSnippet trims s to ≤ cap bytes on a UTF-8 char boundary.
func capSnippet(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	idx := cap
	for idx > 0 && !isCharBoundary(s, idx) {
		idx--
	}
	return s[:idx]
}

// isCharBoundary reports whether idx is at a UTF-8 sequence boundary.
func isCharBoundary(s string, idx int) bool {
	if idx == 0 || idx == len(s) {
		return true
	}
	b := s[idx]
	return b < 0x80 || b >= 0xc0
}

// percentEncodeSlug encodes a slug for use inside a /content/<zim>/<slug>
// path. Mirrors the Rust SLUG_ENCODE_SET: encode space, #, ?, <, >. Forward
// slashes are preserved (kiwix slugs may contain slashes for nested paths).
func percentEncodeSlug(slug string) string {
	var b strings.Builder
	b.Grow(len(slug))
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		switch c {
		case ' ', '#', '?', '<', '>':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, "%%%02X", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// ── OPDS catalog parsing ──────────────────────────────────────────────

// opdsEntry mirrors one kiwix-serve catalog entry. Per the Rust port we
// inspect entry's <title>, <language>, <articleCount>, <flavour>, and
// <name>. When <name> is absent the parser falls back to parsing the ZIM
// out of a <link href="/content/<zim>/..."> attribute.
type opdsEntry struct {
	Title        string     `xml:"title"`
	Language     string     `xml:"language"`
	Name         string     `xml:"name"`
	ArticleCount string     `xml:"articleCount"`
	Flavour      string     `xml:"flavour"`
	Links        []opdsLink `xml:"link"`
}

type opdsLink struct {
	Href string `xml:"href,attr"`
}

type opdsFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []opdsEntry `xml:"entry"`
}

// ParseCatalogXML parses an OPDS catalog feed. Best-effort: fields present in
// real kiwix-serve output are extracted; missing fields yield nil rather than
// parse errors.
func ParseCatalogXML(body string) ([]BookInfo, error) {
	var feed opdsFeed
	if err := xml.Unmarshal([]byte(body), &feed); err != nil {
		return nil, fmt.Errorf("kiwix catalog: bad xml: %w", err)
	}
	books := make([]BookInfo, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		zim := e.Name
		if zim == "" {
			for _, l := range e.Links {
				if tail := strings.TrimPrefix(l.Href, "/content/"); tail != l.Href {
					if slash := strings.IndexByte(tail, '/'); slash > 0 {
						zim = tail[:slash]
						break
					}
				}
			}
		}
		if zim == "" {
			continue
		}
		bi := BookInfo{ZimID: zim, Title: e.Title}
		if e.Language != "" {
			lang := e.Language
			bi.Language = &lang
		}
		if e.ArticleCount != "" {
			if n, err := strconv.ParseUint(e.ArticleCount, 10, 64); err == nil {
				bi.ArticleCount = &n
			}
		}
		if e.Flavour != "" {
			f := e.Flavour
			bi.Flavour = &f
		}
		books = append(books, bi)
	}
	return books, nil
}

// StripBTags removes <b> and </b> markers from a kiwix snippet. kiwix-serve's
// match-highlight markup is bounded to <b> only; a regex-free strip is
// sufficient.
func StripBTags(s string) string {
	if !strings.ContainsAny(s, "<") {
		return s
	}
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	return s
}
