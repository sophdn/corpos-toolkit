package grounding

import (
	"path"
	"strings"
)

// normalizeSlug strips vault-style path prefixes and the .md suffix so a
// source_ref like "learnings/general/2026-05-12_floor-char-boundary.md"
// matches assistant-text references using only "2026-05-12_floor-char-boundary".
// TT1.5 §7.1 documented that 80%+ of `mentioned` references in real
// transcripts use the slug form, not the full path.
//
// Behavior:
//   - The final path segment is taken (strips any number of leading
//     directories: "learnings/general/", "decisions/", "vault/...", etc.).
//   - A trailing ".md" extension is removed; other extensions stay.
//   - kiwix-style refs of the form "<zim>::<slug>" are passed through —
//     splitting on path separators would mangle the zim_id.
//   - Empty/whitespace-only refs return "" so the detector can skip them.
func normalizeSlug(sourceRef string) string {
	s := strings.TrimSpace(sourceRef)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "::") {
		return s
	}
	base := path.Base(s)
	return strings.TrimSuffix(base, ".md")
}
