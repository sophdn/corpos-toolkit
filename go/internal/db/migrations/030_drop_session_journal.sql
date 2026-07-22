-- session_journal retired: the Sessions dashboard page and its
-- /sessions HTTP handler were removed, and the table had no writers
-- after measure-lib was archived (CONVENTIONS.md §"Surface migration
-- order" — T42/T43 cancelled).
DROP INDEX IF EXISTS idx_session_project;
DROP TABLE IF EXISTS session_journal;
