package main

import (
	"strings"
	"testing"
)

func twoTasks() []taskContent {
	return []taskContent{
		{slug: "t1", position: 1, problemStatement: "do one", acceptanceCriteria: "a\n- b", contextRequired: "ctx1", constraints: "c1"},
		{slug: "t2", position: 2, problemStatement: "do two", acceptanceCriteria: "", contextRequired: "", constraints: ""},
	}
}

func TestCompare_Identical_NoDiffs(t *testing.T) {
	c := chainContent{output: "out", completionCondition: "done when x"}
	if diffs := compare(c, twoTasks(), c, twoTasks()); len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %v", diffs)
	}
}

func TestCompare_ChainOutputDrift_NamesColumn(t *testing.T) {
	prod := chainContent{output: "out", completionCondition: "cc"}
	replay := chainContent{output: "DIFFERENT", completionCondition: "cc"}
	diffs := compare(prod, twoTasks(), replay, twoTasks())
	if len(diffs) != 1 || !strings.HasPrefix(diffs[0], "chain.output:") {
		t.Fatalf("expected one chain.output diff, got %v", diffs)
	}
}

func TestCompare_TaskFieldDrift_NamesSlugAndColumn(t *testing.T) {
	prod := twoTasks()
	replay := twoTasks()
	replay[0].problemStatement = "do something else"
	diffs := compare(chainContent{}, prod, chainContent{}, replay)
	if len(diffs) != 1 {
		t.Fatalf("expected one diff, got %v", diffs)
	}
	if !strings.Contains(diffs[0], `task[0] "t1".problem_statement`) {
		t.Errorf("diff should name the task index, slug, and column: %q", diffs[0])
	}
}

func TestCompare_TaskCountMismatch_ShortCircuits(t *testing.T) {
	prod := twoTasks()
	replay := twoTasks()[:1]
	diffs := compare(chainContent{}, prod, chainContent{}, replay)
	if len(diffs) != 1 || !strings.HasPrefix(diffs[0], "task count:") {
		t.Fatalf("expected a single task-count diff, got %v", diffs)
	}
}

func TestCompare_AcceptanceCriteriaDrift(t *testing.T) {
	prod := twoTasks()
	replay := twoTasks()
	replay[0].acceptanceCriteria = "a\n- b\n- c"
	diffs := compare(chainContent{}, prod, chainContent{}, replay)
	if len(diffs) != 1 || !strings.Contains(diffs[0], ".acceptance_criteria") {
		t.Fatalf("expected one acceptance_criteria diff, got %v", diffs)
	}
}

// TestAcceptanceCriteriaRoundTripIdentity is the load-bearing assumption
// behind the replay: a stored acceptance_criteria value, split on the
// projection's "\n- " separator and re-joined by the same separator,
// reproduces the original bytes — even when an individual criterion
// itself contains the separator (Join∘Split is an identity on the joined
// string for any separator).
func TestAcceptanceCriteriaRoundTripIdentity(t *testing.T) {
	cases := []string{
		"",
		"single criterion",
		"first\n- second\n- third",
		"first\n- multi-line item\nwith an embedded newline\n- third",
		"- leading dash that is part of the text\n- next",
	}
	for _, s := range cases {
		got := strings.Join(strings.Split(s, acSeparator), acSeparator)
		if got != s {
			t.Errorf("round-trip drift:\n in:  %q\n out: %q", s, got)
		}
	}
}

func TestTruncQuote_CapsLongValues(t *testing.T) {
	short := truncQuote("abc")
	if short != `"abc"` {
		t.Errorf("short value should quote verbatim: %q", short)
	}
	long := truncQuote(strings.Repeat("x", 500))
	if !strings.Contains(long, "+340 bytes") {
		t.Errorf("long value should report overflow byte count: %q", long)
	}
}
