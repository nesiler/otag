package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// Message delivery (phase 4, docs/MESSAGES.md):
//
//   - busy: PostToolUse/PostToolUseFailure and UserPromptSubmit hooks carry messages as
//     additionalContext; a Stop hook blocks the turn's end with them
//     ({"decision":"block","reason":...}), proven by the next Stop with
//     stop_hook_active;
//   - idle: the mod submits them as a user turn ($.prompt.submit). DeliverReceipt drops
//     the text into RunDir/inbox/<attempt>.json; the mod polls that directory once a
//     second, empties each file it takes and submits its text. The UserPromptSubmit
//     hook of that turn shows the delivery markers: the proof.

// inboxDir is the mod's inbox in the run dir (run.InboxDir: otag removes it once the
// run has ended and been cleaned up); readyFile is written there by the mod when the
// session starts and on every poll tick (no fresh ready file, no idle delivery).
const (
	inboxDir  = "inbox"
	readyFile = ".mod-ready"
)

// readyMaxAge: a ready file older than this means the mod no longer polls (the session
// hung or exited without otag seeing it). The mod touches it every second.
const readyMaxAge = 5 * time.Second

// inboxMaxAge: the mod ignores (and otag removes) inbox files older than this; their
// attempt has long been settled unknown.
const inboxMaxAge = 10 * time.Minute

// RenderDelivery implements adapter.HookDeliverer.
func (*Adapter) RenderDelivery(kind string, base []byte, text string) []byte {
	var out map[string]any
	if len(base) > 0 && json.Unmarshal(base, &out) != nil {
		out = nil
	}
	if out == nil {
		out = map[string]any{}
	}
	switch kind {
	case "Stop":
		out["decision"] = "block"
		out["reason"] = text
	case "SessionStart", "PostToolUse", "PostToolUseFailure", "UserPromptSubmit":
		hso, _ := out["hookSpecificOutput"].(map[string]any)
		if hso == nil {
			hso = map[string]any{}
		}
		hso["hookEventName"] = kind
		if prev, _ := hso["additionalContext"].(string); prev != "" {
			text = prev + "\n\n" + text
		}
		hso["additionalContext"] = text
		out["hookSpecificOutput"] = hso
	default:
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// inboxFile is what DeliverReceipt writes for the mod.
type inboxFile struct {
	Attempt string `json:"attempt"`
	Text    string `json:"text"`
	At      int64  `json:"at"` // unix ms
}

// DeliverReceipt implements adapter.ReceiptDeliverer for an idle session: the run's
// mod submits the text as a user turn. Without a polling mod (no ready file, or one
// not touched for readyMaxAge) nothing is written (ErrNotReady: tried again soon). A
// written file counts as sent: the mod takes it within a second; the UserPromptSubmit
// marker is the proof.
func (*Adapter) DeliverReceipt(_ context.Context, t adapter.Target, m adapter.Message) (adapter.Receipt, error) {
	if t.RunDir == "" {
		return adapter.Receipt{}, fmt.Errorf("claude: run %s has no run dir: %w", t.RunID, adapter.ErrNotSent)
	}
	if m.Nonce == "" || strings.ContainsAny(m.Nonce, `/\.`) {
		return adapter.Receipt{}, fmt.Errorf("claude: bad attempt nonce %q: %w", m.Nonce, adapter.ErrNotSent)
	}
	dir := filepath.Join(t.RunDir, inboxDir)
	st, err := os.Stat(filepath.Join(dir, readyFile))
	if err != nil {
		return adapter.Receipt{}, fmt.Errorf("claude: the otag mod of run %s is not running: %w", t.RunID, adapter.ErrNotReady)
	}
	if age := time.Since(st.ModTime()); age > readyMaxAge {
		return adapter.Receipt{}, fmt.Errorf("claude: the otag mod of run %s has not polled for %s: %w", t.RunID,
			age.Round(time.Second), adapter.ErrNotReady)
	}
	cleanInbox(dir)
	b, _ := json.Marshal(inboxFile{Attempt: m.Nonce, Text: m.Text, At: time.Now().UnixMilli()})
	tmp := filepath.Join(dir, "."+m.Nonce+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return adapter.Receipt{}, fmt.Errorf("claude: %w: %w", err, adapter.ErrNotSent)
	}
	// The rename makes the file appear whole; the mod lists only *.json.
	if err := os.Rename(tmp, filepath.Join(dir, m.Nonce+".json")); err != nil {
		_ = os.Remove(tmp)
		return adapter.Receipt{}, fmt.Errorf("claude: %w: %w", err, adapter.ErrNotSent)
	}
	return adapter.Receipt{Channel: adapter.ChanModSubmit}, nil
}

// cleanInbox removes inbox files the mod took (emptied) and stale ones.
func cleanInbox(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.Name() == readyFile || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if (info.Size() == 0 && strings.HasSuffix(e.Name(), ".json")) || time.Since(info.ModTime()) > inboxMaxAge {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

var (
	_ adapter.HookDeliverer    = (*Adapter)(nil)
	_ adapter.ReceiptDeliverer = (*Adapter)(nil)
)
