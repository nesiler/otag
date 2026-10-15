package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/proc"
)

// Include birth time in each snapshot; verify a pid/pgid birth before signalling. ps is
// used on both Darwin and Linux. Only groups whose leader belongs to the owned
// tree are signalled as groups; PTYs/setsid descendants are included too.
type process struct {
	pid, parent, group int
	birth, state       string
}

// pollInterval paces the ps snapshots of supervisors and watchdogs. Each snapshot
// forks ps over every process on the host, so sample at most twice a second.
const pollInterval = 500 * time.Millisecond

// processes takes one snapshot of the process table. ps runs in the C locale, whose
// lstart is always five fields ("Mon Oct 14 10:00:00 2026"); under another locale it
// can be fewer or more, and the stat column would be misread. A snapshot that cannot
// be parsed, or that misses this process, is an error: callers treat it as unknown,
// never as "the process exited".
func processes() (map[int]process, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,lstart=,stat=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	out := map[int]process{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 9 {
			return nil, fmt.Errorf("ps: unexpected line %q", line)
		}
		var n [3]int
		for i := range n {
			if n[i], err = strconv.Atoi(f[i]); err != nil {
				return nil, fmt.Errorf("ps: unexpected line %q", line)
			}
		}
		out[n[0]] = process{n[0], n[1], n[2], strings.Join(f[3:8], " "), f[8]}
	}
	if _, ok := out[os.Getpid()]; !ok {
		return nil, fmt.Errorf("ps: snapshot without this process (%d lines)", len(out))
	}
	return out, nil
}
func descendants(all map[int]process, root int, known map[int]process) {
	for changed := true; changed; {
		changed = false
		for pid, p := range all {
			if _, ok := known[pid]; ok {
				continue
			}
			parent, inherited := known[p.parent]
			if pid == root || (inherited && all[p.parent].birth == parent.birth) {
				known[pid] = p
				changed = true
			}
		}
	}
}

// snapshot is processes() tried up to tries times: a failed snapshot says nothing
// about any process, so it is never acted on.
func snapshot(tries int) (map[int]process, error) {
	for i := 1; ; i++ {
		all, err := processes()
		if err == nil || i >= tries {
			return all, err
		}
		time.Sleep(pollInterval)
	}
}

func signalOwned(known map[int]process, root int, sig syscall.Signal) {
	all, err := snapshot(3)
	if err != nil {
		return
	}
	groups := map[int]bool{}
	// Children first, leaving the leader unreaped until group signals are sent.
	for pid, old := range known {
		if pid == root {
			continue
		}
		p, live := all[pid]
		if !live || p.birth != old.birth {
			continue
		}
		leader, owned := known[p.group]
		if owned && all[p.group].birth == leader.birth && p.group != root {
			if !groups[p.group] {
				_ = syscall.Kill(-p.group, sig)
				groups[p.group] = true
			}
		} else {
			_ = syscall.Kill(pid, sig)
		}
	}
	if p, live := all[root]; live && p.birth == known[root].birth {
		if p.group == root {
			_ = syscall.Kill(-root, sig)
		} else {
			_ = syscall.Kill(root, sig)
		}
	}
}

// stillRuns reports whether pid still runs the process born at birth, read on its own
// (sysctl for one pid, not the ps snapshot; elsewhere only its existence). A check
// that fails counts as running: unknown is never an exit.
func stillRuns(pid int, birth string) bool {
	same, err := proc.Same(pid, birth)
	return same || err != nil
}

// killRoot kills root (and its group when it leads one) when it is verifiably still
// the process born at birth: the last resort when no snapshot can be taken.
func killRoot(root int, birth string) {
	if same, err := proc.Same(root, birth); err != nil || !same {
		return
	}
	if p, ok, err := proc.Get(root); err == nil && ok {
		_, _ = proc.Signal(p, syscall.SIGKILL)
	}
}

// An independent watchdog survives supervisor SIGKILL/panic. It remembers the
// child tree while genealogy is available, then verifies births before cleanup. A
// failed snapshot is never taken for an exit; while snapshots fail, the owner and
// the root are checked one by one, and once either is gone the tree is cleaned up
// with what is known and the watchdog ends.
func watchOwner(owner, root int) error {
	ownerBirth, rootBirth := proc.Birth(owner), proc.Birth(root)
	known := map[int]process{}
	var parent process
	seen := false
	for {
		all, err := processes()
		if err == nil {
			p, alive := all[owner]
			if !seen {
				if !alive {
					descendants(all, root, known)
					signalOwned(known, root, syscall.SIGKILL)
					return nil
				}
				parent, seen = p, true
			}
			descendants(all, root, known)
			if !alive || p.birth != parent.birth || strings.HasPrefix(p.state, "Z") {
				signalOwned(known, root, syscall.SIGKILL)
				return nil
			}
			// A reaped child means graceful supervision has already cleaned the tree.
			if _, alive := all[root]; !alive {
				signalOwned(known, root, syscall.SIGKILL)
				return nil
			}
		} else if !stillRuns(owner, ownerBirth) || !stillRuns(root, rootBirth) {
			signalOwned(known, root, syscall.SIGKILL)
			killRoot(root, rootBirth)
			return err
		}
		time.Sleep(pollInterval)
	}
}

func startWatchdog(root int) (*exec.Cmd, error) {
	// Unit tests may run the supervisor without a otag run/CLI entrypoint.
	if os.Getenv("OTAG_RUN_ID") == "" {
		return nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "sidecar", "codex", "--", "watchdog", strconv.Itoa(os.Getpid()), strconv.Itoa(root))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
