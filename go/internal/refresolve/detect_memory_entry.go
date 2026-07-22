package refresolve

// detectMemoryEntry emits a ShapeMemoryEntry reference per token
// from the auto-memory index that appears as a whole-word match in
// the message. Reference-resolution-migration T10: pairs with
// memoryEntryResolver to surface MEMORY.md entries when the user
// names something the auto-memory already remembers.
//
// Returns nil when the catalog is empty (MEMORY.md absent or no
// hyphenated identifiers extracted). Detection runs alongside the
// other catalog-based detectors in Detect().
func detectMemoryEntry(message string, tokens []string) []Reference {
	return detectExactCatalogTokens(message, tokens, ShapeMemoryEntry, "memory_index_keyword")
}
