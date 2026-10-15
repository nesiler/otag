// Package cli implements the otag command line.
package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/guard"
	"github.com/nesiler/otag/internal/identity"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/run"
)

// EnvelopeVersion is the version of the --json output envelope.
const EnvelopeVersion = 1

// Exit codes.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitRetryable = 75 // EX_TEMPFAIL: safe to retry
)

// Env is the process environment the CLI runs in; injected for tests.
type Env struct {
	Stdin          io.Reader // hook payloads (`otag event`)
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	// Environ lists the environment (for prefix matches such as LC_*); nil = the
	// variable names of os.Environ, looked up through Getenv.
	Environ func() []string
	Cwd     string
	// Build identifies the binary for `otag version`; see ResolveBuild.
	Build Build
}

type app struct {
	env       Env
	json      bool
	project   string
	run       string
	requestID string
	replayed  bool
	db        *db.DB
}

// result is what a command produces: data for --json and a human renderer.
type result struct {
	data  any
	human func(w io.Writer)
}

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, a *app, args []string) (result, error)
}

func commands() []command {
	return []command{
		{"sync", "validate the TOML registry and sync it into the database", cmdSync},
		{"profiles", "list profiles [--tag t]", cmdProfiles},
		{"task", "manage tasks: add|list|show|edit|cancel|ready|claim|block|deliver|me", cmdTask},
		{"run", "manage runs: start|list|show|stop [--park]|attach|resume", cmdRun},
		{"status", "task and run overview per project", cmdStatus},
		{"stats", "usage per model; runs, durations and review reject rate per profile", cmdStats},
		{"quota", "quota per account and window [--refresh] [--account A]", cmdQuota},
		{"send", `send a message: otag send <run|task|coord|human>[,...] "text" [--kind info|command|question]`, cmdSend},
		{"inbox", "your messages: otag inbox [--wait] [--peek] [--all]", cmdInbox},
		{"reply", `reply to a message: otag reply <msg-id> "text" (answers an open question)`, cmdReply},
		{"needs", "what waits for you: open questions, unknown deliveries, blocked tasks, idle runs", cmdNeeds},
		{"tui", "full-screen view of projects, tasks and runs with a live preview: otag tui [--project P]", cmdTui},
		{"coord", "start or attach the project's coordinator: otag coord <project>", cmdCoord},
		{"review", `record a review: otag review <task> --accept ["note"] | --reject "reason"`, cmdReview},
		{"done", `child: finish the run: otag done "summary"`, cmdDone},
		{"fail", `child: give up the run: otag fail "reason"`, cmdFail},
		{"ask", `child: ask the coordinator, then end the turn: otag ask "question" [--park]`, cmdAsk},
		{"event", "hook bridge: otag event <harness> <kind> < payload", nil},
		{"prime", "orientation for the calling agent: identity, task or project, commands, unread messages", cmdPrime},
		{"skill", "agent skill (human only): otag skill install|uninstall|status [--dir HOME]", cmdSkill},
		{"context", "show the resolved identity (project, run) and paths", cmdContext},
		{"version", "print the otag version: otag version | otag --version", cmdVersion},
		{"doctor", "check database, registry and harness binaries", cmdDoctor},
		{"daemon", "background service: otag daemon [--once] | install|uninstall|status", cmdDaemon},
		{"notify", "notifications: list [--all] | ack ID...|--all", cmdNotify},
	}
}

// Main runs the CLI and returns the process exit code.
func Main(ctx context.Context, args []string, env Env) int {
	a := &app{env: env}
	defer func() {
		if a.db != nil {
			_ = a.db.Close()
		}
	}()

	fs := a.flagSet("otag")
	showVersion := fs.Bool("version", false, "print the version")
	err := fs.Parse(args)
	rest := fs.Args()
	if err == nil && *showVersion && len(rest) == 0 {
		rest = []string{"version"}
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		a.usage()
		return ExitOK
	case err != nil:
		return a.finish(result{}, usageErr("%v", err))
	case len(rest) == 0:
		a.usage()
		return a.finish(result{}, usageErr("missing command"))
	}
	switch rest[0] {
	case "event":
		return a.hookMain(ctx, rest[1:])
	case "statusline": // hidden: a harness statusLine command (docs/QUOTA.md)
		return cmdStatusline(ctx, a, rest[1:])
	case "sidecar": // hidden: started by otag in a run's window
		res, err := cmdSidecar(ctx, a, rest[1:])
		return a.finish(res, err)
	}
	for _, c := range commands() {
		if c.name == rest[0] && c.run != nil {
			if err := a.childGate(ctx, rest); err != nil {
				return a.finish(result{}, err)
			}
			res, err := c.run(ctx, a, rest[1:])
			return a.finish(res, err)
		}
	}
	if rest[0] == "help" {
		a.usage()
		return ExitOK
	}
	return a.finish(result{}, usageErr("unknown command %q", rest[0]))
}

// childGate refuses coordinator commands inside a run (OTAG_RUN_ID set): a child may
// run only its own commands and read-only views (guard.IsChildCommand), whether a
// shell line the guard saw started it or a script it did not see. A coordinator run
// (role coordinator, its OTAG_RUN_TOKEN verified) is not limited. Messaging commands
// open to a child check their own scope (send only to coord, reply only to its own
// messages; cli/msg.go).
//
// A caller without OTAG_RUN_ID that still looks like a run (runResidue: a child that
// dropped OTAG_RUN_ID) gets only the child commands that never act as the human: not
// the ones in callerCommands, which would otherwise take it for the human.
func (a *app) childGate(ctx context.Context, args []string) error {
	runID := a.env.Getenv("OTAG_RUN_ID")
	why := ""
	if runID == "" {
		if why = a.runResidue(); why == "" {
			return nil
		}
		runID = "(unknown: " + why + ")"
	}
	sub := ""
	for i := 1; i < len(args); i++ {
		name, _, attached := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch {
		case strings.HasPrefix(args[i], "-"):
			if !attached && (name == "project" || name == "run" || name == "request-id") {
				i++
			}
			continue
		}
		sub = args[i]
		break
	}
	if why != "" && slices.Contains(callerCommands, args[0]) {
		return apperr.New(apperr.Forbidden, "otag %s without OTAG_RUN_ID, but %s: a run acts only as itself", args[0], why)
	}
	if guard.IsChildCommand(args[0], sub) {
		return nil
	}
	if why == "" {
		d, err := a.openDB(ctx)
		if err != nil {
			return err
		}
		// The role is a privilege only for the run that holds its token.
		if err := run.CheckToken(ctx, d, runID, a.env.Getenv(run.TokenEnv)); err != nil {
			if e, ok := apperr.As(err); ok && (e.Code == apperr.Forbidden || e.Code == apperr.NotFound) {
				return apperr.New(apperr.Forbidden, "inside run %s, `otag %s`: %s", runID, strings.TrimSpace(args[0]+" "+sub), e.Message)
			}
			return err
		}
		// A run without a token hash (created before tokens) never holds the role.
		var role string
		if d.QueryRowContext(ctx, `SELECT role FROM runs WHERE id = ? AND token_hash IS NOT NULL`, runID).Scan(&role) == nil && role == "coordinator" {
			return nil
		}
	}
	return apperr.New(apperr.Forbidden, "inside run %s, `otag %s` is a coordinator command; a child runs otag done, fail, "+
		"ask, inbox, reply, send coord, task me, context, status and read-only list/show", runID, strings.TrimSpace(args[0]+" "+sub))
}

// callerCommands are the child commands that act as their caller, the human when no
// run is set (cli/msg.go msgCaller, cmdReview): refused to a caller with run residue
// and no OTAG_RUN_ID.
var callerCommands = []string{"ask", "inbox", "reply", "send", "review", "prime"}

func (a *app) usage() {
	w := a.env.Stderr
	_, _ = fmt.Fprintln(w, "usage: otag [--json] [--project P] [--run R] [--request-id ID] <command> [args]\n       otag --version")
	_, _ = fmt.Fprintln(w, "\ncommands:")
	for _, c := range commands() {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
}

// flagSet returns a FlagSet carrying the global flags, so they work before or after the command.
func (a *app) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&a.json, "json", a.json, "emit the versioned JSON envelope")
	fs.StringVar(&a.project, "project", a.project, "project name")
	fs.StringVar(&a.run, "run", a.run, "run id")
	fs.StringVar(&a.requestID, "request-id", a.requestID, "idempotency key for mutations")
	return fs
}

// parse parses flags anywhere among positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, usageErr("%s: see README for flags", fs.Name())
			}
			return nil, usageErr("%s: %v", fs.Name(), err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func usageErr(format string, args ...any) error {
	return apperr.New(apperr.Usage, format, args...)
}

type envelope struct {
	V     int      `json:"v"`
	OK    bool     `json:"ok"`
	Data  any      `json:"data"`
	Error *errBody `json:"error,omitempty"`
}

type errBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// finish writes the result or error and returns the exit code.
func (a *app) finish(res result, err error) int {
	if a.replayed {
		_, _ = fmt.Fprintf(a.env.Stderr, "otag: replayed result of request %s\n", a.requestID)
	}
	if err == nil {
		if a.json {
			a.writeJSON(envelope{V: EnvelopeVersion, OK: true, Data: res.data})
		} else if res.human != nil {
			res.human(a.env.Stdout)
		}
		return ExitOK
	}
	e, ok := apperr.As(err)
	if !ok {
		e = &apperr.Error{Code: apperr.Internal, Message: err.Error()}
	}
	if a.json {
		a.writeJSON(envelope{V: EnvelopeVersion, OK: false, Data: res.data,
			Error: &errBody{Code: e.Code, Message: e.Message, Retryable: e.Retryable}})
	} else if res.human != nil {
		res.human(a.env.Stdout)
	}
	_, _ = fmt.Fprintf(a.env.Stderr, "otag: %s (%s)\n", e.Message, e.Code)
	switch {
	case e.Code == apperr.Usage:
		return ExitUsage
	case e.Retryable:
		return ExitRetryable
	}
	return ExitError
}

func (a *app) writeJSON(v any) {
	enc := json.NewEncoder(a.env.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (a *app) dbPath() string    { return paths.DB(a.env.Getenv) }
func (a *app) configDir() string { return paths.ConfigDir(a.env.Getenv) }

func (a *app) openDB(ctx context.Context) (*db.DB, error) {
	if a.db != nil {
		return a.db, nil
	}
	d, err := db.Open(ctx, a.dbPath())
	if err != nil {
		if _, ok := apperr.As(err); ok {
			return nil, err
		}
		return nil, fmt.Errorf("open %s: %w", a.dbPath(), err)
	}
	a.db = d
	return d, nil
}

func (a *app) identity(ctx context.Context) (identity.Identity, error) {
	d, err := a.openDB(ctx)
	if err != nil {
		return identity.Identity{}, err
	}
	return identity.Resolve(ctx, d, identity.Input{
		ProjectFlag: a.project, RunFlag: a.run,
		EnvProject: a.env.Getenv("OTAG_PROJECT"), EnvRun: a.env.Getenv("OTAG_RUN_ID"),
		Cwd: a.env.Cwd,
	})
}

func (a *app) actorName(id identity.Identity) string {
	if id.RunID != "" {
		return "run:" + id.RunID
	}
	if u := a.env.Getenv("USER"); u != "" {
		return "user:" + u
	}
	return "user"
}

// mutate runs fn in a write transaction. With --request-id the first successful
// result is stored in the same transaction and replayed for later identical calls.
func mutate[T any](ctx context.Context, a *app, key opKey, fn func(tx *sql.Tx) (T, error)) (T, error) {
	var out T
	d, err := a.openDB(ctx)
	if err != nil {
		return out, err
	}
	kb, err := json.Marshal(key)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(kb)
	hash := hex.EncodeToString(sum[:])
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		if a.requestID != "" {
			var storedHash, stored string
			err := tx.QueryRowContext(ctx, `SELECT args_hash, result FROM request_log WHERE request_id = ?`,
				a.requestID).Scan(&storedHash, &stored)
			switch {
			case err == nil && storedHash != hash:
				return apperr.New(apperr.RequestIDReused, "request id %s was used for a different command", a.requestID)
			case err == nil:
				a.replayed = true
				return json.Unmarshal([]byte(stored), &out)
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		var err error
		if out, err = fn(tx); err != nil {
			return err
		}
		if a.requestID == "" {
			return nil
		}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO request_log (request_id, command, args_hash, result) VALUES (?, ?, ?, ?)`,
			a.requestID, key.Command, hash, string(b))
		return err
	})
	return out, err
}

// opKey identifies a mutation for --request-id. It is hashed as JSON, so repeated
// flag values and positional arguments keep their boundaries.
type opKey struct {
	Command string         `json:"command"`
	Project string         `json:"project"`
	Run     string         `json:"run"`
	Flags   map[string]any `json:"flags"` // encoding/json sorts the keys
	Args    []string       `json:"args"`
}

func requestKey(cmd string, id identity.Identity, fs *flag.FlagSet, pos []string) opKey {
	k := opKey{Command: cmd, Project: projectName(id), Run: id.RunID, Flags: map[string]any{}, Args: pos}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "json", "request-id", "project", "run":
			return
		}
		if m, ok := f.Value.(*multi); ok {
			k.Flags[f.Name] = []string(*m)
		} else {
			k.Flags[f.Name] = f.Value.String()
		}
	})
	return k
}

func projectName(id identity.Identity) string {
	if id.Project == nil {
		return ""
	}
	return id.Project.Name
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }
