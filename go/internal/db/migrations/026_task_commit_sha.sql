-- Add commit_sha to tasks so task_complete can associate the closing
-- commit with the task record. Mirrors bugs.resolved_commit_sha.
-- Bug: task-complete-has-no-commit-sha-param-no-equivalent-of-bug-stamp-sha-for-tasks
ALTER TABLE tasks ADD COLUMN commit_sha TEXT;
