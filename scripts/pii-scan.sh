#!/usr/bin/env bash
# pii-scan.sh — corpos-gate custom guard: block secrets / PII from entering the repo.
#
# Scans STAGED, newly-ADDED lines only (git diff --cached), so pre-existing
# content and unstaged work are never flagged — only what this commit introduces.
#
#   - Secret patterns (private keys + provider token formats) are ALWAYS on.
#     They are deliberately high-precision so the guard doesn't cry wolf and
#     train everyone to reach for --no-verify.
#   - A repo-local `.pii-denylist` (one grep -E regex per line, `#` comments
#     ignored) adds project-specific PII terms — real names, personal emails,
#     campaign proper nouns. Optional: absent = that dimension is simply off.
#
# False positive? Append the marker `pii-allow` to the offending line, and the
# guard skips it.
#
# Exit 0 = clean (or nothing to scan); exit 1 = a match was found → commit blocked.
set -euo pipefail

git rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 0
root="$(git rev-parse --show-toplevel)"
cd "$root"

# Newly-added lines from added/copied/modified files, minus the `+++` file headers.
# The guard's own config files are excluded so a denylist never matches itself.
staged_added="$(git diff --cached -U0 --diff-filter=ACM -- . \
  ':(exclude).pii-denylist' ':(exclude).pii-allow' ':(exclude).publish-scrub-map' 2>/dev/null \
  | grep -E '^\+' | grep -Ev '^\+\+\+ ' || true)"
[ -n "$staged_added" ] || exit 0

# Lines the author explicitly waived.
scan="$(printf '%s\n' "$staged_added" | grep -Ev 'pii-allow' || true)"
[ -n "$scan" ] || exit 0

# High-precision secret patterns. Narrow on purpose — provider token shapes and
# private-key headers essentially never appear by accident.
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

hits=""
for pat in "${secret_patterns[@]}"; do
  m="$(printf '%s\n' "$scan" | grep -En -- "$pat" || true)"
  [ -n "$m" ] && hits="${hits}"$'\n'"[secret ~ ${pat}]"$'\n'"${m}"
done

# Project-defined PII denylist (regex per line, `#` comments and blanks ignored).
if [ -f .pii-denylist ]; then
  while IFS= read -r term; do
    [ -z "$term" ] && continue
    case "$term" in \#*) continue ;; esac
    m="$(printf '%s\n' "$scan" | grep -Ein -- "$term" || true)"
    [ -n "$m" ] && hits="${hits}"$'\n'"[pii ~ ${term}]"$'\n'"${m}"
  done < .pii-denylist
fi

if [ -n "$hits" ]; then
  {
    echo "pii-scan: possible secret / PII in staged changes:"
    printf '%s\n' "$hits"
    echo
    echo "If this is a false positive: append 'pii-allow' to the line, or refine .pii-denylist."
  } >&2
  exit 1
fi
exit 0
