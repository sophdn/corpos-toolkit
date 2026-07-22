#!/usr/bin/env bash
# verify_handler_consolidation.sh — post-deploy sanity check for chain
# telemetry-substrate-cleanup T2 (migration 046).
#
# Confirms that:
#   1. New grounding_events rows for vault_search carry pass1_latency_ms.
#   2. New grounding_events rows for kiwix_search carry qwen_fell_back +
#      kiwix_hits_in/out.
#   3. The legacy per-handler tables have stopped receiving new writes
#      since the deploy cutover (last row predates the cutover marker).
#   4. (Optional) backfill coverage on historical rows — surfaces gaps as
#      FYI, not failures.
#
# Usage:
#   scripts/verify_handler_consolidation.sh [<toolkit_db_path>] [<cutover_ts>]
#     toolkit_db_path  default: data/toolkit.db
#     cutover_ts       ISO-8601 UTC timestamp of the migration 046 deploy;
#                      default: 24h ago (assumes the script runs ~1d post-deploy)
#
# Exit codes:
#   0  all checks pass
#   1  a check FAILED (something is wrong with the writer switch)
#   2  bad arguments or DB unreachable
set -euo pipefail

DB="${1:-data/toolkit.db}"
CUTOVER_TS="${2:-$(date -u -d '24 hours ago' '+%Y-%m-%dT%H:%M:%SZ')}"

if [[ ! -r "$DB" ]]; then
    echo "ERROR: DB not readable: $DB" >&2
    exit 2
fi

echo "→ DB: $DB"
echo "→ cutover marker (post-deploy window): $CUTOVER_TS"
echo

failures=0
ok()   { echo "  [OK]   $*"; }
fail() { echo "  [FAIL] $*"; failures=$((failures + 1)); }
info() { echo "  [INFO] $*"; }

q() { sqlite3 "$DB" "$1"; }

echo "1. vault_search grounding_events rows carry pass1_latency_ms post-cutover"
vault_post=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='vault_search' AND created_at >= '$CUTOVER_TS';")
vault_with_p1=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='vault_search' AND created_at >= '$CUTOVER_TS' AND pass1_latency_ms IS NOT NULL;")
if [[ "$vault_post" -eq 0 ]]; then
    info "no vault_search calls since cutover — can't assert pass1 coverage"
elif [[ "$vault_with_p1" -eq "$vault_post" ]]; then
    ok "$vault_with_p1/$vault_post vault_search rows have pass1_latency_ms"
else
    fail "$vault_with_p1/$vault_post vault_search rows have pass1_latency_ms (expected 100%)"
fi

echo
echo "2. kiwix_search grounding_events rows carry qwen_fell_back + hits post-cutover"
kiwix_post=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='kiwix_search' AND created_at >= '$CUTOVER_TS';")
kiwix_with_extras=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='kiwix_search' AND created_at >= '$CUTOVER_TS' AND qwen_fell_back IS NOT NULL AND kiwix_hits_in IS NOT NULL AND kiwix_hits_out IS NOT NULL;")
if [[ "$kiwix_post" -eq 0 ]]; then
    info "no kiwix_search calls since cutover — can't assert handler-column coverage"
elif [[ "$kiwix_with_extras" -eq "$kiwix_post" ]]; then
    ok "$kiwix_with_extras/$kiwix_post kiwix_search rows have all consolidated columns"
else
    fail "$kiwix_with_extras/$kiwix_post kiwix_search rows have all consolidated columns (expected 100%)"
fi

echo
echo "3. Legacy per-handler tables not receiving new writes post-cutover"
if [[ "$(q "SELECT name FROM sqlite_master WHERE type='table' AND name='vault_search_invocations';")" == "vault_search_invocations" ]]; then
    vsi_post=$(q "SELECT COUNT(*) FROM vault_search_invocations WHERE created_at >= '$CUTOVER_TS';")
    if [[ "$vsi_post" -eq 0 ]]; then
        ok "vault_search_invocations: no new rows since cutover"
    else
        fail "vault_search_invocations: $vsi_post rows added since cutover (expected 0; writer may not have switched)"
    fi
else
    info "vault_search_invocations table is gone — migration 047 already applied"
fi
if [[ "$(q "SELECT name FROM sqlite_master WHERE type='table' AND name='kiwix_offload_invocations';")" == "kiwix_offload_invocations" ]]; then
    koi_post=$(q "SELECT COUNT(*) FROM kiwix_offload_invocations WHERE created_at >= '$CUTOVER_TS';")
    if [[ "$koi_post" -eq 0 ]]; then
        ok "kiwix_offload_invocations: no new rows since cutover"
    else
        fail "kiwix_offload_invocations: $koi_post rows added since cutover (expected 0; writer may not have switched)"
    fi
else
    info "kiwix_offload_invocations table is gone — migration 047 already applied"
fi

echo
echo "4. (FYI) Backfill coverage on historical rows"
vault_pre=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='vault_search' AND created_at < '$CUTOVER_TS';" 2>/dev/null || echo 0)
vault_pre_with_p1=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='vault_search' AND created_at < '$CUTOVER_TS' AND pass1_latency_ms IS NOT NULL;" 2>/dev/null || echo 0)
info "vault_search historical backfill: $vault_pre_with_p1/$vault_pre rows have pass1_latency_ms (NULL is acceptable for pre-substrate rows)"
kiwix_pre=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='kiwix_search' AND created_at < '$CUTOVER_TS';" 2>/dev/null || echo 0)
kiwix_pre_with_extras=$(q "SELECT COUNT(*) FROM grounding_events WHERE action='kiwix_search' AND created_at < '$CUTOVER_TS' AND qwen_fell_back IS NOT NULL;" 2>/dev/null || echo 0)
info "kiwix_search historical backfill: $kiwix_pre_with_extras/$kiwix_pre rows have qwen_fell_back (NULL is acceptable for pre-substrate rows)"

echo
echo "5. qwen_invocations is untouched (per-Qwen-call universal table — different granularity)"
qwen_exists=$(q "SELECT name FROM sqlite_master WHERE type='table' AND name='qwen_invocations';")
if [[ "$qwen_exists" == "qwen_invocations" ]]; then
    qwen_count=$(q "SELECT COUNT(*) FROM qwen_invocations;")
    ok "qwen_invocations table exists with $qwen_count rows (deliberately preserved by T2)"
else
    fail "qwen_invocations table is missing — T2 contract violated (table should NOT be touched)"
fi

echo
if [[ "$failures" -eq 0 ]]; then
    echo "All consolidation checks passed."
    exit 0
else
    echo "$failures check(s) FAILED."
    exit 1
fi
