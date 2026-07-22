package construct

import (
	"strings"
)

// optionalStr maps "" → nil so an omitted optional field is absent from the
// marshaled event payload rather than persisting as an empty string. Forge
// uses the same convention (forge.optionalStringPtr); matching it keeps the
// construct payload byte-identical to forge's for equivalent input (B-V/EV).
func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// deriveSlug reproduces forge's slug-derive: explicit slug wins; otherwise the
// slug is the title run through SlugifyTitle (relocated into construct at
// P2-C.2; the canonical title→slug rule, so construct can't drift — B-G3).
func deriveSlug(slug, title string) string {
	if strings.TrimSpace(slug) != "" {
		return slug
	}
	return SlugifyTitle(title)
}
