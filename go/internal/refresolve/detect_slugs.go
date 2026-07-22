package refresolve

import (
	"regexp"
	"sort"
	"strings"
)

// kebabSlugRe matches kebab-case slug tokens: lowercase ASCII letter
// or digit start, runs of [a-z0-9] separated by single hyphens,
// minimum two segments (a single-word lowercase token is not slug-
// shaped — too easily collides with English nouns).
//
// Anchors: a slug may sit at the start/end of the message or be
// preceded/followed by whitespace, punctuation, or backticks. The
// regex itself doesn't anchor; tokenizeKebabSlugs walks the message
// and applies boundary checks via boundaryOK.
var kebabSlugRe = regexp.MustCompile(`[a-z][a-z0-9]*(?:-[a-z0-9]+)+`)

// detectChainSlug emits a Reference for every kebab-case token that
// matches a known chain slug. Slugs not in the catalog are NOT
// emitted as chain_slug (they fall through to task / bug / no-hit);
// the detector's promise is "this is a chain slug" only when the
// catalog confirms.
func detectChainSlug(message string, chainSlugs []string) []Reference {
	return detectSlugAgainstCatalog(message, chainSlugs, ShapeChainSlug)
}

// detectTaskSlug emits a Reference for kebab-case tokens that match
// a known task slug.
func detectTaskSlug(message string, taskSlugs []string) []Reference {
	return detectSlugAgainstCatalog(message, taskSlugs, ShapeTaskSlug)
}

// detectBugSlug emits a Reference for kebab-case tokens that match
// a known bug slug.
func detectBugSlug(message string, bugSlugs []string) []Reference {
	return detectSlugAgainstCatalog(message, bugSlugs, ShapeBugSlug)
}

// detectSlugAgainstCatalog is the shared logic for chain/task/bug
// slug detection. Builds a set from the catalog for O(1) match,
// walks regex matches, emits one Reference per matched-and-in-set
// token. Token positions are byte offsets into the source message.
func detectSlugAgainstCatalog(message string, catalog []string, shape ShapeCategory) []Reference {
	if len(catalog) == 0 || message == "" {
		return nil
	}
	set := make(map[string]bool, len(catalog))
	for _, s := range catalog {
		set[s] = true
	}
	out := []Reference{}
	for _, m := range kebabSlugRe.FindAllStringIndex(message, -1) {
		start, end := m[0], m[1]
		if !boundaryOK(message, start, end) {
			continue
		}
		tok := message[start:end]
		if set[tok] {
			out = append(out, Reference{
				Token:           tok,
				Shape:           shape,
				Confidence:      1.0,
				DetectionMethod: "regex+list_match",
				StartPos:        start,
				EndPos:          end,
			})
		}
	}
	return out
}

// boundaryOK confirms the byte positions [start,end) sit on a
// non-alphanumeric boundary so we don't extract "blah" from
// "blah-blue-ish" or pick up substrings of larger identifiers.
// Backtick, paren, colon, comma, period, space, tab, newline, and
// start/end-of-message all qualify; ASCII letter or digit on either
// side disqualifies.
func boundaryOK(message string, start, end int) bool {
	if start > 0 {
		c := message[start-1]
		if isAlnum(c) {
			return false
		}
	}
	if end < len(message) {
		c := message[end]
		if isAlnum(c) {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// loadCatalogsSorted is a helper for tests and the production
// initializer; returns the catalog with duplicates removed and
// sorted for deterministic iteration order.
func loadCatalogsSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
