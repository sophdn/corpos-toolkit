// Package vault walks ~/.claude/vault/ (or a configured root), parses YAML
// frontmatter, and validates paths against the root. Mirrors
// knowledge_lib::vault on the Rust side.
//
// No HTTP, no DB — pure filesystem + YAML. Security-critical paths (traversal,
// symlink escape) and frontmatter degradation are covered in tests.
package vault

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	// DefaultVaultRelativeToHome is the default vault root under $HOME.
	DefaultVaultRelativeToHome = ".claude/vault"
	// VaultRootEnv lets tests/config override the default without monkey-patching.
	VaultRootEnv = "TOOLKIT_VAULT_ROOT"
	// ForgeMarkdownRootEnv mirrors forge's own override (see
	// internal/forge.resolveMarkdownRoot). When set, vault_search resolves
	// to "<FORGE_MARKDOWN_ROOT>/vault" so a forge(vault-note) write and a
	// subsequent vault_search agree on disk location without callers
	// having to set two env vars in lockstep. Takes precedence below the
	// direct TOOLKIT_VAULT_ROOT so any caller that intentionally
	// separates write and read roots can still do so.
	ForgeMarkdownRootEnv = "FORGE_MARKDOWN_ROOT"
	// ForgeMarkdownVaultSubdir is the vault subdirectory under
	// FORGE_MARKDOWN_ROOT. Forge's resolveMarkdownRoot writes vault-target
	// schemas under "<root>/vault/<subdir>/<file>.md"; this constant pins
	// the symmetric read path.
	ForgeMarkdownVaultSubdir = "vault"
	// SummaryMaxChars caps the body-excerpt summary per entry. 160 chars
	// ≈ 40–50 tokens per entry; combined with path/title/tags it keeps a
	// 75-candidate pass-1 prompt under Qwen's 8192-token context.
	SummaryMaxChars = 160
	// metadataReadBytes is the head-read window for title/tag/summary extraction
	// during walk(). Enough for frontmatter + opening paragraph; cheap on large
	// vaults.
	metadataReadBytes = 4096
)

// Entry is one discovered vault note's metadata. The body is loaded separately
// via ReadNote — walk() only extracts title/tags/summary/body-for-scoring so
// the path list build stays fast.
type Entry struct {
	// Path is relative to the vault root, forward-slash normalized.
	Path string
	// Title is frontmatter `title`, falling back to the first H1, then the
	// filename stem.
	Title string
	// Tags are frontmatter `tags`, deduplicated and lowercased.
	Tags []string
	// Summary is a one-sentence excerpt — first non-blank, non-heading,
	// non-bullet line of the body, capped at SummaryMaxChars. Empty when the
	// body has no usable opening line.
	Summary string
	// BodyForScoring is the first metadataReadBytes worth of body text,
	// used by KeywordScore so an older note whose title/summary don't match
	// the query but whose body does still surfaces from the prefilter. Not
	// exposed in vault_search responses — only Title / Tags / Path are.
	//
	// Bug 1324: before this field, the prefilter scored against
	// path/title/tags/summary only; a body-keyword-only match was invisible
	// and the truncation cap silently hid older notes. The 4 KB window is
	// already read by readMetadata; keeping the slice avoids a second read.
	BodyForScoring string
}

// Frontmatter is the typed YAML metadata block at the head of a vault note.
//
// Fields are derived from the vault's observed conventions (see
// `~/.claude/vault/` for the corpus that established this set). Unknown YAML
// keys are dropped by yaml.Unmarshal — adding a new vault convention is a
// trigger to add a field here. The schema being visible in code is the
// point: clients can discover the response shape from this type instead of
// inspecting arbitrary YAML.
//
// Fields use omitempty so JSON output omits absent values rather than
// rendering `"date": ""` or `"tags": null` — preserves the prior map-based
// behaviour where absent keys simply didn't appear.
//
// Both `chain` (singular) and `chains` (plural) are observed in the corpus;
// both are accepted. Same for `topic` and `domain` (the convention drifted
// across vault eras; both are preserved rather than picking a winner).
type Frontmatter struct {
	Title           string   `yaml:"title" json:"title,omitempty"`
	Date            string   `yaml:"date" json:"date,omitempty"`
	Created         string   `yaml:"created" json:"created,omitempty"`
	Topic           string   `yaml:"topic" json:"topic,omitempty"`
	Domain          string   `yaml:"domain" json:"domain,omitempty"`
	Type            string   `yaml:"type" json:"type,omitempty"`
	Name            string   `yaml:"name" json:"name,omitempty"`
	Description     string   `yaml:"description" json:"description,omitempty"`
	Status          string   `yaml:"status" json:"status,omitempty"`
	Project         string   `yaml:"project" json:"project,omitempty"`
	Chain           string   `yaml:"chain" json:"chain,omitempty"`
	ParentChain     string   `yaml:"parent_chain" json:"parent_chain,omitempty"`
	Supersedes      string   `yaml:"supersedes" json:"supersedes,omitempty"`
	Tags            []string `yaml:"tags" json:"tags,omitempty"`
	Aliases         []string `yaml:"aliases" json:"aliases,omitempty"`
	Chains          []string `yaml:"chains" json:"chains,omitempty"`
	CrossReferences []string `yaml:"cross_references" json:"cross_references,omitempty"`
}

// NoteContent is one note's structured content. Mirrors knowledge_lib::vault::NoteContent.
type NoteContent struct {
	// Path is relative to the vault root.
	Path string `json:"path"`
	// Frontmatter is the parsed YAML metadata, or nil when absent / unparseable.
	Frontmatter *Frontmatter `json:"frontmatter,omitempty"`
	// Content is the markdown body (everything after the frontmatter delimiter
	// pair). Equal to the full file when no frontmatter is present.
	Content string `json:"content"`
	// FrontmatterWarning is a non-empty reason when frontmatter parsing
	// degraded gracefully (e.g. malformed YAML). Empty on the happy path.
	FrontmatterWarning string `json:"frontmatter_warning,omitempty"`
}

// Error types — typed sentinels so callers can map onto MCP error envelopes.
var (
	// ErrRootMissing fires when the resolved root path does not exist.
	ErrRootMissing = errors.New("vault root missing")
	// ErrRootNotDir fires when the resolved root path exists but isn't a directory.
	ErrRootNotDir = errors.New("vault root not a directory")
	// ErrPathTraversal fires when a relative path resolves outside the vault root.
	ErrPathTraversal = errors.New("path traversal")
	// ErrNoteNotFound fires when a path is in-scope but the file does not exist.
	ErrNoteNotFound = errors.New("note not found")
	// ErrHomeUnresolvable fires when $HOME is not set and no override is supplied.
	ErrHomeUnresolvable = errors.New("HOME unresolvable")
)

// ResolveRoot picks the vault root by precedence:
//  1. override (used by tests / config).
//  2. $TOOLKIT_VAULT_ROOT (direct vault root override).
//  3. $FORGE_MARKDOWN_ROOT + "/vault" (symmetric with forge writes —
//     forge.resolveMarkdownRoot lays vault-target files under
//     "<root>/vault/<subdir>/<file>.md", so the vault_search read root
//     is the same parent + "vault" subdir).
//  4. $HOME/.claude/vault.
//
// The returned path is symlink-resolved (filepath.EvalSymlinks); downstream
// path-traversal checks depend on canonicalization to be robust against `..`
// and symlink escapes.
func ResolveRoot(override string) (string, error) {
	var raw string
	switch {
	case override != "":
		raw = override
	case os.Getenv(VaultRootEnv) != "":
		raw = os.Getenv(VaultRootEnv)
	case os.Getenv(ForgeMarkdownRootEnv) != "":
		raw = filepath.Join(os.Getenv(ForgeMarkdownRootEnv), ForgeMarkdownVaultSubdir)
	default:
		home, ok := os.LookupEnv("HOME")
		if !ok || home == "" {
			return "", ErrHomeUnresolvable
		}
		raw = filepath.Join(home, DefaultVaultRelativeToHome)
	}
	info, err := os.Stat(raw)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", ErrRootMissing, raw)
	}
	if err != nil {
		return "", fmt.Errorf("stat vault root %s: %w", raw, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrRootNotDir, raw)
	}
	canonical, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("eval symlinks %s: %w", raw, err)
	}
	return canonical, nil
}

// Walk returns one Entry per markdown file under root, sorted by path.
// Hidden files and .obsidian/ are skipped; symlinks whose targets land outside
// root are silently skipped (tooling artefact, not a hard failure).
func Walk(root string) ([]Entry, error) {
	var entries []Entry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Walk-time errors → skip the entry, don't abort the call.
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		// Skip hidden / .obsidian at any depth >= 1.
		if rel != "." {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == ".obsidian" {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		// Canonicalize + guard against symlinks escaping root.
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil {
			return nil
		}
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			return nil
		}
		if !strings.HasPrefix(canonical, rootAbs+string(filepath.Separator)) && canonical != rootAbs {
			return nil
		}
		relCanonical, err := filepath.Rel(rootAbs, canonical)
		if err != nil {
			return nil
		}
		relCanonical = filepath.ToSlash(relCanonical)

		title, tags, summary, bodyForScoring := readMetadata(canonical, relCanonical)
		entries = append(entries, Entry{
			Path:           relCanonical,
			Title:          title,
			Tags:           tags,
			Summary:        summary,
			BodyForScoring: bodyForScoring,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func readMetadata(absolutePath, relative string) (title string, tags []string, summary string, bodyForScoring string) {
	content, err := readFirstNBytes(absolutePath, metadataReadBytes)
	if err != nil {
		return filenameTitle(relative), nil, "", ""
	}
	parsed, hasFM := parseFrontmatter(content)

	body := content
	if hasFM {
		body = parsed.body
	}

	// Title precedence: frontmatter `title` → first H1 → filename.
	title = filenameTitle(relative)
	if hasFM && parsed.frontmatter != nil && parsed.frontmatter.Title != "" {
		title = parsed.frontmatter.Title
	} else if h1 := firstH1(content); h1 != "" {
		title = h1
	}

	// Tags: frontmatter `tags` array, lowercased + sorted + deduped.
	if hasFM && parsed.frontmatter != nil && len(parsed.frontmatter.Tags) > 0 {
		set := make(map[string]struct{}, len(parsed.frontmatter.Tags))
		for _, s := range parsed.frontmatter.Tags {
			set[strings.ToLower(s)] = struct{}{}
		}
		tags = make([]string, 0, len(set))
		for k := range set {
			tags = append(tags, k)
		}
		sort.Strings(tags)
	}

	summary = FirstBodyExcerpt(body, SummaryMaxChars)
	bodyForScoring = body
	return title, tags, summary, bodyForScoring
}

func readFirstNBytes(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(buf[:read]), nil
}

// FirstBodyExcerpt returns the first non-blank, non-heading, non-bullet line of
// body, trimmed and capped at maxChars (truncated on a word boundary with an
// ellipsis suffix). Markdown formatting is preserved verbatim.
func FirstBodyExcerpt(body string, maxChars int) string {
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") ||
			strings.HasPrefix(line, "-") ||
			strings.HasPrefix(line, "*") ||
			strings.HasPrefix(line, "+") ||
			strings.HasPrefix(line, ">") ||
			strings.HasPrefix(line, "<!--") {
			continue
		}
		if utf8.RuneCountInString(line) <= maxChars {
			return line
		}
		// Truncate at the last word boundary inside the char cap.
		return truncateOnWordBoundary(line, maxChars)
	}
	return ""
}

// truncateOnWordBoundary trims s to at most maxChars runes, preferring the last
// whitespace inside the window. Appends '…' when truncation occurs.
func truncateOnWordBoundary(s string, maxChars int) string {
	lastSpaceByte := -1
	runeCount := 0
	cutByte := len(s)
	for i, r := range s {
		if runeCount >= maxChars {
			cutByte = i
			break
		}
		if (r == ' ' || r == '\t' || r == '\n') && runeCount > 0 {
			lastSpaceByte = i
		}
		runeCount++
	}
	if lastSpaceByte > 0 {
		cutByte = lastSpaceByte
	}
	return s[:cutByte] + "…"
}

func filenameTitle(relative string) string {
	base := filepath.Base(relative)
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '.' {
			return base[:i]
		}
	}
	return base
}

func firstH1(content string) string {
	for _, raw := range strings.Split(content, "\n") {
		if strings.HasPrefix(raw, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(raw, "#"))
		}
	}
	return ""
}

// parsedFrontmatter is the internal result of parseFrontmatter — only useful
// when (hasFrontmatter == true). `frontmatter` is nil if YAML failed to parse;
// `warning` carries the parse error message in that case.
type parsedFrontmatter struct {
	frontmatter *Frontmatter
	body        string
	warning     string
}

// parseFrontmatter looks for a leading `---` block, parses it as YAML, and
// returns the post-delimiter body. Returns (zero, false) when no frontmatter
// is present. Returns (with warning, true) when the frontmatter is present
// but malformed — never propagates a parse error; vault_read must degrade
// gracefully.
func parseFrontmatter(content string) (parsedFrontmatter, bool) {
	// Strip leading UTF-8 BOM (U+FEFF) if present.
	trimmed := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
		return parsedFrontmatter{}, false
	}
	// Find end of opening `---` line.
	nl := strings.IndexByte(trimmed, '\n')
	if nl < 0 {
		return parsedFrontmatter{}, false
	}
	afterOpen := trimmed[nl+1:]

	// Find next `---` on its own line.
	var closeOffset = -1
	offset := 0
	for {
		next := strings.IndexByte(afterOpen[offset:], '\n')
		var line string
		if next < 0 {
			line = afterOpen[offset:]
		} else {
			line = afterOpen[offset : offset+next]
		}
		if strings.TrimSpace(line) == "---" {
			closeOffset = offset
			break
		}
		if next < 0 {
			break
		}
		offset += next + 1
	}
	if closeOffset < 0 {
		return parsedFrontmatter{}, false
	}

	yamlStr := afterOpen[:closeOffset]
	// Body starts after the closing `---` line's newline (if any).
	bodyStart := closeOffset + len("---")
	if rest := afterOpen[closeOffset:]; len(rest) > 0 {
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			bodyStart = closeOffset + nl + 1
		} else {
			bodyStart = len(afterOpen)
		}
	}
	body := ""
	if bodyStart <= len(afterOpen) {
		body = afterOpen[bodyStart:]
	}

	var parsed Frontmatter
	if err := yaml.Unmarshal([]byte(yamlStr), &parsed); err != nil {
		return parsedFrontmatter{
			frontmatter: nil,
			body:        body,
			warning:     fmt.Sprintf("frontmatter parse failed: %s", err.Error()),
		}, true
	}
	return parsedFrontmatter{frontmatter: &parsed, body: body}, true
}

// ValidatePathWithinRoot rejects a relative path that escapes the vault root.
// Returns the canonical absolute path on success, or a typed error.
func ValidatePathWithinRoot(canonicalRoot, relative string) (string, error) {
	if relative == "" {
		return "", fmt.Errorf("%w: (empty)", ErrPathTraversal)
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, relative)
	}
	joined := filepath.Join(canonicalRoot, relative)
	canonical, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoteNotFound, relative)
	}
	rootAbs, err := filepath.Abs(canonicalRoot)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", canonicalRoot, err)
	}
	canonicalAbs, err := filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", canonical, err)
	}
	if !strings.HasPrefix(canonicalAbs, rootAbs+string(filepath.Separator)) && canonicalAbs != rootAbs {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, relative)
	}
	return canonicalAbs, nil
}

// ReadNote reads one note by its relative path. Validates path scope before
// opening so a malicious caller can't escape the vault via `..` or symlinks.
func ReadNote(canonicalRoot, relative string) (NoteContent, error) {
	resolved, err := ValidatePathWithinRoot(canonicalRoot, relative)
	if err != nil {
		return NoteContent{}, err
	}
	rawBytes, err := os.ReadFile(resolved)
	if err != nil {
		return NoteContent{}, fmt.Errorf("read note %s: %w", relative, err)
	}
	raw := string(rawBytes)

	parsed, hasFM := parseFrontmatter(raw)
	if !hasFM {
		return NoteContent{Path: relative, Frontmatter: nil, Content: raw}, nil
	}
	return NoteContent{
		Path:               relative,
		Frontmatter:        parsed.frontmatter,
		Content:            parsed.body,
		FrontmatterWarning: parsed.warning,
	}, nil
}

// ReadNoteBodyExcerpt reads a note's body and returns the first maxChars
// characters, truncated on a word boundary with a trailing ellipsis when the
// body exceeds the cap. Used by the vault_search pass-2 reranker so Qwen can
// score by what the note actually says, not just its title + summary.
func ReadNoteBodyExcerpt(canonicalRoot, relative string, maxChars int) (string, error) {
	resolved, err := ValidatePathWithinRoot(canonicalRoot, relative)
	if err != nil {
		return "", err
	}
	rawBytes, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read note %s: %w", relative, err)
	}
	raw := string(rawBytes)
	body := raw
	if parsed, hasFM := parseFrontmatter(raw); hasFM {
		body = parsed.body
	}
	body = strings.TrimSpace(body)
	if utf8.RuneCountInString(body) <= maxChars {
		return body, nil
	}
	return truncateOnWordBoundary(body, maxChars), nil
}

// ── Keyword pre-filter ────────────────────────────────────────────────

// tokenise splits s into lowercase alphanumeric tokens of length ≥ 2.
func tokenise(s string) []string {
	var out []string
	var current strings.Builder
	flush := func() {
		if current.Len() >= 2 {
			out = append(out, strings.ToLower(current.String()))
		}
		current.Reset()
	}
	for _, r := range s {
		isAlnum := ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')
		if isAlnum {
			current.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// KeywordScore scores an entry against a query by weighted keyword overlap.
// Weights: title 2.0, tags 2.0, summary 1.5, path 1.0, body 1.0.
//
// The body term (bug 1324) eliminates the prior "recency illusion": an
// older note whose query-relevant content lives past the 160-char summary
// would score 0 against title/tags/summary even when the body was the
// best match. The score is still bounded — each token contributes at
// most once per field, so a long body doesn't dominate a short title.
func KeywordScore(entry Entry, query string) float32 {
	queryTokens := tokenise(query)
	if len(queryTokens) == 0 {
		return 0
	}
	querySet := make(map[string]struct{}, len(queryTokens))
	for _, t := range queryTokens {
		querySet[t] = struct{}{}
	}

	overlap := func(s string) int {
		seen := make(map[string]struct{})
		for _, t := range tokenise(s) {
			if _, ok := querySet[t]; ok {
				seen[t] = struct{}{}
			}
		}
		return len(seen)
	}

	pathScore := float32(overlap(entry.Path)) * 1.0
	titleScore := float32(overlap(entry.Title)) * 2.0
	tagScore := float32(overlap(strings.Join(entry.Tags, " "))) * 2.0
	summaryScore := float32(overlap(entry.Summary)) * 1.5
	bodyScore := float32(overlap(entry.BodyForScoring)) * 1.0
	return pathScore + titleScore + tagScore + summaryScore + bodyScore
}

// KeywordPrefilter ranks entries by query-overlap score and returns the top
// `limit`. Ties preserve walk order. When no entry scores > 0, falls back to
// the first `limit` entries in walk order so Qwen still receives a candidate
// set rather than an empty list.
func KeywordPrefilter(entries []Entry, query string, limit int) []Entry {
	type scored struct {
		entry Entry
		score float32
		order int
	}
	all := make([]scored, len(entries))
	anyMatch := false
	for i, e := range entries {
		s := KeywordScore(e, query)
		if s > 0 {
			anyMatch = true
		}
		all[i] = scored{entry: e, score: s, order: i}
	}
	if anyMatch {
		sort.SliceStable(all, func(i, j int) bool {
			if all[i].score != all[j].score {
				return all[i].score > all[j].score
			}
			return all[i].order < all[j].order
		})
	}
	if limit > len(all) {
		limit = len(all)
	}
	out := make([]Entry, limit)
	for i := 0; i < limit; i++ {
		out[i] = all[i].entry
	}
	return out
}
