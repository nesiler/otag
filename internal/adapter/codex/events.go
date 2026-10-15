package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

type item struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Command string `json:"command"`
}
type turn struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Error  *codexError `json:"error"`
	Items  []item      `json:"items"`
}
type codexError struct {
	Message string          `json:"message"`
	Info    json.RawMessage `json:"codexErrorInfo"`
}
type thread struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Status status `json:"status"`
	Turns  []turn `json:"turns"`
}
type status struct {
	Type  string   `json:"type"`
	Flags []string `json:"activeFlags"`
}
type threadResult struct {
	Model  string `json:"model"`
	Thread thread `json:"thread"`
}
type tokens struct {
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	Cached     int64 `json:"cachedInputTokens"`
	CacheWrite int64 `json:"cacheWriteInputTokens"`
	Reasoning  int64 `json:"reasoningOutputTokens"`
	Total      int64 `json:"totalTokens"`
}
type payload struct {
	Cwd          string          `json:"cwd"`
	SessionID    string          `json:"session_id"`
	Transcript   string          `json:"transcript_path"`
	TurnID       string          `json:"turn_id"`
	ToolID       string          `json:"tool_use_id"`
	Tool         string          `json:"tool_name"`
	Input        json.RawMessage `json:"tool_input"`
	Source       string          `json:"source"`
	Reason       string          `json:"reason"`
	Message      string          `json:"last_assistant_message"`
	ThreadID     string          `json:"threadId"`
	ServerTurnID string          `json:"turnId"`
	Thread       thread          `json:"thread"`
	Turn         turn            `json:"turn"`
	Item         item            `json:"item"`
	Status       status          `json:"status"`
	Error        *codexError     `json:"error"`
	WillRetry    bool            `json:"willRetry"`
	Model        string          `json:"model"`
	UsageSource  string          `json:"usageSource"`
	TokenUsage   struct {
		Last  tokens `json:"last"`
		Total tokens `json:"total"`
	} `json:"tokenUsage"`
	Questions []struct {
		Question string `json:"question"`
	} `json:"questions"`
	RateLimits struct {
		Reached string `json:"rateLimitReachedType"`
		Primary *struct {
			Used   float64 `json:"usedPercent"`
			Resets int64   `json:"resetsAt"`
		} `json:"primary"`
		Secondary *struct {
			Used   float64 `json:"usedPercent"`
			Resets int64   `json:"resetsAt"`
		} `json:"secondary"`
	} `json:"rateLimits"`
}

func (a *Adapter) ParseEvent(hc adapter.HookContext, kind string, raw []byte) (adapter.HookResult, error) {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return adapter.HookResult{}, err
	}
	sid := p.SessionID
	if sid == "" {
		sid = p.ThreadID
	}
	if sid == "" {
		sid = p.Thread.ID
	}
	tid := p.TurnID
	if tid == "" {
		tid = p.ServerTurnID
	}
	if tid == "" {
		tid = p.Turn.ID
	}
	r := adapter.HookResult{SessionRef: sid}
	add := func(typ string) { r.Events = append(r.Events, adapter.Event{Type: typ, HarnessSessionID: sid}) }
	end := func(reason, text string) {
		r.Events = append(r.Events, adapter.Event{Type: adapter.TurnEnd, HarnessSessionID: sid, Reason: reason, Text: text})
		if tid != "" {
			r.EventID = sid + ":" + tid + ":end"
		}
	}
	start := func() {
		add(adapter.TurnStart)
		if tid != "" {
			r.EventID = sid + ":" + tid + ":start"
		}
	}
	errorEvents := func(e *codexError) {
		if e == nil {
			e = &codexError{Message: "Codex turn failed"}
		}
		r.Events = append(r.Events, adapter.Event{Type: adapter.Error, Text: e.Message})
		if strings.Contains(strings.ToLower(string(e.Info)), "limitexceeded") {
			r.Events = append(r.Events, adapter.Event{Type: adapter.RateLimit, Text: e.Message})
		}
	}
	switch strings.TrimPrefix(kind, "as.") {
	case "SessionStart", "session_start":
		if sid == "" {
			return r, nil
		}
		reason := p.Source
		if reason == "" {
			reason = "startup"
		}
		r.Events = []adapter.Event{{Type: adapter.SessionStart, HarnessSessionID: sid, TranscriptPath: p.Transcript, Reason: reason}}
		r.EventID = hc.RunID + ":" + sid + ":session:" + reason
	case "SessionEnd", "session_end":
		add(adapter.SessionEnd)
		r.Events[0].Reason = p.Reason
	case "UserPromptSubmit", "user_prompt_submit", "turn/started":
		start()
	case "Stop", "stop":
		end("answer", p.Message)
	case "Interrupt", "interrupt":
		end("aborted", "")
	case "PreToolUse", "pre_tool_use", "PostToolUse", "post_tool_use", "PermissionRequest", "permission_request":
		question := strings.Contains(p.Tool, "request_user_input")
		pre := kind == "PreToolUse" || kind == "pre_tool_use"
		permission := kind == "PermissionRequest" || kind == "permission_request"
		if question {
			if !pre {
				return r, nil
			}
			add(adapter.Question)
			r.Events[0].Text = questionText(p.Input)
		} else if !permission {
			add(adapter.Activity)
			r.Events[0].Tool = p.Tool
			r.Events[0].ToolPhase = adapter.ToolEnd
			if pre {
				r.Events[0].ToolPhase = adapter.ToolStart
			}
		} else if hc.Getenv != nil && hc.Getenv("OTAG_CODEX_MODE") == "hooks" {
			add(adapter.Permission)
			r.Events[0].Tool = p.Tool
			r.Events[0].Text = inputText(p.Input)
		}
		if p.ToolID != "" {
			r.EventID = sid + ":" + p.ToolID + ":" + kind
		}
		if pre || permission {
			if d := checkToolAt(hc, p.Tool, p.Input, p.Cwd); !d.Allow {
				r.Output = a.DenyOutput(kind, d.Reason)
			}
		}
	case "turn/completed":
		text := ""
		for _, it := range p.Turn.Items {
			if it.Type == "agentMessage" {
				text = it.Text
			}
		}
		switch p.Turn.Status {
		case "completed":
			end("answer", text)
		case "interrupted":
			end("aborted", text)
		case "failed", "systemError":
			errorEvents(p.Turn.Error)
			end("error", text)
		}
	case "error":
		errorEvents(p.Error)
		if p.WillRetry {
			for i := range r.Events {
				r.Events[i].Data = map[string]any{"retry": true} // not a quota end
			}
		} else {
			end("error", "")
		}
		if p.WillRetry && tid != "" {
			r.EventID = sid + ":" + tid + ":retry-error"
		}
	case "thread/status/changed":
		if p.Status.Type == "systemError" {
			errorEvents(p.Error)
			end("error", "")
			break
		}
		for _, flag := range p.Status.Flags {
			switch flag {
			case "waitingOnApproval":
				add(adapter.Permission)
			case "waitingOnUserInput":
				add(adapter.Question)
			}
		}
		// idle alone is not a completed turn. turn/completed carries the outcome.
	case "item/started", "item/completed":
		if p.Item.Type == "agentMessage" || p.Item.Type == "userMessage" || p.Item.Type == "reasoning" {
			return r, nil
		}
		add(adapter.Activity)
		r.Events[0].Tool = p.Item.Type
		r.Events[0].ToolPhase = adapter.ToolEnd
		if kind == "item/started" {
			r.Events[0].ToolPhase = adapter.ToolStart
		}
		if p.Item.ID != "" {
			r.EventID = sid + ":" + p.Item.ID + ":" + kind
		}
		if kind == "item/completed" && p.Item.Type == "contextCompaction" {
			r.Prime = adapter.PrimeMessage
		}
	case "item/tool/requestUserInput":
		add(adapter.Question)
		r.Events[0].Text = questionText(raw)
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "mcpServer/elicitation/request":
		add(adapter.Permission)
		r.Events[0].Tool = kind
	case "thread/tokenUsage/updated":
		v := p.TokenUsage.Last
		if tid == "" || v.Total == 0 {
			return r, nil
		}
		key := fmt.Sprintf("%s:%s:tokens:%d:%d:%d:%d:%d", sid, tid, p.TokenUsage.Total.Input, p.TokenUsage.Total.Output, p.TokenUsage.Total.Cached, p.TokenUsage.Total.CacheWrite, p.TokenUsage.Total.Reasoning)
		source := "sse"
		if p.UsageSource == "rollout" {
			source = "rollout"
		}
		r.EventID = key
		r.Events = []adapter.Event{{Type: adapter.Usage, Usage: &adapter.UsageData{Model: p.Model, InputTokens: max(0, v.Input-v.Cached), OutputTokens: v.Output, CacheReadTokens: v.Cached, CacheWriteTokens: v.CacheWrite, ReasoningTokens: v.Reasoning, Source: source, DedupeKey: key}}}
	case "account/rateLimits/updated":
		limited := p.RateLimits.Reached != ""
		var reset int64
		for _, window := range [](*struct {
			Used   float64 `json:"usedPercent"`
			Resets int64   `json:"resetsAt"`
		}){p.RateLimits.Primary, p.RateLimits.Secondary} {
			if window != nil && window.Used >= 100 {
				limited = true
				if window.Resets > reset {
					reset = window.Resets
				}
			}
		}
		if limited {
			add(adapter.RateLimit)
			r.Events[0].Text = "Codex account limit reached"
			if reset > 0 {
				r.Events[0].ResetsAt = time.Unix(reset, 0)
			}
		}
	}
	return r, nil
}

func inputText(raw []byte) string {
	var in struct {
		Description string `json:"description"`
		Command     string `json:"command"`
	}
	_ = json.Unmarshal(raw, &in)
	if in.Description != "" {
		return in.Description
	}
	return in.Command
}
func questionText(raw []byte) string {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(raw, &in)
	var out []string
	for _, q := range in.Questions {
		out = append(out, q.Question)
	}
	return strings.Join(out, "\n")
}

func checkTool(hc adapter.HookContext, tool string, raw []byte) guard.Decision {
	return checkToolAt(hc, tool, raw, "")
}

func checkToolAt(hc adapter.HookContext, tool string, raw []byte, cwd string) guard.Decision {
	if hc.Workdir == "" {
		return guard.Decision{Allow: true}
	}
	pol := hc.GuardPolicy()
	pol.Cwd = cwd
	if strings.HasPrefix(tool, "mcp__") || strings.HasPrefix(tool, "mcp:") {
		return pol.CheckToolArgs(tool, raw) // otag cannot tell what an MCP tool writes or runs
	}
	var in struct {
		Command  string `json:"command"`
		Cmd      string `json:"cmd"`
		Patch    string `json:"patch"`
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
		Cwd      string `json:"cwd"`
		Workdir  string `json:"workdir"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil && len(strings.TrimSpace(string(raw))) > 0 {
			return guard.Decision{Reason: "otag guard: the " + tool + " input cannot be checked; refusing it"}
		}
		in.Command = s
	}
	if in.Cwd != "" {
		pol.Cwd = in.Cwd
	}
	if in.Workdir != "" {
		pol.Cwd = in.Workdir
	}
	if strings.Contains(tool, "apply_patch") {
		patch := in.Command
		if patch == "" {
			patch = in.Patch
		}
		return pol.CheckPatch(patch)
	}
	for _, path := range []string{in.Path, in.FilePath} {
		if path != "" {
			if d := pol.CheckWrite(path); !d.Allow {
				return d
			}
		}
	}
	command := in.Command
	if command == "" {
		command = in.Cmd
	}
	if command != "" {
		return pol.CheckCommand(command)
	}
	return guard.Decision{Allow: true}
}

// GuardKind implements adapter.Guarder: the pre-tool and permission hooks decide.
func (*Adapter) GuardKind(kind string) bool {
	switch kind {
	case "PreToolUse", "pre_tool_use", "PermissionRequest", "permission_request":
		return true
	}
	return false
}

// DenyOutput implements adapter.Guarder.
func (*Adapter) DenyOutput(kind, reason string) []byte {
	out := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}
	if kind == "PermissionRequest" || kind == "permission_request" {
		out = map[string]any{"hookEventName": "PermissionRequest", "decision": map[string]any{"behavior": "deny", "message": reason}}
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": out})
	return b
}

var _ adapter.Guarder = (*Adapter)(nil)
