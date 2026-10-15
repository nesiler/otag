package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// claudeWindows maps Claude Code's rate_limits keys to window names.
var claudeWindows = map[string]string{"five_hour": "5h", "seven_day": "7d"}

// ParseClaudeStatusline extracts rate_limits from a Claude Code statusLine payload
// (Pro/Max: five_hour, seven_day; gateways: spend_limit). An absent rate_limits gives
// no windows. A five_hour/seven_day used_percentage above 101 is dropped: Claude Code
// has been seen putting the resets_at epoch there.
func ParseClaudeStatusline(payload []byte) ([]Window, error) {
	var p struct {
		RateLimits map[string]*struct {
			Used     *float64 `json:"used_percentage"`
			ResetsAt float64  `json:"resets_at"`
			UsedUSD  *float64 `json:"used_usd"`
			LimitUSD *float64 `json:"limit_usd"`
			Period   string   `json:"period"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, err
	}
	var out []Window
	for _, key := range sortedKeys(p.RateLimits) {
		v := p.RateLimits[key]
		if v == nil {
			continue
		}
		w := Window{Name: key, ResetsAt: epoch(v.ResetsAt)}
		if n, ok := claudeWindows[key]; ok {
			w.Name = n
			if v.Used != nil && *v.Used > 101 {
				v.Used = nil
			}
		}
		if key == "spend_limit" {
			w.Name = "spend"
			if v.Period != "" {
				w.Name += "-" + v.Period
			}
			w.Used, w.Limit = v.UsedUSD, v.LimitUSD
		}
		if v.Used != nil {
			w.UsedPercent = ptr(*v.Used)
		}
		if w.UsedPercent == nil && w.Used == nil {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// ParseStatusline parses a statusLine payload. Only Claude Code's ("rate_limits") is
// supported: harness "" and "claude" both mean it. It returns the harness and source used.
func ParseStatusline(harness string, payload []byte, now time.Time) (string, string, []Window, error) {
	if harness != "" && harness != "claude" {
		return harness, "", nil, fmt.Errorf("statusline: unsupported harness %q", harness)
	}
	w, err := ParseClaudeStatusline(payload)
	return "claude", SourceClaudeStatusline, w, err
}

// StatusFile is the last statusLine observation of an account, written atomically by
// `otag statusline` before it touches the database. Collectors ingest it when the
// database write was skipped or failed.
type StatusFile struct {
	Account    string    `json:"account"`
	Harness    string    `json:"harness"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	RecordedAt time.Time `json:"recorded_at"` // last successful database write
	Windows    []fileWin `json:"windows"`
}

type fileWin struct {
	Name     string    `json:"window"`
	Used     *float64  `json:"used_percent,omitempty"`
	UsedAbs  *float64  `json:"used,omitempty"`
	Limit    *float64  `json:"limit,omitempty"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
	Reached  bool      `json:"reached,omitempty"`
}

// StatusFilePath is <data>/quota/statusline-<account>.json.
func StatusFilePath(dataDir, account string) string {
	return filepath.Join(dataDir, "quota", "statusline-"+filepath.Base(account)+".json")
}

// NewStatusFile builds the file content of an observation.
func NewStatusFile(account, harness, source string, at time.Time, ws []Window) StatusFile {
	f := StatusFile{Account: account, Harness: harness, Source: source, ObservedAt: at.UTC()}
	for _, w := range ws {
		f.Windows = append(f.Windows, fileWin{w.Name, w.UsedPercent, w.Used, w.Limit, w.ResetsAt, w.Reached})
	}
	return f
}

// WindowsOf turns the file back into windows.
func (f StatusFile) WindowsOf() []Window {
	var out []Window
	for _, w := range f.Windows {
		out = append(out, Window{Name: w.Name, UsedPercent: w.Used, Used: w.UsedAbs, Limit: w.Limit,
			ResetsAt: w.ResetsAt, Reached: w.Reached})
	}
	return out
}

// SameWindows reports whether two files carry the same values.
func (f StatusFile) SameWindows(g StatusFile) bool {
	a, err1 := json.Marshal(f.Windows)
	b, err2 := json.Marshal(g.Windows)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

// ReadStatusFile reads a status file; a missing or broken file is ok=false.
func ReadStatusFile(path string) (StatusFile, bool) {
	var f StatusFile
	b, err := readSmall(path, 1<<20)
	if err != nil || json.Unmarshal(b, &f) != nil {
		return StatusFile{}, false
	}
	return f, true
}

// WriteStatusFile writes the file atomically (temp file + rename, 0600).
func WriteStatusFile(path string, f StatusFile) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".statusline-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ClaudeStatusLineCommand finds the statusLine command the user configured for a
// Claude Code session, in Claude Code's precedence order: the project's
// .claude/settings.local.json and .claude/settings.json (projectDir from the payload),
// then the account's settings.json. "" = none.
func ClaudeStatusLineCommand(configDir, projectDir string) string {
	var files []string
	if projectDir != "" {
		files = append(files, filepath.Join(projectDir, ".claude", "settings.local.json"),
			filepath.Join(projectDir, ".claude", "settings.json"))
	}
	if configDir != "" {
		files = append(files, filepath.Join(configDir, "settings.json"))
	}
	for _, f := range files {
		if cmd, ok := statusLineIn(f); ok {
			return cmd
		}
	}
	return ""
}

// statusLineIn reads statusLine.command of a settings file; ok is false when the file
// has no statusLine (so a lower-precedence file is consulted).
func statusLineIn(path string) (string, bool) {
	b, err := readSmall(path, 4<<20)
	if err != nil {
		return "", false
	}
	var s struct {
		StatusLine *struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if json.Unmarshal(b, &s) != nil || s.StatusLine == nil {
		return "", false
	}
	return s.StatusLine.Command, true
}

// PayloadProjectDir is workspace.project_dir (or cwd) of a statusLine payload.
func PayloadProjectDir(payload []byte) string {
	var p struct {
		Cwd       string `json:"cwd"`
		Workspace struct {
			ProjectDir string `json:"project_dir"`
		} `json:"workspace"`
	}
	_ = json.Unmarshal(payload, &p)
	if p.Workspace.ProjectDir != "" {
		return p.Workspace.ProjectDir
	}
	return p.Cwd
}

// RecursionEnv is set for the wrapped command; a `otag statusline` that sees it prints
// nothing instead of wrapping again.
const RecursionEnv = "OTAG_STATUSLINE_DEPTH"

// IsWrapper reports whether cmd looks like `otag statusline` itself (the user put the
// wrapper into the settings file the wrapper reads).
func IsWrapper(cmd string) bool {
	_, ok := wrapperArgs(cmd)
	return ok
}

// wrapperArgs returns the arguments after `otag statusline` when cmd is the wrapper:
// otag in command position (after VAR=value assignments, `exec` or `env`), not "otag
// statusline" appearing as an argument of something else.
func wrapperArgs(cmd string) ([]string, bool) {
	f := shellWords(cmd)
	i := 0
	for i < len(f) && (f[i] == "exec" || f[i] == "env" || assignment.MatchString(f[i])) {
		i++
	}
	if i+1 < len(f) && filepath.Base(f[i]) == "otag" && f[i+1] == "statusline" {
		return f[i+2:], true
	}
	return nil, false
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// UnwrapCommand returns the command a settings statusLine really runs: cmd itself, or
// for `otag statusline … --command C` (the user opted their own sessions into the
// wrapper, docs/QUOTA.md) C, unwrapped again if needed. A wrapper without --command
// wraps nothing: "". A wrapper is never returned, so otag never runs itself.
func UnwrapCommand(cmd string) string {
	for range 5 {
		args, ok := wrapperArgs(cmd)
		if !ok {
			return cmd
		}
		cmd = ""
		for i, a := range args {
			if v, ok := strings.CutPrefix(a, "--command="); ok {
				cmd = v
			} else if v, ok := strings.CutPrefix(a, "-command="); ok {
				cmd = v
			} else if (a == "--command" || a == "-command") && i+1 < len(args) {
				cmd = args[i+1]
			}
		}
		if cmd == "" {
			return ""
		}
	}
	return ""
}

// shellWords splits a POSIX shell command line into words: whitespace separates,
// single quotes are literal, double quotes allow \ \" \$ \` escapes, a backslash
// outside quotes escapes the next character. Operators (; | &&) are not interpreted.
func shellWords(s string) []string {
	var out []string
	var w strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			in = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				w.WriteString(s[i+1:])
				i = len(s)
			} else {
				w.WriteString(s[i+1 : i+1+j])
				i += j + 1
			}
		case c == '"':
			in = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\\\"$`\n", s[i+1]) >= 0 {
					i++
				}
				w.WriteByte(s[i])
			}
		case c == '\\' && i+1 < len(s):
			in = true
			i++
			w.WriteByte(s[i])
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				out = append(out, w.String())
				w.Reset()
				in = false
			}
		default:
			in = true
			w.WriteByte(c)
		}
	}
	if in {
		out = append(out, w.String())
	}
	return out
}

// Passthrough runs the user's statusLine command through /bin/sh with the payload on
// stdin and copies its output. It never fails: errors only end the output early. The
// command runs in a process group of its own; when ctx ends, the whole group is
// killed, so a child that inherited stdout (`sleep 3600 & wait`) cannot keep the
// harness waiting for the end of the output.
func Passthrough(ctx context.Context, command string, payload []byte, stdout, stderr io.Writer, env []string) {
	if strings.TrimSpace(command) == "" {
		return
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = append(env, RecursionEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	_ = cmd.Run()
	if ctx.Err() != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // stragglers of a timed-out command
	}
}

// readSmall reads a regular file of at most limit bytes.
func readSmall(path string, limit int64) ([]byte, error) {
	// Stat first: opening a FIFO would block.
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }
