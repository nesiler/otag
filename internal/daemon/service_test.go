package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/apperr"
)

func TestSingleInstanceLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if running, _, err := Probe(dir); running || err != nil {
		t.Fatalf("probe before: %v %v", running, err)
	}
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); apperr.Code(err) != apperr.Conflict {
		t.Fatalf("second acquire: %v", err)
	}
	running, pid, err := Probe(dir)
	if !running || pid != os.Getpid() || err != nil {
		t.Fatalf("probe: %v %d %v", running, pid, err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if running, _, _ := Probe(dir); running {
		t.Fatal("still running after release")
	}
	l2, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = l2.Release()
}

// The lock is held by another process too (flock is per open file, not per process).
func TestLockAcrossProcesses(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	out, _ := exec.Command("python3", "-c", `
import fcntl, sys
f = open(sys.argv[1], "r+")
try:
    fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    print("acquired")
except BlockingIOError:
    print("busy")`, LockFile(dir)).CombinedOutput()
	if strings.TrimSpace(string(out)) != "busy" {
		t.Fatalf("other process: %s", out)
	}
}

func TestLaunchdInstall(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	logf := filepath.Join(t.TempDir(), "launchctl.log")
	fake := filepath.Join(t.TempDir(), "launchctl")
	// print succeeds only once bootstrap ran.
	script := "#!/bin/sh\necho \"$*\" >> '" + logf + "'\ncase $1 in\nprint) grep -q bootstrap '" + logf + "' ;;\nbootout) exit 3 ;;\nesac\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	l := Launchd{Home: home, Launchctl: fake, UID: 501}
	if st := l.Status(ctx); st.Installed || st.Loaded {
		t.Fatalf("status before %+v", st)
	}
	spec := PlistSpec{OtagBin: "/opt/otag/bin/otag", LogPath: filepath.Join(home, "data", "daemon.log"),
		Env: map[string]string{"PATH": "/opt/otag/bin:/usr/bin:/bin", "OTAG_DB": "/d/otag & co.db"}}
	path, err := l.Install(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents", "io.github.nesiler.otag.plist"); path != want {
		t.Fatalf("plist at %s", path)
	}
	b, _ := os.ReadFile(path)
	plist := string(b)
	for _, want := range []string{"<string>io.github.nesiler.otag</string>",
		"<string>/opt/otag/bin/otag</string>\n\t\t<string>daemon</string>\n\t\t<string>--service</string>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>",
		"<key>ExitTimeOut</key>\n\t<integer>150</integer>", "<key>StandardErrorPath</key>\n\t<string>" + spec.LogPath,
		"<string>/opt/otag/bin:/usr/bin:/bin</string>", "otag &amp; co.db"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist lacks %q:\n%s", want, plist)
		}
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v %s", err, out)
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("plist mode %v", st.Mode().Perm())
	}
	if env, err := ReadPlistEnv(path); err != nil || len(env) != 2 || env["OTAG_DB"] != "/d/otag & co.db" ||
		env["PATH"] != "/opt/otag/bin:/usr/bin:/bin" {
		t.Fatalf("plist env %v %v", env, err)
	}
	if st := l.Status(ctx); !st.Installed || !st.Loaded {
		t.Fatalf("status after install %+v", st)
	}
	removed, err := l.Uninstall(ctx)
	if err != nil || !removed {
		t.Fatalf("uninstall %v %v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("plist left behind")
	}
	if removed, err := l.Uninstall(ctx); removed || err != nil {
		t.Fatalf("second uninstall %v %v", removed, err)
	}
	calls, _ := os.ReadFile(logf)
	want := "print gui/501/io.github.nesiler.otag\nbootout gui/501/io.github.nesiler.otag\nbootstrap gui/501 " + path +
		"\nprint gui/501/io.github.nesiler.otag\nbootout gui/501/io.github.nesiler.otag\nbootout gui/501/io.github.nesiler.otag\n"
	if string(calls) != want {
		t.Fatalf("launchctl calls:\n%s\nwant\n%s", calls, want)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := ConfigFile(dir)
	c, warns, err := LoadConfig(path)
	if err != nil || len(warns) != 0 || c.Tick != 3*time.Second || c.MaxAttempts != 3 || !c.DesktopNotify ||
		c.QuotaInterval != time.Minute || c.StallAfter != 20*time.Minute || c.ToolStallAfter != 45*time.Minute || c.StallPaneActivity {
		t.Fatalf("defaults %+v %v %v", c, warns, err)
	}
	_ = os.WriteFile(path, []byte(`
[daemon]
tick = "2s"
stall_after = "15m"
tool_stall_after = "1h"
stall_pane_activity = true
unanswered_after = "5m"
max_attempts = 2
quota_threshold = 80
quota_interval = "2m"
fallback_on = ["quota", "stalled"]
desktop_notifications = false
env_passthrough = ["OPENAI_API_KEY", "LITELLM_*"]
bogus = 1
[daemon.stall_after_harness]
codex = "30m"
[other]
x = 1
`), 0o644)
	c, warns, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tick != 2*time.Second || c.StallAfter != 15*time.Minute || c.UnansweredAfter != 5*time.Minute ||
		c.MaxAttempts != 2 || c.QuotaThreshold != 80 || strings.Join(c.FallbackOn, ",") != "quota,stalled" ||
		c.DesktopNotify || c.stallAfter("codex", false) != 30*time.Minute || c.stallAfter("claude", false) != 15*time.Minute ||
		c.stallAfter("claude", true) != time.Hour || c.stallAfter("codex", true) != time.Hour || !c.StallPaneActivity ||
		c.DeadGrace != 5*time.Second || c.QuotaInterval != 2*time.Minute || strings.Join(c.EnvPassthrough, ",") != "OPENAI_API_KEY,LITELLM_*" {
		t.Fatalf("config %+v", c)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "daemon.bogus") {
		t.Fatalf("warnings %v", warns)
	}
	for _, bad := range []string{`tick = "1ms"`, `stall_after = "soon"`, `max_attempts = 0`, `fallback_on = ["done"]`,
		`quota_threshold = 120`, `quota_interval = "10ms"`, `tool_stall_after = "0s"`, `launch_timeout = "1s"`, `env_passthrough = ["A-B"]`} {
		_ = os.WriteFile(path, []byte("[daemon]\n"+bad+"\n"), 0o644)
		if _, _, err := LoadConfig(path); apperr.Code(err) != apperr.ConfigInvalid {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// fakeLaunchctl is a launchctl that keeps its state in dir: "loaded", "linger" (print
// still sees a booted-out service this many more times), "busy" (bootstrap fails with
// EIO this many more times), "deny" (bootstrap or bootout fails for good).
func fakeLaunchctl(t *testing.T, dir string) (bin, calls string) {
	t.Helper()
	bin, calls = filepath.Join(dir, "launchctl"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
d='` + dir + `'
echo "$*" >> "$d/calls"
dec() { n=$(cat "$d/$1" 2>/dev/null || echo 0); [ "$n" -gt 0 ] || return 1; echo $((n - 1)) > "$d/$1"; }
case $1 in
print)
	[ -e "$d/loaded" ] && exit 0
	dec linger && exit 0
	echo "Could not find service" >&2; exit 113 ;;
bootout)
	if [ -e "$d/deny" ]; then echo "Boot-out failed: 1: Operation not permitted" >&2; exit 1; fi
	[ -e "$d/loaded" ] || { echo "Boot-out failed: 3: No such process" >&2; exit 3; }
	rm "$d/loaded" ;;
bootstrap)
	if dec busy; then echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; fi
	if [ -e "$d/deny" ]; then echo "Bootstrap failed: 1: Operation not permitted" >&2; exit 1; fi
	: > "$d/loaded" ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// Re-installing over a loaded agent waits until launchd let go of the old one and
// retries a bootstrap that fails with "5: Input/output error" meanwhile.
func TestLaunchdReinstallWaitsAndRetries(t *testing.T) {
	ctx := context.Background()
	dir, home := t.TempDir(), t.TempDir()
	bin, calls := fakeLaunchctl(t, dir)
	_ = os.WriteFile(filepath.Join(dir, "loaded"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "linger"), []byte("2"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "busy"), []byte("1"), 0o600)
	l := Launchd{Home: home, Launchctl: bin, UID: 501, Wait: 2 * time.Second}
	spec := PlistSpec{OtagBin: "/opt/otag/bin/otag", LogPath: filepath.Join(home, "daemon.log")}
	path, err := l.Install(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	svc := "gui/501/io.github.nesiler.otag"
	want := "bootout " + svc + "\nprint " + svc + "\nprint " + svc + "\nprint " + svc +
		"\nbootstrap gui/501 " + path + "\nbootstrap gui/501 " + path + "\n"
	if string(b) != want {
		t.Fatalf("calls:\n%s\nwant\n%s", b, want)
	}
	if st := l.Status(ctx); !st.Loaded {
		t.Fatal("not loaded")
	}
}

// A bootstrap that fails for good, or a real bootout error, is reported.
func TestLaunchdInstallFailures(t *testing.T) {
	ctx := context.Background()
	dir, home := t.TempDir(), t.TempDir()
	bin, calls := fakeLaunchctl(t, dir)
	l := Launchd{Home: home, Launchctl: bin, UID: 501, Wait: time.Second}
	spec := PlistSpec{OtagBin: "/opt/otag/bin/otag", LogPath: filepath.Join(home, "daemon.log")}

	_ = os.WriteFile(filepath.Join(dir, "busy"), []byte("9"), 0o600)
	_, err := l.Install(ctx, spec)
	if apperr.Code(err) != apperr.Unavailable || !strings.Contains(err.Error(), "5: Input/output error") {
		t.Fatalf("bootstrap kept failing: %v", err)
	}
	b, _ := os.ReadFile(calls)
	if n := strings.Count(string(b), "bootstrap"); n != 5 {
		t.Fatalf("%d bootstrap attempts", n)
	}

	_ = os.Remove(filepath.Join(dir, "busy"))
	_ = os.WriteFile(filepath.Join(dir, "deny"), nil, 0o600)
	if _, err := l.Install(ctx, spec); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("permanent bootstrap failure: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "loaded"), nil, 0o600)
	if _, err := l.Install(ctx, spec); err == nil || !strings.Contains(err.Error(), "Boot-out failed: 1") {
		t.Fatalf("bootout failure: %v", err)
	}
	if _, err := l.Uninstall(ctx); err == nil {
		t.Fatal("uninstall ignored a bootout failure")
	}
}

func TestPlistPATH(t *testing.T) {
	home := t.TempDir()
	fnm := filepath.Join(home, "fnm", "aliases", "default", "bin")
	_ = os.MkdirAll(fnm, 0o755)
	path, dropped := PlistPATH("/opt/otag/bin/otag",
		".:/usr/bin:"+home+"/.local/state/fnm_multishells/123_456/bin:bin:/var/folders/x/T/tool:/opt/homebrew/bin/:/usr/bin::/bin",
		[]string{"/var/folders"}, fnm)
	if want := "/opt/otag/bin:/usr/bin:" + fnm + ":/opt/homebrew/bin:/bin"; path != want {
		t.Fatalf("PATH %s\nwant %s", path, want)
	}
	if strings.Join(dropped, " ") != ". "+home+"/.local/state/fnm_multishells/123_456/bin bin /var/folders/x/T/tool" {
		t.Fatalf("dropped %v", dropped)
	}
	if path, _ := PlistPATH("/n/otag", "/a/fnm_multishells/1/bin:/b", nil, "/does/not/exist"); path != "/n:/b" {
		t.Fatalf("missing fnm default: %s", path)
	}
}

func TestRotateLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	_ = os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600)
	out, err := os.OpenFile(filepath.Join(dir, "fd"), os.O_WRONLY|os.O_CREATE, 0o600) // stands in for fd 2
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	if rotated, err := RotateLog(path, 1000, int(out.Fd())); rotated || err != nil {
		t.Fatalf("small log rotated: %v %v", rotated, err)
	}
	if rotated, err := RotateLog(path, 50, int(out.Fd())); !rotated || err != nil {
		t.Fatalf("rotate: %v %v", rotated, err)
	}
	if b, _ := os.ReadFile(path + ".1"); len(b) != 100 {
		t.Fatalf("old log %d bytes", len(b))
	}
	_, _ = out.WriteString("after\n") // the descriptor now writes to the new log
	if b, _ := os.ReadFile(path); string(b) != "after\n" {
		t.Fatalf("new log %q", b)
	}
}
