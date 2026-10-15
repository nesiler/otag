package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServerShutdownReapsOwnedGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// Both processes deliberately ignore SIGINT, exercising forced cleanup.
		done <- runServer(ctx, filepath.Join(dir, "server.log"), []string{"/bin/sh", "-c", `trap '' INT; sleep 60 & echo "$$ $!" > "$1"; wait`, "sh", pidFile})
	}()
	var pids []int
	deadline := time.Now().Add(5 * time.Second)
	for len(pids) != 2 && time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			for _, field := range strings.Fields(string(b)) {
				pid, err := strconv.Atoi(field)
				if err != nil {
					t.Fatal(err)
				}
				pids = append(pids, pid)
			}
		}
		if len(pids) != 2 {
			pids = nil
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(pids) != 2 {
		t.Fatal("server did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server shutdown did not finish")
	}
	deadline = time.Now().Add(2 * time.Second)
	for _, pid := range pids {
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Fatalf("owned process %d survived: %v", pid, err)
		}
	}
}

// Exercise real setsid descendants and an independent watchdog after SIGKILL.
func TestOwnedProcessHelper(t *testing.T) {
	mode := os.Getenv("OTAG_PROCESS_TEST")
	if mode == "" {
		return
	}
	dir := os.Getenv("OTAG_PROCESS_DIR")
	switch mode {
	case "root":
		signal.Ignore(syscall.SIGINT)
		child := exec.Command(os.Args[0], "-test.run=^TestOwnedProcessHelper$")
		child.Env = append(os.Environ(), "OTAG_PROCESS_TEST=child")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(filepath.Join(dir, "tree"), []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0o600)
		_ = child.Wait()
	case "child":
		signal.Ignore(syscall.SIGINT)
		for {
			time.Sleep(time.Second)
		}
	case "owner":
		args := []string{os.Args[0], "-test.run=^TestOwnedProcessHelper$"}
		_ = os.Setenv("OTAG_PROCESS_TEST", "root")
		_ = runServer(context.Background(), filepath.Join(dir, "log"), args)
	case "watchdog":
		owner, _ := strconv.Atoi(os.Getenv("OTAG_PROCESS_OWNER"))
		root, _ := strconv.Atoi(os.Getenv("OTAG_PROCESS_ROOT"))
		_ = watchOwner(owner, root)
	}
	os.Exit(0)
}
func TestSeparateGroupAndSupervisorCrash(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint(crash), func(t *testing.T) {
			dir := t.TempDir()
			var cancel context.CancelFunc
			var owner *exec.Cmd
			done := make(chan error, 1)
			environment := append(os.Environ(), "OTAG_PROCESS_DIR="+dir, "OTAG_PROCESS_TEST=root", "OTAG_RUN_ID=")
			if crash {
				owner = exec.Command(os.Args[0], "-test.run=^TestOwnedProcessHelper$")
				owner.Env = append(environment, "OTAG_PROCESS_TEST=owner")
				if err := owner.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
			} else {
				ctx, c := context.WithCancel(context.Background())
				cancel = c
				defer c()
				// Use env argv rather than mutating the parent's environment during races.
				go func() {
					done <- runServer(ctx, filepath.Join(dir, "log"), []string{"/usr/bin/env", "OTAG_PROCESS_DIR=" + dir, "OTAG_PROCESS_TEST=root", "OTAG_RUN_ID=", os.Args[0], "-test.run=^TestOwnedProcessHelper$"})
				}()
			}
			var pids []int
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if b, err := os.ReadFile(filepath.Join(dir, "tree")); err == nil {
					for _, s := range strings.Fields(string(b)) {
						pid, _ := strconv.Atoi(s)
						pids = append(pids, pid)
					}
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(pids) != 2 {
				t.Fatal("tree did not start")
			}
			t.Cleanup(func() {
				for _, pid := range pids {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			all, err := processes()
			if err != nil || all[pids[0]].group == all[pids[1]].group {
				t.Fatal("no separate group", err)
			}
			if crash {
				watcher := exec.Command(os.Args[0], "-test.run=^TestOwnedProcessHelper$")
				watcher.Env = append(environment, "OTAG_PROCESS_TEST=watchdog", "OTAG_PROCESS_OWNER="+strconv.Itoa(owner.Process.Pid), "OTAG_PROCESS_ROOT="+strconv.Itoa(pids[0]))
				if err := watcher.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = watcher.Process.Kill(); _ = watcher.Wait() })
				time.Sleep(300 * time.Millisecond)
				if err := owner.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = owner.Wait()
			} else {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("shutdown stuck")
				}
			}
			deadline = time.Now().Add(3 * time.Second)
			for _, pid := range pids {
				for time.Now().Before(deadline) {
					all, _ := processes()
					p, ok := all[pid]
					if !ok || strings.HasPrefix(p.state, "Z") {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				all, _ := processes()
				if p, ok := all[pid]; ok && !strings.HasPrefix(p.state, "Z") {
					t.Fatalf("process %d survived", pid)
				}
			}
		})
	}
}
