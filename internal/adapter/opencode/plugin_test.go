package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// driver loads the plugin with a fake v2 ctx (hooks recorded by name) and runs the
// hooks the way OpenCode does; it prints what happened as JSON.
const driver = `
import plugin from %q
const hooks = {}
const reg = (domain) => ({ hook: async (name, fn) => { hooks[domain + "." + name] = fn } })
const ctx = { options: JSON.parse(process.argv[2]), location: { directory: "/w" },
  permission: reg("permission"), tool: reg("tool"), session: reg("session") }
await plugin.setup(ctx)
const out = { id: plugin.id, hooks: Object.keys(hooks).sort() }
await hooks["tool.execute.before"]({ tool: "write", id: "call_w", sessionID: "ses_a", input: {} })
await hooks["tool.execute.before"]({ tool: "shell", id: "call_s", sessionID: "ses_a", input: { command: "x", workdir: "sub" } })
const ev = async (e) => { await hooks["permission.evaluate"](e); return { effect: e.effect, message: e.message } }
out.push = await ev({ sessionID: "ses_a", action: "shell", resources: ["git push"], effect: "allow", source: { id: "call_s" } })
out.ls = await ev({ sessionID: "ses_a", action: "shell", resources: ["ls"], effect: "allow" })
out.read = await ev({ sessionID: "ses_a", action: "read", resources: ["x"], effect: "ask" })
out.external = await ev({ sessionID: "ses_a", action: "external_directory", resources: ["/tmp/*"], effect: "allow", source: { id: "call_w" } })
out.denied = await ev({ sessionID: "ses_a", action: "shell", resources: ["git push"], effect: "deny" })
if (hooks["session.context"]) {
  const c = { system: [] }
  await hooks["session.context"](c)
  out.system = c.system
}
console.log(JSON.stringify(out))
`

// stubOtag answers like `otag event opencode permission.evaluate`: deny for git push
// and for edit tools outside the workspace, {} (no opinion) otherwise. It logs every call.
const stubOtag = `#!/bin/sh
payload=$(cat)
printf '%s %s\n' "$*" "$payload" >>"$STUB_LOG"
case $payload in
*'git push'*) printf '{"effect":"deny","message":"otag guard: no push"}' ;;
*'"tool":"write"'*) printf '{"effect":"deny","message":"otag guard: outside"}' ;;
*) printf '{}' ;;
esac
`

func runPlugin(t *testing.T, opts map[string]string) (map[string]json.RawMessage, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	plug := filepath.Join(dir, "plugin", "otag")
	_ = os.MkdirAll(plug, 0o700)
	_ = os.WriteFile(filepath.Join(plug, "index.js"), pluginJS, 0o600)
	_ = os.WriteFile(filepath.Join(plug, "package.json"), pluginPkg, 0o600)
	drv := filepath.Join(dir, "driver.mjs")
	_ = os.WriteFile(drv, []byte(strings.Replace(driver, "%q", strconvQuote("file://"+filepath.Join(plug, "index.js")), 1)), 0o600)
	logf := filepath.Join(dir, "calls.log")
	b, _ := json.Marshal(opts)
	cmd := exec.Command(node, drv, string(b))
	cmd.Env = append(os.Environ(), "STUB_LOG="+logf)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var res map[string]json.RawMessage
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("driver output %q: %v", out, err)
	}
	calls, _ := os.ReadFile(logf)
	return res, string(calls)
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestPluginGuardAndNote(t *testing.T) {
	dir := t.TempDir()
	otag := filepath.Join(dir, "otag")
	_ = os.WriteFile(otag, []byte(stubOtag), 0o700)
	note := filepath.Join(dir, "instructions.md")
	_ = os.WriteFile(note, []byte("You are a otag child agent: run r1.\n"), 0o600)
	res, calls := runPlugin(t, map[string]string{"otag": otag, "run": "r1", "instructions": note})

	effect := func(k string) (e struct{ Effect, Message string }) {
		_ = json.Unmarshal(res[k], &e)
		return e
	}
	if string(res["id"]) != `"otag"` {
		t.Errorf("id %s", res["id"])
	}
	if e := effect("push"); e.Effect != "deny" || e.Message != "otag guard: no push" {
		t.Errorf("git push: %+v", e)
	}
	if e := effect("ls"); e.Effect != "allow" || e.Message != "" {
		t.Errorf("ls: %+v", e)
	}
	if e := effect("read"); e.Effect != "ask" {
		t.Errorf("a read otag does not refuse keeps its effect: %+v", e)
	}
	if e := effect("external"); e.Effect != "deny" {
		t.Errorf("external_directory from the write tool: %+v", e)
	}
	if e := effect("denied"); e.Effect != "deny" {
		t.Errorf("already denied: %+v", e)
	}
	var sys []struct{ Type, Text string }
	_ = json.Unmarshal(res["system"], &sys)
	if len(sys) != 1 || sys[0].Type != "text" || sys[0].Text != "You are a otag child agent: run r1." {
		t.Errorf("system note %s", res["system"])
	}
	// otag is asked only for guarded actions not already denied (reads included); the
	// denied request never spawns it.
	lines := strings.Split(strings.TrimSpace(calls), "\n")
	if len(lines) != 4 {
		t.Fatalf("otag calls:\n%s", calls)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "event opencode permission.evaluate ") {
			t.Errorf("call %q", l)
		}
	}
	if !strings.Contains(lines[0], `"tool":"shell"`) || !strings.Contains(lines[0], `"workdir":"sub"`) {
		t.Errorf("shell call lacks the tool's workdir: %s", lines[0])
	}
	if !strings.Contains(lines[3], `"tool":"write"`) || !strings.Contains(lines[3], `"callID":"call_w"`) {
		t.Errorf("external_directory call lacks the tool: %s", lines[3])
	}
}

// A missing or broken otag denies: the guard fails closed (config denies back it up too).
func TestPluginFailsClosed(t *testing.T) {
	res, _ := runPlugin(t, map[string]string{"otag": "/nonexistent/otag", "run": "r1", "instructions": "/nonexistent"})
	var e struct{ Effect string }
	_ = json.Unmarshal(res["push"], &e)
	if e.Effect != "deny" {
		t.Errorf("git push with a broken otag: %s", res["push"])
	}
	if _, ok := res["system"]; ok {
		t.Errorf("no instructions file: no context hook")
	}
	// A otag that fails, is killed or answers nothing denies even an ordinary command.
	for name, body := range map[string]string{
		"exit 1": "#!/bin/sh\ncat >/dev/null\nexit 1\n",
		"signal": "#!/bin/sh\ncat >/dev/null\nkill -TERM $$\n",
		"silent": "#!/bin/sh\ncat >/dev/null\n",
		"error":  "#!/bin/sh\ncat >/dev/null\nprintf '{}'\nexit 2\n",
	} {
		otag := filepath.Join(t.TempDir(), "otag")
		_ = os.WriteFile(otag, []byte(body), 0o700)
		res, _ := runPlugin(t, map[string]string{"otag": otag, "run": "r1", "instructions": "/nonexistent"})
		var e struct{ Effect string }
		_ = json.Unmarshal(res["ls"], &e)
		if e.Effect != "deny" {
			t.Errorf("%s: ls %s", name, res["ls"])
		}
	}
}
