# Messaging (phase 4)

A durable outbox: every message is a `messages` row plus one `message_deliveries` row per
recipient, each with its own state machine. Package `internal/msg` owns both; it never
talks to a harness. Delivery into a live session is claimed in `msg`, done outside any
transaction by an adapter (or a hook's output), and settled and proven back in `msg`.
Every state change is a CAS on `message_deliveries.version` (or `messages.closed_at`)
plus an `events` row in the same `BEGIN IMMEDIATE` transaction; a lost CAS is a no-op.

## Schema (v8, `0008_messaging.sql`)

- **messages**: `kind` question | info | answer | command; sender `run` (verified with
  the run token), `human` or `system`; `task_id` (what it is about); `reply_to_id`;
  `body` (1 byte to 32 KiB); `client_msg_id` UNIQUE (the request identity:
  `req:<request-id>`, else random); `expires_at`.
  - **ask** (`ask = 1`): a question from `otag ask` that put its task in waiting.
    `messages_one_open_ask` allows one open (`closed_at IS NULL`) ask per task. `park = 1`
    for `otag ask --park`.
  - A question closes (`closed_at`, `closed_reason`) when answered, or with
    asker_done | asker_failed | superseded | cancelled.
  - `messages_one_answer`: one answer per question.
- **message_deliveries**: `recipient_kind` / `recipient` as addressed:
  - `run` / `<run-id>`: the run; if it ends undelivered, it follows the run's task.
  - `task` / `<task-id>`: whichever run of the task takes it first (its next run when
    none is live).
  - `role` / `coordinator`: the project's live coordinator run, else the human.
  - `human` / `human`: `otag inbox` outside a run.

  `run_id` is the run the delivery is (or was last) bound to; `task_id` is the task it
  follows. The attempt columns are `attempt_nonce`, `attempts`, `channel`, `injected`,
  `receipt_ref`, `next_attempt_at` and `last_error`. The proof columns are `proof`
  (marker | receipt | channel | cli) and `proof_ref`.
- **message_proofs**: receipt refs seen before the matching attempt stored its receipt
  (dropped by `Expire` after an hour).
- The v1 `messages` rows keep their ids. They become closed history: undelivered rows
  become `expired` deliveries, so nothing old is ever delivered.

## Delivery states

```
queued ──Claim──▶ dispatched ──proof──▶ delivered ──MarkRead──▶ read
  ▲                 │  │  ├─ Settle(NotSent) ─▶ queued (backoff), dead after MaxAttempts
  ▲                 │  │  └─ Settle(NotReady) ─▶ queued (5 s), attempt given back, never dead
  │                 │  └──── Settle(Unknown) / ProofTimeout ─▶ outcome_unknown ─late proof─▶ delivered
  └─ Requeue ◀──────┴─────── (a person: otag send --resend) ◀── outcome_unknown | dead
queued ──MarkRead (otag inbox, channel cli)──▶ read
queued ──Expire──▶ expired (TTL, question closed) | dead (task closed, task-less run gone)
```

- **Nonce.** An attempt has one random nonce, shared by the deliveries it carries.
  Settling touches only the deliveries that carry that nonce. A stale nonce returns
  `Stale` and changes nothing.
- **Serialization.** An injected channel (`adapter.Injected`: mod_submit, api_start,
  api_steer, launch_prompt) is claimed only while no injected delivery of the run is
  `dispatched`. So one injected attempt is in flight per run, until it is proven,
  unknown or requeued. Hook channels are not serialized.
- **Run status.** Injected channels need a run that is `idle` or `running`; the launch
  prompt needs `queued` or `starting`; hook channels need any run that has not gone. A
  run in `waiting` (a harness dialog) never gets injected text. The dispatcher also
  passes the status it planned for (`ClaimInput.Status`; a Claude mod submit only while
  `idle`), checked inside the claim's transaction.
- **Not now vs not sent.** A refusal that only means "not now" (`adapter.ErrNotReady`:
  the Claude mod is not up or has stopped polling, Codex `not_ready`, `busy`,
  `busy_mismatch`, `waiting_dialog`) settles `NotReady`: queued again after 5 s, its
  attempt given back, so it never makes a delivery dead. Only a definite refusal
  (`NotSent`: no run dir, a refused connection, Codex `rejected`/`bad_request`, an
  OpenCode 4xx) counts toward `MaxAttempts`; a target that has gone is swept by
  `Expire`.
- **Proof** is always correlated, never "any next event":
  - The marker `[otag#d<delivery id>]` in a hook payload of the same run (`ScanMarkers`).
    It proves a delivery that is dispatched, outcome_unknown (late proof), or queued
    after a not-sent attempt.
  - A harness ref (`HookResult.Refs`) equal to the attempt's `receipt_ref` or nonce. A ref
    that arrives first is kept, and `Settle(Sent)` with that receipt proves every
    delivery of the attempt before consuming it.
  - The channel itself: hook output produced in time (`Settle(ChannelProved)`), or a Stop
    with `stop_hook_active` for a Stop block (`Prove{Channel}`). The session's latest
    Stop block is proven even after its proof timeout made it unknown
    (`ReasonProofTimeout`); a block a turn end made unknown never is.
  - The launch prompt of a harness without a marker hook or receipt there (OpenCode,
    `adapter.LaunchTurnProver`): the run's turn start (`Prove{Channel: launch_prompt}`),
    a weaker proof.
  - Reading it with `otag inbox`.
- **Never automatic resend.** `outcome_unknown` and `dead` deliveries show in
  `otag needs`; only a person re-queues them (`otag send --resend <delivery>`).

## Ask, answer and the task

1. `otag ask "q"` stores the ask and moves the task running → waiting in one
   transaction, then returns at once. The run stays open.
   - The same question again from the same run returns the open ask (`replayed`).
   - A different question while one is open is `CONFLICT`.
   - An open ask from an earlier run of the task that has gone is closed as
     superseded.
2. `otag reply <ask>` stores the answer and closes the ask. The replier's own copy is
   read, and other queued copies expire.
   - The answer goes to the asking run while it can take messages; once that run has
     gone, it goes to the task.
3. The task goes waiting → running only when the answer reaches the run: proven or read,
   by the task's current run, while that run is `idle` or `running`, with no other ask
   open. An answer whose delivery is unknown leaves the task waiting and shows in
   `otag needs`. A harness dialog that closes leaves the task waiting too while the run's
   ask is open or its answer has not been delivered or read (`AwaitsAnswer`).
4. `task.Transition(id, claimed)` from waiting (`task.ClaimWaiting`) lets the next run take
   over the task once its run is parked. It is `CONFLICT` while a run of the task is still
   live. `otag task claim` still takes only ready tasks.

**Parked.** P4 parks a run by ending it: run status `ended`, ended_reason `parked`; the
task stays `waiting`. The reserved run status `parked` (schema v2) is accepted too:
`ClaimWaiting`, `OpenAsks` and the message routing treat both as gone.

## CLI

The sender is never a flag. Inside a run (`OTAG_RUN_ID`), `OTAG_RUN_TOKEN` must match, and
the sender is that run. Outside a run, the sender is the human (`user:$USER`); `--run` is
refused. A child (role `child` or `review`) may run `ask`, `inbox`, `reply` (only for
messages addressed to its run or task) and `send coord` (only to `coord`). `needs` is not
open to a child. A coordinator run and the human may do everything in their project.

A run's role is a privilege only with its token: inside a run, a coordinator command
needs `OTAG_RUN_TOKEN` of that run (a mismatch is `FORBIDDEN`; `cli.childGate`). A child
cannot pass for the human by dropping `OTAG_RUN_ID`. Without it, the CLI refuses
coordinator commands and the commands that act as their caller (`ask`, `inbox`,
`reply`, `send`, `review`) when `OTAG_RUN_TOKEN` is set or when the working directory is
inside `<data>/worktrees/`. The child's guard refuses otag commands that
change `OTAG_*` in front of them (`OTAG_*=`, `env -i`, `env -u OTAG_*`), and it refuses
`unset`/`export` of `OTAG_*` and bare `OTAG_*=` assignments.

| command | data (`--json` envelope) |
|---|---|
| `otag send <to>[,<to>...] "text" [--kind info\|command\|question] [--task T] [--ttl 2h] [--no-deliver]` | `{message, replayed, attempts, resume:null}` |
| `otag send --resend <delivery>` | `{delivery}` |
| `otag reply <msg-id> "text" [--info] [--no-deliver]` | `{message, replayed, attempts, resume}` (`resume`: P4) |
| `otag ask "q" [--park] [--to coord\|human]` | `{message, replayed, attempts, park, instruction}` |
| `otag inbox [--wait [--timeout 10m]] [--peek] [--all] [--limit N]` | `{messages: [message & {delivery}], timed_out}` |
| `otag needs [--all]` | `{questions, deliveries, blocked, idle, answers}` |

- `<to>` is a run id, a task id, `coord` or `human`.
- `message` is `{id, client_msg_id, project, kind, from{kind, run, task, role, actor},
  task, reply_to, body, ask, park, closed_at, closed_reason, answer_id, expires_at,
  created_at, deliveries[]}`.
- A delivery is `{id, message, to{kind, recipient, run, task}, state, channel, injected,
  nonce, attempts, receipt, proof, proof_ref, next_attempt_at, last_error, dispatched_at,
  delivered_at, read_at, closed_at, version, created_at}`.
- `attempts` is `[{delivery, result: dispatched|delivered|queued|skipped|failed, reason}]`.
- `--request-id` works through `request_log`, and as `client_msg_id = req:<id>` after the
  log is pruned.
- `inbox` marks what it shows as read, unless you pass `--peek` or `--all`. It selects
  and marks in one transaction (`msg.Take`) and shows only what it took, so an entry a
  dispatcher claimed meanwhile is not shown twice. A run that is stopping or has ended
  takes nothing (`CONFLICT`): its messages stay queued for the task's next run. Without a
  project, the human's `inbox` and `needs` cover every project.
- An injected attempt in flight is neither listed nor marked read, so it keeps the run's
  injected slot. If its outcome turns unknown, it shows again.
- `otag needs` lists:
  - open questions to the human or the coordinator;
  - outcome_unknown and dead deliveries;
  - blocked tasks;
  - idle child and review runs with no open ask;
  - answers: waiting tasks whose latest ask is answered, but the answer is neither
    delivered nor read, and no live run of the task can take it, or the answer is
    expired, unknown or dead.
- otag itself sends `system` messages: idle reminders to a run (`nudge:<run>:<n>`) and,
  when a task becomes done, an `info` notice to `coord` (`done:<task>`; the human reads
  it while no coordinator is live). See docs/DAEMON.md, "Park and resume" and "Delivery".
- A finished task (`done`, `delivered`, `cancelled`) takes no new message, and the
  queued deliveries that follow it die (`Expire`). A message to `coord` about it is not
  bound to the task and stays.
- `coord`, `review`, `run resume` and `run stop --park` are registered stubs that return
  `NOT_IMPLEMENTED`. P4 replaces `cli/coord.go`, `cli/review.go` and `cli/resume.go`.

## Contracts for P3 (delivery) and P4 (lifecycle)

All functions are in `internal/msg`. A mutation takes `(ctx, tx, …, msg.Actor{Name, Source})`.

- **Synchronous attempt.** In `cli/msg.go`, set
  `newDispatcher = func(*app) msg.Dispatcher`, where
  `Dispatcher.Attempt(ctx, deliveryID) AttemptResult`. `nil` leaves deliveries queued; that
  is the P1 state, so nothing is delivered live yet.
  - A dispatcher may also implement `msg.RunDispatcher`:
    `AttemptRun(ctx, runID) []AttemptResult`. The CLI prefers it, calling it once per run
    the new message is bound to.
- **Daemon order.** `Due(ctx, q, now, limit) ([]string, error)` returns the runs with
  claimable queued deliveries that are idle or running and have no injected attempt in
  flight. Runs served least recently by an injected attempt come first, then the run with
  the oldest delivery. `DueRuns(ctx, q, DueInput{Now, Limit, Eligible})` drops the runs
  `Eligible` refuses before ordering and limiting: the daemon passes
  `run.Dispatcher.Eligible`, so runs no injected channel can serve (busy Claude, Codex
  hooks mode) never sort first forever and starve the others.
- **Daemon budget.** The dispatch step stops starting attempts with less than 1 s of its
  5 s budget left, and caps each adapter call at what is left: a hung harness server
  cannot hold the tick (a call cut short has an unknown outcome).
- **Claim.** `Claim(ctx, tx, ClaimInput{RunID, Channel, Max, MaxChars, Now, IDs, Status}, a) (*Attempt, error)`.
  - It returns nil when there is nothing to do.
  - `IDs` optionally limits the claim to those deliveries.
  - `Status`, when set, takes nothing unless the run is still in that status.
  - `Attempt{Nonce, RunID, Channel, Items}`. Use `Attempt.Text(maxChars)` (= `Render`) as
    the text, and pass `adapter.Message{ID: <from Nonce>, Nonce, Text}`.
- **Settle.** Call it after the adapter call, in a new transaction:
  `Settle(ctx, tx, SettleInput{Nonce, Outcome: Sent|NotSent|NotReady|Unknown|ChannelProved, Receipt, Error, RetryAt, MaxAttempts}, a) (SettleResult, error)`.
  - `errors.Is(err, adapter.ErrNotReady)` → `NotReady` (check it first: it wraps
    `ErrNotSent`); `errors.Is(err, adapter.ErrNotSent)` → `NotSent`; any other error →
    `Unknown`.
  - A hook output that was produced in time → `ChannelProved`.
  - A hook claims and settles inside its own transaction, each step in a savepoint; a
    failed step (an error or a recovered panic) undoes its claim and leaves no attempt,
    so nothing is printed and a Stop keeps its turn end.
  - Residual window: hook output is printed right after the hook's transaction commits.
    A hook killed in between (the harness's hook timeout) leaves its messages
    dispatched without output; their proof timeout makes them `outcome_unknown`
    (shown in `otag needs`, never re-sent on their own).
- **Proof.** Call it from the hook transaction:
  `Prove(ctx, tx, ProveInput{RunID, Markers: ScanMarkers(prompt), Refs: res.Refs, Channel, EventID, Nonce}, a) ([]int64, error)`.
  - Call it before claiming for the same hook, so a hook never proves its own output.
  - Scan only the prompt or turn-input field: the UserPromptSubmit `prompt`, or the turn
    input. Never scan the whole payload: tool output and transcripts can quote a marker
    without the message having been delivered.
  - Stop blocks. A Stop with `stop_hook_active` proves only the block it continues:
    pass `Channel: adapter.ChanStopBlock` plus that block's `Nonce`. A Stop without
    `stop_hook_active` means the earlier block was not continued (Esc, kill), so call
    `MarkUnknown(ctx, tx, runID, adapter.ChanStopBlock, "", reason, a) ([]int64, error)`.
    That moves the run's dispatched blocks to outcome_unknown; a non-empty nonce limits it
    to one attempt.
  - `Settle(Sent)` consumes an early proof only if the proof was recorded at or after the
    attempt's `dispatched_at`.
- **Timeouts and sweep.**
  `ProofTimeout(ctx, tx, before, channel, limit, a)` moves deliveries to outcome_unknown.
  `Expire(ctx, tx, now, limit, a) (ExpireResult, error)` sweeps TTLs, closed targets and
  old proofs. Neither is scheduled yet: the daemon step is P3's.
- **Reads.** `Inbox(ctx, q, Reader, all, limit)`;
  `MarkRead(ctx, tx, ids, Reader, a)`; `ReaderFor(ctx, q, runID)`;
  `AddressedTo(ctx, q, Reader, msgID)`; `Get`; `GetDelivery`.
- **Asks.** `OpenAsks(ctx, q, AskFilter{ProjectID, TaskID, RunID}) ([]OpenAsk, error)`
  gives `Message.Park`, `Asker`, `AskerStatus`, `AskerEndedReason` and `TaskStatus`.
  - `HasOpenAsk(ctx, q, taskID)`; `HasOpenAskBy(ctx, q, taskID, runID)` counts only
    the asks of run `runID`. P3: when a harness dialog of a run closes (`run/state.go`,
    `from == Waiting`), the task keeps waiting while that run's own ask is open (an ask
    of an earlier run that has gone does not hold it).
  - `CloseAsks(ctx, tx, taskID, msg.ClosedAskerDone|ClosedAskerFailed|ClosedCancelled, a)`.
    P4 calls it from done, fail and cancel.
- **Writers.** `Send`, `Ask`, `Reply`, `Resolve`, `Requeue`, `NeedsOf`; `Render`,
  `Headline`, `Marker`, `ScanMarkers`.
- **Adapter types** (`internal/adapter`), all additive; `Deliverer` is unchanged:
  - `ReceiptDeliverer`, `Receipt{Channel, Ref}` and `ErrNotSent`;
  - the `Chan*` constants and `Injected`;
  - `DeliverySlot{Channel, Continued}`, `HookDeliverer`, `HookResult.Slot` and
    `HookResult.Refs`;
  - `Target.RunDir` and `Message.Nonce`.
- **Resume.** P4 resumes a parked task with
  `task.Transition(ctx, tx, id, task.Claimed, "", 0, actor)` from waiting, then claims the
  answer for the new run with `ChanLaunchPrompt`.
  - The resumed task goes to running only when `Claim(ChanLaunchPrompt)` returned the
    answer. Otherwise the task stays waiting (or goes back to waiting) and shows in
    `otag needs` under answers. Never let the launch make it running without the answer.
