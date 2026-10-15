package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// fakeServer is a minimal OpenCode v2 API for SetupServer, Stop and Deliver.
type fakeServer struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any
	config string // JSON for /api/config
	plugin []string
}

func newFakeServer(t *testing.T, spec adapter.RunSpec) (*fakeServer, *httptest.Server) {
	f := &fakeServer{t: t, bodies: map[string]map[string]any{},
		plugin: []string{"", "loading", "active"},
		config: fmt.Sprintf(`[{"type":"document","path":"/acc/opencode.json","info":{"plugins":["file:///elsewhere"]}},`+
			`{"type":"document","info":{"plugins":[{"package":"file://%s","options":{}}],"permissions":[]}}]`,
			filepath.Join(spec.RunDir, pluginDir))}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	user, pw, ok := r.BasicAuth()
	if !ok || user != "opencode" || pw != "s3cret" {
		w.WriteHeader(401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key+"?"+r.URL.RawQuery)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			f.bodies[key] = m
		}
	}
	switch key {
	case "GET /api/info":
		_, _ = io.WriteString(w, `{"version":"2.0.20"}`)
	case "GET /api/config":
		_, _ = io.WriteString(w, f.config)
	case "POST /api/session":
		_, _ = io.WriteString(w, `{"data":{"id":"ses_ed90b908effeKEodEwFdduzqcz","model":{"providerID":"litellm","id":"deepseek-flash-deepseek"},"location":{"directory":"/w/al-1"}}}`)
	case "GET /api/session/ses_old":
		_, _ = io.WriteString(w, `{"data":{"id":"ses_old","model":{"providerID":"opencode","id":"other"},"location":{"directory":"/w/al-1"}}}`)
	case "GET /api/session/ses_gone":
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	case "GET /api/session/ses_elsewhere":
		_, _ = io.WriteString(w, `{"data":{"id":"ses_elsewhere","location":{"directory":"/other/repo"}}}`)
	case "POST /api/session/ses_old/model", "POST /api/session/ses_a/interrupt", "PATCH /api/session/ses_old":
		_, _ = io.WriteString(w, `{"interrupted":true}`)
	case "POST /api/session/ses_a/prompt":
		_, _ = io.WriteString(w, `{"data":{"id":"msg_1"}}`)
	case "GET /api/plugin":
		state := f.plugin[0]
		if len(f.plugin) > 1 {
			f.plugin = f.plugin[1:]
		}
		list := `{"id":"opencode.browser","source":{"type":"builtin"},"state":{"status":"active"}}`
		if state != "" {
			list += fmt.Sprintf(`,{"id":"otag","source":{"type":"local"},"state":{"status":%q,"error":"boom"}}`, state)
		}
		_, _ = io.WriteString(w, `{"location":{"directory":"/w/al-1"},"data":[`+list+`]}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeServer) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func setupLaunch(t *testing.T, a *Adapter, spec adapter.RunSpec, url string) *adapter.LaunchSpec {
	t.Helper()
	ls, err := a.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	ls.Server.URL = RecordedURL(url, spec.RunDir) // the fake listens elsewhere
	return ls
}

func TestSetupServerCreatesSession(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	f, srv := newFakeServer(t, spec)
	ls := setupLaunch(t, a, spec, srv.URL)
	if err := a.SetupServer(context.Background(), spec, ls); err != nil {
		t.Fatal(err)
	}
	sid := "ses_ed90b908effeKEodEwFdduzqcz"
	if ls.HarnessSessionID != sid {
		t.Errorf("session %q", ls.HarnessSessionID)
	}
	if i := slices.Index(ls.Argv, "-s"); i < 0 || ls.Argv[i+1] != sid || ls.Argv[3] != "--server" || ls.Argv[4] != srv.URL {
		t.Errorf("argv %q", ls.Argv)
	}
	if args := strings.Join(ls.Sidecars[0].Argv, " "); !strings.Contains(args, "--session "+sid) ||
		!strings.Contains(args, "--url "+srv.URL) {
		t.Errorf("sidecar %s", args)
	}
	body := f.bodies["POST /api/session"]
	if _, ok := body["id"]; ok {
		t.Errorf("otag must not invent session ids: %v", body)
	}
	if m := body["model"].(map[string]any); m["providerID"] != "litellm" || m["id"] != "deepseek-flash-deepseek" {
		t.Errorf("model %v", body["model"])
	}
	if loc := body["location"].(map[string]any); loc["directory"] != "/w/al-1" {
		t.Errorf("location %v", body["location"])
	}
	if md := body["metadata"].(map[string]any); md["otag_run"] != "al-1-r1" || md["otag_task"] != "al-1" {
		t.Errorf("metadata %v", body["metadata"])
	}
	var rules []Rule
	p, _ := json.Marshal(body["permissions"])
	if _ = json.Unmarshal(p, &rules); !slices.Contains(rules, Rule{"shell", "git push *", "deny"}) {
		t.Errorf("permissions %s", p)
	}
	if n := f.called("GET /api/plugin?location%5Bdirectory%5D=%2Fw%2Fal-1"); n != 3 {
		t.Errorf("plugin polled %d times (lazy load: until active)", n)
	}
}

func TestSetupServerResume(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	spec.ResumeSessionID = "ses_old"
	f, srv := newFakeServer(t, spec)
	ls := setupLaunch(t, a, spec, srv.URL)
	if err := a.SetupServer(context.Background(), spec, ls); err != nil {
		t.Fatal(err)
	}
	if ls.HarnessSessionID != "ses_old" || f.called("POST /api/session?") != 0 {
		t.Errorf("resume created a session: %v", f.calls)
	}
	if m := f.bodies["POST /api/session/ses_old/model"]; m == nil {
		t.Errorf("model not switched to the profile's: %v", f.calls)
	}

	// Today's deny rules are applied to the resumed session.
	var rules []Rule
	p, _ := json.Marshal(f.bodies["PATCH /api/session/ses_old"]["permissions"])
	if _ = json.Unmarshal(p, &rules); !slices.Contains(rules, Rule{"edit", "*/.env*", "deny"}) {
		t.Errorf("resume rules %s", p)
	}

	spec.ResumeSessionID = "ses_gone"
	ls = setupLaunch(t, a, spec, srv.URL)
	if err := a.SetupServer(context.Background(), spec, ls); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing session: %v", err)
	}
	spec.ResumeSessionID = "ses_elsewhere"
	ls = setupLaunch(t, a, spec, srv.URL)
	if err := a.SetupServer(context.Background(), spec, ls); err == nil || !strings.Contains(err.Error(), "/other/repo") {
		t.Errorf("session of another workspace: %v", err)
	}
}

func TestSetupServerChecks(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	a.SetupTimeout = time.Second

	f, srv := newFakeServer(t, spec)
	f.config = `[{"type":"document","info":{"plugins":[]}}]`
	if err := a.SetupServer(context.Background(), spec, setupLaunch(t, a, spec, srv.URL)); err == nil ||
		!strings.Contains(err.Error(), "OPENCODE_CONFIG_CONTENT") {
		t.Errorf("config not loaded: %v", err)
	}

	f, srv = newFakeServer(t, spec)
	f.plugin = []string{"failed"}
	if err := a.SetupServer(context.Background(), spec, setupLaunch(t, a, spec, srv.URL)); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("failed plugin: %v", err)
	}

	f, srv = newFakeServer(t, spec)
	f.plugin = []string{""}
	if err := a.SetupServer(context.Background(), spec, setupLaunch(t, a, spec, srv.URL)); err == nil ||
		!strings.Contains(err.Error(), "not active") {
		t.Errorf("missing plugin: %v", err)
	}

	ls := setupLaunch(t, a, spec, srv.URL)
	ls.Env["OPENCODE_PASSWORD"] = "wrong"
	if err := a.SetupServer(context.Background(), spec, ls); err == nil {
		t.Error("wrong password accepted")
	}
}

type fakePane struct {
	mu   sync.Mutex
	keys []string
	text []string
}

func (p *fakePane) SendKeys(_ context.Context, keys ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, keys...)
	return nil
}
func (p *fakePane) SendText(_ context.Context, text string) error {
	p.text = append(p.text, text)
	return nil
}
func (p *fakePane) Capture(context.Context) (string, error) { return "", nil }

func targetEnv(t *testing.T, runID string) func(string) string {
	data := t.TempDir()
	dir := filepath.Join(data, "runs", runID, "opencode")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte("s3cret"), 0o600)
	return func(k string) string {
		if k == "OTAG_DATA_DIR" {
			return data
		}
		return ""
	}
}

func TestStopAndDeliver(t *testing.T) {
	a, spec := testAdapter(), testSpec(t)
	a.Getenv = targetEnv(t, "al-1-r1")
	f, srv := newFakeServer(t, spec)
	pane := &fakePane{}
	tg := adapter.Target{RunID: "al-1-r1", HarnessSessionID: "ses_a", ServerURL: srv.URL, Pane: pane}
	if err := a.Stop(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if f.called("POST /api/session/ses_a/interrupt") != 1 || !slices.Equal(pane.keys, []string{"C-c", "C-c"}) {
		t.Errorf("stop: calls %v keys %v", f.calls, pane.keys)
	}

	// Without a reachable server Stop still quits the TUI.
	pane = &fakePane{}
	if err := a.Stop(context.Background(), adapter.Target{RunID: "x", Pane: pane}); err != nil || len(pane.keys) != 2 {
		t.Errorf("stop without server: %v %v", err, pane.keys)
	}

	if err := a.Deliver(context.Background(), tg, adapter.Message{ID: "msg_abc", Text: "next step"}); err != nil {
		t.Fatal(err)
	}
	if b := f.bodies["POST /api/session/ses_a/prompt"]; b["text"] != "next step" || b["delivery"] != "queue" || b["id"] != "msg_abc" {
		t.Errorf("deliver body %v", b)
	}
	if err := a.Deliver(context.Background(), tg, adapter.Message{ID: "m-1", Text: "change course", Steer: true}); err != nil {
		t.Fatal(err)
	}
	if b := f.bodies["POST /api/session/ses_a/prompt"]; b["delivery"] != "steer" || b["id"] != nil {
		t.Errorf("deliver steer body %v", b)
	}
	if err := a.Deliver(context.Background(), adapter.Target{RunID: "nope", HarnessSessionID: "ses_a", ServerURL: srv.URL},
		adapter.Message{Text: "x"}); err == nil {
		t.Error("deliver without a password file must fail")
	}

	// The run dir recorded in the server URL wins over the caller's environment.
	a.Getenv = func(string) string { return "" }
	runDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(runDir, "opencode"), 0o700)
	_ = os.WriteFile(filepath.Join(runDir, "opencode", "password"), []byte("s3cret\n"), 0o600)
	tg = adapter.Target{RunID: "al-1-r1", HarnessSessionID: "ses_a", ServerURL: RecordedURL(srv.URL, runDir)}
	if err := a.Deliver(context.Background(), tg, adapter.Message{Text: "via recorded dir"}); err != nil {
		t.Fatalf("deliver with the recorded run dir: %v", err)
	}
	if b := f.bodies["POST /api/session/ses_a/prompt"]; b["text"] != "via recorded dir" {
		t.Errorf("deliver body %v", b)
	}
}
