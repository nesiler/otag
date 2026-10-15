package opencode

import (
	"testing"

	"github.com/nesiler/otag/internal/adapter"
)

func TestCompactionPrimesOnlyTheRootSession(t *testing.T) {
	a := New()
	frame := func(otag string) []byte {
		return []byte(`{"id":"evt_c","type":"session.compaction.ended","data":{"sessionID":"ses_a","model":{"providerID":"p","id":"m"},
			"tokens":{"input":900,"output":80,"reasoning":0,"cache":{"read":0,"write":0}}},"otag":` + otag + `}`)
	}
	root, _ := a.ParseEvent(adapter.HookContext{}, "session.compaction.ended", frame(`{"model":"p/m"}`))
	if root.Prime != adapter.PrimeMessage {
		t.Errorf("root compaction: Prime = %q", root.Prime)
	}
	sub, _ := a.ParseEvent(adapter.HookContext{}, "session.compaction.ended", frame(`{"model":"p/m","sub":true}`))
	if sub.Prime != "" {
		t.Errorf("subagent compaction: Prime = %q", sub.Prime)
	}
	if len(sub.Events) != 1 || sub.Events[0].Type != adapter.Usage {
		t.Errorf("subagent usage dropped: %+v", sub.Events)
	}
	failed, _ := a.ParseEvent(adapter.HookContext{}, "session.compaction.failed", frame(`{}`))
	if failed.Prime != "" {
		t.Errorf("failed compaction: Prime = %q", failed.Prime)
	}
}
