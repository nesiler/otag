package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseClaudeStatusline(t *testing.T) {
	ws, err := ParseClaudeStatusline(fixture(t, "claude-statusline.json"))
	if err != nil || len(ws) != 2 {
		t.Fatalf("%+v %v", ws, err)
	}
	if ws[0].Name != "5h" || *ws[0].UsedPercent != 23.5 || ws[0].ResetsAt != time.Unix(1791670430, 0).UTC() {
		t.Fatalf("5h %+v", ws[0])
	}
	if ws[1].Name != "7d" || *ws[1].UsedPercent != 41.2 {
		t.Fatalf("7d %+v", ws[1])
	}

	// The resets_at-in-used_percentage bug drops the value; spend may exceed 100.
	ws, err = ParseClaudeStatusline(fixture(t, "claude-statusline-bug.json"))
	if err != nil || len(ws) != 2 {
		t.Fatalf("%+v %v", ws, err)
	}
	if ws[0].Name != "7d" || !ws[0].Exhausted() {
		t.Fatalf("7d %+v", ws[0])
	}
	if s := ws[1]; s.Name != "spend-monthly" || *s.UsedPercent != 112.5 || *s.Used != 562.5 || *s.Limit != 500 {
		t.Fatalf("spend %+v", s)
	}

	// No rate_limits (API key, before the first response): nothing.
	if ws, err := ParseClaudeStatusline([]byte(`{"model":{}}`)); err != nil || len(ws) != 0 {
		t.Fatalf("%+v %v", ws, err)
	}
	if _, err := ParseClaudeStatusline([]byte(`{`)); err == nil {
		t.Fatal("broken JSON accepted")
	}
}

func TestParseStatuslineHarness(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, harness := range []string{"", "claude"} {
		if h, src, _, err := ParseStatusline(harness, fixture(t, "claude-statusline.json"), now); err != nil || h != "claude" || src != SourceClaudeStatusline {
			t.Fatalf("harness %q: %s %s %v", harness, h, src, err)
		}
	}
	if _, _, _, err := ParseStatusline("other", fixture(t, "claude-statusline.json"), now); err == nil {
		t.Fatal("unsupported harness accepted")
	}
}

func TestParseRolloutLineSkipsOtherBuckets(t *testing.T) {
	var got []*CodexLimits
	for _, l := range strings.Split(string(fixture(t, "rollout.jsonl")), "\n") {
		if c := ParseRolloutLine([]byte(l)); c != nil {
			got = append(got, c)
		}
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d lines", len(got))
	}
	last := got[1]
	if last.At != time.Date(2026, 10, 10, 19, 3, 55, 279000000, time.UTC) || len(last.Windows) != 2 {
		t.Fatalf("%+v", last)
	}
	p, s := last.Windows[0], last.Windows[1]
	if p.Name != "5h" || *p.UsedPercent != 100 || !p.Reached || p.ResetsAt != time.Unix(1791670430, 0).UTC() {
		t.Fatalf("primary %+v", p)
	}
	if s.Name != "7d" || *s.UsedPercent != 46 || s.Reached {
		t.Fatalf("secondary %+v", s)
	}
}

// rolloutLine is a main-bucket token_count line at time at with primary usage used.
func rolloutLine(at time.Time, used float64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":`+
		`{"limit_id":"codex","primary":{"used_percent":%v,"window_minutes":300,"resets_at":%d},"secondary":null,`+
		`"rate_limit_reached_type":null}}}`+"\n", at.UTC().Format(time.RFC3339Nano), used, at.Add(time.Hour).Unix())
}

func TestRolloutReaderBoundedAndIncremental(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "10", "10")
	older := filepath.Join(home, "sessions", "2026", "10", "09")
	_ = os.MkdirAll(day, 0o755)
	_ = os.MkdirAll(older, 0o755)
	t0 := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)

	// An older session with a main-bucket line; the newest one only has "premium".
	_ = os.WriteFile(filepath.Join(older, "rollout-a.jsonl"), []byte(rolloutLine(t0, 30)), 0o644)
	newest := filepath.Join(day, "rollout-b.jsonl")
	premium := strings.Split(string(fixture(t, "rollout.jsonl")), "\n")[3] + "\n"
	_ = os.WriteFile(newest, []byte(premium), 0o644)
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(newest, future, future)

	r := &RolloutReader{Home: home}
	l, err := r.Latest()
	if err != nil || l == nil || *l.Windows[0].UsedPercent != 30 {
		t.Fatalf("older session not used: %+v %v", l, err)
	}

	// A big file: only its tail is read, the line cut by the tail start is skipped.
	big := filepath.Join(day, "rollout-c.jsonl")
	f, _ := os.Create(big)
	filler := strings.Repeat("x", 1000)
	for f2 := 0; f2 < (rolloutTail/1000)+500; f2++ {
		_, _ = fmt.Fprintf(f, `{"timestamp":"2026-10-10T09:00:00Z","type":"response_item","payload":{"t":%q}}`+"\n", filler)
	}
	_, _ = f.WriteString(rolloutLine(t0.Add(time.Minute), 40))
	_ = f.Close()
	later := future.Add(time.Minute)
	_ = os.Chtimes(big, later, later)
	l, _ = r.Latest()
	if l == nil || *l.Windows[0].UsedPercent != 40 {
		t.Fatalf("tail of big file: %+v", l)
	}
	st, _ := os.Stat(big)
	if c := r.cursors[big]; c.offset != st.Size() {
		t.Fatalf("offset %d of %d", c.offset, st.Size())
	}

	// Appends are read from the cached offset; a partial line waits for its newline.
	af, _ := os.OpenFile(big, os.O_APPEND|os.O_WRONLY, 0)
	line := rolloutLine(t0.Add(2*time.Minute), 55)
	_, _ = af.WriteString(line[:40])
	_ = af.Sync()
	if l, _ = r.Latest(); *l.Windows[0].UsedPercent != 40 {
		t.Fatalf("partial line parsed: %+v", l)
	}
	_, _ = af.WriteString(line[40:])
	_ = af.Close()
	if l, _ = r.Latest(); *l.Windows[0].UsedPercent != 55 {
		t.Fatalf("appended line: %+v", l)
	}

	// A truncated file starts over.
	_ = os.WriteFile(big, []byte(rolloutLine(t0.Add(3*time.Minute), 60)), 0o644)
	_ = os.Chtimes(big, later, later)
	if l, _ = r.Latest(); *l.Windows[0].UsedPercent != 60 {
		t.Fatalf("after truncation: %+v", l)
	}

	// No sessions directory: nothing, no error.
	if l, err := (&RolloutReader{Home: t.TempDir()}).Latest(); l != nil || err != nil {
		t.Fatalf("%+v %v", l, err)
	}
}

// opencodeConfig writes a wrapper-style account dir with a LiteLLM provider whose key is a
// {file:} reference.
func opencodeConfig(t *testing.T, baseURL string) (configDir, key string) {
	t.Helper()
	configDir = t.TempDir()
	key = "sk-test-" + fmt.Sprint(time.Now().UnixNano())
	_ = os.WriteFile(filepath.Join(configDir, "litellm-key"), []byte(key+"\n"), 0o600)
	dir := filepath.Join(configDir, "config", "opencode")
	_ = os.MkdirAll(dir, 0o755)
	cfg := `{
  // jsonc comments and trailing commas are fine
  "provider": {
    "other": {"options": {"baseURL": "http://unused"}},
    "litellm": {"name": "LiteLLM", "options": {"baseURL": "` + baseURL + `/v1/", "apiKey": "{file:../../litellm-key}",},},
  },
}`
	_ = os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(cfg), 0o600)
	return configDir, key
}

func TestLiteLLMKeyInfo(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/info" {
			http.NotFound(w, r)
			return
		}
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write(fixture(t, "key-info.json"))
	}))
	defer srv.Close()
	configDir, key := opencodeConfig(t, srv.URL)
	tg, err := LoadLiteLLMTarget(OpenCodeConfigPath(configDir, "/nohome"), "", "/nohome", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if tg.Provider != "litellm" || tg.BaseURL != srv.URL {
		t.Fatalf("target %s %s", tg.Provider, tg.BaseURL)
	}
	ws, err := FetchLiteLLM(context.Background(), srv.Client(), tg)
	if err != nil || len(ws) != 1 {
		t.Fatalf("%+v %v", ws, err)
	}
	if auth.Load() != "Bearer "+key {
		t.Fatal("key not sent as bearer token") // the value itself is not printed
	}
	w := ws[0]
	if w.Name != "budget-30d" || *w.UsedPercent != 15.63 || *w.Used != 15.63 || *w.Limit != 100 ||
		w.ResetsAt != time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("%+v", w)
	}
}

func TestResolveSecretEnv(t *testing.T) {
	env := func(k string) string {
		if k == "LITELLM_KEY" {
			return "v"
		}
		return ""
	}
	if s, err := resolveSecret("{env:LITELLM_KEY}", "/", "/", env); err != nil || s != "v" {
		t.Fatal("env reference")
	}
	if _, err := resolveSecret("{env:MISSING}", "/", "/", env); err == nil {
		t.Fatal("missing env accepted")
	}
	if _, err := resolveSecret("{file:/does/not/exist}", "/", "/", env); err == nil || strings.Contains(err.Error(), "exist") {
		t.Fatalf("file error should not echo the path: %v", err)
	}
}

func TestClaudeOAuthUsage(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var hits atomic.Int32
	status := atomic.Int32{}
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || !strings.HasPrefix(r.Header.Get("User-Agent"), "claude-cli/") ||
			r.Header.Get("Authorization") != "Bearer tok-c2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s := int(status.Load()); s != 200 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write(fixture(t, "oauth-usage.json"))
	}))
	defer srv.Close()

	home := "/Users/u"
	c2 := home + "/.claude-work"
	var asked []string
	keychain := func(_ context.Context, svc string) ([]byte, error) {
		asked = append(asked, svc)
		if svc == KeychainServices(c2, home)[0] {
			return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-c2","refreshToken":"r","expiresAt":%d}}`,
				now.Add(time.Hour).UnixMilli())), nil
		}
		return nil, errors.New("not found")
	}
	tok, err := claudeToken(context.Background(), keychain, c2, home, now)
	if err != nil || tok != "tok-c2" {
		t.Fatalf("token: %v", err)
	}
	if want := "Claude Code-credentials-" + "87169a1d"; KeychainServices("/Users/u/.claude-work", "/Users/u")[0] != want {
		t.Fatalf("suffix formula: %v", KeychainServices("/Users/u/.claude-work", "/Users/u"))
	}
	if s := KeychainServices(home+"/.claude", home); len(s) != 2 || s[0] != "Claude Code-credentials" {
		t.Fatalf("default dir services %v", s)
	}
	// Expired tokens are not refreshed: no request at all.
	expired := func(context.Context, string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"old","expiresAt":%d}}`, now.Add(-time.Minute).UnixMilli())), nil
	}
	if _, err := claudeToken(context.Background(), expired, c2, home, now); !errors.Is(err, errTokenExpired) {
		t.Fatalf("expired: %v", err)
	}

	ws, err := FetchClaudeUsage(context.Background(), srv.Client(), srv.URL, tok, "claude-cli/2.1.293 (external, cli)", now)
	if err != nil || len(ws) != 3 {
		t.Fatalf("%+v %v", ws, err)
	}
	if ws[0].Name != "5h" || *ws[0].UsedPercent != 37 || ws[0].ResetsAt.Format(time.RFC3339) != "2026-10-10T22:00:00Z" {
		t.Fatalf("5h %+v", ws[0])
	}
	if ws[1].Name != "7d" || ws[2].Name != "7d-opus" || *ws[2].UsedPercent != 91 {
		t.Fatalf("%+v", ws)
	}
	status.Store(429)
	_, err = FetchClaudeUsage(context.Background(), srv.Client(), srv.URL, tok, "claude-cli/2.1.293 (external, cli)", now)
	var ra *RetryAfterError
	if !errors.As(err, &ra) || ra.Until != now.Add(2*time.Minute) {
		t.Fatalf("429: %v", err)
	}
}
