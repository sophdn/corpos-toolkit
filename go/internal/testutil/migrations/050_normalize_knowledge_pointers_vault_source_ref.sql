-- Bug 1469: knowledge_pointers source_ref prefix drift (114 rows used
-- ".claude/vault/<subdir>/<file>", 40 rows used bare "<subdir>/<file>").
-- Picks the bare form as canonical per bug 1469's recommendation:
-- shorter, matches what `Path.relative_to(vault_root)` naturally
-- produces, and matches what the Go forge/indexsync.go writer already
-- emits today. Migration strips the legacy ".claude/vault/" prefix from
-- existing rows. Verified pre-flight: zero unique-constraint conflicts
-- on (project_id, source_type, source_ref) after the strip.
--
-- Companion: crates/knowledge-shared/src/types.rs's validate_source_ref
-- accepts both forms in this commit (transition) and the Go writer
-- normalizer in go/internal/knowledge/pointers/normalize.go is the
-- canonical writer-side gate going forward.

UPDATE knowledge_pointers
SET source_ref = SUBSTR(source_ref, LENGTH('.claude/vault/') + 1)
WHERE source_type = 'vault'
  AND source_ref LIKE '.claude/vault/%';
