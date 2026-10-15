// Package proc identifies local processes by pid plus birth (start) time, so a pid the
// kernel reused is never taken for the process otag started, and finds their
// descendants. On macOS it reads the kernel's process table with sysctl (KERN_PROC);
// it never runs ps. Elsewhere only existence checks work and births are unknown.
//
// A failed or implausible read is an error, never "the process exited": callers treat
// it as unknown and look again later.
package proc

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// TimeFormat is the layout of Birth; the same as db.TimeFormat (UTC, milliseconds).
const TimeFormat = "2006-01-02T15:04:05.000Z"

// Process is one entry of the process table.
type Process struct {
	PID, PPID, PGID int
	UID             int    // effective user id; -1 when unknown
	Birth           string // start time (TimeFormat); "" when unknown
	Zombie          bool   // exited, not reaped yet: it runs no code any more
}

// ErrUnsupported: the process table cannot be read on this system.
var ErrUnsupported = errors.New("process table not readable on this system")

func formatBirth(t time.Time) string { return t.UTC().Format(TimeFormat) }

// Birth returns the start time of pid, "" when it cannot be read.
func Birth(pid int) string {
	p, ok, err := Get(pid)
	if err != nil || !ok {
		return ""
	}
	return p.Birth
}

// Get returns process pid. ok is false only when the kernel says there is no such
// process; any other failure is an error (unknown).
func Get(pid int) (p Process, ok bool, err error) {
	if pid <= 0 {
		return Process{}, false, nil
	}
	return get(pid)
}

// All returns the whole process table by pid. A table that does not even contain this
// process is rejected as implausible.
func All() (map[int]Process, error) {
	all, err := all()
	if err != nil {
		return nil, err
	}
	if _, ok := all[os.Getpid()]; !ok {
		return nil, errors.New("process table without this process")
	}
	return all, nil
}

// Same reports whether pid still runs the process born at birth (a zombie does not
// count). An error means it could not be told.
func Same(pid int, birth string) (bool, error) {
	p, ok, err := Get(pid)
	if err != nil || !ok {
		return false, err
	}
	return !p.Zombie && (birth == "" || p.Birth == "" || p.Birth == birth), nil
}

// Tree returns root and every descendant of it in all (by parent links), root first.
// Births are taken from the same snapshot, so the result can be verified later with Same.
func Tree(all map[int]Process, root int) []Process {
	r, ok := all[root]
	if !ok || root <= 1 {
		return nil
	}
	children := map[int][]int{}
	for pid, p := range all {
		if pid != p.PPID {
			children[p.PPID] = append(children[p.PPID], pid)
		}
	}
	out := []Process{r}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i].PID] {
			if !seen[c] {
				seen[c] = true
				out = append(out, all[c])
			}
		}
	}
	return out
}

// Family returns the processes that belong to the process otag started as pid, born
// at birth: while it runs, itself, its descendants and the members of its session;
// after it exited, the members of its session born after it. tmux makes every pane
// process a session leader (session id = its pid), and macOS never hands out a pid
// that is still in use as a session or group id, so while a member of that session is
// left no other process can get the pid. This finds children that were reparented to
// launchd when their parent died; one that left the session (setsid) is only found
// through its parent, while that runs. With birth "" (unknown) only a running pid is
// trusted, as the caller vouches for it (its pane still runs it).
func Family(all map[int]Process, pid int, birth string) []Process {
	if pid <= 1 {
		return nil
	}
	var out []Process
	seen := map[int]bool{}
	add := func(ps []Process) {
		for _, p := range ps {
			if p.PID > 1 && !seen[p.PID] {
				seen[p.PID] = true
				out = append(out, p)
			}
		}
	}
	r, ok := all[pid]
	switch {
	case ok && (birth == "" || r.Birth == birth):
		if !r.Zombie {
			add(Tree(all, pid))
		}
		add(sessionMembers(all, pid))
	case !ok && birth != "":
		var left []Process
		for _, m := range sessionMembers(all, pid) {
			if m.Birth >= birth {
				left = append(left, m)
			}
		}
		add(left)
	}
	return out
}

// Ancestors returns pid and every ancestor of it in all (stopping at launchd).
func Ancestors(all map[int]Process, pid int) map[int]bool {
	out := map[int]bool{}
	for p, ok := all[pid]; ok && !out[p.PID] && p.PID > 1; p, ok = all[p.PPID] {
		out[p.PID] = true
	}
	out[pid] = true
	return out
}

// Signal sends sig to p if pid still runs the same process (p.Birth, when known), and
// to its process group as well when p leads one. pid 1 and below are never signalled.
// It reports whether p was still running.
func Signal(p Process, sig syscall.Signal) (bool, error) {
	if p.PID <= 1 {
		return true, errors.New("refusing to signal pid <= 1")
	}
	same, err := Same(p.PID, p.Birth)
	if err != nil || !same {
		return same, err
	}
	if p.PGID == p.PID && p.PGID != syscall.Getpgrp() {
		_ = syscall.Kill(-p.PGID, sig)
	}
	if err := syscall.Kill(p.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return true, err
	}
	return true, nil
}

// exists is the portable existence check: kill(pid, 0).
func exists(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	}
	return false, err
}
