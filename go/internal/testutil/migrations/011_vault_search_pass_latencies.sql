-- Per-pass latency split for vault_search. The single `latency_ms`
-- column stays as the round-trip total (pass-1 + pass-2) so existing
-- p50/p95 metrics keep their meaning. The two new columns let the
-- stage-2 trigger investigation distinguish "pass-1 alone is too slow
-- (cap on enriched-prompt size)" from "pass-2 dominates (specificity
-- rerank cost)" — different fixes apply.
--
-- Both NULLABLE so back-compat with rows from before the two-pass
-- rerank landed (chain vault-rag-precision-sharpening, T2). The
-- dispatch arm always sets pass1_latency_ms going forward; pass2
-- stays NULL when the pass is skipped (pass-1 returned 0 or 1 result,
-- so re-ranking would be a no-op).

ALTER TABLE vault_search_invocations ADD COLUMN pass1_latency_ms INTEGER;
ALTER TABLE vault_search_invocations ADD COLUMN pass2_latency_ms INTEGER;
