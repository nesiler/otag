package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// Receipt is Codex's acknowledgement of a delivered message: Kind "start" (a new
// turn, result.turn.id) or "steer" (added to the active turn, result.turnId).
type Receipt struct {
	Kind   string
	TurnID string
}

// DeliveryError is a delivery the listener refused or could not confirm. Reason
// is one of the Reason* constants; only ReasonUnknown may have reached Codex.
type DeliveryError struct {
	Reason  string
	Message string
}

func (e *DeliveryError) Error() string {
	if e.Message == "" {
		return "codex delivery: " + e.Reason
	}
	return "codex delivery: " + e.Reason + ": " + e.Message
}

// Unknown reports that the message may have been delivered (never resend it).
func (e *DeliveryError) Unknown() bool { return e.Reason == ReasonUnknown }

// Deliver implements adapter.Deliverer (mode B) through the listener's control
// socket. msg.ID is the attempt nonce and msg.Text must carry the caller's marker.
func (*Adapter) Deliver(ctx context.Context, t adapter.Target, msg adapter.Message) error {
	_, err := DeliverReceipt(ctx, t, msg)
	return err
}

// DeliverReceipt is Deliver returning Codex's receipt. Errors are *DeliveryError.
func DeliverReceipt(ctx context.Context, t adapter.Target, msg adapter.Message) (Receipt, error) {
	path, err := ctlPath(t)
	if err != nil {
		return Receipt{}, err
	}
	if msg.ID == "" {
		return Receipt{}, &DeliveryError{Reason: ReasonBadRequest, Message: "a delivery needs an attempt id"}
	}
	ctx, cancel := context.WithTimeout(ctx, ctlDeadline+5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return Receipt{}, &DeliveryError{Reason: ReasonNotReady, Message: err.Error()}
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	line, _ := json.Marshal(ctlRequest{Op: "deliver", Attempt: msg.ID, Thread: t.HarnessSessionID, Text: msg.Text, Steer: msg.Steer})
	if _, err := conn.Write(append(line, '\n')); err != nil {
		// The listener acts only on a complete line.
		return Receipt{}, &DeliveryError{Reason: ReasonNotReady, Message: err.Error()}
	}
	answer, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Receipt{}, &DeliveryError{Reason: ReasonUnknown, Message: "no answer from the Codex listener: " + err.Error()}
	}
	var resp ctlResponse
	if err := json.Unmarshal(answer, &resp); err != nil {
		return Receipt{}, &DeliveryError{Reason: ReasonUnknown, Message: "bad answer from the Codex listener: " + err.Error()}
	}
	if !resp.OK {
		return Receipt{}, &DeliveryError{Reason: resp.Reason, Message: resp.Error}
	}
	return Receipt{Kind: resp.Kind, TurnID: resp.TurnID}, nil
}

// ctlPath is RunDir/c.sock; the run's server URL is unix://RunDir/as.sock.
func ctlPath(t adapter.Target) (string, error) {
	sock, ok := strings.CutPrefix(t.ServerURL, "unix://")
	if !ok || filepath.Base(sock) != "as.sock" {
		return "", &DeliveryError{Reason: ReasonNotReady, Message: fmt.Sprintf("run %s has no Codex app-server (mode B) to deliver to", t.RunID)}
	}
	return filepath.Join(filepath.Dir(sock), ctlSocket), nil
}

var _ adapter.Deliverer = (*Adapter)(nil)
