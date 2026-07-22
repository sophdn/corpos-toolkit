package refresolve

import (
	"context"
	"regexp"
	"strings"
	"unicode"
)

// titleCasePhraseRe matches sequences of capitalized words (Title
// Case Phrase) of 2-5 tokens. Captures the phrase only — boundary
// handling sits outside the capture group.
//
// Conservative: requires explicit capitalization of every word
// (rules out plain English with one accidentally-capitalized
// pronoun); allows hyphens within tokens (so "Self-Attention" is
// one token).
var titleCasePhraseRe = regexp.MustCompile(
	`(?:^|[^A-Za-z])` +
		`([A-Z][a-z]+(?:-[A-Za-z]+)?(?:\s+[A-Z][a-z]+(?:-[A-Za-z]+)?){1,4})` +
		`(?:[^A-Za-z]|$)`)

// detectExternalTechnical emits a Reference for each title-cased
// multi-word phrase that hasn't already been matched by a higher-
// priority shape. Falls through to a kiwix lookup on resolve.
//
// Skipped when:
//   - the phrase starts inside an already-emitted reference's span
//   - all the words are common English (very small stop-list)
//
// Confidence is fixed at 0.5 — heuristic detection only; the
// resolver's hit-or-miss is the real signal.
func detectExternalTechnical(message string, alreadyEmitted []Reference) []Reference {
	if message == "" {
		return nil
	}
	out := []Reference{}
	spans := spansFromReferences(alreadyEmitted)
	for _, m := range titleCasePhraseRe.FindAllStringSubmatchIndex(message, -1) {
		if len(m) < 4 {
			continue
		}
		start, end := m[2], m[3]
		// Skip overlap with higher-priority detections.
		if overlapsAnySpan(start, end, spans) {
			continue
		}
		token := message[start:end]
		if isCommonStartOfSentencePhrase(token) {
			continue
		}
		out = append(out, Reference{
			Token:           token,
			Shape:           ShapeExternalTechnical,
			Confidence:      0.5,
			DetectionMethod: "heuristic",
			StartPos:        start,
			EndPos:          end,
		})
	}
	return out
}

type span struct{ start, end int }

func spansFromReferences(refs []Reference) []span {
	out := make([]span, 0, len(refs))
	for _, r := range refs {
		out = append(out, span{start: r.StartPos, end: r.EndPos})
	}
	return out
}

func overlapsAnySpan(start, end int, spans []span) bool {
	for _, s := range spans {
		if start < s.end && end > s.start {
			return true
		}
	}
	return false
}

// commonSentenceStarts catches phrases that are almost always
// English at the start of a sentence, not a technical concept.
// Closed set; expansion requires a chain-level decision.
var commonSentenceStarts = map[string]bool{
	"I Am":      true,
	"We Are":    true,
	"You Are":   true,
	"They Are":  true,
	"It Is":     true,
	"This Is":   true,
	"That Is":   true,
	"There Is":  true,
	"There Are": true,
	"Here Is":   true,
	"Here Are":  true,
	"Let Me":    true,
	"Let Us":    true,
	"I Have":    true,
	"I Was":     true,
	"I Did":     true,
	"I Do":      true,
	"I Will":    true,
	"I Would":   true,
}

func isCommonStartOfSentencePhrase(s string) bool {
	// Match the first two title-cased words against the stop list.
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return false
	}
	key := fields[0] + " " + fields[1]
	return commonSentenceStarts[key]
}

// detectDomainTerm extracts candidate domain-term phrases from the
// message and consults the classifier for each candidate. Emits a
// Reference for each phrase the classifier flags as a domain term
// with confidence ≥ 0.6 (the threshold lives in design doc §2.1).
//
// Candidate phrases come from the same title-cased phrase regex
// used for external-technical detection, MINUS already-matched
// higher-priority shapes (slugs, paths, skill/project/tool/schema/
// library catalogs). The classifier sees only short noun-phrase
// candidates, not whole messages.
func detectDomainTerm(ctx context.Context, message string, alreadyEmitted []Reference, classifier DomainTermClassifier) ([]Reference, error) {
	if message == "" || classifier == nil {
		return nil, nil
	}
	spans := spansFromReferences(alreadyEmitted)
	out := []Reference{}
	for _, m := range titleCasePhraseRe.FindAllStringSubmatchIndex(message, -1) {
		if len(m) < 4 {
			continue
		}
		start, end := m[2], m[3]
		if overlapsAnySpan(start, end, spans) {
			continue
		}
		phrase := message[start:end]
		if isCommonStartOfSentencePhrase(phrase) {
			continue
		}
		isDomain, conf, err := classifier.IsDomainTerm(ctx, phrase)
		if err != nil {
			// Per design doc §2.1 + §2.4: classifier failure falls
			// back to a permissive default — surface no domain term
			// (not the wrong shape), don't abort detection.
			continue
		}
		if !isDomain || conf < 0.6 {
			continue
		}
		out = append(out, Reference{
			Token:           phrase,
			Shape:           ShapeDomainTerm,
			Confidence:      conf,
			DetectionMethod: "rubric",
			StartPos:        start,
			EndPos:          end,
		})
	}
	return out, nil
}

// looksLikeTitleCasePhrase is a helper for tests; reports whether
// the supplied phrase would be picked up by the title-cased phrase
// regex. Wraps the phrase with sentinel boundary chars before
// matching so we don't depend on context.
func looksLikeTitleCasePhrase(phrase string) bool {
	wrapped := " " + phrase + " "
	m := titleCasePhraseRe.FindStringSubmatchIndex(wrapped)
	return len(m) >= 4
}

// hasAnyLetter is a small helper used by callers that want to
// reject phrases that are pure punctuation. Cheap; called rarely.
func hasAnyLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
