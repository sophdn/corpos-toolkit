package arcreview

import (
	"fmt"
	"strings"
)

// ComposeArcSummaryPrompt builds the (system, user) prompt pair for
// the Qwen arc-summary pre-call (per design Q2). The pre-call runs
// before the main review and produces a one-paragraph summary that
// frames the review prompt's {arc_summary} slot.
//
// Keep the system prompt short — the arc-summary call's job is purely
// to compress; the prescriptive filing language lives in the main
// review prompt.
func ComposeArcSummaryPrompt(snap Snapshot) (system, user string) {
	system = arcSummarySystem
	user = renderSnapshot(snap) + "\n\nTASK: Summarize the activity in this " +
		"conversation snapshot in ONE paragraph. Name what the agent was " +
		"working on, what was accomplished, and what surprises or " +
		"workarounds (if any) occurred. Output the paragraph and nothing else."
	return system, user
}

// ComposeReviewPrompt builds the (system, user) prompt pair for the
// main Qwen review call (per design §Review-prompt). The system prompt
// is the prescriptive review language (be ACTIVE, signal taxonomy,
// do-not-file anti-patterns, preference order — content derived from
// T1's rewritten skill bodies at
// ~/.claude/skills/{vault-filing-discipline,bug-filing-discipline}/SKILL.md).
//
// The user prompt carries the per-fire content: snapshot, arc summary,
// trigger signals, and the in-arc already-filed list (bug 1472's
// dedupe enrichment). The system half is template; the user half is
// session-specific.
//
// triggers comes from the detector's trigger payload (T3's
// hooks/arc-close-detector.sh emits the trigger slug list); the
// substrate-side listener uses the event-name slugs. Empty list is
// acceptable — the review can fire purely on the snapshot.
//
// recentFilings is the list of artifacts filed earlier in the same arc
// (queried by handler.go via recentFilingsInArc). Empty list omits the
// dedupe block entirely so the prompt stays compact for clean-arc
// reviews. When non-empty, the block instructs Qwen to skip
// re-proposing forge_bug for these slugs/titles — closes bug 1472.
func ComposeReviewPrompt(snap Snapshot, arcSummary string, triggers []string, recentFilings []RecentFiling) (system, user string) {
	system = reviewSystemPrompt()
	var b strings.Builder
	b.WriteString("CONVERSATION SNAPSHOT:\n")
	b.WriteString(renderSnapshot(snap))
	b.WriteString("\n\nARC SUMMARY:\n")
	if strings.TrimSpace(arcSummary) == "" {
		b.WriteString("(no arc summary; rely on snapshot)")
	} else {
		b.WriteString(strings.TrimSpace(arcSummary))
	}
	b.WriteString("\n\nTRIGGER SIGNAL(S):\n")
	if len(triggers) == 0 {
		b.WriteString("(none — fire issued without trigger metadata)")
	} else {
		b.WriteString(strings.Join(triggers, ", "))
	}
	if len(recentFilings) > 0 {
		b.WriteString("\n\nALREADY FILED IN THIS ARC (do NOT re-propose these — they were filed earlier in the conversation):")
		for _, r := range recentFilings {
			b.WriteString(fmt.Sprintf("\n  [%s] %s", r.Kind, r.Slug))
			if r.Title != "" {
				b.WriteString(": ")
				b.WriteString(r.Title)
			}
		}
	}
	b.WriteString("\n\nTASK: Output a `filing_decisions` JSON array per the schema named in the system prompt. " +
		"Each decision either FILES something or returns `nothing_to_file` with clear reasoning. " +
		"Be specific. Confidence ≥ 0.85 = auto-execute; 0.50-0.85 = surface for confirm; <0.50 = skip. " +
		"Output ONLY the JSON object — no preface, no trailing commentary.")
	return system, b.String()
}

// renderSnapshot joins the snapshot's messages into a single string in
// "ROLE: content\n\n" form. Plain readability beats fancy markdown
// here; Qwen handles either but the simpler shape is easier to test
// against. Truncation header is included so the review prompt knows
// when the snapshot was capped.
func renderSnapshot(snap Snapshot) string {
	var b strings.Builder
	if snap.Truncated {
		fmt.Fprintf(&b, "[snapshot truncated to %d turns / ~%d tokens; earlier turns dropped]\n\n",
			len(snap.Messages), snap.EstimatedTokens)
	}
	for _, m := range snap.Messages {
		fmt.Fprintf(&b, "%s: %s\n\n", strings.ToUpper(m.Role), m.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// reviewSystemPrompt assembles the main review system prompt with the
// verbatim friction-vs-suggestion definition spliced in. The definition
// is read from ~/.claude/skills/suggestion-filing-discipline/SKILL.md
// (cached after first call); a missing skill file degrades to the
// embedded fallback so the prompt still expresses the distinction.
//
// Building the prompt every call (rather than caching the assembled
// string) keeps the cost-of-test in check: each compose_test fire can
// substitute a different skill file via TOOLKIT_SUGGESTION_SKILL_ROOT
// without sync.Once pinning a stale value. The definition itself is
// cached via FrictionVsSuggestionDefinition's sync.Once — only the
// string concatenation runs per call.
func reviewSystemPrompt() string {
	return reviewSystemBase + frictionVsSuggestionBlock() + reviewSystemOutputSchema
}

// frictionVsSuggestionBlock renders the loaded definition into the
// shaped prose slot the system prompt expects. Kept as a separate
// function so tests can assert on the rendered shape directly without
// having to scrape it out of the assembled prompt.
func frictionVsSuggestionBlock() string {
	return `

FRICTION vs SUGGESTION (the verbatim rule shared with the
suggestion-filing-discipline skill — agent and Qwen apply the SAME
distinction so the two filing surfaces stay coherent):

  ` + FrictionVsSuggestionDefinition() + `

Apply the rule per observation:
  - Workaround silently applied / spec underspecified / inter-task
    seam orphan / tool surprise → forge_bug.
  - "Current X works but Y would be cleaner" / "we could add a missing
    test for Z" / "this prose has drifted, worth tidying" → forge_suggestion.

forge_suggestion uses NATIVE vocabulary distinct from forge_bug:
priority (low/medium/high) — NOT severity. Resolution vocabulary
(adopted/deferred/rejected) is not part of the filing payload; that's
only set later via suggestion_resolve.`
}

// arcSummarySystem is the system prompt for the arc-summary pre-call.
// Short on purpose — the prescriptive filing language is for the
// main review prompt only.
const arcSummarySystem = `You summarize a recent activity arc from a software-engineering ` +
	`agent session. Output ONE paragraph: what was being worked on, ` +
	`what was accomplished, and any surprises, workarounds, or ` +
	`unresolved threads. No preface, no closing remarks, no headings.`

// reviewSystem is the main review system prompt. Content aligned with
// T1's rewritten skill bodies at ~/.claude/skills/{vault-filing-discipline,
// bug-filing-discipline}/SKILL.md. The structured-output schema
// description matches schema.go's ActionKind / payload structs
// exactly — Qwen sees the same field names ValidateDecision parses.
//
// Critical framing per design §Review-prompt: "Be ACTIVE" / "nothing
// to file is valid but should NOT be default" / "missed learning
// opportunity, not a neutral outcome". This is the language the
// substrate fires INSTEAD of the agent reading the skill body in-
// flight — the structural firing path that replaces the
// agent-internalized compulsion to file.
const reviewSystemBase = `ROLE: You are reviewing a recent activity arc from a software-engineering ` +
	`agent session for filing-worthy content. Most non-trivial arcs ` +
	`surface at least one filing-worthy bug, vault note, suggestion, ` +
	`skill update, or memory write.

Be ACTIVE. "Nothing to file" is a valid outcome but it should NOT be ` +
	`the default — a pass that does nothing is a missed learning ` +
	`opportunity, not a neutral outcome. If the arc ran cleanly with no ` +
	`workarounds, no surprises, no decisions named, no patterns ` +
	`re-derived — fine, return a single nothing_to_file decision with ` +
	`reasoning. Otherwise, act.

SIGNAL TAXONOMY (any one warrants a filing decision):

  Vault-worthy:
    • DECISION MADE + RATIONALE. A design choice plus the constraint
      that drove it; future agents need both to tell if the decision
      still applies.
    • LESSON RE-DERIVED. A pattern, gotcha, or failure mode noticed
      twice — or once but recognized as recurring.
    • REFERENCE ASSEMBLED. Durable how-to material gathered while
      solving the immediate task (command recipes, conventions,
      kiwix queries future-you will look up again).
    • CROSS-PROJECT FRAMING. A way of thinking that reads cleanly
      without naming this project (e.g. "the trade-off is...",
      "two distinct axes...", "the pattern is...").

  Bug-worthy:
    • WORKAROUND APPLIED SILENTLY. Fixed in-task without naming the
      friction.
    • TOOL SURPRISE. Handler did the wrong thing or the right thing
      in an unergonomic way; took more calls than it should have.
    • SPEC UNDERSPECIFIED. Derived scope that should have been
      prescribed (transitive deps, internal modules, build context).
    • INTER-TASK SEAM. Work that fell between two tasks that both
      excluded it ("followed orders" + "next task didn't pick it
      up" = orphan).
    • DOCUMENTATION DRIFT. What docs say doesn't match what the
      code does.

  Skill-worthy:
    • User corrected style / format / workflow.
    • New technique that future sessions would benefit from.
    • Existing skill turned out wrong or missing.

  Memory-worthy:
    • User revealed preferences or expectations.
    • Durable user-behavior fact that future sessions should know.

  Suggestion-worthy (forward-looking proposals — see FRICTION vs
  SUGGESTION block below for the boundary against Bug-worthy):
    • PROSE DRIFT. Stale skill body, out-of-date doc fragment,
      comment that no longer matches the code, description that has
      outgrown its meaning.
    • MISSING TEST. A code path that ships untested, a regression
      risk worth pinning, an edge case the test set glosses past.
    • SHARED COMPONENT OPPORTUNITY. Parallel structure across two
      modules / pages / handlers worth extracting now that the
      shape is concrete.
    • REDUNDANT CONTENT. Code or prose that has outlived its
      purpose — dead code, stale TODO, leftover scaffolding,
      duplicate enum that should collapse, unused export.
    • CONVENTION DRIFT. One outlier in an otherwise-consistent set
      (naming, file structure, error-handling shape, ordering).
    • ERGONOMIC NIT. A workflow micro-friction that isn't a bug
      (no observed friction event) but would smooth future sessions
      if cleaned up.

DO NOT FILE (anti-patterns):
  • Environment-dependent failures (missing binaries, network blips).
  • Negative claims about tools ("X is broken") without a recurrence
    shape.
  • Session-specific transient errors that resolved by retry.
  • One-off task narratives ("I analyzed today's PR"). Not a class
    of cross-project insight.
  • Project-state facts queryable from the toolkit DB (bug X is
    open, chain Y is at task 3). Pointer-only entries add nothing.
  • PLATFORM-CONSTRAINT REMINDERS. Workflow rules enforced by the
    agent's tooling itself (e.g. "must Read before Edit", "git
    commit needs hook to pass", "Bash quote spaces in paths") are
    not novel learnings — they're constraints the platform already
    enforces. The constraint surfacing in the arc means the agent
    ran into the guard; that's the system working, not a vault-
    worthy pattern.
  • CITATION-AS-WORK CONFUSION. The snapshot may MENTION a prior
    bug slug, vault note, chain, or task as REFERENCE MATERIAL
    ("per bug X", "as documented in [[Y]]", "see chain Z's T4
    closure"). A citation is not work. Only propose forges /
    skill_updates / memory_writes for artifacts the session
    ACTUALLY CREATED, MODIFIED, or DISCOVERED NOVEL FRICTION
    ABOUT. If the only mention of an artifact is in a "per X" /
    "see Y" / "as captured in Z" citation, that artifact is OUT
    OF SCOPE for this review — do not re-file it, do not propose
    amendments to it on the basis of citation alone.

CONTENT-SHAPE ANTI-PATTERNS (per docs/CONTENT_ROUTING_TAXONOMY.md
§3, the routing decision table). These shapes consistently
produce noise filings; the canonical home is NOT the vault:

  • DIARY-STYLE BODY OPENER. forge_vault_note bodies whose
    opening sentence is "This note captures...", "Documenting...",
    "This {process,decision,implementation,note} documents...",
    "The process for...", "During the {session,implementation,
    process}...". Diary framing — the note IS the work narrated
    in past tense, not the cross-project lesson. → nothing_to_file.
  • OUTCOME PARAPHRASE. Bodies containing past-tense outcome
    language: "was tested showing N%", "successfully implemented",
    "improvement over baseline", "were added/committed/landed",
    "the process ensures X". Narrates the commit; the commit
    message is the durable record. → nothing_to_file.
  • COMMIT-SPECIFIC NARRATIVE. Body describes what changed in a
    specific commit (X was added, Y was fixed, the function
    signature changed to Z, the script was extended to support W).
    The commit message owns this. → nothing_to_file.
  • PROCEDURAL HOW-TO IN VAULT SHAPE. "Always do X", "When Y
    happens, do Z", "Step 1... Step 2..." content with no
    cross-project framing belongs in a SKILL body, not the
    vault. → skill_update against the matching skill if one
    exists (vault-filing-discipline, knowledge-pull-discipline,
    coding-philosophy, etc.); → nothing_to_file if no skill
    fits and the content is too narrow for a new skill.
  • OPERATOR-ERROR WORKAROUND. "Binary not found", "forgot to
    run", "had to be rebuilt before X could work" — operator
    error workarounds are not patterns; the operator already
    knows. → nothing_to_file.
  • UNDER-400-WORD BODY WITH NO NAMED CROSS-PROJECT PATTERN.
    Short forge_vault_note bodies that don't name a generalisable
    pattern, a cross-applicable framework, or a recurrence-by-
    shape lesson. If the body is "<400 words AND has no
    'cross-project' / 'every project' / 'future agents' / 'the
    pattern is' / 'two distinct axes' / named-framework
    framing", → nothing_to_file (or skill_update if procedural).

PREFERENCE ORDER (pick the earliest that fits):
  1. AMEND or SUPERSEDE an existing artifact when there's ≥80%
     overlap. (The dispatcher checks vault/bug rolls before
     executing; you propose, it routes.)
  2. EXTEND an existing umbrella — add to a related note or open
     bug rather than parallel-file.
  3. FILE NEW.

DEDUPE AGAINST IN-ARC FILINGS: the user prompt may include an
"ALREADY FILED IN THIS ARC" block listing artifacts the user or
agent filed earlier in the same conversation — both bugs (kind=bug)
and vault notes (kind=vault_note). If a proposed forge_bug OR
forge_vault_note duplicates a slug or title from that list, OR
substantially restates content the listed entry already covers,
return nothing_to_file for that slot with reasoning naming the
existing slug — do NOT re-file. ALSO scan the snapshot itself for
agent-issued forge calls and Write/Edit operations against vault
paths within the conversation; even when the "ALREADY FILED" block
doesn't enumerate them (snapshot-extraction is best-effort), a
visible "[tool_use: ...] forge(vault-note, ...)" or "[tool_use:
Write] {...vault/...}" entry IS evidence the agent already filed
that artifact in the arc and you should not re-propose it. The
dispatcher cannot retract a forge once fired; in-arc dupes are
precision misses you can prevent here.`

// reviewSystemOutputSchema appends the OUTPUT SCHEMA half of the system
// prompt. Split from reviewSystemBase so reviewSystemPrompt() can splice
// the runtime-loaded friction-vs-suggestion block between them without
// scanning a giant single const.
const reviewSystemOutputSchema = `

OUTPUT SCHEMA — return EXACTLY this JSON object, no preface, no
trailing commentary:

{
  "filing_decisions": [
    {
      "action": "forge_bug" | "forge_vault_note" | "forge_suggestion" |
                "skill_update" | "memory_write" | "nothing_to_file",
      "payload": <action-specific shape; null for nothing_to_file>,
      "confidence": <number in [0, 1]>,
      "reasoning": "<one-sentence why-this-decision>"
    }
  ],
  "summary": "<one-paragraph human-readable arc summary>"
}

Per-action payload shapes:

  forge_bug:
    { "title": "...", "problem_statement": "...",
      "surface": "comma,kebab,tags", "severity": "low|medium|high",
      "tags": "comma,kebab,tags" }

  forge_vault_note:
    { "note_kind": "decision|learning|reference",
      "title": "...", "body": "<markdown>", "tags": "..." }

  forge_suggestion:
    { "title": "...", "problem_statement": "...",
      "surface": "comma,kebab,tags", "priority": "low|medium|high",
      "source": "<session retro on YYYY-MM-DD, or originating session/arc id>",
      "acceptance_criteria": "...", "constraints": "...",
      "tags": "comma,kebab,tags (testing|lint|docs|tooling|prose|architecture|skill|workflow)" }

  skill_update:
    { "skill_slug": "...", "patch_kind":
      "add_section|extend_paragraph|add_trigger", "content": "..." }

  memory_write:
    { "memory_kind": "user|feedback|project|reference",
      "name": "kebab-slug", "description": "one-line", "body": "..." }

  nothing_to_file:
    payload: null

Output ONLY the JSON object. No code fences, no preface.`
