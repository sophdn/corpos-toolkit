package refresolve

// detectCanonToken emits a ShapeCanonToken reference per token from the canon
// catalog (canonical names, retired aliases, old paths/ports) that appears in the
// message. Pairs with canonResolver, which returns the current canonical
// identity. canon_resolve extraction (follow-on to chain 435).
//
// Returns nil when the catalog is empty (nothing learned yet). Runs alongside the
// other catalog-based detectors in Detect().
func detectCanonToken(message string, tokens []string) []Reference {
	return detectExactCatalogTokens(message, tokens, ShapeCanonToken, "canon_token")
}
