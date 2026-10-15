package typing

import (
	"context"
	"sync/atomic"
	"time"
)

// Result is the outcome of N ops (more than one when literals were merged).
type Result struct {
	Op      Op
	N       int
	Err     error // the send failed: the sender dropped the rest of the generation
	Skipped bool  // not sent: its generation had ended (cancelled, or an earlier failure)
}

// Exec runs one Op (one tmux invocation), after checking that the pane still belongs
// to op.Run.
type Exec func(ctx context.Context, op Op) error

// Sender delivers Ops in order on one goroutine. Ops carry a generation: Cancel (or a
// failed send) ends the current one, and every op of an ended generation still queued,
// or waiting at the Enter gate, is skipped, never sent.
type Sender struct {
	ctx  context.Context
	exec Exec
	in   chan Op
	out  chan Result
	gen  atomic.Uint64
	wake chan struct{} // Cancel wakes an Enter waiting at the gate
	// Gates (EnterGap, PasteGap) and the backlog over which consecutive literals merge.
	gap, pasteGap time.Duration
	backlog       int
	now           func() time.Time
}

// Queue is the sender's channel size. A key that finds it full is not sent, and typing
// mode ends: the keys after a gap must not reach the agent (an Enter would submit a
// text with a hole).
const Queue = 4096

// NewSender starts a sender; it ends with ctx.
func NewSender(ctx context.Context, exec Exec) *Sender {
	s := &Sender{ctx: ctx, exec: exec, in: make(chan Op, Queue), out: make(chan Result, Queue),
		wake: make(chan struct{}, 1), gap: EnterGap, pasteGap: PasteGap, backlog: 64, now: time.Now}
	go s.loop()
	return s
}

// Gen is the current generation: ops sent with it go out until it ends.
func (s *Sender) Gen() uint64 { return s.gen.Load() }

// Cancel ends the current generation: what is queued of it is skipped. It returns the
// new generation.
func (s *Sender) Cancel() uint64 {
	g := s.gen.Add(1)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return g
}

// Send queues op without blocking; false: the queue is full and op was dropped.
func (s *Sender) Send(op Op) bool {
	select {
	case s.in <- op:
		return true
	default:
		return false
	}
}

// Results delivers one Result per queued Op (per merge of literals), sent or skipped.
func (s *Sender) Results() <-chan Result { return s.out }

func (s *Sender) loop() {
	ready := map[string]time.Time{} // per pane: when an Enter may go
	var next *Op
	for {
		var op Op
		if next != nil {
			op, next = *next, nil
		} else {
			select {
			case <-s.ctx.Done():
				return
			case op = <-s.in:
			}
		}
		n := 1
		// Under a backlog (a held key, a burst) consecutive literals to the same pane
		// and run merge into one write; never across another key or a generation.
		if op.Kind == Literal && len(s.in) > s.backlog {
			for len(s.in) > 0 {
				o := <-s.in
				if o.Kind != Literal || o.Pane != op.Pane || o.Run != op.Run || o.Gen != op.Gen {
					next = &o
					break
				}
				op.Text += o.Text
				n++
			}
		}
		res := Result{Op: op, N: n}
		if !s.wait(op, ready) {
			res.Skipped = true
		} else if res.Err = s.exec(s.ctx, op); res.Err != nil {
			// Stop at once: the rest of this generation (more text, the Enter) would
			// submit an incomplete input.
			s.gen.CompareAndSwap(op.Gen, op.Gen+1)
		} else {
			switch op.Kind {
			case Literal:
				ready[op.Pane] = later(ready[op.Pane], s.now().Add(s.gap))
			case Paste:
				ready[op.Pane] = later(ready[op.Pane], s.now().Add(s.pasteGap))
			}
		}
		select {
		case s.out <- res:
		case <-s.ctx.Done():
			return
		}
	}
}

// wait holds an Enter at the gate and reports whether op may go: its generation is
// still the current one (checked again after the wait).
func (s *Sender) wait(op Op, ready map[string]time.Time) bool {
	for isEnter(op) && op.Gen == s.gen.Load() {
		w := ready[op.Pane].Sub(s.now())
		if w <= 0 {
			break
		}
		t := time.NewTimer(w)
		select {
		case <-s.ctx.Done():
			t.Stop()
			return false
		case <-s.wake:
		case <-t.C:
		}
		t.Stop()
	}
	return op.Gen == s.gen.Load() && s.ctx.Err() == nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
