#!/usr/bin/env bash
# test-materialize-memory.sh — regression scaffold for the SessionStart
# materialization hook. Each case sets up a sandboxed vault root +
# projects root via TOOLKIT_VAULT_ROOT + TOOLKIT_PROJECTS_ROOT envs,
# invokes the hook with a scripted SessionStart payload, and asserts the
# expected materialized state.
#
# Cases:
#   1. user-kind entry fans out to every existing project's harness dir.
#   2. feedback-kind with metadata.project routes to its matching project
#      AND the neutral dev-root aggregate, but NOT other projects (regression
#      for materialize-memory-hook-skips-project-scoped-memories-for-neutral-
#      dev-cwd: neutral ~/dev sessions must auto-load project-scoped memories).
#   3. feedback-kind without metadata.project routes to SessionStart cwd's
#      project AND the neutral dev-root aggregate.
#   4. Stale cleanup: a sentinel-marked file in a harness dir whose vault
#      backing was deleted is removed.
#   5. User-curated (non-sentinel) files in harness dirs are NEVER removed.
#   6. MEMORY.md is rebuilt between markers; user-curated lines outside
#      the markers survive.
#   7. Re-running the hook with no vault changes is a no-op
#      (idempotent — no churn on materialized files).
#   8. Missing vault root → exits 0 silently (no materialization needed).
#   9. SessionStart emits a valid hookSpecificOutput.additionalContext
#      envelope carrying the regenerated MEMORY.md content.
#  10. The emitted MEMORY.md is the LAUNCH project's, not the neutral
#      aggregate (project-scoped rules for other projects are excluded).
#  11. A neutral (~/dev) session emits the dev-root aggregate MEMORY.md
#      (which carries project-scoped rules).
#
# Run:  bash hooks/test-materialize-memory.sh

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$SCRIPT_DIR/materialize-memory.sh"

if [ ! -x "$HOOK" ]; then
    echo "FAIL: hook not executable: $HOOK"
    exit 1
fi

TMP_BASE=$(mktemp -d -t mat-mem-test-XXXXXX)
trap 'rm -rf "$TMP_BASE"' EXIT

PASS=0
FAIL=0

assert_contains() {
    local label="$1"
    local needle="$2"
    local haystack="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "PASS $label"
        PASS=$((PASS + 1))
    else
        echo "FAIL $label (missing: $needle)"
        FAIL=$((FAIL + 1))
    fi
}

assert_not_contains() {
    local label="$1"
    local needle="$2"
    local haystack="$3"
    if printf '%s' "$haystack" | grep -qF -- "$needle"; then
        echo "FAIL $label (unexpected: $needle)"
        FAIL=$((FAIL + 1))
    else
        echo "PASS $label"
        PASS=$((PASS + 1))
    fi
}

assert_file_exists() {
    local label="$1"
    local path="$2"
    if [ -f "$path" ]; then
        echo "PASS $label"
        PASS=$((PASS + 1))
    else
        echo "FAIL $label (no file at $path)"
        FAIL=$((FAIL + 1))
    fi
}

assert_file_missing() {
    local label="$1"
    local path="$2"
    if [ ! -f "$path" ]; then
        echo "PASS $label"
        PASS=$((PASS + 1))
    else
        echo "FAIL $label (unexpected file at $path)"
        FAIL=$((FAIL + 1))
    fi
}

# Helper: build a sandbox vault + projects layout under tmp dir.
# Usage: setup_sandbox <case-dir>
setup_sandbox() {
    local case_dir="$1"
    mkdir -p "$case_dir/vault/memory"/{user,feedback,project,reference}
    mkdir -p "$case_dir/projects"
}

# Helper: write a vault memory entry.
# Usage: write_vault_entry <case-dir> <kind> <name> <project-or-empty> [extra-frontmatter]
write_vault_entry() {
    local case_dir="$1"
    local kind="$2"
    local name="$3"
    local project="$4"
    local file="$case_dir/vault/memory/$kind/$name.md"
    {
        printf -- '---\n'
        printf 'name: %s\n' "$name"
        printf 'description: Test entry %s\n' "$name"
        printf 'metadata:\n'
        printf '  type: %s\n' "$kind"
        if [ -n "$project" ]; then
            printf '  project: %s\n' "$project"
        fi
        printf -- '---\n\n'
        printf 'Body for %s.\n' "$name"
    } >"$file"
}

# Helper: run the hook with a scripted SessionStart payload. stdout (the
# additionalContext JSON envelope) is discarded — file-state cases assert
# on the materialized filesystem, not stdout. Use run_hook_capture when
# the assertion is about the emitted envelope.
# Usage: run_hook <case-dir> <cwd>
run_hook() {
    local case_dir="$1"
    local cwd="$2"
    local payload
    payload=$(printf '{"session_id":"s","cwd":"%s"}' "$cwd")
    TOOLKIT_VAULT_ROOT="$case_dir/vault" TOOLKIT_PROJECTS_ROOT="$case_dir/projects" \
        bash "$HOOK" <<<"$payload" >/dev/null
}

# Helper: run the hook and return its stdout (the SessionStart
# additionalContext JSON envelope) for assertion.
# Usage: OUT=$(run_hook_capture <case-dir> <cwd>)
run_hook_capture() {
    local case_dir="$1"
    local cwd="$2"
    local payload
    payload=$(printf '{"session_id":"s","cwd":"%s"}' "$cwd")
    TOOLKIT_VAULT_ROOT="$case_dir/vault" TOOLKIT_PROJECTS_ROOT="$case_dir/projects" \
        bash "$HOOK" <<<"$payload"
}

# ----- case 1: user-kind fans out to every project -------------------
CASE=$TMP_BASE/case1; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
mkdir -p "$CASE/projects/-home-sophi-dev-seed-packet/memory"
write_vault_entry "$CASE" "user" "linguistic-tics" ""
run_hook "$CASE" "/home/user/dev"
assert_file_exists "case1_user_kind_lands_in_project_a" "$CASE/projects/-home-sophi-dev/memory/linguistic-tics.md"
assert_file_exists "case1_user_kind_lands_in_project_b" "$CASE/projects/-home-sophi-dev-seed-packet/memory/linguistic-tics.md"
assert_contains "case1_user_kind_has_sentinel" "<!-- materialized-from-vault -->" "$(cat "$CASE/projects/-home-sophi-dev/memory/linguistic-tics.md")"

# ----- case 2: feedback w/ project → matching dir + neutral aggregate,
#               NOT other projects (bug regression) ---------------------
CASE=$TMP_BASE/case2; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
mkdir -p "$CASE/projects/-home-sophi-dev-seed-packet/memory"
mkdir -p "$CASE/projects/-home-sophi-dev-dm-toolkit/memory"
write_vault_entry "$CASE" "feedback" "seed-packet-only-rule" "seed-packet"
run_hook "$CASE" "/home/user/dev"
assert_file_exists "case2_feedback_routes_to_matching_project" "$CASE/projects/-home-sophi-dev-seed-packet/memory/seed-packet-only-rule.md"
# The neutral dev-root dir (slug -home-sophi-dev) is the aggregate surface:
# a ~/dev session MUST auto-load this project-scoped memory.
assert_file_exists "case2_feedback_lands_in_neutral_aggregate" "$CASE/projects/-home-sophi-dev/memory/seed-packet-only-rule.md"
# But scoped dirs stay scoped — an unrelated project does NOT receive it.
assert_file_missing "case2_feedback_skips_other_project" "$CASE/projects/-home-sophi-dev-dm-toolkit/memory/seed-packet-only-rule.md"

# ----- case 3: feedback-kind without project routes to current cwd ---
CASE=$TMP_BASE/case3; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev-mcp-servers/memory"
write_vault_entry "$CASE" "feedback" "no-project-tag" ""
run_hook "$CASE" "/home/user/dev/mcp-servers"
assert_file_exists "case3_fallback_uses_current_cwd" "$CASE/projects/-home-sophi-dev-mcp-servers/memory/no-project-tag.md"
# Also lands in the neutral aggregate (created by the hook if absent).
assert_file_exists "case3_no_project_also_lands_in_neutral_aggregate" "$CASE/projects/-home-sophi-dev/memory/no-project-tag.md"

# ----- case 4: stale cleanup removes sentinel-marked orphans ----------
CASE=$TMP_BASE/case4; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
# Plant a sentinel-marked file that has NO backing vault entry.
{
    printf -- '---\nname: orphan\ndescription: stale\nmetadata:\n  type: user\n---\n\nbody\n'
    printf '<!-- materialized-from-vault -->\n'
} >"$CASE/projects/-home-sophi-dev/memory/orphan.md"
run_hook "$CASE" "/home/user/dev"
assert_file_missing "case4_orphan_sentinel_file_cleaned" "$CASE/projects/-home-sophi-dev/memory/orphan.md"

# ----- case 5: user-curated (non-sentinel) files survive cleanup -----
CASE=$TMP_BASE/case5; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
# Plant a user-curated file (no sentinel) with NO matching vault entry.
{
    printf -- '---\nname: user-curated\ndescription: hand-authored\nmetadata:\n  type: feedback\n---\n\nuser body\n'
} >"$CASE/projects/-home-sophi-dev/memory/user-curated.md"
run_hook "$CASE" "/home/user/dev"
assert_file_exists "case5_user_curated_file_survives" "$CASE/projects/-home-sophi-dev/memory/user-curated.md"

# ----- case 6: MEMORY.md regen + user-curated line preservation ------
CASE=$TMP_BASE/case6; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
write_vault_entry "$CASE" "user" "tic-one" ""
write_vault_entry "$CASE" "user" "tic-two" ""
# Pre-existing MEMORY.md with user-curated line outside markers.
{
    printf '<!-- materialized-from-vault:start -->\n'
    printf '<!-- materialized-from-vault:end -->\n'
    printf '\n- [user-curated-thing](manual.md) — preserved across rebuilds\n'
} >"$CASE/projects/-home-sophi-dev/memory/MEMORY.md"
run_hook "$CASE" "/home/user/dev"
MEMORY_MD=$(cat "$CASE/projects/-home-sophi-dev/memory/MEMORY.md")
assert_contains "case6_memory_md_lists_tic_one" "[tic-one](tic-one.md)" "$MEMORY_MD"
assert_contains "case6_memory_md_lists_tic_two" "[tic-two](tic-two.md)" "$MEMORY_MD"
assert_contains "case6_memory_md_preserves_user_curated" "preserved across rebuilds" "$MEMORY_MD"

# ----- case 7: idempotent re-run produces no churn -------------------
CASE=$TMP_BASE/case7; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
write_vault_entry "$CASE" "user" "idempotent-test" ""
run_hook "$CASE" "/home/user/dev"
MTIME_BEFORE=$(stat -c %Y "$CASE/projects/-home-sophi-dev/memory/idempotent-test.md" 2>/dev/null || stat -f %m "$CASE/projects/-home-sophi-dev/memory/idempotent-test.md")
sleep 1
run_hook "$CASE" "/home/user/dev"
MTIME_AFTER=$(stat -c %Y "$CASE/projects/-home-sophi-dev/memory/idempotent-test.md" 2>/dev/null || stat -f %m "$CASE/projects/-home-sophi-dev/memory/idempotent-test.md")
if [ "$MTIME_BEFORE" = "$MTIME_AFTER" ]; then
    echo "PASS case7_idempotent_no_rewrite"
    PASS=$((PASS + 1))
else
    echo "FAIL case7_idempotent_no_rewrite (mtime changed $MTIME_BEFORE → $MTIME_AFTER)"
    FAIL=$((FAIL + 1))
fi

# ----- case 8: missing vault root → silent exit ----------------------
CASE=$TMP_BASE/case8
mkdir -p "$CASE/projects"
OUT=$(TOOLKIT_VAULT_ROOT="$CASE/nonexistent-vault" TOOLKIT_PROJECTS_ROOT="$CASE/projects" \
    bash "$HOOK" <<<'{"session_id":"s","cwd":"/home/user/dev"}' 2>&1)
if [ -z "$OUT" ]; then
    echo "PASS case8_missing_vault_silent_exit"
    PASS=$((PASS + 1))
else
    echo "FAIL case8_missing_vault_silent_exit (stdout=$OUT)"
    FAIL=$((FAIL + 1))
fi

# ----- case 9: SessionStart emits a valid additionalContext envelope --
CASE=$TMP_BASE/case9; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
write_vault_entry "$CASE" "user" "emit-test-tic" ""
OUT=$(run_hook_capture "$CASE" "/home/user/dev")
assert_contains "case9_emits_hookSpecificOutput" '"hookSpecificOutput"' "$OUT"
assert_contains "case9_emits_sessionstart_event" '"SessionStart"' "$OUT"
assert_contains "case9_emits_additionalContext" '"additionalContext"' "$OUT"
# The injected context carries the regenerated MEMORY.md index line.
assert_contains "case9_additionalContext_carries_memory_index" "emit-test-tic" "$OUT"
if printf '%s' "$OUT" | jq -e . >/dev/null 2>&1; then
    echo "PASS case9_output_is_valid_json"
    PASS=$((PASS + 1))
else
    echo "FAIL case9_output_is_valid_json"
    FAIL=$((FAIL + 1))
fi
# The additionalContext text (not just the envelope) carries the index.
ADDL=$(printf '%s' "$OUT" | jq -r '.hookSpecificOutput.additionalContext' 2>/dev/null)
assert_contains "case9_additionalContext_text_has_index_line" "[emit-test-tic](emit-test-tic.md)" "$ADDL"

# ----- case 10: emit prefers the LAUNCH project's MEMORY.md -----------
CASE=$TMP_BASE/case10; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
mkdir -p "$CASE/projects/-home-sophi-dev-mcp-servers/memory"
mkdir -p "$CASE/projects/-home-sophi-dev-seed-packet/memory"
write_vault_entry "$CASE" "feedback" "seed-scoped-rule" "seed-packet"
write_vault_entry "$CASE" "feedback" "mcp-scoped-rule" "mcp-servers"
OUT=$(run_hook_capture "$CASE" "/home/user/dev/mcp-servers")
# Launched from mcp-servers: its MEMORY.md has the mcp-scoped rule but NOT
# the seed-packet-scoped rule (which only the neutral aggregate carries).
assert_contains "case10_launch_project_memory_has_own_scoped_rule" "mcp-scoped-rule" "$OUT"
assert_not_contains "case10_launch_project_memory_excludes_other_scope" "seed-scoped-rule" "$OUT"

# ----- case 11: neutral session emits the dev-root aggregate ----------
CASE=$TMP_BASE/case11; setup_sandbox "$CASE"
mkdir -p "$CASE/projects/-home-sophi-dev/memory"
mkdir -p "$CASE/projects/-home-sophi-dev-seed-packet/memory"
write_vault_entry "$CASE" "feedback" "seed-scoped-rule" "seed-packet"
OUT=$(run_hook_capture "$CASE" "/home/user/dev")
# Neutral cwd → aggregate MEMORY.md carries the project-scoped rule.
assert_contains "case11_neutral_emits_aggregate_with_scoped_rule" "seed-scoped-rule" "$OUT"

echo
echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
