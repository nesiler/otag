package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

func TestRolloutWatcherSkipsHistoryAndReadsPartialLines(t *testing.T) {
	root := t.TempDir()
	rollout := filepath.Join(root, "rollout.jsonl")
	session := filepath.Join(root, "SessionStart.json")
	old := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"old"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"old","last_agent_message":"history"}}` + "\n"
	start := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"current"}}` + "\n"
	end := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"current","last_agent_message":"new answer"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(old+start+end[:len(end)/2]), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"session_id": "s", "transcript_path": rollout, "source": "resume"})
	_ = os.WriteFile(session, b, 0o600)
	_ = os.WriteFile(filepath.Join(root, "UserPromptSubmit.json"), []byte(`{"session_id":"s","turn_id":"current"}`), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := make(chan string, 5)
	finished := make(chan error, 1)
	go func() {
		finished <- watchRollout(ctx, adapter.Sidecar{Ready: func() error { return nil }, Emit: func(_ context.Context, kind string, b []byte, _ string) ([]byte, error) {
			if strings.Contains(string(b), "old") {
				t.Error("replayed history")
			}
			events <- kind
			return nil, nil
		}}, session, "model")
	}()
	select {
	case kind := <-events:
		if kind != "turn/started" {
			t.Fatal(kind)
		}
	case <-ctx.Done():
		t.Fatal("no start")
	}
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(end[len(end)/2:])
	_ = f.Close()
	select {
	case kind := <-events:
		if kind != "turn/completed" {
			t.Fatal(kind)
		}
	case <-ctx.Done():
		t.Fatal("no end")
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestRolloutUsageAndError(t *testing.T) {
	for _, tt := range []struct {
		raw   string
		types []string
	}{
		{`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t","last_agent_message":null}}`, []string{"error", "turn_end"}},
		{`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2,"reasoning_output_tokens":1,"total_tokens":12},"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2,"reasoning_output_tokens":1,"total_tokens":12}}}}`, []string{"usage"}},
	} {
		kind, p, _ := rolloutEvent([]byte(tt.raw), "s", "t", "model")
		b, _ := json.Marshal(p)
		r, err := New().ParseEvent(adapter.HookContext{}, kind, b)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, ev := range r.Events {
			got = append(got, ev.Type)
			if ev.Type == adapter.Usage && ev.Usage.Source != "rollout" {
				t.Fatal(ev.Usage)
			}
		}
		if strings.Join(got, ",") != strings.Join(tt.types, ",") {
			t.Fatal(got)
		}
	}
}

func TestRolloutRateLimit(t *testing.T) {
	rl := rolloutRateLimit([]byte(`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"rate_limit_reached_type":"rate_limit_reached","primary":{"used_percent":100,"resets_at":1791215285}}}}`))
	if rl == nil {
		t.Fatal("missing rate limit")
	}
	b, _ := json.Marshal(map[string]any{"rateLimits": rl})
	r, err := New().ParseEvent(adapter.HookContext{}, "account/rateLimits/updated", b)
	if err != nil || len(r.Events) != 1 || r.Events[0].ResetsAt.Unix() != 1791215285 {
		t.Fatal(r, err)
	}
}

func TestSessionTrackingPreservesFirstPromptAndResetsPath(t *testing.T) {
	dir := t.TempDir()
	sc := adapter.Sidecar{Getenv: func(string) string { return dir }, Emit: func(context.Context, string, []byte, string) ([]byte, error) { return nil, nil }}
	input := func(kind, raw string) {
		t.Helper()
		old := os.Stdin
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = r
		defer func() { os.Stdin = old; _ = r.Close() }()
		_, _ = w.WriteString(raw)
		_ = w.Close()
		if err := trackSession(context.Background(), sc, kind); err != nil {
			t.Fatal(err)
		}
	}
	input("SessionStart", `{"session_id":"s","transcript_path":"one"}`)
	input("UserPromptSubmit", `{"session_id":"s","turn_id":"first"}`)
	input("UserPromptSubmit", `{"session_id":"s","turn_id":"second"}`)
	b, err := os.ReadFile(filepath.Join(dir, "UserPromptSubmit-s.json"))
	if err != nil || !strings.Contains(string(b), "first") {
		t.Fatal("first prompt overwritten", string(b), err)
	}
	input("SessionStart", `{"session_id":"s","transcript_path":"two"}`)
	if _, err := os.Stat(filepath.Join(dir, "UserPromptSubmit-s.json")); !os.IsNotExist(err) {
		t.Fatal("stale anchor on path change")
	}
	input("UserPromptSubmit", `{"session_id":"s","turn_id":"new"}`)
	b, _ = os.ReadFile(filepath.Join(dir, "UserPromptSubmit-s.json"))
	if !strings.Contains(string(b), "new") {
		t.Fatal(string(b))
	}
}
func TestRolloutWatcherResetsOnNewTranscript(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "SessionStart.json")
	write := func(sid, tid string) {
		t.Helper()
		path := filepath.Join(dir, sid+".jsonl")
		raw := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + tid + `"}}` + "\n" + `{"type":"turn_context","payload":{"model":"actual"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"` + tid + `","last_agent_message":"ok"}}` + "\n"
		_ = os.WriteFile(path, []byte(raw), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "UserPromptSubmit-"+sid+".json"), []byte(`{"session_id":"`+sid+`","turn_id":"`+tid+`"}`), 0o600)
		b, _ := json.Marshal(map[string]any{"session_id": sid, "transcript_path": path})
		tmp := session + ".tmp"
		_ = os.WriteFile(tmp, b, 0o600)
		_ = os.Rename(tmp, session)
	}
	write("one", "first")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ends := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- watchRollout(ctx, adapter.Sidecar{Ready: func() error { return nil }, Emit: func(_ context.Context, kind string, b []byte, _ string) ([]byte, error) {
			if kind == "turn/completed" {
				var p payload
				_ = json.Unmarshal(b, &p)
				ends <- p.Turn.ID
			}
			return nil, nil
		}}, session, "")
	}()
	for _, want := range []string{"first", "new"} {
		select {
		case got := <-ends:
			if got != want {
				t.Fatal(got, want)
			}
		case <-ctx.Done():
			t.Fatal("stale rollout anchor", want)
		}
		if want == "first" {
			write("two", "new")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
