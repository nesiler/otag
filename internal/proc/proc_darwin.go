package proc

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// szomb is SZOMB of <sys/proc.h>: the process exited and waits to be reaped.
const szomb = 5

func fromKinfo(k *unix.KinfoProc) Process {
	tv := k.Proc.P_starttime
	return Process{
		PID: int(k.Proc.P_pid), PPID: int(k.Eproc.Ppid), PGID: int(k.Eproc.Pgid), UID: int(k.Eproc.Ucred.Uid),
		Birth:  formatBirth(time.Unix(int64(tv.Sec), int64(tv.Usec)*1000)),
		Zombie: k.Proc.P_stat == szomb,
	}
}

func get(pid int) (Process, bool, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A pid without a process returns no entry, which x/sys reports as EIO.
		if errors.Is(err, unix.EIO) {
			if ok, xerr := exists(pid); xerr == nil && !ok {
				return Process{}, false, nil
			}
		}
		return Process{}, false, fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	p := fromKinfo(k)
	if p.PID != pid {
		return Process{}, false, fmt.Errorf("sysctl kern.proc.pid %d returned pid %d", pid, p.PID)
	}
	return p, true, nil
}

// sessionMembers returns the processes of this user in session sid, from all.
func sessionMembers(all map[int]Process, sid int) []Process {
	var out []Process
	uid := os.Geteuid()
	for pid, p := range all {
		if p.UID != uid || p.Zombie {
			continue
		}
		if s, err := unix.Getsid(pid); err == nil && s == sid {
			out = append(out, p)
		}
	}
	return out
}

func all() (map[int]Process, error) {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.proc.all: %w", err)
	}
	out := make(map[int]Process, len(ks))
	for i := range ks {
		p := fromKinfo(&ks[i])
		out[p.PID] = p
	}
	return out, nil
}
