// Package adapter defines the boundary between otag and a harness (Claude Code, Codex,
// OpenCode, ...). An adapter is pure translation: it turns a RunSpec into a
// LaunchSpec (argv, env, files, optional server) and turns raw hook payloads into
// normalized Events. It never touches the database, tmux or git; the run package does.
//
// See docs/ADAPTERS.md for the contract.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/guard"
)

// Adapter is implemented by every harness.
type Adapter interface {
	// Harness is the name used in accounts.harness and `otag event <harness> <kind>`.
	Harness() string

	// Prepare builds the launch of one interactive session. It must not write files or
	// change global harness config; files go into LaunchSpec.Files and are written by otag
	// under spec.RunDir. It may run local, read-only probes (e.g. `codex app-server`
	// hooks/list) and cache results under spec.CacheDir.
	Prepare(ctx context.Context, spec RunSpec) (*LaunchSpec, error)

	// ParseEvent translates one hook invocation (`otag event <harness> <kind>`, payload on
	// stdin) into normalized events and the bytes the hook prints on stdout.
	// It must be fast and side-effect free: no network, no DB, no blocking. It may be
	// called twice for one payload (the second time with a filled HookContext when the
	// run was resolved through HookResult.SessionRef).
	ParseEvent(hc HookContext, kind string, payload []byte) (HookResult, error)

	// NoopOutput is printed by the bridge when anything fails (unknown run, DB error,
	// ParseEvent error, panic) so the harness keeps working unchanged. Most harnesses
	// accept empty output.
	NoopOutput(kind string) []byte
}

// TrustHandler is implemented by harnesses that show a folder-trust dialog in new
// directories that cannot be pre-trusted per launch (Claude). After launching the
// pane, otag polls the visible pane text until the first hook event arrives or
// LaunchSpec.StartupTimeout passes; when AcceptTrust matches, otag sends the keys once.
// This is the only place where otag reads the screen.
type TrustHandler interface {
	AcceptTrust(screen string) (keys []string, ok bool)
}

// ServerSetup is implemented by server-backed harnesses (OpenCode, Codex app-server).
// It runs after LaunchSpec.Server is ready and before the TUI pane starts; it may call
// the local server (e.g. create the session with model and metadata) and adjust launch,
// typically Argv and HarnessSessionID.
type ServerSetup interface {
	SetupServer(ctx context.Context, spec RunSpec, launch *LaunchSpec) error
}

// Stopper asks the harness to exit cleanly (e.g. send /exit) before otag kills the pane.
// otag waits a few seconds for the pane to die and kills it anyway.
type Stopper interface {
	Stop(ctx context.Context, t Target) error
}

// Deliverer delivers a message into a live session (phase 4, docs/MESSAGES.md).
// Hook-based adapters may instead deliver through HookResult.Output on their next hook
// (HookResult.Slot).
//
// Contract: nil means the harness accepted the text (it was sent; that is not yet proof
// of delivery: proof is the marker or the receipt ref seen later, see Message). An error
// wrapping ErrNotSent means the text definitely did not reach the harness, so otag may
// retry it later. Any other error is ambiguous: the delivery becomes outcome_unknown and
// is never re-sent automatically. Deliver must not wait for the model to act on the text.
type Deliverer interface {
	Deliver(ctx context.Context, t Target, msg Message) error
}

// ReceiptDeliverer is optionally implemented by a Deliverer that learns the harness's
// own id for the delivery (Codex turn/start result.turn.id or turn/steer turnId,
// OpenCode's inbox message id). otag prefers it over Deliver; the error contract is the
// same. Receipt.Ref is matched against HookResult.Refs to prove delivery, and a proof
// may arrive before DeliverReceipt returns.
type ReceiptDeliverer interface {
	DeliverReceipt(ctx context.Context, t Target, msg Message) (Receipt, error)
}

// Receipt is what a ReceiptDeliverer learned about an accepted delivery.
type Receipt struct {
	Channel string // Chan* the text went in by (e.g. ChanAPIStart or ChanAPISteer); "" = as requested
	Ref     string // the harness's id for it, later seen in HookResult.Refs; "" = none
}

// ErrNotSent marks a delivery error as definite: nothing reached the harness (no
// session, connection refused, a dialog is open, a 4xx before the text was accepted).
// Wrap it (fmt.Errorf("...: %w", adapter.ErrNotSent)); an unwrapped error is ambiguous.
var ErrNotSent = errors.New("message not sent")

// ErrNotReady is ErrNotSent for a refusal that is only "not now": the session cannot
// take text at the moment (its mod is not up yet, a turn is running and cannot be
// steered, a dialog is open), but the target is there. otag retries it soon without
// counting an attempt, so it never makes a delivery dead. It wraps ErrNotSent, so
// errors.Is(err, ErrNotSent) holds too; check ErrNotReady first.
var ErrNotReady = fmt.Errorf("harness not ready: %w", ErrNotSent)

// Delivery channels: how a message goes into a session (message_deliveries.channel).
// Hook channels put the text into a hook's output; injected channels submit it as a
// prompt, with at most one injected attempt in flight per run.
const (
	ChanToolContext   = "hook_tool_context"   // PostToolUse(Failure) additionalContext; proven when the hook output is produced in time
	ChanPromptContext = "hook_prompt_context" // UserPromptSubmit additionalContext; proven like ChanToolContext
	ChanStopBlock     = "hook_stop_block"     // Stop {"decision":"block","reason":...}; proven by the next Stop with stop_hook_active (Slot.Continued)
	ChanModSubmit     = "mod_submit"          // Claude mod $.prompt.submit (injected)
	ChanAPIStart      = "api_start"           // Codex turn/start, OpenCode prompt delivery=queue (injected)
	ChanAPISteer      = "api_steer"           // Codex turn/steer, OpenCode prompt delivery=steer (injected)
	ChanLaunchPrompt  = "launch_prompt"       // the first prompt of a new or resumed run (injected)
	ChanCLI           = "cli"                 // `otag inbox`: the agent read it itself; no attempt
)

// Injected reports whether channel submits the text as a prompt (marker or receipt
// proof, serialized per run) rather than in a hook's output.
func Injected(channel string) bool {
	switch channel {
	case ChanModSubmit, ChanAPIStart, ChanAPISteer, ChanLaunchPrompt:
		return true
	}
	return false
}

// DeliverySlot says that a hook invocation can carry messages in its output: the
// bridge claims queued messages for the run on Channel and has the HookDeliverer put
// the rendered text into the output.
type DeliverySlot struct {
	Channel string // ChanToolContext | ChanPromptContext | ChanStopBlock
	// Continued marks a Stop that follows a Stop block (Claude stop_hook_active): it
	// proves the block's deliveries and must not block again.
	Continued bool
}

// HookDeliverer is implemented by adapters that offer DeliverySlots. RenderDelivery
// returns the hook output carrying text (which already holds the delivery markers)
// on top of base, the output ParseEvent produced (nil = NoopOutput). Pure.
type HookDeliverer interface {
	RenderDelivery(kind string, base []byte, text string) []byte
}

// LaunchTurnProver is implemented by a harness whose first prompt (a launch argument,
// OpenCode's TUI --prompt) reaches no hook otag can scan for delivery markers and has
// no receipt. Its run's turn start proves the launch prompt's messages: a weaker
// proof than a marker (the turn that started is the one the pane was launched with;
// no other injected attempt can be in flight meanwhile).
type LaunchTurnProver interface {
	LaunchProvedByTurnStart() bool
}

// SidecarRunner is implemented by adapters whose events come from a stream instead of
// (or besides) hooks: OpenCode SSE, a Codex app-server client. A LaunchSpec.Sidecars
// entry built with SidecarProcess runs the hidden command
// `otag sidecar <harness> --ready-file F [args...]`, which calls RunSidecar in-process
// with an Emit that feeds the hook bridge directly (no process per event). RunSidecar
// must subscribe, call Ready, then emit until ctx is cancelled (the window is killed
// with the run). After a reconnect it should reconcile state the stream missed.
type SidecarRunner interface {
	RunSidecar(ctx context.Context, sc Sidecar, args []string) error
}

// Sidecar is what a running sidecar gets from otag.
type Sidecar struct {
	RunID  string
	Getenv func(string) string
	// Emit is `otag event <harness> <kind> [--event-id id]` in-process. It returns the
	// bridge output for the harness (rarely needed by sidecars) and an error that is
	// for logging only.
	Emit func(ctx context.Context, kind string, payload []byte, eventID string) ([]byte, error)
	// Ready reports that the subscription is established; otag starts the TUI only
	// after every sidecar is ready, so no early event is lost.
	Ready func() error
}

// SidecarProcess builds the Process that runs `otag sidecar` for spec's harness. The
// ready file lives in RunDir and is the Process.Ready probe.
func SidecarProcess(spec RunSpec, name string, args ...string) Process {
	ready := filepath.Join(spec.RunDir, "sidecar-"+name+".ready")
	argv := append([]string{spec.OtagBin, "sidecar", spec.Harness, "--ready-file", ready, "--"}, args...)
	return Process{Name: name, Argv: argv, Ready: "file:" + ready, ReadyTimeout: 15 * time.Second}
}

// RunSpec is everything an adapter needs to launch one run.
type RunSpec struct {
	RunID   string
	TaskID  string // empty for coordinators
	Project string
	Role    string // child | coordinator | review
	Attempt int

	Harness string
	Account Account
	Profile Profile

	Workdir     string // agent cwd: the task worktree, or the project path in "same" mode
	ProjectPath string // the registered repository
	Branch      string // the task branch in worktree mode; "" in same mode or a detached review worktree
	RunDir      string // private per-run dir (0700) for LaunchSpec.Files
	CacheDir    string // per-harness cache dir, shared across runs
	OtagBin     string // absolute path of otag; use it in hook commands

	// Env must reach the harness process and every hook it runs (OTAG_RUN_ID,
	// OTAG_PROJECT, OTAG_TASK_ID, OTAG_BIN and, in tests, OTAG_DB/OTAG_CONFIG_DIR/...).
	// otag already sets it on the pane; adapters starting other processes pass it on.
	Env map[string]string

	// Prompt is the first user message (task, extra prompt, how to finish).
	Prompt string
	// Instructions is system-level text (identity and rules) to append to the system
	// prompt: Claude --append-system-prompt-file, Codex developer_instructions,
	// OpenCode plugin context hook.
	Instructions string
	// Deny lists path globs agents must not write or read (guards): the project's,
	// plus guard.DefaultDeny for a light run.
	Deny []string
	// Guard is the run's guard level (project setting `guard`). Light runs drop the
	// harness's native sandbox; the hook guard applies the light rules. "" is strict.
	Guard guard.Level

	// ResumeSessionID resumes an earlier harness session (phase 4 park/resume).
	ResumeSessionID string
}

// Account is the frozen account part of the run's profile revision.
type Account struct {
	Name      string
	ConfigDir string            // e.g. ~/.claude-work -> CLAUDE_CONFIG_DIR
	Command   string            // binary or wrapper; "" = harness default
	Env       map[string]string // account env
}

// CommandOr returns the account's command with a leading "~/" expanded to the home
// directory (revisions synced before otag expanded it keep the raw form), or def when
// the account has none.
func (a Account) CommandOr(def string) string {
	c := a.Command
	if c == "" {
		return def
	}
	if strings.HasPrefix(c, "~/") {
		home := os.Getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		if home != "" {
			return filepath.Join(home, c[2:])
		}
	}
	return c
}

// Profile is the frozen profile revision the run is bound to.
type Profile struct {
	Name       string
	Model      string
	Effort     string
	Mode       string // interactive | headless
	Env        map[string]string
	Args       []string // extra harness arguments
	RevisionID int64
	ConfigHash string
}

// LaunchSpec is what otag starts in the run's tmux window.
type LaunchSpec struct {
	Argv []string          // exec'd directly in the pane (no shell)
	Env  map[string]string // added on top of RunSpec.Env (adapter wins)
	Cwd  string            // default RunSpec.Workdir
	// Files are written under RunDir before anything starts.
	Files []File
	// HarnessSessionID is the harness session id when otag chooses it up front
	// (claude --session-id, OpenCode POST /api/session); otherwise bound by a
	// session_start event.
	HarnessSessionID string
	// Server runs before the pane in its own hidden window (OpenCode serve, Codex
	// app-server); otag waits for Server.Ready, then calls ServerSetup.
	Server *Process
	// Sidecars start after the server and before the pane, in hidden windows (e.g. an
	// SSE listener built with SidecarProcess). otag waits for each one's Ready probe
	// before starting the pane. They are killed with the run.
	Sidecars []Process
	// StartupTimeout bounds how long `otag run start` waits for the first hook event
	// while watching for a trust dialog (TrustHandler only). 0 = 20s.
	StartupTimeout time.Duration
}

// File is written to RunDir/Path (Path must be relative and stay inside RunDir).
type File struct {
	Path string
	Data []byte
	Mode fs.FileMode // 0 = 0600
}

// Process is an auxiliary process started in its own tmux window.
type Process struct {
	Name string // window suffix, e.g. "server"
	Argv []string
	Env  map[string]string
	Cwd  string
	// Ready is polled until it succeeds: "tcp:127.0.0.1:4096", "unix:/path/sock",
	// "file:/path" (exists), or "" (no wait).
	Ready        string
	ReadyTimeout time.Duration // 0 = 15s
	URL          string        // recorded as sessions.server_url
}

// HookContext is what the bridge knows about the run when the hook fires. RunID is
// empty when the run is unknown.
type HookContext struct {
	RunID   string
	TaskID  string
	Role    string // child | coordinator | review
	Status  string // run status
	Workdir string
	Branch  string // the run's branch; "" in same mode or a detached review worktree
	Deny    []string
	Guard   guard.Level // the level the run started with ("" = strict)
	// DB, TmuxSocket and OtagBin are otag's own database, tmux socket name and binary,
	// which a child's guard keeps the agent away from (GuardPolicy).
	DB, TmuxSocket, OtagBin string
	Getenv                  func(string) string // hook process env (TMUX_PANE, ...)
}

// GuardPolicy is the run's guard policy: level, workspace, deny globs (with the
// level's defaults), own branch and, for every role but the coordinator, otag's own
// state. Adapters add Cwd and AllowOutside.
func (hc HookContext) GuardPolicy() guard.Policy {
	pol := guard.Policy{Workdir: hc.Workdir, Deny: guard.DenyGlobs(hc.Guard, hc.Deny), Branch: hc.Branch, Level: hc.Guard}

	if hc.Role != "coordinator" {
		pol.DB, pol.TmuxSocket, pol.OtagBin = hc.DB, hc.TmuxSocket, hc.OtagBin
	}
	return pol
}

// Guarder is implemented by adapters whose hooks carry an enforcement decision (a
// pre-tool or permission hook). For those kinds the bridge fails closed: when it
// cannot apply the run's policy (database unavailable or newer than this binary,
// unknown or ended run, malformed payload, a forged run token) it prints
// DenyOutput(kind, reason) instead of NoopOutput, and a decision ParseEvent made is
// printed even when recording the event fails.
type Guarder interface {
	GuardKind(kind string) bool
	DenyOutput(kind, reason string) []byte
}

// HookResult is the outcome of ParseEvent.
type HookResult struct {
	Events []Event
	// Output is printed on stdout for the harness (decisions, additionalContext).
	// nil means NoopOutput(kind).
	Output []byte
	// SessionRef is the harness session id found in the payload; used to find the run
	// when OTAG_RUN_ID is missing, and stored with the raw payload.
	SessionRef string
	// EventID is a stable id of this hook invocation if the payload has one
	// (e.g. tool_use_id+kind); `otag event --event-id` wins over it. Events are
	// deduplicated per (harness, EventID).
	EventID string
	// Slot is set when this hook's output can carry messages (phase 4).
	Slot *DeliverySlot
	// Refs are harness ids that confirm a delivery (Codex turn/started turn id, OpenCode
	// inbox.delivered message id), matched against Receipt.Ref. The bridge scans the raw
	// payload for delivery markers itself (msg.ScanMarkers).
	Refs []string
	// Prime asks the bridge to re-orient the agent after its context was compacted
	// (`otag prime` text): PrimeOutput puts it into this hook's output (HookDeliverer
	// renders it), PrimeMessage queues it as a system message to the run.
	Prime string
}

// HookResult.Prime values.
const (
	PrimeOutput  = "output"
	PrimeMessage = "message"
)

// Event types. See docs/ADAPTERS.md for how each one moves the run and task.
const (
	SessionStart = "session_start" // harness session bound/started (HarnessSessionID, TranscriptPath, Reason=startup|resume|clear)
	SessionEnd   = "session_end"   // harness process is exiting (Reason)
	TurnStart    = "turn_start"    // agent began working (prompt submitted)
	TurnEnd      = "turn_end"      // turn finished; agent idle (Text=last message, Reason=answer|aborted|error)
	Activity     = "activity"      // progress inside a turn (tool call); proves liveness
	Question     = "question"      // agent asks the user (Text)
	Permission   = "permission"    // agent waits for a permission decision (Text, Tool)
	Usage        = "usage"         // token usage (Usage)
	Error        = "error"         // harness/provider error (Text)
	RateLimit    = "rate_limit"    // provider limit hit (Text, ResetsAt)
)

// Tool phases (Event.ToolPhase).
const (
	ToolStart = "start"
	ToolEnd   = "end"
)

// EventTypes lists every normalized event type.
var EventTypes = []string{SessionStart, SessionEnd, TurnStart, TurnEnd, Activity, Question, Permission, Usage, Error, RateLimit}

// Event is one normalized event.
type Event struct {
	Type             string
	HarnessSessionID string
	TranscriptPath   string
	Reason           string
	Text             string
	Tool             string
	// ToolPhase marks an activity as the start (ToolStart) or the end (ToolEnd) of a
	// tool call, "" when unknown. While more tools started than ended in the current
	// turn the daemon allows a longer silence (tool_stall_after, docs/DAEMON.md): a
	// long Bash command or build emits no events.
	ToolPhase string
	ResetsAt  time.Time
	Usage     *UsageData
	// Data is a small extract stored in events.data (never the whole payload; the raw
	// payload is stored separately).
	Data map[string]any
}

// UsageData is one usage observation. DedupeKey makes repeated reports idempotent
// (e.g. the transcript message id).
type UsageData struct {
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	CostUSD          *float64
	Source           string // transcript | rollout | sse | litellm | estimate | hook
	Estimated        bool
	DedupeKey        string
}

// Target addresses a live session for Stopper/Deliverer.
type Target struct {
	RunID            string
	HarnessSessionID string
	ServerURL        string
	Pane             Pane
	RunDir           string // the run's private dir (phase 4; "" when unknown)
}

// Pane controls the run's tmux pane without exposing tmux details.
type Pane interface {
	SendKeys(ctx context.Context, keys ...string) error // tmux key names: "Enter", "Escape", "C-c", "y"
	SendText(ctx context.Context, text string) error    // literal text, no Enter
	Capture(ctx context.Context) (string, error)        // visible text
}

// Message is one delivery attempt for Deliverer (phase 4). Text is the rendered
// message(s), each starting with its marker "[otag#d<delivery id>]"; a hook payload that
// later shows the marker (e.g. the UserPromptSubmit prompt) proves the delivery.
type Message struct {
	// ID identifies the attempt for harness-side idempotency (a retry of the same
	// attempt must not deliver twice). It is derived from Nonce; a harness with its own
	// id format gets a prefixed form (OpenCode msg_*).
	ID   string
	Text string
	// Steer asks for delivery inside the current turn instead of after it.
	Steer bool
	// Nonce is the attempt nonce (message_deliveries.attempt_nonce), unique per attempt.
	Nonce string
}
