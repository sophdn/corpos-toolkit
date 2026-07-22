package refresolve

import "strings"

// detectSkillCandidate is the weak-boundary sibling of
// detectSkillTrigger. It emits one ShapeSkillCandidate Reference per
// trigger keyword match where boundaryOKCatalog (strict — rejects
// hyphen/underscore neighbors) would have rejected but boundaryOK
// (loose — only rejects alphanumeric neighbors) accepts. The two
// detectors are MUTUALLY EXCLUSIVE per match position by construction:
//
//	strict accepts → detectSkillTrigger emits ShapeSkillTrigger,
//	                 detectSkillCandidate skips (strict already passed)
//	strict rejects + loose accepts → detectSkillCandidate emits
//	                 ShapeSkillCandidate (the niche this detector owns)
//	loose rejects → neither emits (alphanumeric neighbor — not a
//	                word-like context)
//
// Filed as suggestion
// `weak-boundary-skill-candidate-emit-when-trigger-keyword-prefixes-a-kebab-slug`
// after chain parse-context-skill-body-inline-on-use-directly T6's
// vault decision §Lessons #3 documented the strict-boundary's
// user-pattern dependency: when chain slugs contain trigger keywords
// as prefixes (e.g., "parse-context" inside "parse-context-skill-body-..."),
// the strict path correctly refuses the match but the keyword IS a
// relevant cue. This detector surfaces those cases as candidates.
//
// Returns nil when triggers is empty (manifest absent).
func detectSkillCandidate(message string, triggers []string) []Reference {
	if len(triggers) == 0 || message == "" {
		return nil
	}
	out := []Reference{}
	for _, name := range triggers {
		if name == "" {
			continue
		}
		idx := 0
		for {
			j := strings.Index(message[idx:], name)
			if j == -1 {
				break
			}
			start := idx + j
			end := start + len(name)
			// Niche: strict rejects but loose accepts. The strict
			// detector (detectSkillTrigger) already covers the
			// strict-accepts case; this detector owns the rest.
			if !boundaryOKCatalog(message, start, end) && boundaryOK(message, start, end) {
				out = append(out, Reference{
					Token:           message[start:end],
					Shape:           ShapeSkillCandidate,
					Confidence:      0.7, // weaker than strict's 1.0
					DetectionMethod: "manifest_keyword_weak_boundary",
					StartPos:        start,
					EndPos:          end,
				})
			}
			idx = start + 1
		}
	}
	return out
}
