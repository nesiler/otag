package run

import (
	"context"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/claude"
	"github.com/nesiler/otag/internal/adapter/opencode"
	"github.com/nesiler/otag/internal/db"
)

// primeRun adds an idle child run x-2-r1 on its own account/profile of the harness.
func primeRun(t *testing.T, d *db.DB, harness string) {
	t.Helper()
	_, err := d.Exec(`
INSERT INTO accounts (id, name, harness, config_hash) VALUES (2, 'h2', ?, 'h');
INSERT INTO profiles (id, name, account_id, config_hash) VALUES (2, 'p2', 2, 'h');
INSERT INTO profile_revisions (id, profile_id, account_id, harness, config_hash, config) VALUES (2, 2, 2, ?, 'h', '{}');
INSERT INTO tasks (id, project_id, title, status) VALUES ('x-2', 1, 't2', 'running');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('x-2-r1', 1, 'x-2', 'child', 2, 2, 2, ?, 'idle');
UPDATE tasks SET current_run_id = 'x-2-r1' WHERE id = 'x-2';
INSERT INTO sessions (run_id, harness) VALUES ('x-2-r1', ?)`, harness, harness, harness, harness)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrimeHookReinjects(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	primeRun(t, d, "claude")
	reg := adapter.NewRegistry(claude.New(), opencode.New())
	hook := func(kind, payload string) HookResponse {
		t.Helper()
		resp, err := HandleHook(ctx, d, reg, HookRequest{Harness: "claude", Kind: kind, RunID: "x-2-r1", Payload: []byte(payload)})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		return resp
	}
	out := string(hook("SessionStart", `{"session_id":"s1","source":"compact"}`).Output)
	for _, want := range []string{`"hookEventName":"SessionStart"`, `"additionalContext"`, "x-2-r1", "otag done"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact output lacks %q: %s", want, out)
		}
	}
	if out := string(hook("SessionStart", `{"session_id":"s1","source":"startup"}`).Output); strings.Contains(out, "additionalContext") {
		t.Errorf("startup output carries context: %s", out)
	}
}

func TestPrimeMessageOnCompaction(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	primeRun(t, d, "opencode")
	reg := adapter.NewRegistry(opencode.New())
	frame := `{"id":"evt_c","type":"session.compaction.ended","data":{"sessionID":"ses_a","reason":"auto"}}`
	for i := 0; i < 2; i++ { // a repeated frame queues nothing new
		if _, err := HandleHook(ctx, d, reg, HookRequest{Harness: "opencode", Kind: "session.compaction.ended",
			RunID: "x-2-r1", Payload: []byte(frame)}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var body string
	_ = d.QueryRow(`SELECT count(*), coalesce(max(body), '') FROM messages WHERE sender_kind = 'system'`).Scan(&n, &body)
	if n != 1 || !strings.Contains(body, "x-2-r1") {
		t.Fatalf("system messages %d: %q", n, body)
	}
}

func TestBuildPrimeKinds(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	p, err := BuildPrime(ctx, d, PrimeInput{RunID: "x-1-r1"})
	if err != nil || p.Kind != PrimeChild {
		t.Fatalf("%+v %v", p, err)
	}
	text := p.Text()
	for _, want := range []string{"run x-1-r1", "Task x-1: t", "otag done", "otag ask", "No unread messages"} {
		if !strings.Contains(text, want) {
			t.Errorf("child text lacks %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "\n"); n > 60 {
		t.Errorf("child prime is %d lines", n)
	}
	p, err = BuildPrime(ctx, d, PrimeInput{ProjectID: 1})
	if err != nil || p.Kind != PrimeProject || !strings.Contains(p.Text(), "running 1") {
		t.Fatalf("%+v %v\n%s", p, err, p.Text())
	}
	p, err = BuildPrime(ctx, d, PrimeInput{})
	if err != nil || p.Kind != PrimeNone || !strings.Contains(p.Text(), "outside any registered project") {
		t.Fatalf("%+v %v", p, err)
	}
}
