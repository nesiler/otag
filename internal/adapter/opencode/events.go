package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

// Event kinds that are not OpenCode SSE types. SSE frames use their own type as kind.
const (
	KindSession   = "otag.session"        // sidecar: session bound (once per sidecar)
	KindReconcile = "otag.reconcile"      // sidecar: state after a (re)connect
	KindTUIExit   = "otag.tui.exit"       // tui.sh: the TUI process exited
	KindEvaluate  = "permission.evaluate" // plugin: guard question
)

// envelope is one SSE frame of GET /api/event. The sidecar may add "otag" (things the
// stream only says in earlier frames: the last assistant text, the step's model).
type envelope struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	Otag *extra          `json:"otag,omitempty"`
}

type extra struct {
	Text  string `json:"text,omitempty"`
	Model string `json:"model,omitempty"`
	Sub   bool   `json:"sub,omitempty"` // the event is of a subagent session, not the run's own
}

type sessionError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// permissionRequest is Permission.Request (permission.asked, GET .../permission).
type permissionRequest struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Message   string   `json:"message"`
}

// form is Form.Info (form.created, GET .../form).
type form struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Title     string `json:"title"`
	Fields    []struct {
		Key         string `json:"key"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Options     []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"fields"`
}

// data is the union of the fields otag reads from SSE frame data.
type data struct {
	SessionID          string        `json:"sessionID"`
	ParentID           string        `json:"parentID"`
	AssistantMessageID string        `json:"assistantMessageID"`
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	Reason             string        `json:"reason"`
	Error              *sessionError `json:"error"`
	Attempt            int           `json:"attempt"`
	At                 int64         `json:"at"`
	Cost               *float64      `json:"cost"`
	Tokens             *tokens       `json:"tokens"`
	Model              *Model        `json:"model"`
	Text               string        `json:"text"`
	Action             string        `json:"action"`
	Resources          []string      `json:"resources"`
	Message            string        `json:"message"`
	RequestID          string        `json:"requestID"`
	Reply              string        `json:"reply"`
	Form               *form         `json:"form"`
	InboxID            string        `json:"inboxID"`
}

// ParseEvent implements adapter.Adapter. It is pure: everything comes from the
// payload and hc.
func (a *Adapter) ParseEvent(hc adapter.HookContext, kind string, raw []byte) (adapter.HookResult, error) {
	switch kind {
	case KindEvaluate:
		return parseEvaluate(hc, raw)
	case KindTUIExit:
		var p struct {
			Code int `json:"code"`
		}
		if err := unmarshal(raw, &p); err != nil {
			return adapter.HookResult{}, err
		}
		return adapter.HookResult{Events: []adapter.Event{{Type: adapter.SessionEnd, Reason: "exit",
			Data: map[string]any{"exit_code": p.Code}}}}, nil
	case KindSession:
		var p struct {
			SessionID string `json:"sessionID"`
			Reason    string `json:"reason"`
		}
		if err := unmarshal(raw, &p); err != nil {
			return adapter.HookResult{}, err
		}
		if p.SessionID == "" {
			return adapter.HookResult{}, fmt.Errorf("opencode: %s without sessionID", kind)
		}
		reason := p.Reason
		if reason == "" {
			reason = "startup"
		}
		return adapter.HookResult{SessionRef: p.SessionID, Events: []adapter.Event{{Type: adapter.SessionStart,
			HarnessSessionID: p.SessionID, Reason: reason}}}, nil
	case KindReconcile:
		return parseReconcile(hc, raw)
	}

	var env envelope
	if err := unmarshal(raw, &env); err != nil {
		return adapter.HookResult{}, err
	}
	var d data
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return adapter.HookResult{}, fmt.Errorf("opencode: %s data: %w", kind, err)
		}
	}
	res := adapter.HookResult{SessionRef: d.SessionID, EventID: env.ID}
	if d.Form != nil && res.SessionRef == "" {
		res.SessionRef = d.Form.SessionID
	}
	ev := func(e adapter.Event) { res.Events = append(res.Events, e) }

	switch kind {
	case "session.execution.started":
		ev(adapter.Event{Type: adapter.TurnStart})
	case "session.execution.succeeded":
		e := adapter.Event{Type: adapter.TurnEnd, Reason: "answer"}
		if env.Otag != nil {
			e.Text = env.Otag.Text
		}
		ev(e)
	case "session.execution.interrupted":
		ev(adapter.Event{Type: adapter.TurnEnd, Reason: "aborted", Data: map[string]any{"interrupt": d.Reason}})
	case "session.execution.failed":
		ev(errorEvent(d.Error, time.Time{}))
		ev(adapter.Event{Type: adapter.TurnEnd, Reason: "error", Text: errText(d.Error)})
	case "session.retry.scheduled":
		var at time.Time
		if d.At > 0 {
			at = time.UnixMilli(d.At)
		}
		e := errorEvent(d.Error, at)
		e.Data = map[string]any{"attempt": d.Attempt, "retry": true}
		if d.Error != nil {
			e.Data["error_type"] = d.Error.Type
		}
		ev(e)
	case "permission.asked":
		ev(adapter.Event{Type: adapter.Permission, Tool: d.Action, Text: permissionText(d.Action, d.Resources, d.Message),
			Data: map[string]any{"request": d.ID}})
	case "permission.replied":
		ev(adapter.Event{Type: adapter.Activity, Tool: "permission", Data: map[string]any{"reply": d.Reply, "request": d.RequestID}})
	case "form.created":
		if d.Form != nil {
			ev(adapter.Event{Type: adapter.Question, Tool: "question", Text: formText(*d.Form),
				Data: map[string]any{"form": d.Form.ID}})
		}
	case "form.replied", "form.cancelled":
		ev(adapter.Event{Type: adapter.Activity, Tool: "question"})
	case "session.step.started":
		ev(adapter.Event{Type: adapter.Activity})
	case "session.tool.input.started":
		ev(adapter.Event{Type: adapter.Activity, Tool: d.Name, ToolPhase: adapter.ToolStart})
	case "session.tool.success", "session.tool.failed":
		e := adapter.Event{Type: adapter.Activity, ToolPhase: adapter.ToolEnd}
		if kind == "session.tool.failed" && d.Error != nil {
			e.Data = map[string]any{"tool_error": d.Error.Type}
		}
		ev(e)
	case "session.step.ended", "session.step.failed":
		// One step = one assistant message: the message id is the stable key (also used
		// by the sidecar's reconcile from the message list).
		model := ""
		if env.Otag != nil {
			model = env.Otag.Model
		}
		if u := usage(d, d.AssistantMessageID, model); u != nil {
			ev(adapter.Event{Type: adapter.Usage, Usage: u})
		}
	case "session.compaction.ended", "session.compaction.failed":
		// Only the run's own session loses otag's orientation; a subagent's compaction
		// still counts for usage below.
		if kind == "session.compaction.ended" && (env.Otag == nil || !env.Otag.Sub) {
			res.Prime = adapter.PrimeMessage
		}
		// Compactions carry no message id: the frame id is the stable key.
		model := ""
		if d.Model != nil {
			model = d.Model.ProviderID + "/" + d.Model.ID
		} else if env.Otag != nil {
			model = env.Otag.Model
		}
		if env.ID != "" {
			if u := usage(d, "compaction:"+env.ID, model); u != nil {
				ev(adapter.Event{Type: adapter.Usage, Usage: u})
			}
		}
	case "session.deleted":
		ev(adapter.Event{Type: adapter.SessionEnd, HarnessSessionID: d.SessionID, Reason: "deleted"})
	case KindInboxDelivered:
		// The session took an inbox item: proof of a delivery with that receipt.
		if d.InboxID != "" {
			res.Refs = []string{d.InboxID}
		}
	}
	// Everything else (deltas, reasoning, inbox, step.streamed, catalog updates) is
	// noise: no events, the raw payload is still stored.
	return res, nil
}

// errorEvent is rate_limit for provider limits and quotas, error otherwise.
func errorEvent(e *sessionError, resetsAt time.Time) adapter.Event {
	typ := adapter.Error
	if e != nil && (e.Type == "provider.rate-limit" || e.Type == "provider.quota") {
		typ = adapter.RateLimit
	}
	return adapter.Event{Type: typ, Text: errText(e), ResetsAt: resetsAt}
}

func errText(e *sessionError) string {
	if e == nil {
		return "unknown error"
	}
	if e.Type == "" {
		return e.Message
	}
	return e.Type + ": " + e.Message
}

func permissionText(action string, resources []string, message string) string {
	s := action
	if len(resources) > 0 {
		s += ": " + strings.Join(resources, ", ")
	}
	if message != "" {
		s += " (" + message + ")"
	}
	return s
}

func formText(f form) string {
	parts := []string{}
	if f.Title != "" {
		parts = append(parts, f.Title)
	}
	for _, fl := range f.Fields {
		q := fl.Title
		if q == "" {
			q = fl.Description
		}
		if q == "" {
			q = fl.Key
		}
		if len(fl.Options) > 0 {
			opts := make([]string, 0, len(fl.Options))
			for _, o := range fl.Options {
				opts = append(opts, o.Label)
			}
			q += " [" + strings.Join(opts, " | ") + "]"
		}
		if q != "" && q != f.Title {
			parts = append(parts, q)
		}
	}
	return strings.Join(parts, " — ")
}

// usage turns reported tokens into a usage row deduplicated on key. Cost is kept
// only when OpenCode reports one: 0 is also what LiteLLM without a cost header looks
// like.
func usage(d data, key, model string) *adapter.UsageData {
	if d.Tokens == nil || key == "" {
		return nil
	}
	t := d.Tokens
	if t.Input == 0 && t.Output == 0 && t.Reasoning == 0 && t.Cache.Read == 0 && t.Cache.Write == 0 {
		return nil
	}
	u := &adapter.UsageData{
		InputTokens: t.Input, OutputTokens: t.Output, ReasoningTokens: t.Reasoning,
		CacheReadTokens: t.Cache.Read, CacheWriteTokens: t.Cache.Write,
		Source: "sse", DedupeKey: key, Model: model,
	}
	if d.Cost != nil && *d.Cost > 0 {
		c := *d.Cost
		u.CostUSD = &c
	}
	return u
}

// evaluation is the plugin's guard question.
type evaluation struct {
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Tool      string   `json:"tool"`
	Workdir   string   `json:"workdir"` // the shell tool's workdir argument, if any
	CallID    string   `json:"callID"`
	Effect    string   `json:"effect"`
}

// editTools are the OpenCode tools that write files.
var editTools = map[string]bool{"edit": true, "write": true, "patch": true, "apply_patch": true, "multiedit": true}

// parseEvaluate runs the shared guard.
//   - shell: resources are the simple commands of the line, checked as one script
//     (a `cd` moves the directory for the commands after it) from the tool's own
//     workdir, which must be inside the workspace (a light run allows any);
//   - edit: location-relative (absolute outside) paths;
//   - external_directory ("<dir>/*"): denied for edit tools outside the workspace and
//     for a strict run's shell whose workdir is outside; other reads stay OpenCode's decision.
//
// A denial answers {"effect":"deny","message":...} and records an activity event; an
// allowed request answers nothing and records nothing.
func parseEvaluate(hc adapter.HookContext, raw []byte) (adapter.HookResult, error) {
	var e evaluation
	if err := unmarshal(raw, &e); err != nil {
		return adapter.HookResult{}, err
	}
	res := adapter.HookResult{SessionRef: e.SessionID}
	if hc.Workdir == "" {
		return res, nil // unknown run: the bridge refuses (Guarder)
	}
	d := evaluate(hc.GuardPolicy(), e)
	if d.Allow {
		return res, nil
	}
	res.Output = denyEffect(d.Reason)
	tool := e.Tool
	if tool == "" {
		tool = e.Action
	}
	res.Events = []adapter.Event{{Type: adapter.Activity, Tool: tool,
		Data: map[string]any{"guard": "deny", "action": e.Action, "reason": d.Reason}}}
	return res, nil
}

func evaluate(pol guard.Policy, e evaluation) guard.Decision {
	ok := guard.Decision{Allow: true}
	if (e.Action == "shell" || e.Tool == "shell") && e.Workdir != "" {
		// A light run lets commands run anywhere; the command checks still apply.
		if !pol.Inside(e.Workdir) && !pol.Level.IsLight() {
			return guard.Decision{Reason: "otag guard: commands must run inside the workspace (" + pol.Workdir +
				"), not in " + e.Workdir}
		}
		pol.Cwd = e.Workdir
	}
	switch e.Action {
	case "read":
		pol.Cwd = ""
		for _, r := range e.Resources {
			if d := pol.CheckRead(r); !d.Allow {
				return d
			}
		}
	case "shell":
		if len(e.Resources) > 0 {
			return pol.CheckCommand(strings.Join(e.Resources, "\n"))
		}
	case "edit":
		pol.Cwd = ""
		for _, r := range e.Resources {
			if strings.Contains(r, "*** Begin Patch") {
				if d := pol.CheckPatch(r); !d.Allow {
					return d
				}
				continue
			}
			if d := pol.CheckWrite(r); !d.Allow {
				return d
			}
		}
	case "external_directory":
		if !editTools[e.Tool] {
			return ok
		}
		pol.Cwd = ""
		for _, r := range e.Resources {
			if d := pol.CheckWrite(strings.TrimSuffix(strings.TrimSuffix(r, "*"), "/")); !d.Allow {
				return d
			}
		}
	}
	return ok
}

func denyEffect(reason string) []byte {
	out, _ := json.Marshal(map[string]string{"effect": "deny", "message": reason})
	return out
}

// GuardKind implements adapter.Guarder: the plugin's permission question decides.
func (*Adapter) GuardKind(kind string) bool { return kind == KindEvaluate }

// DenyOutput implements adapter.Guarder.
func (*Adapter) DenyOutput(_, reason string) []byte { return denyEffect(reason) }

var _ adapter.Guarder = (*Adapter)(nil)

// Reconcile is what the sidecar learned from the API after a reconnect: the stream
// drops events while disconnected (and OpenCode's local server keeps no replayable
// event log), so the sidecar reads the current state instead.
type Reconcile struct {
	SessionID   string              `json:"sessionID"`
	Active      bool                `json:"active"`
	Deleted     bool                `json:"deleted,omitempty"`
	Outcome     string              `json:"outcome,omitempty"` // last finished turn: succeeded|failed|interrupted
	Permissions []permissionRequest `json:"permissions"`
	Forms       []form              `json:"forms"`
}

// parseReconcile turns the reconciled state into the events the stream may have
// missed, relative to the run's current status. A turn that ended while the stream
// was down keeps its real ending (Session.Info.outcome).
func parseReconcile(hc adapter.HookContext, raw []byte) (adapter.HookResult, error) {
	var r Reconcile
	if err := unmarshal(raw, &r); err != nil {
		return adapter.HookResult{}, err
	}
	res := adapter.HookResult{SessionRef: r.SessionID}
	data := map[string]any{"reconciled": true}
	add := func(e adapter.Event) {
		e.Data = data
		res.Events = append(res.Events, e)
	}
	busy := hc.Status == "running" || hc.Status == "waiting"
	switch {
	case r.Deleted:
		add(adapter.Event{Type: adapter.SessionEnd, HarnessSessionID: r.SessionID, Reason: "deleted"})
	case len(r.Permissions) > 0:
		if p := r.Permissions[0]; hc.Status != "waiting" {
			add(adapter.Event{Type: adapter.Permission, Tool: p.Action, Text: permissionText(p.Action, p.Resources, p.Message)})
		}
	case len(r.Forms) > 0:
		if hc.Status != "waiting" {
			add(adapter.Event{Type: adapter.Question, Tool: "question", Text: formText(r.Forms[0])})
		}
	case r.Active:
		if hc.Status != "running" && hc.Status != "" {
			add(adapter.Event{Type: adapter.TurnStart})
		}
	case busy:
		switch r.Outcome {
		case "failed":
			add(adapter.Event{Type: adapter.Error, Text: "turn failed while otag was disconnected"})
			add(adapter.Event{Type: adapter.TurnEnd, Reason: "error"})
		case "interrupted":
			add(adapter.Event{Type: adapter.TurnEnd, Reason: "aborted"})
		default:
			add(adapter.Event{Type: adapter.TurnEnd, Reason: "answer"})
		}
	}
	return res, nil
}

func unmarshal(raw []byte, v any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("opencode: bad payload: %w", err)
	}
	return nil
}
