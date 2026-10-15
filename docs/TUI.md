# `otag tui`

`otag tui` is a full-screen view of every project, task and run, with a live preview of the
selected agent's pane (phase 5a; plan: [DESIGN.md](DESIGN.md) "Faz 5 planı / 5a"). State
comes from the database, never from the screen. The TUI only reads the database; its
actions (from T2) run the CLI's own command code. The daemon is not required. Status:
T1-T4 are done, so phase 5a is complete.

Stack: `charm.land/bubbletea/v2` v2.1.0, `charm.land/bubbles/v2` v2.2.1,
`charm.land/lipgloss/v2` v2.0.6 (exact versions; most examples online are still v1). No
embedded terminal emulator, no bubblezone.

There are three ways to interact with an agent:
1. **Message box**: `otag send` / `otag reply` (phase 4 delivery, with proof).
2. **Typing mode** in the preview: keys go to the agent with `tmux send-keys`.
3. **Full-screen attach**: the TUI suspends and attaches to otag's tmux server.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Mouse hit-testing | A **pure `layout(m)`** returns z-ordered rectangles; `Update` and `View` both use it. Lip Gloss v2 layers compose the overlays (help; later menus and dialogs). `View.OnMouse` is not used. | bubbletea v2 runs the `OnMouse` result in `go p.Send(cmd())`, so hit messages can arrive out of order, which breaks double-click and drag. Raw `Mouse*Msg` arrive in order, and a pure layout is unit-testable. |
| Preview capture | **Live**: `capture-pane -p -e` (no `-J`, so rows match the pane grid and the cursor). **Scroll-lock history**: `-p -e -J -S -N -E -1`. | Typing mode needs `cursor_x/y` to line up. |
| Actions | The TUI calls the **same CLI command functions in-process** through a small backend in `internal/cli/tui.go`; reads go to package functions directly. | No logic is duplicated: child gate, `--request-id`, delivery after commit and the CLI's human text all stay the same. |
| tmux UI bindings | Applied **at runtime** by `tmux.Client.EnsureUI` before every attach (TUI, `otag run attach`, `otag coord --attach`); never written to a file. Opt out with `set -g @otag_ui off`. | Survives a server restart, needs no file writes, respects the user's own `tmux.conf`. |
| Return keys | **Both `M-q` and `C-q`** in otag's tmux root table, plus a clickable `← otag` in the status bar. | macOS Option-as-Meta is often off; `C-q` is the claude-squad / agent-deck convention. Claude, Codex and OpenCode bind neither. |
| otag's tmux server | `set -g mouse on` (wheel scrollback and the clickable `← otag`). Native text selection while attached needs Option+drag (Fn+drag in Terminal.app). | User decision. |
| Leaving typing mode | **Only `C-q` / `M-q` or a click outside the preview.** Esc, and Esc Esc, always go to the agent. | Claude's double Esc (rewind) keeps working. |
| UI language | English, like the CLI and the docs. | |
| Deliver dialog | "Delete branch if merged" is **unchecked** by default. | |
| Daemon | Not required. The header shows `daemon ●/○`. Without it parked runs are not resumed and queued messages are not retried; the TUI says so instead of doing the daemon's job. | |
| Inside otag's own tmux server | `otag tui` **refuses** to start. | The return key would detach the TUI's own client. |

## Package layout

```
internal/cli/tui.go     cmdTui: flags, refusals, opens the DB, builds tui.Options{Text, Call}, runs tui.Run
internal/tmux/ui.go     PaneView (pane info + capture in one call); T3: EnsureUI, SendKey/SendLiteral/Paste, PopupArgs, PopupSupported
internal/tui/
  tui.go                Options, Outcome, Run (tea.NewProgram, WithContext, WithFPS)
  model.go              root Model, Init/Update, data/liveness/capture loops, message routing
  input.go              key dispatch and actions
  mouse.go              click / double-click / wheel / drag state machine
  layout.go             pure layout: blocks and parts, z-order, hit
  keys.go               key table: the single source for dispatch, footer hints and the help overlay
  view.go               View: blocks side by side, Lip Gloss layers when an overlay is open
  data/                 Snapshot (one read transaction), Watcher (PRAGMA data_version), Liveness
  tree/                 Snapshot → rows; expansion; selection by stable key; row rendering
  preview/              sanitize (SGR only), crop / h-scroll, follow and scroll-lock
  tabs/                 Needs, Inbox, Details/Stats text, Log
  style/                theme (ANSI 16 colours), status glyphs, text fitting
  fixture/, tuitest/    test helpers: a hand-built snapshot; isolated otag environments
  testdata/*.golden
  actions.go            T2: actions through Options.Call, busy flags, outcomes, toasts, Log, quit wait
  dialogs.go            T2: every dialog (a form plus the argv its submit builds)
  msgbox.go, items.go   T2: the message box; the Needs / Inbox items with inline actions
  form/                 T2: the dialog kit (editor, text, area, select, checkbox, list, note, buttons)
  interact.go           T3: typing mode and attach in the model (enter/leave, send, burst, attach flow)
  typing/               T3: Encode (key → send-keys op), CleanPaste, Sender (FIFO, Enter gate, coalescing)
  attach/               T3: Decide (exec or popup), Plan.Command with its stderr
  tuitest/pastecheck/   T3 test helper: an input box with paste detection (the "typist" profile)
  menu.go, polish.go    T4: context menus (disabled reasons), filter, copy, expand all, help scroll
  state.go              T4: <data>/tui/state.json (atomic), the OTAG_TUI_DEBUG log
```

The import direction is `cli → tui`; `tui` never imports `cli`. The backend comes in as
functions in `Options`:

```go
type Options struct {
    DB      *db.DB             // the same handle the backend uses
    Tmux    *tmux.Client       // otag's socket (paths.TmuxSocket/TmuxConf)
    Getenv  func(string) string
    DataDir string
    Project string             // --project: initial selection (else the project of the cwd)
    Call    func(ctx context.Context, args ...string) Outcome         // mutation through the CLI command table
    Text    func(ctx context.Context, args ...string) (string, error) // human output of a read command
    Environ func() []string                                           // T3: the environment of an attach
}
type Outcome struct {
    Data  any            // res.data exactly as --json would carry it
    Human string         // the human output: toast and Log text
    Err   *apperr.Error  // code, message, retryable
}
```

`Text` (T1) runs a read command in-process with a buffer for stdout and stderr and the
shared DB handle, after the child gate, and returns exactly what the command prints.
`Call` (T2) does the same for mutations: `--json`, a `--request-id`, and
`context.WithoutCancel` (a cut-short delivery has an unknown outcome). The TUI passes
the request id (`tui-<16 hex>`): one per action, and a dialog keeps one per set of
arguments, so submitting the same form again after an unclear failure replays instead
of running twice; `Call` adds one only when none is given. `Outcome.Human` is the
command's human text (rendered also under `--json`) followed by its stderr, exactly as
the command line prints it. The in-process commands run with a neutral environment: no
working directory, no `OTAG_PROJECT`, no `OTAG_RUN_*` (the TUI refuses to run inside a
run), so where the TUI was started never scopes or refuses an action. Every action
about a target passes its project as `--project`; the global ones (`inbox`, `notify ack`,
`send --resend`) pass none, like the global Needs and Inbox tabs.

Quitting waits for actions in flight: start, resume, coord and deliver until they end,
the others up to 5 s ("finishing N action(s)… (q again quits now)"); a second `q` or
`ctrl+c` quits at once. `cmdTui` keeps the database open until the calls still running
end, saying so on stderr ("waiting for N action(s) to finish (ctrl+c to stop)"); ctrl+c
or 30 s end the wait, and the process exit then closes the handle.

`cmdTui` refuses:
- `--json`;
- a stdin or stdout that is not a terminal;
- a run (the child gate: `tui` is not a child command);
- `$TMUX` naming otag's own socket (`tmux.SocketPath(socket)`).

When the database schema is newer than the binary (`db.ReadOnly`), the header says
`READ-ONLY` and (T2) every action is disabled.

## Screen

```
┌ otag │ ⚑ 3 needs you │ ✉ 2 │ ⇪ 1 to deliver │ daemon ● │ c2 91% (5h) ────────────────────────────────┐ header (counters clickable)
│ ▾ alpha  strict    3 open · 1 ready  │ 1 Preview │ 2 Details │ 3 Needs 3 │ 4 Inbox 2 │ 5 Stats │ 6 Log │ tabs (clickable)
│     ○ coord sonnet      idle 2h q91% │ ● al-12-r2 · sonnet · running · %7 · FOLLOW                  │ preview title bar
│   ▾ ⇪ Ready to deliver (1)           │ <capture-pane of the selected run, cropped, h-scrollable>    │
│     ▸ ⇪ al-9 Add CSV export  done 3h │                                                              │
│   ▾ ● al-12 Fix login   running 12m  │                                                              │
│       ● r2 sonnet       running 12m  │                                                              │
│       +1 earlier                     │                                                              │
│     · al-13 Docs        ready p2 3h  │                                                              │
│ ▸ beta  light      1 open · 1 ready  │                                                              │
├──────────────────────────────────────┴──────────────────────────────────────────────────────────────┤
├── message ──────────────────────────────────────────────────────────────────────────────────────────┤ message box: a separator,
│ to: al-12-r2 ▾  info ▾  type a message… (Enter send · Alt+Enter newline · ctrl+t recipient)  [Send] │ then 1-6 text rows
└ [q] quit [?] help [!] needs [tab] focus [.] closed …                                     ✓ sent #41 ┘ footer buttons + toast
```

- Below 90 columns: one pane at a time; `Tab` (or the `[tab]` button) swaps tree and
  right pane. Below 60x16: only "terminal too small".
- The divider is a 1-column region; the tree is about 38% wide by default, clamped to
  `[24, width-31]`.

### Root model

```go
type Model struct {
    w, h       int
    focus      focus           // tree | pane (T2-T4: typing, message box, dialog, menu)
    tab        tabs.Tab        // Preview | Details | Needs | Inbox | Stats | Log
    snap       *data.Snapshot  // last good snapshot; never changed in place
    tree       tree.Model      // rows, cursor key, expansion, scroll, showClosed
    prev       preview.Model   // target pane, content, scroll-lock, h-scroll
    cap        …               // capture loop: loop id, busy, still count, scroll-lock fetch
    live       map[string]tmux.Liveness
    divider    int             // tree width; 0 = default
    mouse      mouseState      // last click (region, time, cell), divider drag
    text       map[tabs.Tab]*textView // Details, Stats: key, text, in flight
    log        []string        // Log tab (T2)
    toast      …               // text, severity, expiry
    frame      string          // cached screen, cleared by any visible change
}
```

The focus ring with `Tab` / `shift+Tab` is tree → right pane → message box (narrow mode
swaps tree and right pane on the way).
Dialogs and menus are modal (T2/T4): they own the keyboard, a click outside closes a
menu and is ignored by a dialog. Typing is a sub-mode of the preview (T3).

### Tree rows

Rows come from the snapshot, in this order:

1. **Project rows**: name, guard, open and ready counts. The live coordinator is the
   first child row (`coord`).
2. **"Ready to deliver" group** (`task list --deliverable`), when not empty.
3. **Open tasks** by attention: waiting / blocked / review first, then running / claimed,
   then ready, then draft; priority descending, then id.
4. **Review tasks** nested under the task they review (`review_of` edge), when that task
   is listed.
5. **Runs** under each task: the newest run is always shown; older attempts sit behind a
   `+N earlier` row, and expanding it loads the full run history.
6. **Closed tasks** (done, delivered, cancelled), hidden unless toggled with `.`; a done
   task already in the deliver group is not listed twice.

Selection is kept by a **stable key** (`p:<project>`, `c:<project>`, `g:deliver:<project>`,
`t:<task>`, `r:<run>`, `h:<task>`), so a refresh never moves the cursor. When the selected
row disappears the cursor keeps its index. Default expansion: projects and the deliver
group open; a task open when its newest run is live or it has review tasks.

Each row shows a status glyph and colour (`● running`, `○ idle`, `? waiting on an open
ask`, `⚠ waiting on a harness dialog`, `⏸ parked`, `✕ crashed / failed / dead pane`,
`✓ done / delivered`, `⇪ deliverable`, `⊘ blocked`, `◎ review`, `· ready`, `◌ draft`,
`■ stopped`), the id and title, the profile, and the elapsed time (run: since
`started_at`, an ended run its duration; task: since `status_changed_at`). A live run
also shows its account's worst quota window from 50% (`q91%`, red at the quota threshold
or in cooldown) and `DEAD` when its pane is gone or exited.

### Right tabs

| Tab | Shows | Source |
|---|---|---|
| Preview | Live capture of a run row, a task row's current run, or a project's coordinator; else "no live pane" and why | tmux |
| Details | `run show`, `task show` or `status --project P` output, refetched on each new snapshot | `Text` |
| Needs | `msg.NeedsOf(0)` in groups, each with the command that settles it and inline actions: [Reply] (dialog), [Resend] (`send --resend D`), [Unblock] (`task ready`), [Message] / [Stop] for an idle run, [Resume] | snapshot |
| Inbox | Unread human messages (peeked, never marked read; [Mark read] runs `inbox`, a question has [Reply]) and unread notifications ([Ack], [Ack all]) | snapshot |
| Stats | `otag stats` and `otag quota` output, on demand (`ctrl+r` refreshes) | `Text` |
| Log | The TUI's own action outcomes (T2) | memory |

## Data and refresh

### Snapshot

`data.Load` runs in **one read transaction**, `BeginTx(ReadOnly: true)`: modernc's
`_txlock=immediate` applies only to write transactions, so this is a plain deferred
`BEGIN`, a consistent WAL snapshot that never takes the write lock. It ends right after
the queries, so WAL checkpoints are never blocked. It reads:
- `registry.ListProjects`, `registry.ListProfiles("")`;
- `task.List{Open}`, `task.List{Deliverable}`, and with closed tasks shown
  `task.List{done, delivered, cancelled; Limit 200}`;
- the `review_of` edges (one read-only query on `task_deps`);
- `run.List{Active, Limit 1000}` (coordinators included), the run count and newest run of
  every listed task (an ended newest run is read with `run.Get`), and `run.List{TaskID}`
  for each task whose history is expanded;
- `msg.NeedsOf(0)`, `msg.OpenAsks{}`, `msg.Inbox(human, unread, 50)`;
- `notify.Unread`, `notify.List{unread}`;
- `quota.List{MaxAge 7d}`, `quota.Cooldowns`;
- `daemon.Probe(dataDir)` (after the transaction).

The snapshot is immutable once built.

### Change detection

`data.Watcher` pins one `*sql.Conn` for the TUI's lifetime and reads `PRAGMA data_version`
on it every **250 ms**. It changes whenever any *other* connection commits: the CLI, hooks,
the daemon, and the TUI's own backend writes (other pool connections). The baseline is
read before the first load, so a commit between the two is not missed.

On a change: load a snapshot, at most one in flight (a change seen during a load loads
once more afterwards), and at least **150 ms** after the previous load. Safety nets: a full
reload every **10 s** (ages, quota staleness) and, from T2, right after every action and
after returning from an attach. Measured: a task added by the CLI shows in about 0.2 s.

### Liveness

`run.CheckLive` (one `list-panes -a`) every **2 s** and on each new snapshot, merged into
the rows at render time.

### Clock

A 1 s tick redraws only when an elapsed time shows seconds (something started or changed
within the last minute); otherwise every 10 s. It does no IO.

### Preview capture

- Only when the Preview tab is visible with a live target pane, not scroll-locked; only
  the selected target is captured.
- **4 Hz**, slowing to **1 Hz** while the terminal has no focus (`View.ReportFocus`) or
  after 8 captures in a row that changed nothing (an idle agent); the first change speeds
  it up again. T3: a burst 40 ms after a typed key (capped at 20/s).
- **One capture at a time**: each capture is scheduled by the previous result. A retarget
  or tab change ends the chain (a loop id): its scheduled capture skips tmux and its result
  is dropped.
- One tmux invocation per capture (`tmux.Client.PaneView`, 1 s timeout):
  `display-message -p -t %P '#{pane_width}\t#{pane_height}\t#{cursor_x}\t#{cursor_y}\t#{cursor_flag}\t#{alternate_on}\t#{pane_in_mode}\t#{pane_dead}' ; capture-pane -p -e -t %P`.
  The TUI resolves the tmux binary once (an absolute `Client.Bin`).
- Content is compared by FNV hash: an unchanged capture draws nothing.
- **Sanitize**: only text and SGR sequences are kept (parsed with `x/ansi`); OSC, cursor
  movement and other controls are dropped; tabs become spaces.
- **Crop**: `ansi.Cut(line, xoff, xoff+w)`. The preview shows the bottom rows of the
  screen's *used* part (down to its last text or the cursor), since agents keep their input
  at the bottom but a short screen starts at the top. The agent window is never resized for
  the preview (otag creates windows at 200x50; an attach resizes it anyway). A fit-to-preview
  key (`z`) was dropped in T4: the preview crops and scrolls instead.
- **Scroll**: wheel-up, `↑` or `PgUp` in the preview enters scroll-lock: the scrollback
  (`-J -S -2000 -E -1`) is fetched once and live updates pause; the title bar shows
  `SCROLL ↑n`. `G`, `End` or scrolling back to the bottom follows live again.

### No busy loops

Every periodic job is a one-shot timer that reschedules itself after its result; the
watcher and the capture do their IO inside the timer (one message per check). During an
attach the event loop is blocked in `ExecProcess`, so at most one timer of each kind is
pending afterwards.

### Rendering cost

The layout blocks tile the screen, so `View` joins their lines; Lip Gloss layers compose
only when an overlay is open. The frame is cached and recomposed only when an update may
have changed it (an unchanged `data_version`, an identical capture or an unchanged
liveness change nothing). The renderer runs at 20 fps (the default 60 wakes up 60 times a
second for nothing). Measured on macOS with 41 tasks and 10 runs: 0.3-0.5% CPU idle with a
still preview (13% before the frame cache, the fps cap and the capture backoff).

### SQLite concurrency

- One `*db.DB` pool serves the reads, the watcher's pinned connection and the backend's
  writes.
- The TUI never writes SQL. Writes happen only in CLI command code (`mutate` / `d.Tx`:
  `BEGIN IMMEDIATE`, busy_timeout 10 s, CAS + events).
- Task edits and cancels (T2) pass `--version <snapshot version>`: a stale form fails with
  `CONFLICT`, the TUI says "changed meanwhile — reloaded" and keeps the form open.

## Message flow

```
Init → load, watch(250ms), full(10s), live(2s), clock(1s)
watch ──▶ watchMsg{changed} ──▶ load ──▶ loadMsg{snap|err} ──▶ tree.Rebuild (keep key); liveness; retarget preview; Details
live tick ──▶ liveMsg{map[runID]Liveness} ──▶ retarget
capture(loop) ──▶ captureMsg ──▶ preview.Set (hash) ; schedule the next capture of the same loop
key / mouse ──▶ action (pure: Update decides) ──▶ T2: dialog | actionCmd ; T3: attachCmd | typing op
actionCmd(key, args) ──▶ Options.Call ──▶ outcomeMsg ──▶ busy[key]=false, toast, log, reload, optional result dialog   (T2)
typing op ──▶ sender channel ; sentMsg{err} ──▶ burst capture                                                        (T3)
attachCmd ──▶ EnsureUI ──▶ tea.ExecProcess ──▶ attachDoneMsg ──▶ ClearScreen + reload + capture                      (T3)
WindowSizeMsg ──▶ layout ; FocusMsg/BlurMsg ──▶ capture rate ; BackgroundColorMsg ──▶ theme (T4)
```

`Update` is pure apart from the `Cmd`s it returns: every side effect (DB, tmux, backend)
runs in a `Cmd`. The side effects are functions in the model (`deps`), so tests drive
`Update` with fakes and fire timers by hand.

## Keyboard and mouse

Every mouse action has a key. The binding table in `keys.go` drives dispatch, the footer
buttons and the `?` overlay.

### Global (not in typing mode, a dialog or a menu)

| Key | Action | Mouse | Step |
|---|---|---|---|
| `q`, `ctrl+c` | quit (no agent is affected) | footer | T1 |
| `?` | help overlay | footer `[?]` | T1 |
| `1`-`6` | tabs Preview / Details / Needs / Inbox / Stats / Log | click tab | T1 |
| `!` | Needs tab | click "⚑ N needs you" (✉: Inbox, ⇪: the deliver group) | T1 |
| `ctrl+r` / `F5` | reload now (and refetch Stats) | — | T1 |
| `ctrl+l` | full redraw | — | T1 |
| `Tab` / `shift+Tab` | focus tree ↔ right pane (narrow: swap) | click the region | T1 |
| `<` / `>` / `=` | narrower / wider / default tree width | drag the divider | T1 |
| `.` | show / hide closed tasks | menu "Show closed tasks" | T1 / T4 |
| `/` | filter the tree (id, title, profile; case-insensitive, matches keep their ancestors, everything expanded meanwhile). Enter keeps it, Esc clears it (also when not editing) | menu "Filter…" | T4 |

### Tree

| Key | Action | Mouse | Step |
|---|---|---|---|
| `↑↓` / `k j`, `PgUp PgDn`, `g G` / `Home End` | move | click a row; the wheel scrolls the tree | T1 |
| `← →` / `h l`, `space` | collapse (or go to the parent) / expand | click `▸/▾` | T1 |
| `Enter` | project, group: toggle; a run, coord, or task with a live run: attach (else toggle) | double-click | T1 / T3 |
| `i` | typing mode into the selected run's pane | click the preview | T3 |
| `m` | message box, recipient = selection | click the message box | T2 |
| `o`, `shift+F10`, `menu` | context menu: the row (tree), the highlighted item (Needs, Inbox), the tab (right pane), the message box (`shift+F10` there) | right-click | T4 |
| `n` | new task (project of the selection) | menu / footer | T2 |
| `e` / `x` | edit / cancel task (confirm + reason) | menu | T2 |
| `R` / `b` | mark ready (`task ready`) / block… (`task block --reason`) | menu | T2 |
| `s` / `S` / `p` / `u` | start run (profile picker) / stop (confirm; offers Park with an open ask) / park / resume | menu / footer | T2 |
| `c` | coordinator: attach if live, else start (profile picker) | menu on a project | T2 / T3 |
| `r` | reply to the open question of the selected task or run (message box in reply mode) | Needs: "Reply" | T2 |
| `v` / `D` | review… (on a review task: the task it reviews) / deliver… (on the "⇪" group: its first task) | menu, "⇪" group | T2 |
| `A` | acknowledge every notification (confirm) | Inbox: [Ack all] | T2 |
| `y` | copy id / attach command (OSC 52, `tea.SetClipboard`); on Needs, Inbox the item's id | menu | T4 |
| `E` | expand every row of the selected project | menu "Expand all" | T4 |
| `Y` | sync the registry (`otag sync`) | menu "Sync registry" | T4 |

### Right pane

| Key | Preview | Text tabs |
|---|---|---|
| `↑↓` / `k j`, `PgUp PgDn` | scroll history (the first scroll up enters scroll-lock) | scroll |
| `g` / `Home` | top of the history | top |
| `G` / `End` | follow live | bottom |
| `← →` / `h l` | horizontal scroll | — |
| `↑↓` / `Enter` on Needs, Inbox | — | select an item / run its first action (T2) |
| `i` (T3) | typing mode | — |
| `Enter` (T3) | attach | — |

Mouse: the wheel over the preview scrolls it, shift+wheel (or a horizontal wheel)
scrolls sideways; T3: double-click attaches.

### Message box (T2)

| Key | Action |
|---|---|
| `Enter` | send |
| `alt+enter`, `shift+enter`, `ctrl+j` | newline |
| `ctrl+k` | cycle kind info → command → question |
| `ctrl+t` | recipient picker: this run / this task / coord |
| `Esc` | back to the tree (text kept) |

Mouse: click the recipient chip for the picker, click `[Send]` to send; the wheel scrolls
the text area. When the selection is a needs question, or `r` was pressed, the box is in
**reply mode**: the chip reads `reply #42 →` and Enter runs `reply 42 -- text`.

### Typing mode (T3)

Only `C-q` / `M-q` and a click outside the preview are intercepted. Everything else,
Esc and Esc Esc included, goes to the agent. A click inside the preview is not
forwarded; a double click there attaches.

### Mouse mechanics

- **Mode**: `View.MouseMode = MouseModeCellMotion` (click, release, wheel, motion while a
  button is held). Hover (`AllMotion`) is opt-in in T4 (`OTAG_TUI_HOVER=1`; there is no global otag.toml);
  it only highlights the row under the pointer.
- **Click and double-click**: a `MouseClickMsg` is hit-tested with the layout, topmost z
  first. A double click is the same region within 400 ms and at most one cell of
  movement; a third click starts over. Right-click opens the context menu (T4); the
  middle button is ignored.
- **Drag**: a press on the divider starts it; `MouseMotionMsg` sets the width (clamped);
  `MouseReleaseMsg` ends it (T4 saves the width then, not on every motion).
- **Wheel**: a wheel event goes **only** to the region under the pointer. (The bubbles
  viewport would scroll on any wheel event, so it never gets unrouted mouse messages.)
- **Clickable regions**: header counters, tabs, tree rows and their arrows, the preview,
  footer buttons (T1); message box parts, dialog fields and buttons, menu items, the toast
  (T2-T4).

## Typing mode (path 2, T3)

**Entering**: `i`, or a click on the preview, on a target whose pane is alive. Entering
shows the Preview tab (from any tab), replaces the preview's title bar with a reverse
yellow `TYPING → al-12-r1` and `C-q / M-q / click outside to leave`, puts `View.Cursor` on
the agent's cursor (`cursor_x - xoff`, `cursor_y` mapped into the preview; hidden when
`cursor_flag=0` or cropped out) and keeps the capture at 4 Hz (never the 1 Hz of a still
pane). A typed key leaves scroll-lock, as in a terminal. A dialog that opens meanwhile (a
late result) waits until typing mode ends.

**Encoding** (pure `typing.Encode(tea.Key) (Op, ok)`; `KeyReleaseMsg` is ignored):
- `Text != ""` with modifiers ⊆ {shift} → literal text (sent through a buffer, see Sender).
- Special keys → `send-keys -t %P <name>`: Enter `Enter`, Tab `Tab`, shift+Tab `BTab`,
  Backspace `BSpace`, Delete `DC`, Insert `IC`, Esc `Escape`, arrows `Up Down Left Right`,
  `Home End`, PgUp/PgDn `PPage NPage`, `F1`-`F12`, space `Space`.
- Modifiers become prefixes in the order `C-` `M-` `S-`: `ctrl+a`→`C-a`, `alt+b`→`M-b`,
  `ctrl+left`→`C-Left`, `alt+enter`→`M-Enter`, `shift+enter`→`S-Enter`, `ctrl+alt+x`→`C-M-x`.
- super / hyper / meta (Cmd) combinations are not sent; the banner flashes "not sent".
- IME / multi-rune `KeyExtended` → literal text.
- **Paste** (`tea.PasteMsg`) → `load-buffer -b otag-tui-<pid> -` (the text on stdin: no
  command-line limit, never in `ps`) ; `paste-buffer -p -d -b otag-tui-<pid> -t %P` (`-p`:
  bracket codes only if the agent asked for bracketed paste; CRLF and CR → LF, other
  controls but tab dropped; tmux turns LF into CR; capped at 256 KiB).

**Submission reliability**: fast text followed by Enter is taken as a paste by Claude
(and Codex has a paste-burst heuristic), so the Enter becomes a newline. Rules:
1. One `send-keys` per key event; text and keys are never batched ("text+Enter" never).
2. **Enter is gated**: the sender holds every Enter (`Enter`, `KPEnter`, with any
   modifier) until 150 ms after the last literal write to that pane (500 ms after a paste).
   The sender is FIFO, so later keys wait and order is kept (`typing.EnterGap`,
   `typing.PasteGap`). Measured with `pastecheck` (an input box that takes an Enter less
   than 100 ms after text for part of a paste): 10 of 10 messages submit through the TUI,
   0 of 10 with the gate set to 0.
3. Coalescing only under backlog (> 64 queued ops, a held key): consecutive literals merge,
   never across a non-literal key.
4. Typing mode never presses Enter on its own.
5. Messages never go through tmux typing: the message box, reply, review and dialogs use
   `otag send` / `otag reply` (proven delivery). With no injected channel ready the message
   stays `queued` and the TUI shows it; it never falls back to typing.

**Sender**: one goroutine fed by a buffered channel (4096). Update enqueues without
blocking. A full queue ends typing mode with a toast: the keys after a lost one must not
follow it (an Enter would submit a text with a hole). Results come back through one
listen `Cmd` at a time, kept only while ops are outstanding; each result schedules a
burst capture. Each op is **one** tmux invocation (2 s limit) that checks and sends
atomically (`tmux.Guard`): `if-shell -F -t %P '<pane alive, @otag_run = run, pane_pid =
pid>' '<send>' '<report the mismatch>'` (an untagged window needs the pid). A pane id
reused after a server restart, or a pane of another run, gets nothing, and no restart can
fall between the check and the send. No typed text is ever put into a tmux command
string or onto a command line: text and pastes go `load-buffer -b otag-tui-<pid> -` from
stdin, then the guarded branch runs `paste-buffer -d -r` (typed text: bytes as they are)
or `paste-buffer -p -d` (a paste: bracket codes when asked, LF→CR); a failed guard
deletes the buffer. Keys go as `send-keys -t %P '<name>'`, the name (ours, from
`Encode`) quoted for tmux's parser. Tested with `;`, `#{…}`, `#(…)`, quotes, `\`, `$`
and a leading `-`.

Ops carry a **generation**. Leaving typing mode (C-q, M-q, a click outside, another tab,
the preview's run or pane changing, an attach) ends it: every op of it still queued, the
Enter waiting at the gate included, is skipped, never sent, and once the queue is empty
the toast says how many keys were not sent. **Any failed send** (a timeout, a gone pane,
a failed check) ends the generation at once in the sender itself, so no later text or
Enter makes an incomplete input; typing mode ends with "typing stopped, the rest was not
sent: …"; a failure of an earlier session's op only counts as not sent. **An attach
waits until the queue has drained** (skipped or sent), both before it is planned and
when the plan arrives, and typing mode cannot start while an attach is being prepared:
the terminal never goes to tmux while typed keys may still be delivered.

**Leaving**: `C-q` / `M-q` (the same keys that return from an attach), a click outside
the preview (then the click is processed normally), another tab, an attach, or the pane
dying (the preview's target changes: the run ended, the liveness check found the pane
gone).

**Limits** (listed in `?`): clicks inside the preview are not forwarded to the agent; no
image paste; keys tmux cannot encode for the pane (e.g. ctrl+shift+letter) degrade;
`S-Enter` reaches the agent as shift+enter only when it turned on extended keys (else use
alt+enter, ctrl+j or the message box); a wider pane is cropped (`←→`, or attach);
IME preedit is not shown. Typing mode answers harness dialogs (run `waiting`, `⚠`), which
never get injected text (MESSAGES.md).

## Full-screen attach (path 3, T3)

**What attaches**: `Enter` or a double click on a run or coordinator row, or on a task row
whose current run has a live pane (other rows toggle); `Enter` or a double click in the
preview; `c` on a project whose coordinator runs; [Attach] in the Run started and
Coordinator result dialogs. One attach at a time.

**Plan** (`attach.Decide`, pure):
1. No live pane: no attach; the toast says why ("run … ended", "pane %7 is gone").
2. `$TMUX` set (the user's own tmux) and its `tmux -V` ≥ 3.3 (`display-popup -B`): **popup**.
3. Otherwise **exec** (inside an older tmux: a nested client, `TMUX` unset).

**Before attaching**, `tmux.Client.EnsureUI` makes one invocation on otag's server, wrapped
in `if-shell -F '#{!=:#{@otag_ui},off}' '…'` ("no server" is ignored; a failure is only a
warning):

```
set -g mouse on
set -s extended-keys on                           # apps that ask get CSI-u (shift+enter etc.)
bind -n M-q detach-client
bind -n C-q detach-client
bind -n MouseDown1Control0 detach-client          # the clickable "← otag"
set -g status-left  '#[range=control|0,reverse] ← otag #[norange default] '
set -g status-right '#{?@otag_run,run #{@otag_run} · ,}M-q / C-q: back to otag '
```

Verified on tmux 3.8: `range=control|N` triggers the `ControlN` mouse key, and these root
bindings are accepted. `#{@otag_run}` is the window tag otag already sets. Root bindings
exist only on otag's private server; `otag run attach` gets the same keys (`execAttach`
calls `EnsureUI`).

**Exec path**: `tc.AttachArgs(session, pane)` run with `tea.ExecProcess`, environment
without `TMUX` / `TMUX_PANE`.

**Popup path**, with the user's environment (keeping `$TMUX`):

```
tmux display-popup -E -B -w 100% -h 100% -t $TMUX_PANE \
     "env -u TMUX -u TMUX_PANE <tmux> -u -S <otag's socket path> select-window -t '%P' \; attach-session -t '=S'"
```

also through `tea.ExecProcess`; the return key detaches the inner client and `-E` closes
the popup. The command is one shell string (the outer server's `default-shell` runs it,
with the outer server's environment, hence `-S` with the socket path instead of `-L`;
`=S` is quoted because zsh expands `=word`). `display-popup` blocks until the popup closes
and returns the inner command's exit status. The command's stderr is kept and added to
an attach error toast.

**Restore**: bubbletea v2 `RestoreTerminal` re-emits alt screen, bracketed paste, focus
reporting, mouse mode and keyboard enhancements from the last view and checks the size.
The TUI also returns `tea.Batch(tea.ClearScreen, reload, liveness check, captureNow)`. An
attach error becomes a toast, and so does a failed `EnsureUI` (the attach still runs).

Not chosen: `switch-client` (cannot cross servers), a pty proxy with stdin interception,
an embedded `x/vt` pane.

## Dialogs and forms (T2)

`form` kit: text, textarea (click-to-cursor), select, checkbox, buttons; Tab / shift+Tab
between fields, `ctrl+s` or Enter on a button submits, Esc cancels; every field and button
is a layout region. Client-side checks cover required fields only; a CLI error shows in
the dialog (code + message) and the form stays open.

| Dialog | Fields | Call |
|---|---|---|
| Task add | title*, body, priority, profile (optional), draft ☐, blocked-by | `task add --project P [--body] [--priority] [--profile] [--draft] [--blocked-by]… -- <title>` |
| Task edit | the same, prefilled | `task edit ID … --version V` (changed flags only) |
| Cancel / Block | reason | `task cancel ID --reason R --version V` / `task block ID --reason R` |
| Profile picker | name, harness/model/effort, tags; unavailable ones greyed with the reason (`run.ProfileAvailable`); default preselected (task profile → project default; coord: coordinator_profile) | start / coord |
| Start run | picker → confirm | `run start TASK --profile P --no-attach`; the result offers [Attach] [Type] [Close] |
| Stop / Park | reason; [Stop] [Park] (with an open ask of this run) [Cancel] | `run stop RUN --reason R` / `run stop RUN --park` |
| Reply | the question (read-only), answer | `reply ID -- text` |
| Review | task, reviewed run and profile, branch; [Accept] (note) [Reject] (reason required) | `review TASK --accept [note]` / `--reject reason` |
| Deliver | task, branch `otag/<id>`, worktree, "worktree will be removed; branch kept"; note; ☐ delete branch if merged (unchecked); ☐ force (needs a second confirm) | `task deliver ID --note N [--delete-branch] [--force]` |
| Confirm / Result | yes/no; `Outcome.Human` in a viewport | — |

Context menu (right-click, or `o` / `shift+F10` / `menu`; T4), each item with its key and,
when disabled, why. An item is disabled only for what its key checks before it calls the CLI
(read-only / `cannotAct`, a target of the wrong kind, no live pane, no open question), never for
the backend's own rules: the CLI's error is the toast, as when the key is pressed
(`TestMenuEnabledMatchesKeys` compares every item with its key). **Every item
runs the code of its key or button**: `m.do(action, scope)` for a key, `itemAct` for an
inline button, `boxKey` for a key of the message box; the menu adds no action of its own.
Right-click selects the row (or Needs / Inbox item) first, as a left click does. A disabled
item is dim with `— reason` after its key; choosing it shows the reason as a toast and keeps
the menu open. Keys in a menu: `↑↓ k j`, `pgup pgdn`, `g G`, `enter`/`space` run, `esc`/`q`/`o`
close, the wheel moves; a click outside closes it (a right-click there opens that place's
menu). A dialog that opens closes the menu, and so does a snapshot that removes or
replaces the menu's row or Needs / Inbox item; one that only changes availability rebuilds it (also after a pane-liveness result).
- **Project**: New task…, Coordinator (attach / start…), Message the coordinator, Expand
  all, Expand/Collapse, Show/Hide closed tasks, Details, Stats, Sync registry, Filter….
- **Task**: Start run…, Attach, Type into pane, Message task, Reply, Review…, Deliver…,
  Edit…, Mark ready, Block…, Cancel…, Resume, Details, Copy id.
- **Run, coordinator**: Attach, Type into pane, Message, Reply, Stop…, Park, Resume,
  Details, Copy attach command.
- **Deliver group**: Deliver…, Expand/Collapse, Details. **"+N earlier"**: Show/Hide.
- **Needs / Inbox item** (`o` with the pane focused, or right-click on it): the item's own
  buttons (Reply, Resend `send --resend D`, Unblock, Ack, …) through `itemAct`, plus Reload.
- **Preview**: Attach, Type, Follow live, Top of the history, Message, Details, Copy
  attach command. **Text tabs**: Top, Bottom (Refresh on Stats). **Tab bar**: the six tabs.
  **Divider**: narrower / wider / default. **Message box**: Send, Recipient…, Next kind, Back
  to the tree (they press the box's keys). **Header, footer, empty tree**: Filter…, Show
  closed, Reload, Redraw, Help, Quit.

Inbox actions: Enter on a notification `notify ack ID`; `A` ack all after a confirm;
"Mark messages read" runs `inbox`.

## Testing

1. **Pure `Update` tests** (`model_test.go`, `mouse_test.go`): a fixture snapshot
   (`fixture/`) and fake side effects; timers fire by hand and the clock is injected.
   Covered in T1: selection across snapshots, expansion and the history load, focus ring,
   every key in the table (and that no key shadows another), the data loop (debounce,
   dirty reload, safety net), the capture loop (one chain, 0 captures on other tabs,
   4 Hz / 1 Hz), scroll-lock, gone panes, Details/Stats commands, clicks, double-click
   timing, wheel routing, divider drag clamp, narrow and too-small layouts, layout z-order.
   T2-T4 add dialogs (exact argv, `--` before user text), busy flags, CONFLICT, reply mode,
   typing (C-q / M-q / click outside), attach plan, menus.
2. **Unit tests**: `preview` (sanitize, crop with wide runes, hash skip, scroll-lock),
   `tree` (order, nesting, expansion, selection, closed tasks, row text), `data.Load` and
   the watcher against a temp DB built with the CLI and the fake harness (`tuitest/`),
   `tmux.PaneView`, `cmdTui` refusals and `Text`. T3: `typing.Encode`, the sender (one exec
   per key, Enter gate, FIFO, coalescing).
3. **Golden View tests** (`view_test.go`, `go test ./internal/tui -update` rewrites them):
   120x40 and 80x24 at a fixed clock; `ansi.Strip(View().Content)` against
   `testdata/*.golden`. Screens: main, run preview, needs, inbox, details, help, closed
   tasks, read-only, narrow tree, narrow pane, too small (T2-T4: dialogs, menu, typing
   banner). A test checks that the Lip Gloss layers sit at the layout's blocks (hit tests
   agree).
4. **E2E in tmux** (`e2e_test.go`, skipped without tmux): an outer server
   `tmux -L otag-tui-test-<pid>-<rand>` runs `otag tui` (160x45) with an isolated `OTAG_DB`,
   `OTAG_CONFIG_DIR`, `OTAG_DATA_DIR`, `OTAG_TMUX_SOCKET`, `OTAG_BIN`, `HOME`. Keys go in with
   `send-keys`, clicks as raw SGR mouse sequences (`send-keys -l '\e[<0;X;YM' '\e[<0;X;Ym'`;
   button 2 right, 64/65 wheel); the screen is polled with `capture-pane`. Both servers are
   killed at cleanup. Scenarios (fake harness only):
   1. Start-up: tree, header counts; a click previews a run; a CLI change shows within
      0.5 s with the selection kept. **(T1)**
   2. `n` task add → row appears. (T2, `TestE2EAddStartSendStop`)
   3. `s` start with profile `manual` → run row; preview shows `fake: start`; the message
      box sends to the run (dispatched / delivered / queued, never typed into the pane);
      `S` stops it. (T2, same test)
   4. A question (`ask:` step of the fake) → "⚑ 2"; `r` reply → the task leaves waiting.
      (T2, `TestE2EReplyParkResume`)
   5. Typing: profile `typist` (the `run:` step runs `pastecheck`, an input box with paste
      detection) → `i`; ten messages, each written in one burst with `Enter` right after
      it → ten submits, each whole and once; a bracketed paste and Enter → one submit with
      the newline inside; Esc Esc stays in typing mode; `C-q` leaves. Control: text and
      Enter as two quick `send-keys` straight to the pane do not submit. (T3,
      `TestE2ETyping`)
   6. Attach, exec path (the TUI with `TMUX` unset): `otag run attach` in another window,
      `C-q` returns to its shell; `Enter` → `← otag` and `run <id>` in the status bar;
      `M-q` → TUI back; a click still selects (mouse restored); again with a double click
      and `C-q`, and with `Enter` on the task row and a click on `← otag`. (T3,
      `TestE2EAttachExec`)
   7. Attach, popup path: the TUI runs inside a second throwaway tmux
      (`-L otag-tui-user-…`) whose client is the outer server's pane; the window shows in
      the popup (the TUI's own pane does not show it); `M-q`, `C-q` and a click on
      `← otag` each return, and a click selects afterwards. (T3, `TestE2EAttachPopup`)
   8. Review accept / reject, deliver (worktree gone, result dialog). (T2,
      `TestE2EReviewDeliver`)
   9. Stop → Park on a run with an open ask; a reply that stays queued; `u` resumes. (T2,
      `TestE2EReplyParkResume`)
   10. Divider drag; the layout (divider, tab, a collapsed project) comes back after a
       restart, and a corrupt state file gives the defaults. (T4, `TestE2EDividerDragAndRestart`)
   11. A context-menu click: right-click a task, a disabled item says why, Details shows the
       tab, Mark ready runs `task ready`; `o` opens the menu, Esc closes it. (T4,
       `TestE2EContextMenu`)

## Steps

Each step: Sonnet/Opus writes it, Codex (or Opus) reviews it; live tests only in isolated
directories.

### T1: skeleton, data, tree, preview (read-only) — done

Scope: Charm v2 dependencies; `cmdTui` and its refusals; `data` (Snapshot, Watcher,
liveness); `layout` and hit testing; the tree with every row kind and the ordering; header
counters; tabs Preview / Details / Needs / Inbox / Stats / Log (read-only; Details and Stats
through `Text`); preview capture (live, scroll-lock, h-scroll, focus rate); click select,
wheel routing, divider drag, keyboard navigation; narrow mode; `?` overlay of the bindings
so far.

Accepted:
- With the daemon stopped, a CLI change shows in about 0.2 s (E2E 1 asserts < 0.5 s).
- 41 tasks and 10 runs: 0.3-0.5% CPU idle (measured with `ps` on macOS).
- Preview: at most 4 captures/s for the one visible run (1/s when still), 0 on other tabs,
  none for runs not shown (unit tests).
- Selection survives refresh; pure, golden and E2E 1 tests pass.

Notes from T1:
- `.` (closed tasks) is in T1 so every row kind can be shown; the `/` filter stays in T4.
- `Enter` and a double click toggle rows until T3 makes them attach.
- The help overlay is cut on terminals shorter than about 40 rows (fixed in T4: it
  scrolls).
- The coordinator cannot run on the fake harness, so coordinator rows are covered by the
  fixture tests only.

### T2: actions, dialogs, message box — done

Scope: `cli/tui.go` `Call`; the `form` kit and every dialog above; all action keys and
footer buttons; Needs-tab inline actions; the message box with recipient, kind and reply
mode; busy flags, toasts, Log tab; request ids; read-only mode.

Accept: every action's argv equals the documented CLI usage (unit tests); the outcome text
equals the CLI's human output; CONFLICT keeps the form open; a double click or double Enter
does not start twice (busy flag + request id); E2E 2, 3, 4, 8, 9; no `Exec` / `Tx` under
`internal/tui` (the DB is written only by CLI code).

Accepted:
- `actions_test.go`: the exact argv of every dialog and key (`--` before user text), the
  busy flag against a double submit, request ids (the same arguments keep theirs),
  CONFLICT (form kept, version refreshed from the next snapshot), reply mode, the
  recipient picker, read-only, Needs / Inbox actions, the quit wait, dialog mouse, and
  `TestNoWritesInTUI`. `cli/tuicall_test.go`: `Outcome.Human` equals the CLI's output,
  error codes, the request-id replay, the child gate. `form/form_test.go`: the kit.
- Goldens: dialog-add, dialog-start, dialog-deliver, dialog-short, dialog-error,
  msgbox-reply, log (and every T1 screen, now with the message box).
- E2E 2, 3, 4, 8, 9 and the message box pass against the fake harness.

Notes and deviations from T2:
- Dialogs, the message box and the item tabs live in package `tui` (`dialogs.go`,
  `msgbox.go`, `items.go`): they need the model's snapshot and selection. Only the kit is
  its own package (`form/`, with a small soft-wrapping editor instead of the bubbles
  textarea, so click-to-cursor and the hit rectangles stay in our hands).
- The busy key of an action is its whole command line: the same action on the same target
  (with the same text) runs once at a time, others run side by side. Toasts and the Log
  name it by its command words (`run start al-12`). The footer shows
  `… N action(s) running` while no toast shows.
- A dialog closed with Esc while its action runs does not cancel it; its outcome is a
  toast only: no result dialog, no selection move. A dialog never replaces another: a
  late one (the [Mark read] result, a task read for the edit form) waits in a queue until
  the open dialog closes.
- The deliver dialog's force step puts [Back] first and makes it the default (where
  [Deliver] was): a double click or a second ctrl+s goes back; force needs a click or
  Enter on [Deliver with force] itself.
- A send finishing leaves reply mode alone when the user entered another one meanwhile, and
  empties the box only when its text is unchanged.
- `r` on a tree row uses the message box in reply mode; the
  Needs / Inbox [Reply] button (and `r` there) opens the Reply dialog, which shows the
  question.
- Start / coord show the CLI's output in a Result dialog with [Close]; [Attach] and [Type]
  join it in T3. `c` on a project whose coordinator runs selects that row.
- `p` is a confirm dialog for `run stop RUN --park`; the stop dialog's [Park] also passes
  `--reason` when one is typed. Review is `review TASK --accept [-- note]` /
  `--reject -- reason` (a reject without a reason is refused in the form).
- Task edit reads the task first (`data.Task`, for the body, version and blocked-by edges);
  blocked-by changes become `--add-dep blocks:X` / `--rm-dep blocks:X`; edit has no draft
  box (`--status` is not offered). A field cleared to empty is sent as such; the CLI says
  what it thinks of it.
- The profile picker greys out `data.Snapshot.Unavailable` (`run.ProfileAvailable` of
  every active profile, read in the snapshot transaction). The wanted profile (the
  task's, else the project's default) stays selected even when unavailable: Start then
  says why, and another profile is an explicit choice, never a silent substitute.
- The send toast is the CLI's first line, e.g. `info #3 → al-1-r1: dispatched: sent by
  api_start; proof of delivery to come` (the proof arrives later; Needs shows a delivery
  whose outcome turns unknown).
- The message box takes 2-7 rows below the tree and the right pane, always (narrow mode
  too). A dialog taller than the screen loses its top rows; the buttons stay.
- The help overlay grew with the action keys; below about 50 rows it was cut (T4 made it
  scroll).
- With Needs or Inbox focused, the action keys act on the selected item, never on the tree
  row out of sight: `R`, `u`, `r`, `S`, `m` run the item's matching button ([Unblock],
  [Resume], [Reply], [Stop], [Message]) or say what the item offers; `n` and `A` work as
  everywhere; the other keys say to use the item's buttons or Tab to the tree. The item
  selection is kept by the item's kind and id (`q:42`, `b:al-16`), so a reload that adds
  or removes items above it does not move it; when the item itself goes away, the item
  that takes its place becomes the selection (its key is adopted). Footer buttons act like
  their keys (on the item while Needs / Inbox has the focus).
- `tmux.Panes` (`list-panes -a`) treats "no current target" / "no sessions" (a server
  exiting after its last window) as an empty server; it made `run start` fail right after
  another run of the same server ended.
- Test support: the fake harness has `ask:<question>` and `askpark:<question>` steps
  (`otag ask`), `tuitest` has a profile `asker` and gives beta `review_profile = "manual"`.
  In tmux E2E a key sent right after Esc reads as alt+key, so the tests pause after Esc.

### T3: typing mode, attach, tmux bindings — done

Scope: `typing` (Encode, Sender with the Enter gate, paste); cursor overlay; `attach`
(plan, exec, popup); `tmux.EnsureUI` and the send helpers; `execAttach` calls `EnsureUI`;
restore + ClearScreen on return; docs on keys, `@otag_ui off` and Option-as-Meta.

Accept: E2E 5, 6, 7 on tmux 3.8; manual check in iTerm2, Ghostty and Terminal.app, inside
and outside the user's tmux (after return: mouse, alt screen and keys work, no stray
characters); a Claude coordinator in an isolated `OTAG_*` environment, typed via typing
mode, submits on Enter 10 of 10 times; `M-q`, `C-q` and the `← otag` click each return;
`otag run attach` has the same return keys.

Accepted:
- E2E 5, 6, 7 pass on tmux 3.8 (`e2e_t3_test.go`). E2E 5 with `typing.EnterGap` set to 0:
  0 of 10 submits; with 150 ms: 10 of 10.
- Live: a Claude coordinator (`c2-haiku`) in the isolated `/private/tmp/otag-e2e`
  environment, driven through `otag tui` in a throwaway tmux: ten messages, each typed in
  one burst with Enter right after it, 10 of 10 submitted (one `UserPromptSubmit` each,
  each prompt whole in the transcript).
- Unit tests: `typing` (Encode table, IsReturnKey, CleanPaste, the sender: one exec per
  key, the gate, FIFO, coalescing only under a backlog, a full queue), `attach.Decide`,
  `tmux` (EnsureUI incl. `@otag_ui off`, guarded sends with tricky text and every guard failure, PopupArgs,
  PopupSupported), the model (`interact_test.go`: every key forwarded, C-q / M-q, clicks
  in and outside, the burst and the 4 Hz floor, pane death, queued dialogs, the cursor,
  every way to attach, the return, [Attach] / [Type]), goldens `typing` and
  `dialog-started`.
- Not done here: the manual matrix in iTerm2, Ghostty and Terminal.app (the E2E runs
  inside tmux panes only).

Notes and deviations from T3:
- No preview border: the title bar turns into the reverse yellow `TYPING → <run>` banner,
  so the preview keeps its size.
- The popup needs tmux ≥ 3.3 (`display-popup -B`; also `-T` is 3.3), not 3.2, and has no
  title (no border to show it in). It addresses otag's server with `-S <socket path>`, not
  `-L`, because the popup's shell has the outer server's environment (`TMUX_TMPDIR` may
  differ). The outer tmux binary is otag's own (`Client.Bin`); its `tmux -V` is read once.
- `tmux.ShellQuote` now quotes a word starting with `=`: zsh (macOS's default shell, and
  so the popup's `default-shell`) expands `=word`, which broke `attach-session -t =S` in
  the popup and in `otag run attach --print` output pasted into zsh.
- Paste uses `load-buffer -` (stdin) instead of `set-buffer`: no command-line size limit,
  and the text never shows in `ps`. The buffer is `otag-tui-<pid>`.
- The sender's queue is 4096, and a full one ends typing mode instead of dropping the key
  and going on (a later Enter would submit a text with a hole).
- After the T3 reviews: every send is checked against the run's identity in the same
  tmux invocation (`Guard`: alive, `@otag_run`, pane pid), typed text goes through a
  buffer (`paste-buffer -r`) instead of `send-keys -l`, ops carry a generation that
  leaving typing mode cancels, a failed send stops the rest at once, and an attach waits
  for the queue to drain. So keys still queued when typing mode ends are not sent (the
  toast counts them): a paste, Enter and an immediate C-q lose the Enter. The gate holds every
  Enter, with any modifier (`S-Enter` too), not only `Enter` / `M-Enter` / `KPEnter`.
- `i` works from any tab (it shows the Preview); a single click on a live preview enters
  typing mode (it used to only focus the pane). Enter on a task row attaches only when its
  run's pane is live; elsewhere it still toggles. A double click on a task row with a live
  run attaches instead of toggling.
- The [Type] button selects the new run's row and enters typing mode as soon as its pane
  shows in the preview (up to 10 s: the row and the liveness arrive with the next reload).
- An attach's stderr is kept and shown in the error toast (the TUI's redraw would hide
  it); `display-popup` returns the inner client's exit status.
- `EnsureUI` sets `status-left` / `status-right` globally on otag's server, replacing what a
  user's otag `tmux.conf` sets there, unless `@otag_ui off`.
- The outer tmux with `mouse off` still passes the clicks on `← otag` to the popup (checked
  with the E2E's user server set to `mouse off`).
- Test support: the fake harness has a `run:<command>` step; `tuitest.BuildOtag` also builds
  `pastecheck`, and the registry has a profile `typist`. The E2E TUI runs with `TMUX`
  unset (exec path) unless a test starts it inside its own tmux.

### T4: polish — done

Scope: context menus everywhere (right-click / `o`) with disabled reasons; optional hover
the `/` filter; the full help overlay from the binding table;
persisted UI state in `<data>/tui/state.json` (divider, expanded keys, tab; corrupt →
defaults); light/dark theme from the background colour; `View.ProgramStatus` (OSC 7501:
blocked when needs > 0, else idle); `View.WindowTitle` `otag (3)`; debug log
`OTAG_TUI_DEBUG=1` → `<data>/logs/tui.log`.

Accept: every mouse action in this document has a key (a test iterates the region kinds);
the help overlay matches the binding table (golden); E2E 10 and 11; restarting the TUI
restores the layout.

Accepted:
- `polish_test.go`: `TestMouseActionsHaveKeys` iterates every region kind up to
  `regKindCount` (a new clickable part without a key fails it); `TestMenuRoutesLikeKeys`
  compares each menu item with its key (same calls, dialog, attach, tab, toast) on two
  harnesses; disabled reasons, Needs / Inbox menus, menus everywhere, filter, `y`, `E`,
  `Y`, help scroll (every line reachable) and `TestHelpMatchesBindings` (each binding's keys
  and words are in the overlay), state file (round trip, corrupt, versions, atomic, capped),
  persistence in the model, theme, hover, the debug log never holding typed text.
- Goldens: help, help-scrolled, help-short (80x24), help-tall, menu-task, menu-run,
  menu-short (80x16), menu-needs, filter.
- E2E 10 and 11 pass against the fake harness; the layout comes back after a restart.

### Persisted state, theme, terminal reports, debug log (T4)

- **State** `<data>/tui/state.json`: `{version, divider, tab, expanded}` (`expanded` is the
  explicit expansion choices of the tree, never the "+N earlier" rows). Written
  once, synchronously, by `tui.Run` after the program has returned (a temporary file in the
  directory, then a rename, on a capped copy of the map; a failure prints one line to stderr).
  There is no save worker: a crash loses only the layout changed since the last start. Missing, unreadable, corrupt
  (bad JSON, another version, a negative divider, a tab out of range, more than 1000
  choices) loads as the defaults; the TUI never fails to start over it. The filter, the
  selection and the scroll are not saved.
- **Theme**: the TUI asks for the terminal's background colour at start
  (`tea.RequestBackgroundColor`); a light one switches `style.Default` to `style.Light`
  (darker 256-colour accent, good, warn, bad and typing colours; the rest stays on the ANSI
  palette). A terminal that does not answer keeps the dark theme.
- **Terminal reports**: `View.WindowTitle` is `otag (N)` with N the needs-you count (`otag`
  at 0); `View.ProgramStatus` (OSC 7501) is `blocked` / question with `N need you` while N
  > 0, else `idle`. Bubble Tea only writes them when they change.
- **Debug log**: `OTAG_TUI_DEBUG=1` appends to `<data>/logs/tui.log` (0600): message types
  (not key presses, pastes or mouse events, nor the periodic ticks), load failures,
  action names with ok / error code, typing on / off with the run id, attach results, menu
  opens and runs. Never a message body, a typed or pasted key, a filter text or an
  action's output (`TestDebugLogHasNoText`).
- **Hover**: `OTAG_TUI_HOVER=1` switches the mouse mode to `AllMotion`; the tree row under
  the pointer is drawn bold. A motion that changes nothing does not redraw.

Notes and deviations from T4:
- Fit-to-preview (`z`, `resize-window`) was dropped after review: it changed a real agent's
  window with its ownership only in TUI memory (a crash or another attach would leave it
  resized). The preview truncates and scrolls (`←→`) instead.
- Hover is `OTAG_TUI_HOVER=1`, not `otag.toml [tui] hover = true`: otag has no global config
  file (`otag.toml` does not exist; the registry TOML only holds accounts, profiles and
  projects), and a new one for one switch was not worth it.
- New keys beyond the table above: `E` (expand all) and `Y` (sync registry), so that every
  menu item has a key; `esc` clears the filter.
- The help overlay scrolls (`↑↓ pgup pgdn g G`, the wheel, a position line) instead of
  using columns; its text is wrapped to the overlay's width, with the descriptions
  hanging. The first screenful of a 120x40 terminal shows the key tables, `end` the notes.
- The filter prompt lives in the header (`/text`), not the footer, so the footer buttons
  stay; typing mode and dialogs take priority over it.
- A menu taller than the screen scrolls with the selection (no scroll bar).

## Risks

1. **Paste heuristics in agents** (seen with Claude; Codex has one too): one key per exec,
   the Enter gate, no auto-Enter, messages only through injected channels. A very fast
   typist could still trip it; T3's 10/10 test guards the gate.
2. **Terminal restore after an attach** across iTerm2, Ghostty, Terminal.app and nested
   tmux: v2 re-applies modes from the last view; T3 adds `ClearScreen` and a manual matrix.
3. **Option-as-Meta off on macOS**: `M-q` arrives as "œ"; `C-q` and the `← otag` button
   cover it, and the help says so.
4. **`mouse on` in otag's server** changes drag-select while attached (Option / Fn + drag);
   the wheel enters copy mode for agents that do not take the mouse.
5. **The outer tmux eats mouse or wheel** when it has `mouse off`: the TUI works with the
   keyboard alone.
6. **Bubble Tea v2 is young**: exact versions pinned, every import path checked.
7. **Preview fidelity**: cropping, wide runes, SGR only (no hyperlinks or images). Attach
   is the full-fidelity path.
8. **`extended-keys on`** changes what agents that ask receive for modified keys; covered
   by `@otag_ui off`.
9. **Long actions** (`run start`, `task deliver`) take seconds: they run async with a busy
   row; quitting waits up to 5 s.
