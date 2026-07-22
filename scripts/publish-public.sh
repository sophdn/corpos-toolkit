#!/usr/bin/env bash
# publish-public.sh — produce a scrubbed, squashed public-mirror tree from a
# private repo's current HEAD. Core primitive of the mirror-publish pipeline
# (chain 438). It does NOT push and handles NO credentials — a Gitea Actions
# workflow calls this, runs the pre-push secret scan, then force-pushes.
#
# Model: squash-from-current-tree. Only HEAD's tracked tree is ever published;
# git history is discarded (one snapshot commit), so commits that predate the
# pii-scan gate can never leak.
#
# Three private-config files (all AUTO-DROPPED from the output so they can never
# leak the private strings they name):
#   .publish-manifest   — git-pathspecs to DROP entirely (private data / files).
#   .publish-scrub-map   — content genericization (chain 438 Option B): regex
#                          replacements applied to every text file in the output
#                          tree, so the PRIVATE repo keeps real infra strings
#                          (hostnames, IPs, paths) while the PUBLIC mirror gets
#                          placeholders. One rule per line: `<perl-regex> ==> <replacement>`.
#                          Rules apply top-to-bottom — order specific before general.
#   .pii-denylist        — grep -E patterns (shared with the pii-scan commit gate);
#                          run FAIL-CLOSED over the scrubbed output as a
#                          completeness check — if any private pattern survives
#                          the scrub map, the publish ABORTS. This is what makes
#                          Option B safe: a missed mapping stops the push, never leaks.
#
# Usage:
#   publish-public.sh --repo <path> [--manifest <file>] [--scrub-map <file>] \
#                     [--denylist <file>] --out <dir> [--dry-run]
set -euo pipefail

repo="$PWD"; manifest=""; scrubmap=""; denylist=""; out=""; dry=0
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) repo="$2"; shift 2 ;;
    --manifest) manifest="$2"; shift 2 ;;
    --scrub-map) scrubmap="$2"; shift 2 ;;
    --denylist) denylist="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    --dry-run) dry=1; shift ;;
    -h|--help) grep '^#' "$0" | sed 's/^#\{1,\} \{0,1\}//'; exit 0 ;;
    *) echo "publish-public: unknown arg: $1" >&2; exit 2 ;;
  esac
done

repo="$(cd "$repo" 2>/dev/null && git rev-parse --show-toplevel 2>/dev/null)" \
  || { echo "publish-public: --repo is not a git repo" >&2; exit 2; }
[ -z "$manifest" ] && manifest="$repo/.publish-manifest"
[ -z "$scrubmap" ] && scrubmap="$repo/.publish-scrub-map"
[ -z "$denylist" ] && denylist="$repo/.pii-denylist"
src_sha="$(git -C "$repo" rev-parse --short HEAD)"

if [ "$dry" -eq 0 ] && [ -z "$out" ]; then
  echo "publish-public: --out required (or use --dry-run)" >&2; exit 2
fi

# Read manifest → exclude patterns.
excludes=()
if [ -f "$manifest" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    [ -z "${line// /}" ] && continue
    case "$line" in \#*) continue ;; esac
    excludes+=("$line")
  done < "$manifest"
fi

# Resolve dropped files via git's own pathspec matching; flag stale patterns.
dropped=(); stale=()
for pat in "${excludes[@]:-}"; do
  [ -z "$pat" ] && continue
  mapfile -t m < <(git -C "$repo" ls-tree -r --name-only HEAD -- "$pat" 2>/dev/null || true)
  if [ "${#m[@]}" -eq 0 ]; then stale+=("$pat"); else dropped+=("${m[@]}"); fi
done
# Never publish the private-config files themselves — they name the private
# paths / strings the scrub is meant to keep OUT of the mirror.
for cfg in "$manifest" "$scrubmap" "$denylist"; do
  cfg_rel=""
  case "$cfg" in "$repo"/*) cfg_rel="${cfg#"$repo"/}" ;; esac
  if [ -n "$cfg_rel" ] && [ -n "$(git -C "$repo" ls-tree HEAD -- "$cfg_rel" 2>/dev/null)" ]; then
    dropped+=("$cfg_rel")
  fi
done
[ "${#dropped[@]}" -gt 0 ] && mapfile -t dropped < <(printf '%s\n' "${dropped[@]}" | sort -u)

# Read scrub-map rules: `<perl-regex> ==> <replacement>`.
scrub_lhs=(); scrub_rhs=()
if [ -f "$scrubmap" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    [ -z "${line// /}" ] && continue
    case "$line" in \#*) continue ;; esac
    case "$line" in
      *" ==> "*) scrub_lhs+=("${line%% ==> *}"); scrub_rhs+=("${line#* ==> }") ;;
      *) echo "publish-public: WARN malformed scrub-map line (no ' ==> '): $line" >&2 ;;
    esac
  done < "$scrubmap"
fi

total="$(git -C "$repo" ls-tree -r --name-only HEAD | wc -l)"
kept=$(( total - ${#dropped[@]} ))

report() {
  echo "publish-public: source $repo @ $src_sha"
  if [ -f "$manifest" ]; then echo "  manifest: $manifest (${#excludes[@]} pattern(s))"
  else echo "  manifest: $manifest (absent -> publish everything)"; fi
  if [ -f "$scrubmap" ]; then echo "  scrub-map: $scrubmap (${#scrub_lhs[@]} rule(s))"
  else echo "  scrub-map: $scrubmap (absent -> no content scrub)"; fi
  if [ -f "$denylist" ]; then echo "  denylist: $denylist (post-scrub fail-closed verify)"
  else echo "  denylist: $denylist (absent -> post-scrub verify OFF)"; fi
  echo "  tracked at HEAD: $total  |  drop: ${#dropped[@]}  |  publish: $kept"
  if [ "${#dropped[@]}" -gt 0 ]; then echo "  DROPPED:"; printf '    - %s\n' "${dropped[@]}"; fi
  if [ "${#stale[@]}" -gt 0 ]; then
    echo "  WARN manifest patterns matching NOTHING (stale, or a private file was renamed -> possible leak):"
    printf '    - %s\n' "${stale[@]}"
  fi
}

if [ "$dry" -eq 1 ]; then
  report; echo "  (dry-run — nothing written, nothing pushed)"; exit 0
fi

# Materialize HEAD's tree, drop excluded files.
if [ -e "$out" ] && [ -n "$(ls -A "$out" 2>/dev/null)" ]; then
  echo "publish-public: --out '$out' exists and is not empty; refusing" >&2; exit 2
fi
mkdir -p "$out"
git -C "$repo" archive --format=tar HEAD | tar -x -C "$out"
for f in "${dropped[@]:-}"; do [ -n "$f" ] && rm -f "$out/$f"; done
find "$out" -mindepth 1 -type d -empty -delete 2>/dev/null || true

# ── Content scrub (Option B) ────────────────────────────────────────────────
# Apply each scrub-map rule, in order, to every TEXT file in the output tree.
# Binary files (images, etc.) are skipped. LHS is a Perl regex; RHS a literal
# replacement. Both ride via env so the shell can't interpolate them.
if [ "${#scrub_lhs[@]}" -gt 0 ]; then
  text_files=0
  while IFS= read -r -d '' f; do
    grep -Iq . "$f" 2>/dev/null || continue   # skip binary/empty
    text_files=$((text_files+1))
    for i in "${!scrub_lhs[@]}"; do
      SCRUB_LHS="${scrub_lhs[$i]}" SCRUB_RHS="${scrub_rhs[$i]}" \
        perl -0777 -i -pe 's{$ENV{SCRUB_LHS}}{$ENV{SCRUB_RHS}}g' "$f" \
        || { echo "publish-public: perl scrub failed on $f (rule: ${scrub_lhs[$i]})" >&2; exit 1; }
    done
  done < <(find "$out" -type f -print0)
  echo "publish-public: scrub-map applied (${#scrub_lhs[@]} rule(s) over $text_files text file(s))"
fi

# ── Post-scrub fail-closed verify ───────────────────────────────────────────
# If any denylist pattern SURVIVES the scrub, abort — never publish a leak.
if [ -f "$denylist" ]; then
  leak=0
  while IFS= read -r term || [ -n "$term" ]; do
    term="${term%$'\r'}"
    [ -z "${term// /}" ] && continue
    case "$term" in \#*) continue ;; esac
    dm="$(grep -rInE --binary-files=without-match --exclude-dir=.git -e "$term" -- "$out" 2>/dev/null || true)"
    if [ -n "$dm" ]; then
      echo "publish-public: LEAK — denylist pattern survived scrub: /$term/" >&2
      printf '%s\n' "$dm" | sed 's/^/    /' >&2
      leak=1
    fi
  done < "$denylist"
  if [ "$leak" -ne 0 ]; then
    echo "publish-public: ABORT — private pattern(s) present in scrubbed tree; refusing to publish." >&2
    echo "  Fix: add a rule to $scrubmap (or drop the file via $manifest)." >&2
    exit 1
  fi
  echo "publish-public: post-scrub denylist verify clean"
fi

# Squash to one commit with pinned identity+date (idempotent snapshot).
git -C "$out" init -q
git -C "$out" add -A
src_date="$(git -C "$repo" show -s --format=%cI HEAD)"
GIT_AUTHOR_NAME=publish-public GIT_AUTHOR_EMAIL=publish-public@local GIT_AUTHOR_DATE="$src_date" \
GIT_COMMITTER_NAME=publish-public GIT_COMMITTER_EMAIL=publish-public@local GIT_COMMITTER_DATE="$src_date" \
  git -C "$out" commit -q -m "Public mirror snapshot (source $src_sha)"
report
echo "  -> scrubbed tree at $out (snapshot $(git -C "$out" rev-parse --short HEAD))"
