package daemon

import (
	"os"
	"syscall"
)

// LogMaxBytes is the size at which the launch agent's log is rotated.
const LogMaxBytes = 10 << 20

// RotateLog keeps the daemon log bounded. launchd opens StandardOutPath/
// StandardErrorPath itself and hands the daemon the descriptors, so when path is
// larger than max, RotateLog renames it to path.1 (replacing an older one), creates a
// new path (0600) and points fds (1 and 2 for the daemon) at it with dup2, so the
// logger and panics write to the new file. It reports whether it rotated.
func RotateLog(path string, max int64, fds ...int) (bool, error) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= max {
		return false, nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return true, err
	}
	defer func() { _ = f.Close() }()
	for _, fd := range fds {
		if err := syscall.Dup2(int(f.Fd()), fd); err != nil {
			return true, err
		}
	}
	return true, nil
}
