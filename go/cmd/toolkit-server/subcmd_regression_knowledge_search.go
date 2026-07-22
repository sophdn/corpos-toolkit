package main

// Regression harness for the knowledge_search retrieval surface.
// Measures MRR + Top-3 hit rate across a 36-scenario corpus, comparing
// the Qwen-rerank path against the BM25-only baseline.
//
// ## Intended use
//
// **Workflow served:** verify the knowledge_search retrieval surface
// hasn't regressed against the curated gold corpus. Compares (a)
// FTS5 pre-filter + Qwen two-pass rerank vs (b) FTS5-only.
//
// **Invocation pattern:** `toolkit-server regression-knowledge-search
// [--db PATH] [--gold-dir PATH] [--project-id ID] [--llama-url URL]`.
//
// **Pass bars:** MRR >= 0.70; Top-3 hit rate >= 0.80; Reason/Attribute
// >= 3 of 5 must pass.
//
// Folded into toolkit-server as `toolkit-server
// regression-knowledge-search` by harvest-the-consolidation T4. Ported
// from benchmarks/src/bin/regression_knowledge_search.rs per chain
// rust-retirement-and-db-hardening T5 Phase 5.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"toolkit/internal/benchmarks"
	"toolkit/internal/db"
	"toolkit/internal/inference/router"
	"toolkit/internal/knowledge/pointers"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
	"toolkit/internal/qwenretrieve"

	_ "modernc.org/sqlite"
)

const (
	regKnowledgeMRRPassBar  = 0.70
	regKnowledgeTop3PassBar = 0.80
	regKnowledgeRAPassCount = 3 // ≥3 of 5 RA scenarios must pass
	regKnowledgeTopK        = 5
)

// runRegressionKnowledgeSearch drives the `regression-knowledge-search`
// subcommand. Returns 0 on all pass-bars met; 1 otherwise.
func runRegressionKnowledgeSearch(args []string) int {
	fs := flag.NewFlagSet("regression-knowledge-search", flag.ContinueOnError)
	dbPath := fs.String("db", defaultToolkitDBPath(), "path to toolkit.db")
	goldDir := fs.String("gold-dir", "", "gold corpus dir")
	llamaURL := fs.String("llama-url", "http://localhost:8081", "llama.cpp server URL")
	projectID := fs.String("project-id", "mcp-servers", "project_id for benchmark_results scoping")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: toolkit-server regression-knowledge-search [--db PATH] [--gold-dir PATH] [--project-id ID] [--llama-url URL]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	scenarios, err := benchmarks.LoadKnowledgeSearchScenarios(*goldDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load retrieval scenarios: %v\n", err)
		return 1
	}
	raScenarios, err := benchmarks.LoadRaScenarios(*goldDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load RA scenarios: %v\n", err)
		return 1
	}

	installProjectionsFoldHook()
	pool, err := db.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db %s: %v\n", *dbPath, err)
		return 1
	}
	defer pool.Close()

	r, err := router.New(*llamaURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init router: %v\n", err)
		return 1
	}

	fmt.Printf("→ db:           %s\n", *dbPath)
	fmt.Printf("→ project_id:   %s\n", *projectID)
	fmt.Printf("→ model:        %s\n", r.ModelName())
	fmt.Printf("→ scenarios:    %d (retrieval) + %d (reason/attribute)\n", len(scenarios), len(raScenarios))
	fmt.Println()

	ctx := qwenctx.WithTaskID(context.Background(), "knowledge-search-regression")

	pointerMap, err := loadAllActivePointers(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load pointers: %v\n", err)
		return 1
	}
	fmt.Printf("→ pointers loaded: %d active\n\n", len(pointerMap))

	if rc := runRetrievalScenarios(ctx, pool, r, pointerMap, scenarios, *projectID); rc != 0 {
		return rc
	}
	return runReasonAttributeScenarios(ctx, pool, r, pointerMap, raScenarios, *projectID)
}

// loadAllActivePointers reads every active row from knowledge_pointers.
// The regression harness needs the full set because each scenario's
// expected pointer is resolved by source_ref substring (Rust parity).
func loadAllActivePointers(ctx context.Context, pool *db.Pool) (map[int64]pointers.KnowledgePointer, error) {
	rows, err := pool.DB().QueryContext(ctx, `SELECT id FROM knowledge_pointers WHERE status='active'`)
	if err != nil {
		return nil, fmt.Errorf("list pointer ids: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	loaded, err := pointers.GetByIDs(ctx, pool, ids)
	if err != nil {
		return nil, fmt.Errorf("get pointers: %w", err)
	}
	out := make(map[int64]pointers.KnowledgePointer, len(loaded))
	for _, p := range loaded {
		out[p.ID] = p
	}
	return out, nil
}

// resolveRegKnowledgePointer finds the first pointer whose source_ref
// contains needle. Picks the shortest source_ref on ties (most
// specific). Matches Rust.
func resolveRegKnowledgePointer(pointersByID map[int64]pointers.KnowledgePointer, needle string) (pointers.KnowledgePointer, bool) {
	var best pointers.KnowledgePointer
	var found bool
	for _, p := range pointersByID {
		if !strings.Contains(p.SourceRef, needle) {
			continue
		}
		if !found || len(p.SourceRef) < len(best.SourceRef) {
			best = p
			found = true
		}
	}
	return best, found
}

// regKnowledgeSearchResult bundles one rank-of-gold output for a
// single search-path call.
type regKnowledgeSearchResult struct {
	rankedIDs []int64
	fellBack  bool
	latencyMS int64
}

// searchQwen runs FTS5 pre-filter + Qwen two-pass rerank.
func searchQwen(ctx context.Context, pool *db.Pool, r *router.Router, query string) regKnowledgeSearchResult {
	start := time.Now()
	candidateIDs, err := pointers.FTSSearch(ctx, pool, query, regKnowledgeTopK*benchmarks.KsCandidateMultiplier)
	if err != nil {
		return regKnowledgeSearchResult{fellBack: true, latencyMS: time.Since(start).Milliseconds()}
	}
	if len(candidateIDs) == 0 {
		return regKnowledgeSearchResult{fellBack: true, latencyMS: time.Since(start).Milliseconds()}
	}
	cands, err := pointers.GetByIDs(ctx, pool, candidateIDs)
	if err != nil {
		return regKnowledgeSearchResult{fellBack: true, latencyMS: time.Since(start).Milliseconds()}
	}

	retrieveCands := make([]qwenretrieve.RetrieveCandidate, len(cands))
	for i, p := range cands {
		title := fmt.Sprintf("[%s] %s", p.SourceType, p.SourceRef)
		summary := regKnowledgeTruncateRunes(p.Question, 160)
		c := qwenretrieve.RetrieveCandidate{
			Path:  "ptr/" + strconv.FormatInt(p.ID, 10),
			Title: &title,
			Tags:  append([]string(nil), p.Tags...),
		}
		if summary != "" {
			c.Summary = &summary
		}
		retrieveCands[i] = c
	}

	rerank, rerankErr := qwenretrieve.DispatchTwoPassRetrieve(
		ctx, r, query, regKnowledgeTopK, retrieveCands, qwenretrieve.CorpusShapeVault, nil,
	)
	latencyMS := time.Since(start).Milliseconds()
	if rerankErr != nil {
		// Fall back to FTS5 order on rerank failure.
		limit := regKnowledgeTopK
		if limit > len(candidateIDs) {
			limit = len(candidateIDs)
		}
		return regKnowledgeSearchResult{rankedIDs: candidateIDs[:limit], fellBack: true, latencyMS: latencyMS}
	}

	var rankedIDs []int64
	for _, path := range rerank.RankedPaths {
		idStr, ok := strings.CutPrefix(path, "ptr/")
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		rankedIDs = append(rankedIDs, id)
	}
	return regKnowledgeSearchResult{rankedIDs: rankedIDs, fellBack: rerank.FellBack, latencyMS: latencyMS}
}

// searchBM25 runs FTS5-only (no rerank).
func searchBM25(ctx context.Context, pool *db.Pool, query string) regKnowledgeSearchResult {
	start := time.Now()
	ids, err := pointers.FTSSearch(ctx, pool, query, regKnowledgeTopK)
	if err != nil {
		return regKnowledgeSearchResult{latencyMS: time.Since(start).Milliseconds()}
	}
	return regKnowledgeSearchResult{rankedIDs: ids, latencyMS: time.Since(start).Milliseconds()}
}

// regKnowledgeRankOf returns the 1-based rank of needleID in
// rankedIDs, or 0 if absent.
func regKnowledgeRankOf(rankedIDs []int64, needleID int64) int {
	for i, id := range rankedIDs {
		if id == needleID {
			return i + 1
		}
	}
	return 0
}

// regKnowledgeReciprocalRank: 1/rank or 0.0 if not found.
func regKnowledgeReciprocalRank(rankedIDs []int64, needleID int64) float64 {
	r := regKnowledgeRankOf(rankedIDs, needleID)
	if r == 0 {
		return 0
	}
	return 1.0 / float64(r)
}

func runRetrievalScenarios(ctx context.Context, pool *db.Pool, r *router.Router, pointerMap map[int64]pointers.KnowledgePointer, scenarios []benchmarks.KsScenario, projectID string) int {
	fmt.Println("── Retrieval-accuracy scenarios ──")

	var (
		mrrSumQ, mrrSumB float64
		top3Q, top3B     int
		heldOutSumQ      float64
		heldOutN         int
		skipped          int
	)
	for _, s := range scenarios {
		gold, ok := resolveRegKnowledgePointer(pointerMap, s.SourceRefContains)
		if !ok {
			fmt.Printf("  %-50s SKIP (no pointer with source_ref containing %q)\n", s.Slug, s.SourceRefContains)
			skipped++
			continue
		}

		qwen := searchQwen(ctx, pool, r, s.Query)
		bm25 := searchBM25(ctx, pool, s.Query)

		rrQ := regKnowledgeReciprocalRank(qwen.rankedIDs, gold.ID)
		rrB := regKnowledgeReciprocalRank(bm25.rankedIDs, gold.ID)
		rankQ := regKnowledgeRankOf(qwen.rankedIDs, gold.ID)
		rankB := regKnowledgeRankOf(bm25.rankedIDs, gold.ID)
		inTop3Q := rankQ > 0 && rankQ <= 3
		inTop3B := rankB > 0 && rankB <= 3

		mrrSumQ += rrQ
		mrrSumB += rrB
		if inTop3Q {
			top3Q++
		}
		if inTop3B {
			top3B++
		}
		if s.HeldOut {
			heldOutSumQ += rrQ
			heldOutN++
		}

		marker := "✓"
		if !inTop3Q {
			marker = "✗"
		}
		held := ""
		if s.HeldOut {
			held = " (held-out)"
		}
		fmt.Printf("  %s %-46s Qwen rank=%-3s BM25 rank=%-3s (%d ms)%s\n",
			marker, s.Slug, regKnowledgeRankStr(rankQ), regKnowledgeRankStr(rankB), qwen.latencyMS, held)

		recordRetrievalRow(ctx, pool, projectID, "knowledge-search", s.Slug,
			r.ModelName(), qwen.latencyMS, inTop3Q, rrQ, rrB, rankQ, rankB, qwen.fellBack)
	}

	denom := float64(len(scenarios) - skipped)
	if denom == 0 {
		fmt.Println("\nno scored scenarios; skipping summary")
		return 1
	}
	mrrQ := mrrSumQ / denom
	mrrB := mrrSumB / denom
	top3RateQ := float64(top3Q) / denom
	top3RateB := float64(top3B) / denom

	fmt.Println()
	fmt.Println("── Retrieval-accuracy summary ──")
	fmt.Printf("  Qwen-rerank   MRR=%.3f  Top3=%.3f  (n=%d, skipped=%d)\n",
		mrrQ, top3RateQ, len(scenarios)-skipped, skipped)
	fmt.Printf("  BM25-only     MRR=%.3f  Top3=%.3f\n", mrrB, top3RateB)
	if heldOutN > 0 {
		fmt.Printf("  Qwen held-out MRR=%.3f  (n=%d)\n", heldOutSumQ/float64(heldOutN), heldOutN)
	}
	fmt.Printf("  Pass bars: MRR ≥ %.2f → %s, Top3 ≥ %.2f → %s\n",
		regKnowledgeMRRPassBar, passFailString(mrrQ >= regKnowledgeMRRPassBar),
		regKnowledgeTop3PassBar, passFailString(top3RateQ >= regKnowledgeTop3PassBar),
	)
	fmt.Println()

	if mrrQ < regKnowledgeMRRPassBar || top3RateQ < regKnowledgeTop3PassBar {
		return 1
	}
	return 0
}

func runReasonAttributeScenarios(ctx context.Context, pool *db.Pool, r *router.Router, pointerMap map[int64]pointers.KnowledgePointer, scenarios []benchmarks.RaScenario, projectID string) int {
	fmt.Println("── Reason/Attribute scenarios ──")
	passed := 0
	skipped := 0
	for _, s := range scenarios {
		gold, ok := resolveRegKnowledgePointer(pointerMap, s.SourceRefContains)
		if !ok {
			fmt.Printf("  %-50s SKIP (no pointer with source_ref containing %q)\n", s.Slug, s.SourceRefContains)
			skipped++
			continue
		}

		qwen := searchQwen(ctx, pool, r, s.Query)
		rank := regKnowledgeRankOf(qwen.rankedIDs, gold.ID)
		inTop3 := rank > 0 && rank <= 3

		marker := "✓"
		if !inTop3 {
			marker = "✗"
		}
		fmt.Printf("  %s %-46s rank=%-3s (%s, %d ms)\n",
			marker, s.Slug, regKnowledgeRankStr(rank), s.Origin, qwen.latencyMS)

		recordRetrievalRow(ctx, pool, projectID, "knowledge-grounding", s.Slug,
			r.ModelName(), qwen.latencyMS, inTop3,
			regKnowledgeReciprocalRank(qwen.rankedIDs, gold.ID), 0,
			rank, 0, qwen.fellBack)

		if inTop3 {
			passed++
		}
	}

	fmt.Println()
	fmt.Println("── Reason/Attribute summary ──")
	fmt.Printf("  passed: %d/%d (skipped=%d, pass bar ≥ %d)  → %s\n",
		passed, len(scenarios)-skipped, skipped, regKnowledgeRAPassCount, passFailString(passed >= regKnowledgeRAPassCount))
	fmt.Println()

	if passed < regKnowledgeRAPassCount {
		return 1
	}
	return 0
}

// recordRetrievalRow persists one Retrieve-shape benchmark row via the
// event substrate.
func recordRetrievalRow(ctx context.Context, pool *db.Pool, projectID, taskID, scenarioSlug, modelName string, latencyMS int64, inTop3 bool, qwenRR, bm25RR float64, qwenRank, bm25Rank int, fellBack bool) {
	notes := fmt.Sprintf(`{"qwen_rank":%d,"bm25_rank":%d,"fell_back":%t}`, qwenRank, bm25Rank, fellBack)
	label := "not-found"
	if qwenRank > 0 {
		label = strconv.Itoa(qwenRank)
	}
	res := db.ClassifyResult{
		Label:         label,
		RawResponse:   notes,
		LatencyMS:     latencyMS,
		InvocationOK:  inTop3,
		NotesOverride: notes,
		RunShape:      "regression",
	}
	if err := db.RecordBenchmarkDispatch(ctx, pool, projectID, taskID, modelName, res); err != nil {
		obs.Logger(ctx).Warn("regression-knowledge-search: record dispatch failed",
			slog.String("err", err.Error()),
			slog.String("scenario", scenarioSlug),
		)
	}
	_ = qwenRR
	_ = bm25RR
}

func regKnowledgeTruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count >= n {
			return s[:i]
		}
		count++
	}
	return s
}

func regKnowledgeRankStr(rank int) string {
	if rank == 0 {
		return "—"
	}
	return strconv.Itoa(rank)
}
