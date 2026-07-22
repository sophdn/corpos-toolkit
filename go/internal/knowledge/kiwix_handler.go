package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"toolkit/internal/knowledge/kiwix"
	"toolkit/internal/mcpparam"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
	"toolkit/internal/qwenretrieve"
)

// kiwixBaseURLEnv is the env var that overrides the default kiwix-serve URL
// when neither a per-call `base_url` param is supplied nor Deps.KiwixBaseURL
// is set.
const kiwixBaseURLEnv = "KIWIX_BASE_URL"

// HandleKiwixSearch implements kiwix_search with Qwen rerank.
//
// Required params: zim_id, pattern.
// Optional params: limit (default 10, ≥1), base_url (override the kiwix-serve URL).
//
// On any kiwix HTTP error the response is the typed kiwix error envelope.
// On Qwen rerank failure the original kiwix hit list is returned with
// QwenFellBack=true (matching Rust).
func HandleKiwixSearch(ctx context.Context, deps Deps, params json.RawMessage) (KiwixSearchResult, error) {
	zimID := mcpparam.String(params, "zim_id")
	pattern := mcpparam.String(params, "pattern")
	if zimID == "" || pattern == "" {
		return KiwixSearchResult{Error: "params.zim_id and params.pattern are required"}, nil
	}
	limit := int(mcpparam.Int64(params, "limit", 10))
	if limit < 1 {
		limit = 1
	}

	client := buildKiwixClient(deps, params)

	hits, err := client.Search(ctx, zimID, pattern, limit)
	if err != nil {
		return KiwixSearchResult{Error: err.Error()}, nil
	}
	hitsIn := len(hits)

	// Build Qwen rerank candidates. path = "<zim>/<slug>", summary = stripped
	// kiwix snippet. Mirrors Rust's kiwix_search_with_qwen_rerank.
	candidates := make([]qwenretrieve.RetrieveCandidate, len(hits))
	for i, h := range hits {
		title := h.Title
		summary := kiwix.StripBTags(h.Snippet)
		c := qwenretrieve.RetrieveCandidate{
			Path: h.ArticleRef.ZimID + "/" + h.ArticleRef.Slug,
		}
		if title != "" {
			c.Title = &title
		}
		if summary != "" {
			c.Summary = &summary
		}
		candidates[i] = c
	}

	// Single-pass Qwen rerank (kiwix has no pass-2 body excerpt). Inference
	// failure → fall back to original hit list with QwenFellBack=true and
	// no token attribution.
	ctx = qwenctx.WithTaskID(ctx, "kiwix-rerank-retrieve")
	result, dispErr := qwenretrieve.DispatchTwoPassRetrieve(
		ctx, deps.Router, pattern, limit,
		candidates, qwenretrieve.CorpusShapeKiwix, nil,
	)
	if dispErr != nil {
		obs.Logger(ctx).Warn("kiwix_search: Qwen rerank failed; falling back to original hits",
			slog.String("err", dispErr.Error()))
		paths := make([]string, len(hits))
		for i, h := range hits {
			paths[i] = h.ArticleRef.ZimID + "/" + h.ArticleRef.Slug
		}
		result = qwenretrieve.TwoPassRetrieveResult{
			RankedPaths: paths,
			FellBack:    true,
		}
	}

	// Reorder hits by result.RankedPaths. Each path appears at most once in
	// the input hits, so a simple lookup map suffices.
	byPath := make(map[string]kiwix.SearchHit, len(hits))
	for _, h := range hits {
		byPath[h.ArticleRef.ZimID+"/"+h.ArticleRef.Slug] = h
	}
	finalHits := make([]kiwix.SearchHit, 0, len(result.RankedPaths))
	for _, p := range result.RankedPaths {
		if h, ok := byPath[p]; ok {
			finalHits = append(finalHits, h)
			delete(byPath, p)
		}
	}
	hitsOut := len(finalHits)

	// Kiwix is cross-project; same convention as vault_search. The
	// per-handler kiwix_offload_invocations table was retired by chain
	// telemetry-substrate-cleanup T2 (migration 046); qwen_fell_back +
	// hits_in/out live inline on the grounding_events row now.
	hitsInI64 := int64(hitsIn)
	hitsOutI64 := int64(hitsOut)
	fellBack := result.FellBack
	recordGroundingEvent(ctx, deps.Pool, "", "kiwix_search", pattern, int64(hitsOut), groundingRefsFromKiwix(finalHits), HandlerTelemetry{
		QwenFellBack: &fellBack,
		KiwixHitsIn:  &hitsInI64,
		KiwixHitsOut: &hitsOutI64,
	})

	return KiwixSearchResult{
		Hits:         finalHits,
		QwenFellBack: result.FellBack,
		HitsIn:       hitsIn,
		HitsOut:      hitsOut,
	}, nil
}

// KiwixFetchResult is the response shape for kiwix_fetch. On success Article
// carries the fetched content; on failure Error is set. The Article field is
// declared with omitempty + pointer so absent-on-error renders as a missing
// key rather than an empty struct (preserves prior shape).
type KiwixFetchResult struct {
	Article *kiwix.Article `json:"article,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// HandleKiwixFetch implements kiwix_fetch.
//
// Required params: zim_id, slug.
// Optional param: base_url.
func HandleKiwixFetch(ctx context.Context, deps Deps, params json.RawMessage) (KiwixFetchResult, error) {
	zimID := mcpparam.String(params, "zim_id")
	slug := mcpparam.String(params, "slug")
	if zimID == "" || slug == "" {
		return KiwixFetchResult{Error: "params.zim_id and params.slug are required"}, nil
	}
	client := buildKiwixClient(deps, params)
	art, err := client.Fetch(ctx, kiwix.ArticleRef{ZimID: zimID, Slug: slug})
	if err != nil {
		return KiwixFetchResult{Error: err.Error()}, nil
	}
	return KiwixFetchResult{Article: &art}, nil
}

// HandleKiwixListBooks implements kiwix_list_books with optional substring filter.
//
// Optional params: q (case-insensitive substring filter on zim_id + title),
// base_url.
func HandleKiwixListBooks(ctx context.Context, deps Deps, params json.RawMessage) (KiwixListBooksResult, error) {
	client := buildKiwixClient(deps, params)
	books, err := client.ListBooks(ctx)
	if err != nil {
		return KiwixListBooksResult{Error: err.Error()}, nil
	}
	totalCount := len(books)
	q := strings.ToLower(mcpparam.String(params, "q"))
	var filtered []kiwix.BookInfo
	if q != "" {
		filtered = make([]kiwix.BookInfo, 0, len(books))
		for _, b := range books {
			if strings.Contains(strings.ToLower(b.ZimID), q) ||
				strings.Contains(strings.ToLower(b.Title), q) {
				filtered = append(filtered, b)
			}
		}
	} else {
		filtered = books
	}
	return KiwixListBooksResult{
		Items:         filtered,
		TotalCount:    totalCount,
		ReturnedCount: len(filtered),
		Filter:        q,
		Note: "ZIM IDs here are unversioned (e.g. devdocs_en_rust). kiwix_search results carry " +
			"versioned IDs (e.g. devdocs_en_rust_2026-04). Use the versioned zim_id from kiwix_search " +
			"hit article_ref for snapshot_id in reference_add calls.",
	}, nil
}

// buildKiwixClient resolves the kiwix base URL by precedence:
//  1. params.base_url
//  2. deps.KiwixBaseURL (struct override)
//  3. $KIWIX_BASE_URL
//  4. kiwix.DefaultBaseURL ("http://localhost:8889").
func buildKiwixClient(deps Deps, params json.RawMessage) *kiwix.Client {
	baseURL := mcpparam.String(params, "base_url")
	if baseURL == "" {
		baseURL = deps.KiwixBaseURL
	}
	if baseURL == "" {
		baseURL = os.Getenv(kiwixBaseURLEnv)
	}
	if baseURL == "" {
		baseURL = kiwix.DefaultBaseURL
	}
	if deps.KiwixClient != nil {
		// Test-only injection — the explicit override wins.
		return deps.KiwixClient
	}
	return kiwix.New(baseURL)
}
