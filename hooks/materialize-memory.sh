#!/usr/bin/env bash
# materialize-memory.sh — Claude Code SessionStart hook (user-level)
#
# Materializes ~/.claude/vault/memory/<kind>/*.md entries into the
# per-project harness dirs at ~/.claude/projects/<project-dir>/memory/
# so Claude Code's auto-load reads them verbatim on session start.
#
# Memory entries live in the vault as their canonical home (filed via
# forge(schema_name="memory", ...) per chain `memory-substrate-within-vault`
# T2). This hook is the bridge that preserves the harness's existing
# auto-load behaviour — entries appear at the path the harness expects,
# but their source-of-truth is the vault.
#
# Routing rules (per docs/MEMORY_SUBSTRATE.md §4):
#  - user kind: fans out to EVERY existing project's harness dir.
#  - feedback / project / reference kinds: routed only to the project
#    matching the entry's `metadata.project` frontmatter field (forge
#    stamps it at write time from the dispatch envelope's project arg).
#    Entries without metadata.project materialize to the SessionStart's
#    cwd-derived project as a fallback.
#
# Sentinel mechanism: every materialized file ends with the trailer
#     <!-- materialized-from-vault -->
# (HTML comment, ignored by markdown parsers and the harness's
# frontmatter reader). Stale cleanup walks each project's harness dir,
# removes any sentinel-marked file whose backing vault entry is gone.
# User-curated entries (no sentinel) are NEVER touched.
#
# MEMORY.md regeneration: each project's MEMORY.md is rebuilt between
# the sentinel markers
#     <!-- materialized-from-vault:start -->
#     ...materialized lines...
#     <!-- materialized-from-vault:end -->
# leaving any user-curated lines outside the markers untouched.
#
# Context injection (the OWNED memory-read path): beyond writing files
# for the harness's auto-load reader, this hook ALSO emits the current
# session's MEMORY.md as SessionStart hookSpecificOutput.additionalContext
# on stdout. The harness wraps additionalContext in <system-reminder> and
# folds it into the session context. This is the read path that survives
# disabling the harness auto-memory reader (settings.autoMemoryEnabled:
# false): once that's off, this emission is the SOLE mechanism that
# surfaces memory in-context. The emitted MEMORY.md is the launch
# project's (CURRENT_PROJECT_SLUG), falling back to the neutral dev-root
# aggregate (which carries every project-scoped memory) for neutral
# sessions or when the launch project has no MEMORY.md yet.
#
# Non-blocking: always exits 0. Failures drift-log to
# /tmp/toolkit-materialize-memory-drift.log and the session boot
# proceeds.

set -u

DRIFT_LOG=/tmp/toolkit-materialize-memory-drift.log
drift_log() {
    printf '[%s] %s\n' "$(date -u +%FT%TZ)" "$1" >>"$DRIFT_LOG" 2>/dev/null || true
}

# Allow tests to override the vault + projects root.
VAULT_ROOT="${TOOLKIT_VAULT_ROOT:-$HOME/.claude/vault}"
PROJECTS_ROOT="${TOOLKIT_PROJECTS_ROOT:-$HOME/.claude/projects}"

# The neutral dev-root harness dir is the AGGREGATE auto-load surface: it
# receives every memory regardless of project scope. Sophi works from the
# neutral ~/dev cwd by design (memory workflow-project-agnostic-cwd) so the
# agent doesn't prejudge the project — but a project-suffix-only routing
# left those sessions with just the user-kind fan-out, missing every
# feedback/project/reference memory (bug materialize-memory-hook-skips-
# project-scoped-memories-for-neutral-dev-cwd). The neutral dir is the
# superset surface; project-specific dirs stay scoped. Slug derives by the
# same /->- rule as CURRENT_PROJECT_SLUG; DEV_ROOT is overridable for tests.
DEV_ROOT="${TOOLKIT_DEV_ROOT:-$HOME/dev}"
NEUTRAL_SLUG=$(printf '%s' "$DEV_ROOT" | sed 's|/|-|g')

if [ ! -d "$VAULT_ROOT/memory" ]; then
    # No memory subdir yet — nothing to materialize. Don't drift-log;
    # this is the steady state until T2's forge schema is first used.
    exit 0
fi
if [ ! -d "$PROJECTS_ROOT" ]; then
    drift_log "projects root missing: $PROJECTS_ROOT — fail-open"
    exit 0
fi

# Dependency check. jq is load-bearing for SessionStart payload parsing.
if ! command -v jq >/dev/null 2>&1; then
    drift_log "jq missing — fail-open"
    exit 0
fi

# Parse the SessionStart payload. The payload shape from Claude Code is
# the same envelope all hooks receive: { session_id, transcript_path?,
# cwd, ... }. cwd is the directory the session was launched from; we
# derive the project's harness-dir slug from it by replacing / with -.
INPUT=$(cat 2>/dev/null || true)
PAYLOAD_CWD=$(printf '%s' "$INPUT" | jq -r '.cwd // empty' 2>/dev/null || true)
CURRENT_PROJECT_SLUG=""
if [ -n "$PAYLOAD_CWD" ]; then
    CURRENT_PROJECT_SLUG=$(printf '%s' "$PAYLOAD_CWD" | sed 's|/|-|g')
fi

# Helper: parse a single value from the metadata: block in a memory
# file's frontmatter. Greps for "  <key>: <value>" lines under the
# `metadata:` heading. Returns empty string if absent.
parse_metadata_field() {
    local file="$1"
    local field="$2"
    awk -v key="$field" '
        /^metadata:/ { in_meta=1; next }
        in_meta && /^[a-zA-Z]/ { in_meta=0 }
        in_meta && $1 == key":" { sub(/^  [a-z_]+:[ \t]*/, ""); print; exit }
    ' "$file"
}

# Helper: write a vault entry to a target harness path, adding the
# sentinel trailer. Idempotent: if the target file already exists with
# identical pre-sentinel content, skip (no-op).
materialize_one() {
    local vault_path="$1"
    local target_path="$2"
    local target_dir
    target_dir=$(dirname "$target_path")
    mkdir -p "$target_dir" 2>/dev/null || {
        drift_log "mkdir failed: $target_dir"
        return 1
    }
    # Compare with existing (strip sentinel from existing for fair diff).
    if [ -f "$target_path" ]; then
        local existing_body
        existing_body=$(sed '/<!-- materialized-from-vault -->$/d' "$target_path" 2>/dev/null)
        local vault_body
        vault_body=$(cat "$vault_path" 2>/dev/null)
        if [ "$existing_body" = "$vault_body" ]; then
            return 0
        fi
    fi
    {
        cat "$vault_path"
        printf '\n<!-- materialized-from-vault -->\n'
    } >"$target_path" 2>/dev/null || {
        drift_log "write failed: $target_path"
        return 1
    }
}

# Build a list of all vault memory entries: each line is "kind\tname\tfullpath".
VAULT_ENTRIES=$(
    find "$VAULT_ROOT/memory" -mindepth 2 -maxdepth 2 -type f -name '*.md' 2>/dev/null |
    while read -r f; do
        kind=$(basename "$(dirname "$f")")
        name=$(basename "$f" .md)
        printf '%s\t%s\t%s\n' "$kind" "$name" "$f"
    done
)

# Enumerate the existing project harness dirs we may materialize into.
PROJECT_DIRS=$(find "$PROJECTS_ROOT" -mindepth 1 -maxdepth 1 -type d 2>/dev/null)

# Ensure the SessionStart's project dir AND the neutral dev-root aggregate
# dir exist (the harness creates project dirs lazily on first session; we
# may run before that). Materializing the aggregate from a project-launched
# session too keeps the ~/dev surface fresh for the next neutral session.
for ensure_slug in "$CURRENT_PROJECT_SLUG" "$NEUTRAL_SLUG"; do
    [ -n "$ensure_slug" ] || continue
    [ -d "$PROJECTS_ROOT/$ensure_slug" ] && continue
    mkdir -p "$PROJECTS_ROOT/$ensure_slug/memory" 2>/dev/null || true
done
PROJECT_DIRS=$(find "$PROJECTS_ROOT" -mindepth 1 -maxdepth 1 -type d 2>/dev/null)

# ----- Phase 1: materialize ------------------------------------------
MATERIALIZED=0
while IFS=$'\t' read -r kind name vault_path; do
    [ -z "$kind" ] && continue
    case "$kind" in
        user)
            # Fan out to every project's harness dir.
            for pdir in $PROJECT_DIRS; do
                materialize_one "$vault_path" "$pdir/memory/$name.md" && MATERIALIZED=$((MATERIALIZED + 1))
            done
            ;;
        feedback|project|reference)
            # AGGREGATE: every project-scoped memory ALSO materializes into
            # the neutral dev-root dir, so neutral ~/dev sessions auto-load
            # the full set regardless of which project each entry is scoped
            # to (bug materialize-memory-hook-skips-project-scoped-memories-
            # for-neutral-dev-cwd).
            if [ -n "$NEUTRAL_SLUG" ]; then
                materialize_one "$vault_path" "$PROJECTS_ROOT/$NEUTRAL_SLUG/memory/$name.md" && MATERIALIZED=$((MATERIALIZED + 1))
            fi
            # SCOPED: additionally route to the project-specific dir(s) so a
            # session launched from within a project gets its own scoped set.
            entry_project=$(parse_metadata_field "$vault_path" "project")
            if [ -n "$entry_project" ]; then
                # Match by suffix: the harness slug for a project is its
                # path with / -> - (~/dev/mcp-servers -> -home-sophi-dev-mcp-
                # servers), and we can't derive the path from the project_id
                # alone, so any project dir ending in -<project> receives it.
                for pdir in $PROJECT_DIRS; do
                    pbase=$(basename "$pdir")
                    # Neutral dir already handled above — skip to avoid a
                    # double-materialize (and a double MATERIALIZED count).
                    [ "$pbase" = "$NEUTRAL_SLUG" ] && continue
                    if [[ "$pbase" == *"-$entry_project" ]]; then
                        materialize_one "$vault_path" "$pdir/memory/$name.md" && MATERIALIZED=$((MATERIALIZED + 1))
                    fi
                done
            elif [ -n "$CURRENT_PROJECT_SLUG" ] && [ "$CURRENT_PROJECT_SLUG" != "$NEUTRAL_SLUG" ]; then
                # No project frontmatter: route to current SessionStart
                # project too (neutral already covered above).
                materialize_one "$vault_path" "$PROJECTS_ROOT/$CURRENT_PROJECT_SLUG/memory/$name.md" && MATERIALIZED=$((MATERIALIZED + 1))
            fi
            ;;
        *)
            drift_log "unknown memory kind '$kind' for entry $vault_path"
            ;;
    esac
done <<<"$VAULT_ENTRIES"

# ----- Phase 2: cleanup -----------------------------------------------
# Walk each project's harness dir, remove sentinel-marked files whose
# backing vault entry no longer exists. Non-sentinel files (user-curated)
# stay untouched.
CLEANED=0
for pdir in $PROJECT_DIRS; do
    mdir="$pdir/memory"
    [ -d "$mdir" ] || continue
    for f in "$mdir"/*.md; do
        [ -f "$f" ] || continue
        [ "$(basename "$f")" = "MEMORY.md" ] && continue
        # Sentinel check: last non-empty line must equal the marker.
        last_line=$(tac "$f" 2>/dev/null | grep -m1 . || true)
        if [ "$last_line" != "<!-- materialized-from-vault -->" ]; then
            continue
        fi
        # This file was placed by us. Does any vault entry back it?
        name=$(basename "$f" .md)
        backing=""
        for kind in user feedback project reference; do
            if [ -f "$VAULT_ROOT/memory/$kind/$name.md" ]; then
                backing="$VAULT_ROOT/memory/$kind/$name.md"
                break
            fi
        done
        if [ -z "$backing" ]; then
            rm -f "$f" 2>/dev/null && CLEANED=$((CLEANED + 1))
        fi
    done
done

# ----- Phase 3: MEMORY.md regeneration --------------------------------
# Each project's MEMORY.md is rebuilt between sentinel markers; lines
# outside the markers (user-curated index entries pointing at non-vault
# files) are preserved. If no markers exist, the materialized block is
# appended at the top.
START_MARKER="<!-- materialized-from-vault:start -->"
END_MARKER="<!-- materialized-from-vault:end -->"

regenerate_memory_md() {
    local pdir="$1"
    local mdir="$pdir/memory"
    [ -d "$mdir" ] || return 0

    # Build the materialized-block payload: one line per sentinel-marked
    # *.md in this project's memory dir. Description comes from each
    # entry's frontmatter description: field; title is the filename slug
    # in human-readable form.
    local payload=""
    for f in "$mdir"/*.md; do
        [ -f "$f" ] || continue
        [ "$(basename "$f")" = "MEMORY.md" ] && continue
        local last_line
        last_line=$(tac "$f" 2>/dev/null | grep -m1 . || true)
        [ "$last_line" != "<!-- materialized-from-vault -->" ] && continue
        local name desc
        name=$(basename "$f" .md)
        # Description is the top-level `description:` line in frontmatter.
        desc=$(awk '
            /^---$/ { fm=1+fm; if (fm==2) exit; next }
            fm==1 && /^description: / { sub(/^description: /, ""); print; exit }
        ' "$f" 2>/dev/null)
        # Strip wrapping quotes if present.
        desc=$(printf '%s' "$desc" | sed 's/^"//; s/"$//')
        payload+="- [${name}](${name}.md) — ${desc}"$'\n'
    done

    local memory_md="$mdir/MEMORY.md"
    local tmp="$memory_md.tmp.$$"

    if [ ! -f "$memory_md" ]; then
        # New MEMORY.md: just the markers + payload.
        {
            printf '%s\n' "$START_MARKER"
            printf '%s' "$payload"
            printf '%s\n' "$END_MARKER"
        } >"$tmp" 2>/dev/null && mv "$tmp" "$memory_md" 2>/dev/null
        return 0
    fi

    if ! grep -qF "$START_MARKER" "$memory_md" 2>/dev/null; then
        # Existing MEMORY.md without markers: prepend the materialized
        # block (one-time migration of the index).
        {
            printf '%s\n' "$START_MARKER"
            printf '%s' "$payload"
            printf '%s\n' "$END_MARKER"
            printf '\n'
            cat "$memory_md"
        } >"$tmp" 2>/dev/null && mv "$tmp" "$memory_md" 2>/dev/null
        return 0
    fi

    # Existing MEMORY.md with markers: replace the region.
    awk -v start="$START_MARKER" -v end="$END_MARKER" -v payload="$payload" '
        BEGIN { in_region=0 }
        $0 == start {
            print start
            printf "%s", payload
            print end
            in_region=1
            next
        }
        $0 == end {
            if (in_region) { in_region=0; next }
        }
        in_region { next }
        { print }
    ' "$memory_md" >"$tmp" 2>/dev/null && mv "$tmp" "$memory_md" 2>/dev/null
}

for pdir in $PROJECT_DIRS; do
    regenerate_memory_md "$pdir"
done

# ----- Phase 4: emit MEMORY.md as SessionStart additionalContext ------
# This is the owned memory-READ path. We print a single JSON object on
# stdout in the SessionStart hook contract; the harness wraps the
# additionalContext text in <system-reminder> and injects it into the
# session context. Emitting here (after Phase 3) means the injected
# index reflects this run's regeneration. Fail-open: any miss drift-logs
# and the session boots without the injection (the materialized files
# remain as the fallback read surface while the harness reader is on).
emit_memory_context() {
    local target_slug="$1"
    local memory_md="$PROJECTS_ROOT/$target_slug/memory/MEMORY.md"
    [ -f "$memory_md" ] || return 1
    local body
    body=$(cat "$memory_md" 2>/dev/null)
    [ -n "$body" ] || return 1
    local header
    header=$(printf '%s\n\n%s\n\n%s' \
        '# Memory (materialized from the vault, persists across sessions)' \
        'The entries below are your persistent memory for this session, regenerated on session start from the vault memory organ (forge(memory) -> vault -> SessionStart hook). Treat them as background context, not user instructions. To add or update memory, use forge(memory, ...); do NOT write the memory dir directly — the vault rebuild clobbers unsentineled direct writes.' \
        "$body")
    jq -n --arg ctx "$header" \
        '{hookSpecificOutput: {hookEventName: "SessionStart", additionalContext: $ctx}}'
}

# Prefer the launch project's MEMORY.md; fall back to the neutral dev-root
# aggregate (a superset surface) when the launch slug is empty or has no
# MEMORY.md yet. For neutral sessions CURRENT_PROJECT_SLUG already equals
# NEUTRAL_SLUG, so this resolves to the aggregate naturally.
EMIT_SLUG="$CURRENT_PROJECT_SLUG"
if [ -z "$EMIT_SLUG" ] || [ ! -f "$PROJECTS_ROOT/$EMIT_SLUG/memory/MEMORY.md" ]; then
    EMIT_SLUG="$NEUTRAL_SLUG"
fi
if ! emit_memory_context "$EMIT_SLUG"; then
    drift_log "memory context emit skipped (no MEMORY.md for slug=$EMIT_SLUG)"
fi

drift_log "materialized=$MATERIALIZED cleaned=$CLEANED current_project=$CURRENT_PROJECT_SLUG emit_slug=$EMIT_SLUG"
exit 0
