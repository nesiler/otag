-- otag schema v8: messaging (phase 4, docs/MESSAGES.md).
--   * messages: one row per message. client_msg_id is the durable request identity
--     ('req:<request-id>' or a random id); the sender is verified by the CLI (run token)
--     or is the human/system. An ask (`otag ask`) is a question with ask = 1 that put its
--     task in waiting; at most one ask per task is open (messages_one_open_ask).
--   * message_deliveries: one row per recipient with its own state machine:
--       queued -> dispatched -> delivered -> read, plus outcome_unknown, expired, dead.
--     A dispatch carries a random attempt nonce (shared by the deliveries one attempt
--     carries); the attempt is settled by that nonce only. channel, receipt_ref (the
--     harness's id for the attempt) and proof/proof_ref (how delivery was proven) are kept.
--   * message_proofs: correlated proofs (receipt refs) that arrived before the attempt's
--     receipt was stored; settling the attempt consumes them.
-- The v1 messages table was never written by released code. Its rows are kept with
-- their ids (notifications.message_id points at them) as closed history: undelivered
-- ones become expired deliveries, so nothing old is ever delivered.

CREATE TABLE new_messages (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id),
    client_msg_id TEXT    NOT NULL UNIQUE,
    kind          TEXT    NOT NULL CHECK (kind IN ('question', 'info', 'answer', 'command')),
    sender_kind   TEXT    NOT NULL CHECK (sender_kind IN ('run', 'human', 'system')),
    sender_run_id TEXT,                          -- set iff sender_kind = 'run'
    sender_actor  TEXT    NOT NULL DEFAULT '',   -- run:<id> | user:<name> | daemon
    task_id       TEXT,                          -- the task the message is about
    reply_to_id   INTEGER REFERENCES messages (id),
    body          TEXT    NOT NULL CHECK (length(body) > 0),
    ask           INTEGER NOT NULL DEFAULT 0 CHECK (ask IN (0, 1)),
    park          INTEGER NOT NULL DEFAULT 0 CHECK (park IN (0, 1)),  -- `otag ask --park`
    closed_at     TEXT,                          -- questions: answered or closed otherwise
    closed_reason TEXT,                          -- answered | asker_done | asker_failed | superseded | cancelled | v1
    expires_at    TEXT,                          -- queued deliveries expire after this
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (id, project_id),
    CHECK ((sender_kind = 'run') = (sender_run_id IS NOT NULL)),
    CHECK (ask = 0 OR (kind = 'question' AND sender_kind = 'run' AND task_id IS NOT NULL)),
    CHECK (park = 0 OR ask = 1),
    CHECK (kind <> 'answer' OR reply_to_id IS NOT NULL),
    CHECK ((closed_at IS NULL) = (closed_reason IS NULL)),
    CHECK (closed_at IS NULL OR kind = 'question'),
    FOREIGN KEY (sender_run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (task_id, project_id) REFERENCES tasks (id, project_id)
);
-- v1 rows: an answer without a question (or a second answer) becomes info; questions
-- are closed; an empty body gets a placeholder.
INSERT INTO new_messages (id, project_id, client_msg_id, kind, sender_kind, sender_run_id, sender_actor, task_id,
                          reply_to_id, body, closed_at, closed_reason, created_at, updated_at)
SELECT m.id, m.project_id, 'v1:' || m.id,
       CASE WHEN m.kind = 'answer' AND (m.reply_to_id IS NULL OR EXISTS (
                 SELECT 1 FROM messages o WHERE o.kind = 'answer' AND o.reply_to_id = m.reply_to_id AND o.id < m.id))
            THEN 'info' ELSE m.kind END,
       CASE WHEN m.from_run_id IS NULL THEN 'human' ELSE 'run' END, m.from_run_id, m.from_actor,
       coalesce(m.to_task_id, (SELECT r.task_id FROM runs r WHERE r.id = m.from_run_id)),
       m.reply_to_id, CASE WHEN m.body = '' THEN '(empty)' ELSE m.body END,
       CASE WHEN m.kind = 'question' THEN m.updated_at END, CASE WHEN m.kind = 'question' THEN 'v1' END,
       m.created_at, m.updated_at
FROM messages m;

CREATE TABLE message_deliveries (
    id              INTEGER PRIMARY KEY,
    message_id      INTEGER NOT NULL,
    project_id      INTEGER NOT NULL REFERENCES projects (id),
    -- run: a run (follows its task to the next run if it ends undelivered);
    -- task: whichever run of the task takes it first (its next run when none is live);
    -- role: the project's live coordinator, else the human; human: `otag inbox` outside a run.
    recipient_kind  TEXT    NOT NULL CHECK (recipient_kind IN ('run', 'task', 'role', 'human')),
    recipient       TEXT    NOT NULL,   -- as addressed: <run-id> | <task-id> | coordinator | human
    run_id          TEXT,               -- the run it is (or was last) bound to
    task_id         TEXT,               -- the task it follows (task recipients, the run's task)
    state           TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN (
                        'queued', 'dispatched', 'delivered', 'read', 'outcome_unknown', 'expired', 'dead')),
    attempt_nonce   TEXT,               -- the current (or last) attempt; shared by one attempt's deliveries
    attempts        INTEGER NOT NULL DEFAULT 0,
    channel         TEXT,               -- adapter.Chan*: how the current attempt goes in
    injected        INTEGER NOT NULL DEFAULT 0 CHECK (injected IN (0, 1)),  -- the channel injects a prompt
    receipt_ref     TEXT,               -- the harness's id for the attempt (Codex turn id, OpenCode msg id)
    proof           TEXT,               -- marker | receipt | channel | cli | v1
    proof_ref       TEXT,               -- the event id, ref or nonce that proved it
    next_attempt_at TEXT,               -- not before (after a definite not-sent failure)
    last_error      TEXT,
    dispatched_at   TEXT,
    delivered_at    TEXT,
    read_at         TEXT,
    closed_at       TEXT,               -- expired or dead
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (message_id, recipient_kind, recipient),
    CHECK (recipient_kind <> 'run' OR run_id IS NOT NULL),
    CHECK (recipient_kind <> 'task' OR task_id IS NOT NULL),
    CHECK (recipient_kind <> 'human' OR run_id IS NULL),
    CHECK (state NOT IN ('dispatched', 'outcome_unknown') OR (attempt_nonce IS NOT NULL AND channel IS NOT NULL AND run_id IS NOT NULL)),
    CHECK (state <> 'delivered' OR channel IS NOT NULL),
    FOREIGN KEY (message_id, project_id) REFERENCES messages (id, project_id),
    FOREIGN KEY (run_id, project_id) REFERENCES runs (id, project_id),
    FOREIGN KEY (task_id, project_id) REFERENCES tasks (id, project_id)
);
INSERT INTO message_deliveries (message_id, project_id, recipient_kind, recipient, run_id, task_id, state, channel,
                                proof, last_error, delivered_at, read_at, closed_at, created_at, updated_at)
SELECT m.id, m.project_id,
       CASE WHEN m.to_run_id IS NOT NULL THEN 'run' WHEN m.to_task_id IS NOT NULL THEN 'task'
            WHEN m.to_actor IN ('coordinator', 'coord') THEN 'role' ELSE 'human' END,
       CASE WHEN m.to_run_id IS NOT NULL THEN m.to_run_id WHEN m.to_task_id IS NOT NULL THEN m.to_task_id
            WHEN m.to_actor IN ('coordinator', 'coord') THEN 'coordinator' ELSE 'human' END,
       m.to_run_id, coalesce(m.to_task_id, (SELECT r.task_id FROM runs r WHERE r.id = m.to_run_id)),
       CASE m.status WHEN 'delivered' THEN 'delivered' WHEN 'acked' THEN 'read' WHEN 'answered' THEN 'read'
                     ELSE 'expired' END,
       CASE WHEN m.status IN ('delivered', 'acked', 'answered') THEN 'v1' END,
       CASE WHEN m.status IN ('delivered', 'acked', 'answered') THEN 'v1' END,
       CASE WHEN m.status IN ('pending', 'cancelled') THEN 'schema v1 message, never delivered' END,
       m.delivered_at, coalesce(m.acked_at, m.answered_at),
       CASE WHEN m.status IN ('pending', 'cancelled') THEN m.updated_at END,
       m.created_at, m.updated_at
FROM messages m;
DROP TABLE messages;
ALTER TABLE new_messages RENAME TO messages;

CREATE INDEX messages_reply ON messages (reply_to_id);
CREATE INDEX messages_task ON messages (task_id, id);
CREATE INDEX messages_sender ON messages (sender_run_id, id);
CREATE UNIQUE INDEX messages_one_open_ask ON messages (task_id) WHERE ask = 1 AND closed_at IS NULL;
CREATE UNIQUE INDEX messages_one_answer ON messages (reply_to_id) WHERE kind = 'answer';
CREATE TRIGGER messages_touch AFTER UPDATE ON messages FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE messages SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;

CREATE INDEX message_deliveries_message ON message_deliveries (message_id);
CREATE INDEX message_deliveries_open_run ON message_deliveries (run_id, state)
    WHERE state IN ('queued', 'dispatched', 'delivered', 'outcome_unknown');
CREATE INDEX message_deliveries_open_task ON message_deliveries (task_id, state)
    WHERE state IN ('queued', 'dispatched', 'delivered', 'outcome_unknown');
CREATE INDEX message_deliveries_open_project ON message_deliveries (project_id, recipient_kind, state)
    WHERE state IN ('queued', 'dispatched', 'delivered', 'outcome_unknown');
CREATE INDEX message_deliveries_nonce ON message_deliveries (attempt_nonce) WHERE attempt_nonce IS NOT NULL;
CREATE INDEX message_deliveries_receipt ON message_deliveries (run_id, receipt_ref) WHERE receipt_ref IS NOT NULL;
CREATE TRIGGER message_deliveries_touch AFTER UPDATE ON message_deliveries FOR EACH ROW
    WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE message_deliveries SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;

CREATE TABLE message_proofs (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects (id),
    run_id     TEXT    NOT NULL,
    ref        TEXT    NOT NULL,
    event_id   INTEGER REFERENCES events (id),
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (run_id, ref),
    FOREIGN KEY (run_id, project_id) REFERENCES runs (id, project_id)
);
CREATE TRIGGER message_proofs_touch AFTER UPDATE ON message_proofs FOR EACH ROW WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE message_proofs SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE rowid = NEW.rowid; END;
