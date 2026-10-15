package fake

import (
	"context"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
)

func TestPrepare(t *testing.T) {
	a := New()
	ls, err := a.Prepare(context.Background(), adapter.RunSpec{RunID: "p-1-r1", RunDir: "/runs/p-1-r1",
		Profile: adapter.Profile{Env: map[string]string{"FAKE_SERVER": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ls.Argv, " ") != "/bin/sh /runs/p-1-r1/fake-harness.sh start busy idle wait" {
		t.Fatalf("argv %v", ls.Argv)
	}
	if ls.HarnessSessionID != "fake-p-1-r1" || ls.Env["FAKE_SESSION_ID"] != ls.HarnessSessionID {
		t.Fatalf("session %q env %v", ls.HarnessSessionID, ls.Env)
	}
	if len(ls.Files) != 4 || ls.Files[0].Mode != 0o700 || ls.Server == nil || ls.Server.Ready != "file:/runs/p-1-r1/server.ready" {
		t.Fatalf("files/server %+v %+v", ls.Files, ls.Server)
	}
}

func TestParseEvent(t *testing.T) {
	a := New()
	res, err := a.ParseEvent(adapter.HookContext{}, "session_start", []byte(`{"session_id":"s1","source":"startup","event_id":"x"}`))
	if err != nil || len(res.Events) != 1 || res.SessionRef != "s1" || res.EventID != "x" {
		t.Fatalf("%+v %v", res, err)
	}
	if ev := res.Events[0]; ev.Type != adapter.SessionStart || ev.HarnessSessionID != "s1" || ev.Reason != "startup" {
		t.Fatalf("event %+v", ev)
	}
	res, _ = a.ParseEvent(adapter.HookContext{}, "usage", []byte(`{"input_tokens":3,"output_tokens":2,"id":"u"}`))
	if u := res.Events[0].Usage; u == nil || u.InputTokens != 3 || u.DedupeKey != "u" {
		t.Fatalf("usage %+v", res.Events[0])
	}
	res, _ = a.ParseEvent(adapter.HookContext{}, "pre_tool", nil)
	if string(res.Output) != `{"decision":"allow"}` || res.Events[0].Type != adapter.Activity {
		t.Fatalf("pre_tool %+v", res)
	}
	if res, _ := a.ParseEvent(adapter.HookContext{}, "something", []byte(`{}`)); len(res.Events) != 0 {
		t.Fatalf("unknown kind produced %+v", res.Events)
	}
	if _, err := a.ParseEvent(adapter.HookContext{}, "turn_end", []byte(`{nope`)); err == nil {
		t.Fatal("bad json accepted")
	}
	if keys, ok := a.AcceptTrust("…\n" + TrustPrompt + " (y/n)"); !ok || strings.Join(keys, " ") != "y Enter" {
		t.Fatalf("trust %v %v", keys, ok)
	}
	if _, ok := a.AcceptTrust("$ "); ok {
		t.Fatal("trust matched a prompt")
	}
}

func TestRegistry(t *testing.T) {
	r := adapter.NewRegistry(New())
	if _, ok := r.Get("fake"); !ok || strings.Join(r.Names(), ",") != "fake" {
		t.Fatal("registry")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate harness accepted")
		}
	}()
	r.Register(New())
}
