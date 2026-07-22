package refresolve

import (
	"regexp"
	"strings"
)

// frictionPatterns is the closed list of observation-of-friction
// phrases the detector recognizes. Expansion requires a chain-level
// decision (the T6 supersession verification ran against this list;
// adding patterns changes the supersession-coverage guarantee).
//
// Patterns are case-insensitive substring matches; the detector
// uses (?i) at the regex level. Each pattern names a "noticing"
// shape: the user is reporting friction without filing it as a bug.
//
// Trigger-phrase list inherited from the retired
// friction-filing-reminder.sh Stop hook (removed 2026-05-19 in chain
// arc-close-filing-review T1.5) plus a small set of
// observed-but-not-caught patterns from past sessions.
var frictionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\balso\s+noted\b`),
	regexp.MustCompile(`(?i)\bcould\s+file\b`),
	regexp.MustCompile(`(?i)\bnoted\s+but\s+not\s+filed\b`),
	regexp.MustCompile(`(?i)\bwant\s+me\s+to\s+file\b`),
	regexp.MustCompile(`(?i)\bminor\s+observation\b`),
	regexp.MustCompile(`(?i)\bworth\s+filing\b`),
	regexp.MustCompile(`(?i)\bseparate\s+bug\b`),
	regexp.MustCompile(`(?i)\bpaper\s+cut\b`),
	// Broader patterns observed in user-side friction reports.
	regexp.MustCompile(`(?i)\bthat['']?s\s+weird\b`),
	regexp.MustCompile(`(?i)\bannoying\s+that\b`),
	regexp.MustCompile(`(?i)\bwhy\s+does\s+\w+(?:\s+\w+)?\s+always\b`),
	regexp.MustCompile(`(?i)\bthis\s+should\s+just\s+work\b`),
	regexp.MustCompile(`(?i)\bshouldn['']?t\s+(?:have\s+to|need\s+to|need)\b`),
	regexp.MustCompile(`(?i)\bworkaround\b`),
	regexp.MustCompile(`(?i)\bworking\s+around\b`),
	regexp.MustCompile(`(?i)\bunexpected(?:ly)?\s+(?:broken|fails|failed)\b`),
}

// detectFrictionShape emits at most ONE Reference per message —
// friction-shape is whole-message-level, not token-level. The
// matched substring is recorded as the Reference's Token (for
// debug surfaces); StartPos/EndPos point at the first matching
// pattern.
//
// Multiple matches in one message collapse to one Reference; the
// agent's filing suggestion covers "this message contains observed
// friction" — counting matches isn't load-bearing.
//
// Confidence is fixed at 1.0 for pattern hits — the patterns are
// closed and the detector promise is "this message contains a
// recognized friction phrase." The Qwen rubric refinement
// mentioned in the T6 description is deferred to a follow-on when
// real-data patterns require it; the closed pattern list catches
// the documented cases.
func detectFrictionShape(message string) []Reference {
	if message == "" {
		return nil
	}
	// Walk patterns in order; first match wins. Multiple-match
	// case is fine — a follow-on Reference per pattern would
	// duplicate the filing suggestion.
	for _, re := range frictionPatterns {
		if m := re.FindStringIndex(message); m != nil {
			return []Reference{{
				Token:           strings.TrimSpace(message[m[0]:m[1]]),
				Shape:           ShapeFrictionShape,
				Confidence:      1.0,
				DetectionMethod: "regex_pattern",
				StartPos:        m[0],
				EndPos:          m[1],
			}}
		}
	}
	return nil
}
