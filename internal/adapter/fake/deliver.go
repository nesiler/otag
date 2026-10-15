package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nesiler/otag/internal/adapter"
)

// Message delivery of the fake harness (phase 4), for tests:
//
//   - hook channels: a payload with "slot" (tool|prompt|stop) offers a DeliverySlot;
//     RenderDelivery answers {"context": text} or, for stop, {"decision":"block",...};
//   - injected: DeliverReceipt appends the text to RunDir/fake-inbox.txt and returns
//     the receipt "fake-<nonce>". RunDir/fake-deliver selects another outcome:
//     "notsent" (ErrNotSent), "notready" (ErrNotReady) or "unknown" (an ambiguous
//     error). The harness step "inbox" submits what arrived as a prompt (turn_start
//     with the markers).

// InboxFile and ModeFile are the fake harness's delivery files in the run dir.
const (
	InboxFile = "fake-inbox.txt"
	ModeFile  = "fake-deliver"
)

var slots = map[string]string{
	"tool":   adapter.ChanToolContext,
	"prompt": adapter.ChanPromptContext,
	"stop":   adapter.ChanStopBlock,
}

// RenderDelivery implements adapter.HookDeliverer.
func (*Adapter) RenderDelivery(kind string, base []byte, text string) []byte {
	out := map[string]any{"context": text}
	if kind == "turn_end" {
		out = map[string]any{"decision": "block", "reason": text}
	}
	b, _ := json.Marshal(out)
	return b
}

// DeliverReceipt implements adapter.ReceiptDeliverer.
func (*Adapter) DeliverReceipt(_ context.Context, t adapter.Target, m adapter.Message) (adapter.Receipt, error) {
	if t.RunDir == "" {
		return adapter.Receipt{}, fmt.Errorf("fake: no run dir: %w", adapter.ErrNotSent)
	}
	mode, _ := os.ReadFile(filepath.Join(t.RunDir, ModeFile))
	switch strings.TrimSpace(string(mode)) {
	case "notsent":
		return adapter.Receipt{}, fmt.Errorf("fake: refused: %w", adapter.ErrNotSent)
	case "notready":
		return adapter.Receipt{}, fmt.Errorf("fake: not now: %w", adapter.ErrNotReady)
	case "unknown":
		return adapter.Receipt{}, errors.New("fake: no answer")
	}
	f, err := os.OpenFile(filepath.Join(t.RunDir, InboxFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return adapter.Receipt{}, fmt.Errorf("fake: %w: %w", err, adapter.ErrNotSent)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(m.Text + "\n"); err != nil {
		return adapter.Receipt{}, fmt.Errorf("fake: %w", err)
	}
	return adapter.Receipt{Ref: "fake-" + m.Nonce}, nil
}

var (
	_ adapter.HookDeliverer    = (*Adapter)(nil)
	_ adapter.ReceiptDeliverer = (*Adapter)(nil)
)
