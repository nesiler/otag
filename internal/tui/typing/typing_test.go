package typing

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestEncode(t *testing.T) {
	lit := func(s string) Op { return Op{Kind: Literal, Text: s} }
	key := func(s string) Op { return Op{Kind: Key, Text: s} }
	for _, c := range []struct {
		name string
		k    tea.Key
		want Op
	}{
		{"letter", tea.Key{Code: 'a', Text: "a"}, lit("a")},
		{"shift letter", tea.Key{Code: 'a', ShiftedCode: 'A', Text: "A", Mod: tea.ModShift}, lit("A")},
		{"caps lock", tea.Key{Code: 'a', Text: "A", Mod: tea.ModCapsLock}, lit("A")},
		{"symbol", tea.Key{Code: '?', Text: "?"}, lit("?")},
		{"no text", tea.Key{Code: 'x'}, lit("x")},
		{"wide", tea.Key{Code: '中', Text: "中"}, lit("中")},
		{"ime", tea.Key{Code: tea.KeyExtended, Text: "日本語"}, lit("日本語")},
		{"space", tea.Key{Code: tea.KeySpace, Text: " "}, lit(" ")},
		{"enter", tea.Key{Code: tea.KeyEnter}, key("Enter")},
		{"kp enter", tea.Key{Code: tea.KeyKpEnter}, key("KPEnter")},
		{"tab", tea.Key{Code: tea.KeyTab}, key("Tab")},
		{"shift tab", tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}, key("BTab")},
		{"backspace", tea.Key{Code: tea.KeyBackspace}, key("BSpace")},
		{"delete", tea.Key{Code: tea.KeyDelete}, key("DC")},
		{"insert", tea.Key{Code: tea.KeyInsert}, key("IC")},
		{"esc", tea.Key{Code: tea.KeyEscape}, key("Escape")},
		{"alt esc (esc esc)", tea.Key{Code: tea.KeyEscape, Mod: tea.ModAlt}, key("M-Escape")},
		{"up", tea.Key{Code: tea.KeyUp}, key("Up")},
		{"down", tea.Key{Code: tea.KeyDown}, key("Down")},
		{"left", tea.Key{Code: tea.KeyLeft}, key("Left")},
		{"right", tea.Key{Code: tea.KeyRight}, key("Right")},
		{"home", tea.Key{Code: tea.KeyHome}, key("Home")},
		{"end", tea.Key{Code: tea.KeyEnd}, key("End")},
		{"pgup", tea.Key{Code: tea.KeyPgUp}, key("PPage")},
		{"pgdown", tea.Key{Code: tea.KeyPgDown}, key("NPage")},
		{"f1", tea.Key{Code: tea.KeyF1}, key("F1")},
		{"f12", tea.Key{Code: tea.KeyF12}, key("F12")},
		{"ctrl a", tea.Key{Code: 'a', Mod: tea.ModCtrl}, key("C-a")},
		{"ctrl c", tea.Key{Code: 'c', Mod: tea.ModCtrl}, key("C-c")},
		{"alt b", tea.Key{Code: 'b', Mod: tea.ModAlt}, key("M-b")},
		{"alt shift b", tea.Key{Code: 'b', Mod: tea.ModAlt | tea.ModShift}, key("M-B")},
		{"alt shift 1", tea.Key{Code: '1', ShiftedCode: '!', Mod: tea.ModAlt | tea.ModShift}, key("M-!")},
		{"ctrl left", tea.Key{Code: tea.KeyLeft, Mod: tea.ModCtrl}, key("C-Left")},
		{"alt enter", tea.Key{Code: tea.KeyEnter, Mod: tea.ModAlt}, key("M-Enter")},
		{"shift enter", tea.Key{Code: tea.KeyEnter, Mod: tea.ModShift}, key("S-Enter")},
		{"ctrl alt x", tea.Key{Code: 'x', Mod: tea.ModCtrl | tea.ModAlt}, key("C-M-x")},
		{"ctrl shift a", tea.Key{Code: 'a', Mod: tea.ModCtrl | tea.ModShift}, key("C-S-a")},
		{"ctrl space", tea.Key{Code: tea.KeySpace, Mod: tea.ModCtrl}, key("C-Space")},
		{"shift up", tea.Key{Code: tea.KeyUp, Mod: tea.ModShift}, key("S-Up")},
	} {
		got, ok, why := Encode(c.k)
		if !ok || got != c.want {
			t.Errorf("%s: Encode(%+v) = %+v %v %q, want %+v", c.name, c.k, got, ok, why, c.want)
		}
	}
	for _, k := range []tea.Key{{Code: 'c', Mod: tea.ModSuper}, {Code: 'v', Mod: tea.ModMeta | tea.ModShift},
		{Code: 'x', Mod: tea.ModHyper}, {Code: tea.KeyMediaPlay}, {Code: tea.KeyCapsLock}} {
		if op, ok, why := Encode(k); ok || why == "" {
			t.Errorf("Encode(%+v) = %+v, want not sent", k, op)
		}
	}
}

func TestIsReturnKey(t *testing.T) {
	for k, want := range map[tea.Key]bool{
		{Code: 'q', Mod: tea.ModCtrl}: true, {Code: 'q', Mod: tea.ModAlt}: true,
		{Code: 'q', Mod: tea.ModCtrl | tea.ModCapsLock}: true, {Code: 'q', Text: "q"}: false,
		{Code: 'q', Mod: tea.ModCtrl | tea.ModAlt}: false, {Code: 'œ', Text: "œ"}: false,
		{Code: tea.KeyEscape}: false,
	} {
		if IsReturnKey(k) != want {
			t.Errorf("IsReturnKey(%+v) = %v", k, !want)
		}
	}
}

func TestCleanPaste(t *testing.T) {
	if got := CleanPaste("a\r\nb\rc\x1b[201~d\te\x00f"); got != "a\nb\nc[201~d\tef" {
		t.Fatalf("%q", got)
	}
	big := strings.Repeat("é", MaxPaste) // 2 bytes each: cut inside a rune
	if got := CleanPaste(big); len(got) > MaxPaste || !strings.HasPrefix(big, got) || len(got) < MaxPaste-1 {
		t.Fatalf("len %d", len(got))
	}
}

// rec records the ops a sender runs and when.
type rec struct {
	mu   sync.Mutex
	ops  []Op
	at   []time.Time
	gate chan struct{} // when set, each exec waits for a token
}

func (r *rec) exec(_ context.Context, op Op) error {
	if r.gate != nil {
		<-r.gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
	r.at = append(r.at, time.Now())
	return nil
}

func collect(t *testing.T, s *Sender, n int) []Result {
	t.Helper()
	var out []Result
	got := 0
	for got < n {
		select {
		case r := <-s.Results():
			out = append(out, r)
			got += r.N
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d results", got, n)
		}
	}
	return out
}

func TestSenderOneExecPerKeyAndEnterGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &rec{}
	s := NewSender(ctx, r.exec)
	s.gap, s.pasteGap = 80*time.Millisecond, 200*time.Millisecond
	ops := []Op{{Kind: Literal, Pane: "%1", Text: "h"}, {Kind: Literal, Pane: "%1", Text: "i"}, {Kind: Key, Pane: "%1", Text: "Enter"}, {Kind: Key, Pane: "%1", Text: "C-c"},
		{Kind: Paste, Pane: "%1", Text: "x\ny"}, {Kind: Key, Pane: "%1", Text: "M-Enter"}, {Kind: Literal, Pane: "%2", Text: "z"}}
	for _, op := range ops {
		if !s.Send(op) {
			t.Fatal("queue full")
		}
	}
	res := collect(t, s, len(ops))
	if len(res) != len(ops) {
		t.Fatalf("%d results for %d ops: literals merged without a backlog", len(res), len(ops))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Equal(r.ops, ops) {
		t.Fatalf("order %+v", r.ops)
	}
	// Enter waits for the gap after the last text; the key after it waits with it (FIFO).
	if d := r.at[2].Sub(r.at[1]); d < 80*time.Millisecond {
		t.Errorf("Enter %v after the text", d)
	}
	if d := r.at[5].Sub(r.at[4]); d < 200*time.Millisecond {
		t.Errorf("M-Enter %v after the paste", d)
	}
	if d := r.at[3].Sub(r.at[2]); d > 50*time.Millisecond {
		t.Errorf("C-c waited %v (it is no Enter)", d)
	}
}

func TestSenderEnterAfterQuietGoesAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &rec{}
	s := NewSender(ctx, r.exec)
	s.gap = 100 * time.Millisecond
	s.Send(Op{Kind: Literal, Pane: "%1", Text: "a"})
	collect(t, s, 1)
	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	s.Send(Op{Kind: Key, Pane: "%1", Text: "Enter"})
	collect(t, s, 1)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("Enter after a quiet pane waited %v", d)
	}
	// Another pane's text does not hold this pane's Enter.
	s.Send(Op{Kind: Literal, Pane: "%2", Text: "b"})
	start = time.Now()
	s.Send(Op{Kind: Key, Pane: "%1", Text: "Enter"})
	collect(t, s, 2)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("Enter held by another pane: %v", d)
	}
}

func TestSenderCoalescesUnderBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &rec{gate: make(chan struct{})}
	s := NewSender(ctx, r.exec)
	s.gap = time.Millisecond
	// The first op blocks in exec while 100 more queue up: a backlog.
	s.Send(Op{Kind: Literal, Pane: "%1", Text: "0"})
	for i := 0; i < 70; i++ {
		s.Send(Op{Kind: Literal, Pane: "%1", Text: "a"})
	}
	s.Send(Op{Kind: Key, Pane: "%1", Text: "Left"})
	for i := 0; i < 29; i++ {
		s.Send(Op{Kind: Literal, Pane: "%1", Text: "b"})
	}
	go func() {
		for {
			select {
			case r.gate <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}()
	collect(t, s, 101)
	r.mu.Lock()
	defer r.mu.Unlock()
	var texts []string
	for _, op := range r.ops {
		texts = append(texts, op.Text)
	}
	// Order kept; literals merged only up to the key, never across it.
	left := slices.Index(texts, "Left")
	if left < 0 || strings.Join(texts[:left], "") != "0"+strings.Repeat("a", 70) ||
		strings.Join(texts[left+1:], "") != strings.Repeat("b", 29) {
		t.Fatalf("ops %q", texts)
	}
	if len(texts) == 101 {
		t.Fatal("nothing merged")
	}
}

// Cancel skips what is queued of the generation, the Enter waiting at the gate too;
// the next generation goes out.
func TestSenderCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &rec{}
	s := NewSender(ctx, r.exec)
	s.gap = time.Second
	g := s.Gen()
	s.Send(Op{Kind: Literal, Pane: "%1", Text: "x", Gen: g})
	s.Send(Op{Kind: Key, Pane: "%1", Text: "Enter", Gen: g}) // waits a second at the gate
	s.Send(Op{Kind: Literal, Pane: "%1", Text: "y", Gen: g})
	if res := collect(t, s, 1); res[0].Skipped || res[0].Err != nil {
		t.Fatalf("first %+v", res[0])
	}
	start := time.Now()
	g2 := s.Cancel()
	s.Send(Op{Kind: Literal, Pane: "%1", Text: "z", Gen: g2})
	res := collect(t, s, 3)
	if !res[0].Skipped || !res[1].Skipped || res[2].Skipped || res[2].Op.Text != "z" {
		t.Fatalf("results %+v", res)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("the cancelled Enter waited %v at the gate", d)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ops) != 2 || r.ops[0].Text != "x" || r.ops[1].Text != "z" {
		t.Fatalf("sent %+v", r.ops)
	}
}

// A failed send stops the generation at once: the text and the Enter after it are
// skipped.
func TestSenderStopsOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var sent []string
	s := NewSender(ctx, func(_ context.Context, op Op) error {
		if op.Text == "b" {
			return errors.New("tmux send-keys: timeout")
		}
		mu.Lock()
		sent = append(sent, op.Text)
		mu.Unlock()
		return nil
	})
	s.gap = time.Millisecond
	g := s.Gen()
	for _, x := range []string{"a", "b", "c"} {
		s.Send(Op{Kind: Literal, Pane: "%1", Text: x, Gen: g})
	}
	s.Send(Op{Kind: Key, Pane: "%1", Text: "Enter", Gen: g})
	res := collect(t, s, 4)
	if res[0].Err != nil || res[1].Err == nil || !res[2].Skipped || !res[3].Skipped {
		t.Fatalf("results %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sent, []string{"a"}) || s.Gen() == g {
		t.Fatalf("sent %q gen %d", sent, s.Gen())
	}
}

func TestSenderQueueFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &rec{gate: make(chan struct{})}
	s := NewSender(ctx, r.exec)
	dropped := 0
	for i := 0; i < Queue+10; i++ {
		if !s.Send(Op{Kind: Key, Pane: "%1", Text: "Up"}) {
			dropped++
		}
	}
	if dropped == 0 || dropped > 11 { // the first op may be taken before the queue fills
		t.Fatalf("dropped %d", dropped)
	}
}
