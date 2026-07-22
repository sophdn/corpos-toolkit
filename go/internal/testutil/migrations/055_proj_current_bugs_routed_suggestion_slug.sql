-- agent-suggestion-box T8 — extend proj_current_bugs to surface
-- bugs.routed_suggestion_slug on the read path.
--
-- Migration 054 added routed_suggestion_slug to the canonical `bugs`
-- table; this migration mirrors it onto the proj_current_bugs read
-- projection so GET /bugs and bug_list / bug_read (which read from the
-- projection per agent-first-substrate T4) surface the field too.
--
-- The Go-side projection helpers in go/internal/projections/bugs.go
-- get extended in the same commit so RebuildFromEmpty + refreshBugRow
-- write the new column. The DEFAULT '' clause matches the bugs-table
-- shape: empty string, not NULL, so caller JSON parsers don't need a
-- third nullability path.

ALTER TABLE proj_current_bugs ADD COLUMN routed_suggestion_slug TEXT NOT NULL DEFAULT '';

-- Backfill the new column from the canonical bugs table for existing
-- rows. The DEFAULT clause covers the ALTER itself, but the explicit
-- UPDATE picks up any bugs that already have a non-empty
-- routed_suggestion_slug from earlier forge_edit calls landing between
-- migration 054 and this one.
UPDATE proj_current_bugs
SET routed_suggestion_slug = (
    SELECT b.routed_suggestion_slug
    FROM bugs b
    WHERE b.project_id = proj_current_bugs.project_id
      AND b.slug = proj_current_bugs.slug
)
WHERE EXISTS (
    SELECT 1 FROM bugs b
    WHERE b.project_id = proj_current_bugs.project_id
      AND b.slug = proj_current_bugs.slug
      AND b.routed_suggestion_slug != ''
);
