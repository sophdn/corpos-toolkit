package construct

import (
	"regexp"
	"strings"
)

// DeferralCaptureNudge is the soft hint surfaced on a task forge-create when
// the task DEFERS to an external recommendation/decision (e.g. "decide X vs Y
// per the design_decisions recommendation") yet carries no context_required.
//
// Bug decide-per-recommendation-task-strands-the-recommendation-in-transcript:
// a chain task read "Decide audit-mapping vs new IntentRefactor shape per the
// design_decisions recommendation, then implement…" while every structured
// field (acceptance_criteria, context_required, constraints, handoff) was
// empty — so the recommendation (+ its rationale, rejected alternative, and
// calibration constraints) lived ONLY in a prior session's JSONL transcript.
// A cold-pickup agent had to grep two large transcripts to recover it, or
// risk re-deriving and diverging from the author's intent.
//
// Exported so tests (and a future doc-string check) can assert the exact text.
// Non-blocking by design — the create always succeeds (the bug's own
// constraint: "Don't over-engineer into a mandatory schema field"); this only
// reminds the forging agent to land the decision context where the task points.
const DeferralCaptureNudge = "This task defers to an external recommendation/decision but its context_required is empty — the recommendation (rationale, rejected alternatives, calibration constraints) risks living only in the session transcript. Capture it in context_required, or link a vault decision note, so a cold-pickup agent doesn't have to reconstruct it."

// deferralPattern matches task text that DEFERS the decision elsewhere or names
// an unresolved choice, rather than carrying the decision itself:
//   - "… per the <X> recommendation"  — explicit deferral to an external rec
//   - "decide … vs …" / "decide between …" — an open choice the task must resolve
//
// Case-insensitive; the inter-token spans are lazily bounded so the match stays
// local to the deferral phrasing and doesn't sprawl across a long statement.
var deferralPattern = regexp.MustCompile(`(?i)(per\s+the\s+[\w\s-]{0,40}?recommendation|\bdecide\b[^.?!]{0,60}?\bvs\b|\bdecide\s+between\b)`)

// TaskDefersWithoutCapturedContext reports whether a task's problem_statement
// defers to an external recommendation/decision (deferralPattern) while
// context_required is empty (whitespace-only counts as empty). When the author
// already populated context_required they captured the decision context, so no
// nudge fires — this is the calibration that keeps well-specified "decide X vs
// Y" tasks (which legitimately carry their full spec) from over-firing.
func TaskDefersWithoutCapturedContext(problemStatement, contextRequired string) bool {
	if strings.TrimSpace(contextRequired) != "" {
		return false
	}
	return deferralPattern.MatchString(problemStatement)
}

// deferringChainTaskSlugs returns the slugs of full-object chain-task entries
// that defer to a recommendation/decision without captured context. Pipe-shape
// entries are skipped — they carry no problem_statement/context_required to
// inspect (only slug + scope). This is the forge(chain) counterpart to the
// standalone TaskDefersWithoutCapturedContext check.
func deferringChainTaskSlugs(entries []ChainTaskEntry) []string {
	var out []string
	for _, e := range entries {
		if e.Mode == ChainTaskModeFull && TaskDefersWithoutCapturedContext(e.ProblemStatement, e.ContextRequired) {
			out = append(out, e.Slug)
		}
	}
	return out
}

// chainDeferralNudge prefixes DeferralCaptureNudge with the offending task
// slug(s) so a forge(chain) response points the author at the specific task to
// fix (the chain create response is a single envelope for the whole batch).
func chainDeferralNudge(slugs []string) string {
	return "Chain task(s) " + strings.Join(slugs, ", ") + ": " + DeferralCaptureNudge
}

// joinHints combines two soft hints into one Hint string, dropping empties.
// Several independent nudges (burst, deferral) can co-occur on one response.
func joinHints(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + " " + b
	}
}
