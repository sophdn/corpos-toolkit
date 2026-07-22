package work

// Shared param-resolution helpers consumed by every handler that
// accepts aliased fields (slug/chain/chain_slug, status/state/resolve_state,
// commit_sha/sha, max/limit/max_results). Keeps the aliasing concentrated
// in one place — handlers express their accepted aliases via struct json
// tags and resolve them through these helpers.

// firstNonEmpty returns the first string in s that isn't empty. Used to
// resolve `slug` / `chain_slug` / `chain` and similar alias triples.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstNonZeroInt64 returns the first non-zero int64 in s, falling back
// to def when every entry is zero. Used for max / limit / max_results
// alias triples.
func firstNonZeroInt64(s ...int64) int64 {
	if len(s) == 0 {
		return 0
	}
	def := s[len(s)-1]
	for _, v := range s[:len(s)-1] {
		if v > 0 {
			return v
		}
	}
	return def
}

// normalizeLimitOffset applies the work surface's default-limit + clamp
// convention: an explicit zero limit means "use default"; negative offsets
// clamp to zero. Mirrors readLimitOffset's behavior from the prior
// map[string]any decoder.
func normalizeLimitOffset(limit, offset, def int64) (int64, int64) {
	if limit == 0 {
		limit = def
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
