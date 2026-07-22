package measure

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"toolkit/internal/db"
)

// TeamContext is the derived bandwidth + prior-signal context appended to a
// chain-assessment classify call. Port of measure_lib::chain_assessment::TeamContext.
type TeamContext struct {
	ClosuresPerWeek     int64
	Bandwidth           string // "low" | "nominal" | "high"
	TrailingP50         int64
	TrailingSampleWeeks int
	VaultHits           int
	PriorSignal         string // "weak" | "mid" | "strong"
	Keywords            []string
	MatchedPaths        []string
	ProjectScope        string // empty when cross-project
}

const (
	minWeeksForPercentile = 4
	trailingWeeks         = 12
	keywordMinLen         = 4
	maxKeywords           = 8
	maxPathsInProse       = 3
)

// stopwords are generic task-spec tokens dropped during keyword extraction so
// they don't over-match vault notes. Mirrors Rust STOPWORDS.
var stopwords = map[string]struct{}{
	"task": {}, "chain": {}, "should": {}, "would": {}, "could": {},
	"needs": {}, "needed": {}, "from": {}, "with": {}, "into": {},
	"this": {}, "that": {}, "these": {}, "those": {}, "there": {},
	"their": {}, "them": {}, "they": {}, "than": {}, "then": {},
	"have": {}, "been": {}, "being": {}, "what": {}, "when": {},
	"where": {}, "which": {}, "while": {}, "after": {}, "before": {},
	"above": {}, "below": {}, "about": {}, "across": {}, "against": {},
	"between": {}, "during": {}, "through": {}, "without": {}, "within": {},
	"spec": {}, "specs": {}, "input": {}, "output": {}, "value": {},
	"values": {}, "data": {}, "result": {}, "results": {}, "tests": {},
	"test": {},
}

// DeriveTeamContext queries the DB for bandwidth + scans the local vault for
// prior signal. vaultRoot empty means use $HOME/.claude/vault. project empty
// means count closures across all projects (matching Rust semantics).
//
// Vault unavailable is a soft failure: prior_signal degrades to "weak" with
// zero hits rather than hard-failing the classify call.
func DeriveTeamContext(ctx context.Context, pool *db.Pool, vaultRoot, project, taskSpec string) (*TeamContext, error) {
	closures, err := closuresCount(ctx, pool, project)
	if err != nil {
		return nil, fmt.Errorf("derive team context: closures: %w", err)
	}
	pct, err := trailingPercentiles(ctx, pool, project)
	if err != nil {
		return nil, fmt.Errorf("derive team context: percentiles: %w", err)
	}

	keywords := extractKeywords(taskSpec)
	vaultHits, matchedPaths := scanVault(vaultRoot, keywords)

	return &TeamContext{
		ClosuresPerWeek:     closures,
		Bandwidth:           bandwidthLabel(closures, pct),
		TrailingP50:         pct.p50,
		TrailingSampleWeeks: pct.sampleWeeks,
		VaultHits:           vaultHits,
		PriorSignal:         priorSignalLabel(vaultHits),
		Keywords:            keywords,
		MatchedPaths:        matchedPaths,
		ProjectScope:        project,
	}, nil
}

// Prose renders the two-line team-context block embedded in the dispatcher input.
func (tc *TeamContext) Prose() string {
	scope := ""
	if tc.ProjectScope != "" {
		scope = fmt.Sprintf(" [project: %s]", tc.ProjectScope)
	}
	baseline := ""
	if tc.TrailingSampleWeeks > 0 {
		baseline = fmt.Sprintf("; project trailing P50 = %d/week", tc.TrailingP50)
	}
	bandwidthLine := fmt.Sprintf(
		"team_bandwidth: %s — %d task closure(s) in the last 7 days%s%s.",
		tc.Bandwidth, tc.ClosuresPerWeek, baseline, scope,
	)

	keywordBlurb := "no keywords extracted from the task spec"
	if len(tc.Keywords) > 0 {
		keywordBlurb = "keywords: " + strings.Join(tc.Keywords, ", ")
	}
	pathBlurb := ""
	if len(tc.MatchedPaths) > 0 {
		pathBlurb = " Top matches: " + strings.Join(tc.MatchedPaths, ", ") + "."
	}
	priorLine := fmt.Sprintf(
		"prior_signal_strength: %s — %d vault decision(s) match the task's domain (%s).%s",
		tc.PriorSignal, tc.VaultHits, keywordBlurb, pathBlurb,
	)

	return bandwidthLine + "\n" + priorLine
}

// closuresCount returns the count of tasks closed in the last 7 days, optionally
// scoped to a project via the chains.project_id join.
func closuresCount(ctx context.Context, pool *db.Pool, project string) (int64, error) {
	var n int64
	var err error
	if project != "" {
		err = pool.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM proj_current_tasks t
			   JOIN proj_chain_status c ON c.id = t.chain_id
			  WHERE t.status = 'closed'
			    AND t.updated_at >= datetime('now', '-7 days')
			    AND c.project_id = ?`, project).Scan(&n)
	} else {
		err = pool.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM proj_current_tasks
			  WHERE status = 'closed'
			    AND updated_at >= datetime('now', '-7 days')`).Scan(&n)
	}
	return n, err
}

type percentiles struct {
	p25, p50, p75 int64
	sampleWeeks   int
}

func fallbackPercentiles() percentiles {
	return percentiles{p25: 2, p50: 4, p75: 7, sampleWeeks: 0}
}

// trailingPercentiles computes P25/P50/P75 weekly close rates over the 12 complete
// weeks before the current partial week. Returns fallback when fewer than 4
// distinct weeks have non-zero closures.
func trailingPercentiles(ctx context.Context, pool *db.Pool, project string) (percentiles, error) {
	var rows *sql.Rows
	var err error
	if project != "" {
		rows, err = pool.DB().QueryContext(ctx,
			`SELECT CAST((julianday('now') - julianday(t.updated_at)) / 7 AS INTEGER) AS slot,
			        COUNT(*) AS cnt
			   FROM proj_current_tasks t
			   JOIN proj_chain_status c ON c.id = t.chain_id
			  WHERE t.status = 'closed'
			    AND t.updated_at >= datetime('now', '-91 days')
			    AND t.updated_at < datetime('now', '-7 days')
			    AND c.project_id = ?
			  GROUP BY slot`, project)
	} else {
		rows, err = pool.DB().QueryContext(ctx,
			`SELECT CAST((julianday('now') - julianday(updated_at)) / 7 AS INTEGER) AS slot,
			        COUNT(*) AS cnt
			   FROM proj_current_tasks
			  WHERE status = 'closed'
			    AND updated_at >= datetime('now', '-91 days')
			    AND updated_at < datetime('now', '-7 days')
			  GROUP BY slot`)
	}
	if err != nil {
		return percentiles{}, err
	}
	defer rows.Close()

	slotMap := make(map[int64]int64)
	for rows.Next() {
		var slot, cnt int64
		if err := rows.Scan(&slot, &cnt); err != nil {
			return percentiles{}, err
		}
		slotMap[slot] = cnt
	}
	if err := rows.Err(); err != nil {
		return percentiles{}, err
	}

	// Fill zeros for every slot in the trailing window so quiet weeks count
	// against the baseline.
	weekly := make([]int64, 0, trailingWeeks)
	activeWeeks := 0
	for s := int64(1); s <= int64(trailingWeeks); s++ {
		c := slotMap[s]
		weekly = append(weekly, c)
		if c > 0 {
			activeWeeks++
		}
	}

	if activeWeeks < minWeeksForPercentile {
		return fallbackPercentiles(), nil
	}

	sort.Slice(weekly, func(i, j int) bool { return weekly[i] < weekly[j] })
	n := len(weekly)
	pickAt := func(p int) int64 {
		idx := (n * p) / 100
		if idx >= n {
			idx = n - 1
		}
		return weekly[idx]
	}
	return percentiles{
		p25:         pickAt(25),
		p50:         pickAt(50),
		p75:         pickAt(75),
		sampleWeeks: n,
	}, nil
}

func bandwidthLabel(current int64, pct percentiles) string {
	switch {
	case current > pct.p75:
		return "high"
	case current < pct.p25:
		return "low"
	default:
		return "nominal"
	}
}

func priorSignalLabel(hits int) string {
	switch {
	case hits == 0:
		return "weak"
	case hits <= 3:
		return "mid"
	default:
		return "strong"
	}
}

// extractKeywords pulls discriminating tokens from the task spec for the vault
// scan. Splits on non-alphanumeric (preserving '-' and '_'), drops stopwords +
// tokens < 4 chars, dedupes, caps at 8.
func extractKeywords(taskSpec string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, maxKeywords)

	splitFn := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	}
	for _, raw := range strings.FieldsFunc(taskSpec, splitFn) {
		lower := strings.ToLower(strings.Trim(raw, "-_"))
		if len(lower) < keywordMinLen {
			continue
		}
		if _, drop := stopwords[lower]; drop {
			continue
		}
		// Require at least one alphabetic char (drops pure-number tokens).
		hasAlpha := false
		for _, r := range lower {
			if unicode.IsLetter(r) {
				hasAlpha = true
				break
			}
		}
		if !hasAlpha {
			continue
		}
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, lower)
		if len(out) >= maxKeywords {
			break
		}
	}
	return out
}

// summaryMaxChars caps the body excerpt extracted as `summary`, matching
// knowledge_lib::vault::SUMMARY_MAX_CHARS in the Rust source.
const summaryMaxChars = 160

// vaultEntry mirrors knowledge_lib::vault::VaultEntry — the parsed shape Rust
// builds for each markdown file before matching keywords. Keeping the same
// (path + title + tags + summary) haystack matters for parity: matching against
// raw body content (the original Go scan) would over-count compared to Rust.
type vaultEntry struct {
	path    string
	title   string
	tags    []string
	summary string
}

// scanVault walks the vault's decisions/ subtree, parses frontmatter + body
// per file, and counts entries whose (path + title + tags + summary) haystack
// substring-matches any keyword. Mirrors measure_lib::chain_assessment::scan_vault
// + knowledge_lib::vault::walk.
//
// Soft-failure semantics: vault unavailable returns (0, nil) rather than an
// error, so chain-assessment dispatch never blocks on a missing vault.
func scanVault(vaultRoot string, keywords []string) (int, []string) {
	if len(keywords) == 0 {
		return 0, nil
	}
	root := vaultRoot
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, nil
		}
		root = filepath.Join(home, ".claude", "vault")
	}
	decisionsDir := filepath.Join(root, "decisions")
	if _, err := os.Stat(decisionsDir); err != nil {
		return 0, nil
	}

	lowerKeywords := make([]string, len(keywords))
	for i, k := range keywords {
		lowerKeywords[i] = strings.ToLower(k)
	}

	var hits []string
	_ = filepath.WalkDir(decisionsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		// Forward-slash normalize to match Rust's `forward-slash-normalized`
		// VaultEntry.path on case the host filesystem disagrees.
		rel = filepath.ToSlash(rel)

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		entry := parseVaultEntry(rel, string(content))
		haystack := strings.ToLower(
			entry.path + " " + entry.title + " " +
				strings.Join(entry.tags, " ") + " " + entry.summary,
		)
		for _, kw := range lowerKeywords {
			if strings.Contains(haystack, kw) {
				hits = append(hits, rel)
				break
			}
		}
		return nil
	})

	sort.Strings(hits)
	total := len(hits)
	if total > maxPathsInProse {
		return total, hits[:maxPathsInProse]
	}
	return total, hits
}

// parseVaultEntry extracts (path, title, tags, summary) from a vault file's
// raw content. Mirrors knowledge_lib::vault::walk's per-entry parse:
//   - title: frontmatter `title` field, else first H1 in body, else filename stem
//   - tags: frontmatter `tags` field (inline [a,b,c] or YAML list), lowercased+deduped
//   - summary: first non-blank, non-heading body line, capped at 160 chars
func parseVaultEntry(relPath, content string) vaultEntry {
	frontmatter, body := splitFrontmatter(content)
	fmTitle, fmTags := parseFrontmatterFields(frontmatter)

	title := fmTitle
	if title == "" {
		title = firstH1(body)
	}
	if title == "" {
		base := filepath.Base(relPath)
		title = strings.TrimSuffix(base, ".md")
	}

	return vaultEntry{
		path:    relPath,
		title:   title,
		tags:    fmTags,
		summary: firstBodyLine(body),
	}
}

// splitFrontmatter returns (frontmatter, body). When the file does not start
// with `---\n`, frontmatter is "" and body is the entire content.
func splitFrontmatter(content string) (string, string) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", content
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
		}
	}
	// Unterminated frontmatter — treat as no frontmatter.
	return "", content
}

// parseFrontmatterFields pulls `title` and `tags` from a YAML frontmatter block.
// Handles tags in inline `[a, b, c]` form and YAML list form (`- a` on subsequent
// indented lines). Other fields are ignored.
func parseFrontmatterFields(fm string) (title string, tags []string) {
	if fm == "" {
		return "", nil
	}
	lines := strings.Split(fm, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "title:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "title:"))
			val = strings.Trim(val, `"'`)
			title = val
			continue
		}
		if strings.HasPrefix(line, "tags:") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "tags:"))
			if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
				inner := strings.TrimSuffix(strings.TrimPrefix(rest, "["), "]")
				for _, t := range strings.Split(inner, ",") {
					t = strings.TrimSpace(t)
					t = strings.Trim(t, `"'`)
					if t != "" {
						tags = appendDedupedLower(tags, t)
					}
				}
				continue
			}
			// YAML list form: `tags:` then subsequent `  - tag` lines.
			for j := i + 1; j < len(lines); j++ {
				item := strings.TrimSpace(lines[j])
				if strings.HasPrefix(item, "- ") {
					t := strings.TrimSpace(strings.TrimPrefix(item, "- "))
					t = strings.Trim(t, `"'`)
					if t != "" {
						tags = appendDedupedLower(tags, t)
					}
					continue
				}
				// First non-list-item line ends the tags block.
				if item == "" {
					continue
				}
				break
			}
		}
	}
	return title, tags
}

func appendDedupedLower(tags []string, t string) []string {
	lower := strings.ToLower(t)
	for _, existing := range tags {
		if existing == lower {
			return tags
		}
	}
	return append(tags, lower)
}

// firstH1 returns the text of the first `# Heading` line in body, or "".
func firstH1(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

// firstBodyLine returns the first non-blank, non-heading line of body, capped
// at summaryMaxChars bytes (mirroring Rust's SUMMARY_MAX_CHARS).
func firstBodyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(trimmed) > summaryMaxChars {
			// floor to a valid UTF-8 boundary so we never mid-rune-cut a body.
			cut := summaryMaxChars
			for cut > 0 && !utf8RuneStart(trimmed[cut]) {
				cut--
			}
			return trimmed[:cut]
		}
		return trimmed
	}
	return ""
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune.
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}
