-- otag schema v11: merge/pull request flow primitives (docs/DAEMON.md "MR/PR flow").
--   * projects gain the settings base, branch_prefix, remote and push (projects/<name>.toml;
--     `otag sync` writes them). branch_prefix defaults to 'feature/' for new task branches;
--     a task keeps the branch it got first (tasks.branch).
--   * tasks.base overrides the project's base for that task (frozen once the task has
--     its worktree); tasks.branch is the task branch, stored when its worktree is first
--     created and used from then on (backfilled from the task's first workspace, so
--     existing tasks keep their otag/<task> branch).
--   * tasks.review_profile_id / review_none override the project's review_profile for
--     the task (`--review <profile|none>`).
--   * tasks.push_* record the last `otag task push` (remote, branch, pushed commit, time).
--   * reviews.stale_at marks a verdict that no longer counts: the task was reopened.
-- Only columns are added (no CHECK on an existing column changes), so no table is rebuilt.

ALTER TABLE projects ADD COLUMN base TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN branch_prefix TEXT NOT NULL DEFAULT 'feature/';
ALTER TABLE projects ADD COLUMN remote TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN push TEXT NOT NULL DEFAULT 'coordinator'
    CHECK (push IN ('none', 'coordinator', 'child'));

ALTER TABLE tasks ADD COLUMN base TEXT;
ALTER TABLE tasks ADD COLUMN branch TEXT;
ALTER TABLE tasks ADD COLUMN review_profile_id INTEGER REFERENCES profiles (id);
ALTER TABLE tasks ADD COLUMN review_none INTEGER NOT NULL DEFAULT 0
    CHECK (review_none IN (0, 1) AND NOT (review_none = 1 AND review_profile_id IS NOT NULL));
ALTER TABLE tasks ADD COLUMN push_remote TEXT;
ALTER TABLE tasks ADD COLUMN push_branch TEXT;
ALTER TABLE tasks ADD COLUMN push_sha TEXT;
ALTER TABLE tasks ADD COLUMN pushed_at TEXT;

-- Existing tasks keep the branch their worktree was created on.
UPDATE tasks SET branch = (SELECT w.branch FROM workspaces w
                           WHERE w.task_id = tasks.id AND w.mode = 'worktree' AND w.branch IS NOT NULL
                           ORDER BY w.id LIMIT 1)
WHERE branch IS NULL;

ALTER TABLE reviews ADD COLUMN stale_at TEXT;
