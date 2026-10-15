package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

// Who delivers: the human and the coordinator, never a child (nor a caller with run
// residue). done by hand tells the coordinator; --deliverable lists the task until it
// is delivered; delivered only through `otag task deliver`; stats show the wait.
func TestDeliverCLI(t *testing.T) {
	h := newMsgHarness(t)
	h.env["OTAG_DATA_DIR"] = filepath.Join(h.root, "data")
	h.env["OTAG_TMUX_SOCKET"] = fmt.Sprintf("otag-test-%d-%d", os.Getpid(), time.Now().UnixNano()%1e9)
	h.env["OTAG_TMUX_CONF"] = os.DevNull

	code, e := h.as(h.coordEnv, "task", "deliver", h.task)
	wantCode(t, "deliver a running task", code, e, "INVALID_TRANSITION")
	// The child's run ends; the task is marked done by hand.
	d, err := db.Open(context.Background(), h.env["OTAG_DB"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, err := d.Exec(`UPDATE runs SET status = 'ended', ended_reason = 'stopped' WHERE id = ?`, h.child); err != nil {
		t.Fatal(err)
	}
	if code, e := h.json("task", "edit", h.task, "--status", "done"); code != 0 {
		t.Fatalf("done by hand: %+v", e.Error)
	}
	var to, body string
	_ = d.QueryRow(`SELECT d.recipient_kind, m.body FROM messages m JOIN message_deliveries d ON d.message_id = m.id
		WHERE m.client_msg_id = ?`, "done:"+h.task).Scan(&to, &body)
	if to != "role" || !strings.Contains(body, "otag task deliver "+h.task) {
		t.Fatalf("done notice to %q: %q", to, body)
	}
	list := func() []string {
		t.Helper()
		code, e := h.json("task", "list", "--deliverable")
		var ts []task.Task
		if code != 0 || json.Unmarshal(e.Data, &ts) != nil {
			t.Fatalf("list --deliverable: %d %s", code, e.Data)
		}
		var ids []string
		for _, x := range ts {
			ids = append(ids, x.ID)
		}
		return ids
	}
	if got := list(); len(got) != 1 || got[0] != h.task {
		t.Fatalf("deliverable %v", got)
	}

	code, e = h.json("task", "edit", h.task, "--status", "delivered")
	wantCode(t, "delivered through task edit", code, e, "INVALID_TRANSITION")
	code, e = h.as(h.childEnv, "task", "deliver", h.task)
	wantCode(t, "child delivers", code, e, "FORBIDDEN")
	code, e = h.as(map[string]string{run.TokenEnv: "child-token"}, "task", "deliver", h.task)
	wantCode(t, "run residue delivers", code, e, "FORBIDDEN")

	code, e = h.as(h.coordEnv, "task", "deliver", h.task, "--note", "merged by coord")
	var res run.DeliverResult
	if code != 0 || json.Unmarshal(e.Data, &res) != nil || !res.Delivered || res.TaskStatus != task.Delivered {
		t.Fatalf("coordinator delivers: %d %s %+v", code, e.Data, e.Error)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("deliverable after delivery %v", got)
	}
	_, e = h.json("task", "show", h.task)
	var tk task.Task
	_ = json.Unmarshal(e.Data, &tk)
	if tk.Status != task.Delivered || tk.DeliveredAt == "" || tk.StatusReason != "merged by coord" {
		t.Fatalf("task %+v", tk)
	}
	if out := h.ok("task", "show", h.task); !strings.Contains(out, "delivered:") {
		t.Fatalf("task show:\n%s", out)
	}
	// A delivered task is closed for messages, like a done one.
	code, e = h.json("send", h.task, "one more thing")
	wantCode(t, "message to a delivered task", code, e, "INVALID_TRANSITION")
	if out := h.ok("task", "deliver", h.task); !strings.Contains(out, "was delivered before") {
		t.Fatalf("second deliver:\n%s", out)
	}
	if out := h.ok("status"); !strings.Contains(out, "DELIVERED") {
		t.Fatalf("status:\n%s", out)
	}
	code, e = h.json("stats")
	var st statsResult
	if code != 0 || json.Unmarshal(e.Data, &st) != nil || len(st.Delivery) != 1 || st.Delivery[0].Project != "alpha" ||
		st.Delivery[0].Delivered != 1 || st.Delivery[0].Waiting != 0 || st.Delivery[0].AvgWaitS == nil {
		t.Fatalf("stats: %s", e.Data)
	}
}

// End to end with a real run: the child commits and finishes, the human gets the done
// notice, delivers the unmerged branch (a warning; the worktree goes, the branch
// stays) and, after merging it, deletes the branch.
func TestDeliverRunFlow(t *testing.T) {
	h := newRunHarness(t)
	id := h.addTask("alpha", "Add a line")
	h.start(id)
	h.waitRun(id+"-r1", "run done", func(r run.Run) bool { return r.Status == run.Ended && r.TaskStatus == task.Done })

	code, e := h.json("--project", "alpha", "inbox")
	var in inboxResult
	if code != 0 || json.Unmarshal(e.Data, &in) != nil || len(in.Messages) != 1 {
		t.Fatalf("inbox: %d %s", code, e.Data)
	}
	if b := in.Messages[0].Body; !strings.Contains(b, "Task "+id+" is done") || !strings.Contains(b, "Branch: feature/"+id+", 1 commit(s)") ||
		!strings.Contains(b, "Summary: did the thing") {
		t.Fatalf("notice:\n%s", b)
	}

	wt := filepath.Join(h.env["OTAG_DATA_DIR"], "worktrees", "alpha", id)
	code, out, _ := h.run("task", "deliver", id, "--delete-branch")
	if code != 0 {
		t.Fatalf("deliver: exit %d\n%s", code, out)
	}
	for _, want := range []string{"delivered " + id, "worktree " + wt + ": removed",
		"branch feature/" + id + ": kept (1 commit(s) not in main)", "warning: branch feature/" + id + " has 1 commit(s) that main does not contain"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree left")
	}
	gitIn(t, h.repo, "merge", "-q", "--no-ff", "-m", "merge", "feature/"+id)
	code, e = h.json("task", "deliver", id, "--delete-branch")
	var res run.DeliverResult
	if code != 0 || json.Unmarshal(e.Data, &res) != nil || res.Delivered || len(res.Branches) != 1 || !res.Branches[0].Deleted {
		t.Fatalf("delete branch: %d %s", code, e.Data)
	}
	if out := gitIn(t, h.repo, "branch", "--list", "feature/"+id); out != "" {
		t.Fatalf("branch left: %q", out)
	}
}
