package arcreview

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"toolkit/internal/db"
)

// session_filing_dedupe.go — T6 of chain arc-close-decision-authoring-split.
// The same-session dedup GUARD: before a decision is staged or executed,
// check it against artifacts the in-session agent ALREADY FILED this
// session (its own forge / MemoryWritten / in-snapshot vault forge). On a
// near-match, downgrade the decision from "file a new note" to "suggest
// enriching the existing one" (EnrichExisting marker → surface_for_confirm
// with an enrich prompt) so the substrate never stages a duplicate of work
// the seat just did.
//
// Distinct from the other dedup passes:
//   - F2 (ApplyExistingArtifactDedupe): project-wide EXISTING artifacts.
//   - F3 (ApplySameSessionDedupe): prior QWEN PROPOSALS this session.
//   - T6 (here): the AGENT'S OWN actual filings this session.
//
// Scope guard (T6 constraint): same-session agent filings only. This does
// NOT re-solve general vault-semantic-dedup (bug 899) — it leans on the
// already-gathered in-arc filing set (recentFilingsInArc + the snapshot
// extraction + MemoryWritten), not a fresh project-wide semantic scan.

// EnrichExistingMatch records the agent's own session filing that a
// decision duplicates. Slug/Title name the existing artifact so the
// dispatch surface can say "enrich <slug>" instead of "file a new note".
type EnrichExistingMatch struct {
	Kind       string  `json:"kind"`
	Slug       string  `json:"slug"`
	Title      string  `json:"title"`
	Similarity float64 `json:"similarity"`
}

// enrichJaccardEnvVar overrides the T6 title-similarity threshold.
const enrichJaccardEnvVar = "TOOLKIT_ARCCLOSE_ENRICH_JACCARD"

// enrichDefaultJaccard is the default same-session-agent-filing match
// threshold. Title-to-title within one session tends to be near-verbatim
// (the agent and Qwen both name the same arc), so a moderate threshold
// keeps recall without firing on incidental token overlap. Mirrors F3's
// 0.40.
const enrichDefaultJaccard = 0.40

func enrichJaccardThreshold() float64 {
	raw := strings.TrimSpace(os.Getenv(enrichJaccardEnvVar))
	if raw == "" {
		return enrichDefaultJaccard
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1 {
		return enrichDefaultJaccard
	}
	return v
}

// enrichKindFor maps a decision action to the RecentFiling.Kind value(s)
// that count as "the same kind of artifact" for the enrich-existing match.
// A vault-note decision only dedupes against vault filings, a bug against
// bug filings, etc. — cross-kind title collisions (a bug and a note with
// similar titles) are NOT duplicates.
func enrichKindFor(action ActionKind) map[string]bool {
	switch action {
	case ActionForgeVaultNote:
		return map[string]bool{"vault_note": true, "vault": true}
	case ActionMemoryWrite:
		return map[string]bool{"memory_write": true, "memory": true}
	case ActionForgeBug:
		return map[string]bool{"bug": true}
	case ActionForgeSuggestion:
		return map[string]bool{"suggestion": true}
	}
	return nil
}

// ApplyEnrichExistingDedupe annotates each decision with EnrichExisting
// when its title is Jaccard-near a same-session agent filing of a
// compatible kind. Mutates result.Decisions in place. Title-to-title
// match: same-session duplicates repeat the title near-verbatim even when
// the body differs, and the agent filings carry only a title.
func ApplyEnrichExistingDedupe(result *ArcReviewResult, agentFilings []RecentFiling) {
	if result == nil || len(agentFilings) == 0 {
		return
	}
	threshold := enrichJaccardThreshold()
	for i := range result.Decisions {
		d := &result.Decisions[i]
		if d.Action == ActionNothingToFile {
			continue
		}
		kinds := enrichKindFor(d.Action)
		if len(kinds) == 0 {
			continue
		}
		titleTok := titleTokens(decisionTitle(d))
		if len(titleTok) == 0 {
			continue
		}
		best := EnrichExistingMatch{}
		for _, f := range agentFilings {
			if !kinds[f.Kind] {
				continue
			}
			sim := similarity(titleTok, titleTokens(f.Title))
			if sim > best.Similarity {
				best = EnrichExistingMatch{Kind: f.Kind, Slug: f.Slug, Title: f.Title, Similarity: roundTo(sim, 3)}
			}
		}
		if best.Similarity >= threshold {
			d.EnrichExisting = &best
		}
	}
}

// recentMemoryFilings returns the agent's own MemoryWritten entries since
// `since` as RecentFilings (Kind="memory_write", Title=name) so memory_write
// decisions dedupe against memory the agent already wrote this session.
// MemoryWritten is the one body-heavy kind with a typed event; vault notes
// emit none and are covered by the snapshot extraction instead.
func recentMemoryFilings(ctx context.Context, pool *db.Pool, since time.Time) ([]RecentFiling, error) {
	if pool == nil {
		return nil, nil
	}
	rs, err := pool.DB().QueryContext(ctx, `
		SELECT json_extract(payload, '$.name')
		FROM events
		WHERE type = 'MemoryWritten' AND ts >= ?
		ORDER BY ts DESC
		LIMIT ?`,
		since.UTC().Format("2006-01-02 15:04:05"), recentFilingsCap)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []RecentFiling
	for rs.Next() {
		var name string
		if err := rs.Scan(&name); err != nil {
			return nil, err
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, RecentFiling{Kind: "memory_write", Slug: name, Title: name})
	}
	return out, rs.Err()
}
