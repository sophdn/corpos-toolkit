package rubric

import (
	"strings"
	"unicode"
)

// ParsedLabel is the outcome of parsing a classify response.
type ParsedLabel int

const (
	ParsedNone           ParsedLabel = iota // no match and model didn't say unclassifiable
	ParsedSingle                            // exactly one label matched
	ParsedUnclassifiable                    // model explicitly said "unclassifiable"
	ParsedMultiple                          // multiple distinct labels matched
)

// ParseResult holds the outcome and the matched label(s).
type ParseResult struct {
	Kind   ParsedLabel
	Label  string   // set when Kind == ParsedSingle
	Labels []string // set when Kind == ParsedMultiple
}

// ParseSingleClass extracts a label from a model response against the allowed
// set. Tolerates leading bullets, numbered prefixes, backticks, and
// leading/trailing whitespace. Does NOT strip trailing characters — digit
// suffixes like "tier-zero" are valid label content.
func ParseSingleClass(response string, allowed []string) ParseResult {
	lowerAllowed := make([]string, len(allowed))
	for i, a := range allowed {
		lowerAllowed[i] = strings.ToLower(a)
	}

	var matched []string
	unclassifiable := false

	for _, raw := range strings.Split(response, "\n") {
		line := stripLeadingDecoration(strings.TrimSpace(raw))
		if line == "" {
			continue
		}
		for _, piece := range strings.Split(line, ",") {
			cleaned := strings.TrimSpace(piece)
			cleaned = strings.Trim(cleaned, "`")
			cleaned = strings.Trim(cleaned, ":")
			cleaned = strings.TrimSpace(cleaned)
			cleaned = strings.ToLower(cleaned)
			if cleaned == "" {
				continue
			}
			if cleaned == "unclassifiable" {
				unclassifiable = true
				continue
			}
			for i, lower := range lowerAllowed {
				if lower == cleaned {
					canonical := allowed[i]
					dup := false
					for _, m := range matched {
						if m == canonical {
							dup = true
							break
						}
					}
					if !dup {
						matched = append(matched, canonical)
					}
					break
				}
			}
		}
	}

	switch {
	case len(matched) == 0 && unclassifiable:
		return ParseResult{Kind: ParsedUnclassifiable}
	case len(matched) == 0:
		return ParseResult{Kind: ParsedNone}
	case len(matched) == 1:
		return ParseResult{Kind: ParsedSingle, Label: matched[0]}
	default:
		return ParseResult{Kind: ParsedMultiple, Labels: matched}
	}
}

// stripLeadingDecoration removes bullet leaders and numbered-list prefixes
// from the start of a line. Trailing characters are left untouched.
func stripLeadingDecoration(line string) string {
	trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
	for _, prefix := range []string{"- ", "* ", "• "} {
		if after, ok := strings.CutPrefix(trimmed, prefix); ok {
			return strings.TrimLeftFunc(after, unicode.IsSpace)
		}
	}
	// Strip numbered list prefix: optional "(" + digits + "." / ")" / ":".
	i := 0
	if i < len(trimmed) && trimmed[i] == '(' {
		i++
	}
	digitStart := i
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i > digitStart && i < len(trimmed) {
		next := trimmed[i]
		if next == '.' || next == ')' || next == ':' {
			return strings.TrimLeftFunc(trimmed[i+1:], unicode.IsSpace)
		}
	}
	return trimmed
}
