package arcreview

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"toolkit/internal/db"
)

// RecentFiling is one artifact filed earlier in the current arc — i.e.
// before the current substrate review fires. The handler queries
// these from the events table and passes them through DispatchReview
// into ComposeReviewPrompt, where they surface as an "ALREADY FILED"
// anti-pattern block so Qwen doesn't re-propose forge_bug for work
// the user / agent already filed.
//
// Closes bug 1472 (Qwen substrate review proposes forges for bugs /
// notes already filed earlier in the same arc).
//
// Today the closed set is BugReported only — that's the only event
// type the dispatch boundary persists when an artifact lands. Vault
// notes and memory writes don't have typed events yet (the file-side
// indexer in forge/indexsync.go writes knowledge_pointers rows but
// doesn't emit a typed event). When those event types land, extend
// recentFilingsInArc's type-filter list.
type RecentFiling struct {
	Kind  string // "bug" today; future: "vault_note", "memory_write"
	Slug  string
	Title string
}

// defaultRecentFilingsWindow is the look-back window for the in-arc
// dedupe enrichment. The snapshot extractor caps at 20 turns of
// conversation; in wall-clock that's typically 10-90 minutes. 30
// minutes is a soft middle that catches most multi-bug arcs without
// pulling in stale filings from earlier sessions.
const defaultRecentFilingsWindow = 30 * time.Minute

// recentFilingsCap is the row-count ceiling on the returned slice.
// Pathological arcs (a sweep of 100 filings) shouldn't bloat the
// review prompt past usefulness; cap at 20 most-recent.
const recentFilingsCap = 20

// Pre-compiled patterns for extractInArcFilingsFromSnapshot. The
// snapshot renders tool_use parts as "[tool_use: <name>] <json-input>"
// (see snapshot.go::renderPart) with the input truncated to ~1200 head
// + 200 tail chars; that head budget reliably contains schema_name,
// title, and file_path fields for typical forge / Write calls.
// JSON field extraction via regex is brittle for adversarial inputs but
// adequate for the snapshot-extraction domain where the JSON was just
// serialized by Claude Code's tool-use machinery.
var (
	toolUseMarkerRe   = regexp.MustCompile(`\[tool_use:\s*[^\]]+\]`)
	schemaNameRe      = regexp.MustCompile(`"schema_name"\s*:\s*"([a-zA-Z0-9_-]+)"`)
	titleRe           = regexp.MustCompile(`"title"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	writeFilePathRe   = regexp.MustCompile(`"file_path"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	vaultPathFragment = "/vault/"
)

// extractInArcFilingsFromSnapshot scans the rendered snapshot for
// agent-issued filings (forge calls + Write/Edit against vault paths)
// and returns them as RecentFilings. Used in addition to
// recentFilingsInArc to dedupe Qwen proposals against work the agent
// completed during the snapshot window — vault notes in particular
// emit no typed event today, so the event-based recentFilingsInArc
// misses them entirely (closes bug 1480; pairs with bug 1472's
// event-based pass).
//
// Detection scope:
//   - forge(vault-note, ...) calls — slug taken from title.
//   - forge(bug, ...) calls — supplements the event-based query for
//     when the agent forged inside the snapshot window but no events
//     row landed yet (e.g. mid-batch).
//   - Write/Edit tool_use entries whose file_path is under a /vault/
//     directory — taken as a vault_note filing with the file's basename
//     as the slug.
//
// Returns may contain duplicates against recentFilingsInArc; the
// caller dedupes by (kind, slug) before passing to the prompt
// composer.
func extractInArcFilingsFromSnapshot(snap Snapshot) []RecentFiling {
	if len(snap.Messages) == 0 {
		return nil
	}
	var out []RecentFiling
	for _, msg := range snap.Messages {
		if msg.Role != "assistant" {
			continue
		}
		// Split on every "[tool_use: ..." marker; each suffix-segment
		// up to the next marker is one tool's rendered input. We
		// retain the marker text by using FindAllStringIndex.
		idxs := toolUseMarkerRe.FindAllStringIndex(msg.Content, -1)
		for i, span := range idxs {
			end := len(msg.Content)
			if i+1 < len(idxs) {
				end = idxs[i+1][0]
			}
			marker := msg.Content[span[0]:span[1]]
			input := msg.Content[span[1]:end]
			if filing, ok := parseToolUseSegment(marker, input); ok {
				out = append(out, filing)
			}
		}
	}
	return out
}

// parseToolUseSegment inspects one rendered tool_use segment and
// returns a RecentFiling when it looks like a filing the dedupe layer
// should mention. Returns (_, false) for non-filing tool calls.
func parseToolUseSegment(marker, input string) (RecentFiling, bool) {
	// forge-shaped tool_uses surface the schema name in JSON; Write/
	// Edit surface a file_path. Inspect both — a single segment can
	// only match one (mutually exclusive content shapes).
	if m := schemaNameRe.FindStringSubmatch(input); m != nil {
		schema := m[1]
		switch schema {
		case "vault-note":
			title := captureTitle(input)
			if title == "" {
				return RecentFiling{}, false
			}
			return RecentFiling{Kind: "vault_note", Slug: titleToSlugApprox(title), Title: title}, true
		case "bug":
			title := captureTitle(input)
			if title == "" {
				return RecentFiling{}, false
			}
			return RecentFiling{Kind: "bug", Slug: titleToSlugApprox(title), Title: title}, true
		case "suggestion":
			title := captureTitle(input)
			if title == "" {
				return RecentFiling{}, false
			}
			return RecentFiling{Kind: "suggestion", Slug: titleToSlugApprox(title), Title: title}, true
		}
	}
	if strings.Contains(marker, "Write") || strings.Contains(marker, "Edit") {
		if m := writeFilePathRe.FindStringSubmatch(input); m != nil {
			path := m[1]
			if !strings.Contains(path, vaultPathFragment) {
				return RecentFiling{}, false
			}
			base := filepath.Base(path)
			slug := strings.TrimSuffix(base, filepath.Ext(base))
			return RecentFiling{Kind: "vault_note", Slug: slug, Title: base}, true
		}
	}
	return RecentFiling{}, false
}

// captureTitle reads the first "title" field out of a truncated JSON
// fragment. Returns "" when the field is absent or unterminated.
func captureTitle(input string) string {
	m := titleRe.FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	// Resolve simple JSON escape sequences. The forge dispatcher
	// re-derives the slug from the unescaped title, so dedupe match
	// quality benefits from unescaping here too.
	return jsonUnescape(m[1])
}

// jsonUnescape resolves the subset of JSON escape sequences likely to
// appear in titles (\", \\, \n, \t, \/). A full JSON-string parse is
// overkill — we just want a clean dedupe key.
func jsonUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\', '/':
				b.WriteByte(s[i+1])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i])
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// titleToSlugApprox returns a slug approximation matching the forge
// dispatcher's identity-from-title convention (lower-case, ASCII
// alphanumerics + hyphens). Used only as a dedupe-comparison key, not
// as the canonical slug — the dispatcher's actual slug derivation
// (forge/slugify.go) is the source of truth for stored rows.
func titleToSlugApprox(title string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '-' || r == '_':
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// mergeRecentFilings combines two RecentFiling slices, deduping by
// (kind, slug). The first slice's entries take precedence (kept
// position-stable for prompt ordering); duplicates from the second
// drop. Empty entries (no slug) are skipped.
func mergeRecentFilings(primary, secondary []RecentFiling) []RecentFiling {
	out := make([]RecentFiling, 0, len(primary)+len(secondary))
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	for _, r := range primary {
		if r.Slug == "" {
			continue
		}
		key := r.Kind + "::" + r.Slug
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	for _, r := range secondary {
		if r.Slug == "" {
			continue
		}
		key := r.Kind + "::" + r.Slug
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// recentFilingsInArc queries the events table for BugReported AND
// SuggestionReported events in the project within the look-back window.
// Returns the rows ordered newest-first so the prompt enrichment leads
// with the most relevant filings. Bug and suggestion entries are
// distinguished by the Kind field on each row ("bug" vs "suggestion").
//
// SuggestionReported was added by chain `agent-suggestion-box` T7; the
// in-arc dedupe applies to both surfaces because the agent should not
// re-propose a suggestion the same session already filed, same way it
// shouldn't re-propose a bug.
//
// Errors are non-fatal at the caller (handler.go): a query failure
// drops back to the un-enriched review rather than failing the fire.
func recentFilingsInArc(ctx context.Context, pool *db.Pool, project string, since time.Time) ([]RecentFiling, error) {
	if pool == nil {
		return nil, fmt.Errorf("recentFilingsInArc: pool is nil")
	}
	sinceStr := since.UTC().Format(time.RFC3339Nano)
	rows, err := pool.DB().QueryContext(ctx, `
		SELECT type, entity_slug, payload
		FROM events
		WHERE type IN ('BugReported', 'SuggestionReported')
		  AND entity_project_id = ?
		  AND ts >= ?
		ORDER BY ts DESC
		LIMIT ?
	`, project, sinceStr, recentFilingsCap)
	if err != nil {
		return nil, fmt.Errorf("query recent filings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RecentFiling
	for rows.Next() {
		var typ, slug, payloadJSON string
		if err := rows.Scan(&typ, &slug, &payloadJSON); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		var p struct {
			Title string `json:"title"`
		}
		// Best-effort title extraction; an unparseable payload still
		// surfaces the slug, which is the load-bearing dedup key.
		_ = json.Unmarshal([]byte(payloadJSON), &p)
		kind := "bug"
		if typ == "SuggestionReported" {
			kind = "suggestion"
		}
		out = append(out, RecentFiling{Kind: kind, Slug: slug, Title: p.Title})
	}
	return out, nil
}
