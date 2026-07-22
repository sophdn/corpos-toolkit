package qwenretrieve

import (
	"os"
	"strconv"
	"strings"
)

// QwenContextTokens is the llama-server context window the retrieve prompts must
// fit within: Qwen2.5-32B served at --ctx-size 8192 (single slot — the dev box's
// 24 GB GPU has no VRAM headroom for parallel slots, so the window is fixed; see
// the substrate memory dev-box-gpu-vram-ceiling-qwen-single-stream). Overridable
// via TOOLKIT_QWEN_CTX_TOKENS if the served context ever changes.
const QwenContextTokens = 8192

// QwenContextTokensEnvVar overrides QwenContextTokens at runtime — set it to the
// llama-server's --ctx-size if that changes without a rebuild.
const QwenContextTokensEnvVar = "TOOLKIT_QWEN_CTX_TOKENS"

// retrieveCharsPerToken is a deliberately conservative chars-per-token estimate
// for the Qwen tokenizer over retrieve prompts. Retrieve text is path/slug/tag
// heavy (kebab slugs + dates tokenize denser than prose, ~3 chars/token) mixed
// with prose summaries (~4), so an effective ~3.4 is realistic-but-slightly-high:
// it biases the estimate up so we trim BEFORE the hard llama.cpp "Context size
// has been exceeded" 500 rather than after. Over-trimming costs a little recall;
// under-trimming costs the whole call (bug 951).
const retrieveCharsPerToken = 3.4

// Pass1TokenReserve holds context back from the budget for the model's response
// (RetrieveMaxTokens) plus a margin for estimator error. The pass-1 prompt is
// trimmed to effectiveContextTokens() - Pass1TokenReserve.
const Pass1TokenReserve = RetrieveMaxTokens + 512

// effectiveContextTokens returns the runtime context window: the env override
// when set to a positive integer, else QwenContextTokens.
func effectiveContextTokens() int {
	if v := os.Getenv(QwenContextTokensEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return QwenContextTokens
}

// Pass1TokenBudget is the estimated-token ceiling for the pass-1 prompt
// (context window minus the response + safety reserve).
func Pass1TokenBudget() int {
	if b := effectiveContextTokens() - Pass1TokenReserve; b > 1 {
		return b
	}
	return 1
}

// EstimatePromptTokens estimates the token count of a composed (system, user)
// prompt pair from its character length. See retrieveCharsPerToken — there is no
// Qwen tokenizer on the Go side, so this is a conservative char-based proxy.
func EstimatePromptTokens(system, user string) int {
	return int(float64(len(system)+len(user)) / retrieveCharsPerToken)
}

// BudgetPass1Candidates returns the longest prefix of the (pre-ranked) candidate
// list whose composed pass-1 prompt is estimated to fit within tokenBudget,
// together with the number of candidates dropped to honor the budget.
//
// Candidates must be ordered best-first (the keyword prefilter already does this),
// so trimming the tail drops the least-relevant. This bounds the pass-1 prompt to
// the model's context window by TOKEN size — fixing bug 951, where the fixed
// 75-candidate count cap overflowed the 8192 window on wordy candidate sets.
//
// The fit test recomposes the real ComposeRetrieve output and binary-searches the
// largest fitting prefix (monotone: more candidates → longer prompt), so the
// estimate tracks the actual prompt shape rather than a divergent guess. When even
// a single candidate exceeds the budget (pathological — a note with a huge
// summary), the top one is kept so the dispatch still runs.
func BudgetPass1Candidates(
	task RetrieveTaskInput,
	candidates []RetrieveCandidate,
	shape CorpusShape,
	tokenBudget int,
) (kept []RetrieveCandidate, droppedForBudget int) {
	if len(candidates) == 0 {
		return candidates, 0
	}
	fits := func(k int) bool {
		system, user := ComposeRetrieve(task, RetrieveContext{
			Candidates:  candidates[:k],
			WithBody:    false,
			CorpusShape: shape,
		})
		return EstimatePromptTokens(system, user) <= tokenBudget
	}
	if fits(len(candidates)) {
		return candidates, 0
	}
	lo, hi, best := 1, len(candidates), 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if fits(mid) {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best == 0 {
		best = 1 // even one candidate overflows; keep the top so the call still runs
	}
	return candidates[:best], len(candidates) - best
}

// IsContextExceededError reports whether an inference error is the llama.cpp
// "context size exceeded" 500 — the prompt was too large for the served window.
// It is the reactive safety net: if the upfront token budget under-estimated, the
// caller halves the candidate set and retries instead of surfacing the 500.
func IsContextExceededError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "context size")
}
