package refresolve

// detectSkillTrigger emits a Reference per trigger keyword from the
// skill manifest that appears as a whole-word match in the message.
// Reference-resolution-migration T5: pairs with skillTriggerResolver
// to surface the right skill body when a user message mentions
// language/tool/discipline keywords (e.g. "cargo" → rust-conventions).
//
// Returns nil when triggers is empty (manifest absent or no skill
// declared a trigger keyword). The detector's Detect method calls
// this alongside the other rule-based detectors.
func detectSkillTrigger(message string, triggers []string) []Reference {
	return detectExactCatalogTokens(message, triggers, ShapeSkillTrigger, "manifest_keyword")
}
