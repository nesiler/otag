package proc

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func needTable(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("process table only on macOS")
	}
}

// A process is identified by pid plus birth: the birth is stable, a wrong birth is
// "another process", and an exited (or zombie) process is gone, not unknown.
func TestIdentity(t *testing.T) {
	needTable(t)
	me := os.Getpid()
	b := Birth(me)
	if b == "" || Birth(me) != b {
		t.Fatalf("birth %q unstable or empty", b)
	}
	if same, err := Same(me, b); !same || err != nil {
		t.Fatalf("self: %v %v", same, err)
	}
	if same, err := Same(me, "2001-01-01T00:00:00.000Z"); same || err != nil {
		t.Fatalf("self with another birth: %v %v", same, err)
	}

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	child := Birth(pid)
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	tree := Tree(all, me)
	found := false
	for _, p := range tree {
		found = found || p.PID == pid
	}
	if !found || tree[0].PID != me {
		t.Fatalf("child %d not in the tree of %d: %+v", pid, me, tree)
	}
	if anc := Ancestors(all, pid); !anc[me] || !anc[pid] {
		t.Fatalf("ancestors of the child %v", anc)
	}
	if running, err := Signal(Process{PID: pid, Birth: "2001-01-01T00:00:00.000Z"}, syscall.SIGKILL); running || err != nil {
		t.Fatalf("signalled a process with another birth: %v %v", running, err)
	}
	if running, err := Signal(Process{PID: pid, Birth: child}, syscall.SIGKILL); !running || err != nil {
		t.Fatalf("signal: %v %v", running, err)
	}
	// Killed but not reaped yet: a zombie counts as gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		same, err := Same(pid, child)
		if err != nil {
			t.Fatal(err)
		}
		if !same {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed child still counts as running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Wait()
	if _, ok, err := Get(pid); ok || err != nil {
		t.Fatalf("reaped child: %v %v", ok, err)
	}
	if running, err := Signal(Process{PID: 1}, syscall.SIGKILL); err == nil {
		t.Fatalf("pid 1 signalled: %v", running)
	}
	if Tree(all, 1) != nil || Tree(all, 0) != nil {
		t.Fatal("a tree of launchd or the kernel")
	}
}
