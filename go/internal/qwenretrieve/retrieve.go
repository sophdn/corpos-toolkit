package qwenretrieve

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"toolkit/internal/inference/router"
	"toolkit/internal/obs"
)

// RetrieveMaxTokens caps the model's response tokens for retrieve calls.
// Sized so a top_k=20 response (~20 paths × ~30 chars ≈ 600 chars ≈ 200 tokens)
// fits comfortably with headroom for over-long paths. Mirrors the Rust budget.
const RetrieveMaxTokens = 300

// ParseRetrieveResponse pulls an ordered, deduplicated list of known paths out
// of Qwen's retrieve response. Strips bullet/list prefixes, backtick wrappers,
// and trailing punctuation; returns empty on a leading "no match" line.
//
// Ported verbatim from inference_clients::dispatcher::retrieve::parse_retrieve_response.
func ParseRetrieveResponse(response string, knownPaths []string) []string {
	known := make(map[string]struct{}, len(knownPaths))
	for _, p := range knownPaths {
		known[p] = struct{}{}
	}

	var out []string
	seen := make(map[string]struct{})

	for _, raw := range strings.Split(response, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if lower == "no match" || lower == "'no match'" {
			if len(out) == 0 {
				return nil
			}
			break
		}
		// Left-strip bullets, numbered prefixes, parens, backticks, whitespace.
		// Digits and dots are stripped LEFT only (part of "1. " or "(2) " prefixes)
		// — preserved on the right so paths ending in ".md" or dates survive.
		stripped := strings.TrimLeftFunc(line, func(r rune) bool {
			switch r {
			case '-', '*', '`', '.', '(', ')':
				return true
			}
			if unicode.IsSpace(r) || unicode.IsDigit(r) {
				return true
			}
			return false
		})
		cleaned := strings.TrimRightFunc(stripped, func(r rune) bool {
			switch r {
			case '`', '.', ',':
				return true
			}
			return unicode.IsSpace(r)
		})
		if _, ok := known[cleaned]; !ok {
			continue
		}
		if _, dup := seen[cleaned]; dup {
			continue
		}
		seen[cleaned] = struct{}{}
		out = append(out, cleaned)
	}
	return out
}

// TwoPassRetrieveResult is the aggregated outcome of one retrieve dispatch
// (single-pass kiwix shape OR two-pass vault shape). Telemetry fields per pass
// are populated; callers persist these to the corpus-specific journal table.
//
// FellBack is true when the most-detailed pass returned empty and the result
// fell back to a less-reranked order:
//   - vault (two-pass): pass-2 emptied → pass-1's order surfaced.
//   - kiwix (single-pass): pass-1 emptied → original input order surfaced.
//
// Pass-2 *inference error* (as opposed to empty parse) does NOT flip the flag —
// it silently uses pass-1, matching the prior vault_search behaviour.
type TwoPassRetrieveResult struct {
	RankedPaths       []string
	FellBack          bool
	Pass1LatencyMS    int64
	Pass1InputTokens  *int64
	Pass1OutputTokens *int64
	Pass2LatencyMS    *int64
	Pass2InputTokens  *int64
	Pass2OutputTokens *int64
}

// BodyExcerptProvider returns body excerpts for the given paths. Vault uses an
// on-disk read; future kiwix two-pass would use HTTP refetch. Synchronous —
// the deployed vault read is synchronous; revisit if a future provider needs
// async I/O.
type BodyExcerptProvider func(paths []string) []string

// SelectPass2OrFallback chooses between pass-2's reranked output and a fallback
// to pass-1. Exposed (capitalized) for test reuse; the call site applies it
// after pass-2 parsing.
//
// Returns (selected, fell_back):
//   - pass-2 non-empty → use pass-2 (fell_back = false).
//   - pass-2 empty, pass-1 non-empty → fall back to pass-1 (fell_back = true).
//     Pass-2's "no match" was meant for the genuine no-match case; in practice
//     it over-rejects when candidates are broadly on-topic.
//   - both empty → return empty (fell_back = false; nothing to fall back to).
func SelectPass2OrFallback(pass1Ranked, pass2Ranked []string) ([]string, bool) {
	if len(pass2Ranked) > 0 {
		return pass2Ranked, false
	}
	if len(pass1Ranked) > 0 {
		return pass1Ranked, true
	}
	return pass2Ranked, false
}

// DispatchTwoPassRetrieve runs a Qwen-driven retrieve dispatch, optionally
// chaining a pass-2 body-excerpt rerank.
//
// bodyProvider semantics:
//   - nil → single-pass mode (kiwix shape). Pass-1 only. Parse-empty returns
//     input candidate order with fell_back=true. Pass-1 inference error
//     returns err so callers choose their own degraded-path response.
//   - non-nil → two-pass mode (vault shape). Pass-1, then if pass-1 returned
//     ≥2 paths, fetch bodies via bodyProvider and run pass-2. Pass-2 parse-empty
//     falls back to pass-1 (fell_back=true). Pass-2 inference error logs and
//     uses pass-1 unchanged (fell_back=false).
//
// Empty candidates short-circuit to a zeroed result — no Qwen call.
func DispatchTwoPassRetrieve(
	ctx context.Context,
	r *router.Router,
	query string,
	topK int,
	candidates []RetrieveCandidate,
	corpusShape CorpusShape,
	bodyProvider BodyExcerptProvider,
) (TwoPassRetrieveResult, error) {
	if len(candidates) == 0 {
		return TwoPassRetrieveResult{}, nil
	}

	knownPaths := make([]string, len(candidates))
	for i, c := range candidates {
		knownPaths[i] = c.Path
	}

	pass1System, pass1User := ComposeRetrieve(
		RetrieveTaskInput{Query: query, TopK: topK},
		RetrieveContext{Candidates: candidates, WithBody: false, CorpusShape: corpusShape},
	)

	pass1, err := r.GenerateWithOpts(ctx, pass1User, pass1System, router.GenerateOpts{MaxTokens: RetrieveMaxTokens})
	if err != nil {
		return TwoPassRetrieveResult{}, fmt.Errorf("dispatch retrieve pass-1: %w", err)
	}
	pass1Paths := ParseRetrieveResponse(pass1.Text, knownPaths)

	result := TwoPassRetrieveResult{
		Pass1LatencyMS:    pass1.LatencyMS,
		Pass1InputTokens:  pass1.InputTokens,
		Pass1OutputTokens: pass1.OutputTokens,
	}

	if bodyProvider == nil {
		// Single-pass mode: parse-empty → fall back to input order.
		if len(pass1Paths) == 0 {
			result.RankedPaths = knownPaths
			result.FellBack = true
			return result, nil
		}
		result.RankedPaths = pass1Paths
		return result, nil
	}

	// Two-pass mode. <2 pass-1 results → no rerank to perform; surface pass-1
	// unchanged with fell_back=false (genuine narrow match, not degradation).
	if len(pass1Paths) < 2 {
		result.RankedPaths = pass1Paths
		return result, nil
	}

	bodies := bodyProvider(pass1Paths)
	byPath := make(map[string]RetrieveCandidate, len(candidates))
	for _, c := range candidates {
		byPath[c.Path] = c
	}
	pass2Candidates := make([]RetrieveCandidate, 0, len(pass1Paths))
	pass2Known := make([]string, 0, len(pass1Paths))
	for i, p := range pass1Paths {
		base, ok := byPath[p]
		if !ok {
			continue
		}
		// Clone with body excerpt populated when non-empty.
		cand := base
		cand.Tags = append([]string(nil), base.Tags...)
		if i < len(bodies) && bodies[i] != "" {
			body := bodies[i]
			cand.BodyExcerpt = &body
		}
		pass2Candidates = append(pass2Candidates, cand)
		pass2Known = append(pass2Known, p)
	}

	pass2System, pass2User := ComposeRetrieve(
		RetrieveTaskInput{Query: query, TopK: topK},
		RetrieveContext{Candidates: pass2Candidates, WithBody: true, CorpusShape: corpusShape},
	)
	pass2, err := r.GenerateWithOpts(ctx, pass2User, pass2System, router.GenerateOpts{MaxTokens: RetrieveMaxTokens})
	if err != nil {
		// Pass-2 inference error → degrade to pass-1 silently (matches the
		// prior vault_search behaviour where pass-2 timeout was a logged
		// degradation rather than a user-visible signal).
		obs.Logger(ctx).Warn("qwenretrieve: two_pass_retrieve pass-2 failed; using pass-1 result",
			slog.String("err", err.Error()))
		result.RankedPaths = pass1Paths
		return result, nil
	}
	parsed := ParseRetrieveResponse(pass2.Text, pass2Known)
	final, fellBack := SelectPass2OrFallback(pass1Paths, parsed)
	result.RankedPaths = final
	result.FellBack = fellBack
	latency := pass2.LatencyMS
	result.Pass2LatencyMS = &latency
	result.Pass2InputTokens = pass2.InputTokens
	result.Pass2OutputTokens = pass2.OutputTokens
	return result, nil
}
