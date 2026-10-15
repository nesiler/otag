package daemon

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/apperr"
)

// Label is the launchd label of the daemon.
const Label = "io.github.nesiler.otag"

// ServiceFlag is the `otag daemon` flag the launch agent passes: permanent startup
// problems (invalid config, schema too new, another daemon holds the lock) then exit 0,
// so launchd (KeepAlive SuccessfulExit=false) does not restart the daemon every 10 s,
// and the daemon rotates its own log.
const ServiceFlag = "--service"

// ExitTimeOut is how long launchd waits after SIGTERM before SIGKILL; longer than a
// tick (TickTimeout), so a fallback launch in progress is never cut in half.
const ExitTimeOut = 150

// PlistSpec describes the launch agent.
type PlistSpec struct {
	OtagBin string            // absolute path of the otag binary
	LogPath string            // stdout and stderr: <data>/daemon.log
	Env     map[string]string // EnvironmentVariables (PATH, OTAG_*, passed-through variables)
}

// Plist renders the launch agent: `otag daemon --service`, restarted by launchd only
// when it fails, logging to LogPath.
func Plist(s PlistSpec) []byte {
	var b bytes.Buffer
	esc := func(v string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(v))
		return e.String()
	}
	str := func(indent, v string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, esc(v)) }
	key := func(indent, k string) { fmt.Fprintf(&b, "%s<key>%s</key>\n", indent, esc(k)) }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key("\t", "Label")
	str("\t", Label)
	key("\t", "ProgramArguments")
	b.WriteString("\t<array>\n")
	str("\t\t", s.OtagBin)
	str("\t\t", "daemon")
	str("\t\t", ServiceFlag)
	b.WriteString("\t</array>\n")
	key("\t", "RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("\t", "KeepAlive")
	b.WriteString("\t<dict>\n")
	key("\t\t", "SuccessfulExit")
	b.WriteString("\t\t<false/>\n")
	b.WriteString("\t</dict>\n")
	key("\t", "ThrottleInterval")
	b.WriteString("\t<integer>10</integer>\n")
	key("\t", "ExitTimeOut")
	fmt.Fprintf(&b, "\t<integer>%d</integer>\n", ExitTimeOut)
	key("\t", "ProcessType")
	str("\t", "Background")
	key("\t", "StandardOutPath")
	str("\t", s.LogPath)
	key("\t", "StandardErrorPath")
	str("\t", s.LogPath)
	if len(s.Env) > 0 {
		key("\t", "EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			key("\t\t", k)
			str("\t\t", s.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// ReadPlistEnv returns the EnvironmentVariables of an installed agent (as written by
// Plist).
func ReadPlistEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := xml.NewDecoder(io.LimitReader(f, 1<<20))
	dec.Strict = false
	env := map[string]string{}
	var depth int
	var lastKey, pendingKey string
	inEnv := false
	envDepth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return env, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "dict":
				if lastKey == "EnvironmentVariables" && !inEnv {
					inEnv, envDepth = true, depth
				}
			case "key":
				var k string
				if err := dec.DecodeElement(&k, &t); err != nil {
					return nil, err
				}
				depth--
				lastKey = k
				if inEnv && depth == envDepth {
					pendingKey = k
				}
			case "string":
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return nil, err
				}
				depth--
				if inEnv && depth == envDepth && pendingKey != "" {
					env[pendingKey] = v
					pendingKey = ""
				}
				lastKey = ""
			default:
				if t.Name.Local != "dict" {
					lastKey = ""
				}
			}
		case xml.EndElement:
			if inEnv && t.Name.Local == "dict" && depth == envDepth {
				inEnv = false
			}
			depth--
		}
	}
}

// PlistPATH joins otag's binary dir in front of path, dropping duplicates and entries
// that will not exist for long: relative ones (".", ""), fnm's per-shell
// `fnm_multishells` links (replaced by fnmDefault, fnm's default alias, when given and
// present) and directories under a temporary directory (tmpDirs). launchd gives agents
// only /usr/bin:/bin:/usr/sbin:/sbin, without tmux or the harnesses. It also returns
// the dropped entries.
func PlistPATH(otagBin, path string, tmpDirs []string, fnmDefault string) (string, []string) {
	if path == "" {
		path = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	var out, dropped []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	under := func(p string) bool {
		for _, t := range tmpDirs {
			t = strings.TrimRight(t, "/")
			if t != "" && (p == t || strings.HasPrefix(p, t+"/")) {
				return true
			}
		}
		return false
	}
	add(filepath.Dir(otagBin))
	for _, p := range filepath.SplitList(path) {
		switch {
		case !filepath.IsAbs(p):
			if p != "" {
				dropped = append(dropped, p)
			}
		case strings.Contains(p, "/fnm_multishells/"):
			dropped = append(dropped, p)
			if fnmDefault != "" {
				if st, err := os.Stat(fnmDefault); err == nil && st.IsDir() {
					add(fnmDefault)
				}
			}
		case under(filepath.Clean(p)):
			dropped = append(dropped, p)
		default:
			add(filepath.Clean(p))
		}
	}
	return strings.Join(out, string(os.PathListSeparator)), dropped
}

// Launchd installs and removes the launch agent of one user.
type Launchd struct {
	Home      string // the agent goes to Home/Library/LaunchAgents
	Launchctl string // default "launchctl"
	UID       int
	// Wait bounds how long Install waits for a booted-out agent to disappear, and the
	// pause between bootstrap retries is derived from it; default 10s.
	Wait time.Duration
}

// PlistFile is the agent's path.
func (l Launchd) PlistFile() string {
	return filepath.Join(l.Home, "Library", "LaunchAgents", Label+".plist")
}

func (l Launchd) domain() string  { return fmt.Sprintf("gui/%d", l.UID) }
func (l Launchd) service() string { return l.domain() + "/" + Label }

func (l Launchd) wait() time.Duration {
	if l.Wait > 0 {
		return l.Wait
	}
	return 10 * time.Second
}

// launchctlError is a failed launchctl call with its exit code and output.
type launchctlError struct {
	args []string
	code int
	out  string
	err  error
}

func (e *launchctlError) Error() string {
	return fmt.Sprintf("launchctl %s: %v: %s", strings.Join(e.args, " "), e.err, e.out)
}

func (l Launchd) launchctl(ctx context.Context, args ...string) (string, error) {
	bin := l.Launchctl
	if bin == "" {
		bin = "launchctl"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		e := &launchctlError{args: args, code: -1, out: strings.TrimSpace(string(out)), err: err}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			e.code = ee.ExitCode()
		}
		return string(out), e
	}
	return string(out), nil
}

// launchctlCode is the exit code of a failed launchctl call and its output (-1 if it
// did not run).
func launchctlCode(err error) (int, string) {
	var e *launchctlError
	if errors.As(err, &e) {
		return e.code, e.out
	}
	return -1, ""
}

// notLoaded: bootout of a service launchd does not have (ESRCH 3, or 113 "Could not
// find service").
func notLoaded(err error) bool {
	code, out := launchctlCode(err)
	return code == 3 || code == 113 || strings.Contains(out, "No such process") ||
		strings.Contains(out, "Could not find service")
}

// busy: launchd is still tearing the old instance down (EIO 5 right after a bootout,
// EALREADY 37).
func busy(err error) bool {
	code, out := launchctlCode(err)
	return code == 5 || code == 37 || strings.Contains(out, "Input/output error") ||
		strings.Contains(out, "already in progress")
}

// bootout unloads the agent and waits (bounded) until launchd no longer lists it. An
// agent that is not loaded is fine; any other failure is returned.
func (l Launchd) bootout(ctx context.Context) error {
	if _, err := l.launchctl(ctx, "bootout", l.service()); err != nil {
		if notLoaded(err) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(l.wait())
	for {
		if _, err := l.launchctl(ctx, "print", l.service()); err != nil {
			return nil // gone
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("launchctl bootout %s: the service is still loaded after %s", l.service(), l.wait())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.wait() / 50):
		}
	}
}

// Install writes the agent (replacing an older one; 0600, it may hold passed-through
// variables), unloads an older instance and waits until it is gone, then loads the new
// one with `launchctl bootstrap gui/<uid>`, retrying while launchd still tears the old
// one down.
func (l Launchd) Install(ctx context.Context, s PlistSpec) (string, error) {
	if l.Home == "" || !filepath.IsAbs(s.OtagBin) {
		return "", apperr.New(apperr.Invalid, "launchd install needs HOME and an absolute otag path (got %q)", s.OtagBin)
	}
	path := l.PlistFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o700); err != nil {
		return "", err
	}
	unlock, err := lockDir(ctx, filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer unlock()
	// A new file of our own (O_EXCL, 0600 from the start: the plist may hold secrets),
	// never one that exists or a symlink, renamed over the agent.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+Label+".*.tmp")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(Plist(s))
	if err := errors.Join(werr, tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := l.bootout(ctx); err != nil { // an older version may be loaded
		return path, apperr.New(apperr.Unavailable, "unload the old agent: %v", err)
	}
	for i := 1; ; i++ {
		if _, err = l.launchctl(ctx, "bootstrap", l.domain(), path); err == nil {
			return path, nil
		}
		if !busy(err) || i == 5 {
			break
		}
		select {
		case <-ctx.Done():
			return path, ctx.Err()
		case <-time.After(time.Duration(i) * l.wait() / 20):
		}
	}
	return path, apperr.New(apperr.Unavailable, "%v", err)
}

// Uninstall unloads the agent (`launchctl bootout`) and removes its file. It is not an
// error when the agent is not loaded or not installed; any other bootout failure is.
func (l Launchd) Uninstall(ctx context.Context) (removed bool, err error) {
	if _, err := os.Stat(filepath.Dir(l.PlistFile())); err == nil {
		unlock, err := lockDir(ctx, filepath.Dir(l.PlistFile()))
		if err != nil {
			return false, err
		}
		defer unlock()
	}
	if err := l.bootout(ctx); err != nil {
		return false, apperr.New(apperr.Unavailable, "%v", err)
	}
	err = os.Remove(l.PlistFile())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// lockDir serializes installs and uninstalls (flock on the LaunchAgents directory,
// waiting at most 30 s), so two concurrent `otag daemon install` never interleave their
// bootout/bootstrap.
func lockDir(ctx context.Context, dir string) (func(), error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, apperr.New(apperr.Conflict, "another otag daemon install/uninstall is running (%s: %v)", dir, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// AgentStatus describes the launch agent.
type AgentStatus struct {
	Installed bool   `json:"installed"`
	Loaded    bool   `json:"loaded"`
	Plist     string `json:"plist"`
}

// Status reports whether the agent file exists and launchd has it loaded.
func (l Launchd) Status(ctx context.Context) AgentStatus {
	st := AgentStatus{Plist: l.PlistFile()}
	if _, err := os.Stat(st.Plist); err == nil {
		st.Installed = true
	}
	_, err := l.launchctl(ctx, "print", l.service())
	st.Loaded = err == nil
	return st
}
