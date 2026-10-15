-- otag schema v10: delivery (phase 5b, docs/DAEMON.md "Delivery").
--   * tasks.status gains 'delivered': a done task whose result was handed over
--     (`otag task deliver`); only done -> delivered. tasks.delivered_at is when; closed_at
--     stays the time the task became done.
--   * ready_tasks treats a delivered blocker as finished, like done.
--   * v_task_delivery: done and delivered tasks (review tasks excluded: they are
--     delivered with the task they review) with the done -> delivered wait.
-- tasks is rebuilt with the standard create/copy/drop/rename procedure (the CHECK
-- changes); ready_tasks reads it and is recreated around the rebuild.

DROP VIEW ready_tasks;

CREATE TABLE new_tasks (
    id                TEXT    PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects (id),
    title             TEXT    NOT NULL,
    body              TEXT    NOT NULL DEFAULT '',
    status            TEXT    NOT NULL DEFAULT 'ready' CHECK (status IN (
                          'draft', 'ready', 'claimed', 'running', 'waiting', 'review', 'done', 'delivered',
                          'blocked', 'cancelled')),
    status_reason     TEXT    NOT NULL DEFAULT '',
    priority          INTEGER NOT NULL DEFAULT 0,
    profile_id        INTEGER REFERENCES profiles (id),
    current_run_id    TEXT,
    version           INTEGER NOT NULL DEFAULT 1,
    external_ref      TEXT,
    metadata          TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
    extra_prompt      TEXT    NOT NULL DEFAULT '',
    claimed_by        TEXT,
    claimed_at        TEXT,
    status_changed_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    closed_at         TEXT,
    delivered_at      TEXT,
    created_by        TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (id, project_id),
    FOREIGN KEY (current_run_id, id) REFERENCES runs (id, task_id),
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);
INSERT INTO new_tasks (id, project_id, title, body, status, status_reason, priority, profile_id, current_run_id,
                       version, external_ref, metadata, extra_prompt, claimed_by, claimed_at, status_changed_at,
                       closed_at, created_by, created_at, updated_at)
SELECT id, project_id, title, body, status, status_reason, priority, profile_id, current_run_id,
       version, external_ref, metadata, extra_prompt, claimed_by, claimed_at, status_changed_at,
       closed_at, created_by, created_at, updated_at
FROM tasks;
DROP TABLE tasks;
ALTER TABLE new_tasks RENAME TO tasks;

CREATE INDEX tasks_project_status ON tasks (project_id, status, priority DESC, created_at);
CREATE INDEX tasks_status ON tasks (status);
CREATE INDEX tasks_external_ref ON tasks (external_ref) WHERE external_ref IS NOT NULL;
CREATE TRIGGER tasks_touch AFTER UPDATE ON tasks FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE tasks SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;

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
        AND b.status NOT IN ('done', 'delivered', 'cancelled'));

-- Done and delivered tasks with the wait between them (seconds; NULL while not
-- delivered). done_at is closed_at, set when the task became done.
CREATE VIEW v_task_delivery AS
SELECT t.id                                                              AS task_id,
       t.project_id,
       p.name                                                            AS project,
       t.title,
       t.status,
       t.closed_at                                                       AS done_at,
       t.delivered_at,
       (julianday(t.delivered_at) - julianday(t.closed_at)) * 86400.0    AS wait_s
FROM tasks t
JOIN projects p ON p.id = t.project_id
WHERE t.status IN ('done', 'delivered')
  AND NOT EXISTS (SELECT 1 FROM task_deps d WHERE d.task_id = t.id AND d.kind = 'review_of');
