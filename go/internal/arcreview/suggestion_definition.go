package arcreview

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// frictionVsSuggestionFallback is the embedded fallback definition,
// surfaced when ~/.claude/skills/suggestion-filing-discipline/SKILL.md
// is missing or unparseable. Keeps the prompt assembling cleanly even
// in environments where the skill isn't installed (CI, smoke tests,
// fresh laptops). Per chain `agent-suggestion-box` design_decisions §7,
// the skill file is the single source of truth; this fallback is a
// graceful-degradation contingency, NOT a parallel canonical copy. The
// fallback is byte-identical to the skill body's blockquote at the time
// of writing so that a missing-skill prompt still expresses the same
// distinction.
const frictionVsSuggestionFallback = `Friction is something that interrupted the normal flow, slowed you down, confused you, and is unintentional in our design. Suggestions are friction which go against past decisions in favour of optimizations we can see now, argue for removing content from code/prose if it serves no purpose, suggest missing tests, suggest code cleanup that will help, etc.`

// suggestionDefinitionHeading is the markdown heading in the skill body
// that immediately precedes the verbatim definition blockquote. The
// loader scans for this exact line, then collects every consecutive
// blockquote line that follows. Both the skill file and this constant
// must move in lockstep when the heading is ever renamed.
const suggestionDefinitionHeading = "## The verbatim friction-vs-suggestion definition"

var (
	suggestionDefinitionOnce sync.Once
	suggestionDefinition     string
)

// FrictionVsSuggestionDefinition returns the verbatim friction-vs-suggestion
// distinction read from the skill body. Cached via sync.Once; subsequent
// calls are zero-cost. When the skill file is missing or the blockquote
// can't be extracted, returns the embedded fallback string and logs no
// error — graceful degradation per the chain's "skill file is single
// source of truth, fallback keeps the prompt assembling" stance.
//
// Resolution path: $HOME/.claude/skills/suggestion-filing-discipline/SKILL.md.
// No env-var override today; the skill location is fixed in the
// cross-project agent-OS conventions.
func FrictionVsSuggestionDefinition() string {
	suggestionDefinitionOnce.Do(func() {
		path := suggestionSkillPath()
		if path == "" {
			suggestionDefinition = frictionVsSuggestionFallback
			return
		}
		text, err := extractFrictionVsSuggestionBlockquote(path)
		if err != nil || text == "" {
			suggestionDefinition = frictionVsSuggestionFallback
			return
		}
		suggestionDefinition = text
	})
	return suggestionDefinition
}

// suggestionSkillPath returns the on-disk path to the
// suggestion-filing-discipline skill body, or "" when the home dir
// can't be resolved. Used by FrictionVsSuggestionDefinition; isolated
// so tests can inject a custom path via SKILL_ROOT-style fixtures
// without monkeypatching the loader.
func suggestionSkillPath() string {
	if root := os.Getenv("TOOLKIT_SUGGESTION_SKILL_ROOT"); root != "" {
		return filepath.Join(root, "suggestion-filing-discipline", "SKILL.md")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills", "suggestion-filing-discipline", "SKILL.md")
}

// extractFrictionVsSuggestionBlockquote reads the skill body, scans
// line-by-line for suggestionDefinitionHeading, then collects every
// subsequent line that begins with `> ` (markdown blockquote prefix)
// until it sees a non-blockquote non-blank line. Joins the
// blockquote-stripped lines with a single space — the prompt slot
// wants prose, not preserved markdown formatting.
//
// Returns ("", nil) when the heading isn't found or no blockquote
// follows it (the caller treats both as "use fallback").
func extractFrictionVsSuggestionBlockquote(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // skill path resolved from user-home dir or env override
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Skill bodies stay well under 1MB; the default scanner buffer (64KB
	// per line) is fine.
	inDefinition := false
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if !inDefinition {
			if strings.TrimSpace(line) == suggestionDefinitionHeading {
				inDefinition = true
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "> "):
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(trimmed, ">")))
		case trimmed == ">":
			// Blank blockquote line — treat as a paragraph break within
			// the blockquote (join with newline preserved).
			lines = append(lines, "")
		case trimmed == "":
			// Allow one blank line between heading and blockquote start,
			// OR between blockquote paragraphs. Only break on a non-blank
			// non-blockquote line — that's the end of the definition.
			if len(lines) == 0 {
				continue
			}
			// Blank line AFTER blockquote content but before content
			// resumes: treat as paragraph break, continue scanning.
			lines = append(lines, "")
		default:
			if len(lines) == 0 {
				// Heading was followed by something else before any
				// blockquote — bail; fallback covers this.
				return "", nil
			}
			// First non-blockquote, non-blank line after the blockquote.
			// Definition done.
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", nil
	}
	// Join with single space, collapsing internal blanks. Strip leading/
	// trailing whitespace.
	return strings.TrimSpace(joinDefinitionLines(lines)), nil
}

// joinDefinitionLines flattens the extracted blockquote-line list into
// a single paragraph string. Consecutive blank entries collapse to a
// single space so a stylistic blank line in the skill body doesn't
// produce double-spacing in the prompt slot.
func joinDefinitionLines(lines []string) string {
	var b strings.Builder
	prevBlank := false
	for _, line := range lines {
		if line == "" {
			if !prevBlank && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevBlank = true
			continue
		}
		if b.Len() > 0 && !prevBlank {
			b.WriteByte(' ')
		}
		b.WriteString(line)
		prevBlank = false
	}
	return b.String()
}
