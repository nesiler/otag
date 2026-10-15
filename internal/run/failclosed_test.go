package run

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/claude"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/workspace"
)

// A guarded hook (fake pre_tool) answers deny whenever otag cannot apply the run's
// policy, and a decision survives a failed write; other hooks keep the no-op.
func TestGuardedHookFailsClosed(t *testing.T) {
	ctx := context.Background()
	reg := adapter.NewRegistry(fake.New())
	wd := t.TempDir()
	const tok = "tok-0123456789abcdef"
	setup := func(t *testing.T) *db.DB {
		d := setupRun(t)
		if _, err := d.Exec(`UPDATE runs SET status = 'running', token_hash = '`+tokenHash(tok)+`';
			INSERT INTO workspaces (project_id, task_id, run_id, mode, path, status) VALUES (1, 'x-1', 'x-1-r1', 'same', ?, 'active')`, wd); err != nil {
			t.Fatal(err)
		}
		return d
	}
	push := HookRequest{Harness: "fake", Kind: "pre_tool", RunID: "x-1-r1", Payload: []byte(`{"command":"git push"}`),
		Getenv: func(k string) string { return map[string]string{TokenEnv: tok}[k] }}
	ls := push
	ls.Payload = []byte(`{"command":"ls"}`)
	denies := func(out []byte) bool { return strings.Contains(string(out), `"decision":"deny"`) }

	d := setup(t)
	if resp, err := HandleHook(ctx, d, reg, push); err != nil || !denies(resp.Output) {
		t.Fatalf("healthy deny: %s %v", resp.Output, err)
	}
	if resp, err := HandleHook(ctx, d, reg, ls); err != nil || denies(resp.Output) {
		t.Fatalf("healthy allow: %s %v", resp.Output, err)
	}

	for name, c := range map[string]struct {
		d   func(t *testing.T) *db.DB
		req HookRequest
	}{
		"no database":   {func(*testing.T) *db.DB { return nil }, ls},
		"unknown run":   {setup, HookRequest{Harness: "fake", Kind: "pre_tool", RunID: "ghost", Payload: ls.Payload}},
		"no run at all": {setup, HookRequest{Harness: "fake", Kind: "pre_tool", Payload: ls.Payload}},
		"malformed":     {setup, HookRequest{Harness: "fake", Kind: "pre_tool", RunID: "x-1-r1", Payload: []byte(`{"command":`)}},
		"ended run": {func(t *testing.T) *db.DB {
			d := setup(t)
			_, _ = d.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'done', ended_at = '2026-01-01T00:00:00.000Z'`)
			return d
		}, ls},
		"schema too new": {func(t *testing.T) *db.DB {
			d := setup(t)
			_, _ = d.Exec(`PRAGMA user_version = 999`)
			ro, err := db.OpenWith(ctx, d.Path, db.Options{NoMigrate: true})
			if err != nil || !ro.ReadOnly() {
				t.Fatalf("read-only open: %v", err)
			}
			t.Cleanup(func() { _ = ro.Close() })
			return ro
		}, ls},
		"legacy run without a token": {func(t *testing.T) *db.DB {
			d := setup(t)
			_, _ = d.Exec(`UPDATE runs SET token_hash = NULL`)
			return d
		}, ls},
		"forged token": {func(t *testing.T) *db.DB {
			d := setup(t)
			_, _ = d.Exec(`UPDATE runs SET token_hash = ?`, tokenHash("right-token-0123456"))
			return d
		}, func() HookRequest {
			r := ls
			r.Getenv = func(k string) string { return map[string]string{TokenEnv: "wrong"}[k] }
			return r
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := HandleHook(ctx, c.d(t), reg, c.req)
			if !denies(resp.Output) || !strings.Contains(string(resp.Output), "fail closed") {
				t.Fatalf("output %s (err %v)", resp.Output, err)
			}
			// The same failure on a non-guarded kind keeps the harness's no-op.
			other := c.req
			other.Kind = "turn_end"
			if resp, _ := HandleHook(ctx, c.d(t), reg, other); resp.Output != nil {
				t.Fatalf("non-guarded output %s", resp.Output)
			}
		})
	}

	// A failed write never drops the decision.
	d = setup(t)
	if _, err := d.Exec(`CREATE TRIGGER fail_raw BEFORE INSERT ON raw_hook_payloads
		BEGIN SELECT RAISE(ABORT, 'forced write failure'); END`); err != nil {
		t.Fatal(err)
	}
	resp, err := HandleHook(ctx, d, reg, push)
	if err == nil || !denies(resp.Output) || strings.Contains(string(resp.Output), "fail closed") {
		t.Fatalf("deny after a failed write: %s %v", resp.Output, err)
	}
	// The right token is accepted.
	d = setup(t)
	_, _ = d.Exec(`UPDATE runs SET token_hash = ?`, tokenHash("right-token-0123456"))
	ok := ls
	ok.Getenv = func(k string) string { return map[string]string{TokenEnv: "right-token-0123456"}[k] }
	if resp, err := HandleHook(ctx, d, reg, ok); err != nil || resp.Output == nil || denies(resp.Output) {
		t.Fatalf("right token: %s %v", resp.Output, err)
	}
	// The run's token never reaches raw_hook_payloads, debug form included.
	d = setup(t)
	for _, raw := range []bool{false, true} {
		echo := ls
		echo.Kind, echo.RawPayloads = "activity", raw
		echo.Payload = []byte(`{"id":"` + tok + `","tool":"Bash","out":"OTAG_RUN_TOKEN=` + tok + `"}`)
		if _, err := HandleHook(ctx, d, reg, echo); err != nil {
			t.Fatal(err)
		}
	}
	var leaked int
	_ = d.QueryRow(`SELECT count(*) FROM raw_hook_payloads WHERE instr(payload, ?) > 0`, tok).Scan(&leaked)
	if leaked != 0 {
		t.Fatalf("%d payload rows keep the run token", leaked)
	}
}

// Raw payloads keep a compact extract by default; the debug form is redacted.
func TestRawPayloadCompactAndRedacted(t *testing.T) {
	payload := []byte(`{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":".env"},
		"tool_response":"API_KEY=sk-abcdefghijklmnopqrstuv\nplain","hook_event_name":"PostToolUse",
		"headers":{"Authorization":"Bearer abc"},"usage":{"input_tokens":5}}`)
	c := compactPayload(payload)
	for _, gone := range []string{"tool_input", "sk-", ".env", "Bearer"} {
		if strings.Contains(c, gone) {
			t.Errorf("compact keeps %q: %s", gone, c)
		}
	}
	if !strings.Contains(c, `"session_id":"s1"`) || !strings.Contains(c, `"tool_name":"Read"`) || !strings.Contains(c, `"otag_omitted_fields":4`) {
		t.Errorf("compact: %s", c)
	}
	r := redactPayload(payload)
	for _, gone := range []string{"sk-abc", "Bearer abc", "API_KEY=sk"} {
		if strings.Contains(r, gone) {
			t.Errorf("redacted keeps %q: %s", gone, r)
		}
	}
	if !strings.Contains(r, `"input_tokens":5`) || !strings.Contains(r, "plain") || !strings.Contains(r, ".env") {
		t.Errorf("redacted lost data: %s", r)
	}
	if got := compactPayload([]byte(`{}`)); got != `{}` {
		t.Errorf("empty object: %s", got)
	}
	big := strings.Repeat("é", 5000)
	if got := redactPayload([]byte(`{"x":"` + big + `"}`)); len(got) > 2*maxField || !strings.Contains(got, "clipped") {
		t.Errorf("clip: %d bytes", len(got))
	}
}

// A run's environment holds the base variables and the profile's passthrough, never
// whatever else otag's environment carries.
func TestRunEnvAllowlist(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "gh")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws")
	t.Setenv("LITELLM_KEY", "lk")
	t.Setenv("LC_CTYPE", "UTF-8")
	t.Setenv("OTAG_DB", "/x")
	m := &Manager{Getenv: os.Getenv}
	p := &plan{}
	p.rev.Profile.EnvPassthrough = []string{"LITELLM_*", "OTAG_*"}
	env := m.runEnv(p)
	if env["LITELLM_KEY"] != "lk" || env["LC_CTYPE"] != "UTF-8" || env["HOME"] == "" {
		t.Fatalf("missing: %v", env)
	}
	for _, k := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "OTAG_DB", "PATH"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s passed", k)
		}
	}
	// A passed-through credential reaches the adapter and survives its scrub list;
	// the profile's own env wins over a passthrough of the same name.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth")
	t.Setenv("LITELLM_OTHER", "from-env")
	p.rev.Profile.EnvPassthrough = []string{"LITELLM_*", "CLAUDE_CODE_OAUTH_TOKEN"}
	p.rev.Profile.Env = map[string]string{"LITELLM_OTHER": "from-profile"}
	spec := m.runSpec(p, workspace.Result{Path: t.TempDir()})
	if spec.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "oauth" || spec.Env["LITELLM_KEY"] != "lk" {
		t.Fatalf("run spec env: %v", spec.Env)
	}
	if _, ok := spec.Env["LITELLM_OTHER"]; ok {
		t.Error("passthrough overrode the profile env")
	}
	spec.RunDir, spec.OtagBin = t.TempDir(), "/opt/otag"
	ls, err := claude.New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(ls.Argv, " "), "-u CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("approved credential scrubbed: %v", ls.Argv)
	}
}
