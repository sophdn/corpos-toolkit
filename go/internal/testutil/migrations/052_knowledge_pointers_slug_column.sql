-- Chain `forge-vault-note-schema-rework` T4: knowledge_pointers needs a
-- per-row slug column for vault entries so re-forge with the same slug
-- updates the existing pointer rather than creating a parallel row
-- (bug 1435). The (project_id, source_type, source_ref) unique
-- constraint stays in place for chain/task/bug; vault rows pick up an
-- additional partial UNIQUE INDEX on (source_type, slug) WHERE
-- source_type='vault' so the writer-side slug-keyed lookup in
-- pointers.Upsert is backed by a DB invariant.
--
-- Three steps:
--   1. ADD COLUMN slug (nullable; populated for vault, NULL elsewhere).
--   2. Backfill existing vault rows from source_ref via SUBSTR/INSTR:
--      "decisions/2026-05-20_my-slug.md" → "my-slug"
--   3. CREATE UNIQUE INDEX (partial) on (slug) WHERE source_type='vault'.
--
-- Pre-flight verified on the live mcp-servers DB: zero duplicate-slug
-- pairs exist in current vault rows post-chain-600 hygiene cleanup, so
-- the partial unique index applies without conflicts.

-- Step 1: add the column.
ALTER TABLE knowledge_pointers ADD COLUMN slug TEXT;

-- Step 2: backfill from source_ref for existing vault rows.
-- source_ref shape: "<subdir>/<date>_<slug>.md" for dated entries OR
-- "<subdir>/<slug>.md" / "<subdir>/<slug>.md" for legacy non-dated.
-- The CASE handles both: if char-11 is '_' and chars-5/8 are '-' (date
-- prefix YYYY-MM-DD_), strip the date prefix and the .md suffix; else
-- just strip the .md suffix.
UPDATE knowledge_pointers
SET slug = (
    WITH file_part AS (
        SELECT SUBSTR(source_ref, INSTR(source_ref, '/') + 1) AS part
    )
    SELECT
        CASE
            WHEN SUBSTR(part, 11, 1) = '_'
              AND SUBSTR(part, 5, 1) = '-'
              AND SUBSTR(part, 8, 1) = '-'
            THEN SUBSTR(part, 12, LENGTH(part) - 14)
            ELSE SUBSTR(part, 1, LENGTH(part) - 3)
        END
    FROM file_part
)
WHERE source_type = 'vault';

-- Step 3: enforce per-(source_type='vault') slug uniqueness at the DB.
-- Partial index so non-vault rows are unaffected; NULL slug values are
-- skipped per SQLite UNIQUE-on-NULL semantics (multiple NULLs are
-- allowed), which is the right behavior for non-vault rows.
CREATE UNIQUE INDEX vault_pointer_slug_uniq
    ON knowledge_pointers (slug)
    WHERE source_type = 'vault';
