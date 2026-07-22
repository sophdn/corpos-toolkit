package refresolve

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/knowledge"
)

// libraryResolver looks up a library entry by dewey number. The
// detector emits ShapeLibraryEntry references for catalog matches
// against library_entries.dewey; the resolver returns the entry's
// citation + establishes prose as a Candidate.
type libraryResolver struct{ pool *db.Pool }

func (libraryResolver) Shape() ShapeCategory   { return ShapeLibraryEntry }
func (libraryResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 5} }

func (r libraryResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	if r.pool == nil {
		return HitSet{ConfidenceTier: TierNoHit, Err: errors.New("library resolver: db pool nil")}, nil
	}
	const q = `SELECT project_id, citation, establishes, status FROM library_entries WHERE dewey = ?`
	rows, err := r.pool.DB().QueryContext(ctx, q, ref.Token)
	if err != nil {
		return HitSet{Err: fmt.Errorf("library lookup: %w", err)}, nil
	}
	defer rows.Close()
	cands := []Candidate{}
	for rows.Next() {
		var project, citation, establishes, status string
		if err := rows.Scan(&project, &citation, &establishes, &status); err != nil {
			return HitSet{Err: fmt.Errorf("library scan: %w", err)}, nil
		}
		title := establishes
		if title == "" {
			title = citation
		}
		cands = append(cands, Candidate{
			ID:         ref.Token,
			Title:      fmt.Sprintf("library entry %s in %s: %s", ref.Token, project, title),
			Score:      1.0,
			SourceRef:  "library:" + project + "/" + ref.Token,
			DebugNotes: fmt.Sprintf("status=%s citation=%s", status, truncateForDebug(citation, 80)),
		})
	}
	if err := rows.Err(); err != nil {
		return HitSet{Err: fmt.Errorf("library rows: %w", err)}, nil
	}
	return HitSet{Candidates: cands}, nil
}

// domainTermResolver wraps knowledge_search for ShapeDomainTerm
// references. The detector flagged the phrase as project-internal;
// the resolver delegates to the unified knowledge surface to find
// matching vault / library / chain / task / bug pointers.
type domainTermResolver struct {
	deps    knowledge.Deps
	project string
}

func (domainTermResolver) Shape() ShapeCategory   { return ShapeDomainTerm }
func (domainTermResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 800} }

func (r domainTermResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	type knowledgeSearchParams struct {
		Query  string `json:"query"`
		TopK   int    `json:"top_k"`
		Source string `json:"source"`
	}
	params, err := json.Marshal(knowledgeSearchParams{
		Query:  ref.Token,
		TopK:   5,
		Source: "reference_resolution",
	})
	if err != nil {
		return HitSet{Err: fmt.Errorf("marshal params: %w", err)}, nil
	}
	result, err := knowledge.HandleKnowledgeSearch(ctx, r.deps, r.project, params)
	if err != nil {
		return HitSet{Err: fmt.Errorf("knowledge_search: %w", err)}, nil
	}
	if result.Error != "" {
		return HitSet{Err: errors.New(result.Error)}, nil
	}
	cands := make([]Candidate, 0, len(result.Results))
	for i, p := range result.Results {
		score := 0.0
		if p.QualityScore != nil {
			score = *p.QualityScore
		}
		// Rank decays linearly so position 1 wins ties.
		positionDecay := 1.0 - float64(i)*0.1
		if positionDecay < 0.1 {
			positionDecay = 0.1
		}
		finalScore := score * positionDecay
		if finalScore == 0 {
			finalScore = positionDecay
		}
		cands = append(cands, Candidate{
			ID:         fmt.Sprintf("%d", p.ID),
			Title:      p.Question,
			Score:      finalScore,
			SourceRef:  fmt.Sprintf("%s:%s", p.SourceType, p.SourceRef),
			DebugNotes: fmt.Sprintf("rank=%d source_type=%s usage=%d", i+1, p.SourceType, p.UsageCount),
		})
	}
	return HitSet{Candidates: cands}, nil
}

// externalTechnicalResolver routes external-technical tokens via
// knowledge_search — the unified surface that already includes the
// kiwix_references index alongside vault / library / chain / task /
// bug pointers. The design's per-shape "kiwix_search direct" intent
// would require picking a specific ZIM at call time (kiwix_search
// requires params.zim_id); the unified path sidesteps that without
// adding a ZIM-routing heuristic. Per-corpus dispatch (the design's
// original split) is the right shape once the source-router model
// ships (T7 + ML capability chain); the unified surface is the
// pragmatic v1 path.
type externalTechnicalResolver struct {
	deps    knowledge.Deps
	project string
}

func (externalTechnicalResolver) Shape() ShapeCategory   { return ShapeExternalTechnical }
func (externalTechnicalResolver) Cost() ResolverCostHint { return ResolverCostHint{TypicalMs: 1500} }

func (r externalTechnicalResolver) Resolve(ctx context.Context, ref Reference) (HitSet, error) {
	type knowledgeSearchParams struct {
		Query  string `json:"query"`
		TopK   int    `json:"top_k"`
		Source string `json:"source"`
	}
	params, err := json.Marshal(knowledgeSearchParams{
		Query:  ref.Token,
		TopK:   5,
		Source: "reference_resolution",
	})
	if err != nil {
		return HitSet{Err: fmt.Errorf("marshal params: %w", err)}, nil
	}
	result, err := knowledge.HandleKnowledgeSearch(ctx, r.deps, r.project, params)
	if err != nil {
		return HitSet{Err: fmt.Errorf("knowledge_search: %w", err)}, nil
	}
	if result.Error != "" {
		return HitSet{Err: errors.New(result.Error)}, nil
	}
	cands := make([]Candidate, 0, len(result.Results))
	for i, p := range result.Results {
		score := 0.0
		if p.QualityScore != nil {
			score = *p.QualityScore
		}
		positionDecay := 1.0 - float64(i)*0.1
		if positionDecay < 0.1 {
			positionDecay = 0.1
		}
		finalScore := score * positionDecay
		if finalScore == 0 {
			finalScore = positionDecay
		}
		cands = append(cands, Candidate{
			ID:         fmt.Sprintf("%d", p.ID),
			Title:      p.Question,
			Score:      finalScore,
			SourceRef:  fmt.Sprintf("%s:%s", p.SourceType, p.SourceRef),
			DebugNotes: fmt.Sprintf("rank=%d source_type=%s usage=%d", i+1, p.SourceType, p.UsageCount),
		})
	}
	return HitSet{Candidates: cands}, nil
}

// truncateForDebug shortens long strings for the Candidate.DebugNotes
// field; keeps human-readable summaries without bloating telemetry.
func truncateForDebug(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// sqlNoRowsAsNoHit is a small helper for resolvers that prefer
// to surface "no row" as a clean TierNoHit rather than a Go
// error. Currently unused; placeholder for resolvers that may
// adopt it once the registry has more clients.
var _ = sql.ErrNoRows
