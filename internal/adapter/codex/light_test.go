package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

// A light run starts Codex with full access and no approvals, in both modes, the TUI
// home and the thread; no native permission profile is built.
func TestPrepareLight(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "codex")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexec "+shellQuote(os.Args[0])+" -test.run=TestCodexProbeHelper -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := adapter.RunSpec{Harness: "codex", RunID: "r1", RunDir: filepath.Join(root, "run"), CacheDir: filepath.Join(root, "cache"),
		Workdir: root, OtagBin: filepath.Join(root, "otag"), Instructions: "identity", Prompt: "tiny task", Guard: guard.Light,
		Account: adapter.Account{Command: probe, ConfigDir: filepath.Join(root, "account"), Env: map[string]string{"OTAG_CODEX_PROBE_TEST": "1"}},
		Profile: adapter.Profile{Model: "gpt-6.1-sol", Effort: "low"}}
	for _, mode := range []string{"server", "hooks"} {
		spec.Profile.Env = map[string]string{"OTAG_CODEX_MODE": mode}
		ls, err := New().Prepare(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		argv := ls.Argv
		if mode == "server" {
			argv = ls.Server.Argv
		}
		var config map[string]any
		var text strings.Builder
		for i, arg := range argv {
			if arg == "-c" && !strings.ContainsAny(strings.SplitN(argv[i+1], "=", 2)[0], " \n") {
				text.WriteString(argv[i+1] + "\n")
			}
		}
		if _, err := toml.Decode(text.String(), &config); err != nil {
			t.Fatal(err)
		}
		if config["sandbox_mode"] != "danger-full-access" || config["approval_policy"] != "never" ||
			config["default_permissions"] != nil || config["permissions"] != nil || config["hooks"] == nil {
			t.Fatalf("%s config: %v", mode, config)
		}
		if mode == "server" {
			var tui map[string]any
			for _, f := range ls.Files {
				if f.Path == "tui-home/config.toml" {
					_, _ = toml.Decode(string(f.Data), &tui)
				}
			}
			if tui["sandbox_mode"] != "danger-full-access" || tui["approval_policy"] != "never" {
				t.Fatalf("tui config: %v", tui)
			}
			if err := New().SetupServer(context.Background(), spec, ls); err != nil {
				t.Fatal(err)
			}
			var params map[string]any
			args := ls.Sidecars[0].Argv
			for i, a := range args {
				if a == "thread/start" {
					_ = json.Unmarshal([]byte(args[i+1]), &params)
				}
			}
			if params["sandbox"] != "danger-full-access" || params["approvalPolicy"] != "never" || params["permissions"] != nil {
				t.Fatalf("thread params: %v", params)
			}
			if p := turnPolicy(params); p["approvalPolicy"] != "never" || p["permissions"] != nil {
				t.Fatalf("turn policy: %v", p)
			}
		}
	}
}

// The pre-tool hook of a light run: pushes, patches outside the workspace and secrets
// are refused, shell writes elsewhere are not.
func TestGuardLight(t *testing.T) {
	root := t.TempDir()
	home, _ := os.UserHomeDir()
	outside := filepath.Join(home, "otag-light-outside")
	hc := adapter.HookContext{Workdir: root, Guard: guard.Light}
	for _, tt := range []struct {
		tool  string
		input any
		deny  bool
	}{
		{"Bash", map[string]string{"command": "git push --force origin HEAD"}, true},
		{"exec_command", map[string]string{"cmd": "git -c push.default=current push"}, true},
		{"apply_patch", map[string]string{"command": "*** Begin Patch\n*** Add File: " + outside + "\n+x\n*** End Patch"}, true},
		{"exec_command", map[string]string{"cmd": "cat .env"}, true},
		{"Bash", map[string]string{"command": "sed -i '' s/a/b/ " + outside}, false},
		{"exec_command", map[string]string{"cmd": "echo hi > ../outside && go test ./..."}, false},
		{"apply_patch", map[string]string{"command": "*** Begin Patch\n*** Add File: x.go\n+x\n*** End Patch"}, false},
	} {
		input, _ := json.Marshal(tt.input)
		raw, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": tt.tool, "tool_input": json.RawMessage(input)})
		r, err := New().ParseEvent(hc, "PreToolUse", raw)
		if err != nil {
			t.Fatal(err)
		}
		if (len(r.Output) > 0) != tt.deny {
			t.Errorf("%s %v: output %s, want deny %v", tt.tool, tt.input, r.Output, tt.deny)
		}
	}
}

// The relay pins a light thread's TUI turns to full access, never to "otag".
func TestPinLight(t *testing.T) {
	policy := turnPolicy(map[string]any{"sandbox": "danger-full-access"})
	params, _ := json.Marshal(map[string]any{"threadId": "s", "permissions": "otag", "approvalsReviewer": "user",
		"cwd": "/elsewhere", "environments": []string{"x"}})
	b, err := pinTUIRequest(envelope{Method: "turn/start", Params: params}, "s", "/work", policy)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct{ Params map[string]any }
	_ = json.Unmarshal(b, &msg)
	p := msg.Params
	sp, _ := p["sandboxPolicy"].(map[string]any)
	if p["approvalPolicy"] != "never" || sp["type"] != "dangerFullAccess" || p["permissions"] != nil ||
		p["approvalsReviewer"] != nil || p["cwd"] != "/work" || p["environments"] != nil {
		t.Fatalf("pinned: %v", p)
	}
	if s := turnPolicy(map[string]any{"permissions": "otag"}); s["permissions"] != "otag" || s["approvalPolicy"] != "on-request" {
		t.Fatalf("strict policy: %v", s)
	}
}
