-- otag schema v1.
-- Conventions:
--   * timestamps are TEXT, UTC, ISO-8601 with milliseconds ("2026-10-06T12:00:00.000Z");
--     durations are derived with julianday() in the analytics views below.
--   * JSON columns are TEXT with json_valid() checks.
--   * config rows (accounts, profiles, projects) are never deleted: `otag sync` sets retired_at.
--   * status columns are authoritative; every transition also writes an `events` row in the same tx.
--   * updated_at is maintained by triggers at the end of this file.

-- ---------------------------------------------------------------- desired state (synced from TOML)

CREATE TABLE accounts (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    harness     TEXT    NOT NULL CHECK (harness IN ('claude', 'codex', 'opencode', 'agy')),
    config_dir  TEXT    NOT NULL DEFAULT '',
    command     TEXT    NOT NULL DEFAULT '',       -- binary or wrapper; '' = harness default
    env         TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(env)),
    config      TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(config)),  -- full parsed TOML entry
    config_hash TEXT    NOT NULL,
    retired_at  TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE profiles (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    account_id  INTEGER NOT NULL REFERENCES accounts (id),
    model       TEXT    NOT NULL DEFAULT '',
    effort      TEXT    NOT NULL DEFAULT '',
    mode        TEXT    NOT NULL DEFAULT 'interactive' CHECK (mode IN ('interactive', 'headless')),
    tags        TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(tags)),
    env         TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(env)),
    args        TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(args)),
    description TEXT    NOT NULL DEFAULT '',
    config      TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(config)),
    config_hash TEXT    NOT NULL,                  -- covers profile + its account + fallback list
    retired_at  TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX profiles_account ON profiles (account_id);

-- Immutable snapshot of every profile configuration ever synced; runs bind to one.
-- account/harness/model/effort are frozen here so runs can reference them consistently
-- even after the live profile row changes.
CREATE TABLE profile_revisions (
    id          INTEGER PRIMARY KEY,
    profile_id  INTEGER NOT NULL REFERENCES profiles (id),
    account_id  INTEGER NOT NULL REFERENCES accounts (id),
    harness     TEXT    NOT NULL CHECK (harness IN ('claude', 'codex', 'opencode', 'agy')),
    model       TEXT    NOT NULL DEFAULT '',
    effort      TEXT    NOT NULL DEFAULT '',
    config_hash TEXT    NOT NULL,
    config      TEXT    NOT NULL CHECK (json_valid(config)),  -- resolved profile + account
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (profile_id, config_hash),
    UNIQUE (id, profile_id, account_id, harness)
);

CREATE TABLE profile_fallbacks (
    profile_id          INTEGER NOT NULL REFERENCES profiles (id),
    position            INTEGER NOT NULL CHECK (position >= 1),
    fallback_profile_id INTEGER NOT NULL REFERENCES profiles (id),
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (profile_id, position),
    UNIQUE (profile_id, fallback_profile_id),
    CHECK (profile_id <> fallback_profile_id)
);

CREATE TABLE projects (
    id                     INTEGER PRIMARY KEY,
    name                   TEXT    NOT NULL UNIQUE,
    path                   TEXT    NOT NULL,
    prefix                 TEXT    NOT NULL,
    coordinator_profile_id INTEGER REFERENCES profiles (id),
    default_profile_id     INTEGER REFERENCES profiles (id),
    review_profile_id      INTEGER REFERENCES profiles (id),
    workspace_mode         TEXT    NOT NULL DEFAULT 'worktree' CHECK (workspace_mode IN ('worktree', 'same')),
    deny                   TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(deny)),
    config                 TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(config)),  -- includes extra fields
    config_hash            TEXT    NOT NULL,
    retired_at             TEXT,
    created_at             TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at             TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE UNIQUE INDEX projects_active_prefix ON projects (prefix) WHERE retired_at IS NULL;

-- ---------------------------------------------------------------- limits and quota

CREATE TABLE account_state (
    account_id           INTEGER PRIMARY KEY REFERENCES accounts (id),
    cooldown_until       TEXT,
    cooldown_reason      TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT,
    last_error_at        TEXT,
    last_success_at      TEXT,
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE profile_state (
    profile_id           INTEGER PRIMARY KEY REFERENCES profiles (id),
    cooldown_until       TEXT,
    cooldown_reason      TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT,
    last_error_at        TEXT,
    last_success_at      TEXT,
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE quota_snapshots (
    id           INTEGER PRIMARY KEY,
    account_id   INTEGER NOT NULL REFERENCES accounts (id),
    profile_id   INTEGER REFERENCES profiles (id),
    source       TEXT    NOT NULL,                 -- claude_statusline | codex_rollout | codex_app_server | agy_statusline | litellm
    quota_window TEXT    NOT NULL DEFAULT '',      -- e.g. 5h, 7d, monthly
    used_percent REAL,
    used         REAL,
    limit_value  REAL,
    resets_at    TEXT,
    raw          TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(raw)),
    observed_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX quota_snapshots_account ON quota_snapshots (account_id, observed_at);

-- ---------------------------------------------------------------- tasks

CREATE TABLE tasks (
    id                TEXT    PRIMARY KEY,         -- <prefix>-<base36>
    project_id        INTEGER NOT NULL REFERENCES projects (id),
    title             TEXT    NOT NULL,
    body              TEXT    NOT NULL DEFAULT '',
    status            TEXT    NOT NULL DEFAULT 'ready' CHECK (status IN (
                          'draft', 'ready', 'claimed', 'running', 'waiting', 'review', 'done', 'blocked', 'cancelled')),
    status_reason     TEXT    NOT NULL DEFAULT '',
    priority          INTEGER NOT NULL DEFAULT 0,  -- higher first
    profile_id        INTEGER REFERENCES profiles (id),  -- requested profile, optional
    current_run_id    TEXT,                        -- must be a run of this task (FK below)
    version          INTEGER NOT NULL DEFAULT 1,  -- CAS token, bumped on every update
    external_ref      TEXT,
    metadata          TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
    extra_prompt      TEXT    NOT NULL DEFAULT '',
    claimed_by        TEXT,
    claimed_at        TEXT,
    status_changed_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    closed_at         TEXT,
    created_by        TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (id, project_id),
    FOREIGN KEY (current_run_id, id) REFERENCES runs (id, task_id)
);
CREATE INDEX tasks_project_status ON tasks (project_id, status, priority DESC, created_at);
CREATE INDEX tasks_status ON tasks (status);
CREATE INDEX tasks_external_ref ON tasks (external_ref) WHERE external_ref IS NOT NULL;

-- Edge (task_id -> depends_on_id) of a given kind:
--   blocks:    task_id cannot start until depends_on_id is done/cancelled
--   review_of: task_id reviews depends_on_id
--   parent:    depends_on_id is the parent of task_id
--   related:   informational
CREATE TABLE task_deps (
    task_id       TEXT NOT NULL REFERENCES tasks (id),
    depends_on_id TEXT NOT NULL REFERENCES tasks (id),
    kind          TEXT NOT NULL CHECK (kind IN ('blocks', 'review_of', 'parent', 'related')),
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (task_id, depends_on_id, kind),
    CHECK (task_id <> depends_on_id)
);
CREATE INDEX task_deps_reverse ON task_deps (depends_on_id, kind);

-- ---------------------------------------------------------------- runs

-- Composite FKs keep a run consistent: its task belongs to its project, its revision
-- belongs to its profile/account/harness, parent and previous attempt share its project.
-- (Composite FKs with a NULL column are not enforced, e.g. task_id of a coordinator.)
CREATE TABLE runs (
    id                  TEXT    PRIMARY KEY,
    project_id          INTEGER NOT NULL REFERENCES projects (id),
    task_id             TEXT,                              -- NULL for coordinators
    role                TEXT    NOT NULL CHECK (role IN ('coordinator', 'child', 'review')),
    parent_run_id       TEXT,
    retry_of_run_id     TEXT,                              -- previous attempt (retry or fallback)
    attempt             INTEGER NOT NULL DEFAULT 1,
    profile_id          INTEGER NOT NULL REFERENCES profiles (id),
    profile_revision_id INTEGER NOT NULL,
    account_id          INTEGER NOT NULL REFERENCES accounts (id),
    harness             TEXT    NOT NULL CHECK (harness IN ('claude', 'codex', 'opencode', 'agy')),
    status             TEXT    NOT NULL DEFAULT 'queued' CHECK (status IN (
                            'queued', 'starting', 'running', 'waiting', 'parked', 'stopping', 'ended')),
    ended_reason        TEXT,   -- done | failed | crashed | stalled | quota | cancelled | superseded ...
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
    FOREIGN KEY (task_id, project_id) REFERENCES tasks (id, project_id),
    FOREIGN KEY (parent_run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (retry_of_run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (retry_of_run_id, task_id) REFERENCES runs (id, task_id),
    FOREIGN KEY (profile_revision_id, profile_id, account_id, harness)
        REFERENCES profile_revisions (id, profile_id, account_id, harness)
);
CREATE INDEX runs_task ON runs (task_id);
CREATE INDEX runs_project_status ON runs (project_id, status);
CREATE INDEX runs_profile ON runs (profile_id, ended_at);
CREATE INDEX runs_parent ON runs (parent_run_id);
CREATE INDEX runs_lease ON runs (lease_expires_at) WHERE status <> 'ended';

-- One run may span several harness sessions (e.g. Claude /clear issues a new session id).
CREATE TABLE sessions (
    id                 INTEGER PRIMARY KEY,
    run_id             TEXT    NOT NULL REFERENCES runs (id),
    harness            TEXT    NOT NULL CHECK (harness IN ('claude', 'codex', 'opencode', 'agy')),
    harness_session_id TEXT,
    status             TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'ended')),
    tmux_session       TEXT,
    tmux_pane          TEXT,
    herdr_pane         TEXT,
    server_url         TEXT,
    server_pid         INTEGER,
    pid                INTEGER,
    pid_started_at     TEXT,   -- with pid, guards against pid reuse
    transcript_path    TEXT,
    started_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ended_at           TEXT,
    end_reason         TEXT,
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (id, run_id)
);
CREATE INDEX sessions_run ON sessions (run_id);
CREATE INDEX sessions_harness_session ON sessions (harness, harness_session_id);

CREATE TABLE workspaces (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects (id),
    task_id     TEXT,
    run_id      TEXT,
    mode        TEXT    NOT NULL CHECK (mode IN ('worktree', 'same')),
    path        TEXT    NOT NULL,
    branch      TEXT,
    base_ref    TEXT,
    base_sha    TEXT,
    head_sha    TEXT,
    status      TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'kept', 'cleaned', 'failed')),
    cleanup     TEXT    NOT NULL DEFAULT 'on_merge',  -- on_merge | on_done | never
    cleaned_at  TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (task_id, project_id) REFERENCES tasks (id, project_id),
    FOREIGN KEY (run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (run_id, task_id) REFERENCES runs (id, task_id)
);
CREATE INDEX workspaces_task ON workspaces (task_id);
CREATE INDEX workspaces_run ON workspaces (run_id);

CREATE TABLE run_outputs (
    id         INTEGER PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs (id),
    kind       TEXT NOT NULL CHECK (kind IN ('commit', 'summary', 'artifact', 'diff_stat', 'test_result')),
    value      TEXT NOT NULL DEFAULT '',   -- sha, summary text, path ...
    data       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(data)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX run_outputs_run ON run_outputs (run_id, kind);

CREATE TABLE run_usage (
    id                 INTEGER PRIMARY KEY,
    run_id             TEXT    NOT NULL REFERENCES runs (id),
    session_id         INTEGER,                      -- a session of run_id (FK below)
    model             TEXT    NOT NULL DEFAULT '',  -- model actually reported
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL,
    source             TEXT    NOT NULL,             -- transcript | rollout | sse | litellm | estimate
    estimated          INTEGER NOT NULL DEFAULT 0 CHECK (estimated IN (0, 1)),
    dedupe_key         TEXT    UNIQUE,
    observed_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (session_id, run_id) REFERENCES sessions (id, run_id)
);
CREATE INDEX run_usage_run ON run_usage (run_id);

-- Profiles are not stored here: they derive from the reviewed/reviewer runs, so they
-- cannot disagree with them.
CREATE TABLE reviews (
    id               INTEGER PRIMARY KEY,
    review_task_id   TEXT    REFERENCES tasks (id),     -- the review task (review_of edge)
    reviewed_task_id TEXT    NOT NULL REFERENCES tasks (id),
    reviewed_run_id  TEXT,                              -- a run of reviewed_task_id
    reviewer_run_id  TEXT,                              -- a run of review_task_id
    reviewer_actor   TEXT    NOT NULL DEFAULT '',       -- when a human reviews
    verdict          TEXT    NOT NULL CHECK (verdict IN ('accept', 'reject')),
    reason           TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (reviewed_run_id, reviewed_task_id) REFERENCES runs (id, task_id),
    FOREIGN KEY (reviewer_run_id, review_task_id) REFERENCES runs (id, task_id),
    CHECK (reviewer_run_id IS NULL OR review_task_id IS NOT NULL)
);
CREATE INDEX reviews_reviewed_task ON reviews (reviewed_task_id);
CREATE INDEX reviews_reviewed_run ON reviews (reviewed_run_id, verdict);

-- ---------------------------------------------------------------- messaging

CREATE TABLE messages (
    id                INTEGER PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects (id),
    kind              TEXT    NOT NULL CHECK (kind IN ('question', 'info', 'answer', 'command')),
    from_run_id       TEXT,
    from_actor        TEXT    NOT NULL DEFAULT '',  -- human | coordinator | daemon when no run
    to_run_id         TEXT,
    to_task_id        TEXT,
    to_actor          TEXT    NOT NULL DEFAULT '',
    reply_to_id       INTEGER REFERENCES messages (id),
    body              TEXT    NOT NULL,
    park              INTEGER NOT NULL DEFAULT 0 CHECK (park IN (0, 1)),
    status            TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN (
                          'pending', 'delivered', 'acked', 'answered', 'cancelled')),
    delivery_attempts INTEGER NOT NULL DEFAULT 0,
    delivered_at      TEXT,
    acked_at          TEXT,
    answered_at       TEXT,
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (from_run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (to_run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (to_task_id, project_id) REFERENCES tasks (id, project_id)
);
CREATE INDEX messages_to_run ON messages (to_run_id, status);
CREATE INDEX messages_reply ON messages (reply_to_id);

CREATE TABLE notifications (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER REFERENCES projects (id),
    task_id    TEXT,
    run_id     TEXT,
    message_id INTEGER REFERENCES messages (id),
    kind       TEXT    NOT NULL,
    severity   TEXT    NOT NULL DEFAULT 'info' CHECK (severity IN ('info', 'warn', 'error')),
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    channel    TEXT    NOT NULL DEFAULT 'tui',
    sent_at    TEXT,
    read_at    TEXT,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK ((task_id IS NULL AND run_id IS NULL) OR project_id IS NOT NULL),
    FOREIGN KEY (task_id, project_id) REFERENCES tasks (id, project_id),
    FOREIGN KEY (run_id, project_id) REFERENCES runs (id, project_id)
);
CREATE INDEX notifications_unread ON notifications (read_at, created_at);

-- ---------------------------------------------------------------- events and audit

CREATE TABLE events (
    id          INTEGER PRIMARY KEY,
    source      TEXT    NOT NULL,         -- cli | sync | daemon | hook:<harness>
    dedupe_key  TEXT,                     -- NULL = no dedupe
    kind        TEXT    NOT NULL,         -- task.created | task.status | task.claimed | ...
    entity_type TEXT    NOT NULL,         -- task | run | session | message | registry ...
    entity_id   TEXT    NOT NULL,
    from_status TEXT,
    to_status   TEXT,
    project_id  INTEGER REFERENCES projects (id),
    task_id     TEXT,
    run_id      TEXT,
    actor       TEXT    NOT NULL DEFAULT '',
    data        TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(data)),
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (source, dedupe_key),
    CHECK ((task_id IS NULL AND run_id IS NULL) OR project_id IS NOT NULL),
    FOREIGN KEY (task_id, project_id) REFERENCES tasks (id, project_id),
    FOREIGN KEY (run_id, project_id) REFERENCES runs (id, project_id)
);
CREATE INDEX events_entity ON events (entity_type, entity_id, id);
CREATE INDEX events_task ON events (task_id, id);
CREATE INDEX events_run ON events (run_id, id);
CREATE INDEX events_created ON events (created_at);

-- Untouched hook input; run/session ids are plain text so capture never fails on FKs.
CREATE TABLE raw_hook_payloads (
    id          INTEGER PRIMARY KEY,
    event_id    INTEGER REFERENCES events (id),
    harness     TEXT    NOT NULL,
    hook        TEXT    NOT NULL,
    run_id      TEXT,
    session_ref TEXT,
    payload     TEXT    NOT NULL,
    received_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX raw_hook_payloads_run ON raw_hook_payloads (run_id, id);

-- Idempotency for mutations carrying --request-id: the stored result is replayed.
CREATE TABLE request_log (
    request_id TEXT PRIMARY KEY,
    command    TEXT NOT NULL,
    args_hash  TEXT NOT NULL,
    result     TEXT NOT NULL CHECK (json_valid(result)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- ---------------------------------------------------------------- views

-- Ready tasks: status ready, active project, and no blocker that is still open.
CREATE VIEW ready_tasks AS
SELECT t.*
FROM tasks t
JOIN projects p ON p.id = t.project_id AND p.retired_at IS NULL
WHERE t.status = 'ready'
  AND NOT EXISTS (
      SELECT 1
      FROM task_deps d
      JOIN tasks b ON b.id = d.depends_on_id
      WHERE d.task_id = t.id
        AND d.kind = 'blocks'
        AND b.status NOT IN ('done', 'cancelled'));

-- One row per run with durations (seconds) and summed usage.
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

-- Per profile: runs, tokens, cost, durations and review reject rate.
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

-- Time spent in each status, from the event log. seconds is NULL while still in that status.
CREATE VIEW v_status_durations AS
SELECT entity_type,
       entity_id,
       project_id,
       to_status                                                              AS status,
       created_at                                                             AS entered_at,
       lead(created_at) OVER w                                                AS left_at,
       (julianday(lead(created_at) OVER w) - julianday(created_at)) * 86400.0 AS seconds
FROM events
WHERE to_status IS NOT NULL
WINDOW w AS (PARTITION BY entity_type, entity_id ORDER BY id);

-- ---------------------------------------------------------------- updated_at triggers

CREATE TRIGGER accounts_touch AFTER UPDATE ON accounts FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE accounts SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER profiles_touch AFTER UPDATE ON profiles FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE profiles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER profile_revisions_touch AFTER UPDATE ON profile_revisions FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE profile_revisions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER profile_fallbacks_touch AFTER UPDATE ON profile_fallbacks FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE profile_fallbacks SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER projects_touch AFTER UPDATE ON projects FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE projects SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER account_state_touch AFTER UPDATE ON account_state FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE account_state SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER profile_state_touch AFTER UPDATE ON profile_state FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE profile_state SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER quota_snapshots_touch AFTER UPDATE ON quota_snapshots FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE quota_snapshots SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER tasks_touch AFTER UPDATE ON tasks FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE tasks SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER task_deps_touch AFTER UPDATE ON task_deps FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE task_deps SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER runs_touch AFTER UPDATE ON runs FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE runs SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER sessions_touch AFTER UPDATE ON sessions FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE sessions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER workspaces_touch AFTER UPDATE ON workspaces FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE workspaces SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER run_outputs_touch AFTER UPDATE ON run_outputs FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE run_outputs SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER run_usage_touch AFTER UPDATE ON run_usage FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE run_usage SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER reviews_touch AFTER UPDATE ON reviews FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE reviews SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER messages_touch AFTER UPDATE ON messages FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE messages SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER notifications_touch AFTER UPDATE ON notifications FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE notifications SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER events_touch AFTER UPDATE ON events FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE events SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER raw_hook_payloads_touch AFTER UPDATE ON raw_hook_payloads FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE raw_hook_payloads SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
CREATE TRIGGER request_log_touch AFTER UPDATE ON request_log FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE request_log SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
