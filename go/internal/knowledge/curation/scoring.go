package curation

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"toolkit/internal/inference/llamacpp"
)

// EXCERPT_CHARS bounds how much source material is passed to Qwen in
// either Extract or Score. Matches the Rust EXCERPT_CHARS in
// benchmarks/src/bin/knowledge_curate.rs.
const ExcerptChars = 2000

// ExtractionSystem is the system prompt for QwenExtract. Byte-identical
// to the Rust EXTRACTION_SYSTEM so scores from Go and Rust runs remain
// comparable for the duration of the migration.
const ExtractionSystem = `You extract retrieval metadata from a technical knowledge source.
Output exactly three labeled lines and nothing else:
QUESTION: <one precise question this source can answer>
INVOKE_WHEN: <2-3 sentences describing when an agent should retrieve this source>
DESCRIPTION: <1-2 sentences summarising what the source contains>`

// ScoringSystem is the system prompt for QwenScore. Byte-identical to
// the Rust SCORING_SYSTEM. Description is intentionally withheld from
// the scorer — it would inflate scores because the scorer would grade
// what it itself wrote.
const ScoringSystem = `You assess whether a knowledge source can answer a specific question.
Reply with a decimal score between 0.00 and 1.00, then one sentence of explanation.
No other output. Format: "<score> <explanation>"
Score meaning: 1.0 = source directly and completely answers the question.
0.0 = source is irrelevant to the question.`

// ExtractedMeta is the parsed result of QwenExtract.
type ExtractedMeta struct {
	Question    string
	InvokeWhen  string
	Description string
}

// QwenExtract runs the extraction prompt against the inference client
// and parses the three-labeled-line response into structured metadata.
//
// Returns an error if the underlying generate call fails OR if the
// response cannot be parsed (missing labels, empty question/invoke_when).
// Callers MUST handle the error explicitly — there is no fallback to
// templated metadata. The silent-failure pattern that produced today's
// curation backlog noise is structurally absent from this surface.
func QwenExtract(
	ctx context.Context,
	client *llamacpp.Client,
	sourceType, sourceRef, sourceMaterial string,
) (ExtractedMeta, error) {
	excerpt := truncateChars(sourceMaterial, ExcerptChars)
	prompt := fmt.Sprintf(
		"Source type: %s\nSource identifier: %s\nContent:\n%s",
		sourceType, sourceRef, excerpt,
	)

	resp, err := client.Complete(ctx, llamacpp.CompletionRequest{
		Model: "qwen2.5-32b",
		Messages: []llamacpp.Message{
			{Role: "system", Content: ExtractionSystem},
			{Role: "user", Content: prompt},
		},
		MaxTokens: 1024,
	})
	if err != nil {
		return ExtractedMeta{}, fmt.Errorf("curation extract: generate: %w", err)
	}
	if len(resp.Choices) == 0 {
		return ExtractedMeta{}, fmt.Errorf("curation extract: empty choices")
	}

	meta, ok := ParseExtraction(resp.Choices[0].Message.Content)
	if !ok {
		return ExtractedMeta{}, fmt.Errorf("curation extract: unparseable response: %q",
			resp.Choices[0].Message.Content)
	}
	return meta, nil
}

// ParseExtraction parses the three-line Qwen response into structured
// metadata. Returns (zero, false) if any required label is missing or
// the question or invoke_when fields are empty. Description being empty
// is allowed — the source may be a thin reference.
//
// Pure function; the only thing Qwen-specific here is the label format.
func ParseExtraction(text string) (ExtractedMeta, bool) {
	var meta ExtractedMeta
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "QUESTION: "); ok {
			meta.Question = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "INVOKE_WHEN: "); ok {
			meta.InvokeWhen = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "DESCRIPTION: "); ok {
			meta.Description = strings.TrimSpace(v)
		}
	}
	if meta.Question == "" || meta.InvokeWhen == "" {
		return ExtractedMeta{}, false
	}
	return meta, true
}

// QwenScore runs the adversarial scoring prompt — the question + source
// material, NOT the description — and returns a [0.0, 1.0] score.
//
// Returns an error if generate fails or the response is unparseable
// (no leading decimal, out-of-bounds value). No fallback.
func QwenScore(
	ctx context.Context,
	client *llamacpp.Client,
	question, sourceMaterial string,
) (float64, error) {
	excerpt := truncateChars(sourceMaterial, ExcerptChars)
	prompt := fmt.Sprintf(
		"Question: %s\n\nSource content (first %d characters):\n%s\n\nCan this source answer the question above?",
		question, ExcerptChars, excerpt,
	)

	resp, err := client.Complete(ctx, llamacpp.CompletionRequest{
		Model: "qwen2.5-32b",
		Messages: []llamacpp.Message{
			{Role: "system", Content: ScoringSystem},
			{Role: "user", Content: prompt},
		},
		MaxTokens: 128,
	})
	if err != nil {
		return 0, fmt.Errorf("curation score: generate: %w", err)
	}
	if len(resp.Choices) == 0 {
		return 0, fmt.Errorf("curation score: empty choices")
	}

	score, ok := ParseScore(resp.Choices[0].Message.Content)
	if !ok {
		return 0, fmt.Errorf("curation score: unparseable response: %q",
			resp.Choices[0].Message.Content)
	}
	return score, nil
}

// ParseScore extracts the leading decimal token from text. Returns
// (0, false) for malformed input, empty input, or values outside
// [0.0, 1.0].
//
// Pure function; mirrors the Rust parse_score behavior exactly.
func ParseScore(text string) (float64, bool) {
	trimmed := strings.TrimSpace(text)
	first, _, _ := strings.Cut(trimmed, " ")
	if first == "" {
		return 0, false
	}
	score, err := strconv.ParseFloat(first, 64)
	if err != nil {
		return 0, false
	}
	if score < 0.0 || score > 1.0 {
		return 0, false
	}
	return score, true
}

// truncateChars returns s truncated to at most n runes (not bytes —
// matches Rust's chars().take(n)).
func truncateChars(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
