//go:build !darwin

package proc

// Without a process table only existence is known: no birth, no parent links.
func get(pid int) (Process, bool, error) {
	ok, err := exists(pid)
	if err != nil || !ok {
		return Process{}, false, err
	}
	return Process{PID: pid, PGID: -1, UID: -1}, true, nil
}

func all() (map[int]Process, error) { return nil, ErrUnsupported }

func sessionMembers(map[int]Process, int) []Process { return nil }
