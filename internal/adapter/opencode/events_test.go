package opencode

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

// recorded returns the frames of testdata/sse-recorded.jsonl: a real GET /api/event
// capture (OpenCode 2.0.20, opencode/mimo-v2.6-flash-free) of a turn with a
// permission ask (answered once) and a question form, a plain turn and a turn
// interrupted over the API, plus catalog noise. Paths are anonymized.
func recorded(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "sse-recorded.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	return out
}

func frameType(b []byte) (id, typ string) {
	var e struct{ ID, Type string }
	_ = json.Unmarshal(b, &e)
	return e.ID, e.Type
}

func types(evs []adapter.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestParseRecorded(t *testing.T) {
	a := New()
	hc := adapter.HookContext{RunID: "r1", Status: "running", Workdir: "/work/repo"}
	var seq []string
	byType := map[string][]adapter.HookResult{}
	for _, raw := range recorded(t) {
		id, typ := frameType(raw)
		res, err := a.ParseEvent(hc, typ, raw)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if res.EventID != id {
			t.Errorf("%s: event id %q, want the frame id %q", typ, res.EventID, id)
		}
		if res.Output != nil {
			t.Errorf("%s: SSE kinds print nothing, got %s", typ, res.Output)
		}
		byType[typ] = append(byType[typ], res)
		for _, e := range res.Events {
			seq = append(seq, e.Type)
		}
		if strings.HasSuffix(typ, ".delta") || strings.HasSuffix(typ, ".updated") || strings.HasPrefix(typ, "session.inbox.") ||
			typ == "session.step.streamed" || typ == "server.connected" || typ == "session.text.ended" {
			if len(res.Events) > 0 {
				t.Errorf("noise %s produced %v", typ, types(res.Events))
			}
		}
	}

	// Turn 1: permission + form, both answered; turn 2 plain; turn 3 interrupted.
	if n := strings.Count(strings.Join(seq, " "), adapter.TurnStart); n != 3 {
		t.Errorf("turn_start x%d: %v", n, seq)
	}
	perm := byType["permission.asked"][0]
	if len(perm.Events) != 1 || perm.Events[0].Type != adapter.Permission || perm.Events[0].Tool != "shell" ||
		perm.Events[0].Text != "shell: echo hi" || perm.SessionRef != "ses_ed9098cf6ffeZZEBGpxYIurGms" {
		t.Errorf("permission.asked = %+v", perm)
	}
	if e := byType["permission.replied"][0].Events; len(e) != 1 || e[0].Type != adapter.Activity {
		t.Errorf("permission.replied = %+v", e)
	}
	form := byType["form.created"][0]
	if len(form.Events) != 1 || form.Events[0].Type != adapter.Question || form.SessionRef == "" ||
		!strings.Contains(form.Events[0].Text, "Color preference") || !strings.Contains(form.Events[0].Text, "[Red | Blue]") {
		t.Errorf("form.created = %+v", form)
	}
	if e := byType["form.replied"][0].Events; len(e) != 1 || e[0].Type != adapter.Activity {
		t.Errorf("form.replied = %+v", e)
	}
	ends := byType["session.execution.succeeded"]
	if len(ends) != 2 || ends[0].Events[0].Type != adapter.TurnEnd || ends[0].Events[0].Reason != "answer" {
		t.Errorf("succeeded = %+v", ends)
	}
	intr := byType["session.execution.interrupted"][0].Events
	if len(intr) != 1 || intr[0].Type != adapter.TurnEnd || intr[0].Reason != "aborted" || intr[0].Data["interrupt"] != "user" {
		t.Errorf("interrupted = %+v", intr)
	}
	tool := byType["session.tool.input.started"][0].Events
	if len(tool) != 1 || tool[0].Type != adapter.Activity || tool[0].Tool == "" || tool[0].ToolPhase != adapter.ToolStart {
		t.Errorf("tool.input.started = %+v", tool)
	}
	for _, k := range []string{"session.tool.success", "session.tool.failed"} {
		for _, r := range byType[k] {
			if len(r.Events) != 1 || r.Events[0].ToolPhase != adapter.ToolEnd {
				t.Errorf("%s = %+v", k, r.Events)
			}
		}
	}
	var keys []string
	for _, r := range byType["session.step.ended"] {
		if len(r.Events) != 1 || r.Events[0].Usage == nil {
			t.Fatalf("step.ended = %+v", r)
		}
		u := r.Events[0].Usage
		if u.Source != "sse" || !strings.HasPrefix(u.DedupeKey, "msg_") || u.InputTokens+u.CacheReadTokens == 0 ||
			u.CostUSD != nil {
			t.Errorf("usage %+v", u)
		}
		keys = append(keys, u.DedupeKey)
	}
	if len(keys) < 3 || keys[0] == keys[1] {
		t.Errorf("usage keys %v", keys)
	}
}

// The sidecar adds "otag" with what earlier frames said.
func TestParseEnriched(t *testing.T) {
	a := New()
	res, err := a.ParseEvent(adapter.HookContext{}, "session.execution.succeeded",
		[]byte(`{"id":"evt_1","type":"session.execution.succeeded","data":{"sessionID":"ses_a"},"otag":{"text":"All done."}}`))
	if err != nil || len(res.Events) != 1 || res.Events[0].Text != "All done." || res.SessionRef != "ses_a" {
		t.Errorf("succeeded: %+v %v", res, err)
	}
	res, _ = a.ParseEvent(adapter.HookContext{}, "session.step.ended", []byte(`{"id":"evt_2","type":"session.step.ended",
		"data":{"sessionID":"ses_a","assistantMessageID":"msg_9","finish":"stop","cost":0.0123,
		"tokens":{"input":10,"output":5,"reasoning":2,"cache":{"read":100,"write":7}}},"otag":{"model":"litellm/x"}}`))
	u := res.Events[0].Usage
	if u.Model != "litellm/x" || u.InputTokens != 10 || u.OutputTokens != 5 || u.ReasoningTokens != 2 ||
		u.CacheReadTokens != 100 || u.CacheWriteTokens != 7 || u.CostUSD == nil || *u.CostUSD != 0.0123 || u.DedupeKey != "msg_9" {
		t.Errorf("usage %+v", u)
	}
	// Compactions have no message id: keyed on the frame, model from the event.
	res, _ = a.ParseEvent(adapter.HookContext{}, "session.compaction.ended", []byte(`{"id":"evt_c","type":"session.compaction.ended",
		"data":{"sessionID":"ses_a","reason":"auto","model":{"providerID":"litellm","id":"glm"},"text":"sum","recent":"",
		"cost":0,"tokens":{"input":900,"output":80,"reasoning":0,"cache":{"read":0,"write":0}}},"otag":{"model":"litellm/x"}}`))
	if len(res.Events) != 1 || res.Events[0].Usage.DedupeKey != "compaction:evt_c" || res.Events[0].Usage.Model != "litellm/glm" ||
		res.Events[0].Usage.InputTokens != 900 {
		t.Errorf("compaction usage %+v", res.Events)
	}
	// A step without tokens reports nothing.
	res, _ = a.ParseEvent(adapter.HookContext{}, "session.step.failed", []byte(`{"id":"evt_3","type":"session.step.failed",
		"data":{"sessionID":"ses_a","assistantMessageID":"msg_9","error":{"type":"provider.transport","message":"x"}}}`))
	if len(res.Events) != 0 {
		t.Errorf("step.failed without tokens: %+v", res.Events)
	}
}

func TestParseErrors(t *testing.T) {
	a := New()
	parse := func(kind, raw string) adapter.HookResult {
		t.Helper()
		res, err := a.ParseEvent(adapter.HookContext{}, kind, []byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		return res
	}
	// Recorded: a session whose model does not exist fails fast.
	res := parse("session.execution.failed", `{"id":"evt_126ee53e700197j15rBjhtWT2U","created":1791654515687,"type":"session.execution.failed","data":{"sessionID":"ses_ed911bdaeffeUjp2Ri1P501HmL","error":{"type":"provider.no-route","message":"Model unavailable: opencode/no-such-model-xyz"}},"durable":{"aggregateID":"ses_ed911bdaeffeUjp2Ri1P501HmL","seq":5,"version":1}}`)
	if got := types(res.Events); strings.Join(got, ",") != "error,turn_end" || res.Events[1].Reason != "error" ||
		res.Events[0].Text != "provider.no-route: Model unavailable: opencode/no-such-model-xyz" {
		t.Errorf("failed = %+v", res.Events)
	}
	// Spike (LiteLLM unreachable): transport errors are retried and reported as errors.
	res = parse("session.retry.scheduled", `{"id":"evt_r6","type":"session.retry.scheduled","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","attempt":6,"at":1791223600000,"error":{"type":"provider.transport","message":"ConnectionRefused: Unable to connect."}}}`)
	if len(res.Events) != 1 || res.Events[0].Type != adapter.Error || res.Events[0].Data["attempt"] != 6 {
		t.Errorf("retry transport = %+v", res.Events)
	}
	// Schema (provider-error.ts): limits and quotas are rate_limit, with the retry time.
	res = parse("session.retry.scheduled", `{"id":"evt_r1","type":"session.retry.scheduled","data":{"sessionID":"ses_a","assistantMessageID":"msg_1","attempt":1,"at":1791223600000,"error":{"type":"provider.rate-limit","message":"Rate limit exceeded","status":429}}}`)
	if len(res.Events) != 1 || res.Events[0].Type != adapter.RateLimit || !res.Events[0].ResetsAt.Equal(time.UnixMilli(1791223600000)) {
		t.Errorf("retry rate limit = %+v", res.Events)
	}
	res = parse("session.execution.failed", `{"id":"evt_f","type":"session.execution.failed","data":{"sessionID":"ses_a","error":{"type":"provider.quota","message":"Insufficient quota","status":402}}}`)
	if strings.Join(types(res.Events), ",") != "rate_limit,turn_end" {
		t.Errorf("quota = %+v", res.Events)
	}
	res = parse("session.deleted", `{"id":"evt_d","type":"session.deleted","data":{"sessionID":"ses_a"}}`)
	if len(res.Events) != 1 || res.Events[0].Type != adapter.SessionEnd || res.Events[0].HarnessSessionID != "ses_a" {
		t.Errorf("deleted = %+v", res.Events)
	}

	// Broken JSON is an error (the bridge stores the raw payload); empty payloads are not.
	if _, err := a.ParseEvent(adapter.HookContext{}, "session.execution.started", []byte(`{"id":`)); err == nil {
		t.Error("broken JSON accepted")
	}
	if _, err := a.ParseEvent(adapter.HookContext{}, KindEvaluate, []byte(`nope`)); err == nil {
		t.Error("broken evaluate accepted")
	}
	for _, k := range []string{"session.execution.started", "unknown.kind", KindTUIExit, KindReconcile, KindEvaluate} {
		if _, err := a.ParseEvent(adapter.HookContext{}, k, nil); err != nil {
			t.Errorf("%s with empty payload: %v", k, err)
		}
	}
}

func TestParseSyntheticKinds(t *testing.T) {
	a := New()
	res, err := a.ParseEvent(adapter.HookContext{}, KindSession, []byte(`{"sessionID":"ses_a","reason":"resume"}`))
	if err != nil || len(res.Events) != 1 || res.Events[0].Type != adapter.SessionStart ||
		res.Events[0].HarnessSessionID != "ses_a" || res.Events[0].Reason != "resume" || res.SessionRef != "ses_a" {
		t.Errorf("session: %+v %v", res, err)
	}
	if _, err := a.ParseEvent(adapter.HookContext{}, KindSession, []byte(`{}`)); err == nil {
		t.Error("session without id accepted")
	}
	res, _ = a.ParseEvent(adapter.HookContext{}, KindTUIExit, []byte(`{"code":130}`))
	if len(res.Events) != 1 || res.Events[0].Type != adapter.SessionEnd || res.Events[0].Reason != "exit" ||
		res.Events[0].Data["exit_code"] != 130 {
		t.Errorf("tui exit: %+v", res.Events)
	}
}

func TestReconcile(t *testing.T) {
	a := New()
	perm := `{"id":"per_1","sessionID":"ses_child","action":"shell","resources":["rm -rf x"]}`
	form := `{"id":"frm_1","sessionID":"ses_a","title":"Questions","fields":[{"key":"q0","title":"Which?","type":"string"}]}`
	for _, c := range []struct {
		status, payload, want string
	}{
		{"running", `{"sessionID":"ses_a","active":true,"permissions":[` + perm + `]}`, "permission"},
		{"waiting", `{"sessionID":"ses_a","active":true,"permissions":[` + perm + `]}`, ""},
		{"running", `{"sessionID":"ses_a","active":true,"forms":[` + form + `]}`, "question"},
		{"idle", `{"sessionID":"ses_a","active":true}`, "turn_start"},
		{"starting", `{"sessionID":"ses_a","active":true}`, "turn_start"},
		{"waiting", `{"sessionID":"ses_a","active":true}`, "turn_start"},
		{"running", `{"sessionID":"ses_a","active":true}`, ""},
		{"running", `{"sessionID":"ses_a","active":false}`, "turn_end"},
		{"running", `{"sessionID":"ses_a","outcome":"succeeded"}`, "turn_end"},
		{"running", `{"sessionID":"ses_a","outcome":"failed"}`, "error,turn_end"},
		{"waiting", `{"sessionID":"ses_a","outcome":"interrupted"}`, "turn_end"},
		{"idle", `{"sessionID":"ses_a","outcome":"failed"}`, ""},
		{"running", `{"sessionID":"ses_a","deleted":true}`, "session_end"},
		{"idle", `{"sessionID":"ses_a","deleted":true}`, "session_end"},
		{"waiting", `{"sessionID":"ses_a"}`, "turn_end"},
		{"idle", `{"sessionID":"ses_a"}`, ""},
		{"starting", `{"sessionID":"ses_a"}`, ""},
		{"", `{"sessionID":"ses_a","active":true}`, ""},
	} {
		res, err := a.ParseEvent(adapter.HookContext{Status: c.status}, KindReconcile, []byte(c.payload))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(types(res.Events), ","); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.status, c.payload, got, c.want)
		}
		if len(res.Events) > 0 && res.Events[0].Data["reconciled"] != true {
			t.Errorf("reconciled flag missing")
		}
	}
}

func TestEvaluateGuard(t *testing.T) {
	a := New()
	work := t.TempDir()
	hc := adapter.HookContext{RunID: "r1", Status: "running", Workdir: work, Deny: []string{".env*"}}
	eval := func(action, tool string, resources ...string) adapter.HookResult {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"sessionID": "ses_a", "action": action, "resources": resources,
			"tool": tool, "callID": "call_1", "effect": "allow"})
		res, err := a.ParseEvent(hc, KindEvaluate, b)
		if err != nil {
			t.Fatal(err)
		}
		if res.SessionRef != "ses_a" {
			t.Errorf("session ref %q", res.SessionRef)
		}
		return res
	}
	deny := func(res adapter.HookResult) string {
		if res.Output == nil {
			return ""
		}
		var out struct{ Effect, Message string }
		if err := json.Unmarshal(res.Output, &out); err != nil || out.Effect != "deny" ||
			!strings.HasPrefix(out.Message, "otag guard: ") {
			t.Errorf("bad deny output %s", res.Output)
		}
		if len(res.Events) != 1 || res.Events[0].Type != adapter.Activity || res.Events[0].Data["guard"] != "deny" {
			t.Errorf("deny without activity event: %+v", res.Events)
		}
		return out.Message
	}
	for _, c := range []struct {
		action, tool string
		resources    []string
		deny         bool
	}{
		{"shell", "shell", []string{"git push origin main"}, true},
		{"shell", "shell", []string{"cd sub", "git -C . push"}, true},
		{"shell", "shell", []string{"touch ../outside.txt"}, true},
		{"shell", "shell", []string{"ls -la", "go test ./..."}, false},
		{"edit", "write", []string{"src/main.go"}, false},
		{"edit", "write", []string{"."}, false},
		{"edit", "edit", []string{"config/.env.local"}, true},
		{"edit", "write", []string{"/etc/passwd"}, true},
		{"edit", "apply_patch", []string{"a.go", "../b.go"}, true},
		{"external_directory", "write", []string{"/tmp/elsewhere/*"}, true},
		{"external_directory", "read", []string{"/tmp/elsewhere/*"}, false},
		{"external_directory", "", []string{"/tmp/elsewhere/*"}, false},
		{"read", "read", []string{".env"}, true},
		{"read", "read", []string{"README.md"}, false},
	} {
		res := eval(c.action, c.tool, c.resources...)
		if got := deny(res) != ""; got != c.deny {
			t.Errorf("%s %s %v: deny=%v, want %v (%s)", c.action, c.tool, c.resources, got, c.deny, res.Output)
		}
		if !c.deny && (res.Output != nil || len(res.Events) > 0) {
			t.Errorf("%s %v: allowed requests answer and record nothing: %s %+v", c.action, c.resources, res.Output, res.Events)
		}
	}
	// The shell tool's workdir: commands are judged from where they run, which must be
	// inside the workspace; a cd inside the line moves on.
	hc.Deny = []string{"infra/prod/**"}
	_ = os.MkdirAll(filepath.Join(work, "infra", "prod"), 0o755)
	shell := func(workdir, tool, action string, resources ...string) bool {
		b, _ := json.Marshal(map[string]any{"sessionID": "ses_a", "action": action, "resources": resources,
			"tool": tool, "workdir": workdir, "callID": "call_2"})
		res, err := a.ParseEvent(hc, KindEvaluate, b)
		if err != nil {
			t.Fatal(err)
		}
		return res.Output != nil
	}
	for _, c := range []struct {
		workdir, tool, action string
		resources             []string
		deny                  bool
	}{
		{"", "shell", "shell", []string{"touch main.tf"}, false},
		{"infra/prod", "shell", "shell", []string{"touch main.tf"}, true},
		{filepath.Join(work, "infra"), "shell", "shell", []string{"touch prod/x"}, true},
		{"infra", "shell", "shell", []string{"touch README.md"}, false},
		{"", "shell", "shell", []string{"cd infra/prod", "touch main.tf"}, true},
		{"/tmp", "shell", "shell", []string{"ls"}, true},
		{"..", "shell", "shell", []string{"make"}, true},
		{"/tmp", "shell", "external_directory", []string{"/tmp/*"}, true},
		{"", "shell", "external_directory", []string{"/tmp/*"}, false}, // a read outside
		{"infra/prod", "write", "edit", []string{"main.go"}, false},    // edits stay workspace-relative
	} {
		if got := shell(c.workdir, c.tool, c.action, c.resources...); got != c.deny {
			t.Errorf("workdir %q %s %v: deny=%v, want %v", c.workdir, c.action, c.resources, got, c.deny)
		}
	}

	// Unknown run (no workspace): no opinion.
	b := []byte(`{"sessionID":"ses_a","action":"shell","resources":["git push"]}`)
	if res, _ := a.ParseEvent(adapter.HookContext{}, KindEvaluate, b); res.Output != nil || res.SessionRef != "ses_a" {
		t.Errorf("unknown run: %+v", res)
	}
}

func TestSharedPatchGuard(t *testing.T) {
	p := guard.Policy{Workdir: t.TempDir(), Deny: []string{".env*"}}
	for _, action := range []string{"shell", "edit"} {
		d := evaluate(p, evaluation{Action: action, Tool: "apply_patch", Resources: []string{"apply_patch <<'PATCH'\n*** Begin Patch\n  *** Add File: .env.local\n+x\n*** End Patch\nPATCH"}})
		if d.Allow || !strings.Contains(d.Reason, "deny list") {
			t.Fatal(action, d)
		}
	}
}
