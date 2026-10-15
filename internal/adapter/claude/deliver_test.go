package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

func TestDeliverySlots(t *testing.T) {
	a := New()
	for _, c := range []struct {
		kind, payload, ch string
		continued         bool
	}{
		{"PostToolUse", `{"session_id":"s","tool_name":"Bash","tool_use_id":"t1"}`, adapter.ChanToolContext, false},
		{"PostToolUseFailure", `{"session_id":"s","tool_name":"Bash","tool_use_id":"t2"}`, adapter.ChanToolContext, false},
		{"PostToolUse", `{"session_id":"s","tool_name":"Bash","agent_id":"sub"}`, "", false},
		{"UserPromptSubmit", `{"session_id":"s","prompt":"[otag#d4] hi"}`, adapter.ChanPromptContext, false},
		{"Stop", `{"session_id":"s"}`, adapter.ChanStopBlock, false},
		{"Stop", `{"session_id":"s","stop_hook_active":true}`, adapter.ChanStopBlock, true},
		{"PreToolUse", `{"session_id":"s","tool_name":"Read"}`, "", false},
		{"SessionStart", `{"session_id":"s"}`, "", false},
	} {
		res, err := a.ParseEvent(adapter.HookContext{}, c.kind, []byte(c.payload))
		if err != nil {
			t.Fatal(err)
		}
		got, cont := "", false
		if res.Slot != nil {
			got, cont = res.Slot.Channel, res.Slot.Continued
		}
		if got != c.ch || cont != c.continued {
			t.Errorf("%s %s: slot %q continued %v", c.kind, c.payload, got, cont)
		}
	}
}

func TestRenderDelivery(t *testing.T) {
	a := New()
	var out map[string]any
	_ = json.Unmarshal(a.RenderDelivery("Stop", nil, "[otag#d1] go on"), &out)
	if out["decision"] != "block" || out["reason"] != "[otag#d1] go on" {
		t.Fatalf("stop %v", out)
	}
	for _, kind := range []string{"PostToolUse", "PostToolUseFailure", "UserPromptSubmit"} {
		out = nil
		_ = json.Unmarshal(a.RenderDelivery(kind, nil, "[otag#d2] note"), &out)
		hso, _ := out["hookSpecificOutput"].(map[string]any)
		if hso["hookEventName"] != kind || hso["additionalContext"] != "[otag#d2] note" {
			t.Fatalf("%s %v", kind, out)
		}
	}
	if a.RenderDelivery("PreToolUse", nil, "x") != nil {
		t.Fatal("PreToolUse carries no messages")
	}
}

func TestDeliverReceiptInbox(t *testing.T) {
	a := New()
	dir := t.TempDir()
	tg := adapter.Target{RunID: "r", RunDir: dir}
	m := adapter.Message{Nonce: "abc", Text: "[otag#d3] reply OK"}
	if _, err := a.DeliverReceipt(context.Background(), tg, m); !errors.Is(err, adapter.ErrNotReady) {
		t.Fatalf("without the mod: %v", err)
	}
	inbox := filepath.Join(dir, inboxDir)
	_ = os.MkdirAll(inbox, 0o700)
	// A ready file the mod stopped touching: not ready, nothing written.
	_ = os.WriteFile(filepath.Join(inbox, readyFile), []byte("1"), 0o600)
	stale := time.Now().Add(-2 * readyMaxAge)
	_ = os.Chtimes(filepath.Join(inbox, readyFile), stale, stale)
	if _, err := a.DeliverReceipt(context.Background(), tg, m); !errors.Is(err, adapter.ErrNotReady) {
		t.Fatalf("stale ready file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "abc.json")); err == nil {
		t.Fatal("written for a mod that no longer polls")
	}
	_ = os.WriteFile(filepath.Join(inbox, readyFile), []byte("2"), 0o600) // the mod's next tick
	// A file the mod took (emptied) and a stale one are cleaned up.
	_ = os.WriteFile(filepath.Join(inbox, "old.json"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(inbox, "stale.json"), []byte("{}"), 0o600)
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(inbox, "stale.json"), past, past)
	rc, err := a.DeliverReceipt(context.Background(), tg, m)
	if err != nil || rc.Channel != adapter.ChanModSubmit {
		t.Fatalf("%+v %v", rc, err)
	}
	var f inboxFile
	b, _ := os.ReadFile(filepath.Join(inbox, "abc.json"))
	if json.Unmarshal(b, &f) != nil || f.Text != m.Text || f.Attempt != "abc" {
		t.Fatalf("inbox file %s", b)
	}
	for _, gone := range []string{"old.json", "stale.json"} {
		if _, err := os.Stat(filepath.Join(inbox, gone)); err == nil {
			t.Errorf("%s not cleaned", gone)
		}
	}
	if _, err := a.DeliverReceipt(context.Background(), tg, adapter.Message{Nonce: "../x", Text: "t"}); !errors.Is(err, adapter.ErrNotSent) {
		t.Fatalf("bad nonce: %v", err)
	}
}

// The mod touches its ready file on every poll tick (DeliverReceipt's freshness
// check), and skips inbox files only after the mod_submit proof timeout could no
// longer prove them.
func TestModHeartbeat(t *testing.T) {
	if modMaxAge >= 3*time.Minute {
		t.Fatalf("modMaxAge %s must stay below the 3m mod_submit proof timeout", modMaxAge)
	}
	tick := registerTS[strings.Index(registerTS, "$.clock.every(1000"):]
	tick = tick[:strings.Index(tick, "})")]
	if !strings.Contains(tick, "markReady($)") || !strings.Contains(tick, "takeInbox($)") {
		t.Fatalf("poll tick %q", tick)
	}
	if readyMaxAge > 5*time.Second || readyMaxAge < 2*time.Second {
		t.Fatalf("readyMaxAge %s", readyMaxAge)
	}
}
