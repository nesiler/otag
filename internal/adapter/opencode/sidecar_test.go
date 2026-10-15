package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

type emitted struct {
	kind, id string
	payload  []byte
}

// TestSidecar replays frames over a fake SSE endpoint: the first connection carries a
// turn of the run's session, a subagent session and a foreign session, then drops;
// the second connection triggers a reconcile.
func TestSidecar(t *testing.T) {
	const root, child, other = "ses_root", "ses_child", "ses_other"
	frame := func(id, typ, data string) string {
		return fmt.Sprintf("data: {\"id\":%q,\"type\":%q,\"data\":%s}\n\n", id, typ, data)
	}
	first := strings.Join([]string{
		"data: {\"id\":\"evt_c1\",\"type\":\"server.connected\",\"data\":{}}\n\n",
		": heartbeat\n\n",
		frame("evt_1", "session.execution.started", `{"sessionID":"ses_root"}`),
		frame("evt_2", "session.step.started", `{"sessionID":"ses_root","assistantMessageID":"msg_1","agent":"build","model":{"providerID":"litellm","id":"deepseek-flash-deepseek"}}`),
		frame("evt_3", "session.text.delta", `{"sessionID":"ses_root","delta":"Hel"}`),
		frame("evt_4", "session.created", `{"sessionID":"ses_child","parentID":"ses_root","slug":"x"}`),
		frame("evt_5", "session.execution.started", `{"sessionID":"ses_child"}`),
		frame("evt_6", "permission.asked", `{"id":"per_1","sessionID":"ses_child","action":"shell","resources":["make"]}`),
		frame("evt_7", "session.execution.succeeded", `{"sessionID":"ses_child"}`),
		frame("evt_8", "session.execution.started", `{"sessionID":"ses_other"}`),
		frame("evt_9", "form.created", `{"form":{"id":"frm_1","sessionID":"ses_other","title":"Q","fields":[]}}`),
		frame("evt_10", "session.text.ended", `{"sessionID":"ses_root","assistantMessageID":"msg_1","ordinal":0,"text":"All done."}`),
		frame("evt_11", "session.step.ended", `{"sessionID":"ses_root","assistantMessageID":"msg_1","cost":0,"tokens":{"input":1,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}`),
		frame("evt_12", "session.execution.succeeded", `{"sessionID":"ses_root"}`),
		"data: {\"id\":\"evt_13\",\n" + "data: \"type\":\"session.tool.input.started\",\"data\":{\"sessionID\":\"ses_root\",\"assistantMessageID\":\"msg_2\",\"id\":\"call_1\",\"name\":\"shell\"}}\n\n",
	}, "")

	var mu sync.Mutex
	conns := 0
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "s3cret" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/event":
			mu.Lock()
			conns++
			n := conns
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				_, _ = io.WriteString(w, first)
				return // drop the stream
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"evt_c2\",\"type\":\"server.connected\",\"data\":{}}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/api/session/active":
			_, _ = io.WriteString(w, `{"data":{"ses_root":{"type":"running"}}}`)
		case "/api/session":
			// A subagent created while the stream was down is found on reconnect.
			mu.Lock()
			n := conns
			mu.Unlock()
			if r.URL.Query().Get("parentID") == root && n > 1 {
				_, _ = io.WriteString(w, `{"data":[{"id":"ses_child2"}],"cursor":{}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[],"cursor":{}}`)
		case "/api/session/ses_root":
			_, _ = io.WriteString(w, `{"data":{"id":"ses_root","outcome":"succeeded","location":{"directory":"/w/al-1"}}}`)
		case "/api/session/ses_root/message":
			// One step finished while disconnected, one long before.
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"msg_9","type":"assistant","model":{"providerID":"litellm","id":"m"},`+
				`"time":{"created":1,"completed":%d},"cost":0,"tokens":{"input":5,"output":6,"reasoning":0,"cache":{"read":0,"write":0}}},`+
				`{"id":"msg_0","type":"assistant","time":{"created":1,"completed":1000},"cost":0,`+
				`"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}]}`, time.Now().UnixMilli())
		case "/api/session/ses_child/message", "/api/session/ses_child2/message":
			_, _ = io.WriteString(w, `{"data":[]}`)
		case "/api/permission/request":
			_, _ = io.WriteString(w, `{"data":[{"id":"per_1","sessionID":"ses_child","action":"shell","resources":["make"]},`+
				`{"id":"per_2","sessionID":"ses_other","action":"shell","resources":["x"]},`+
				`{"id":"per_3","sessionID":"ses_child2","action":"edit","resources":["a.go"]}]}`)
		case "/api/form":
			_, _ = io.WriteString(w, `{"data":[]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	var got []emitted
	ready := 0
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sc := adapter.Sidecar{
		RunID: "al-1-r1",
		Getenv: func(k string) string {
			if k == "OPENCODE_PASSWORD" {
				return "s3cret"
			}
			return ""
		},
		Ready: func() error { ready++; return nil },
		Emit: func(_ context.Context, kind string, payload []byte, id string) ([]byte, error) {
			got = append(got, emitted{kind, id, payload})
			if kind == KindReconcile {
				close(done)
			}
			return nil, nil
		},
	}
	errc := make(chan error, 1)
	go func() {
		errc <- New().RunSidecar(ctx, sc, []string{"--url", srv.URL, "--dir", "/w/al-1", "--session", root,
			"--model", "litellm/fallback"})
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("no reconcile; emitted %v", got)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}

	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.kind+"/"+e.id)
	}
	want := []string{
		KindSession + "/al-1-r1:ses_root:session",
		"session.execution.started/evt_1",
		"session.step.started/evt_2",
		"permission.asked/evt_6", // subagent: permission yes, execution no
		"session.step.ended/evt_11",
		"session.execution.succeeded/evt_12",
		"session.tool.input.started/evt_13", // multi-line data frame
		"session.step.ended/usage:msg_9",    // reconcile: usage finished while disconnected
		KindReconcile + "/",
	}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Errorf("emitted\n got %v\nwant %v", kinds, want)
	}
	if ready != 1 {
		t.Errorf("ready called %d times", ready)
	}
	a := New()
	for _, e := range got {
		res, err := a.ParseEvent(adapter.HookContext{Status: "running"}, e.kind, e.payload)
		if err != nil {
			t.Fatalf("%s: %v", e.kind, err)
		}
		switch e.kind {
		case KindSession:
			if res.Events[0].HarnessSessionID != root || res.Events[0].Reason != "startup" {
				t.Errorf("session start %+v", res.Events)
			}
		case "session.execution.succeeded":
			if res.Events[0].Text != "All done." {
				t.Errorf("turn_end text %q", res.Events[0].Text)
			}
		case "session.step.ended":
			u := res.Events[0].Usage
			if e.id == "evt_11" && u.Model != "litellm/deepseek-flash-deepseek" ||
				e.id == "usage:msg_9" && (u.Model != "litellm/m" || u.DedupeKey != "msg_9" || u.OutputTokens != 6) {
				t.Errorf("usage %s %+v", e.id, u)
			}
		case KindReconcile:
			var r Reconcile
			_ = json.Unmarshal(e.payload, &r)
			if !r.Active || r.Outcome != "succeeded" || len(r.Permissions) != 2 || r.Permissions[0].ID != "per_1" ||
				r.Permissions[1].ID != "per_3" {
				t.Errorf("reconcile %+v", r)
			}
			if len(res.Events) != 1 || res.Events[0].Type != adapter.Permission {
				t.Errorf("reconcile events %+v", res.Events)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, q := range queries {
		if strings.HasPrefix(q, "/api/permission/request") || strings.HasPrefix(q, "/api/form") {
			if !strings.Contains(q, "location%5Bdirectory%5D=%2Fw%2Fal-1") {
				t.Errorf("list not scoped to the workspace: %s", q)
			}
		}
	}
}

func TestSidecarArgs(t *testing.T) {
	sc := adapter.Sidecar{Getenv: func(string) string { return "" }}
	if err := New().RunSidecar(context.Background(), sc, []string{"--url", "http://x"}); err == nil {
		t.Error("missing --session accepted")
	}
	if err := New().RunSidecar(context.Background(), sc, []string{"--bogus"}); err == nil {
		t.Error("bad flag accepted")
	}
}

// A stream that stays silent past the heartbeat timeout is reopened.
func TestSidecarHeartbeat(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" {
			_, _ = io.WriteString(w, `{"data":{}}`)
			return
		}
		mu.Lock()
		conns++
		mu.Unlock()
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"type\":\"server.connected\",\"data\":{}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sc := adapter.Sidecar{Getenv: func(string) string { return "pw" }, Ready: func() error { return nil },
		Emit: func(context.Context, string, []byte, string) ([]byte, error) { return nil, nil }}
	a := New()
	a.Heartbeat = 300 * time.Millisecond
	go func() { _ = a.RunSidecar(ctx, sc, []string{"--url", srv.URL, "--session", "ses_a"}) }()
	for ctx.Err() == nil {
		mu.Lock()
		n := conns
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("silent stream was not reopened")
}

// A server that accepts the connection but never sends headers is given up too.
func TestSidecarHeaderTimeout(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		mu.Unlock()
		<-r.Context().Done() // no headers, ever
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a := New()
	a.Heartbeat = 200 * time.Millisecond
	sc := adapter.Sidecar{Getenv: func(string) string { return "pw" }, Ready: func() error { return nil },
		Emit: func(context.Context, string, []byte, string) ([]byte, error) { return nil, nil }}
	go func() { _ = a.RunSidecar(ctx, sc, []string{"--url", srv.URL, "--session", "ses_a"}) }()
	for ctx.Err() == nil {
		mu.Lock()
		n := conns
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a server without response headers stalled the sidecar")
}

// A failing bridge (e.g. a locked database) is retried in order; the stream waits.
func TestSidecarEmitRetry(t *testing.T) {
	frames := "data: {\"id\":\"c\",\"type\":\"server.connected\",\"data\":{}}\n\n" +
		"data: {\"id\":\"evt_1\",\"type\":\"session.execution.started\",\"data\":{\"sessionID\":\"ses_a\"}}\n\n" +
		"data: {\"id\":\"evt_2\",\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses_a\"}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		_, _ = io.WriteString(w, frames)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var ok []string
	fails := map[string]int{"evt_1": 2}
	sc := adapter.Sidecar{RunID: "r", Getenv: func(string) string { return "pw" }, Ready: func() error { return nil },
		Emit: func(_ context.Context, kind string, _ []byte, id string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			if fails[id] > 0 {
				fails[id]--
				return nil, errors.New("database is locked")
			}
			ok = append(ok, id)
			if id == "evt_2" {
				cancel()
			}
			return nil, nil
		}}
	_ = New().RunSidecar(ctx, sc, []string{"--url", srv.URL, "--session", "ses_a"})
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(ok, " ") != "r:ses_a:session evt_1 evt_2" {
		t.Errorf("recorded %v", ok)
	}
}
