package refresolve

// detectDisciplineSkill is the second-pass detector that surfaces
// discipline-skill references based on what the primary detectors
// already found. Reference-resolution-migration T5: this is the
// load-bearing innovation that lets disciplines move from ambient
// to lazy (PARSE_CONTEXT §3.2.5).
//
// Trigger conditions today:
//   - friction_shape detected → emit ShapeDisciplineSkill for
//     "bug-filing-discipline" (the friction-filing-reminder Stop hook's
//     successor)
//
// Future trigger conditions land here as each one's source-of-truth
// is formalized:
//   - domain_term detected with weak_domain tier → vault-pull-discipline
//   - code-being-written shape (not yet implemented) → coding-philosophy
//   - insight-shape (not yet implemented) → vault-filing-discipline
//
// Each emitted Reference uses Token=skill-name so the resolver can
// look it up in the manifest. The detector dedupes — a single
// detected discipline trigger emits one Reference even if multiple
// primary references fire the same condition.
func detectDisciplineSkill(primary []Reference) []Reference {
	seen := map[string]bool{}
	var out []Reference
	for _, ref := range primary {
		var disciplineName string
		switch ref.Shape {
		case ShapeFrictionShape:
			disciplineName = "bug-filing-discipline"
		}
		if disciplineName == "" || seen[disciplineName] {
			continue
		}
		seen[disciplineName] = true
		out = append(out, Reference{
			Token:           disciplineName,
			Shape:           ShapeDisciplineSkill,
			Confidence:      1.0,
			DetectionMethod: string(ref.Shape) + ":" + ref.Token,
			StartPos:        ref.StartPos,
			EndPos:          ref.EndPos,
		})
	}
	return out
}
