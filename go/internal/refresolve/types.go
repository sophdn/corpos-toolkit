package refresolve

import "context"

// ShapeCategory is the closed enum of reference shapes the detector
// recognizes. New shapes are added by registering an entry in
// shape-specific detector files + (in T3) a Resolver implementation.
// Adding a value is a chain-level decision; the values appear in
// telemetry (grounding_events.source_refs entries are tagged with
// shape via SourceRef prefix conventions) and must be stable.
type ShapeCategory string

const (
	ShapeChainSlug         ShapeCategory = "chain_slug"
	ShapeTaskSlug          ShapeCategory = "task_slug"
	ShapeBugSlug           ShapeCategory = "bug_slug"
	ShapePath              ShapeCategory = "path"
	ShapeSkillName         ShapeCategory = "skill_name"
	ShapeProjectName       ShapeCategory = "project_name"
	ShapeToolName          ShapeCategory = "tool_name"
	ShapeForgeSchema       ShapeCategory = "forge_schema"
	ShapeLibraryEntry      ShapeCategory = "library_entry"
	ShapeDomainTerm        ShapeCategory = "domain_term"
	ShapeExternalTechnical ShapeCategory = "external_technical"
	// ShapeFrictionShape is reserved for T6 (hook supersession).
	// The detector emits it via a separate friction-pattern detector
	// added in that task; T2 only exports the constant.
	ShapeFrictionShape ShapeCategory = "friction_shape"
	// Parse-context-only shapes (reference-resolution-migration T5).
	// The detector + per-shape resolvers land in T5's per-resolver
	// phases; the constants exist here so the filter-cache policy
	// table (cache.go) can declare per-shape rules without forward
	// references.
	ShapeSkillTrigger ShapeCategory = "skill_trigger"
	ShapeMemoryEntry  ShapeCategory = "memory_entry"
	// ShapeEcosystemToken is a host / service / host-address token from the
	// local-ecosystem service (chain 435). Its resolver returns the
	// deterministic access summary so parse_context surfaces "do I have access
	// to X" at orient-time, before a cold agent can wrongly deny.
	ShapeEcosystemToken ShapeCategory = "ecosystem_token"
	// ShapeCanonToken is a canonical-name / retired-alias / old-path / old-port
	// token from the ecosystem canon map (canon_resolve extraction). Its resolver
	// returns the current canonical identity so parse_context surfaces "X is
	// retired -> canonical is Y" the moment a stale token appears.
	ShapeCanonToken      ShapeCategory = "canon_token"
	ShapeVaultCandidate  ShapeCategory = "vault_candidate"
	ShapeKiwixBridge     ShapeCategory = "kiwix_bridge"
	ShapeDisciplineSkill ShapeCategory = "discipline_skill"
	// ShapeSkillCandidate is the weak-boundary skill-trigger sibling:
	// emitted when a trigger keyword from skills/_manifest.toml
	// appears inside a kebab token (e.g., "parse-context" as a
	// prefix of the chain slug "parse-context-skill-body-..."),
	// where the strict ShapeSkillTrigger detector correctly rejects
	// the match (boundaryOKCatalog rejects hyphen neighbors). Surfaces
	// the skill as a candidate without reaching `use_directly`, and
	// never triggers inline-body delivery. Filed as suggestion
	// `weak-boundary-skill-candidate-emit-when-trigger-keyword-prefixes-a-kebab-slug`.
	ShapeSkillCandidate ShapeCategory = "skill_candidate"
)

// ConfidenceTier is the dispatcher's classification (T3 lands the
// classification rules). T2 emits Reference.Confidence as a float
// in [0, 1]; T3's dispatcher buckets per resolver into one of these
// tiers.
type ConfidenceTier string

const (
	TierSingleExact ConfidenceTier = "single_exact"
	TierFuzzyMulti  ConfidenceTier = "fuzzy_multi"
	TierWeakDomain  ConfidenceTier = "weak_domain"
	TierNoHit       ConfidenceTier = "no_hit"
	// TierLowConfidenceFallback marks orientation pointers produced
	// by the low-confidence kiwix fallback (chain parse-context-lean-
	// orienting T8). Additive — consumers that already pattern-match
	// on single_exact / fuzzy_multi / weak_domain / no_hit continue
	// unchanged; new consumers can recognise these as hail-mary surfaces
	// and present them with the appropriate caveat.
	TierLowConfidenceFallback ConfidenceTier = "low_confidence_fallback"
)

// Reference is one detected reference-shape token in the source
// message. The detector emits one Reference per (token, shape) pair;
// the same token may appear under multiple shapes (e.g., the token
// looks like both a chain_slug regex match and a task_slug regex
// match) — the dispatcher resolves the ambiguity per the priority
// order in docs/REFERENCE_RESOLUTION.md §2.2.
type Reference struct {
	// Token is the substring extracted from the message (verbatim,
	// preserving case).
	Token string
	// Shape is the detector's classification.
	Shape ShapeCategory
	// Confidence is the detector's confidence in [0, 1]. 1.0 for
	// exact-list matches; rubric output for domain-term; 0.5 for
	// heuristic-only external-technical.
	Confidence float64
	// DetectionMethod names the path that produced the Reference:
	// "regex+list_match", "rubric", "filename_match", "heuristic",
	// etc. Used for debug surfaces and telemetry slicing.
	DetectionMethod string
	// StartPos / EndPos are byte offsets in the source message.
	// Inclusive start, exclusive end (slice-friendly).
	StartPos, EndPos int
}

// Candidate is one result returned by a resolver. Score values are
// resolver-specific and not directly comparable across resolvers;
// the dispatcher (T3) normalizes for tier classification and reranks
// candidates from different resolvers via T7's cross-encoder when
// it lands.
type Candidate struct {
	ID         string  // resolver-specific identifier (chain slug, file path, …)
	Title      string  // human-readable label
	Score      float64 // resolver-specific score; not comparable across resolvers
	SourceRef  string  // canonical pointer (lands in grounding_events.source_refs)
	DebugNotes string  // optional; "rank 1 in chain_find" / "fts5 score 0.83"
}

// HitSet is the output of one Resolver.Resolve call. T3 lands the
// resolver implementations; T2 declares the shape so the detector
// and dispatcher can be implemented in parallel.
type HitSet struct {
	ResolverName    string
	ConfidenceTier  ConfidenceTier
	Candidates      []Candidate
	RetrievalCostMs int64
	// Err is non-nil if the underlying tool failed; in that case
	// ConfidenceTier=TierNoHit and Candidates is empty. The
	// dispatcher continues with other resolvers when Err is set;
	// the error surfaces in the action's response envelope.
	Err error
}

// ResolverCostHint informs the dispatcher's ordering and budget.
type ResolverCostHint struct {
	// TypicalMs is the expected latency in milliseconds; the
	// dispatcher uses 4x this as the per-resolver budget cap.
	TypicalMs int64
}

// Resolver wraps the existing tool for one shape category and
// returns ranked candidates. T3 lands the resolver implementations
// (chain_resolver, task_resolver, bug_resolver, path_resolver,
// skill_resolver, project_resolver, tool_resolver, schema_resolver,
// library_resolver, domain_term_resolver, external_technical_resolver).
type Resolver interface {
	Shape() ShapeCategory
	Resolve(ctx context.Context, ref Reference) (HitSet, error)
	Cost() ResolverCostHint
}
