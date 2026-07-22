#!/usr/bin/env bash
# gradient-question-guard.sh — PreToolUse gate on AskUserQuestion that DENIES
# "completeness gradient" menus: option sets that offer more-vs-less of the SAME
# requested work, or a stop-short / defer / leave-it peer.
#
# WHY THIS IS CODE AND NOT A MEMORY:
#   The anti-pattern is documented in feedback memory
#   `gradient-question-anti-pattern-completeness-as-menu`. Three materialized
#   memories failed to stop it, because a memory is passive prompt text with no
#   enforcement — it only works if the disposition that fails reads and overrides
#   itself in the moment. This hook fires at the instant of the tool call,
#   independent of whether the model "remembered." That is the difference between
#   a note and a wall.
#
# Surface: PreToolUse, matcher "AskUserQuestion".
# Deny contract (per https://code.claude.com/docs/en/hooks.md):
#   exit 0 + stdout
#   {"hookSpecificOutput":{"hookEventName":"PreToolUse",
#     "permissionDecision":"deny","permissionDecisionReason":"<reason>"}}
# Fail-open: any parse error / missing jq → exit 0 with NO decision (tool
#   proceeds). A guard that breaks legitimate tool use would just get disabled,
#   so it must never be the reason a real question can't be asked.
#
# Detection — per question, over each option's "<label> :: <description>":
#   * COMPLETENESS GRADIENT — ≥1 option matches the "fuller" lexicon AND ≥1 the
#     "lesser" lexicon (the classic do-it-fully / do-it-partially menu).
#   * STOP-SHORT PEER — any option matches the "leave it / defer / do nothing"
#     lexicon (a stop-short option normalized by listing it as a peer).
#   * GRADIENT STEM — the question itself asks HOW MUCH of the work to do.
#   Any one trips a deny. Genuine forks (different DIRECTIONS — Postgres vs
#   SQLite) match none of the lexicons and pass untouched.
#
# Every deny is logged (audit trail; no quiet bypass) to TOOLKIT_HOOK_DRIFT_LOG.

set -u

GUARD_LOG="${TOOLKIT_HOOK_DRIFT_LOG:-/tmp/toolkit-hook-drift.log}"

log() {
    printf '%s\tgradient-question-guard\t%s\n' \
        "$(date -Is 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%S+00:00')" \
        "$1" >> "$GUARD_LOG" 2>/dev/null || true
}

INPUT=$(cat)

# Fail-open if jq is unavailable.
if ! command -v jq >/dev/null 2>&1; then
    log "jq missing — fail-open"
    exit 0
fi

TOOL=$(printf '%s' "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null)
[ "$TOOL" = "AskUserQuestion" ] || exit 0

NQ=$(printf '%s' "$INPUT" | jq -r '.tool_input.questions | length' 2>/dev/null)
case "$NQ" in
    '' | *[!0-9]*) exit 0 ;; # non-numeric → malformed → fail-open
esac

# ── lexicons (case-insensitive extended regex) ──────────────────────────────
# "fuller" — an option that is MORE of the same work.
HIGH_RX='\b(full|fully|complete|completely|comprehensive|entire|whole|everything|end-to-end|full[ -]fidelity|full port|whole thing|all of it)\b'
# "lesser" — an option that is LESS of the same work.
LOW_RX='\b(minimal|minimum|partial|partially|basic|bare[ -]?bones|stub|skeleton|mvp|just the|only the|subset|scaled[ -]down|cut[ -]down|trimmed|stripped[ -]down|(quick|simple|lite|light) version)\b'
# "stop-short" — an option that defers / leaves / does nothing instead of the work.
# `defer` needs an object that is the WORK or a LATER TIME. A bare \bdefer also
# swallowed "Defer to library defaults" / "Defer to the platform team", which
# delegate a decision to a default or an owner — genuine directions, and a
# choice the user still gets to make. "Defer to later" stays a stop-short
# despite reading "defer to", hence the explicit time alternatives.
STOP_RX='(leave (it )?as[ -]is|leave as is|leave (it|them) (alone|unchanged|untouched)|do nothing|not at all|don.?t (do|build|implement|bother)|skip (it )?(for now|entirely|altogether)|\bdefer(s|red|ring)? (to (later|a later|the next|another (release|sprint|milestone|version))|this|it|that|the|them|those|until|till|for now)|\bpostpone|\bpunt\b|hide [a-z ]*for now|hold off|revisit later)'
# gradient question STEM — asks how much of the work to do.
# "how much|many" must carry the work as its object ("how much OF THE port").
# Unqualified, it caught capacity questions — "How many replicas should the
# cluster run?", "How much memory should the container get?" — which ask for a
# quantity of a RESOURCE, not an amount of the requested work. The other stems
# (how far / thorough / faithful / …) are inherently about extent, so they need
# no object. This branch is a backstop anyway: a true gradient menu also trips
# the HIGH+LOW option lexicons above.
QSTEM_RX='(how (much|many) of (the|this|that|it|these|those|your|my|our)|how (far|complete|thorough|faithful|deep|minimal|fully)|all or (just )?some|everything or|fully or|whole thing or|minimal or full|how minimal|or (just )?(a )?(minimal|partial|subset))'

trip_kind=""
trip_q=""

for ((qi = 0; qi < NQ; qi++)); do
    qtext=$(printf '%s' "$INPUT" | jq -r ".tool_input.questions[$qi].question // \"\"" 2>/dev/null)
    opts=$(printf '%s' "$INPUT" | jq -r ".tool_input.questions[$qi].options[]? | ((.label // \"\") + \" :: \" + (.description // \"\"))" 2>/dev/null)

    stem_hit=$(printf '%s' "$qtext" | grep -iEc "$QSTEM_RX" 2>/dev/null || true)
    high_hit=$(printf '%s\n' "$opts" | grep -iEc "$HIGH_RX" 2>/dev/null || true)
    low_hit=$(printf '%s\n' "$opts" | grep -iEc "$LOW_RX" 2>/dev/null || true)
    stop_hit=$(printf '%s\n' "$opts" | grep -iEc "$STOP_RX" 2>/dev/null || true)

    if [ "${high_hit:-0}" -gt 0 ] && [ "${low_hit:-0}" -gt 0 ]; then
        trip_kind="completeness gradient (one option offers more of the same work, another less)"
    elif [ "${stop_hit:-0}" -gt 0 ]; then
        trip_kind="stop-short peer (an option defers / leaves-as-is / does nothing instead of the requested work)"
    elif [ "${stem_hit:-0}" -gt 0 ]; then
        trip_kind="gradient question stem (asks HOW MUCH of the work to do)"
    fi

    if [ -n "$trip_kind" ]; then
        trip_q="$qtext"
        break
    fi
done

[ -z "$trip_kind" ] && exit 0

REASON="BLOCKED by gradient-question-guard: this AskUserQuestion is a ${trip_kind}. \
This is the anti-pattern in feedback memory 'gradient-question-anti-pattern-completeness-as-menu' — \
do NOT ask the user to choose HOW MUCH of the work they already requested to do. Two correct moves: \
(1) DEFAULT TO COMPLETENESS — do the whole thing; completeness is not a question. \
(2) If you genuinely believe the work should stop short, say so in PROSE as a yes/no with your \
recommendation and the named cost — never as a menu option, and never later recorded as the user's \
decision. Reserve AskUserQuestion for GENUINE FORKS: different DIRECTIONS you cannot choose between \
(e.g. Postgres vs SQLite), not different amounts of the same work. Offending question: \"${trip_q}\". \
Reformulate as a genuine fork, or just do the full work."

SESSION=$(printf '%s' "$INPUT" | jq -r '.session_id // "unknown"' 2>/dev/null)
log "denied kind=\"$trip_kind\" session=$SESSION question=\"$trip_q\""

jq -n --arg r "$REASON" \
    '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}'
exit 0
