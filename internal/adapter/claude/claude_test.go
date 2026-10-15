package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

func testSpec() adapter.RunSpec {
	return adapter.RunSpec{
		RunID: "al-1-r1", TaskID: "al-1", Project: "alpha", Role: "child", Attempt: 1,
		Harness: Harness,
		Account: adapter.Account{Name: "c2", ConfigDir: "/home/u/.claude-work", Env: map[string]string{"A": "1"}},
		Profile: adapter.Profile{Name: "haiku", Model: "claude-haiku-5-5", Effort: "low",
			Env: map[string]string{"A": "2", "B": "3"}},
		Workdir: "/data/worktrees/alpha/al-1", ProjectPath: "/src/alpha", Branch: "otag/al-1",
		RunDir: "/data/runs/al-1-r1", CacheDir: "/data/cache/claude", OtagBin: "/opt/otag/bin/otag",
		Env:    map[string]string{"OTAG_RUN_ID": "al-1-r1", "OTAG_DB": "/data/otag.db"},
		Prompt: "Task al-1: do it", Instructions: "You are a otag child agent.\n",
		Deny: []string{".env*", "infra/prod/**"},
	}
}

func fileMap(ls *adapter.LaunchSpec) map[string][]byte {
	m := map[string][]byte{}
	for _, f := range ls.Files {
		m[f.Path] = f.Data
	}
	return m
}

func TestPrepare(t *testing.T) {
	spec := testSpec()
	ls, err := New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	sid := ls.HarnessSessionID
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(sid) {
		t.Fatalf("session id %q is not a v5 UUID", sid)
	}
	again, _ := New().Prepare(context.Background(), spec)
	if again.HarnessSessionID != sid || strings.Join(again.Argv, "\x00") != strings.Join(ls.Argv, "\x00") {
		t.Fatal("Prepare is not deterministic")
	}
	other := spec
	other.RunDir = "/other/runs/al-1-r1"
	if SessionID(other) == sid {
		t.Fatal("session id ignores the run dir")
	}

	argv := strings.Join(ls.Argv, " ")
	want := "claude --model claude-haiku-5-5 --effort low --session-id " + sid +
		" --settings /data/runs/al-1-r1/settings.json --plugin-dir /data/runs/al-1-r1/mod" +
		" --append-system-prompt-file /data/runs/al-1-r1/instructions.md --dangerously-skip-permissions -- Task al-1: do it"
	if !strings.HasPrefix(argv, "/usr/bin/env -u CLAUDECODE ") || !strings.HasSuffix(argv, want) {
		t.Fatalf("argv\n%s\nwant suffix\n%s", argv, want)
	}
	if !slices.Contains(ls.Argv, "CLAUDE_CODE_SESSION_ID") || slices.Contains(ls.Argv, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("env scrub %v", ls.Argv)
	}
	if ls.Env["CLAUDE_CONFIG_DIR"] != "/home/u/.claude-work" || ls.Env["A"] != "2" || ls.Env["B"] != "3" {
		t.Fatalf("env %v", ls.Env)
	}

	files := fileMap(ls)
	if string(files["instructions.md"]) != spec.Instructions {
		t.Fatalf("instructions %q", files["instructions.md"])
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
				Timeout int      `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
		PromptSuggestion *bool `json:"promptSuggestionEnabled"`
	}
	if err := json.Unmarshal(files["settings.json"], &s); err != nil {
		t.Fatalf("settings: %v\n%s", err, files["settings.json"])
	}
	for _, kind := range hookKinds {
		g := s.Hooks[kind]
		if len(g) != 1 || len(g[0].Hooks) != 1 {
			t.Fatalf("hooks.%s = %+v", kind, g)
		}
		h := g[0].Hooks[0]
		if h.Type != "command" || h.Command != spec.OtagBin || strings.Join(h.Args, " ") != "event claude "+kind || h.Timeout != hookTimeout {
			t.Fatalf("hooks.%s handler %+v", kind, h)
		}
		if wantM := slices.Contains(toolHooks, kind); (g[0].Matcher == "*") != wantM {
			t.Fatalf("hooks.%s matcher %q", kind, g[0].Matcher)
		}
	}
	for _, r := range []string{"Bash(git push)", "Bash(git push *)", "Bash(git -* push *)", "Bash(*/git push *)", "Edit(//data/worktrees/alpha/al-1/.git/**)",
		"Edit(//data/worktrees/alpha/al-1/**/.env*)", "Edit(//data/worktrees/alpha/al-1/**/.env*/**)",
		"Edit(//data/worktrees/alpha/al-1/infra/prod/**)"} {
		if !slices.Contains(s.Permissions.Deny, r) {
			t.Fatalf("deny %v lacks %s", s.Permissions.Deny, r)
		}
	}
	if slices.Contains(s.Permissions.Deny, "Edit(//data/worktrees/alpha/al-1/infra/prod/**/**)") {
		t.Fatalf("redundant rule in %v", s.Permissions.Deny)
	}
	if s.PromptSuggestion == nil || *s.PromptSuggestion {
		t.Fatal("prompt suggestions not disabled")
	}

	reg := string(files["mod/hooks/register.ts"])
	for _, w := range []string{`const OTAG: string = "/opt/otag/bin/otag"`, `const RUN: string = "al-1-r1"`,
		`"OTAG_DB": "/data/otag.db"`, `'mod-turn-complete'`, `on('turn.complete'`} {
		if !strings.Contains(reg, w) {
			t.Fatalf("register.ts lacks %s:\n%s", w, reg)
		}
	}
	if !json.Valid(files["mod/.claude-plugin/plugin.json"]) || !json.Valid(files["mod/hooks/hooks.json"]) {
		t.Fatal("plugin manifest is not JSON")
	}
	for _, f := range ls.Files {
		if filepath.IsAbs(f.Path) || strings.HasPrefix(f.Path, "..") {
			t.Fatalf("file %q escapes the run dir", f.Path)
		}
	}
}

func TestPrepareVariants(t *testing.T) {
	ctx := context.Background()
	a := New()

	spec := testSpec()
	spec.ResumeSessionID = "11111111-2222-4333-8444-555555555555"
	spec.Account.ConfigDir = ""
	spec.Profile.Effort = ""
	spec.Profile.Args = []string{"--permission-mode", "acceptEdits", "--add-dir", "/x"}
	ls, err := a.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(ls.Argv, " ")
	if ls.HarnessSessionID != spec.ResumeSessionID || !strings.Contains(argv, "--resume "+spec.ResumeSessionID) ||
		strings.Contains(argv, "--session-id") || strings.Contains(argv, "--effort") {
		t.Fatalf("resume argv %s", argv)
	}
	if strings.Contains(argv, "--dangerously-skip-permissions") ||
		!strings.Contains(argv, "--permission-mode acceptEdits --add-dir /x -- Task") {
		t.Fatalf("permission flags %s", argv)
	}
	if !slices.Contains(ls.Argv, "CLAUDE_CONFIG_DIR") || ls.Env["CLAUDE_CONFIG_DIR"] != "" {
		t.Fatalf("default account must unset CLAUDE_CONFIG_DIR: %s %v", argv, ls.Env)
	}

	spec = testSpec()
	spec.Profile.Effort = "extreme"
	if _, err := a.Prepare(ctx, spec); err == nil {
		t.Fatal("bad effort accepted")
	}
	spec = testSpec()
	spec.OtagBin = "otag"
	if _, err := a.Prepare(ctx, spec); err == nil {
		t.Fatal("relative OtagBin accepted")
	}
	spec = testSpec()
	spec.Env["CLAUDE_EFFORT"] = "max" // set by otag: never scrubbed
	spec.Account.Env = map[string]string{"ANTHROPIC_BASE_URL": "http://litellm:4000"}
	ls, _ = a.Prepare(ctx, spec)
	if slices.Contains(ls.Argv, "CLAUDE_EFFORT") || slices.Contains(ls.Argv, "ANTHROPIC_BASE_URL") {
		t.Fatalf("scrubbed a variable otag sets: %v", ls.Argv)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_EFFORT_LEVEL", "ANTHROPIC_MODEL"} {
		if i := slices.Index(ls.Argv, k); i < 1 || ls.Argv[i-1] != "-u" {
			t.Fatalf("%s not scrubbed: %v", k, ls.Argv)
		}
	}
}

func TestPrepareMergesProfileSettings(t *testing.T) {
	dir := t.TempDir()
	user := `{"model":"x","hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]},
		"permissions":{"deny":["Bash(rm -rf *)","Bash(git push *)","Read(/secrets/**)","Read(//etc/x)","Edit(./rel)",
		"Bash(/usr/bin/curl *)"],"allow":["Bash(ls)","Edit(/src/**)"]},"promptSuggestionEnabled":true}`
	_ = os.MkdirAll(filepath.Join(dir, "conf"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "conf", "mine.json"), []byte(user), 0o600)

	// `/path` rules are anchored at the settings source: the file's directory, the
	// workdir for inline JSON. The merged file lives elsewhere, so they become absolute.
	for name, c := range map[string]struct {
		args   []string
		anchor string
	}{
		"inline": {[]string{"--settings", user, "--verbose"}, dir},
		"file":   {[]string{"--verbose", "--settings=conf/mine.json"}, filepath.Join(dir, "conf")},
		"two-last": {[]string{"--settings", `{"bogus":1}`, "--settings", filepath.Join(dir, "conf", "mine.json"), "--verbose"},
			filepath.Join(dir, "conf")},
	} {
		args := c.args
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			spec.Workdir = dir
			spec.Profile.Args = args
			ls, err := New().Prepare(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(strings.Join(ls.Argv, " "), "--settings "); n != 1 || !slices.Contains(ls.Argv, "--verbose") {
				t.Fatalf("argv %v", ls.Argv)
			}
			var s map[string]any
			_ = json.Unmarshal(fileMap(ls)["settings.json"], &s)
			if s["model"] != "x" || s["promptSuggestionEnabled"] != true || s["bogus"] != nil {
				t.Fatalf("user keys lost: %v", s)
			}
			stop := s["hooks"].(map[string]any)["Stop"].([]any)
			if len(stop) != 2 || !strings.Contains(mustJSON(stop[0]), "say done") || !strings.Contains(mustJSON(stop[1]), "event") {
				t.Fatalf("Stop hooks %v", stop)
			}
			perms := s["permissions"].(map[string]any)
			deny := perms["deny"].([]any)
			if deny[0] != "Bash(rm -rf *)" || perms["allow"] == nil || countOf(deny, "Bash(git push *)") != 1 ||
				!slices.Contains(deny, any("Bash(git push)")) {
				t.Fatalf("permissions %v", perms)
			}
			for _, r := range []string{"Read(/" + c.anchor + "/secrets/**)", "Read(//etc/x)", "Edit(./rel)", "Bash(/usr/bin/curl *)"} {
				if !slices.Contains(deny, any(r)) {
					t.Fatalf("deny %v lacks %s", deny, r)
				}
			}
			if slices.Contains(deny, any("Read(/secrets/**)")) || !slices.Contains(perms["allow"].([]any), any("Edit(/"+c.anchor+"/src/**)")) {
				t.Fatalf("not re-anchored: %v", perms)
			}
		})
	}

	for _, bad := range [][]string{{"--settings"}, {"--settings", "{nope"}, {"--settings", "/does/not/exist.json"},
		{"--settings", `{"hooks":[]}`}, {"--settings", `{"permissions":{"deny":"x"}}`}} {
		spec := testSpec()
		spec.Profile.Args = bad
		if _, err := New().Prepare(context.Background(), spec); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// The default ~/.claude account runs with CLAUDE_CONFIG_DIR unset (and an inherited one
// stripped): Claude Code keys its Keychain item and global config on the variable.
func TestPrepareDefaultConfigDirUnset(t *testing.T) {
	old := userHome
	userHome = func() (string, error) { return "/home/u", nil }
	t.Cleanup(func() { userHome = old })
	for _, dir := range []string{"/home/u/.claude", "/home/u/.claude/", ""} {
		spec := testSpec()
		spec.Account.ConfigDir = dir
		ls, err := New().Prepare(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ls.Env["CLAUDE_CONFIG_DIR"]; ok {
			t.Fatalf("%q: CLAUDE_CONFIG_DIR set: %v", dir, ls.Env)
		}
		if i := slices.Index(ls.Argv, "CLAUDE_CONFIG_DIR"); i < 1 || ls.Argv[i-1] != "-u" {
			t.Fatalf("%q: inherited CLAUDE_CONFIG_DIR not stripped: %v", dir, ls.Argv)
		}
		var s struct {
			StatusLine map[string]any `json:"statusLine"`
		}
		_ = json.Unmarshal(fileMap(ls)["settings.json"], &s)
		if cmd := s.StatusLine["command"].(string); strings.Contains(cmd, "--config-dir") {
			t.Fatalf("%q: statusLine %s", dir, cmd)
		}
	}
	spec := testSpec() // ~/.claude-work keeps its variable
	ls, _ := New().Prepare(context.Background(), spec)
	if ls.Env["CLAUDE_CONFIG_DIR"] != "/home/u/.claude-work" || slices.Contains(ls.Argv, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("c2 env %v argv %v", ls.Env, ls.Argv)
	}
}

// The run's statusLine is otag's quota wrapper; a profile statusLine is kept as the
// wrapped command with its other keys.
func TestPrepareStatusLineWrapper(t *testing.T) {
	spec := testSpec()
	ls, err := New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		StatusLine map[string]any `json:"statusLine"`
	}
	_ = json.Unmarshal(fileMap(ls)["settings.json"], &s)
	want := "/opt/otag/bin/otag statusline --harness claude --account c2 --config-dir /home/u/.claude-work"
	if s.StatusLine["type"] != "command" || s.StatusLine["command"] != want {
		t.Fatalf("statusLine %v", s.StatusLine)
	}

	spec.OtagBin = "/opt/my otag/otag"
	spec.Profile.Args = []string{"--settings", `{"statusLine":{"type":"command","command":"sh ~/it's.sh","padding":2}}`}
	ls, err = New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(fileMap(ls)["settings.json"], &s)
	want = `'/opt/my otag/otag' statusline --harness claude --account c2 --config-dir /home/u/.claude-work --command 'sh ~/it'\''s.sh'`
	if s.StatusLine["command"] != want || s.StatusLine["padding"] != float64(2) {
		t.Fatalf("statusLine %v", s.StatusLine)
	}
	spec.Profile.Args = []string{"--settings", `{"statusLine":"x"}`}
	if _, err := New().Prepare(context.Background(), spec); err == nil {
		t.Fatal("non-object statusLine accepted")
	}
}

func countOf(l []any, v any) (n int) {
	for _, x := range l {
		if x == v {
			n++
		}
	}
	return n
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseEvent(t *testing.T) {
	const sid = "1d8b15e7-7a4f-483b-aa57-8a596c543ddc"
	type want struct {
		types   string // event types, space separated
		reason  string // of the first event
		tool    string
		text    string // substring of the first event's text
		eventID string
		phase   string // ToolPhase of the first event
	}
	cases := []struct {
		file, kind string
		want       want
	}{
		{"SessionStart-startup.json", "SessionStart", want{types: "session_start", reason: "startup"}},
		{"SessionStart-resume.json", "SessionStart", want{types: "session_start", reason: "resume"}},
		{"SessionStart-clear.json", "SessionStart", want{types: "session_start", reason: "clear"}},
		{"UserPromptSubmit.json", "UserPromptSubmit", want{types: "turn_start"}},
		{"PreToolUse-Read.json", "PreToolUse", want{types: "activity", tool: "Read", eventID: ":PreToolUse", phase: "start"}},
		{"PreToolUse-Edit.json", "PreToolUse", want{types: "activity", tool: "Edit", phase: "start"}},
		{"PostToolUse-Edit.json", "PostToolUse", want{types: "activity", tool: "Edit", eventID: ":PostToolUse", phase: "end"}},
		{"PreToolUse-AskUserQuestion.json", "PreToolUse", want{types: "question", tool: "AskUserQuestion", text: "Favori renginiz"}},
		{"PermissionRequest-AskUserQuestion.json", "PermissionRequest", want{}},
		{"PermissionRequest-Bash.json", "PermissionRequest", want{types: "permission", tool: "Bash", text: "Bash: touch perm-test.txt"}},
		{"Notification-permission.json", "Notification", want{}},
		{"Stop.json", "Stop", want{types: "turn_end", reason: "answer", text: "OTAG-DONE"}},
		{"StopFailure-rate_limit.json", "StopFailure", want{types: "rate_limit turn_end", text: "Rate limit reached (429 Too Many Requests)"}},
		{"StopFailure-overloaded.json", "StopFailure", want{types: "error turn_end", text: "API Error: Overloaded"}},
		{"SubagentStop-suggestion.json", "SubagentStop", want{}},
		{"SubagentStop-explore.json", "SubagentStop", want{}},
		{"SessionEnd-exit.json", "SessionEnd", want{types: "session_end", reason: "prompt_input_exit"}},
		{"SessionEnd-other.json", "SessionEnd", want{types: "session_end", reason: "other"}},
		{"SessionEnd-clear.json", "SessionEnd", want{}},
		{"SessionEnd-resume.json", "SessionEnd", want{}},
		{"mod-turn-complete-answer.json", ModTurnComplete, want{types: "turn_end", reason: "answer"}},
		{"mod-turn-complete-aborted.json", ModTurnComplete, want{types: "turn_end", reason: "aborted"}},
		{"mod-turn-complete-v2.json", ModTurnComplete, want{types: "turn_end", reason: "aborted", eventID: "mod:al-1-r1:t-7"}},
		{"mod-turn-complete-subagent.json", ModTurnComplete, want{}},
	}
	a := New()
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			raw := fixture(t, c.file)
			res, err := a.ParseEvent(adapter.HookContext{}, c.kind, raw)
			if err != nil {
				t.Fatal(err)
			}
			var types []string
			for _, e := range res.Events {
				types = append(types, e.Type)
			}
			if got := strings.Join(types, " "); got != c.want.types {
				t.Fatalf("types %q, want %q (%+v)", got, c.want.types, res.Events)
			}
			if res.Output != nil {
				t.Fatalf("output without a run: %s", res.Output)
			}
			if !strings.HasPrefix(c.kind, "mod-") && !strings.Contains(c.file, "rate_limit") && !strings.Contains(c.file, "overloaded") &&
				!strings.Contains(c.file, "clear") && res.SessionRef != sid && res.SessionRef != "3064c3b0-9e2d-4d32-937f-c7abac372f7b" {
				t.Fatalf("session ref %q", res.SessionRef)
			}
			if c.want.eventID != "" && !strings.HasSuffix(res.EventID, c.want.eventID) {
				t.Fatalf("event id %q, want suffix %q", res.EventID, c.want.eventID)
			}
			if len(res.Events) == 0 {
				return
			}
			e := res.Events[0]
			if e.Reason != c.want.reason || e.Tool != c.want.tool || !strings.Contains(e.Text, c.want.text) ||
				e.ToolPhase != c.want.phase {
				t.Fatalf("event %+v, want %+v", e, c.want)
			}
			if e.Type == adapter.SessionStart && (e.HarnessSessionID == "" || e.TranscriptPath == "") {
				t.Fatalf("session_start %+v", e)
			}
			if len(res.Events) == 2 && (res.Events[1].Reason != "error" || res.Events[1].Text == "") {
				t.Fatalf("failure turn_end %+v", res.Events[1])
			}
		})
	}

	if _, err := a.ParseEvent(adapter.HookContext{}, "Stop", []byte(`{nope`)); err == nil {
		t.Fatal("broken JSON accepted")
	}
	if res, err := a.ParseEvent(adapter.HookContext{}, "Stop", nil); err != nil || len(res.Events) != 1 {
		t.Fatalf("empty payload: %+v %v", res, err)
	}
	if res, _ := a.ParseEvent(adapter.HookContext{}, "SomethingNew", []byte(`{"session_id":"s"}`)); len(res.Events) != 0 || res.SessionRef != "s" {
		t.Fatalf("unknown kind %+v", res)
	}
	for _, k := range append(slices.Clone(hookKinds), ModTurnComplete) {
		if out := a.NoopOutput(k); len(out) != 0 {
			t.Fatalf("NoopOutput(%s) = %q", k, out)
		}
	}
}

func TestGuard(t *testing.T) {
	wd := t.TempDir()
	scratch := t.TempDir()
	hc := adapter.HookContext{RunID: "al-1-r1", Workdir: wd, Deny: []string{".env*"}}
	a := New()
	pre := func(tool string, input map[string]any) []byte {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": tool, "tool_input": input,
			"tool_use_id": "toolu_x", "scratchpad_dir": scratch})
		res, err := a.ParseEvent(hc, "PreToolUse", raw)
		if err != nil || len(res.Events) != 1 || res.Events[0].Type != adapter.Activity {
			t.Fatalf("%s: %+v %v", tool, res, err)
		}
		return res.Output
	}
	denied := func(out []byte, why string) {
		t.Helper()
		var o struct {
			H struct {
				Event    string `json:"hookEventName"`
				Decision string `json:"permissionDecision"`
				Reason   string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out, &o); err != nil || o.H.Event != "PreToolUse" || o.H.Decision != "deny" ||
			!strings.Contains(o.H.Reason, why) {
			t.Fatalf("want deny (%s), got %s", why, out)
		}
	}

	// Recorded spike payloads.
	res, _ := a.ParseEvent(hc, "PreToolUse", fixture(t, "PreToolUse-Bash-push.json"))
	denied(res.Output, "push")
	res, _ = a.ParseEvent(hc, "PreToolUse", fixture(t, "PreToolUse-Write-outside.json"))
	denied(res.Output, "outside the workspace")

	denied(pre("Bash", map[string]any{"command": "git -C . push origin HEAD"}), "push")
	denied(pre("Write", map[string]any{"file_path": filepath.Join(wd, ".env.local")}), "deny list")
	denied(pre("Edit", map[string]any{"file_path": filepath.Join(wd, ".git", "config")}), "git metadata")
	denied(pre("NotebookEdit", map[string]any{"notebook_path": "/etc/x.ipynb"}), "outside")
	if out := pre("Write", map[string]any{"file_path": filepath.Join(wd, "a.go")}); out != nil {
		t.Fatalf("write inside denied: %s", out)
	}
	if out := pre("Write", map[string]any{"file_path": filepath.Join(scratch, "notes.md")}); out != nil {
		t.Fatalf("scratchpad write denied: %s", out)
	}
	if out := pre("Bash", map[string]any{"command": "go test ./... && git commit -qm x"}); out != nil {
		t.Fatalf("plain command denied: %s", out)
	}
	if out := pre("Read", map[string]any{"file_path": "/etc/hosts"}); out != nil {
		t.Fatalf("read denied: %s", out)
	}
	denied(pre("Bash", map[string]any{"command": "cd /tmp && touch otag-sentinel"}), "outside the workspace")

	// The shell's cwd (payload cwd, moved by an earlier `cd`) is where relative paths go.
	bash := func(cwd, cmd string) []byte {
		raw, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "cwd": cwd,
			"tool_input": map[string]any{"command": cmd}, "tool_use_id": "toolu_y"})
		res, err := a.ParseEvent(hc, "PreToolUse", raw)
		if err != nil {
			t.Fatal(err)
		}
		return res.Output
	}
	denied(bash(os.TempDir(), "touch x"), "outside the workspace")
	_ = os.MkdirAll(filepath.Join(wd, "sub"), 0o755)
	if out := bash(filepath.Join(wd, "sub"), "touch ../x"); out != nil {
		t.Fatalf("relative write inside from a subdirectory denied: %s", out)
	}
	denied(bash(filepath.Join(wd, "sub"), "touch ../../x"), "outside the workspace")

	// Malformed input, secret reads and MCP tools carrying paths outside.
	raw, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Write", "tool_input": "malformed"})
	res, _ = a.ParseEvent(hc, "PreToolUse", raw)
	denied(res.Output, "cannot be checked")
	denied(pre("Write", map[string]any{"content": "x"}), "cannot be checked")
	denied(pre("Read", map[string]any{"file_path": filepath.Join(wd, ".env")}), "deny list")
	denied(pre("Grep", map[string]any{"pattern": "KEY", "path": ".env.local"}), "deny list")
	denied(pre("mcp__filesystem__write_file", map[string]any{"path": "/outside/file", "content": "x"}), "outside the workspace")
	if out := pre("mcp__filesystem__write_file", map[string]any{"path": filepath.Join(wd, "ok.txt")}); out != nil {
		t.Fatalf("mcp write inside denied: %s", out)
	}
	if out := pre("mcp__web__fetch", map[string]any{"url": "https://example.com"}); out != nil {
		t.Fatalf("mcp without paths denied: %s", out)
	}
	// otag's own state.
	child := hc
	child.DB, child.TmuxSocket, child.OtagBin = "/data/otag.db", "otag", "/opt/otag/bin/otag"
	for _, cmd := range []string{"sqlite3 /data/otag.db .tables", "tmux -L otag kill-server", "otag run stop x", "git update-ref refs/heads/main HEAD"} {
		raw, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": "Bash", "tool_input": map[string]any{"command": cmd}})
		res, _ := a.ParseEvent(child, "PreToolUse", raw)
		denied(res.Output, "")
	}

	// No run, no workdir: no decision (the bridge refuses unknown runs itself).
	res, _ = a.ParseEvent(adapter.HookContext{}, "PreToolUse", fixture(t, "PreToolUse-Bash-push.json"))
	if res.Output != nil {
		t.Fatalf("decision without a run: %s", res.Output)
	}
	if !a.GuardKind("PreToolUse") || !a.GuardKind("PermissionRequest") || a.GuardKind("PostToolUse") {
		t.Fatal("guard kinds")
	}
	if out := string(a.DenyOutput("PermissionRequest", "no")); !strings.Contains(out, `"behavior":"deny"`) {
		t.Fatalf("permission deny %s", out)
	}
}

// The per-run settings force hooks on and enable Claude Code's sandbox in strict mode;
// a profile cannot switch either off.
func TestPrepareSandboxAndHooks(t *testing.T) {
	root := t.TempDir()
	repo, wt := filepath.Join(root, "repo"), filepath.Join(root, "wt")
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@e", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	_ = os.MkdirAll(repo, 0o755)
	git(repo, "init", "-q", "-b", "main")
	git(repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(repo, "branch", "otag/other")
	git(repo, "worktree", "add", "-q", "-b", "otag/al-1", wt)
	real, _ := filepath.EvalSymlinks(repo)
	common := filepath.Join(real, ".git")

	spec := testSpec()
	spec.Workdir = wt
	ls, err := New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		DisableAllHooks *bool `json:"disableAllHooks"`
		Sandbox         struct {
			Enabled, FailIfUnavailable, AllowUnsandboxedCommands *bool
			Filesystem                                           struct {
				AllowWrite, DenyWrite []string
				Disabled              *bool
			}
		}
		Permissions struct{ Deny []string }
	}
	if err := json.Unmarshal(fileMap(ls)[settingsFile], &s); err != nil {
		t.Fatal(err)
	}
	sb := s.Sandbox
	if s.DisableAllHooks == nil || *s.DisableAllHooks || sb.Enabled == nil || !*sb.Enabled ||
		sb.FailIfUnavailable == nil || !*sb.FailIfUnavailable || sb.AllowUnsandboxedCommands == nil || *sb.AllowUnsandboxedCommands {
		t.Fatalf("settings: %s", fileMap(ls)[settingsFile])
	}
	fs := sb.Filesystem
	if fs.Disabled == nil || *fs.Disabled {
		t.Fatal("filesystem.disabled not forced false")
	}
	for _, p := range []string{wt, filepath.Join(common, "objects"), filepath.Join(common, "refs/heads/otag/al-1"),
		filepath.Join(common, "worktrees", "wt"), "/data/otag.db", "/data/otag.db-wal", "/data/otag.db-shm"} {
		if !slices.Contains(fs.AllowWrite, p) {
			t.Errorf("allowWrite lacks %s: %v", p, fs.AllowWrite)
		}
	}
	for _, p := range []string{filepath.Join(common, "refs/heads/main"), filepath.Join(common, "refs/heads/otag/other"),
		filepath.Join(common, "packed-refs"), filepath.Join(common, "HEAD")} {
		if !slices.Contains(fs.DenyWrite, p) {
			t.Errorf("denyWrite lacks %s: %v", p, fs.DenyWrite)
		}
	}
	if slices.Contains(fs.DenyWrite, filepath.Join(common, "refs/heads/otag/al-1")) {
		t.Error("own branch denied")
	}
	if !slices.Contains(s.Permissions.Deny, "Read(/"+wt+"/**/.env*)") || !slices.Contains(s.Permissions.Deny, "Edit(/"+wt+"/.git)") {
		t.Errorf("deny rules %v", s.Permissions.Deny)
	}
	// Same mode: the git dir is the workspace's own and stays writable.
	same := testSpec()
	same.Workdir, same.ProjectPath, same.Branch = repo, repo, ""
	ls, _ = New().Prepare(context.Background(), same)
	if b := string(fileMap(ls)[settingsFile]); strings.Contains(b, "Edit(/"+repo+"/.git") || strings.Contains(b, "packed-refs") {
		t.Errorf("same mode settings deny git: %s", b)
	}
	// A detached review worktree has no branch either, but it is a linked worktree:
	// its .git pointer and every branch ref stay protected.
	review := testSpec()
	review.Workdir, review.ProjectPath, review.Branch = wt, repo, ""
	ls, _ = New().Prepare(context.Background(), review)
	var rs struct {
		Permissions struct{ Deny []string } `json:"permissions"`
		Sandbox     struct {
			Filesystem struct{ AllowWrite, DenyWrite []string } `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(fileMap(ls)[settingsFile], &rs); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rs.Permissions.Deny, "Edit(/"+wt+"/.git)") {
		t.Errorf("detached review worktree: .git not denied: %v", rs.Permissions.Deny)
	}
	for _, p := range []string{filepath.Join(common, "refs/heads/main"), filepath.Join(common, "refs/heads/otag/al-1")} {
		if !slices.Contains(rs.Sandbox.Filesystem.DenyWrite, p) {
			t.Errorf("detached review worktree: denyWrite lacks %s: %v", p, rs.Sandbox.Filesystem.DenyWrite)
		}
	}

	for name, profile := range map[string]string{
		"hooks off":       `{"disableAllHooks":true}`,
		"sandbox off":     `{"sandbox":{"enabled":false}}`,
		"escape hatch":    `{"sandbox":{"allowUnsandboxedCommands":true}}`,
		"filesystem off":  `{"sandbox":{"filesystem":{"disabled":true}}}`,
		"fail open start": `{"sandbox":{"failIfUnavailable":false}}`,
	} {
		bad := testSpec()
		bad.Profile.Args = []string{"--settings", profile}
		if _, err := New().Prepare(context.Background(), bad); err == nil {
			t.Errorf("%s: profile accepted", name)
		}
	}
	ok := testSpec()
	ok.Profile.Args = []string{"--settings", `{"sandbox":{"excludedCommands":["docker *"],"filesystem":{"allowWrite":["~/.cache/go-build"]}}}`}
	if ls, err := New().Prepare(context.Background(), ok); err != nil || !strings.Contains(string(fileMap(ls)[settingsFile]), "go-build") {
		t.Fatalf("profile sandbox additions: %v", err)
	}

	// Managed settings that disable hooks refuse the launch.
	managedSettingsDir = t.TempDir()
	t.Cleanup(func() { managedSettingsDir = "/Library/Application Support/ClaudeCode" })
	_ = os.WriteFile(filepath.Join(managedSettingsDir, "managed-settings.json"), []byte(`{"disableAllHooks":true}`), 0o644)
	if _, err := New().Prepare(context.Background(), testSpec()); err == nil || !strings.Contains(err.Error(), "disableAllHooks") {
		t.Fatalf("managed disableAllHooks: %v", err)
	}
	for _, m := range []string{`{"sandbox":{"filesystem":{"disabled":true}}}`, `{"sandbox":{"allowUnsandboxedCommands":true}}`,
		`{"sandbox":{"failIfUnavailable":false}}`, `{"sandbox":{"enabled":false}}`} {
		_ = os.WriteFile(filepath.Join(managedSettingsDir, "managed-settings.json"), []byte(m), 0o644)
		if _, err := New().Prepare(context.Background(), testSpec()); err == nil {
			t.Errorf("managed %s accepted", m)
		}
	}
	_ = os.Remove(filepath.Join(managedSettingsDir, "managed-settings.json"))

	// A linked worktree whose refs cannot be listed fails the launch.
	broken := testSpec()
	broken.Workdir = wt
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /nonexistent\n"), 0o644)
	if _, err := New().Prepare(context.Background(), broken); err == nil {
		t.Error("unreadable git metadata accepted")
	}
}

func TestUsage(t *testing.T) {
	path, _ := filepath.Abs(filepath.Join("testdata", "transcript.jsonl"))
	raw, _ := json.Marshal(map[string]any{"session_id": "s", "transcript_path": path, "last_assistant_message": "done"})
	res, err := New().ParseEvent(adapter.HookContext{}, "Stop", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 3 || res.Events[0].Type != adapter.TurnEnd {
		t.Fatalf("events %+v", res.Events)
	}
	u2, u3 := res.Events[1].Usage, res.Events[2].Usage
	if u2 == nil || u3 == nil || u2.DedupeKey != "msg_2" || u3.DedupeKey != "msg_3" {
		t.Fatalf("usage %+v %+v", u2, u3)
	}
	if u2.InputTokens != 10 || u2.OutputTokens != 206 || u2.CacheReadTokens != 24883 || u2.CacheWriteTokens != 9629 ||
		u2.ReasoningTokens != 128 || u2.Model != "claude-haiku-5-5" || u2.Source != "transcript" {
		t.Fatalf("msg_2 %+v", u2)
	}

	// The tail read drops a cut first line and still finds the last turns.
	data, _ := os.ReadFile(path)
	big := filepath.Join(t.TempDir(), "t.jsonl")
	pad := `{"type":"user","promptId":"p0","message":{"content":"` + strings.Repeat("x", transcriptTail) + `"}}` + "\n"
	_ = os.WriteFile(big, append([]byte(pad), data...), 0o600)
	if evs := usageEvents(big); len(evs) != 2 {
		t.Fatalf("tail read: %+v", evs)
	}
	if evs := usageEvents(filepath.Join(t.TempDir(), "missing.jsonl")); evs != nil {
		t.Fatalf("missing transcript: %+v", evs)
	}
	if evs := usageEvents(t.TempDir()); evs != nil {
		t.Fatalf("directory as transcript: %+v", evs)
	}

	// A FIFO as the transcript must not block the hook.
	fifo := filepath.Join(t.TempDir(), "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan []adapter.Event, 1)
	go func() { done <- usageEvents(fifo) }()
	select {
	case evs := <-done:
		if evs != nil {
			t.Fatalf("fifo: %+v", evs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usageEvents blocked on a FIFO")
	}
}

// A turn longer than the first tail window keeps its early messages: the read grows
// until it reaches the turn's start.
func TestUsageLongTurn(t *testing.T) {
	var b strings.Builder
	line := func(v any) { j, _ := json.Marshal(v); b.Write(j); b.WriteByte('\n') }
	asst := func(id string, out int) {
		line(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "model": "m",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": out}}})
	}
	line(map[string]any{"type": "user", "promptId": "p1", "message": map[string]any{"content": "old"}})
	asst("msg_old", 1)
	line(map[string]any{"type": "user", "promptId": "p2", "message": map[string]any{"content": "go"}})
	asst("msg_a", 10)
	big := strings.Repeat("y", 3*transcriptTail) // one huge tool result inside the turn
	line(map[string]any{"type": "user", "promptId": "p2", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "content": big}}}})
	asst("msg_b", 20)
	path := filepath.Join(t.TempDir(), "long.jsonl")
	_ = os.WriteFile(path, []byte(b.String()), 0o600)

	var ids []string
	for _, e := range usageEvents(path) {
		ids = append(ids, e.Usage.DedupeKey)
	}
	if strings.Join(ids, " ") != "msg_old msg_a msg_b" {
		t.Fatalf("ids %v", ids)
	}
}

func TestAcceptTrust(t *testing.T) {
	a := New()
	screen := string(fixture(t, "trust-screen.txt"))
	if keys, ok := a.AcceptTrust(screen); !ok || strings.Join(keys, " ") != "Down Enter" {
		t.Fatalf("trust %v %v", keys, ok)
	}
	moved := strings.Replace(strings.Replace(screen, "❯ No, exit", "  No, exit", 1), "  Yes, I trust", "❯ Yes, I trust", 1)
	if keys, ok := a.AcceptTrust(moved); !ok || strings.Join(keys, " ") != "Enter" {
		t.Fatalf("trust with cursor on yes %v %v", keys, ok)
	}
	// Stale dialog text with something else drawn below it (a later prompt or another
	// dialog) must not match, nor a dialog without its title.
	stale := screen + "\n ❯ Try \"fix the tests\"\n"
	other := screen + "\n Bypass Permissions mode\n ❯ No, exit\n   Yes, I accept\n Enter to confirm · Esc to cancel\n"
	noTitle := strings.Replace(screen, "Accessing workspace:", "Something else:", 1)
	for _, s := range []string{"", "> ", "❯ Try \"fix the tests\"", strings.Replace(screen, "❯", " ", 1),
		strings.Replace(screen, "Yes, I trust this folder", "Yes, trust", 1), stale, other, noTitle,
		strings.Replace(screen, "   Yes, I trust", " ❯ Yes, I trust", 1)} { // two cursors
		if _, ok := a.AcceptTrust(s); ok {
			t.Fatalf("matched %q", s)
		}
	}
}

type fakePane struct{ keys []string }

func (p *fakePane) SendKeys(_ context.Context, keys ...string) error {
	p.keys = append(p.keys, keys...)
	return nil
}
func (p *fakePane) SendText(context.Context, string) error  { return nil }
func (p *fakePane) Capture(context.Context) (string, error) { return "", nil }

func TestStop(t *testing.T) {
	p := &fakePane{}
	if err := New().Stop(context.Background(), adapter.Target{Pane: p}); err != nil || strings.Join(p.keys, " ") != "C-c C-c" {
		t.Fatalf("stop keys %v %v", p.keys, err)
	}
}

func TestRegistered(t *testing.T) {
	if New().Harness() != "claude" {
		t.Fatal("harness name")
	}
}
