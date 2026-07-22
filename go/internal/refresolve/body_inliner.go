package refresolve

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Skill-body inlining for parse_context envelopes (chain 602).
//
// When parse_context emits RecommendedAction=PresentUseDirectly for a
// skill_trigger or discipline_skill ref, the body should arrive WITH
// the envelope rather than as a pointer. Closes the procedural-cue
// gap named in bug 1429's third observation: the dispatcher's "body
// at <path>" pointer doesn't reliably trigger a Read; pushing the
// body into the envelope converts the cue from "pull on demand" to
// "pushed on detection."
//
// Design doc: process-docs/adhoc/parse-context-inline-body-design-2026-05-20.md.
// Survey:     process-docs/adhoc/skill-body-inlining-survey-2026-05-20.md.

const (
	// InlineBodyEnvVar is the kill-switch for body inlining. Default
	// is ENABLED; set to "0" / "false" / "no" / "off" to disable
	// server-wide. Per-request override via parseContextParams.InlineSkillBodies.
	//
	// The default flipped from OFF to ON on 2026-05-21 (chain 602 T6
	// follow-up): the original env-var-on-by-default-OFF rollout shipped
	// 2026-05-20 with TOOLKIT_PARSE_CONTEXT_INLINE_BODIES=1 set only in
	// go/launch.sh for the HTTP daemon. Stdio MCP children spawned by
	// Claude Code never received the env var (no .mcp.json env block was
	// authored), so the feature was silently dormant for every
	// interactive agent session for 4 days. Flipping the default to ON
	// makes the feature operational across all surfaces without
	// per-environment config plumbing; the env var stays as a
	// kill-switch for emergencies.
	InlineBodyEnvVar = "TOOLKIT_PARSE_CONTEXT_INLINE_BODIES"
	// DefaultInlineBudgetBytes is the envelope-total cap on inlined body
	// bytes across all eligible refs. Chosen so 2-3 inline-truncate
	// (2-8 KB) skills fit comfortably; pointer-only (>8 KB) bodies fall
	// to BodySummary instead.
	DefaultInlineBudgetBytes = 20 * 1024 // 20 KB
	// MaxInlinedBytesPerSkill is the per-skill cap. Bodies above this
	// fall to BodySummary regardless of envelope room.
	MaxInlinedBytesPerSkill = 8 * 1024 // 8 KB
	// BodySummaryBytes is the head-N size for the pointer-only tier
	// fallback. Enough cue to know whether to fetch the full body.
	BodySummaryBytes = 500
	// SourceRefSkillPrefix is the scheme prefix the skill resolvers stamp
	// onto Candidate.SourceRef. The body inliner strips it to recover the
	// repo-relative body path.
	SourceRefSkillPrefix = "skill:"
)

// BodyCache memoises skill body reads by absolute path. Mtime check
// per lookup; stale entries reload. Cheap (~50µs stat) and amortises
// across handler calls (the same skill body is read once per session,
// not once per envelope).
type BodyCache struct {
	mu      sync.RWMutex
	entries map[string]bodyCacheEntry
}

type bodyCacheEntry struct {
	content []byte
	mtime   time.Time
}

// NewBodyCache constructs an empty skill-body cache. Server startup
// wires one of these into HandlerDeps so reads amortise across calls;
// tests can omit it and the inliner will build a per-call fallback.
func NewBodyCache() *BodyCache {
	return &BodyCache{entries: make(map[string]bodyCacheEntry)}
}

// get returns the body bytes for the given absolute path, reading
// from disk if absent or stale. Returns (nil, err) if the file can't
// be read; the caller treats this as "no body to inline" and proceeds
// without filling Body* fields.
func (c *BodyCache) get(absPath string) ([]byte, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}
	mtime := info.ModTime()

	c.mu.RLock()
	entry, ok := c.entries[absPath]
	c.mu.RUnlock()
	if ok && entry.mtime.Equal(mtime) {
		return entry.content, nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[absPath] = bodyCacheEntry{content: content, mtime: mtime}
	c.mu.Unlock()
	return content, nil
}

// inlineBodyOpts bundles the per-call inputs to applyBodyInlining.
type inlineBodyOpts struct {
	// enabled is the resolved feature-flag state (env var + per-request
	// override). When false, applyBodyInlining is a no-op.
	enabled bool
	// envelopeBudget is the total inlined-body byte cap for this envelope.
	envelopeBudget int
	// repoRoot is prepended to relative body paths from the manifest.
	repoRoot string
	// manifest provides body_path → bucket lookup for precedence ranking.
	// May be nil; without it, precedence falls back to alphabetic on path.
	manifest *SkillManifest
	// cache is the body bytes cache. Must be non-nil when enabled.
	cache *BodyCache
}

// resolveInlineOpts derives the effective inlineBodyOpts for one call,
// applying the precedence: per-request override > env var > default OFF.
// manifest is the catalogs-loaded SkillManifest (cats.SkillManifest);
// passed explicitly so the inliner doesn't reach into a global.
func resolveInlineOpts(p parseContextParams, deps HandlerDeps, manifest *SkillManifest) inlineBodyOpts {
	// Default ON since 2026-05-21 (see InlineBodyEnvVar comment). The
	// env var is now a kill-switch — set to "0" / "false" / "no" /
	// "off" to disable. Per-request override (p.InlineSkillBodies)
	// still wins when supplied.
	enabled := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv(InlineBodyEnvVar))) {
	case "0", "false", "no", "off":
		enabled = false
	}
	if p.InlineSkillBodies != nil {
		enabled = *p.InlineSkillBodies
	}
	budget := p.InlineBudgetBytes
	if budget <= 0 {
		budget = DefaultInlineBudgetBytes
	}
	cache := deps.BodyCache
	if cache == nil && enabled {
		// Lazy-init a per-call cache so tests that omit BodyCache still
		// exercise the inlining path. Production wiring should pass a
		// long-lived cache so reads amortise across handler calls.
		cache = NewBodyCache()
	}
	return inlineBodyOpts{
		enabled:        enabled,
		envelopeBudget: budget,
		repoRoot:       deps.RepoRoot,
		manifest:       manifest,
		cache:          cache,
	}
}

// applyBodyInlining walks refs and fills BodyInlined / BodyTruncated /
// BodySummary / BodyBytes on the eligible ones. Mutates refs in place
// and returns (totalInlinedBytes, inlinedRefsCount) for envelope-level
// telemetry.
//
// Eligibility (per design §2.2):
//   - Shape ∈ {SkillTrigger, DisciplineSkill}
//   - ConfidenceTier == TierSingleExact (= RecommendedAction PresentUseDirectly)
//   - Body file resolvable and readable
//
// Precedence when envelope budget pressed:
//   - bucket: keep-ambient (0) > condense-lazy (1) > pure-lazy (2) > unknown (3)
//   - tiebreaker: alphabetic on body_path
//
// T3 ships head-N truncation; T4 replaces with the load-bearing-section
// preserving heuristic.
func applyBodyInlining(refs []ResolvedReference, opts inlineBodyOpts) (inlinedBytes, inlinedRefs int) {
	if !opts.enabled || opts.cache == nil {
		return 0, 0
	}

	// Step 1: collect indices of eligible refs alongside their body
	// metadata. Two-pass so we can apply precedence before deciding
	// what to inline.
	type candidate struct {
		idx      int    // index into refs
		bodyPath string // repo-relative path from SourceRef
		bucket   string // manifest bucket for precedence
		content  []byte // loaded body bytes
	}
	var candidates []candidate
	for i := range refs {
		r := &refs[i]
		if !isInlineEligible(r) {
			continue
		}
		bodyPath := stripSkillSourcePrefix(r.TopCandidates[0].SourceRef)
		if bodyPath == "" {
			continue
		}
		absPath := resolveBodyAbsPath(opts.repoRoot, bodyPath)
		content, err := opts.cache.get(absPath)
		if err != nil {
			// File missing or unreadable: leave Body* zero; the agent
			// still has the pointer in PresentedAs.
			continue
		}
		candidates = append(candidates, candidate{
			idx:      i,
			bodyPath: bodyPath,
			bucket:   lookupBucket(opts.manifest, bodyPath),
			content:  content,
		})
	}
	if len(candidates) == 0 {
		return 0, 0
	}

	// Step 2: rank by precedence (bucket asc, then alphabetic).
	sort.SliceStable(candidates, func(i, j int) bool {
		bi, bj := bucketRank(candidates[i].bucket), bucketRank(candidates[j].bucket)
		if bi != bj {
			return bi < bj
		}
		return candidates[i].bodyPath < candidates[j].bodyPath
	})

	// Step 3: walk in precedence order; for each candidate, decide
	// inline-whole / inline-truncated / summary based on tier (size
	// classification) and remaining envelope budget.
	//
	// Dedup pass: a single message may contain multiple trigger
	// keywords resolving to the SAME skill (e.g. 'golang' + 'go-test'
	// both → go-conventions). Without dedup, each ref gets its own
	// full inlined copy. We track first-inlined bodyPath → ref index
	// and emit a BodyInlinedFromRefIndex back-reference on subsequent
	// refs instead of duplicating the body. envelope inlinedBytes
	// counts each unique body once.
	inlinedRefIndexByPath := make(map[string]int, len(candidates))
	remaining := opts.envelopeBudget
	for _, c := range candidates {
		r := &refs[c.idx]
		r.BodyBytes = len(c.content)
		// Dedup check before any body assignment / budget debit.
		if priorIdx, seen := inlinedRefIndexByPath[c.bodyPath]; seen {
			idxCopy := priorIdx
			r.BodyInlinedFromRefIndex = &idxCopy
			continue
		}
		switch classifyBody(len(c.content)) {
		case tierInlineClean:
			// Always inline whole — under 2 KB; trivial budget impact.
			r.BodyInlined = string(c.content)
			remaining -= len(c.content)
			inlinedBytes += len(c.content)
			inlinedRefs++
			inlinedRefIndexByPath[c.bodyPath] = c.idx
		case tierInlineTruncate:
			// Inline whole if both per-skill cap and envelope budget allow.
			perSkillCap := MaxInlinedBytesPerSkill
			if len(c.content) <= perSkillCap && len(c.content) <= remaining {
				r.BodyInlined = string(c.content)
				remaining -= len(c.content)
				inlinedBytes += len(c.content)
				inlinedRefs++
				inlinedRefIndexByPath[c.bodyPath] = c.idx
				continue
			}
			// Truncate to min(per-skill cap, remaining envelope room).
			room := perSkillCap
			if remaining < room {
				room = remaining
			}
			if room < BodySummaryBytes {
				// Not enough room even for a useful truncation: fall back
				// to summary so the agent gets at least the cue.
				summary := truncateHeadN(c.content, BodySummaryBytes)
				r.BodySummary = string(summary)
				inlinedBytes += len(summary)
				inlinedRefs++
				inlinedRefIndexByPath[c.bodyPath] = c.idx
				continue
			}
			truncated := truncatePreservingLoadBearing(c.content, room)
			r.BodyInlined = string(truncated)
			r.BodyTruncated = true
			remaining -= len(truncated)
			inlinedBytes += len(truncated)
			inlinedRefs++
			inlinedRefIndexByPath[c.bodyPath] = c.idx
		case tierPointerOnly:
			// Never inline whole; emit summary only. Summary draws from a
			// small fixed budget that doesn't count against per-skill cap
			// (the cue is small enough that 17 pointer-only summaries
			// together stay well under any sane envelope).
			summary := truncateHeadN(c.content, BodySummaryBytes)
			r.BodySummary = string(summary)
			inlinedBytes += len(summary)
			inlinedRefs++
			inlinedRefIndexByPath[c.bodyPath] = c.idx
		}
	}
	return inlinedBytes, inlinedRefs
}

// isInlineEligible returns true iff the ref is a skill-shaped
// TierSingleExact with a Candidate carrying a skill: SourceRef.
func isInlineEligible(r *ResolvedReference) bool {
	if r.RecommendedAction != PresentUseDirectly {
		return false
	}
	if r.Shape != ShapeSkillTrigger && r.Shape != ShapeDisciplineSkill {
		return false
	}
	if len(r.TopCandidates) == 0 {
		return false
	}
	return strings.HasPrefix(r.TopCandidates[0].SourceRef, SourceRefSkillPrefix)
}

// stripSkillSourcePrefix recovers the body path from a Candidate.SourceRef
// of the form "skill:skills/<name>" or "skill:skills/<name>.md".
func stripSkillSourcePrefix(sourceRef string) string {
	return strings.TrimPrefix(sourceRef, SourceRefSkillPrefix)
}

// resolveBodyAbsPath turns a manifest body_path (e.g. "skills/parse-context-first-call"
// or "skills/reference-resolution.md") into the absolute path of the
// markdown file to read. Directory-shaped entries resolve to <dir>/SKILL.md.
func resolveBodyAbsPath(repoRoot, bodyPath string) string {
	full := filepath.Join(repoRoot, bodyPath)
	if strings.HasSuffix(bodyPath, ".md") {
		return full
	}
	return filepath.Join(full, "SKILL.md")
}

// lookupBucket finds the manifest entry for bodyPath and returns its
// bucket. Returns empty string if not found; bucketRank then treats it
// as the lowest priority.
func lookupBucket(manifest *SkillManifest, bodyPath string) string {
	if manifest == nil {
		return ""
	}
	for _, e := range manifest.Skills {
		if e.BodyPath == bodyPath {
			return e.Bucket
		}
	}
	return ""
}

// bucketRank orders buckets for inlining precedence. Lower = higher priority.
func bucketRank(bucket string) int {
	switch bucket {
	case "keep-ambient":
		return 0
	case "condense-lazy":
		return 1
	case "pure-lazy":
		return 2
	default:
		return 3
	}
}

// bodyTier classifies a body by size into one of the three inline tiers.
type bodyTier int

const (
	tierInlineClean    bodyTier = iota // < 2 KB
	tierInlineTruncate                 // 2-8 KB
	tierPointerOnly                    // > 8 KB
)

func classifyBody(size int) bodyTier {
	switch {
	case size < 2*1024:
		return tierInlineClean
	case size <= 8*1024:
		return tierInlineTruncate
	default:
		return tierPointerOnly
	}
}

// truncateHeadN returns the first n bytes of content, or content
// unchanged when shorter. Used for the pointer-only-tier BodySummary
// fallback, where the cue lives in the head (frontmatter + title +
// first paragraph). For inline-truncation use truncatePreservingLoadBearing.
func truncateHeadN(content []byte, n int) []byte {
	if len(content) <= n {
		return content
	}
	return content[:n]
}

// sectionPriority orders SKILL.md H2 sections for truncation. Lower =
// keep longer. Section bodies (intro + H2 chunks) are dropped in
// reverse priority order until the budget fits.
type sectionPriority int

const (
	// priorityMustKeep guards the procedurally load-bearing sections
	// (Fire rule, Skip rule, How to apply, When NOT to apply,
	// Calibration, Anti-patterns, Pre-send ritual, TL;DR, Failure mode).
	priorityMustKeep sectionPriority = iota
	priorityMidPriority
	// priorityElideFirst marks history / context / exemplar sections
	// (Anchors, Open questions, Why this skill exists, anything matching
	// "history" or "example"). Dropped before mid-priority on overflow.
	priorityElideFirst
)

// loadBearingHeadings names H2 sections that are procedurally load-bearing
// across Sophi's discipline-skill bodies. Match is case-insensitive
// substring on the heading text.
var loadBearingHeadings = []string{
	"fire rule",
	"skip rule",
	"how to apply",
	"when not to apply",
	"when not to use",
	"calibration",
	"anti-patterns",
	"pre-send ritual",
	"tl;dr",
	"failure mode",
	"severity rubric",
	"resolve-state decision tree",
	"surface multi-tag taxonomy",
	"update triggers",
	"prefer-fix-over-patch",
	"subdir routing",
	"forge call shape",
	"frontmatter",
	"filing path",
}

// elideFirstHeadings names H2 sections that are history / context /
// exemplar — the first to drop when budget pressed.
var elideFirstHeadings = []string{
	"anchors",
	"open questions",
	"why this skill exists",
	"history",
	"example",
	"exemplar",
	"recognition signals",
	"interaction with",
	"contrast with",
	"boundary with",
}

// truncatePreservingLoadBearing returns at most n bytes of body content,
// preserving frontmatter + intro + load-bearing H2 sections preferentially
// and eliding history / exemplar sections first. Replaces naive head-N
// for the inline-truncate tier (chain 602 T4).
//
// Strategy:
//  1. Split body into the intro (everything before first H2) + a list
//     of H2 sections.
//  2. Classify each H2 section by heading text → priority.
//  3. Always keep intro if it fits.
//  4. Add sections in priority order (must-keep → mid → elide-first)
//     until the next section would exceed n.
//  5. If even the intro overflows n, fall back to head-N truncation of
//     the whole body so the agent at least gets the frontmatter cue.
//
// Returns the truncated content. Section order in the output preserves
// the original document order (we drop sections, never reorder them) —
// the agent's mental model of the skill stays intact.
func truncatePreservingLoadBearing(content []byte, n int) []byte {
	if len(content) <= n {
		return content
	}
	sections := splitH2Sections(content)
	if len(sections) <= 1 {
		// No H2 boundaries to elide on. Fall back to head-N.
		return content[:n]
	}
	intro := sections[0]
	if len(intro.body) > n {
		// Intro alone exceeds budget. Head-N is the best fallback —
		// preserves frontmatter + title + first prose.
		return content[:n]
	}

	// Classify sections and pick which to keep, in document order.
	keep := make([]bool, len(sections))
	keep[0] = true // intro always
	remaining := n - len(intro.body)

	// Two passes: must-keep first, then mid-priority, then elide-first.
	for _, targetPri := range []sectionPriority{priorityMustKeep, priorityMidPriority, priorityElideFirst} {
		for i := 1; i < len(sections); i++ {
			if keep[i] {
				continue
			}
			if sections[i].priority != targetPri {
				continue
			}
			if len(sections[i].body) <= remaining {
				keep[i] = true
				remaining -= len(sections[i].body)
			}
		}
	}

	// Concatenate kept sections in document order.
	var out []byte
	for i, sec := range sections {
		if keep[i] {
			out = append(out, sec.body...)
		}
	}
	return out
}

// sectionInfo is one chunk of a SKILL body split on H2 boundaries.
// The first sectionInfo (index 0) is the intro (everything before the
// first H2); subsequent entries cover one H2 section each.
type sectionInfo struct {
	heading  string // empty for the intro; H2 line for the rest
	body     []byte // includes the heading line + everything until next H2
	priority sectionPriority
}

// splitH2Sections walks content line-by-line and groups it into the
// intro plus one chunk per H2 section. A line starts an H2 iff it
// begins with "## " (exactly two hashes followed by a space). H3+
// subsections (### ) stay with their parent H2.
func splitH2Sections(content []byte) []sectionInfo {
	lines := splitLinesKeepNL(content)
	var sections []sectionInfo
	var current sectionInfo
	flushCurrent := func() {
		if len(current.body) > 0 || current.heading != "" {
			current.priority = classifyHeading(current.heading)
			sections = append(sections, current)
		}
		current = sectionInfo{}
	}
	for _, line := range lines {
		if isH2(line) {
			flushCurrent()
			current.heading = strings.TrimSpace(strings.TrimPrefix(string(line), "##"))
		}
		current.body = append(current.body, line...)
	}
	flushCurrent()
	return sections
}

// splitLinesKeepNL splits content on '\n' boundaries but keeps the
// trailing newline on each line. The final line may have no newline
// (preserved as-is). Lets concatenation round-trip the original bytes.
func splitLinesKeepNL(content []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range content {
		if b == '\n' {
			lines = append(lines, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

// isH2 returns true iff the line is a markdown H2 heading (exactly two
// hashes followed by a space). Excludes H1 (#) and H3+ (###).
func isH2(line []byte) bool {
	return len(line) >= 3 && line[0] == '#' && line[1] == '#' && line[2] != '#' && line[2] == ' '
}

// classifyHeading inspects a heading string (already trimmed of the
// "## " prefix) and returns its truncation priority.
func classifyHeading(heading string) sectionPriority {
	if heading == "" {
		// Intro / no-heading chunks default to must-keep (frontmatter +
		// title + first prose are load-bearing).
		return priorityMustKeep
	}
	lower := strings.ToLower(heading)
	for _, needle := range loadBearingHeadings {
		if strings.Contains(lower, needle) {
			return priorityMustKeep
		}
	}
	for _, needle := range elideFirstHeadings {
		if strings.Contains(lower, needle) {
			return priorityElideFirst
		}
	}
	return priorityMidPriority
}
