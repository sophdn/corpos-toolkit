-- Normalize bugs.surface and bugs.tags to comma-delimited form.
--
-- Earlier forge writes that received list-shaped surface/tags joined
-- on "\n- " (the OptionalStringOrList AsJoined convention). After the
-- schema was tightened to optional_string, new writes coerce-join on
-- ",", but rows persisted before the tighten still carry the "\n- "
-- pattern. Filter consumers (bug_list surface tokenization) split on
-- "," and won't match individual tags on the stale rows.
--
-- This migration replaces the "\n- " separator with "," in-place.
-- char(10) || '- ' is SQLite's portable way to write the literal
-- newline-dash-space sequence. The LIKE-guarded WHERE clauses keep
-- the update idempotent: a second run finds no rows to touch.

UPDATE bugs
SET surface = REPLACE(surface, char(10) || '- ', ',')
WHERE surface LIKE '%' || char(10) || '- %';

UPDATE bugs
SET tags = REPLACE(tags, char(10) || '- ', ',')
WHERE tags LIKE '%' || char(10) || '- %';
