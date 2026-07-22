-- Task blocker join table.
--
-- Each row is a directed edge: the task identified by `blocked_task_id` is
-- blocked BY the task identified by `blocker_task_id`. A task may carry
-- multiple blocker rows (multi-dep). The task's status='blocked' is set
-- when the first blocker is added; status returns to 'pending' only when
-- ALL blocker rows for that task have been removed.
--
-- The `reason` column is freeform prose explaining the dependency. It is
-- not the canonical "which task" pointer (that's `blocker_task_id`); it
-- carries human-readable context, e.g. "waiting on T17's reattach memo".
--
-- Cascade-on-delete on both sides: removing either the blocked task or the
-- blocker task from `tasks` cascades the join row, so a task_cancel or a
-- task_complete that physically deletes a task (it doesn't, today, but the
-- guard is cheap) cannot strand FK pointers. Practically, tasks are not
-- deleted today; the cascade is belt-and-suspenders against future
-- deletion semantics.
--
-- Pre-existing rows in `tasks` that have status='blocked' before this
-- migration ran retain that status without any join rows. They behave as
-- "manually blocked, no specific blocker tracked" — the legacy single-flag
-- shape is still expressible by calling task_block without a blocker slug.

CREATE TABLE IF NOT EXISTS task_blockers (
    blocked_task_id   INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    blocker_task_id   INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    reason            TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (blocked_task_id, blocker_task_id),
    CHECK (blocked_task_id != blocker_task_id)
);

CREATE INDEX IF NOT EXISTS idx_task_blockers_blocked
    ON task_blockers(blocked_task_id);

CREATE INDEX IF NOT EXISTS idx_task_blockers_blocker
    ON task_blockers(blocker_task_id);
