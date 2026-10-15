// Package fake is a scripted harness for tests and demos. The "harness" is a POSIX
// shell script that runs the steps listed in the profile's args and reports each one
// through `otag event fake <kind>`, exactly like a real harness's hooks. It exercises
// every part of the adapter contract: files, env, pre-assigned session ids, a trust
// dialog, an optional server process, dedupe ids and a non-empty no-op output.
package fake

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

//go:embed harness.sh
var script []byte

// TrustPrompt is the dialog printed by the "trust" step.
const TrustPrompt = "Do you trust the files in this folder?"

// DefaultSteps run when the profile has no args.
var DefaultSteps = []string{"start", "busy", "idle", "wait"}

// Adapter is the fake harness adapter.
type Adapter struct{}

// New returns the fake adapter.
func New() *Adapter { return &Adapter{} }

// Harness implements adapter.Adapter.
func (*Adapter) Harness() string { return "fake" }

// Prepare implements adapter.Adapter. Profile env FAKE_SERVER=1 adds a server process.
func (*Adapter) Prepare(_ context.Context, spec adapter.RunSpec) (*adapter.LaunchSpec, error) {
	steps := spec.Profile.Args
	if len(steps) == 0 {
		steps = DefaultSteps
	}
	sid := "fake-" + spec.RunID
	if spec.ResumeSessionID != "" {
		sid = spec.ResumeSessionID
	}
	scriptPath := filepath.Join(spec.RunDir, "fake-harness.sh")
	ls := &adapter.LaunchSpec{
		Argv: append([]string{"/bin/sh", scriptPath}, steps...),
		Env:  map[string]string{"FAKE_SESSION_ID": sid},
		Files: []adapter.File{
			{Path: "fake-harness.sh", Data: script, Mode: 0o700},
			{Path: "instructions.md", Data: []byte(spec.Instructions)},
			{Path: "prompt.md", Data: []byte(spec.Prompt)},
			// Tests drive hooks and child commands as the run (Token).
			{Path: tokenFile, Data: []byte(spec.Env["OTAG_RUN_TOKEN"])},
		},
		HarnessSessionID: sid,
		StartupTimeout:   10 * time.Second,
	}
	switch spec.Profile.Env["FAKE_SIDECAR"] {
	case "1":
		ls.Sidecars = append(ls.Sidecars, adapter.SidecarProcess(spec, "listen", "hello"))
	case "never-ready":
		sc := adapter.SidecarProcess(spec, "listen", "--never-ready")
		if d, err := time.ParseDuration(spec.Profile.Env["FAKE_READY_TIMEOUT"]); err == nil {
			sc.ReadyTimeout = d
		}
		ls.Sidecars = append(ls.Sidecars, sc)
	}
	if spec.Profile.Env["FAKE_SERVER"] == "1" {
		ready := filepath.Join(spec.RunDir, "server.ready")
		ls.Server = &adapter.Process{
			Name:         "server",
			Argv:         []string{"/bin/sh", scriptPath, "server"},
			Env:          map[string]string{"FAKE_READY_FILE": ready},
			Ready:        "file:" + ready,
			ReadyTimeout: 10 * time.Second,
			URL:          "fake://" + spec.RunID,
		}
	}
	return ls, nil
}

// payload is the JSON the fake harness sends.
type payload struct {
	SessionID    string `json:"session_id"`
	Source       string `json:"source"`
	Reason       string `json:"reason"`
	Text         string `json:"text"`
	Tool         string `json:"tool"`
	Command      string `json:"command"`
	EventID      string `json:"event_id"`
	ResetsAt     string `json:"resets_at"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	ID           string `json:"id"`
	Retry        bool   `json:"retry"`            // rate_limit: the harness retries by itself
	Phase        string `json:"phase"`            // activity: tool start|end
	Prompt       string `json:"prompt"`           // turn_start: the submitted prompt (marker proof)
	Slot         string `json:"slot"`             // tool|prompt|stop: the output can carry messages
	StopActive   bool   `json:"stop_hook_active"` // slot stop: this one follows a Stop block
}

// ParseEvent implements adapter.Adapter. Kinds equal to a normalized event type map
// 1:1; "pre_tool" is an activity whose neutral answer is {"decision":"allow"}; "pre_tool" with a "command" is checked by the guard and may answer deny;
// other kinds are recorded raw only.
func (a *Adapter) ParseEvent(hc adapter.HookContext, kind string, raw []byte) (adapter.HookResult, error) {
	var p payload
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return adapter.HookResult{}, err
		}
	}
	res := adapter.HookResult{SessionRef: p.SessionID, EventID: p.EventID}
	if ch, ok := slots[p.Slot]; ok {
		res.Slot = &adapter.DeliverySlot{Channel: ch, Continued: p.StopActive}
	}
	typ := kind
	if kind == "pre_tool" {
		typ = adapter.Activity
		res.Output = a.NoopOutput(kind)
		if p.Command != "" {
			pol := hc.GuardPolicy()
			if d := pol.CheckCommand(p.Command); !d.Allow {
				res.Output = a.DenyOutput(kind, d.Reason)
			}
		}
	}
	if !slices.Contains(adapter.EventTypes, typ) {
		return res, nil
	}
	ev := adapter.Event{
		Type: typ, HarnessSessionID: p.SessionID, Text: p.Text, Tool: p.Tool,
		Reason: p.Reason, ToolPhase: p.Phase,
	}
	if ev.Reason == "" {
		ev.Reason = p.Source
	}
	if p.Retry {
		ev.Data = map[string]any{"retry": true}
	}
	if p.ResetsAt != "" {
		ev.ResetsAt, _ = time.Parse(time.RFC3339, p.ResetsAt)
	}
	if typ == adapter.Usage {
		ev.Usage = &adapter.UsageData{
			Model: p.Model, InputTokens: p.InputTokens, OutputTokens: p.OutputTokens,
			Source: "hook", DedupeKey: p.ID,
		}
	}
	res.Events = []adapter.Event{ev}
	return res, nil
}

const tokenFile = "token"

// Token returns the OTAG_RUN_TOKEN of a fake run from its run dir, for tests that
// send hooks or child commands as the run.
func Token(runDir string) string {
	b, _ := os.ReadFile(filepath.Join(runDir, tokenFile))
	return string(b)
}

// GuardKind implements adapter.Guarder: pre_tool decides.
func (*Adapter) GuardKind(kind string) bool { return kind == "pre_tool" }

// DenyOutput implements adapter.Guarder.
func (*Adapter) DenyOutput(_, reason string) []byte {
	out, _ := json.Marshal(map[string]string{"decision": "deny", "reason": reason})
	return out
}

// NoopOutput implements adapter.Adapter.
func (*Adapter) NoopOutput(kind string) []byte {
	if kind == "pre_tool" {
		return []byte(`{"decision":"allow"}`)
	}
	return nil
}

// AcceptTrust implements adapter.TrustHandler.
func (*Adapter) AcceptTrust(screen string) ([]string, bool) {
	if strings.Contains(screen, TrustPrompt) {
		return []string{"y", "Enter"}, true
	}
	return nil, false
}

// RunSidecar implements adapter.SidecarRunner: it reports ready, emits one activity
// event per argument and waits for the run to end.
func (*Adapter) RunSidecar(ctx context.Context, sc adapter.Sidecar, args []string) error {
	if len(args) > 0 && args[0] == "--never-ready" {
		<-ctx.Done() // keeps `otag run start` waiting (tests the stop/launch race)
		return nil
	}
	if err := sc.Ready(); err != nil {
		return err
	}
	for i, arg := range args {
		payload, _ := json.Marshal(map[string]string{"tool": "sidecar:" + arg})
		if _, err := sc.Emit(ctx, adapter.Activity, payload, fmt.Sprintf("sidecar-%s-%d", sc.RunID, i)); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return nil
}

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.TrustHandler  = (*Adapter)(nil)
	_ adapter.SidecarRunner = (*Adapter)(nil)
	_ adapter.Guarder       = (*Adapter)(nil)
)
