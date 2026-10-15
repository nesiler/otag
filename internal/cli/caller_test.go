package cli

import (
	"encoding/json"
	"testing"

	"github.com/nesiler/otag/internal/run"
)

// The coordinator's privileges need the coordinator's token: a child token with
// OTAG_RUN_ID set to the coordinator is FORBIDDEN, and a child that dropped OTAG_RUN_ID
// cannot review (or message) as the human.
func TestCallerVerified(t *testing.T) {
	h := newMsgHarness(t)
	spoof := map[string]string{"OTAG_RUN_ID": h.coord, run.TokenEnv: "child-token"}
	code, e := h.as(spoof, "task", "add", "spoofed")
	wantCode(t, "child token as the coordinator", code, e, "FORBIDDEN")
	code, e = h.as(map[string]string{"OTAG_RUN_ID": h.coord}, "task", "add", "no token")
	wantCode(t, "coordinator id without a token", code, e, "FORBIDDEN")
	if code, e = h.as(h.coordEnv, "task", "add", "real"); code != 0 {
		t.Fatalf("coordinator task add: %+v", e.Error)
	}

	tokenOnly := map[string]string{run.TokenEnv: "child-token"}
	for _, args := range [][]string{
		{"review", h.task, "--accept"},
		{"review", h.task, "--reject", "no"},
		{"inbox"},
		{"reply", "1", "x"},
	} {
		code, e = h.as(tokenOnly, args...)
		wantCode(t, "token without run id: "+args[0], code, e, "FORBIDDEN")
	}
	// Read-only views stay open.
	if code, e = h.as(tokenOnly, "task", "list"); code != 0 {
		t.Fatalf("task list with a token: %+v", e.Error)
	}
}

// otag inbox prints only what it took.
func TestInboxPrintsTaken(t *testing.T) {
	h := newMsgHarness(t)
	if code, e := h.json("send", h.child, "hello"); code != 0 {
		t.Fatalf("send: %+v", e.Error)
	}
	code, e := h.as(h.childEnv, "inbox")
	var res inboxResult
	_ = json.Unmarshal(e.Data, &res)
	if code != 0 || len(res.Messages) != 1 || res.Messages[0].Delivery.State != "read" {
		t.Fatalf("inbox: %d %s", code, e.Data)
	}
}
