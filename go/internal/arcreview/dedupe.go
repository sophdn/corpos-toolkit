package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"toolkit/internal/db"
)

// F2 of chain arc-close-filing-review-dedupe-and-noise-reduction:
// pre-filing dedupe against existing artifacts. For each proposed
// forge_bug / forge_suggestion / forge_vault_note decision, compute
// Jaccard similarity on title tokens against existing artifacts of
// the same kind. When similarity ≥ threshold (default 0.30 per F1
// corpus tuning), mark the decision as deduped — the partition
// step demotes it to a less-aggressive bucket.
//
// Distinct from F4 (CheckBoilerplate): F4 rejects by CONTENT shape;
// F2 rejects by SIMILARITY to existing artifacts. They compose: a
// decision passes only if both filters pass.

// dedupeJaccardEnvVar is the env-var name for the similarity
// threshold override. Useful for live tuning + A/B without a
// rebuild. Empty / unparseable env vars fall back to the default.
const dedupeJaccardEnvVar = "TOOLKIT_ARCCLOSE_DEDUPE_JACCARD_THRESHOLD"

// dedupeJaccardDefaultThreshold matches the F1 corpus-tuned value
// (docs/ARC_CLOSE_FILING_REVIEW_DEDUPE.md §4). 0.30 catches the
// "Orphan Precommit-fmt Stashes" / "Automatic cleanup of orphaned
// precommit-fmt stashes" near-duplicate pair (Jaccard ~0.29) that
// the original conservative 0.40 missed.
const dedupeJaccardDefaultThreshold = 0.30

// dedupeJaccardThreshold reads the env var or returns the default.
func dedupeJaccardThreshold() float64 {
	raw := strings.TrimSpace(os.Getenv(dedupeJaccardEnvVar))
	if raw == "" {
		return dedupeJaccardDefaultThreshold
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1 {
		return dedupeJaccardDefaultThreshold
	}
	return v
}

// stopwords is the closed set of low-signal tokens excluded from
// Jaccard comparison. Matches the Python script's stopword list at
// measure/arc-close-corpus-classify.py so corpus replay yields the
// same Jaccard numbers the F1 design doc cites.
var stopwords = map[string]struct{}{
	"and": {}, "or": {}, "the": {}, "for": {}, "into": {}, "with": {},
	"this": {}, "that": {}, "from": {}, "session": {}, "retro": {},
	"after": {}, "before": {}, "task": {}, "bug": {}, "via": {},
	"should": {}, "may": {}, "could": {}, "would": {}, "their": {},
	"are": {}, "not": {}, "but": {}, "all": {}, "now": {}, "also": {},
}

// tokenSplitRE matches token boundaries: any non-alphanumeric, non-
// hyphen, non-underscore character. Mirrors the Python regex used in
// the F1 classifier so token sets agree between Go runtime and
// corpus-replay.
var tokenSplitRE = regexp.MustCompile(`[a-z0-9][a-z0-9\-_]{2,}`)

// TitleTokens returns the lowercased token set for a title or any
// short label string. Filters tokens shorter than 3 characters and
// the stopword set. Used both for the proposed decision's title and
// for the existing artifacts' titles in the similarity index.
// Exported so tests can construct token-set inputs to FindBestMatch
// matching the production code's tokenisation.
func TitleTokens(s string) map[string]struct{} { return titleTokens(s) }

func titleTokens(s string) map[string]struct{} {
	if s == "" {
		return nil
	}
	matches := tokenSplitRE.FindAllString(strings.ToLower(s), -1)
	out := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		if _, isStop := stopwords[m]; isStop {
			continue
		}
		out[m] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// jaccard computes the standard Jaccard similarity |A∩B| / |A∪B|.
// Returns 0 when either set is empty.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	unionSize := len(a) + len(b) - inter
	if unionSize == 0 {
		return 0
	}
	return float64(inter) / float64(unionSize)
}

// minContainment computes |A∩B| / min(|A|, |B|) — the containment
// similarity. Rewards "small set is contained in big set" cases that
// plain Jaccard penalizes by inflating the denominator with the big
// set's unique-to-itself tokens. Used as a complement to Jaccard:
// dedupe catches a hit when EITHER metric is above threshold.
//
// Motivating case (session 2026-05-23): proposed bug "Agent did not
// utilize batch forge capability" (24 tokens) vs existing "Agent makes
// N sequential forge calls instead of one work.batch..." (38 tokens
// with rich problem_statement prose). Plain Jaccard = 8/54 = 0.148
// (below the 0.30 threshold). Containment = 8/min(24,38) = 0.333
// (above threshold). The dup gets caught.
//
// Symmetric in the sense that whichever side is shorter wins; we
// don't bias toward "proposed contained in existing" vs "existing
// contained in proposed" — either direction signals duplicate authoring.
func minContainment(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	minSize := len(a)
	if len(b) < minSize {
		minSize = len(b)
	}
	if minSize == 0 {
		return 0
	}
	return float64(inter) / float64(minSize)
}

// similarity returns the max of jaccard and minContainment. Either
// metric crossing threshold flags the pair as a duplicate; the
// composition catches both "symmetric near-duplicate" (Jaccard) and
// "short paraphrase of a verbose original" (containment) cases without
// the operator having to tune two separate thresholds.
func similarity(a, b map[string]struct{}) float64 {
	j := jaccard(a, b)
	c := minContainment(a, b)
	if c > j {
		return c
	}
	return j
}

// ExistingArtifact is one row from the bug/suggestion/vault/memory
// index. Slug and Title carry through to the dedupe match output so
// the telemetry consumer can see which existing artifact the proposed
// decision matched. ProblemStatement is included so the Jaccard
// signature can cover both the title AND the longer-form rationale —
// without it, semantically-identical-but-textually-different titles
// (e.g. "Agent makes N sequential forge calls instead of one work.batch"
// vs the proposed "Agent did not utilize batch forge capability") fall
// below the threshold even though their problem statements overlap
// heavily. Bug `arc-close-filing-review-no-dedup-against-existing-
// artifacts`.
type ExistingArtifact struct {
	Slug             string
	Title            string
	ProblemStatement string
}

// ExistingArtifactsByKind is the index F2 compares against. Loaded
// once per arc-close fire before the dedupe pass; cheap (a few
// hundred rows per kind on the live DB; no per-row queries during
// the comparison loop).
//
// Memories tracks current auto-memory entries, read from the proj_memories
// projection (chain substrate-health-audit-projections T7 gave memories a
// projection so this stopped JSON-extracting the events ledger directly).
// Each entry's Slug is the memory name; Title is the description;
// ProblemStatement is empty (MemoryWritten carries no body, only
// body_length_bytes). The pre-fix bug
// `arc-close-filing-review-no-dedup-against-existing-artifacts`
// observed this session 2026-05-23: the review fired twice on
// consecutive commits and both times proposed a memory_write for
// `check-for-batch-capability` even though the canonically-equivalent
// `feedback-batch-primitive-for-multi-op-mcp` was already in the
// session's memory dir.
type ExistingArtifactsByKind struct {
	Bugs        []ExistingArtifact
	Suggestions []ExistingArtifact
	VaultNotes  []ExistingArtifact
	Memories    []ExistingArtifact
}

// LoadExistingArtifactsForDedupe queries the projection tables +
// vault knowledge_pointers for open / recently-resolved artifacts.
// Fail-open: any per-kind query failure logs and returns an empty
// slice for that kind. F2's dedupe degrades to "no match" rather
// than blocking the fire on a DB hiccup.
//
// Scope: includes bugs / suggestions in all statuses (the dedupe
// catches "you're proposing the same bug as one that just got
// fixed last session" too). Vault notes pulled from
// knowledge_pointers where source_type='vault' — the canonical
// literal the ingestion path writes (pointers/normalize.go,
// integrity.go, curation/sources/vault_note.go). A prior 'vault-note'
// literal here matched 0 rows, silently disabling the vault-note
// dedup arm (bug arc-close-dedup-misses-semantically-duplicative-
// decisions); every in-memory dedupe test bypassed this query so it
// shipped untested.
func LoadExistingArtifactsForDedupe(ctx context.Context, pool *db.Pool, project string) (ExistingArtifactsByKind, error) {
	if pool == nil {
		return ExistingArtifactsByKind{}, fmt.Errorf("arcreview dedupe: pool is nil")
	}
	out := ExistingArtifactsByKind{}

	if bugs, err := queryArtifactsByProject(ctx, pool, `
		SELECT slug, title, COALESCE(problem_statement, '') FROM proj_current_bugs
		WHERE project_id = ?
		ORDER BY filed_at DESC`, project); err == nil {
		out.Bugs = bugs
	}

	if sugs, err := queryArtifactsByProject(ctx, pool, `
		SELECT slug, title, COALESCE(problem_statement, '') FROM proj_current_suggestions
		WHERE project_id = ?
		ORDER BY filed_at DESC`, project); err == nil {
		out.Suggestions = sugs
	}

	if vault, err := queryArtifactsNoArgs(ctx, pool, `
		SELECT source_ref AS slug, COALESCE(question, '') AS title, COALESCE(description, '')
		FROM knowledge_pointers
		WHERE source_type = 'vault'`); err == nil {
		out.VaultNotes = vault
	}

	// Memories: read the proj_memories projection (chain substrate-health-
	// audit-projections T7). Previously this JSON-extracted MemoryWritten
	// payloads from the events ledger directly — the last entity kind doing
	// so; now it reads the same clean projection surface every sibling kind
	// uses.
	//
	// NOT project-scoped — matches the vault-note sibling query above. The
	// auto-memory dir is a single GLOBAL namespace keyed by filename, so a
	// memory filed under ANY project is a real dedup candidate; scoping to
	// one project would miss cross-project dups (and the motivating bug
	// `arc-close-filing-review-no-dedup-against-existing-artifacts` was a
	// MISSED dup). proj_memories is keyed by name (last-write-wins), so this
	// returns one row per distinct memory — cleaner than the old per-event
	// rows. problem_statement is empty because MemoryWritten carries no body
	// field (only body_length_bytes); the prior events query's
	// json_extract($.body) likewise always yielded '', so the dedup
	// signature (description only) is unchanged.
	if mems, err := queryArtifactsNoArgs(ctx, pool, `
		SELECT name AS slug,
		       COALESCE(description, '') AS title,
		       '' AS problem_statement
		FROM proj_memories
		ORDER BY filed_at DESC`); err == nil {
		out.Memories = mems
	}

	return out, nil
}

// queryArtifactsByProject runs a project-scoped query that takes a
// single string project argument. Typed (string) rather than
// variadic-any so the forbidigo lint stays clean.
func queryArtifactsByProject(ctx context.Context, pool *db.Pool, query, project string) ([]ExistingArtifact, error) {
	rows, err := pool.DB().QueryContext(ctx, query, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanArtifactRows(rows)
}

// queryArtifactsNoArgs runs a query that takes no bind args.
func queryArtifactsNoArgs(ctx context.Context, pool *db.Pool, query string) ([]ExistingArtifact, error) {
	rows, err := pool.DB().QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanArtifactRows(rows)
}

// scanArtifactRows is the shared row-iteration body for both query
// helpers.
func scanArtifactRows(rows *sql.Rows) ([]ExistingArtifact, error) {
	out := []ExistingArtifact{}
	for rows.Next() {
		var a ExistingArtifact
		if err := rows.Scan(&a.Slug, &a.Title, &a.ProblemStatement); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DedupeMatch is the F2 match record. Slug names the existing
// artifact that the proposed decision is duplicating; Similarity
// is the Jaccard score (≥ threshold by construction). Surfaced on
// FilingDecision.DedupedAgainst so the dispatcher's partition step
// knows to demote, and so telemetry consumers see why.
type DedupeMatch struct {
	Slug       string  `json:"slug"`
	Similarity float64 `json:"similarity"`
}

// FindBestMatch scans the existing-artifact index for the proposed
// decision's title+problem_statement signature; returns the highest-
// similarity match if it meets threshold. Returns nil when nothing
// matches.
//
// proposedSignature should be the title-plus-problem_statement (or
// memory-name-plus-body) token set so the Jaccard has enough tokens
// to discriminate. Title-only signatures missed the canonical case
// from session 2026-05-23 — "Agent makes N sequential forge calls..."
// vs "Agent did not utilize batch forge capability" Jaccard ~0.18 at
// the title-only level vs >0.30 once both problem statements were
// folded in. Bug `arc-close-filing-review-no-dedup-against-existing-
// artifacts`.
func FindBestMatch(action ActionKind, proposedSignature map[string]struct{}, index ExistingArtifactsByKind, threshold float64) *DedupeMatch {
	if len(proposedSignature) == 0 {
		return nil
	}
	var candidates []ExistingArtifact
	switch action {
	case ActionForgeBug:
		candidates = index.Bugs
	case ActionForgeSuggestion:
		candidates = index.Suggestions
	case ActionForgeVaultNote:
		candidates = index.VaultNotes
	case ActionMemoryWrite:
		candidates = index.Memories
	default:
		return nil
	}
	best := DedupeMatch{}
	for _, c := range candidates {
		sim := similarity(proposedSignature, titleTokens(c.Title+" "+c.ProblemStatement))
		if sim > best.Similarity {
			best = DedupeMatch{Slug: c.Slug, Similarity: sim}
		}
	}
	if best.Similarity >= threshold {
		// Round to 3 decimal places for stable telemetry output.
		best.Similarity = roundTo(best.Similarity, 3)
		return &best
	}
	return nil
}

// ApplyExistingArtifactDedupe walks the result's Decisions slice and
// applies the F2 match against the supplied index. Decisions matching
// the threshold acquire a DedupedAgainst field; the partition step
// then demotes them (auto_execute → surface_for_confirm; surface_for_
// confirm → skip).
//
// Threshold value reads from TOOLKIT_ARCCLOSE_DEDUPE_JACCARD_THRESHOLD
// env var with a default of 0.30 per the F1 corpus tuning.
//
// Mutates `result.Decisions` in place — appending DedupedAgainst to
// matched entries; non-matched entries pass through unchanged.
func ApplyExistingArtifactDedupe(result *ArcReviewResult, index ExistingArtifactsByKind) {
	if result == nil {
		return
	}
	threshold := dedupeJaccardThreshold()
	for i := range result.Decisions {
		d := &result.Decisions[i]
		// Compose the same title+body signature F3 uses so the two
		// dedupe filters agree on what "the same decision" means; a
		// signature that catches duplicates within-session would
		// otherwise miss across-session and vice versa. See
		// PayloadSignature.
		sig := PayloadSignature(d)
		match := FindBestMatch(d.Action, sig, index, threshold)
		if match == nil {
			continue
		}
		d.DedupedAgainst = match
	}
}

// decisionTitle extracts the title-like identifier from each action's
// payload. For forge_bug / forge_suggestion / forge_vault_note this is
// the Title field. For memory_write it's the memory Name + Description
// (Name alone is too short to feed Jaccard usefully; Description adds
// the one-line summary that captures the rule). Returns empty for
// nothing_to_file / skill_update.
func decisionTitle(d *FilingDecision) string {
	if len(d.Payload) == 0 {
		return ""
	}
	switch d.Action {
	case ActionForgeBug:
		var p ForgeBugPayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.Title
		}
	case ActionForgeSuggestion:
		var p ForgeSuggestionPayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.Title
		}
	case ActionForgeVaultNote:
		var p ForgeVaultNotePayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.Title
		}
	case ActionMemoryWrite:
		var p MemoryWritePayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.Name + " " + p.Description
		}
	}
	return ""
}

// roundTo rounds f to digits decimal places. Helper.
func roundTo(f float64, digits int) float64 {
	mul := 1.0
	for i := 0; i < digits; i++ {
		mul *= 10
	}
	shifted := f*mul + 0.5
	return float64(int64(shifted)) / mul
}
