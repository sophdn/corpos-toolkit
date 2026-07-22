package refresolve

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Discipline-intent cap per chain parse-context-lean-orienting T7
// acceptance criteria. The surface is noise-budgeted because
// discipline reminders have historically over-fired (bug-filing-
// discipline trigger surfacing on no-friction sessions is the
// canonical example) and erode trust when they keep showing up.
const disciplineIntentMaxPerEnvelope = 2

// disciplineRecentFireTTL is the wall-clock proxy for the "discipline
// already fired in the last ~5 turns on the same intent" suppression
// rule. 5 minutes mirrors shortFiveTurnsTTL / the PolicyShortFiveTurns
// conversational proxy from cache.go — a discipline reminder seen this
// recently is noise, not signal. The turn count itself is not tracked;
// the wall-clock TTL is the sole load-bearing proxy.
const disciplineRecentFireTTL = 5 * time.Minute

// intentDisciplineMap is the per-intent → discipline-list mapping
// from T7's acceptance criteria. Closed mapping; expanding the
// vocabulary requires a chain task that revisits the design.
//
// Docs-shape intents (explain, summarize, status, list) deliberately
// have NO entries — disciplines apply to write actions, and surfacing
// them on read-shapes is the over-firing failure mode T7's constraints
// explicitly guard against.
//
// Conditional entries (fix's bug-filing-discipline, audit's
// refactoring-discipline + vault-filing-discipline + suggestion-
// filing-discipline) gate on a further signal pattern, applied via
// entryApplies.
type intentDisciplineMapping struct {
	Discipline string
	// Conditional gates the discipline on a message-shape predicate
	// beyond the bare intent match. Nil = always applies once the
	// intent fires.
	Conditional func(message string) bool
}

var intentDisciplineMap = map[IntentShape][]intentDisciplineMapping{
	IntentVerify: {
		{Discipline: "requesting-code-review"},
		{Discipline: "code-review"},
	},
	IntentImplement: {
		// cannibalize-discipline ranked FIRST on tool-ownership prompts
		// (gated on cannibalizePattern): owning a rented Claude Code
		// harness tool is a source-derived-parity-net job, not a generic
		// implement. For non-cannibalize implements the conditional fails
		// and the coding disciplines below surface as before.
		{Discipline: "cannibalize-discipline", Conditional: cannibalizePattern.MatchString},
		{Discipline: "coding-philosophy"},
		// Language-conventions branches based on file extensions
		// observed in the message. Resolved at evaluation time so
		// the message ("…in the rust crate…") drives which one fires.
		{Discipline: "rust-conventions", Conditional: rustPattern.MatchString},
		{Discipline: "go-conventions", Conditional: goPattern.MatchString},
		{Discipline: "python-conventions", Conditional: pythonPattern.MatchString},
		{Discipline: "expo-conventions", Conditional: expoPattern.MatchString},
	},
	IntentFix: {
		// bug-fixing-discipline is the canonical end-to-end fix playbook
		// (pick up bug → root-cause → patch → regression test → stamp).
		// Ranked FIRST: "fix that bug" is overwhelmingly "fix a filed
		// bug", and the playbook is the primary skill for that.
		// Bug `parse-context-misses-literal-skill-name-when-intent-
		// pattern-matches`: pre-fix bug-fixing-discipline wasn't in this
		// map (or the manifest), so fix-intent only surfaced systematic-
		// debugging — silently demoting to the narrower diagnosis sibling.
		{Discipline: "bug-fixing-discipline"},
		// bug-filing-discipline only when the prompt names observed
		// friction the agent might file as a bug. Avoids the over-
		// firing pattern. Ordered before systematic-debugging so the
		// friction-fix case surfaces [bug-fixing, bug-filing] within the
		// cap of 2 — systematic-debugging stays reachable via bug-fixing-
		// discipline's own cross-reference in that case.
		{Discipline: "bug-filing-discipline", Conditional: frictionShapePattern.MatchString},
		// systematic-debugging: the 4-phase diagnosis methodology.
		// Coexists with bug-fixing-discipline (composition, not
		// duplication — generic debugging vs the filed-bug lifecycle).
		// Plain fix surfaces both within the cap; friction-fix yields
		// this slot to bug-filing-discipline.
		{Discipline: "systematic-debugging"},
	},
	IntentAudit: {
		// refactoring-discipline on refactor-shape prompts that carry no
		// literal trigger keyword. Gated on refactorShapePattern — the
		// SAME pattern that routes these prompts to the audit intent
		// (intent_detect.go), so a prompt classified audit-for-refactor-
		// reasons always passes this conditional, and a generic audit
		// ("audit X for unused exports") never does. Ranked first: it is
		// the primary discipline for the refactor-flavored audit.
		// Authorized by chain refactor-intent-discipline-surfacing (the
		// closed-mapping revisit §13.2 requires).
		{Discipline: "refactoring-discipline", Conditional: refactorShapePattern.MatchString},
		// vault-filing-discipline only when the prompt signals
		// cross-project insight worth filing.
		{Discipline: "vault-filing-discipline", Conditional: crossProjectPattern.MatchString},
		// suggestion-filing-discipline only on explicit improvement-
		// ideas request — the discipline's own skill body documents
		// this as the activation condition.
		{Discipline: "suggestion-filing-discipline", Conditional: improvementIdeasPattern.MatchString},
	},
	IntentExecute: {
		// scratchpad-discipline is the chain-execution reflex (chain
		// parse-context-directive-intent-extension §14.4): a session
		// driving named work to completion should maintain a scratchpad.
		// Single entry — NO language-conventions / coding-philosophy:
		// the work shape isn't inferable from an execute verb (the named
		// chain's tasks decide it), and surfacing speculative coding
		// disciplines re-introduces the over-firing failure mode this
		// map's caps exist to prevent.
		{Discipline: "scratchpad-discipline"},
	},
	// IntentExplain, IntentSummarize, IntentStatus, IntentList,
	// IntentNone — deliberately empty per T7 §"Docs-shape intents".
}

// Conditional-discipline predicates. Each pattern is a closed match
// against the message text — case-insensitive substring or word-
// boundary patterns, calibrated to avoid re-introducing the over-
// firing pattern. The compiled regexes are used directly as the
// mapping's Conditional via their MatchString method value (type
// func(string) bool); no per-pattern wrapper indirection.
var (
	rustPattern   = regexp.MustCompile(`(?i)\b(rust|cargo|clippy|rustfmt|tokio|\.rs\b)`)
	goPattern     = regexp.MustCompile(`(?i)\b(go\s+module|go-test|go\s+file|\.go\b|golang)`)
	pythonPattern = regexp.MustCompile(`(?i)\b(python|pytest|ruff|mypy|\.py\b)`)
	expoPattern   = regexp.MustCompile(`(?i)\b(expo|react native|expo-router|\.tsx\b)`)

	// cannibalizePattern gates cannibalize-discipline on tool-ownership
	// language — owning/reimplementing a rented Claude Code harness tool.
	// Calibrated to skip generic "own"/"implement" so it never surfaces on
	// ordinary implement prompts.
	cannibalizePattern = regexp.MustCompile(`(?i)(cannibaliz|own(ing)? the (harness|claude[ -]?code|read|write|edit|grep|glob|ls|bash|webfetch)\b|reimplement(ing)? the harness|owned[ -]tooling|harness[ -]swap|parity (net|floor)|deny-?list swap|fit to substrate|stop using claude[ -]?code)`)

	frictionShapePattern    = regexp.MustCompile(`(?i)\b(friction|paper-?cut|annoying|frustrating|footgun|trip(ped|s) over|keeps?\s+\w+ing)\b`)
	crossProjectPattern     = regexp.MustCompile(`(?i)\b(cross-?project|across\s+(every|all)\s+project|in\s+(every|all)\s+repo|generally\s+true|reusable\s+(insight|lesson))\b`)
	improvementIdeasPattern = regexp.MustCompile(`(?i)\b(file (this|that|these)? ?(as )?(a )?suggestions?|improvement ideas?|suggestion[\s-]?box|propose\s+(an?\s+)?improvement)\b`)
)

// disciplineOptOutPattern catches the explicit "skip the disciplines"
// language T7's don't-surface flag names. Conservative; the patterns
// only match phrasings that clearly signal "don't surface" — generic
// phrases like "skip" alone don't qualify (too much false-positive
// risk).
var disciplineOptOutPattern = regexp.MustCompile(`(?i)\b(don'?t worry about\s+(disciplines|reminders|conventions)|skip\s+(the\s+)?(disciplines|reminders|linters)|just\s+do\s+it(\s+(quickly|please))?|no\s+(discipline|reminder)\s+(reminders|surfacing))\b`)

// DisciplineSurfacingTelemetry is the per-call summary the handler
// stamps onto the ParseContextDisciplineSurfaced event. Distinct
// from the resolved ResolvedReference slice so the emit doesn't
// have to re-walk the list.
type DisciplineSurfacingTelemetry struct {
	IntentShape            string
	Surfaced               []string
	SuppressedByDedup      []string
	SuppressedByOptOut     []string
	SuppressedByRecentFire []string
}

// DisciplineFireTracker holds per-MCP-session per-(intent,
// discipline) recent-fire state for T7's "discipline already fired
// in the last 5 turns on the same intent" suppression rule. Thread-
// safe.
type DisciplineFireTracker struct {
	mu      sync.Mutex
	entries map[disciplineFireKey]time.Time
	// now is the clock source. Production wires it to time.Now via
	// NewDisciplineFireTracker; tests override it (SetClockForTest) so
	// the recent-fire TTL boundary and expiry branches are reachable
	// without sleeping real wall-clock minutes. A nil now falls back to
	// time.Now, keeping a zero-value tracker safe.
	now func() time.Time
}

type disciplineFireKey struct {
	SessionID  string
	Intent     IntentShape
	Discipline string
}

// NewDisciplineFireTracker builds an empty tracker wired to the wall clock.
func NewDisciplineFireTracker() *DisciplineFireTracker {
	return &DisciplineFireTracker{entries: make(map[disciplineFireKey]time.Time), now: time.Now}
}

// clock returns the tracker's current time, defaulting to time.Now when
// unset (zero-value tracker). Reads the now field, which is set once at
// construction and never mutated under lock.
func (t *DisciplineFireTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// recentlyFired reports whether (sessionID, intent, discipline) has
// fired within disciplineRecentFireTTL. Stale entries stay in the
// map until next markFired or cache eviction — the wall-clock guard
// is the load-bearing check.
func (t *DisciplineFireTracker) recentlyFired(sessionID string, intent IntentShape, discipline string) bool {
	if t == nil || sessionID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.entries[disciplineFireKey{sessionID, intent, discipline}]
	if !ok {
		return false
	}
	return t.clock().Sub(last) < disciplineRecentFireTTL
}

// markFired stamps a fire timestamp for (sessionID, intent, discipline).
func (t *DisciplineFireTracker) markFired(sessionID string, intent IntentShape, discipline string) {
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	t.entries[disciplineFireKey{sessionID, intent, discipline}] = t.clock()
	t.mu.Unlock()
}

// ResetSession drops a session's recent-fire state. Test-only.
func (t *DisciplineFireTracker) ResetSession(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	for k := range t.entries {
		if k.SessionID == sessionID {
			delete(t.entries, k)
		}
	}
	t.mu.Unlock()
}

// entryApplies reports whether a mapping entry fires for this message:
// an unconditional entry (nil Conditional) always applies; a conditional
// entry applies only when its predicate matches. Shared by the opt-out
// suppressed-list pass and the main surfacing loop so the two stay in
// lockstep on what "would fire".
func entryApplies(m intentDisciplineMapping, message string) bool {
	return m.Conditional == nil || m.Conditional(message)
}

// ResolveIntentDisciplines is the intent → discipline surfacing
// pass. Runs alongside (not in place of) the existing
// detectDisciplineSkill / disciplineSkillResolver pipeline. Returns
// additional ResolvedReferences capped per the noise-budget rule,
// plus telemetry partitioning surfaced / suppressed.
//
// Dedup: ResolvedReferences already in alreadySurfacedDisciplines
// (passed in from the handler, populated from any discipline_skill
// refs the existing keyword-driven pipeline produced) are dropped
// from the intent-mapping output. Keyword-match wins; intent-match
// is a heuristic that defers.
//
// Opt-out: when the message contains explicit opt-out language
// (disciplineOptOutPattern), no intent-mapped disciplines surface.
// The telemetry records the suppressed set for T10 measurement.
//
// Recent-fire: a (sessionID, intent, discipline) tuple that fired
// within the last 5 minutes (≈5 turns) is suppressed. Prevents
// repeat-firing under back-to-back work on the same intent shape.
//
// No-op when intent has no entries in the map (docs intents, none)
// or when sessionID is empty (no session = no tracking).
// RawIntentDisciplines returns the intent-mapped disciplines that APPLY to the
// message — the deterministic detect/map half: the intent→discipline mapping,
// the per-entry conditional gate (entryApplies), the message-level opt-out, and
// manifest presence. It deliberately OMITS the firing policy (the per-envelope
// cap, the already-surfaced dedup, and the recent-fire suppression). A client
// that owns the firing cadence (corpos, chain toolkit-decomposition T5) reads
// these raw candidates from the envelope's CandidateDisciplines and applies its
// own noise-budget. Opt-out stays here — it is a message-shape detect signal,
// not a firing-cadence one. Returns nil when the intent maps to nothing or the
// message opts out.
func RawIntentDisciplines(manifest *SkillManifest, intent IntentShape, message string) []ResolvedReference {
	mapping := intentDisciplineMap[intent]
	if len(mapping) == 0 {
		return nil
	}
	if disciplineOptOutPattern.MatchString(message) {
		return nil
	}
	var out []ResolvedReference
	for _, m := range mapping {
		if !entryApplies(m, message) {
			continue
		}
		ref, ok := intentDisciplineRef(manifest, m.Discipline, intent)
		if !ok {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func ResolveIntentDisciplines(
	_ context.Context,
	manifest *SkillManifest,
	intent IntentShape,
	message string,
	sessionID string,
	alreadySurfacedDisciplines map[string]bool,
	tracker *DisciplineFireTracker,
) ([]ResolvedReference, DisciplineSurfacingTelemetry) {
	mapping := intentDisciplineMap[intent]
	if len(mapping) == 0 {
		return nil, DisciplineSurfacingTelemetry{}
	}
	tel := DisciplineSurfacingTelemetry{IntentShape: string(intent)}

	if disciplineOptOutPattern.MatchString(message) {
		// Opt-out: every mapping entry that would have fired is
		// listed as suppressed-by-optout for T10 measurement, then
		// the function returns no surfacings.
		for _, m := range mapping {
			if !entryApplies(m, message) {
				continue
			}
			tel.SuppressedByOptOut = append(tel.SuppressedByOptOut, m.Discipline)
		}
		return nil, tel
	}

	out := []ResolvedReference{}
	for _, m := range mapping {
		if len(out) >= disciplineIntentMaxPerEnvelope {
			break
		}
		if !entryApplies(m, message) {
			continue
		}
		if alreadySurfacedDisciplines[m.Discipline] {
			tel.SuppressedByDedup = append(tel.SuppressedByDedup, m.Discipline)
			continue
		}
		if tracker.recentlyFired(sessionID, intent, m.Discipline) {
			tel.SuppressedByRecentFire = append(tel.SuppressedByRecentFire, m.Discipline)
			continue
		}
		ref, ok := intentDisciplineRef(manifest, m.Discipline, intent)
		if !ok {
			// Discipline named in the map but not in the manifest —
			// degraded boot, missing install, etc. Silently skip;
			// this is a deployment concern, not an envelope concern.
			continue
		}
		out = append(out, ref)
		tel.Surfaced = append(tel.Surfaced, m.Discipline)
		tracker.markFired(sessionID, intent, m.Discipline)
	}
	return out, tel
}

// intentDisciplineRef composes a discipline_skill-shaped
// ResolvedReference for the named discipline. Returns ok=false when
// the manifest doesn't carry the discipline (caller skips).
func intentDisciplineRef(manifest *SkillManifest, discipline string, intent IntentShape) (ResolvedReference, bool) {
	if manifest == nil {
		return ResolvedReference{}, false
	}
	var entry *SkillManifestEntry
	for i := range manifest.Skills {
		if manifest.Skills[i].Name == discipline {
			entry = &manifest.Skills[i]
			break
		}
	}
	if entry == nil {
		return ResolvedReference{}, false
	}
	return ResolvedReference{
		Token:             discipline,
		Shape:             ShapeDisciplineSkill,
		ConfidenceTier:    TierSingleExact,
		PresentedAs:       fmt.Sprintf("[intent-mapped discipline for %s intent] `%s` — body at %s", intent, discipline, entry.BodyPath),
		RecommendedAction: PresentUseDirectly,
		CachePolicy:       string(PolicyReEvaluatePerCall),
		TopCandidates: []Candidate{{
			ID:         discipline,
			Title:      "discipline " + discipline,
			Score:      1.0,
			SourceRef:  "skill:" + entry.BodyPath,
			DebugNotes: fmt.Sprintf("triggered_by=intent:%s source=discipline-intent-map", intent),
		}},
	}, true
}
