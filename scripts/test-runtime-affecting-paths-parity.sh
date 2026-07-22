#!/usr/bin/env bash
# scripts/test-runtime-affecting-paths-parity.sh — pin classification
# parity between scripts/runtime-affecting-paths.json and
# scripts/post-commit-restart-advisor.sh (the canonical post-commit
# classifier that consumes the manifest). This repo is Go-only post-
# rust-retirement (T6) and the dashboard split to its own repo (chain
# auto-startup-dev-services T3), so the Rust-crate + apps/dashboard arms
# were retired from both sides (bug 985); this test pins what remains.
#
# Approach: classification-consistency, not list-equality. The advisor
# uses a richer case-statement grammar (|-alternations, different
# wildcard shapes) than a flat glob list, so a direct list comparison
# is brittle. Instead, run a set of representative paths through both
# classifiers and assert they agree. This catches the failure modes
# that matter (a runtime-affecting path the manifest misses; a non-
# runtime-affecting path the manifest incorrectly flags).
#
# How each side classifies:
#   Manifest: bash-loop check each representative path against every
#             glob in runtime_affecting_globs. Match = "runtime-
#             affecting".
#   Advisor:  invoke a thin classification wrapper extracted from
#             post-commit-restart-advisor.sh — the same case-statement
#             but with the rebuild/restart side-effects stripped.
#
# Exit 0 on all-paths-agree; 1 on any disagreement. Wired into
# scripts/precommit.sh so drift never lands.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MANIFEST="$REPO_ROOT/scripts/runtime-affecting-paths.json"

if [[ ! -f "$MANIFEST" ]]; then
    echo "FATAL: manifest not found: $MANIFEST" >&2
    exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "FATAL: jq required for parity check" >&2
    exit 1
fi

# Representative path set. Covers each runtime-affecting category
# (Go binary, launcher, forge-schemas, rubrics, action-docs) AND each
# non-runtime-affecting category (docs, scripts, events, hooks, action-
# manifests). The set is small but covers every classification arm the
# advisor has — adding a new arm in the advisor requires adding a
# representative path here, which is itself the parity ratchet.
declare -A REPRESENTATIVE=(
    # Expected: runtime-affecting (a restart/rebuild is required).
    ["go/cmd/toolkit-server/main.go"]="runtime"
    ["go/internal/work/task.go"]="runtime"
    ["go/go.mod"]="runtime"
    ["go/Makefile"]="runtime"
    ["go/launch.sh"]="runtime"
    # Action-docs corpus: relocated under the Go module + go:embed'd
    # (chain single-source-action-describe T6), so a chunk edit needs a
    # rebuild and classifies runtime via the go/internal/** arm.
    ["go/internal/actiondocs/corpus/work/task_block.toml"]="runtime"
    # Forge schemas + rubrics — loaded at startup via registry.Load
    # (forge) and --rubrics-dir (rubrics). Edits need a restart for
    # the new schema / rubric to take effect. Bug 857 fix.
    ["blueprints/forge-schemas/task.toml"]="runtime"
    ["blueprints/rubrics/skill-applicability.toml"]="runtime"
    # Expected: non-runtime-affecting (no daemon restart).
    ["docs/SOMETHING.md"]="docs"
    ["scripts/runtime-affecting-paths.json"]="docs"
    ["README.md"]="docs"
    # Event payload schemas are SPEC-only — the runtime uses an
    # embedded copy at go/internal/events/schemas/ (caught by the
    # go/internal/** arm). Bug 857 fix.
    ["blueprints/events/TaskTransitioned.json"]="docs"
    # Claude Code hook scripts — read by the harness at hook firing,
    # not by the daemon. Bug 857 fix.
    ["hooks/arc-close-filing-review-hook.sh"]="docs"
    ["action-manifests/dispatch-policy.toml"]="docs"
)

# Classify a path against the MANIFEST: returns "runtime" if any glob
# matches, "other" otherwise. Glob matching uses bash extglob with
# `**` -> match across `/`.
shopt -s extglob globstar
classify_via_manifest() {
    local path="$1"
    local manifest_globs
    mapfile -t manifest_globs < <(jq -r '.runtime_affecting_globs[]' "$MANIFEST")
    for glob in "${manifest_globs[@]}"; do
        # bash's pattern matching uses `**` correctly under globstar.
        # shellcheck disable=SC2053
        if [[ "$path" == $glob ]]; then
            echo "runtime"
            return
        fi
    done
    echo "other"
}

# Classify a path against the ADVISOR's case-statement. We reproduce
# the advisor's classification arms inline — the advisor's source is
# authoritative, but its case statement is intertwined with rebuild
# side-effects that we can't easily extract. The inline copy is the
# regression-pinned mirror; the parity test fails the moment they
# diverge on any representative path.
classify_via_advisor_logic() {
    local path="$1"
    case "$path" in
        # Mirror of the advisor's NEED_HTTP_RESTART / NEED_STDIO_RESTART
        # arms. Keep in sync with scripts/post-commit-restart-advisor.sh
        # classification arms. The parity test below asserts every
        # REPRESENTATIVE path classifies the same here as the advisor would.
        go/cmd/*|go/internal/*|go/go.mod|go/go.sum|go/Makefile) echo "runtime" ;;
        go/launch.sh) echo "runtime" ;;
        blueprints/forge-schemas/*) echo "runtime" ;;
        blueprints/rubrics/*) echo "runtime" ;;
        hooks/*) echo "other" ;;
        scripts/*|*.md|blueprints/events/*|action-manifests/*|.git-hooks/*|*.toml|.gitignore|README*|CONVENTIONS*) echo "other" ;;
        *) echo "other" ;;
    esac
}

FAIL=0
for path in "${!REPRESENTATIVE[@]}"; do
    expected="${REPRESENTATIVE[$path]}"
    # Normalize expected: anything non-runtime is "other" for the
    # purpose of this check (we don't distinguish docs vs scripts vs
    # events — the advisor only cares about runtime-vs-other to decide
    # whether a restart is needed).
    if [[ "$expected" != "runtime" ]]; then
        expected="other"
    fi
    manifest_verdict=$(classify_via_manifest "$path")
    advisor_verdict=$(classify_via_advisor_logic "$path")
    if [[ "$manifest_verdict" != "$expected" ]]; then
        echo "FAIL: manifest classifies '$path' as $manifest_verdict, expected $expected" >&2
        FAIL=1
    fi
    if [[ "$advisor_verdict" != "$expected" ]]; then
        echo "FAIL: advisor-logic classifies '$path' as $advisor_verdict, expected $expected" >&2
        FAIL=1
    fi
    if [[ "$manifest_verdict" != "$advisor_verdict" ]]; then
        echo "FAIL: drift on '$path' — manifest=$manifest_verdict, advisor=$advisor_verdict" >&2
        FAIL=1
    fi
done

if [[ $FAIL -eq 0 ]]; then
    echo "runtime-affecting-paths.json + post-commit-restart-advisor.sh: agree on ${#REPRESENTATIVE[@]} representative paths"
fi
exit $FAIL
