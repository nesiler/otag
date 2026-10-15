-- otag schema v3: at most one run per task that has not ended, so two harnesses can never
-- work in the same task worktree. Older duplicates (impossible through the CLI, but
-- possible in v2) are ended as superseded first.
UPDATE runs SET status = 'ended', ended_reason = 'superseded',
                ended_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), version = version + 1
WHERE status <> 'ended' AND task_id IS NOT NULL
  AND id <> (SELECT r2.id FROM runs r2
             WHERE r2.task_id = runs.task_id AND r2.status <> 'ended'
             ORDER BY r2.attempt DESC, r2.queued_at DESC LIMIT 1);

CREATE UNIQUE INDEX runs_one_active_per_task ON runs (task_id) WHERE status <> 'ended' AND task_id IS NOT NULL;
