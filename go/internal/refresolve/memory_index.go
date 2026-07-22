package refresolve

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MemoryEntry is one row from MEMORY.md — the auto-memory index
// the harness loads at session start. PARSE_CONTEXT §3.2.2.
//
// Format the parser recognizes:
//
//   - [Title](file.md) — one-line description
//
// Backticks within the title or description are preserved. The
// resolver renders the body path absolute to MemoryDir so callers
// don't need to know the layout.
type MemoryEntry struct {
	Slug        string // file.md (basename)
	Title       string // human-readable label from the brackets
	Description string // text after the em-dash
	BodyPath    string // absolute path to the entry's body file
}

// MemoryIndex is the parsed MEMORY.md plus a token → entry map
// built once at load. The token set is the union of hyphenated
// identifiers found in entry titles and descriptions, with short
// stopwords filtered out.
type MemoryIndex struct {
	Entries []MemoryEntry
	// byToken maps a normalized token (lowercase, no surrounding
	// punctuation) to the entries that mention it. Used by the
	// detector to emit ShapeMemoryEntry references and by the
	// resolver to map a Reference back to its entries.
	byToken map[string][]int // index into Entries
}

// memoryLinePat matches one MEMORY.md row:
//
//   - [Title text](slug.md) — description text
//
// Group 1: title, Group 2: slug, Group 3: description.
var memoryLinePat = regexp.MustCompile(`^\s*-\s*\[([^\]]+)\]\(([^)]+)\)\s*[—-]\s*(.*)$`)

// hyphenIdentPat extracts hyphenated identifiers (one or more
// kebab-case tokens like "ml-capability-substrate" or "atomic-tasks")
// from a string. Single-word identifiers without a hyphen aren't
// extracted to keep the catalog tight; tokens like "vault" would
// otherwise dominate the index.
var hyphenIdentPat = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:-[a-z0-9]+)+\b`)

// LoadMemoryIndex parses memoryDir/MEMORY.md and returns the index.
// Returns (nil, nil) when memoryDir is empty or MEMORY.md is absent.
// memoryDir is typically `~/.claude/projects/<slug>/memory/`.
func LoadMemoryIndex(memoryDir string) (*MemoryIndex, error) {
	if memoryDir == "" {
		return nil, nil
	}
	path := filepath.Join(memoryDir, "MEMORY.md")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	idx := &MemoryIndex{byToken: make(map[string][]int)}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		m := memoryLinePat.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		entry := MemoryEntry{
			Title:       strings.TrimSpace(m[1]),
			Slug:        strings.TrimSpace(m[2]),
			Description: strings.TrimSpace(m[3]),
			BodyPath:    filepath.Join(memoryDir, strings.TrimSpace(m[2])),
		}
		i := len(idx.Entries)
		idx.Entries = append(idx.Entries, entry)
		// Dedup tokens within this entry before indexing. The slug
		// embeds the title for materialized rows (`- [linguistic-tics]
		// (linguistic-tics.md)`), so the same token matches in both
		// title and slug; without this guard the entry index lands in
		// byToken twice and Lookup surfaces it as duplicate candidates.
		seenTok := make(map[string]bool)
		for _, tok := range hyphenIdentPat.FindAllString(strings.ToLower(entry.Title+" "+entry.Description+" "+entry.Slug), -1) {
			if seenTok[tok] {
				continue
			}
			seenTok[tok] = true
			idx.byToken[tok] = append(idx.byToken[tok], i)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return idx, nil
}

// Tokens returns the sorted set of keywords the detector should
// scan the message for. Used to populate Catalogs.MemoryTokens.
func (m *MemoryIndex) Tokens() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.byToken))
	for k := range m.byToken {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lookup returns the entries that mention the supplied token
// (case-insensitive match against the byToken map). Returns nil
// when no match.
func (m *MemoryIndex) Lookup(token string) []MemoryEntry {
	if m == nil {
		return nil
	}
	is := m.byToken[strings.ToLower(token)]
	if len(is) == 0 {
		return nil
	}
	// Dedup by slug: a slug is a filename, so two index rows with the
	// same slug are the same memory file (e.g. a kebab line plus a
	// title-case duplicate line). Collapsing them here keeps the
	// resolver from emitting duplicate candidates that would force a
	// spurious ask_user_to_disambiguate — the divergence shape that
	// cascaded into a reranker-projection rebuild abort.
	out := make([]MemoryEntry, 0, len(is))
	seenSlug := make(map[string]bool)
	for _, i := range is {
		e := m.Entries[i]
		if seenSlug[e.Slug] {
			continue
		}
		seenSlug[e.Slug] = true
		out = append(out, e)
	}
	return out
}
