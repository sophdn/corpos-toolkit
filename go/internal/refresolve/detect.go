package refresolve

import (
	"context"
	"sort"
)

// Catalogs is the read-only set of name catalogs the rule-based
// detectors match against. Loaded once at server startup (or
// composed in tests) and passed through to Detect.
//
// All slices are CASE-SENSITIVE exact-match lookups except where a
// per-detector function says otherwise. Convert to a set if
// detection volume grows past the current O(N) scan.
type Catalogs struct {
	ChainSlugs    []string // chains.slug from toolkit.db
	TaskSlugs     []string // tasks.slug
	BugSlugs      []string // bugs.slug
	SkillNames    []string // basename of *.toml in skills/
	ToolNames     []string // basename of *.toml in action-manifests/
	ForgeSchemas  []string // basename of *.toml in blueprints/forge-schemas/
	LibrarySlugs  []string // library_entries.slug
	LibraryTitles []string // library_entries.title (lowercase for case-insensitive match)
	// Projects is a closed list; new projects extend
	// known-projects-list maintained alongside this file.
	Projects []string
	// SkillTriggers is the deduplicated, sorted list of every
	// trigger keyword across skills/_manifest.toml (reference-
	// resolution-migration T5). Used by detectSkillTrigger to emit
	// ShapeSkillTrigger references. Empty when the manifest is
	// absent — handlers then no-op for the skill_trigger shape.
	SkillTriggers []string
	// SkillManifest is the parsed manifest the skill_trigger and
	// discipline_skill resolvers consult. May be nil.
	SkillManifest *SkillManifest
	// MemoryTokens is the sorted, deduplicated list of hyphenated
	// identifiers from the auto-memory index (MEMORY.md). Used by
	// detectMemoryEntry to emit ShapeMemoryEntry references.
	// Reference-resolution-migration T10.
	MemoryTokens []string
	// EcosystemTokens is the sorted, deduplicated list of host slugs,
	// service slugs, and host addresses from the local-ecosystem service.
	// Used by detectEcosystemToken to emit ShapeEcosystemToken references.
	// Empty when the ecosystem ships empty / nothing learned yet. chain 435.
	EcosystemTokens []string
	// CanonTokens is the sorted, deduplicated list of canonical names, retired
	// aliases, old paths, and old ports from the ecosystem canon map. Used by
	// detectCanonToken to emit ShapeCanonToken references (canon_resolve).
	CanonTokens []string
	// MemoryIndex is the parsed MEMORY.md the memory_entry resolver
	// consults to map a matched token back to entry bodies. May be nil.
	MemoryIndex *MemoryIndex
}

// DomainTermClassifier abstracts the rubric / classifier path used
// for ShapeDomainTerm detection. T2 wires the Qwen rubric
// (reference-domain-term-detector); T7 hot-swaps a trained
// classifier behind this interface.
//
// IsDomainTerm returns true with a confidence score in [0, 1] if
// the phrase is a project-internal domain term. Implementations
// must be safe for concurrent use and return promptly (≤500ms
// target per the design doc); a failing or unavailable classifier
// should return (false, 0, err) so the detector falls back to a
// permissive default without blocking.
type DomainTermClassifier interface {
	IsDomainTerm(ctx context.Context, phrase string) (bool, float64, error)
}

// Detector is the entry point for Detect. Construct via NewDetector
// and reuse across calls. The detector is stateless beyond its
// configured catalogs + classifier; safe for concurrent use.
type Detector struct {
	cat        Catalogs
	classifier DomainTermClassifier
}

// NewDetector returns a Detector configured with the supplied
// catalogs and domain-term classifier. The classifier may be nil;
// in that case domain-term detection is skipped (rule-based path
// only) and Detect returns only the deterministic shapes.
func NewDetector(cat Catalogs, classifier DomainTermClassifier) *Detector {
	return &Detector{cat: cat, classifier: classifier}
}

// Detect scans the message and returns a deduplicated, sorted list
// of References. The ordering is by StartPos ascending, then
// Shape priority (see shapePriority below) so callers can step
// through the references in source order.
//
// One token may produce multiple References when it matches more
// than one shape; the dispatcher (T3) resolves the ambiguity by
// running resolvers in priority order and short-circuiting on
// single_exact hits.
//
// Returns an empty slice (not nil) for messages with no references.
func (d *Detector) Detect(ctx context.Context, message string) ([]Reference, error) {
	if message == "" {
		return []Reference{}, nil
	}

	refs := []Reference{}

	// Rule-based detectors first — pure Go, deterministic, cheap.
	refs = append(refs, detectChainSlug(message, d.cat.ChainSlugs)...)
	refs = append(refs, detectTaskSlug(message, d.cat.TaskSlugs)...)
	refs = append(refs, detectBugSlug(message, d.cat.BugSlugs)...)
	refs = append(refs, detectPath(message)...)
	refs = append(refs, detectSkillName(message, d.cat.SkillNames)...)
	refs = append(refs, detectProjectName(message, d.cat.Projects)...)
	refs = append(refs, detectToolName(message, d.cat.ToolNames)...)
	refs = append(refs, detectForgeSchema(message, d.cat.ForgeSchemas)...)
	refs = append(refs, detectLibraryEntry(message, d.cat.LibrarySlugs, d.cat.LibraryTitles)...)
	refs = append(refs, detectSkillTrigger(message, d.cat.SkillTriggers)...)
	// Weak-boundary sibling: emits ShapeSkillCandidate refs where
	// the strict boundary check rejects but the loose one accepts.
	// Mutually exclusive with detectSkillTrigger by construction
	// (the new detector skips any match position the strict detector
	// would have accepted).
	refs = append(refs, detectSkillCandidate(message, d.cat.SkillTriggers)...)
	refs = append(refs, detectMemoryEntry(message, d.cat.MemoryTokens)...)
	refs = append(refs, detectEcosystemToken(message, d.cat.EcosystemTokens)...)
	refs = append(refs, detectCanonToken(message, d.cat.CanonTokens)...)

	// Rubric-based domain-term BEFORE the external-technical
	// heuristic, per the design doc §2.2 priority order (domain_term
	// preempts external_technical for the same phrase). Permissive
	// fallback on classifier unavailability.
	if d.classifier != nil {
		dts, err := detectDomainTerm(ctx, message, refs, d.classifier)
		if err == nil {
			refs = append(refs, dts...)
		}
		// Classifier failure is logged at the call site of NewDetector's
		// classifier impl; here we degrade rather than fail Detect.
	}

	// Heuristic external-technical — capitalized multi-word phrases
	// not already matched as a higher-precision shape (including
	// domain_term, which the rubric step above may have just
	// emitted).
	refs = append(refs, detectExternalTechnical(message, refs)...)

	// Friction-shape — whole-message-level pattern match. Always
	// runs last; the detector supersedes the friction-filing-reminder
	// Stop hook by surfacing the filing suggestion at the natural
	// point in the agent's flow (T6 of reference-resolution-substrate).
	refs = append(refs, detectFrictionShape(message)...)

	// Second-pass detectors consume the primary detection result.
	// Discipline-skill triggers fire on shape categories like
	// friction_shape; bridge shapes (vault_candidate, kiwix_bridge)
	// promote domain_term + external_technical primary refs. Both
	// run after the primary detectors (reference-resolution-
	// migration T5).
	refs = append(refs, detectDisciplineSkill(refs)...)
	refs = append(refs, detectBridgeShapes(refs)...)

	return sortAndDedupe(refs), nil
}

// shapePriority returns a small integer for sorting references that
// share a StartPos. Higher-precision shapes sort first so the
// dispatcher's short-circuit logic encounters them in the right
// order. Mirrors the priority order in docs/REFERENCE_RESOLUTION.md
// §2.2.
func shapePriority(s ShapeCategory) int {
	switch s {
	case ShapeCanonToken, ShapeEcosystemToken:
		// Deterministic OWNED resolvers (chain 435 + canon_resolve). They must
		// win the per-token short-circuit over the heuristic project_name / path
		// resolvers when they fire — e.g. a retired token like `mcp-servers`
		// must surface its canon "RETIRED -> corpos-toolkit" answer, not the
		// stale project_name guess at a deleted path. Their catalogs are narrow
		// and learned, so they only fire on a real owned fact. A token is never
		// both canon and ecosystem (disjoint catalogs), so the shared rank is safe.
		return 0
	case ShapeChainSlug:
		return 1
	case ShapeTaskSlug:
		return 2
	case ShapeBugSlug:
		return 3
	case ShapePath:
		return 4
	case ShapeSkillName:
		return 5
	case ShapeProjectName:
		return 6
	case ShapeToolName:
		return 7
	case ShapeForgeSchema:
		return 8
	case ShapeLibraryEntry:
		return 9
	case ShapeDomainTerm:
		return 10
	case ShapeExternalTechnical:
		return 11
	case ShapeFrictionShape:
		return 12
	case ShapeSkillCandidate:
		// Lower priority than ShapeSkillTrigger (which sits among the
		// catalog-shape group). Weak-boundary candidates sort after
		// strict matches if both happen to appear at the same StartPos
		// (which shouldn't happen by construction, but the sort still
		// needs a stable ordering).
		return 13
	default:
		return 99
	}
}

// sortAndDedupe stable-sorts references by (StartPos, ShapePriority)
// and removes exact duplicates (same Token + Shape + StartPos). A
// single token matching multiple shapes is preserved — only literal
// duplicates collapse.
func sortAndDedupe(refs []Reference) []Reference {
	if len(refs) == 0 {
		return []Reference{}
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].StartPos != refs[j].StartPos {
			return refs[i].StartPos < refs[j].StartPos
		}
		return shapePriority(refs[i].Shape) < shapePriority(refs[j].Shape)
	})
	out := make([]Reference, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		key := r.Token + "\x00" + string(r.Shape) + "\x00" + itoa(r.StartPos)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
