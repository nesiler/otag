package codex

import (
	"context"
	"errors"
	"fmt"

	"github.com/nesiler/otag/internal/adapter"
)

// DeliverReceipt implements adapter.ReceiptDeliverer (mode B) on DeliverReceipt: the
// receipt is the turn the text went into (turn/start's or turn/steer's turn id). A
// refusal before anything reached Codex wraps adapter.ErrNotSent: a "not now" one
// (busy, not_ready, busy_mismatch, waiting_dialog) wraps adapter.ErrNotReady, retried
// soon without using up attempts; rejected and bad_request are definite. unknown (and
// any reason this otag does not know) is ambiguous.
func (*Adapter) DeliverReceipt(ctx context.Context, t adapter.Target, m adapter.Message) (adapter.Receipt, error) {
	if m.ID == "" {
		m.ID = m.Nonce
	}
	rc, err := DeliverReceipt(ctx, t, m)
	if err != nil {
		var de *DeliveryError
		if errors.As(err, &de) {
			switch de.Reason {
			case ReasonBusy, ReasonNotReady, ReasonBusyMismatch, ReasonWaitingDialog:
				return adapter.Receipt{}, fmt.Errorf("%w: %w", adapter.ErrNotReady, err)
			case ReasonRejected, ReasonBadRequest:
				return adapter.Receipt{}, fmt.Errorf("%w: %w", adapter.ErrNotSent, err)
			}
		}
		return adapter.Receipt{}, err
	}
	ch := adapter.ChanAPIStart
	if rc.Kind == "steer" {
		ch = adapter.ChanAPISteer
	}
	return adapter.Receipt{Channel: ch, Ref: rc.TurnID}, nil
}

var _ adapter.ReceiptDeliverer = (*Adapter)(nil)
