package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"toolkit/internal/knowledge/pointers"
	"toolkit/internal/mcpparam"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
	"toolkit/internal/qwenretrieve"
)

// KnowledgeSearchCandidateMultiplier sizes the FTS5 pre-filter pool: top_k *
// multiplier candidates are sent to Qwen so the rerank has a meaningful
// selection. Matches Rust dispatch (top_k * 4).
const KnowledgeSearchCandidateMultiplier = 4

// HandleKnowledgeSearch implements knowledge_search: FTS5 pre-filter across the
// unified knowledge_pointers index, then Qwen rerank of the top candidates.
//
// Required: query. Optional: top_k (default 5, clamped [1, 20]).
func HandleKnowledgeSearch(ctx context.Context, deps Deps, project string, params json.RawMessage) (KnowledgeSearchResult, error) {
	query := mcpparam.String(params, "query")
	if query == "" {
		return KnowledgeSearchResult{Error: "params.query is required"}, nil
	}
	topK := int(mcpparam.Int64(params, "top_k", 5))
	if topK < 1 {
		topK = 1
	}
	if topK > 20 {
		topK = 20
	}

	candidateIDs, err := pointers.FTSSearch(ctx, deps.Pool, query, topK*KnowledgeSearchCandidateMultiplier)
	if err != nil {
		return KnowledgeSearchResult{Error: fmt.Sprintf("fts: %s", err.Error())}, nil
	}
	if len(candidateIDs) == 0 {
		return KnowledgeSearchResult{
			Results:      []KnowledgePointerResult{},
			ResultsCount: 0,
			Query:        query,
		}, nil
	}

	candidates, err := pointers.GetByIDs(ctx, deps.Pool, candidateIDs)
	if err != nil {
		return KnowledgeSearchResult{Error: fmt.Sprintf("db: %s", err.Error())}, nil
	}

	// Build retrieve candidates. Path is "ptr/<id>" — the prefix prevents the
	// retrieve-response parser from stripping the bare integer id (its left-
	// strip removes leading digits and dots). Mirrors Rust.
	retrieveCands := make([]qwenretrieve.RetrieveCandidate, len(candidates))
	for i, p := range candidates {
		title := fmt.Sprintf("[%s] %s", p.SourceType, p.SourceRef)
		summary := truncateRunes(p.Question, 160)
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

	rankedIDs, qwenFellBack := rerankKnowledgePointers(
		ctx, deps, query, topK, retrieveCands, candidateIDs,
	)

	byID := make(map[int64]pointers.KnowledgePointer, len(candidates))
	for _, p := range candidates {
		byID[p.ID] = p
	}
	results := make([]KnowledgePointerResult, 0, len(rankedIDs))
	for _, id := range rankedIDs {
		p, ok := byID[id]
		if !ok {
			continue
		}
		if err := pointers.IncrementUsage(ctx, deps.Pool, id); err != nil {
			obs.Logger(ctx).Warn("knowledge_search: increment_usage failed",
				slog.Int64("pointer_id", id),
				slog.String("err", err.Error()),
			)
		}
		results = append(results, KnowledgePointerResult{
			ID:                    p.ID,
			SourceType:            p.SourceType,
			SourceRef:             p.SourceRef,
			Question:              p.Question,
			InvokeWhen:            p.InvokeWhen,
			QualityScore:          p.QualityScore,
			UsageCount:            p.UsageCount,
			NegativeFeedbackCount: p.NegativeFeedbackCount,
		})
	}
	recordGroundingEvent(ctx, deps.Pool, project, "knowledge_search", query, int64(len(results)), groundingRefsFromPointers(results), HandlerTelemetry{})
	return KnowledgeSearchResult{
		Results:      results,
		ResultsCount: len(results),
		Query:        query,
		QwenFellBack: qwenFellBack,
	}, nil
}

// rerankKnowledgePointers runs the Qwen rerank and parses the response back to
// pointer IDs. On inference failure it returns the FTS5 candidate ids in their
// original rank order, capped at top_k, with qwen_fell_back=true.
func rerankKnowledgePointers(
	ctx context.Context,
	deps Deps,
	query string,
	topK int,
	retrieveCands []qwenretrieve.RetrieveCandidate,
	candidateIDs []int64,
) ([]int64, bool) {
	ctx = qwenctx.WithTaskID(ctx, "knowledge-search")
	result, err := qwenretrieve.DispatchTwoPassRetrieve(
		ctx, deps.Router, query, topK, retrieveCands, qwenretrieve.CorpusShapeVault, nil,
	)
	if err != nil {
		obs.Logger(ctx).Warn("knowledge_search: Qwen rerank failed; falling back to FTS5 order",
			slog.String("err", err.Error()))
		return capIDs(candidateIDs, topK), true
	}
	ids := make([]int64, 0, len(result.RankedPaths))
	for _, p := range result.RankedPaths {
		stripped := strings.TrimPrefix(p, "ptr/")
		if stripped == p {
			continue
		}
		id, err := strconv.ParseInt(stripped, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, result.FellBack
}

func capIDs(ids []int64, n int) []int64 {
	if n > len(ids) {
		return ids
	}
	return ids[:n]
}

// HandleKnowledgeReportMiss implements knowledge_report_miss.
//
// Required: pointer_id (integer). Optional: staleness_reason (string).
func HandleKnowledgeReportMiss(ctx context.Context, deps Deps, _ string, params json.RawMessage) (KnowledgeReportMissResult, error) {
	id := mcpparam.Int64(params, "pointer_id", 0)
	if id <= 0 {
		return KnowledgeReportMissResult{Error: "params.pointer_id (integer) is required"}, nil
	}
	var reason *string
	if r := mcpparam.String(params, "staleness_reason"); r != "" {
		reason = &r
	}
	if err := pointers.RecordMiss(ctx, deps.Pool, id, reason); err != nil {
		return KnowledgeReportMissResult{Error: err.Error()}, nil
	}
	return KnowledgeReportMissResult{OK: true, PointerID: id}, nil
}
