package refresolve

import (
	"fmt"
	"strings"
)

// formatResolved produces the PresentedAs string and
// RecommendedAction per design doc §4.1.
func formatResolved(ref Reference, hs HitSet) ResolvedReference {
	out := ResolvedReference{
		Token:          ref.Token,
		Shape:          ref.Shape,
		ConfidenceTier: hs.ConfidenceTier,
		TopCandidates:  hs.Candidates,
	}
	// Weak-boundary skill candidates always present as soft
	// suggestions regardless of how many candidates the manifest
	// lookup returned. The shape itself signals "low confidence —
	// the keyword sits inside a kebab token, not as a free-standing
	// word"; the cardinality detail stays in TopCandidates for the
	// agent to inspect. Without this early-return, multi-candidate
	// matches (e.g., "parse-context" trigger keyword shared by
	// parse-context-first-call + reference-resolution skills) would
	// fall into TierFuzzyMulti's ask_user_to_disambiguate branch —
	// louder than the weak-boundary signal warrants.
	//
	// TierNoHit falls through to the tier switch below (the resolver
	// returned zero candidates — generic acknowledge-no-hit response
	// applies regardless of shape).
	if ref.Shape == ShapeSkillCandidate && len(hs.Candidates) > 0 {
		out.RecommendedAction = PresentMentionAsPossiblyRelevant
		if len(hs.Candidates) == 1 {
			out.PresentedAs = presentSkillCandidate(ref, hs.Candidates[0])
		} else {
			out.PresentedAs = presentSkillCandidateMulti(ref, hs.Candidates)
		}
		return out
	}
	switch hs.ConfidenceTier {
	case TierSingleExact:
		out.RecommendedAction = PresentUseDirectly
		out.PresentedAs = presentSingleExact(ref, hs.Candidates[0])
	case TierFuzzyMulti:
		out.RecommendedAction = PresentAskUserToDisambiguate
		out.PresentedAs = presentFuzzyMulti(ref, hs.Candidates)
	case TierWeakDomain:
		out.RecommendedAction = PresentMentionAsPossiblyRelevant
		out.PresentedAs = presentWeakDomain(ref, hs.Candidates)
	case TierNoHit:
		out.RecommendedAction = PresentAcknowledgeNoHitAndAsk
		out.PresentedAs = fmt.Sprintf("`%s` did not resolve in any %s index.", ref.Token, ref.Shape)
	}
	return out
}

func presentSingleExact(ref Reference, c Candidate) string {
	switch ref.Shape {
	case ShapeChainSlug:
		return fmt.Sprintf("`%s` → chain in %s. %s", ref.Token, projectFromSourceRef(c.SourceRef), c.DebugNotes)
	case ShapeTaskSlug:
		return fmt.Sprintf("`%s` → %s. %s", ref.Token, c.Title, c.DebugNotes)
	case ShapeBugSlug:
		return fmt.Sprintf("`%s` → %s. %s", ref.Token, c.Title, c.DebugNotes)
	case ShapePath:
		return fmt.Sprintf("`%s` → %s (%s)", ref.Token, c.Title, c.DebugNotes)
	case ShapeSkillName:
		return fmt.Sprintf("`%s` → skill at %s", ref.Token, stripSourcePrefix(c.SourceRef))
	case ShapeToolName:
		return fmt.Sprintf("`%s` → action manifest at %s", ref.Token, stripSourcePrefix(c.SourceRef))
	case ShapeForgeSchema:
		return fmt.Sprintf("`%s` → forge schema at %s", ref.Token, stripSourcePrefix(c.SourceRef))
	case ShapeProjectName:
		return fmt.Sprintf("`%s` → project (%s)", ref.Token, c.DebugNotes)
	case ShapeLibraryEntry:
		return fmt.Sprintf("`%s` → %s", ref.Token, c.Title)
	case ShapeDomainTerm:
		return fmt.Sprintf("`%s` → %s (%s, score %.2f)",
			ref.Token, c.Title, stripSourcePrefix(c.SourceRef), c.Score)
	case ShapeExternalTechnical:
		return fmt.Sprintf("`%s` → %s (kiwix: %s)", ref.Token, c.Title, c.DebugNotes)
	case ShapeFrictionShape:
		// The "binding" for friction-shape is a filing suggestion, not
		// a real referent. Compose the prose the agent surfaces at its
		// natural decision point (per design doc §7 supersession).
		return fmt.Sprintf(
			"You observed friction at: %q. Consider filing via `work(action='forge', params={schema_name: 'bug', slug: '...', fields: {title: '...', problem_statement: '...'}})`.",
			ref.Token,
		)
	case ShapeSkillTrigger:
		return fmt.Sprintf("`%s` triggers skill `%s` — body at %s",
			ref.Token, c.ID, stripSourcePrefix(c.SourceRef))
	case ShapeSkillCandidate:
		// Reached only as a fallback; the TierSingleExact branch in
		// formatResolved calls presentSkillCandidate directly instead.
		// Kept here for shape-completeness of the switch.
		return fmt.Sprintf("`%s` may relate to skill `%s` — body at %s",
			ref.Token, c.ID, stripSourcePrefix(c.SourceRef))
	case ShapeDisciplineSkill:
		return fmt.Sprintf("Trigger fired for discipline `%s` (via %s) — apply via %s",
			c.ID, c.DebugNotes, stripSourcePrefix(c.SourceRef))
	case ShapeVaultCandidate:
		return fmt.Sprintf("`%s` may resolve to vault note %s (%s)",
			ref.Token, stripSourcePrefix(c.SourceRef), c.Title)
	case ShapeKiwixBridge:
		return fmt.Sprintf("`%s` may resolve to kiwix entry %s",
			ref.Token, stripSourcePrefix(c.SourceRef))
	case ShapeEcosystemToken:
		// c.Title IS the composed deterministic access answer, e.g.
		// "Yes — ssh youruser@example-host.tailnet.ts.net (key ~/.ssh/id_ed25519)."
		return fmt.Sprintf("`%s` → %s", ref.Token, c.Title)
	case ShapeCanonToken:
		// c.Title IS the composed canonical-identity answer, e.g.
		// "\"mcp-servers\" is RETIRED — the canonical project is now corpos-toolkit."
		return fmt.Sprintf("`%s` → %s", ref.Token, c.Title)
	case ShapeMemoryEntry:
		return fmt.Sprintf("`%s` may resolve to memory entry %s",
			ref.Token, stripSourcePrefix(c.SourceRef))
	default:
		return fmt.Sprintf("`%s` → %s", ref.Token, c.Title)
	}
}

func presentFuzzyMulti(ref Reference, candidates []Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` matched %d candidates:", ref.Token, len(candidates))
	for i, c := range candidates {
		if i >= 5 {
			fmt.Fprintf(&b, "\n  …and %d more", len(candidates)-i)
			break
		}
		fmt.Fprintf(&b, "\n  %d) %s — %s", i+1, c.Title, c.SourceRef)
	}
	return b.String()
}

// presentSkillCandidate composes the PresentedAs string for the
// single-candidate weak-boundary skill match. Differs from
// presentSingleExact's skill_trigger form by softening the
// "triggers" language to "may relate to" — the keyword sits inside
// a kebab token, so it's a context cue rather than a free-standing
// trigger.
func presentSkillCandidate(ref Reference, c Candidate) string {
	return fmt.Sprintf("`%s` (inside a longer token) may relate to skill `%s` — body at %s",
		ref.Token, c.ID, stripSourcePrefix(c.SourceRef))
}

// presentSkillCandidateMulti composes the PresentedAs string when a
// weak-boundary trigger keyword resolves to multiple skill
// candidates (e.g., "parse-context" shared by parse-context-first-call
// AND reference-resolution). The action stays
// PresentMentionAsPossiblyRelevant — softer than
// PresentAskUserToDisambiguate would imply — but the prose lists
// the candidates so the agent can pick if it wants.
func presentSkillCandidateMulti(ref Reference, candidates []Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` (inside a longer token) may relate to %d skills:", ref.Token, len(candidates))
	for i, c := range candidates {
		if i >= 5 {
			fmt.Fprintf(&b, "\n  …and %d more", len(candidates)-i)
			break
		}
		fmt.Fprintf(&b, "\n  %d) %s — %s", i+1, c.ID, stripSourcePrefix(c.SourceRef))
	}
	return b.String()
}

func presentWeakDomain(ref Reference, candidates []Candidate) string {
	if len(candidates) == 0 {
		return fmt.Sprintf("`%s` did not resolve.", ref.Token)
	}
	top := candidates[0]
	return fmt.Sprintf("`%s` may refer to: %s (rank 1, weak match — score %.2f)",
		ref.Token, top.SourceRef, top.Score)
}

// projectFromSourceRef extracts the project identifier from a
// canonical SourceRef like "chain:slug" (no project) or
// "bug:project/slug". The chain resolver only emits "chain:slug"
// since chain slugs are globally unique in the current schema.
func projectFromSourceRef(sourceRef string) string {
	// "chain:slug" → no project segment, return empty/default
	parts := strings.SplitN(sourceRef, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	body := parts[1]
	if i := strings.Index(body, "/"); i > 0 {
		return body[:i]
	}
	return ""
}

func stripSourcePrefix(sourceRef string) string {
	if i := strings.Index(sourceRef, ":"); i > 0 {
		return sourceRef[i+1:]
	}
	return sourceRef
}
