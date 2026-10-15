package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

// payload holds the fields otag reads from Claude Code hook input (and from the mod's
// turn.complete report). Unknown fields are ignored.
type payload struct {
	SessionID       string          `json:"session_id"`
	TranscriptPath  string          `json:"transcript_path"`
	PromptID        string          `json:"prompt_id"`
	ScratchpadDir   string          `json:"scratchpad_dir"`
	Cwd             string          `json:"cwd"`
	Source          string          `json:"source"` // SessionStart
	Reason          string          `json:"reason"` // SessionEnd; mod: aborted|answer (older builds)
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	ToolUseID       string          `json:"tool_use_id"`
	LastMessage     string          `json:"last_assistant_message"`
	Error           string          `json:"error"`         // StopFailure type; PostToolUseFailure text
	ErrorDetails    string          `json:"error_details"` // StopFailure
	AgentID         string          `json:"agent_id"`
	AgentType       *string         `json:"agent_type"`
	AgentTranscript string          `json:"agent_transcript_path"`
	Message         string          `json:"message"` // Notification, Elicitation
	MCPServerName   string          `json:"mcp_server_name"`
	StopHookActive  bool            `json:"stop_hook_active"` // Stop: it follows a Stop block

	// mod-turn-complete
	Run       string `json:"run"`
	TurnID    any    `json:"turnId"`
	ModAgent  any    `json:"agentId"`
	IsAborted *bool  `json:"isAborted"`
	Answer    string `json:"answer"`
}

type toolInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"` // Grep
	Questions    []struct {
		Question string `json:"question"`
	} `json:"questions"`
}

// fileTools write the file named in tool_input.file_path (notebook_path).
var fileTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// ParseEvent implements adapter.Adapter. kind is the Claude Code hook event name, or
// mod-turn-complete for the mod's report. It is pure apart from a bounded read of the
// session transcript on Stop, StopFailure, SubagentStop and SessionEnd (usage).
func (a *Adapter) ParseEvent(hc adapter.HookContext, kind string, raw []byte) (adapter.HookResult, error) {
	var p payload
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return adapter.HookResult{}, fmt.Errorf("%s payload: %w", kind, err)
		}
	}
	res := adapter.HookResult{SessionRef: p.SessionID}
	sid := p.SessionID
	var in toolInput
	var inErr error
	if len(p.ToolInput) > 0 {
		inErr = json.Unmarshal(p.ToolInput, &in) // tool inputs vary; missing fields stay empty
	}
	if p.ToolUseID != "" {
		res.EventID = p.ToolUseID + ":" + kind
	}

	switch kind {
	case "SessionStart":
		res.Events = ev(adapter.Event{Type: adapter.SessionStart, HarnessSessionID: sid,
			TranscriptPath: p.TranscriptPath, Reason: p.Source})
		// A compacted or resumed session may have lost otag's first prompt.
		if p.Source == "compact" || p.Source == "resume" {
			res.Prime = adapter.PrimeOutput
		}

	case "UserPromptSubmit":
		// Usage of the previous turn: an interrupted turn has no Stop of its own.
		res.Events = append(ev(adapter.Event{Type: adapter.TurnStart, HarnessSessionID: sid}),
			usageEvents(p.TranscriptPath)...)
		// The bridge proves deliveries whose marker is in the prompt (a mod submit).
		res.Slot = &adapter.DeliverySlot{Channel: adapter.ChanPromptContext}

	case "PreToolUse":
		res.Output = a.guard(hc, p, in, inErr)
		switch p.ToolName {
		case "AskUserQuestion":
			res.Events = ev(adapter.Event{Type: adapter.Question, HarnessSessionID: sid, Tool: p.ToolName,
				Text: questionText(in)})
		case "ExitPlanMode":
			res.Events = ev(adapter.Event{Type: adapter.Question, HarnessSessionID: sid, Tool: p.ToolName,
				Text: "plan approval"})
		default:
			res.Events = ev(adapter.Event{Type: adapter.Activity, HarnessSessionID: sid, Tool: p.ToolName,
				ToolPhase: adapter.ToolStart})
		}

	case "PostToolUse", "PostToolUseFailure":
		// AskUserQuestion and ExitPlanMode report a question, not a tool start, so their
		// end is no tool end either.
		phase := adapter.ToolEnd
		if p.ToolName == "AskUserQuestion" || p.ToolName == "ExitPlanMode" {
			phase = ""
		}
		res.Events = ev(adapter.Event{Type: adapter.Activity, HarnessSessionID: sid, Tool: p.ToolName, ToolPhase: phase})
		if p.AgentID == "" {
			// A subagent's tool hook would hand the messages to the subagent.
			res.Slot = &adapter.DeliverySlot{Channel: adapter.ChanToolContext}
		}

	case "PermissionRequest":
		// AskUserQuestion asks for "permission" even in bypass mode; its PreToolUse
		// already reported the question.
		if p.ToolName != "AskUserQuestion" {
			res.Events = ev(adapter.Event{Type: adapter.Permission, HarnessSessionID: sid, Tool: p.ToolName,
				Text: permissionText(p.ToolName, in)})
		}

	case "Elicitation":
		text := p.Message
		if text == "" {
			text = "MCP server " + p.MCPServerName + " asks for input"
		}
		res.Events = ev(adapter.Event{Type: adapter.Question, HarnessSessionID: sid,
			Tool: "mcp:" + p.MCPServerName, Text: text})

	case "Stop":
		res.Events = append(ev(adapter.Event{Type: adapter.TurnEnd, HarnessSessionID: sid,
			Text: p.LastMessage, Reason: "answer"}), usageEvents(p.TranscriptPath)...)
		if p.AgentID == "" {
			res.Slot = &adapter.DeliverySlot{Channel: adapter.ChanStopBlock, Continued: p.StopHookActive}
		}

	case "StopFailure":
		// The turn ended on an API error: report it and end the turn.
		text := p.LastMessage
		if text == "" {
			text = p.Error
		}
		if p.ErrorDetails != "" && !strings.Contains(text, p.ErrorDetails) {
			text += " (" + p.ErrorDetails + ")"
		}
		typ := adapter.Error
		if p.Error == "rate_limit" {
			typ = adapter.RateLimit
		}
		res.Events = append(ev(
			adapter.Event{Type: typ, HarnessSessionID: sid, Text: text, Data: map[string]any{"error": p.Error}},
			adapter.Event{Type: adapter.TurnEnd, HarnessSessionID: sid, Text: text, Reason: "error"},
		), usageEvents(p.TranscriptPath)...)

	case "SubagentStop":
		// Only usage, never a state change: Claude Code's own helpers (prompt
		// suggestion, /btw) also fire SubagentStop, after the turn, with an empty
		// agent_type or (under --agent) the main agent's type, so the payload cannot
		// tell them from a real subagent. A real subagent's progress shows through its
		// tool hooks and the Agent tool's PostToolUse. Helpers' tokens are real spend.
		res.Events = usageEvents(p.AgentTranscript)

	case "SessionEnd":
		events := usageEvents(p.TranscriptPath)
		// /clear and /resume (also /branch) end this session id but not the process: the
		// SessionStart that follows (source clear/resume) rebinds the run.
		if p.Reason != "clear" && p.Reason != "resume" {
			events = append(events, adapter.Event{Type: adapter.SessionEnd, HarnessSessionID: sid, Reason: p.Reason})
		}
		res.Events = events

	case ModTurnComplete:
		if id := idString(p.ModAgent); id != "" {
			return res, nil // a subagent's turn
		}
		reason := "answer"
		if (p.IsAborted != nil && *p.IsAborted) || p.Reason == "aborted" {
			reason = "aborted"
		}
		res.Events = ev(adapter.Event{Type: adapter.TurnEnd, Reason: reason, Text: p.Answer})
		if id := idString(p.TurnID); id != "" {
			run := p.Run
			if run == "" {
				run = hc.RunID
			}
			res.EventID = "mod:" + run + ":" + id
		}

	default:
		// Notification and anything else: stored raw, no state change.
	}
	return res, nil
}

func ev(es ...adapter.Event) []adapter.Event { return es }

func idString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return fmt.Sprintf("%.0f", x)
	default:
		return fmt.Sprint(x)
	}
}

// readTools read the file or directory named in tool_input.file_path (Grep: path).
var readTools = map[string]bool{"Read": true, "Grep": true, "NotebookRead": true}

// guard runs the shared guard on a PreToolUse and returns Claude's deny decision, or
// nil (no decision: the bridge prints nothing): Bash commands, file writes, reads of
// protected files, and the path arguments of MCP tools (otag cannot tell whether they
// write). A guarded tool whose input cannot be read is refused. Writes into Claude's
// own scratchpad directory are allowed.
func (a *Adapter) guard(hc adapter.HookContext, p payload, in toolInput, inErr error) []byte {
	if hc.Workdir == "" {
		return nil
	}
	// Relative paths in a Bash command resolve against the shell's cwd, which a
	// previous `cd` may have moved; the boundary stays the workspace.
	pol := hc.GuardPolicy()
	pol.Cwd = p.Cwd
	if p.ScratchpadDir != "" {
		pol.AllowOutside = []string{p.ScratchpadDir}
	}
	malformed := guard.Decision{Reason: "otag guard: the " + p.ToolName + " input cannot be checked; refusing it"}
	var d guard.Decision
	switch {
	case p.ToolName == "Bash":
		d = pol.CheckCommand(in.Command)
		if (inErr != nil || strings.TrimSpace(in.Command) == "") && !pol.Level.IsLight() {
			d = malformed // a light run allows what it cannot inspect
		}
	case fileTools[p.ToolName]:
		target := in.FilePath
		if target == "" {
			target = in.NotebookPath
		}
		d = pol.CheckWrite(target)
		if inErr != nil || target == "" {
			d = malformed
		}
	case readTools[p.ToolName]:
		target := in.FilePath
		if target == "" {
			target = in.Path
		}
		d = pol.CheckRead(target)
	case strings.HasPrefix(p.ToolName, "mcp__"):
		d = pol.CheckToolArgs(p.ToolName, p.ToolInput)
	default:
		return nil
	}
	if d.Allow {
		return nil
	}
	return a.DenyOutput("PreToolUse", d.Reason)
}

// GuardKind implements adapter.Guarder: PreToolUse decides; PermissionRequest (seen
// only outside bypass mode, and for AskUserQuestion) is refused when otag cannot apply
// the run's policy.
func (*Adapter) GuardKind(kind string) bool {
	return kind == "PreToolUse" || kind == "PermissionRequest"
}

// DenyOutput implements adapter.Guarder.
func (*Adapter) DenyOutput(kind, reason string) []byte {
	var out map[string]any
	if kind == "PermissionRequest" {
		out = map[string]any{"hookEventName": kind, "decision": map[string]any{"behavior": "deny", "message": reason}}
	} else {
		out = map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": out})
	return b
}

func questionText(in toolInput) string {
	var qs []string
	for _, q := range in.Questions {
		if q.Question != "" {
			qs = append(qs, q.Question)
		}
	}
	return strings.Join(qs, " / ")
}

func permissionText(tool string, in toolInput) string {
	switch {
	case in.Command != "":
		return tool + ": " + in.Command
	case in.FilePath != "":
		return tool + ": " + in.FilePath
	}
	return tool
}

// NoopOutput implements adapter.Adapter: Claude Code treats empty output of a command
// hook as "no decision".
func (*Adapter) NoopOutput(string) []byte { return nil }
