package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/daemon"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/notify"
)

// install/uninstall only ever touch the temp HOME and a fake launchctl.
func TestDaemonInstallCLI(t *testing.T) {
	h := newHarness(t)
	calls := filepath.Join(h.root, "launchctl.log")
	fake := filepath.Join(h.root, "launchctl")
	_ = os.WriteFile(fake, []byte("#!/bin/sh\necho \"$*\" >> '"+calls+"'\n[ \"$1\" != print ]\n"), 0o700)
	h.env["OTAG_LAUNCHCTL"] = fake
	h.env["OTAG_BIN"] = "/opt/otag/bin/otag"
	h.env["PATH"] = "/usr/bin:/bin"

	code, e := h.json("daemon", "install")
	if code != 0 {
		t.Fatalf("install: %+v", e.Error)
	}
	var res struct {
		Plist string   `json:"plist"`
		Env   []string `json:"env"`
	}
	_ = json.Unmarshal(e.Data, &res)
	if want := filepath.Join(h.root, "Library", "LaunchAgents", "io.github.nesiler.otag.plist"); res.Plist != want {
		t.Fatalf("plist %s", res.Plist)
	}
	b, err := os.ReadFile(res.Plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>/opt/otag/bin/otag</string>", "<string>/opt/otag/bin:/usr/bin:/bin</string>",
		"<key>OTAG_DB</key>\n\t\t<string>" + h.env["OTAG_DB"], filepath.Join(h.root, "daemon.log")} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("plist lacks %q:\n%s", want, b)
		}
	}
	_, e = h.json("daemon", "status")
	var st struct {
		Running bool `json:"running"`
		Agent   struct {
			Installed, Loaded bool
		} `json:"launch_agent"`
	}
	_ = json.Unmarshal(e.Data, &st)
	if st.Running || !st.Agent.Installed || st.Agent.Loaded {
		t.Fatalf("status %s", e.Data)
	}
	h.ok("daemon", "uninstall")
	if _, err := os.Stat(res.Plist); !os.IsNotExist(err) {
		t.Fatal("plist not removed")
	}
	log, _ := os.ReadFile(calls)
	if !strings.Contains(string(log), "bootstrap gui/") || !strings.Contains(string(log), "bootout gui/") {
		t.Fatalf("launchctl calls %s", log)
	}
}

func TestDaemonOnceAndStatus(t *testing.T) {
	h := newHarness(t)
	h.env["OTAG_TMUX_SOCKET"] = "otag-test-unused-daemon" // never the user's otag server
	h.write("otag.toml", "[daemon]\ndesktop_notifications = false\n")
	h.ok("daemon", "--once")

	d, err := db.Open(context.Background(), h.env["OTAG_DB"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	for _, title := range []string{"one", "two"} {
		if _, err := notify.Add(context.Background(), d, notify.New{Kind: "test", Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	_, e := h.json("status")
	var st struct {
		Daemon struct {
			Running bool `json:"running"`
			PID     int  `json:"pid"`
		} `json:"daemon"`
		Unread int `json:"unread_notifications"`
	}
	_ = json.Unmarshal(e.Data, &st)
	if st.Daemon.Running || st.Unread != 2 {
		t.Fatalf("status %s", e.Data)
	}

	lock, err := daemon.Acquire(h.root) // the data dir is the directory of OTAG_DB
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errs := h.run("daemon", "--once"); code == 0 || !strings.Contains(errs, "already running") {
		t.Fatalf("second daemon: %d %s", code, errs)
	}
	_, e = h.json("status")
	_ = json.Unmarshal(e.Data, &st)
	if !st.Daemon.Running || st.Daemon.PID != os.Getpid() {
		t.Fatalf("status with lock %s", e.Data)
	}
	if out := h.ok("status"); !strings.Contains(out, "daemon: running (pid") || !strings.Contains(out, "2 unread") {
		t.Fatalf("status output:\n%s", out)
	}
	_ = lock.Release()

	// notify list / ack
	_, e = h.json("notify", "list")
	var ns []notify.Notification
	_ = json.Unmarshal(e.Data, &ns)
	if len(ns) != 2 || ns[0].Title != "two" {
		t.Fatalf("list %s", e.Data)
	}
	if code, _, _ := h.run("notify", "ack"); code != ExitUsage {
		t.Fatalf("ack without ids: %d", code)
	}
	h.ok("notify", "ack", "1")
	_, e = h.json("notify", "list")
	_ = json.Unmarshal(e.Data, &ns)
	if len(ns) != 1 || ns[0].Title != "two" {
		t.Fatalf("after ack %s", e.Data)
	}
	h.ok("notify", "ack", "--all")
	_, e = h.json("notify", "list", "--all")
	_ = json.Unmarshal(e.Data, &ns)
	if len(ns) != 2 || ns[0].ReadAt == "" || ns[1].ReadAt == "" {
		t.Fatalf("list --all %s", e.Data)
	}
}

// The launch agent gets a curated environment: locale and user basics, a stable
// PATH, the env_passthrough variables; doctor reports an OpenCode {env:VAR} it lacks.
func TestDaemonInstallEnv(t *testing.T) {
	h := newHarness(t)
	fake := filepath.Join(h.root, "launchctl")
	_ = os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$1\" != print ]\n"), 0o700)
	h.env["OTAG_LAUNCHCTL"] = fake
	h.env["OTAG_BIN"] = "/opt/otag/bin/otag"
	h.env["LANG"] = "tr_TR.UTF-8"
	h.env["SSH_AUTH_SOCK"] = "/private/tmp/com.apple.launchd.abc/Listeners" // changes every boot
	h.env["TMPDIR"] = "/var/folders/zz/T/"
	h.env["LITELLM_KEY"] = "sk-test"
	h.env["PATH"] = ".:/usr/bin:" + h.root + "/.local/state/fnm_multishells/1_2/bin:/var/folders/zz/T/x:/bin"
	oc := filepath.Join(h.root, "oc")
	_ = os.MkdirAll(filepath.Join(oc, "config", "opencode"), 0o755)
	_ = os.WriteFile(filepath.Join(oc, "config", "opencode", "opencode.json"),
		[]byte(`{"provider":{"litellm":{"options":{"baseURL":"http://x","apiKey":"{env:LITELLM_KEY}"}}}}`), 0o600)
	h.write("registry.toml", `
[accounts.c1]
harness = "claude"
[accounts.oc]
harness = "opencode"
config_dir = "`+oc+`"
[profiles.sonnet]
account = "c1"
model = "sonnet"
`)
	h.ok("sync")
	h.ok("daemon", "install")
	plist := filepath.Join(h.root, "Library", "LaunchAgents", "io.github.nesiler.otag.plist")
	env, err := daemon.ReadPlistEnv(plist)
	if err != nil {
		t.Fatal(err)
	}
	if env["LANG"] != "tr_TR.UTF-8" || env["HOME"] != h.root || env["USER"] != "tester" || env["TMPDIR"] != "/var/folders/zz/T/" ||
		env["PATH"] != "/opt/otag/bin:/usr/bin:/bin" {
		t.Fatalf("env %v", env)
	}
	if _, ok := env["SSH_AUTH_SOCK"]; ok {
		t.Fatal("pinned launchd's per-boot SSH agent socket")
	}
	if _, ok := env["LITELLM_KEY"]; ok {
		t.Fatal("passed a variable that is not in env_passthrough")
	}
	_, out, _ := h.run("doctor")
	if !strings.Contains(out, "{env:LITELLM_KEY}") || !strings.Contains(out, "env_passthrough") {
		t.Fatalf("doctor did not report the missing variable:\n%s", out)
	}

	h.write("otag.toml", "[daemon]\nenv_passthrough = [\"LITELLM_*\", \"P*\", \"PATH\", \"OTAG_DB\"]\n")
	h.env["SSH_AUTH_SOCK"] = "/opt/agent/agent.sock" // a stable custom agent
	h.ok("daemon", "install")
	env, _ = daemon.ReadPlistEnv(plist)
	if env["LITELLM_KEY"] != "sk-test" || env["PATH"] != "/opt/otag/bin:/usr/bin:/bin" || env["OTAG_DB"] != h.env["OTAG_DB"] || env["SSH_AUTH_SOCK"] != "/opt/agent/agent.sock" {
		t.Fatalf("env %v", env)
	}
	if _, out, _ := h.run("doctor"); strings.Contains(out, "{env:LITELLM_KEY}") {
		t.Fatalf("doctor still reports the variable:\n%s", out)
	}
	if st, _ := os.Stat(plist); st.Mode().Perm() != 0o600 {
		t.Fatalf("plist with secrets has mode %v", st.Mode().Perm())
	}
}

// As the launch agent (--service), permanent startup problems exit 0 so launchd does
// not restart the daemon every 10 seconds; in the foreground they are errors.
func TestDaemonServiceExitsCleanlyOnPermanentErrors(t *testing.T) {
	h := newHarness(t)
	h.env["OTAG_TMUX_SOCKET"] = "otag-test-unused-daemon"
	h.write("otag.toml", "[daemon]\ntick = \"1ms\"\n")
	if code, _, _ := h.run("daemon", "--once"); code == 0 {
		t.Fatal("foreground daemon accepted an invalid config")
	}
	code, _, errs := h.run("daemon", "--service")
	if code != 0 || !strings.Contains(errs, "not starting") || !strings.Contains(errs, "tick must be") {
		t.Fatalf("service with invalid config: %d %s", code, errs)
	}
	h.write("otag.toml", "")
	lock, err := daemon.Acquire(h.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	code, _, errs = h.run("daemon", "--service")
	if code != 0 || !strings.Contains(errs, "already running") {
		t.Fatalf("service with the lock held: %d %s", code, errs)
	}
}
