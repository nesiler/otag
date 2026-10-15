package run

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/opencode"
	"github.com/nesiler/otag/internal/msg"
)

const compactFrame = `{"id":"evt_c","type":"session.compaction.ended","data":{"sessionID":"ses_a","reason":"auto"}}`

func TestPrimeMessageIsRunBound(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	primeRun(t, d, "opencode")
	reg := adapter.NewRegistry(opencode.New())
	if _, err := HandleHook(ctx, d, reg, HookRequest{Harness: "opencode", Kind: "session.compaction.ended",
		RunID: "x-2-r1", Payload: []byte(compactFrame)}); err != nil {
		t.Fatal(err)
	}
	var taskLinked int
	_ = d.QueryRow(`SELECT count(*) FROM message_deliveries WHERE task_id IS NOT NULL`).Scan(&taskLinked)
	if taskLinked != 0 {
		t.Fatalf("%d delivery(ies) linked to the task", taskLinked)
	}
	// The run ends and a retry run of the same task starts: it must not get the note.
	if _, err := d.Exec(`UPDATE runs SET status = 'ended', ended_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		ended_reason = 'stopped' WHERE id = 'x-2-r1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO runs (id, project_id, task_id, role, attempt, profile_id, profile_revision_id, account_id, harness, status)
		VALUES ('x-2-r2', 1, 'x-2', 'child', 2, 2, 2, 2, 'opencode', 'idle')`); err != nil {
		t.Fatal(err)
	}
	rd, err := msg.ReaderFor(ctx, d, "x-2-r2")
	if err != nil {
		t.Fatal(err)
	}
	if items, err := msg.Inbox(ctx, d, rd, false, 10); err != nil || len(items) != 0 {
		t.Fatalf("retry run sees %d message(s): %v", len(items), err)
	}
	// The sweep expires it (not a dead delivery for otag needs).
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := msg.Expire(ctx, tx, time.Time{}, 100, msg.Actor{Name: "t"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = d.QueryRow(`SELECT state FROM message_deliveries`).Scan(&state)
	if state != msg.Expired {
		t.Fatalf("delivery state %q", state)
	}
}

func TestPrimeMessageHugeTask(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	primeRun(t, d, "opencode")
	if _, err := d.Exec(`UPDATE tasks SET body = ?, extra_prompt = ? WHERE id = 'x-2'`,
		strings.Repeat("é", msg.MaxBody), strings.Repeat("x", msg.MaxBody)); err != nil {
		t.Fatal(err)
	}
	p, err := BuildPrime(ctx, d, PrimeInput{RunID: "x-2-r1"})
	if err != nil || !strings.Contains(p.Text(), "otag done") {
		t.Fatalf("%v\n%s", err, p.Text())
	}
	reg := adapter.NewRegistry(opencode.New())
	resp, err := HandleHook(ctx, d, reg, HookRequest{Harness: "opencode", Kind: "session.compaction.ended",
		RunID: "x-2-r1", Payload: []byte(compactFrame)})
	if err != nil || resp.MsgErr != nil {
		t.Fatalf("hook: %v / %v", err, resp.MsgErr)
	}
	var n, size int
	_ = d.QueryRow(`SELECT count(*), coalesce(max(length(CAST(body AS BLOB))), 0) FROM messages WHERE sender_kind = 'system'`).Scan(&n, &size)
	if n != 1 || size == 0 || size > msg.MaxBody {
		t.Fatalf("queued %d message(s), %d bytes (limit %d)", n, size, msg.MaxBody)
	}
}

func TestPrimeReviewRun(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	if _, err := d.Exec(`
INSERT INTO tasks (id, project_id, title, status) VALUES ('x-9', 1, 'review it', 'running');
INSERT INTO task_deps (task_id, depends_on_id, kind) VALUES ('x-9', 'x-1', 'review_of');
INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status)
VALUES ('x-9-r1', 1, 'x-9', 'review', 1, 1, 1, 'fake', 'idle')`); err != nil {
		t.Fatal(err)
	}
	p, err := BuildPrime(ctx, d, PrimeInput{RunID: "x-9-r1"})
	if err != nil || p.Kind != PrimeReview {
		t.Fatalf("%+v %v", p, err)
	}
	text := p.Text()
	for _, want := range []string{"otag review x-1 --accept", "otag review x-1 --reject", "Do not change the reviewed branch"} {
		if !strings.Contains(text, want) {
			t.Errorf("review text lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"otag done", "Commit your changes", "commit and run"} {
		if strings.Contains(text, bad) {
			t.Errorf("review text has %q:\n%s", bad, text)
		}
	}
}

func TestPrimeHumanInbox(t *testing.T) {
	ctx := context.Background()
	d := setupRun(t)
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, _, err := msg.Send(ctx, tx, msg.SendInput{ProjectID: 1, Kind: msg.KindQuestion, Body: "which db?",
			From: msg.Sender{Kind: msg.SenderRun, RunID: "x-1-r1"}, To: []msg.Recipient{{Kind: msg.ToHuman, Ref: "human"}}},
			msg.Actor{Name: "t"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []PrimeInput{{ProjectID: 1}, {}} {
		p, err := BuildPrime(ctx, d, in)
		if err != nil || len(p.Messages) != 1 || !strings.Contains(p.Text(), "Unread messages") {
			t.Fatalf("%+v: %v %+v\n%s", in, err, p.Messages, p.Text())
		}
	}
}
