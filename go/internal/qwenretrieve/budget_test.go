package qwenretrieve

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

// wordyCandidate builds a pass-1 candidate sized to ~`approxChars` characters
// across its path/title/tags/summary (the fields the pass-1 prompt includes).
func wordyCandidate(i, approxChars int) RetrieveCandidate {
	filler := strings.Repeat("alpha-beta-gamma ", approxChars/17+1)
	return RetrieveCandidate{
		Path:    fmt.Sprintf("decisions/2026-05-26_long-architecture-decision-note-number-%03d.md", i),
		Title:   strptr(fmt.Sprintf("Decision %03d — %s", i, filler[:approxChars/3])),
		Tags:    []string{"architecture", "decision", "harness-swap", "substrate", fmt.Sprintf("topic-%d", i)},
		Summary: strptr(filler[:approxChars/2]),
	}
}

func task() RetrieveTaskInput {
	return RetrieveTaskInput{Query: "rented vs owned harness affordances", TopK: 5}
}

// Regression: a 75-candidate wordy set composes a pass-1 prompt that overflows
// the 8192 window (the bug-951 condition) — and BudgetPass1Candidates trims it
// to a fitting prefix.
func TestBudgetPass1Candidates_TrimsOverflowingSet(t *testing.T) {
	const n = 75
	cands := make([]RetrieveCandidate, n)
	for i := range cands {
		cands[i] = wordyCandidate(i, 600) // ~600 chars each → ~45k chars → way over 8192 tokens
	}

	// Reproduce the overflow: the full set's composed prompt estimate exceeds the window.
	sys, usr := ComposeRetrieve(task(), RetrieveContext{Candidates: cands, WithBody: false, CorpusShape: CorpusShapeVault})
	if got := EstimatePromptTokens(sys, usr); got <= QwenContextTokens {
		t.Fatalf("test setup: expected the 75-candidate prompt to overflow %d tokens, got estimate %d", QwenContextTokens, got)
	}

	budget := Pass1TokenBudget()
	kept, dropped := BudgetPass1Candidates(task(), cands, CorpusShapeVault, budget)

	if len(kept) >= n {
		t.Fatalf("expected trimming below %d candidates, kept %d", n, len(kept))
	}
	if dropped != n-len(kept) {
		t.Fatalf("dropped count %d != n-kept %d", dropped, n-len(kept))
	}
	// The kept prefix must actually fit the budget.
	ks, ku := ComposeRetrieve(task(), RetrieveContext{Candidates: kept, WithBody: false, CorpusShape: CorpusShapeVault})
	if est := EstimatePromptTokens(ks, ku); est > budget {
		t.Fatalf("kept prefix still over budget: estimate %d > budget %d", est, budget)
	}
	// And adding one more would have exceeded it (binary search found the boundary).
	if len(kept) < n {
		os1, ou1 := ComposeRetrieve(task(), RetrieveContext{Candidates: cands[:len(kept)+1], WithBody: false, CorpusShape: CorpusShapeVault})
		if est := EstimatePromptTokens(os1, ou1); est <= budget {
			t.Fatalf("kept prefix is not maximal: len(kept)+1 also fits (estimate %d <= budget %d)", est, budget)
		}
	}
}

func TestBudgetPass1Candidates_KeepsAllWhenUnderBudget(t *testing.T) {
	cands := []RetrieveCandidate{
		{Path: "decisions/a.md", Title: strptr("A short note"), Tags: []string{"x"}},
		{Path: "learnings/b.md", Title: strptr("Another short note"), Tags: []string{"y"}},
		{Path: "reference/c.md", Title: strptr("Third short note"), Tags: []string{"z"}},
	}
	kept, dropped := BudgetPass1Candidates(task(), cands, CorpusShapeVault, Pass1TokenBudget())
	if len(kept) != len(cands) || dropped != 0 {
		t.Fatalf("expected all %d kept / 0 dropped, got kept=%d dropped=%d", len(cands), len(kept), dropped)
	}
}

func TestBudgetPass1Candidates_PathologicalSingleHugeKeepsOne(t *testing.T) {
	cands := []RetrieveCandidate{
		wordyCandidate(0, 60000), // one note bigger than the whole budget
		wordyCandidate(1, 300),
	}
	kept, dropped := BudgetPass1Candidates(task(), cands, CorpusShapeVault, Pass1TokenBudget())
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("expected exactly the top candidate kept (1/1 dropped), got kept=%d dropped=%d", len(kept), dropped)
	}
}

func TestBudgetPass1Candidates_Empty(t *testing.T) {
	kept, dropped := BudgetPass1Candidates(task(), nil, CorpusShapeVault, Pass1TokenBudget())
	if len(kept) != 0 || dropped != 0 {
		t.Fatalf("empty input should yield empty/0, got kept=%d dropped=%d", len(kept), dropped)
	}
}

func TestPass1TokenBudget_BelowContext(t *testing.T) {
	if Pass1TokenBudget() >= QwenContextTokens {
		t.Fatalf("budget %d must reserve headroom below the context window %d", Pass1TokenBudget(), QwenContextTokens)
	}
}

func TestEstimatePromptTokens_GrowsWithLength(t *testing.T) {
	small := EstimatePromptTokens("sys", "hi")
	big := EstimatePromptTokens("sys", strings.Repeat("token ", 1000))
	if big <= small {
		t.Fatalf("estimate should grow with length: small=%d big=%d", small, big)
	}
}

func TestIsContextExceededError(t *testing.T) {
	real := errors.New(`dispatch retrieve pass-1: router generate: llamacpp complete: HTTP 500: {"error":{"code":500,"message":"Context size has been exceeded.","type":"server_error"}}`)
	if !IsContextExceededError(real) {
		t.Error("expected the real llama.cpp context-exceeded error to be detected")
	}
	if IsContextExceededError(errors.New("empty choices in response")) {
		t.Error("an empty-response transient is not a context-exceeded error")
	}
	if IsContextExceededError(nil) {
		t.Error("nil is not a context-exceeded error")
	}
}
