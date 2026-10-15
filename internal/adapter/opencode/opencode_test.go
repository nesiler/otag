package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
)

func testAdapter() *Adapter {
	a := New()
	a.FreePort = func() (int, error) { return 4321, nil }
	a.Secret = func() (string, error) { return "s3cret", nil }
	a.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	a.Home = func() (string, error) { return "/home/u", nil }
	return a
}

func testSpec(t *testing.T) adapter.RunSpec {
	return adapter.RunSpec{
		RunID: "al-1-r1", TaskID: "al-1", Project: "alpha", Role: "child", Attempt: 1, Harness: "opencode",
		Account: adapter.Account{Name: "oc", ConfigDir: "/acc", Command: "", Env: map[string]string{"A": "1"}},
		Profile: adapter.Profile{Name: "p", Model: "litellm/deepseek-flash-deepseek", Env: map[string]string{"P": "2"},
			Args: []string{"--auto"}},
		Workdir: "/w/al-1", ProjectPath: "/repo", Branch: "otag/al-1",
		RunDir: filepath.Join(t.TempDir(), "runs", "al-1-r1"), CacheDir: "/cache", OtagBin: "/bin/otag",
		Env:    map[string]string{"OTAG_RUN_ID": "al-1-r1"},
		Prompt: "Task al-1: do it", Instructions: "You are a otag child agent.", Deny: []string{".env*", "infra/prod/**"},
	}
}

func TestPrepare(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	ls, err := a.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spec.RunDir); !os.IsNotExist(err) {
		t.Fatalf("Prepare wrote to the run dir: %v", err)
	}
	tui := filepath.Join(spec.RunDir, "opencode/tui.sh")
	wantArgv := []string{"/bin/sh", tui, "opencode", "--server", "http://127.0.0.1:4321", "--prompt", "Task al-1: do it",
		"--auto", "/w/al-1"}
	if !slices.Equal(ls.Argv, wantArgv) {
		t.Errorf("argv\n got %q\nwant %q", ls.Argv, wantArgv)
	}
	if ls.HarnessSessionID != "" {
		t.Errorf("session id before setup: %q", ls.HarnessSessionID)
	}
	wantEnv := map[string]string{
		"A": "1", "P": "2", "OPENCODE_PASSWORD": "s3cret",
		"XDG_CONFIG_HOME": "/acc/config", "XDG_DATA_HOME": "/acc/data", "XDG_STATE_HOME": "/acc/state",
		"XDG_CACHE_HOME": "/acc/cache", "GH_CONFIG_DIR": "/home/u/.config/gh",
	}
	if len(ls.Env) != len(wantEnv) {
		t.Errorf("env %v", ls.Env)
	}
	for k, v := range wantEnv {
		if ls.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, ls.Env[k], v)
		}
	}

	files := map[string]adapter.File{}
	for _, f := range ls.Files {
		files[f.Path] = f
	}
	for _, p := range []string{"opencode/plugin/otag/package.json", "opencode/plugin/otag/index.js", "opencode/password",
		"opencode/instructions.md", "opencode/tui.sh", "opencode/serve.sh"} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing file %s", p)
		}
	}
	if string(files["opencode/password"].Data) != "s3cret" || files["opencode/password"].Mode != 0 {
		t.Errorf("password file %+v", files["opencode/password"])
	}
	if string(files["opencode/instructions.md"].Data) != spec.Instructions {
		t.Errorf("instructions file")
	}
	if files["opencode/tui.sh"].Mode != 0o700 || files["opencode/serve.sh"].Mode != 0o700 {
		t.Errorf("scripts must be executable")
	}

	s := ls.Server
	if s == nil {
		t.Fatal("no server")
	}
	wantServer := []string{"/bin/sh", filepath.Join(spec.RunDir, "opencode/serve.sh"), "opencode", "serve",
		"--hostname", "127.0.0.1", "--port", "4321"}
	if !slices.Equal(s.Argv, wantServer) || s.Ready != "tcp:127.0.0.1:4321" || s.URL != RecordedURL("http://127.0.0.1:4321", spec.RunDir) ||
		s.Cwd != spec.Workdir || s.Name != "server" {
		t.Errorf("server %+v", s)
	}
	var cfg struct {
		Plugins []struct {
			Package string            `json:"package"`
			Options map[string]string `json:"options"`
		} `json:"plugins"`
		Permissions  []Rule `json:"permissions"`
		Instructions any    `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(s.Env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Plugins) != 1 || cfg.Plugins[0].Package != "file://"+filepath.Join(spec.RunDir, "opencode/plugin/otag") {
		t.Errorf("plugins %+v", cfg.Plugins)
	}
	if o := cfg.Plugins[0].Options; o["otag"] != "/bin/otag" || o["run"] != "al-1-r1" ||
		o["instructions"] != filepath.Join(spec.RunDir, "opencode/instructions.md") {
		t.Errorf("plugin options %v", o)
	}
	if cfg.Instructions != nil {
		t.Errorf("v2 ignores `instructions`; do not rely on it")
	}
	if !slices.Contains(cfg.Permissions, Rule{"shell", "git push *", "deny"}) ||
		!slices.Contains(cfg.Permissions, Rule{"edit", "*/.env*", "deny"}) {
		t.Errorf("permissions %+v", cfg.Permissions)
	}

	if len(ls.Sidecars) != 1 {
		t.Fatalf("sidecars %+v", ls.Sidecars)
	}
	sc := ls.Sidecars[0]
	wantSC := []string{"/bin/otag", "sidecar", "opencode", "--ready-file", filepath.Join(spec.RunDir, "sidecar-events.ready"),
		"--", "--url", "http://127.0.0.1:4321", "--dir", "/w/al-1", "--model", "litellm/deepseek-flash-deepseek"}
	if !slices.Equal(sc.Argv, wantSC) || sc.Ready != "file:"+filepath.Join(spec.RunDir, "sidecar-events.ready") {
		t.Errorf("sidecar %+v", sc)
	}

	// Deterministic for a given spec (port and password come from the injected funcs).
	ls2, _ := a.Prepare(context.Background(), spec)
	if !slices.Equal(ls.Argv, ls2.Argv) || ls.Server.Env["OPENCODE_CONFIG_CONTENT"] != ls2.Server.Env["OPENCODE_CONFIG_CONTENT"] {
		t.Error("Prepare is not deterministic")
	}
}

func TestPrepareResumeAndCommand(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	acc := t.TempDir()
	_ = os.MkdirAll(filepath.Join(acc, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(acc, "bin", "oc"), []byte("#!/bin/sh\n"), 0o755)
	spec.Account = adapter.Account{ConfigDir: acc, Command: "oc"}
	spec.Profile.Args = nil
	spec.Prompt = ""
	spec.ResumeSessionID = "ses_ed90b908effeKEodEwFdduzqcz"
	ls, err := a.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/sh", filepath.Join(spec.RunDir, "opencode/tui.sh"), filepath.Join(acc, "bin", "oc"),
		"--server", "http://127.0.0.1:4321", "-s", spec.ResumeSessionID, "/w/al-1"}
	if !slices.Equal(ls.Argv, want) {
		t.Errorf("argv\n got %q\nwant %q", ls.Argv, want)
	}
	if ls.HarnessSessionID != spec.ResumeSessionID {
		t.Errorf("session id %q", ls.HarnessSessionID)
	}
	if args := strings.Join(ls.Sidecars[0].Argv, " "); !strings.Contains(args, "--session "+spec.ResumeSessionID) ||
		!strings.HasSuffix(args, "--resume") {
		t.Errorf("sidecar %s", args)
	}

	// A command on PATH is kept as is; no account dir means no XDG override.
	a.LookPath = func(string) (string, error) { return "/usr/bin/oc", nil }
	spec.Account = adapter.Account{Command: "oc"}
	ls, _ = a.Prepare(context.Background(), spec)
	if ls.Argv[2] != "oc" || ls.Env["XDG_CONFIG_HOME"] != "" || ls.Env["GH_CONFIG_DIR"] != "" {
		t.Errorf("argv %q env %v", ls.Argv, ls.Env)
	}
}

func TestPrepareErrors(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	spec.Profile.Model = "deepseek"
	if _, err := a.Prepare(context.Background(), spec); err == nil {
		t.Error("a model without provider must fail")
	}
	spec = testSpec(t)
	spec.OtagBin = ""
	if _, err := a.Prepare(context.Background(), spec); err == nil {
		t.Error("missing OtagBin must fail")
	}
}

func TestParseModel(t *testing.T) {
	for in, want := range map[string]*Model{
		"":                                 {},
		"opencode/mimo-v2.6-flash-free":    {ProviderID: "opencode", ID: "mimo-v2.6-flash-free"},
		"litellm/deepseek-flash-deepseek":  {ProviderID: "litellm", ID: "deepseek-flash-deepseek"},
		"openrouter/qwen/qwen3-coder#high": {ProviderID: "openrouter", ID: "qwen/qwen3-coder", Variant: "high"},
	} {
		got, err := ParseModel(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if in == "" {
			if got != nil {
				t.Errorf("empty model = %+v", got)
			}
			continue
		}
		if *got != *want {
			t.Errorf("%q = %+v", in, got)
		}
	}
	for _, bad := range []string{"x", "/m", "p/", "#v"} {
		if _, err := ParseModel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// match is OpenCode's Wildcard.match (packages/core/src/util/wildcard.ts, v2.0.20).
func match(input, pattern string) bool {
	esc := regexp.QuoteMeta(pattern)
	esc = strings.ReplaceAll(esc, `\*`, ".*")
	esc = strings.ReplaceAll(esc, `\?`, ".")
	if strings.HasSuffix(esc, " .*") {
		esc = strings.TrimSuffix(esc, " .*") + "( .*)?"
	}
	return regexp.MustCompile("(?s)^" + esc + "$").MatchString(input)
}

func denied(rules []Rule, action, resource string) bool {
	effect := "allow"
	for _, r := range rules {
		if match(action, r.Action) && match(resource, r.Resource) {
			effect = r.Effect
		}
	}
	return effect == "deny"
}

func TestHardDenies(t *testing.T) {
	rules := HardDenies([]string{".env*", "infra/prod/**", "/secrets/", "*.pem", "db/**/migrations", "[ab].key", "**/fixtures/*.json"})
	for _, c := range []struct {
		action, resource string
		deny             bool
	}{
		{"shell", "git push", true},
		{"shell", "git push origin main", true},
		{"shell", "git pushx", false},
		{"shell", "git status", false},
		{"shell", "git -C . push origin main", true},
		{"shell", "git -c user.name=x push", true},
		{"shell", "git --git-dir=.git push", true},
		{"shell", "env git push origin main", true},
		{"shell", "FOO=1 git push", true},
		{"shell", "env git -C . push", true},
		{"shell", "/usr/bin/git push", true},
		{"shell", "git -C . status", false},
		{"shell", "git commit -m 'push it'", false},
		{"shell", "git log --grep push", false},
		{"edit", ".git/config", true},
		{"edit", "sub/.git/hooks/pre-commit", true},
		{"edit", ".gitignore", false},
		{"edit", ".env", true},
		{"edit", "config/.env.local", true},
		{"edit", "environment.go", false},
		{"edit", "infra/prod/main.tf", true},
		{"edit", "infra/prod", true},
		{"edit", "infra/staging/main.tf", false},
		{"edit", "secrets/key", true},
		{"edit", "a/b/cert.pem", true},
		{"edit", "src/main.go", false},
		{"edit", "db/migrations/1.sql", true}, // ** = zero directories too
		{"edit", "db/a/b/migrations", true},
		{"edit", "db/schema.sql", false},
		{"edit", "a.key", true}, // bracket classes widen to *
		{"edit", "sub/b.key", true},
		{"edit", "fixtures/x.json", true},
		{"edit", "a/fixtures/x.json", true},
	} {
		if got := denied(rules, c.action, c.resource); got != c.deny {
			t.Errorf("%s %q: deny=%v, want %v", c.action, c.resource, got, c.deny)
		}
	}
}

func TestNoopOutput(t *testing.T) {
	a := New()
	if out := a.NoopOutput(KindEvaluate); string(out) != "{}" {
		t.Errorf("NoopOutput(evaluate) = %q, want {} (no opinion)", out)
	}
	for _, k := range []string{KindSession, KindReconcile, KindTUIExit, "session.execution.started", "x"} {
		if out := a.NoopOutput(k); out != nil {
			t.Errorf("NoopOutput(%s) = %q", k, out)
		}
	}
}
