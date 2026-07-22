-- Chain `forge-vault-note-schema-rework` (T4 follow-on; landed in T3): the
-- vault-note schema's `project` field was renamed to `scope` and the
-- dispatcher's top-level-project auto-injection into the same-named field
-- was removed. Existing knowledge_pointers vault rows accumulated misroute
-- artifacts under the old shape — bugs 1433 and 1435 documented the
-- mechanics. T1 of chain 601 quantified the impact in
-- process-docs/adhoc/forge-vault-note-usage-2026-05-20.md: 122 of 234
-- vault rows (52%) carried a silent-misroute fingerprint, including 79%
-- of decisions+reference rows stamped with a non-`vault` project_id.
--
-- This migration realigns the project_id stamp on existing vault rows
-- so the post-rename invariants hold:
--   - decisions / reference     → project_id = 'vault'  (cross-project by kind)
--   - learnings/general/<file>  → project_id = 'vault'  (cross-project bucket)
--   - learnings/<scope>/<file>  → project_id = <scope>  (subdir-aligned)
--   - all other vault rows are left alone (roles/, meta/, projects/,
--     scratch/, and the rare root-level entry are out of vault-note's
--     three-subdir routing scope and may legitimately carry whatever
--     project_id the original writer chose).
--
-- Migration 050 already normalized source_ref to the bare
-- "<subdir>/<file>" canonical, so the LIKE patterns below run on a
-- uniform shape.
--
-- Idempotent: the UPDATE statements are no-ops once project_id is
-- already aligned, so re-running the migration is safe.

-- Step 1: cross-project realignment — decisions and reference always
-- route to vault/decisions/ + vault/reference/, regardless of any
-- project context the original forge call passed.
UPDATE knowledge_pointers
SET project_id = 'vault'
WHERE source_type = 'vault'
  AND (source_ref LIKE 'decisions/%' OR source_ref LIKE 'reference/%')
  AND project_id <> 'vault';

-- Step 2: cross-project bucket — learnings/general/ is the explicit
-- cross-project bucket per the schema.
UPDATE knowledge_pointers
SET project_id = 'vault'
WHERE source_type = 'vault'
  AND source_ref LIKE 'learnings/general/%'
  AND project_id <> 'vault';

-- Step 3: subdir-aligned realignment — learnings/<scope>/ rows take
-- their project_id from the scope segment of the source_ref.
-- SQLite SUBSTR + INSTR pattern: extract the segment between
-- "learnings/" and the next "/".
UPDATE knowledge_pointers
SET project_id = SUBSTR(
    source_ref,
    LENGTH('learnings/') + 1,
    INSTR(SUBSTR(source_ref, LENGTH('learnings/') + 1), '/') - 1
)
WHERE source_type = 'vault'
  AND source_ref LIKE 'learnings/%/%'
  AND source_ref NOT LIKE 'learnings/general/%'
  AND project_id <> SUBSTR(
    source_ref,
    LENGTH('learnings/') + 1,
    INSTR(SUBSTR(source_ref, LENGTH('learnings/') + 1), '/') - 1
  );
