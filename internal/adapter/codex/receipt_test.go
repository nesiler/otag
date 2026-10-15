package codex

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
)

// ctlAnswer serves RunDir/c.sock answering every request with line.
func ctlAnswer(t *testing.T, line string) adapter.Target {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "otag-cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, ctlSocket))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(c).ReadBytes('\n')
			_, _ = c.Write([]byte(line + "\n"))
			_ = c.Close()
		}
	}()
	return adapter.Target{RunID: "r", HarnessSessionID: "th", ServerURL: "unix://" + filepath.Join(dir, "as.sock"), RunDir: dir}
}

func TestDeliverReceiptMapping(t *testing.T) {
	a := New()
	m := adapter.Message{Nonce: "n1", Text: "[otag#d1] hi"}
	for _, c := range []struct {
		line, ch, ref         string
		notSent, notReady, ok bool
	}{
		{`{"ok":true,"kind":"start","turn_id":"T1"}`, adapter.ChanAPIStart, "T1", false, false, true},
		{`{"ok":true,"kind":"steer","turn_id":"T2"}`, adapter.ChanAPISteer, "T2", false, false, true},
		{`{"ok":false,"reason":"busy"}`, "", "", true, true, false},
		{`{"ok":false,"reason":"not_ready"}`, "", "", true, true, false},
		{`{"ok":false,"reason":"busy_mismatch"}`, "", "", true, true, false},
		{`{"ok":false,"reason":"waiting_dialog"}`, "", "", true, true, false},
		{`{"ok":false,"reason":"rejected"}`, "", "", true, false, false},
		{`{"ok":false,"reason":"unknown"}`, "", "", false, false, false},
		{`{"ok":false,"reason":"something_new"}`, "", "", false, false, false},
	} {
		rc, err := a.DeliverReceipt(context.Background(), ctlAnswer(t, c.line), m)
		if c.ok {
			if err != nil || rc.Channel != c.ch || rc.Ref != c.ref {
				t.Errorf("%s: %+v %v", c.line, rc, err)
			}
			continue
		}
		if err == nil || errors.Is(err, adapter.ErrNotSent) != c.notSent || errors.Is(err, adapter.ErrNotReady) != c.notReady {
			t.Errorf("%s: err %v (not sent %v, not ready %v)", c.line, err, c.notSent, c.notReady)
		}
	}
	// No socket at all: not sent.
	if _, err := a.DeliverReceipt(context.Background(), adapter.Target{RunID: "r", ServerURL: "unix:///nonexistent/as.sock"}, m); !errors.Is(err, adapter.ErrNotSent) {
		t.Errorf("no socket: %v", err)
	}
}
