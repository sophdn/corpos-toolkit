#!/usr/bin/env bash
# secret-scan-tree.sh — fail-closed whole-tree secret scan (chain 438).
#
# Run on a scrubbed public-mirror tree immediately BEFORE any force-push. Uses the
# same high-precision secret patterns as the pii-scan commit gate, but scans EVERY
# file in the tree (not a staged diff). Any hit aborts with exit 1, so a tree that
# still contains a secret can never reach the public mirror.
#
# Waiver: a repo-local `.secret-scan-allow` (one repo-relative FILE PATH per line,
# `#` comments ignored) exempts specific files whose secret-pattern matches are
# KNOWN test fixtures — e.g. the ecosystem tests that feed a fake AWS key / GitHub
# token to prove the surface REJECTS inline secrets. The waiver is FILE-scoped
# (never directory-wide): a real-shaped token in any OTHER file still fail-closes.
# The allow file is read from the SOURCE repo (it is dropped from the mirror), so
# set $SECRET_SCAN_ALLOW_DIR or rely on $PWD (the workflow runs from the repo root).
#
# Usage: secret-scan-tree.sh <dir>
set -euo pipefail
dir="${1:-}"
if [ -z "$dir" ] || [ ! -d "$dir" ]; then
  echo "secret-scan-tree: usage: secret-scan-tree.sh <dir>" >&2; exit 2
fi

# Load the file-scoped waiver list from the source repo (absent → no waivers).
allow_file="${SECRET_SCAN_ALLOW_DIR:-$PWD}/.secret-scan-allow"
allow_paths=()
if [ -f "$allow_file" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    [ -z "${line// /}" ] && continue
    case "$line" in \#*) continue ;; esac
    allow_paths+=("$line")
  done < "$allow_file"
fi

is_waived() {  # $1 = repo-relative path of a match
  local rel="$1" a
  for a in "${allow_paths[@]:-}"; do
    [ -n "$a" ] && [ "$rel" = "$a" ] && return 0
  done
  return 1
}

# High-precision secret patterns — kept in sync with scripts/pii-scan.sh.
secret_patterns=(
  '-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----'   # RSA/EC/OPENSSH/PGP/DSA private keys
  'AKIA[0-9A-Z]{16}'                         # AWS access key id
  'ghp_[0-9A-Za-z]{36}'                      # GitHub personal access token
  'gho_[0-9A-Za-z]{36}'                      # GitHub OAuth token
  'ghu_[0-9A-Za-z]{36}'                      # GitHub user-to-server token
  'ghs_[0-9A-Za-z]{36}'                      # GitHub server-to-server token
  'ghr_[0-9A-Za-z]{36}'                      # GitHub refresh token
  'github_pat_[0-9A-Za-z_]{82}'              # GitHub fine-grained PAT
  'xox[baprs]-[0-9A-Za-z-]{10,}'             # Slack token
  'AIza[0-9A-Za-z_-]{35}'                    # Google API key
  'sk_live_[0-9A-Za-z]{24,}'                 # Stripe secret key
  'rk_live_[0-9A-Za-z]{24,}'                 # Stripe restricted key
)

hits=0
for pat in "${secret_patterns[@]}"; do
  raw="$(grep -rInE --binary-files=without-match --exclude-dir=.git -e "$pat" -- "$dir" 2>/dev/null || true)"
  [ -n "$raw" ] || continue
  unwaived=""
  while IFS= read -r ln; do
    [ -z "$ln" ] && continue
    f="${ln%%:*}"          # "<dir>/<rel>"
    rel="${f#"$dir"/}"     # repo-relative path
    is_waived "$rel" && continue
    unwaived+="${ln}"$'\n'
  done <<< "$raw"
  if [ -n "${unwaived%$'\n'}" ]; then
    echo "secret-scan-tree: MATCH ~ ${pat}" >&2
    printf '%s' "$unwaived" | sed 's/^/    /' >&2
    hits=1
  fi
done

if [ "$hits" -ne 0 ]; then
  echo "secret-scan-tree: ABORT — secret(s) present in $dir; refusing to publish." >&2
  exit 1
fi
echo "secret-scan-tree: clean ($dir)"
exit 0
