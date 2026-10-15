-- otag schema v2: run core.
--   * harness is no longer a CHECK enum: it is validated against the adapter registry in Go,
--     so a new harness (or the test harness "fake") needs no table rebuild.
--   * runs.status gains 'idle' (harness alive, turn finished, waiting for input).
--   * sessions gain tmux_window and aux_panes (server/sidecar panes started for the run).
-- Tables are rebuilt with the standard create/copy/drop/rename procedure; the migration
-- runner disables foreign keys for the migration and runs foreign_key_check before commit.

DROP VIEW v_profile_stats;
DROP VIEW v_run_metrics;

-- ---------------------------------------------------------------- accounts

CREATE TABLE new_accounts (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    harness     TEXT    NOT NULL CHECK (harness <> ''),
    config_dir  TEXT    NOT NULL DEFAULT '',
    command     TEXT    NOT NULL DEFAULT '',
    env         TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(env)),
    config      TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(config)),
    config_hash TEXT    NOT NULL,
    retired_at  TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO new_accounts SELECT id, name, harness, config_dir, command, env, config, config_hash, retired_at,
                                created_at, updated_at FROM accounts;
DROP TABLE accounts;
ALTER TABLE new_accounts RENAME TO accounts;
CREATE TRIGGER accounts_touch AFTER UPDATE ON accounts FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE accounts SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;

-- ---------------------------------------------------------------- profile_revisions

CREATE TABLE new_profile_revisions (
    id          INTEGER PRIMARY KEY,
    profile_id  INTEGER NOT NULL REFERENCES profiles (id),
    account_id  INTEGER NOT NULL REFERENCES accounts (id),
    harness     TEXT    NOT NULL CHECK (harness <> ''),
    model       TEXT    NOT NULL DEFAULT '',
    effort      TEXT    NOT NULL DEFAULT '',
    config_hash TEXT    NOT NULL,
    config      TEXT    NOT NULL CHECK (json_valid(config)),
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (profile_id, config_hash),
    UNIQUE (id, profile_id, account_id, harness)
);
INSERT INTO new_profile_revisions SELECT id, profile_id, account_id, harness, model, effort, config_hash, config,
                                         created_at, updated_at FROM profile_revisions;
DROP TABLE profile_revisions;
ALTER TABLE new_profile_revisions RENAME TO profile_revisions;
CREATE TRIGGER profile_revisions_touch AFTER UPDATE ON profile_revisions FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE profile_revisions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;

-- ---------------------------------------------------------------- runs

-- status:       queued -> starting -> idle|running|waiting -> ... -> ended  (also parked, stopping)
-- ended_reason: done | failed | stopped | crashed | exited | launch_failed
--               (phase 3: stalled | quota | superseded); see docs/ADAPTERS.md
CREATE TABLE new_runs (
    id                  TEXT    PRIMARY KEY,
    project_id          INTEGER NOT NULL REFERENCES projects (id),
    task_id             TEXT,
    role                TEXT    NOT NULL CHECK (role IN ('coordinator', 'child', 'review')),
    parent_run_id       TEXT,
    retry_of_run_id     TEXT,
    attempt             INTEGER NOT NULL DEFAULT 1,
    profile_id          INTEGER NOT NULL REFERENCES profiles (id),
    profile_revision_id INTEGER NOT NULL,
    account_id          INTEGER NOT NULL REFERENCES accounts (id),
    harness             TEXT    NOT NULL CHECK (harness <> ''),
    status              TEXT    NOT NULL DEFAULT 'queued' CHECK (status IN (
                            'queued', 'starting', 'idle', 'running', 'waiting', 'parked', 'stopping', 'ended')),
    ended_reason        TEXT,
    exit_code           INTEGER,
    error               TEXT,
    lease_owner         TEXT,
    lease_expires_at    TEXT,
    heartbeat_at        TEXT,
    last_progress_at    TEXT,
    fencing_token       INTEGER NOT NULL DEFAULT 0,
    version             INTEGER NOT NULL DEFAULT 1,
    queued_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at          TEXT,
    first_activity_at   TEXT,
    ended_at            TEXT,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (id, task_id),
    UNIQUE (id, project_id),
    CHECK ((status = 'ended') = (ended_reason IS NOT NULL)),
    FOREIGN KEY (task_id, project_id) REFERENCES tasks (id, project_id),
    FOREIGN KEY (parent_run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (retry_of_run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (retry_of_run_id, task_id) REFERENCES runs (id, task_id),
    FOREIGN KEY (profile_revision_id, profile_id, account_id, harness)
        REFERENCES profile_revisions (id, profile_id, account_id, harness)
);
-- v1 runs that ended without a reason get 'crashed' so the new CHECK holds.
INSERT INTO new_runs SELECT id, project_id, task_id, role, parent_run_id, retry_of_run_id, attempt, profile_id,
                            profile_revision_id, account_id, harness, status,
                            CASE WHEN status = 'ended' THEN coalesce(ended_reason, 'crashed') END,
                            exit_code, error, lease_owner, lease_expires_at, heartbeat_at, last_progress_at,
                            fencing_token, version, queued_at, started_at, first_activity_at, ended_at,
                            created_at, updated_at FROM runs;
DROP TABLE runs;
ALTER TABLE new_runs RENAME TO runs;
CREATE INDEX runs_task ON runs (task_id);
CREATE INDEX runs_project_status ON runs (project_id, status);
CREATE INDEX runs_profile ON runs (profile_id, ended_at);
CREATE INDEX runs_parent ON runs (parent_run_id);
CREATE INDEX runs_lease ON runs (lease_expires_at) WHERE status <> 'ended';
CREATE TRIGGER runs_touch AFTER UPDATE ON runs FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE runs SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;

-- ---------------------------------------------------------------- sessions

CREATE TABLE new_sessions (
    id                 INTEGER PRIMARY KEY,
    run_id             TEXT    NOT NULL REFERENCES runs (id),
    harness            TEXT    NOT NULL CHECK (harness <> ''),
    harness_session_id TEXT,
    status             TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'ended')),
    tmux_session       TEXT,
    tmux_window        TEXT,
    tmux_pane          TEXT,
    aux_panes          TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(aux_panes)),  -- [{role,name,pane,pid}]
    herdr_pane         TEXT,
    server_url         TEXT,
    server_pid         INTEGER,
    pid                INTEGER,
    pid_started_at     TEXT,
    transcript_path    TEXT,
    started_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ended_at           TEXT,
    end_reason         TEXT,
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (id, run_id)
);
INSERT INTO new_sessions (id, run_id, harness, harness_session_id, status, tmux_session, tmux_pane, herdr_pane,
                          server_url, server_pid, pid, pid_started_at, transcript_path, started_at, ended_at,
                          end_reason, created_at, updated_at)
SELECT id, run_id, harness, harness_session_id, status, tmux_session, tmux_pane, herdr_pane, server_url, server_pid,
       pid, pid_started_at, transcript_path, started_at, ended_at, end_reason, created_at, updated_at FROM sessions;
DROP TABLE sessions;
ALTER TABLE new_sessions RENAME TO sessions;
CREATE INDEX sessions_run ON sessions (run_id);
CREATE INDEX sessions_harness_session ON sessions (harness, harness_session_id);
CREATE TRIGGER sessions_touch AFTER UPDATE ON sessions FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE sessions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;

-- ---------------------------------------------------------------- views (unchanged, recreated)

CREATE VIEW v_run_metrics AS
SELECT r.id                                                        AS run_id,
       r.task_id,
       r.role,
       r.status,
       r.ended_reason,
       pr.name                                                     AS project,
       r.profile_id,
       p.name                                                      AS profile,
       a.name                                                      AS account,
       r.harness,
       pv.model,
       pv.effort,
       (julianday(r.started_at) - julianday(r.queued_at)) * 86400.0 AS queue_wait_s,
       (julianday(r.ended_at) - julianday(r.started_at)) * 86400.0  AS duration_s,
       coalesce(u.input_tokens, 0)                                 AS input_tokens,
       coalesce(u.output_tokens, 0)                                AS output_tokens,
       coalesce(u.cache_read_tokens, 0)                            AS cache_read_tokens,
       coalesce(u.cache_write_tokens, 0)                           AS cache_write_tokens,
       coalesce(u.reasoning_tokens, 0)                             AS reasoning_tokens,
       u.cost_usd,
       coalesce(u.estimated, 0)                                    AS usage_estimated
FROM runs r
JOIN projects pr ON pr.id = r.project_id
JOIN profiles p ON p.id = r.profile_id
JOIN profile_revisions pv ON pv.id = r.profile_revision_id
JOIN accounts a ON a.id = r.account_id
LEFT JOIN (SELECT run_id,
                  sum(input_tokens)       AS input_tokens,
                  sum(output_tokens)      AS output_tokens,
                  sum(cache_read_tokens)  AS cache_read_tokens,
                  sum(cache_write_tokens) AS cache_write_tokens,
                  sum(reasoning_tokens)   AS reasoning_tokens,
                  sum(cost_usd)           AS cost_usd,
                  max(estimated)          AS estimated
           FROM run_usage
           GROUP BY run_id) u ON u.run_id = r.id;

CREATE VIEW v_profile_stats AS
SELECT p.id                                                   AS profile_id,
       p.name                                                 AS profile,
       p.retired_at,
       coalesce(m.runs, 0)                                    AS runs,
       coalesce(m.input_tokens, 0)                            AS input_tokens,
       coalesce(m.output_tokens, 0)                           AS output_tokens,
       coalesce(m.cache_read_tokens, 0)                       AS cache_read_tokens,
       m.cost_usd,
       m.avg_duration_s,
       m.total_duration_s,
       m.avg_queue_wait_s,
       coalesce(rv.reviews, 0)                                AS reviews,
       coalesce(rv.rejected, 0)                               AS rejected,
       CASE WHEN rv.reviews > 0 THEN 1.0 * rv.rejected / rv.reviews END AS reject_rate
FROM profiles p
LEFT JOIN (SELECT profile_id,
                  count(*)               AS runs,
                  sum(input_tokens)      AS input_tokens,
                  sum(output_tokens)     AS output_tokens,
                  sum(cache_read_tokens) AS cache_read_tokens,
                  sum(cost_usd)          AS cost_usd,
                  avg(duration_s)        AS avg_duration_s,
                  sum(duration_s)        AS total_duration_s,
                  avg(queue_wait_s)      AS avg_queue_wait_s
           FROM v_run_metrics
           GROUP BY profile_id) m ON m.profile_id = p.id
LEFT JOIN (SELECT r.profile_id,
                  count(*)                                              AS reviews,
                  sum(CASE WHEN rv.verdict = 'reject' THEN 1 ELSE 0 END) AS rejected
           FROM reviews rv
           JOIN runs r ON r.id = rv.reviewed_run_id
           GROUP BY r.profile_id) rv ON rv.profile_id = p.id;
