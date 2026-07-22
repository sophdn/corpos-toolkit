#!/usr/bin/env bash
# test-gradient-question-guard.sh — regression suite for the PreToolUse guard
# that blocks completeness-gradient AskUserQuestion menus.
#
# Two halves that BOTH matter:
#   DENY cases  — the real anti-pattern (more-vs-less of the same work, or a
#                 stop-short peer) must be blocked.
#   ALLOW cases — genuine forks (different directions) and non-target tools must
#                 pass untouched. A guard that blocks legitimate questions just
#                 gets disabled, so the false-positive half is load-bearing.
#
# Run:  bash hooks/test-gradient-question-guard.sh

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$SCRIPT_DIR/gradient-question-guard.sh"

if [ ! -x "$HOOK" ]; then
    echo "FAIL: hook not executable: $HOOK"
    exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "SKIP: jq not available"
    exit 0
fi

PASS=0
FAIL=0
TMP_BASE=$(mktemp -d)
trap 'rm -rf "$TMP_BASE"' EXIT

run_hook() {
    printf '%s' "$1" | TOOLKIT_HOOK_DRIFT_LOG="$TMP_BASE/guard.log" "$HOOK" 2>/dev/null
}

decision_of() {
    # Empty / whitespace-only stdout = the hook emitted no decision (tool proceeds).
    if [ -z "$(printf '%s' "$1" | tr -d '[:space:]')" ]; then
        echo "none"
        return
    fi
    printf '%s' "$1" | jq -r '.hookSpecificOutput.permissionDecision // "none"' 2>/dev/null || echo "none"
}

# assert_decision <name> <input-json> <expected: deny|none>
assert_decision() {
    local name="$1" input="$2" expected="$3"
    local out dec
    out=$(run_hook "$input")
    dec=$(decision_of "$out")
    if [ "$dec" = "$expected" ]; then
        echo "PASS $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL $name (expected decision '$expected', got '$dec')"
        FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local name="$1" needle="$2" haystack="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "PASS $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL $name (missing '$needle')"
        FAIL=$((FAIL + 1))
    fi
}

env_q() { # build a PreToolUse envelope around one questions[] array
    printf '{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","session_id":"t","tool_input":{"questions":%s}}' "$1"
}

# ── DENY: the real anti-pattern ─────────────────────────────────────────────

# 1. The exact Sessions question that started this whole thing.
SESSIONS=$(env_q '[{"question":"How should Sessions (#4) work?","header":"Sessions","options":[{"label":"Minimal real now","description":"list + detail over name/played_at, no graph link"},{"label":"Full dm-manager port","description":"captured_text + EntityTouch + graph"},{"label":"Hide Sessions for now","description":"remove the nav entry"}],"multiSelect":false}]')
assert_decision "deny: sessions minimal/full/hide" "$SESSIONS" "deny"

# 2. The sibling graph question — gradient in the STEM and an option.
GRAPH=$(env_q '[{"question":"How faithful should the rebuilt wiki graph be?","header":"Graph","options":[{"label":"Interactive force graph","description":"draggable nodes"},{"label":"Clickable link list","description":"simpler"},{"label":"Full fidelity","description":"everything dm-manager had"}],"multiSelect":false}]')
assert_decision "deny: how-faithful stem + full fidelity" "$GRAPH" "deny"

# 3. Canonical do-it-fully / partially / leave menu.
FULLPART=$(env_q '[{"question":"How should I handle the refactor?","header":"Refactor","options":[{"label":"Do it fully","description":"all call sites"},{"label":"Do it partially","description":"just the hot path"},{"label":"Leave as-is","description":"no change"}],"multiSelect":false}]')
assert_decision "deny: fully/partially/leave" "$FULLPART" "deny"

# 4. Stop-short peer with no explicit minimal/full pairing.
PATCH=$(env_q '[{"question":"What should I do about the bug?","header":"Bug","options":[{"label":"Fix the root cause","description":"proper fix"},{"label":"Quick patch","description":"band-aid"},{"label":"Leave it as-is","description":"defer the fix"}],"multiSelect":false}]')
assert_decision "deny: stop-short leave-as-is peer" "$PATCH" "deny"

# 5. Minimal vs full, two options only.
MINFULL=$(env_q '[{"question":"Which build?","header":"Build","options":[{"label":"Minimal version","description":"core only"},{"label":"Full version","description":"all features"}],"multiSelect":false}]')
assert_decision "deny: minimal vs full (2 opts)" "$MINFULL" "deny"

# 6. A multi-question call where a LATER question is the gradient (must still trip).
MULTI=$(env_q '[{"question":"Which database?","header":"DB","options":[{"label":"Postgres","description":"x"},{"label":"SQLite","description":"y"}],"multiSelect":false},{"question":"How much of the importer should I build?","header":"Importer","options":[{"label":"Minimal","description":"happy path"},{"label":"Comprehensive","description":"all source formats"}],"multiSelect":false}]')
assert_decision "deny: gradient in 2nd question of a multi-Q call" "$MULTI" "deny"

# 6a. Gradient STEM carrying its work object, with options that trip NO option
# lexicon — so only the stem branch can catch it. Pins the "how much OF THE
# <work>" half that survives the capacity-question narrowing.
HOWMUCH=$(env_q '[{"question":"How much of the migration should I finish?","header":"Migration","options":[{"label":"Through step 3","description":"stop at the FK repoint"},{"label":"Through step 7","description":"the detail tables too"}],"multiSelect":false}]')
assert_decision "deny: how-much-of-the-migration stem" "$HOWMUCH" "deny"

# 6b. Stop-short "defer" WITH a deferral object — must still deny after the
# `defer to <noun>` delegation shape is carved out.
DEFER_NOW=$(env_q '[{"question":"What about the flaky test?","header":"Flake","options":[{"label":"Fix the race","description":"proper fix"},{"label":"Defer this for now","description":"revisit when it bites"}],"multiSelect":false}]')
assert_decision "deny: defer-this-for-now stop-short" "$DEFER_NOW" "deny"

# 6c. Deferral to a later time is a stop-short even though it reads "defer to".
DEFER_LATER=$(env_q '[{"question":"What about the cleanup?","header":"Cleanup","options":[{"label":"Do the cleanup","description":"now"},{"label":"Defer to later","description":"after the release"}],"multiSelect":false}]')
assert_decision "deny: defer-to-later stop-short" "$DEFER_LATER" "deny"

# ── ALLOW: genuine forks and non-targets ────────────────────────────────────

# 7. Direction fork: different databases.
DBS=$(env_q '[{"question":"Which database engine?","header":"DB","options":[{"label":"Postgres","description":"server"},{"label":"SQLite","description":"embedded"},{"label":"MySQL","description":"server"}],"multiSelect":false}]')
assert_decision "allow: postgres/sqlite/mysql direction fork" "$DBS" "none"

# 8. HIGH word alone must NOT trip ("full backup" is a real kind, not a gradient).
BACKUP=$(env_q '[{"question":"Which backup strategy?","header":"Backup","options":[{"label":"Full backup","description":"every file nightly"},{"label":"Incremental backup","description":"only changes"}],"multiSelect":false}]')
assert_decision "allow: full vs incremental backup (HIGH only)" "$BACKUP" "none"

# 9. LOW word alone must NOT trip ("basic auth" is a real scheme).
AUTH=$(env_q '[{"question":"Which auth scheme?","header":"Auth","options":[{"label":"Basic auth","description":"header creds"},{"label":"OAuth","description":"tokens"},{"label":"SAML","description":"enterprise SSO"}],"multiSelect":false}]')
assert_decision "allow: basic/oauth/saml (LOW only)" "$AUTH" "none"

# 10. Keep-vs-change direction fork ("keep" is not a stop-short of requested work).
LANG=$(env_q '[{"question":"What language for the new service?","header":"Lang","options":[{"label":"Rewrite in Rust","description":"perf"},{"label":"Keep TypeScript","description":"velocity"}],"multiSelect":false}]')
assert_decision "allow: rust vs keep-typescript direction" "$LANG" "none"

# 11. Sequencing fork (now vs later as DIRECTIONS, no completeness gradient).
DEPLOY=$(env_q '[{"question":"Where should this land first?","header":"Target","options":[{"label":"Deploy to staging","description":"verify there"},{"label":"Deploy to production","description":"go straight live"}],"multiSelect":false}]')
assert_decision "allow: staging vs production target" "$DEPLOY" "none"

# 11a. Capacity question: "how many" of a RESOURCE, not of the requested work.
# The options are quantities of a thing to run, not amounts of work to do —
# there is no fuller/lesser version of the same task on offer. Regressed from
# bug `gradient-question-guard-false-positives-capacity-and-defer-to-x`.
REPLICAS=$(env_q '[{"question":"How many replicas should the cluster run?","header":"Replicas","options":[{"label":"1","description":"single node"},{"label":"3","description":"quorum"},{"label":"5","description":"larger quorum"}],"multiSelect":false}]')
assert_decision "allow: how-many-replicas capacity question" "$REPLICAS" "none"

# 11b. Capacity question with a units object ("how much" + a resource).
MEMORY=$(env_q '[{"question":"How much memory should the container get?","header":"Memory","options":[{"label":"512 MB","description":"lean"},{"label":"2 GB","description":"headroom"}],"multiSelect":false}]')
assert_decision "allow: how-much-memory capacity question" "$MEMORY" "none"

# 11c. "Defer to X" = delegate the decision to a default — a genuine DIRECTION
# fork, not a stop-short peer. The bare \bdefer token used to swallow this.
DEFER_TO=$(env_q '[{"question":"Which backoff strategy?","header":"Backoff","options":[{"label":"Defer to library defaults","description":"use the client library backoff"},{"label":"Custom backoff","description":"hand-tuned schedule"}],"multiSelect":false}]')
assert_decision "allow: defer-to-library-defaults direction fork" "$DEFER_TO" "none"

# 11d. The other delegation shape — deferring to a convention/owner, not later.
DEFER_OWNER=$(env_q '[{"question":"Who picks the retention window?","header":"Retention","options":[{"label":"Defer to the platform team","description":"they own the policy"},{"label":"Set it here","description":"pin 30d in this repo"}],"multiSelect":false}]')
assert_decision "allow: defer-to-team delegation" "$DEFER_OWNER" "none"

# 12. Non-AskUserQuestion tool → no decision (the matcher also scopes, but defensive).
BASH='{"hook_event_name":"PreToolUse","tool_name":"Bash","session_id":"t","tool_input":{"command":"echo do it fully or minimal"}}'
assert_decision "allow: non-AskUserQuestion tool" "$BASH" "none"

# 13. Malformed JSON → fail-open.
assert_decision "allow: malformed json fails open" 'not json {{{' "none"

# 14. Missing questions array → fail-open.
NOQ='{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","session_id":"t","tool_input":{}}'
assert_decision "allow: missing questions array" "$NOQ" "none"

# ── deny REASON quality + audit log ─────────────────────────────────────────

# 15. The deny reason names the memory and tells the model what to do instead.
REASON_OUT=$(run_hook "$SESSIONS")
assert_contains "deny reason cites the feedback memory" "gradient-question-anti-pattern-completeness-as-menu" "$REASON_OUT"
assert_contains "deny reason says default to completeness" "DEFAULT TO COMPLETENESS" "$REASON_OUT"

# 16. Every deny is logged (no quiet bypass / audit trail).
LOGF="$TMP_BASE/audit.log"
printf '%s' "$SESSIONS" | TOOLKIT_HOOK_DRIFT_LOG="$LOGF" "$HOOK" >/dev/null 2>&1
if [ -s "$LOGF" ] && grep -q "denied" "$LOGF"; then
    echo "PASS deny is written to the audit log"
    PASS=$((PASS + 1))
else
    echo "FAIL deny is written to the audit log"
    FAIL=$((FAIL + 1))
fi

echo
echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
