package observehttp

// rubricMeta carries the verdict metadata that crates/rubric-lib's
// static registry held in Rust. Each entry mirrors one Rust file under
// archive/rubric-lib-2026-05-13/src/rubrics/. The Go rubric package
// (internal/rubric) only carries the prompt-shape fields needed by the
// classify dispatcher; the verdict metadata lives here because only
// the observe-http /benchmarks/tasks + /benchmarks/rubric-cards
// endpoints consume it.
type rubricMeta struct {
	Name               string
	Verdict            string // SmokeVerdict variant name; Rust's format!("{:?}", v) shape
	VerdictNote        string
	RetriggerCondition *string
	Deployable         bool // True iff verdict == "ExtractNowWithQwenDispatch"
}

var rubricRegistry = func() []rubricMeta {
	deployed := func(name, note string) rubricMeta {
		return rubricMeta{
			Name:        name,
			Verdict:     "ExtractNowWithQwenDispatch",
			VerdictNote: note,
			Deployable:  true,
		}
	}
	notDeployed := func(name, verdict, note string, retrigger *string) rubricMeta {
		return rubricMeta{
			Name:               name,
			Verdict:            verdict,
			VerdictNote:        note,
			RetriggerCondition: retrigger,
			Deployable:         false,
		}
	}
	refactorRetrigger := "either (a) prompt-budget allows 4 worked examples (one per named pattern), or (b) labels reframed to drop named-pattern-match"
	summarizeRetrigger := "any deployed call site uses compose_summarize WITH downstream-trim plumbing (verbose bug_list compression / large file-Read summarization / error trace condensation)"
	bugSeverityRetrigger := "dedicated smoke session: run classify_bug_severity over a ≥8-row gold corpus drawn from closed bugs in the bugs table; if accuracy ≥ 70% and honesty on unclear cases ≥ 70%, flip Verdict to ExtractNowWithQwenDispatch and wire forge(bug) post-create hook to auto-classify"
	return []rubricMeta{
		deployed("agentic-audit", "smoke 100% accuracy + 100% honesty (n=1)"),
		deployed("artifact-review", "smoke 80% accuracy + n-a honesty case passed; failures on bias-toward-mixed clause"),
		deployed("chain-assessment", "smoke 100% accuracy WITH team-context-as-input; without it, falls back to low-grounding caveat"),
		deployed("docstring-drift", "smoke 70% accuracy, 100% honesty on unclear-gold cases"),
		deployed("pre-commit-failure", "smoke 97% accuracy + 100% honesty (n=3)"),
		notDeployed(
			"pre-context-summarization",
			"DeferredWithTrigger",
			"Summarize shape (compose_summarize, not compose_classify); 99.3% term-preservation + 41.7% within-budget on 12 scenarios; production-shape but no deployed call site",
			&summarizeRetrigger,
		),
		notDeployed(
			"refactoring-heuristics",
			"RejectedRubricTooSoftForQwen",
			"smoke 60% accuracy on 10 scenarios; failures on named-pattern-match labels (only Pattern 2 in-prompt-grounded)",
			&refactorRetrigger,
		),
		deployed("retirement-signal", "smoke 90% accuracy; not-retirement is the catch-all (no Unclassifiable surface)"),
		deployed("session-routing", "smoke 80% accuracy; output-domain-disambiguation caveat for vocabulary-only role matches"),
		deployed("tiered-context", "smoke 100% accuracy with word-form labels"),
		notDeployed(
			"bug-severity",
			"DeferredWithTrigger",
			"Rubric authored + classify_bug_severity action wired; smoke deferred to a dedicated session per T86 closure note. Two-axis (observer_impact × blast_radius) matrix from skill:bug-filing-discipline; 10 examples in the TOML grounded against real filed bugs from this session's chain walk.",
			&bugSeverityRetrigger,
		),
	}
}()

func rubricLookup(name string) (rubricMeta, bool) {
	for _, r := range rubricRegistry {
		if r.Name == name {
			return r, true
		}
	}
	return rubricMeta{}, false
}
