package refresolve

// detectEcosystemToken emits a ShapeEcosystemToken reference per token from the
// local-ecosystem catalog (host slugs, service slugs, host addresses) that
// appears in the message. Pairs with ecosystemResolver, which returns the
// deterministic access summary. chain 435
// local-ecosystem-service-and-extraction-pattern.
//
// Returns nil when the catalog is empty (the service ships empty / nothing
// learned yet). Runs alongside the other catalog-based detectors in Detect().
func detectEcosystemToken(message string, tokens []string) []Reference {
	return detectExactCatalogTokens(message, tokens, ShapeEcosystemToken, "ecosystem_token")
}
