package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/paths"
)

// client talks to one OpenCode server (HTTP Basic, user "opencode").
type client struct {
	base, password string
	http           *http.Client
}

func newClient(base, password string) *client {
	base, _ = splitURL(base)
	return &client{base: strings.TrimRight(base, "/"), password: password, http: &http.Client{Timeout: 15 * time.Second}}
}

// apiError is a non-2xx answer.
type apiError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

func isStatus(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == code
}

func (c *client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("opencode", c.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do sends a JSON request and decodes the answer's "data" field into out (if not nil).
// Error bodies are cut short; /api/config answers are never echoed (they hold keys).
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return &apiError{Method: method, Path: strings.SplitN(path, "?", 2)[0], Status: resp.StatusCode, Body: msg}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func locationQuery(dir string) string {
	return "?" + url.Values{"location[directory]": {dir}}.Encode()
}

// session is the part of Session.Info otag reads.
type session struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"` // how the last turn ended: succeeded|failed|interrupted
	Model    *Model `json:"model"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// SetupServer implements adapter.ServerSetup: it checks that the server runs otag's
// per-launch config, creates the session (or finds the one to resume), checks that
// otag's plugin is active for the workspace, and points the TUI and the sidecar at the
// session.
func (a *Adapter) SetupServer(ctx context.Context, spec adapter.RunSpec, launch *adapter.LaunchSpec) error {
	if launch.Server == nil {
		return errors.New("no server")
	}
	base, _ := splitURL(launch.Server.URL)
	c := newClient(base, launch.Env["OPENCODE_PASSWORD"])
	ctx, cancel := context.WithTimeout(ctx, a.setupTimeout())
	defer cancel()

	// tcp-ready is not HTTP-ready.
	if err := poll(ctx, func() (bool, error) {
		return c.do(ctx, "GET", "/api/info", nil, nil) == nil, nil
	}); err != nil {
		return fmt.Errorf("server not answering: %w", err)
	}
	if err := a.checkConfig(ctx, c, spec); err != nil {
		return err
	}

	model, _ := ParseModel(spec.Profile.Model)
	var s session
	if id := spec.ResumeSessionID; id != "" {
		var got struct{ Data session }
		if err := c.do(ctx, "GET", "/api/session/"+url.PathEscape(id), nil, &got); err != nil {
			if isStatus(err, 404) {
				return fmt.Errorf("session %s to resume not found in the account's OpenCode data", id)
			}
			return err
		}
		s = got.Data
		// The session must belong to this workspace: the guard judges paths against
		// Workdir, and OpenCode resolves the agent's relative paths against the
		// session's location.
		if !sameDir(s.Location.Directory, spec.Workdir) {
			return fmt.Errorf("session %s to resume belongs to %s, not to the workspace %s", id, s.Location.Directory, spec.Workdir)
		}
		// Today's deny rules replace the ones the session was created with.
		if err := c.do(ctx, "PATCH", "/api/session/"+url.PathEscape(id),
			map[string]any{"permissions": HardDenies(spec.Deny)}, nil); err != nil {
			return fmt.Errorf("update session rules: %w", err)
		}
		if model != nil && (s.Model == nil || s.Model.ProviderID != model.ProviderID || s.Model.ID != model.ID ||
			(model.Variant != "" && s.Model.Variant != model.Variant)) {
			if err := c.do(ctx, "POST", "/api/session/"+url.PathEscape(id)+"/model", map[string]any{"model": model}, nil); err != nil {
				return fmt.Errorf("switch model: %w", err)
			}
		}
	} else {
		// No id: the server makes a native one (a made-up id breaks OpenCode Zen).
		body := map[string]any{
			"title":       "otag " + spec.RunID,
			"location":    map[string]string{"directory": spec.Workdir},
			"metadata":    map[string]string{"otag_run": spec.RunID, "otag_task": spec.TaskID, "otag_project": spec.Project},
			"permissions": HardDenies(spec.Deny),
		}
		if model != nil {
			body["model"] = model
		}
		var got struct{ Data session }
		if err := c.do(ctx, "POST", "/api/session", body, &got); err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		s = got.Data
	}
	if s.ID == "" {
		return errors.New("server returned a session without id")
	}
	if err := a.checkPlugin(ctx, c, spec.Workdir); err != nil {
		return err
	}

	launch.HarnessSessionID = s.ID
	launch.Argv = TUIArgv(spec, a.command(spec), base, s.ID)
	for i, sc := range launch.Sidecars {
		if sc.Name == "events" {
			p := adapter.SidecarProcess(spec, "events", SidecarArgs(spec, base, s.ID)...)
			p.Env, p.ReadyTimeout = sc.Env, sc.ReadyTimeout
			launch.Sidecars[i] = p
		}
	}
	return nil
}

func (a *Adapter) setupTimeout() time.Duration {
	if a.SetupTimeout > 0 {
		return a.SetupTimeout
	}
	return 20 * time.Second
}

// checkConfig verifies that OPENCODE_CONFIG_CONTENT reached the server (#52284: some
// paths silently drop it) by looking for otag's plugin in the config layers. The
// config itself is never logged: it holds provider keys.
func (a *Adapter) checkConfig(ctx context.Context, c *client, spec adapter.RunSpec) error {
	var layers []struct {
		Info struct {
			Plugins []json.RawMessage `json:"plugins"`
		} `json:"info"`
	}
	req, err := c.request(ctx, "GET", "/api/config"+locationQuery(spec.Workdir), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("read config: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&layers); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	want := "file://" + filepath.Join(spec.RunDir, pluginDir)
	for _, l := range layers {
		for _, p := range l.Info.Plugins {
			var s string
			var o struct {
				Package string `json:"package"`
			}
			if json.Unmarshal(p, &s) == nil && s == want || json.Unmarshal(p, &o) == nil && o.Package == want {
				return nil
			}
		}
	}
	return errors.New("server did not load otag's config content (OPENCODE_CONFIG_CONTENT)")
}

// checkPlugin waits until otag's plugin is active for dir. Plugins load lazily per
// location; the first listing starts the load.
func (a *Adapter) checkPlugin(ctx context.Context, c *client, dir string) error {
	var last string
	err := poll(ctx, func() (bool, error) {
		var got struct {
			Data []struct {
				ID    string `json:"id"`
				State struct {
					Status string `json:"status"`
					Error  string `json:"error"`
				} `json:"state"`
			} `json:"data"`
		}
		if err := c.do(ctx, "GET", "/api/plugin"+locationQuery(dir), nil, &got); err != nil {
			last = err.Error()
			return false, nil
		}
		for _, p := range got.Data {
			if p.ID != PluginID {
				continue
			}
			switch p.State.Status {
			case "active":
				return true, nil
			case "failed":
				return false, fmt.Errorf("otag plugin failed to load: %s", p.State.Error)
			}
			last = "plugin " + p.State.Status
		}
		return false, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		if last == "" {
			last = "not listed"
		}
		return fmt.Errorf("otag plugin not active for %s (%s)", dir, last)
	}
	return err
}

// poll calls f every 200 ms until it reports done, fails, or ctx ends.
func poll(ctx context.Context, f func() (bool, error)) error {
	for {
		done, err := f()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// targetClient builds a client for a live run from its recorded server URL and the
// password in its run dir. Stop and Deliver get no RunSpec: the run dir recorded in
// the URL (RecordedURL) wins; without it (older runs) it is found the way otag finds
// it, from OTAG_DATA_DIR/OTAG_DB.
func (a *Adapter) targetClient(t adapter.Target) (*client, error) {
	if t.ServerURL == "" || t.RunID == "" {
		return nil, errors.New("no server recorded for the run")
	}
	base, dir := splitURL(t.ServerURL)
	if dir == "" {
		getenv := a.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		dir = paths.RunDir(getenv, t.RunID)
	}
	pw, err := os.ReadFile(filepath.Join(dir, passwordFile))
	if err != nil {
		return nil, fmt.Errorf("server password: %w", err)
	}
	return newClient(base, strings.TrimSpace(string(pw))), nil
}

// sameDir compares two directories after cleaning and resolving symlinks.
func sameDir(a, b string) bool {
	clean := func(p string) string {
		p = filepath.Clean(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	return a != "" && clean(a) == clean(b)
}

// Stop implements adapter.Stopper: interrupt the running turn over the API, then
// quit the TUI (Ctrl-C quits an idle TUI; a second one covers a busy or non-empty
// prompt). otag kills the server and the pane afterwards.
func (a *Adapter) Stop(ctx context.Context, t adapter.Target) error {
	if c, err := a.targetClient(t); err == nil && t.HarnessSessionID != "" {
		ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = c.do(ictx, "POST", "/api/session/"+url.PathEscape(t.HarnessSessionID)+"/interrupt", nil, nil)
		cancel()
	}
	if t.Pane == nil {
		return errors.New("no pane")
	}
	if err := t.Pane.SendKeys(ctx, "C-c"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(400 * time.Millisecond):
	}
	_ = t.Pane.SendKeys(ctx, "C-c") // the pane may be gone already
	return nil
}

// Deliver implements adapter.Deliverer (phase 4 plumbing): the message goes into the
// session inbox, "queue" (after the current turn) or "steer" (at the next step
// boundary). A message id of the form msg_* makes the delivery idempotent.
func (a *Adapter) Deliver(ctx context.Context, t adapter.Target, msg adapter.Message) error {
	if t.HarnessSessionID == "" {
		return errors.New("no session")
	}
	c, err := a.targetClient(t)
	if err != nil {
		return err
	}
	body := map[string]any{"text": msg.Text, "delivery": "queue"}
	if msg.Steer {
		body["delivery"] = "steer"
	}
	if strings.HasPrefix(msg.ID, "msg_") {
		body["id"] = msg.ID
	}
	return c.do(ctx, "POST", "/api/session/"+url.PathEscape(t.HarnessSessionID)+"/prompt", body, nil)
}
