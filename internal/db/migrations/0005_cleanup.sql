-- otag schema v5: durable cleanup of ended runs.
--   * runs.cleanup_pending = 1 is set in the transaction that ends a run: its windows
--     and processes still have to be killed. Whoever ended it kills them after the commit
--     and clears the flag once nothing is left; the daemon (and the next start in the
--     same workspace) retries a cleanup that failed or never ran.
--   * runs.cleanup_error is the last cleanup failure (NULL once a cleanup succeeded), and
--     runs.cleanup_after when the daemon tries a failed cleanup again.
--   * sessions.pid_started_at now holds the process's real birth (with the pid it
--     identifies the process). Older rows hold the time the pane was recorded, which
--     identifies nothing: it is cleared.
-- Runs that ended before v5 with a recorded pane or aux window are checked once.
ALTER TABLE runs ADD COLUMN cleanup_pending INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_pending IN (0, 1));
ALTER TABLE runs ADD COLUMN cleanup_error TEXT;
ALTER TABLE runs ADD COLUMN cleanup_after TEXT;
UPDATE sessions SET pid_started_at = NULL WHERE pid_started_at IS NOT NULL;
UPDATE runs SET cleanup_pending = 1
WHERE status = 'ended' AND EXISTS (SELECT 1 FROM sessions s WHERE s.run_id = runs.id
                                     AND (s.tmux_pane IS NOT NULL OR s.aux_panes <> '[]'));
CREATE INDEX runs_cleanup_pending ON runs (cleanup_after) WHERE cleanup_pending = 1;
