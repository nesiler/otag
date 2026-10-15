package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/nesiler/otag/internal/adapter"
)

func TestRecordedEvents(t *testing.T) {
	b, err := os.ReadFile("testdata/events.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Kind    string
		Payload json.RawMessage
		Types   []string
	}
	if err := json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(f.Kind+string(f.Payload), func(t *testing.T) {
			r, err := New().ParseEvent(adapter.HookContext{RunID: "r1", Workdir: t.TempDir()}, f.Kind, f.Payload)
			if err != nil {
				t.Fatal(err)
			}
			types := []string{}
			for _, e := range r.Events {
				types = append(types, e.Type)
			}
			if !reflect.DeepEqual(types, f.Types) {
				t.Fatalf("got %v want %v", types, f.Types)
			}
			if f.Kind == "thread/tokenUsage/updated" {
				u := r.Events[0].Usage
				if u.InputTokens != 16 || u.OutputTokens != 5 || u.CacheReadTokens != 4 || u.ReasoningTokens != 2 || u.DedupeKey == "" || r.EventID == "" {
					t.Fatalf("usage: %+v", u)
				}
			}
			if len(New().NoopOutput(f.Kind)) != 0 {
				t.Fatal("non-neutral no-op")
			}
		})
	}
	for _, raw := range []string{"", "{", "null", "[]"} {
		r, err := New().ParseEvent(adapter.HookContext{}, "noise", []byte(raw))
		if raw == "null" {
			if err != nil || len(r.Events) != 0 {
				t.Fatal(r, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("accepted malformed %q", raw)
		}
	}
}

func TestGuard(t *testing.T) {
	root := t.TempDir()
	hc := adapter.HookContext{Workdir: root, Deny: []string{".env*", "infra/prod/**"}}
	for _, tt := range []struct {
		tool  string
		input any
		deny  bool
	}{
		{"Bash", map[string]string{"command": "git -C . push origin HEAD"}, true},
		{"Bash", map[string]string{"command": "echo hi > ../outside"}, true},
		{"Bash", map[string]string{"command": "echo hi > src.txt"}, false},
		{"apply_patch", map[string]string{"command": "*** Begin Patch\n*** Add File: .env.local\n+x\n*** End Patch"}, true},
		{"apply_patch", "*** Begin Patch\n*** Update File: a\n*** Move to: ../outside\n*** End Patch", true},
		{"apply_patch", map[string]string{"patch": "*** Delete File: infra/prod/config"}, true},
		{"Write", map[string]string{"file_path": ".git/config"}, true},
		{"Edit", map[string]string{"file_path": "hello.go"}, false},
		{"exec_command", map[string]string{"cmd": "touch .env.local", "workdir": filepath.Join(root, "sub")}, true},
	} {
		t.Run(fmt.Sprint(tt), func(t *testing.T) {
			input, _ := json.Marshal(tt.input)
			raw, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": tt.tool, "tool_input": json.RawMessage(input)})
			r, err := New().ParseEvent(hc, "PreToolUse", raw)
			if err != nil {
				t.Fatal(err)
			}
			if (len(r.Output) > 0) != tt.deny {
				t.Fatalf("output %s deny %v", r.Output, tt.deny)
			}
			if tt.deny && !strings.Contains(string(r.Output), `"permissionDecision":"deny"`) {
				t.Fatal(string(r.Output))
			}
		})
	}
}

func TestCanonicalTurnDedupe(t *testing.T) {
	a, _ := New().ParseEvent(adapter.HookContext{}, "Stop", []byte(`{"session_id":"s","turn_id":"t"}`))
	b, _ := New().ParseEvent(adapter.HookContext{}, "turn/completed", []byte(`{"threadId":"s","turn":{"id":"t","status":"completed"}}`))
	if a.EventID == "" || a.EventID != b.EventID {
		t.Fatal(a, b)
	}
}

func TestSpikeRecordings(t *testing.T) {
	b, err := os.ReadFile("testdata/recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		RecordedEventID int `json:"recorded_event_id"`
		Kind            string
		Payload         json.RawMessage
	}
	if err := json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"session_start": {"session_start"}, "user_prompt_submit": {"turn_start"}, "pre_tool_use": {"activity"}, "post_tool_use": {"activity"}, "stop": {"turn_end"}, "interrupt": {"turn_end"}, "session_end": {"session_end"}, "permission_request": {"permission"}, "as.turn/started": {"turn_start"}, "as.turn/completed": {"turn_end"}, "as.error": {"error", "turn_end"}, "as.thread/status/changed": {}, "as.thread/closed": {}, "rollout.task_started": {"turn_start"}, "rollout.task_complete": {"turn_end"}, "rollout.turn_aborted": {"turn_end"}}
	for _, f := range fixtures {
		t.Run(fmt.Sprint(f.RecordedEventID), func(t *testing.T) {
			kind, raw := f.Kind, f.Payload
			if strings.HasPrefix(kind, "rollout.") {
				var pl map[string]any
				_ = json.Unmarshal(raw, &pl)
				pl["type"] = strings.TrimPrefix(kind, "rollout.")
				frame, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": pl})
				mapped, p, _ := rolloutEvent(frame, "s", "turn", "model")
				kind = mapped
				raw, _ = json.Marshal(p)
			}
			wantTypes := want[f.Kind]
			if strings.Contains(string(raw), `"tool_name":"request_user_input`) || strings.Contains(string(raw), `"tool_name": "request_user_input`) {
				wantTypes = []string{"question"}
			}
			if f.Kind == "as.turn/completed" && strings.Contains(string(raw), `"failed"`) {
				wantTypes = []string{"error", "turn_end"}
			}
			r, err := New().ParseEvent(adapter.HookContext{Workdir: t.TempDir(), Getenv: func(string) string { return "hooks" }}, kind, raw)
			if err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, ev := range r.Events {
				got = append(got, ev.Type)
			}
			if !slices.Equal(got, wantTypes) {
				t.Fatalf("%s: got %v want %v", f.Kind, got, wantTypes)
			}
			if phase := map[string]string{"pre_tool_use": adapter.ToolStart, "post_tool_use": adapter.ToolEnd}[f.Kind]; phase != "" &&
				len(r.Events) == 1 && r.Events[0].Type == adapter.Activity && r.Events[0].ToolPhase != phase {
				t.Fatalf("%s: tool phase %q", f.Kind, r.Events[0].ToolPhase)
			}
		})
	}
}

func TestPrepare(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "codex")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexec "+shellQuote(os.Args[0])+" -test.run=TestCodexProbeHelper -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := adapter.RunSpec{Harness: "codex", RunID: "r1", RunDir: filepath.Join(root, "run"), CacheDir: filepath.Join(root, "cache"), Workdir: root, OtagBin: filepath.Join(root, "otag's bin"), Instructions: "identity\nsecond line", Prompt: "tiny task", Deny: []string{".env*", "infra/prod/**"},
		Account: adapter.Account{Command: probe, ConfigDir: filepath.Join(root, "account"), Env: map[string]string{"OTAG_CODEX_PROBE_TEST": "1"}}, Profile: adapter.Profile{Model: "gpt-6.1-sol", Effort: "low"}}
	for _, mode := range []string{"server", "hooks"} {
		t.Run(mode, func(t *testing.T) {
			spec.Profile.Env = map[string]string{"OTAG_CODEX_MODE": mode}
			ls, err := New().Prepare(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if ls.Env["CODEX_HOME"] != spec.Account.ConfigDir {
				t.Fatal(ls.Env)
			}
			argv := ls.Argv
			if mode == "server" {
				if ls.Server == nil || ls.Server.URL != "unix://"+filepath.Join(spec.RunDir, "as.sock") {
					t.Fatal(ls.Server)
				}
				argv = ls.Server.Argv
			} else {
				if ls.Server != nil || !slices.Contains(argv, "--no-daemon") {
					t.Fatal(ls)
				}
			}
			var config map[string]any
			var configText strings.Builder
			for i, arg := range argv {
				if arg == "-c" && !strings.ContainsAny(strings.SplitN(argv[i+1], "=", 2)[0], " \n") {
					configText.WriteString(argv[i+1] + "\n")
				}
			}
			if _, err := toml.Decode(configText.String(), &config); err != nil {
				t.Fatalf("TOML: %v\n%s", err, configText.String())
			}
			if config["developer_instructions"] != spec.Instructions || config["default_permissions"] != "otag" {
				t.Fatal(config)
			}
			hooks := config["hooks"].(map[string]any)
			state := hooks["state"].(map[string]any)
			if len(state) != len(hooks)-1 {
				t.Fatalf("trust: %v", state)
			}
			if _, ok := state["foreign"]; ok {
				t.Fatal("trusted foreign hook")
			}
			// Both modes report UserPromptSubmit (mode B: proof of a delivered marker).
			ups, _ := json.Marshal(hooks["UserPromptSubmit"])
			want := "event codex UserPromptSubmit"
			if mode == "hooks" {
				want = "-- track UserPromptSubmit"
			}
			if !strings.Contains(string(ups), want) {
				t.Fatalf("UserPromptSubmit hook: %s", ups)
			}
			if _, err := os.Stat(spec.RunDir); !os.IsNotExist(err) {
				t.Fatalf("Prepare wrote run dir: %v", err)
			}
			if _, err := New().Prepare(context.Background(), spec); err != nil {
				t.Fatal("cached prepare", err)
			}
		})
	}
	spec.ResumeSessionID = "saved"
	ls, err := New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ls.Argv, "saved") {
		t.Fatal(ls.Argv)
	}
	spec.Profile.Mode = "headless"
	if _, err := New().Prepare(context.Background(), spec); err == nil {
		t.Fatal("silent headless downgrade")
	}
	spec.Profile.Mode = "interactive"
	for _, arg := range []string{"--yolo", "--add-dir=/outside", "--dangerously-bypass-hook-trust", "--remote=unix:///elsewhere"} {
		spec.Profile.Args = []string{arg}
		if _, err := New().Prepare(context.Background(), spec); err == nil {
			t.Fatalf("accepted %s", arg)
		}
	}
}

// A local fake CLI exercises the real stdio preflight without Codex or inference.
func TestCodexProbeHelper(t *testing.T) {
	if os.Getenv("OTAG_CODEX_PROBE_TEST") != "1" {
		return
	}
	args := os.Args
	if slices.Contains(args, "--version") {
		fmt.Println("codex-cli fixture")
		os.Exit(0)
	}
	var text strings.Builder
	for i, arg := range args {
		if arg == "-c" && !strings.ContainsAny(strings.SplitN(args[i+1], "=", 2)[0], " \n") {
			text.WriteString(args[i+1] + "\n")
		}
	}
	var config struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	if _, err := toml.Decode(text.String(), &config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var msg envelope
		_ = json.Unmarshal(scan.Bytes(), &msg)
		if len(msg.ID) == 0 {
			continue
		}
		res := any(map[string]any{})
		if msg.Method == "hooks/list" {
			hooks := []map[string]any{{"key": "foreign", "source": "user", "currentHash": "untrusted", "command": "foreign"}}
			for kind, groups := range config.Hooks {
				for _, group := range groups {
					for _, hook := range group.Hooks {
						hooks = append(hooks, map[string]any{"key": "session:" + kind, "source": "sessionFlags", "currentHash": "sha256:" + kind, "command": hook.Command})
					}
				}
			}
			res = map[string]any{"data": []any{map[string]any{"hooks": hooks}}}
		}
		_ = enc.Encode(map[string]any{"id": msg.ID, "result": res})
	}
	os.Exit(0)
}
