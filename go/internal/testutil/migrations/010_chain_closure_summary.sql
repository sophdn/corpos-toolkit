-- Persist the closure narrative passed to `chain_close(summary=...)`.
-- Previously the summary param was silently dropped because the chains
-- table had no column for it; agents would write rich closure narratives
-- and only discover the loss by post-hoc verification (bug #1086).
--
-- The column lands on `chains` directly rather than a sibling
-- `chain_closures` table because chains close at most once in practice;
-- the historical-audit flexibility a sibling table affords doesn't pay
-- for itself here.

ALTER TABLE chains ADD COLUMN closure_summary TEXT NOT NULL DEFAULT '';
