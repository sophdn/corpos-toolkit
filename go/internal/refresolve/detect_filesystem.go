package refresolve

import (
	"regexp"
	"strings"
)

// pathRe matches tokens that look like filesystem paths:
//   - start with /, ~, ./, or a single repo-root-style segment
//     containing at least one slash before a recognized extension.
//
// Conservative on purpose: a stray "foo/bar" without an extension
// could be many things; we only emit Path when the token clearly
// names a file (has an extension we recognize). Directory paths
// get a separate heuristic.
// Right-boundary class deliberately omits `.` so a sentence-ending
// period after a path (e.g. "see docs/X.md.") terminates the match
// instead of greedy-extending into the period and failing. Left
// boundary keeps `.` because a leading period would imply a relative
// path segment, which the inner alternation already handles.
var pathRe = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_/.~-])` + // boundary
		`((?:/|~/|\./|\.\./)?(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\.(?:md|MD|go|rs|toml|TOML|json|JSON|py|sh|sql|SQL|yml|yaml|YAML|ts|tsx|js|jsx|html|css|tf|hcl|proto|graphql|env|conf|cfg|ini|lock|tpl|tmpl|csv|tsv|jsonl))` +
		`(?:$|[^A-Za-z0-9_/~-])`)

// detectPath emits a Reference for every token that matches the
// path regex. The whole regex includes optional boundary chars; we
// take FindAllStringSubmatchIndex to locate the inner capture group
// so the token positions are precise.
func detectPath(message string) []Reference {
	if message == "" {
		return nil
	}
	out := []Reference{}
	for _, m := range pathRe.FindAllStringSubmatchIndex(message, -1) {
		// m[0:1] = whole match; m[2:3] = capture group 1 (the path).
		if len(m) < 4 {
			continue
		}
		start, end := m[2], m[3]
		out = append(out, Reference{
			Token:           message[start:end],
			Shape:           ShapePath,
			Confidence:      1.0,
			DetectionMethod: "regex",
			StartPos:        start,
			EndPos:          end,
		})
	}
	return out
}

// detectSkillName emits a Reference for every catalog skill name
// that appears as a whole-word match in the message. Skill names
// are kebab-case but distinguished from chain/task/bug slugs by the
// catalog source (basenames of *.toml in skills/) — we match
// against the skill catalog specifically.
//
// Boundary-aware to avoid matching "vault-pull-discipline" inside
// "vault-pull-discipline-extended".
func detectSkillName(message string, skillNames []string) []Reference {
	return detectExactCatalogTokens(message, skillNames, ShapeSkillName, "filename_match")
}

// detectProjectName emits a Reference for each project name that
// appears as a whole-word match. The catalog is closed; new
// projects extend Catalogs.Projects.
func detectProjectName(message string, projects []string) []Reference {
	return detectExactCatalogTokens(message, projects, ShapeProjectName, "list_match")
}

// detectToolName emits a Reference for each tool/action name that
// appears as a whole-word match. The catalog is the basenames of
// *.toml files in action-manifests/.
func detectToolName(message string, tools []string) []Reference {
	return detectExactCatalogTokens(message, tools, ShapeToolName, "filename_match")
}

// detectForgeSchema emits a Reference for each forge schema name
// that appears as a whole-word match. The catalog is the basenames
// of *.toml files in blueprints/forge-schemas/.
func detectForgeSchema(message string, schemas []string) []Reference {
	return detectExactCatalogTokens(message, schemas, ShapeForgeSchema, "filename_match")
}

// detectLibraryEntry emits a Reference for each library slug or
// title that appears as a whole-word match. Titles are matched
// case-insensitively (per docs/REFERENCE_RESOLUTION.md §2.1);
// slugs case-sensitively.
//
// Avoids double-emitting when the same library entry's slug and
// title both happen to appear at the same position (rare; the
// dedupe step in Detect collapses literal duplicates anyway).
func detectLibraryEntry(message string, slugs, titlesLower []string) []Reference {
	out := detectExactCatalogTokens(message, slugs, ShapeLibraryEntry, "slug_match")
	// Case-insensitive title scan — walk the lowercased message and
	// project hits back to original positions.
	lower := strings.ToLower(message)
	for _, title := range titlesLower {
		t := strings.ToLower(title)
		if t == "" {
			continue
		}
		idx := 0
		for {
			j := strings.Index(lower[idx:], t)
			if j == -1 {
				break
			}
			start := idx + j
			end := start + len(t)
			if boundaryOK(message, start, end) {
				out = append(out, Reference{
					Token:           message[start:end],
					Shape:           ShapeLibraryEntry,
					Confidence:      1.0,
					DetectionMethod: "title_match_ci",
					StartPos:        start,
					EndPos:          end,
				})
			}
			idx = start + 1
		}
	}
	return out
}

// detectExactCatalogTokens is the shared helper for skill / project
// / tool / schema / library-slug detection. Walks each catalog
// entry as a whole-token substring search; emits References on
// boundary-validated matches.
//
// Catalog lookups for these shapes are case-sensitive — skill files,
// action manifests, schema files, and project names all use a
// canonical case in this codebase. The library title path
// (above) is the only case-insensitive surface.
func detectExactCatalogTokens(message string, catalog []string, shape ShapeCategory, method string) []Reference {
	if len(catalog) == 0 || message == "" {
		return nil
	}
	out := []Reference{}
	for _, name := range catalog {
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
			if boundaryOKCatalog(message, start, end) {
				out = append(out, Reference{
					Token:           message[start:end],
					Shape:           shape,
					Confidence:      1.0,
					DetectionMethod: method,
					StartPos:        start,
					EndPos:          end,
				})
			}
			idx = start + 1
		}
	}
	return out
}

// boundaryOKCatalog is the stricter boundary check for catalog-name
// detectors (skills, tools, schemas, projects, library slugs). Unlike
// boundaryOK, it disqualifies hyphen and underscore neighbors so a
// short catalog name like `chain` doesn't match inside a longer
// kebab/snake token like `fictional-chain-xyz123`. Slug detectors
// keep boundaryOK because they validate the WHOLE token against the
// catalog.
func boundaryOKCatalog(message string, start, end int) bool {
	if start > 0 {
		c := message[start-1]
		if isAlnum(c) || c == '-' || c == '_' {
			return false
		}
	}
	if end < len(message) {
		c := message[end]
		if isAlnum(c) || c == '-' || c == '_' {
			return false
		}
	}
	return true
}
