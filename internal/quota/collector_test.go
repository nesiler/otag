package quota

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/db"
)

func TestCollectorsCollect(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	data := t.TempDir()
	env := map[string]string{"HOME": t.TempDir(), "OTAG_DATA_DIR": data}

	// codex: a rollout in its CODEX_HOME.
	codexHome := t.TempDir()
	day := filepath.Join(codexHome, "sessions", "2026", "10", "10")
	_ = os.MkdirAll(day, 0o755)
	_ = os.WriteFile(filepath.Join(day, "rollout-x.jsonl"), []byte(rolloutLine(now.Add(-time.Minute), 42)), 0o644)

	// opencode: LiteLLM behind httptest.
	var litellmHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		litellmHits.Add(1)
		_, _ = w.Write(fixture(t, "key-info.json"))
	}))
	defer srv.Close()
	ocDir, _ := opencodeConfig(t, srv.URL)

	// claude: OAuth usage enabled, keychain stubbed.
	var oauthHits atomic.Int32
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthHits.Add(1)
		_, _ = w.Write(fixture(t, "oauth-usage.json"))
	}))
	defer usage.Close()
	keychain := func(context.Context, string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"t","expiresAt":%d}}`, now.Add(time.Hour).UnixMilli())), nil
	}

	_, _ = d.Exec(`UPDATE accounts SET config_dir = ? WHERE name = 'cx'`, codexHome)
	_, _ = d.Exec(`UPDATE accounts SET config_dir = ? WHERE name = 'oc'`, ocDir)
	_, _ = d.Exec(`UPDATE accounts SET config_dir = '/c1', config = '{"quota":{"oauth_usage":true}}' WHERE name = 'c1'`)

	c := New(d, Options{
		Getenv: func(k string) string { return env[k] }, HTTP: srv.Client(), Keychain: keychain,
		ClaudeUsageURL: usage.URL, UserAgent: func(context.Context, string) string { return "claude-cli/2.1.293 (external, cli)" },
	})
	if err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	src := func(source string) int {
		return count(t, d, `SELECT count(*) FROM quota_snapshots WHERE source = ?`, source)
	}
	if src(SourceCodexRollout) != 1 || src(SourceLiteLLM) != 1 || src(SourceClaudeOAuth) != 3 {
		t.Fatalf("codex %d litellm %d oauth %d", src(SourceCodexRollout), src(SourceLiteLLM), src(SourceClaudeOAuth))
	}
	// A second round: network sources are spaced out, files are not re-ingested.
	if err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if litellmHits.Load() != 1 || oauthHits.Load() != 1 {
		t.Fatalf("litellm %d oauth %d calls", litellmHits.Load(), oauthHits.Load())
	}
	if n := count(t, d, `SELECT count(*) FROM quota_snapshots`); n != 5 {
		t.Fatalf("rows after second round %d", n)
	}

	// Unreachable LiteLLM: no snapshot and no error, only a problem.
	srv.Close()
	c2 := New(d, Options{Getenv: func(k string) string { return env[k] }, Account: "oc", LiteLLMEvery: time.Nanosecond})
	if err := c2.Collect(ctx); err != nil {
		t.Fatalf("unreachable is an error: %v", err)
	}
	if p := c2.Problems()["oc"]; p == "" {
		t.Fatal("no problem reported")
	}
	if src(SourceLiteLLM) != 1 {
		t.Fatal("snapshot written for an unreachable server")
	}
}

// The OAuth endpoint is not asked while the statusLine is fresh.
func TestOAuthSkippedWhileStatuslineFresh(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	_, _ = d.Exec(`UPDATE accounts SET config = '{"quota":{"oauth_usage":true}}' WHERE name = 'c1'`)
	record(t, d, Observation{AccountID: 1, Source: SourceClaudeStatusline, ObservedAt: time.Now(),
		Windows: []Window{{Name: "5h", UsedPercent: ptr(1)}}})
	asked := false
	c := New(d, Options{Getenv: func(string) string { return "" }, Account: "c1",
		Keychain: func(context.Context, string) ([]byte, error) { asked = true; return nil, os.ErrNotExist }})
	if err := c.Collect(ctx); err != nil || asked {
		t.Fatalf("keychain read while fresh: %v %v", asked, err)
	}
}

func statusLine(t *testing.T, d *db.DB, data string, payload []byte, command string) (StatusLine, *bytes.Buffer, *int) {
	opens := 0
	var out bytes.Buffer
	return StatusLine{
		Account: "c1", Harness: "claude", Command: command, DataDir: data, Payload: payload,
		OpenDB: func(context.Context) (*db.DB, error) { opens++; return d, nil },
		Getenv: func(string) string { return "" }, Env: os.Environ(), Stdout: &out, Stderr: &out,
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	}, &out, &opens
}

func TestStatusLineWrapperRecordsAndPassesThrough(t *testing.T) {
	ctx := context.Background()
	d := testDB(t)
	data := t.TempDir()
	payload := fixture(t, "claude-statusline.json")

	// The original command's output is passed through byte for byte, with the payload
	// on its stdin.
	script := filepath.Join(t.TempDir(), "orig.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nin=$(cat)\nprintf '\\033[2m%s\\033[0m\\nline2 %s' \"$(echo \"$in\" | wc -c | tr -d ' ')\" \"$OTAG_STATUSLINE_DEPTH\"\n"), 0o755)
	want, _ := runSh(script, payload)

	s, out, opens := statusLine(t, d, data, payload, "sh "+script)
	s.Now = time.Unix(1791660000, 0)
	s.Run(ctx)
	if out.String() != want || !strings.HasSuffix(want, "line2 1") {
		t.Fatalf("passthrough:\n got %q\nwant %q", out.String(), want)
	}
	if n := count(t, d, `SELECT count(*) FROM quota_snapshots WHERE account_id = 1 AND source = 'claude_statusline'`); n != 2 || *opens != 1 {
		t.Fatalf("rows %d opens %d", n, *opens)
	}
	f, ok := ReadStatusFile(StatusFilePath(data, "c1"))
	if !ok || len(f.Windows) != 2 || f.RecordedAt.IsZero() {
		t.Fatalf("status file %+v", f)
	}

	// Same values within 30 s: nothing is rewritten at all.
	s1, _, opens1 := statusLine(t, d, data, payload, "true")
	s1.Now = s.Now.Add(20 * time.Second)
	s1.Run(ctx)
	if f1, _ := ReadStatusFile(StatusFilePath(data, "c1")); *opens1 != 0 || !f1.ObservedAt.Equal(s.Now) {
		t.Fatalf("unchanged state rewritten: opens %d file %+v", *opens1, f1)
	}
	// Same values within a minute: no database write, the file moves on.
	s2, _, opens2 := statusLine(t, d, data, payload, "true")
	s2.Now = s.Now.Add(40 * time.Second)
	s2.Run(ctx)
	if *opens2 != 0 {
		t.Fatal("unchanged values written to the database")
	}
	f2, _ := ReadStatusFile(StatusFilePath(data, "c1"))
	if !f2.ObservedAt.Equal(s2.Now) || !f2.RecordedAt.Equal(f.RecordedAt) {
		t.Fatalf("file %+v", f2)
	}

	// The settings lookup finds the user's command; a settings statusLine that is the
	// wrapper itself is not run again.
	cfg := t.TempDir()
	_ = os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"printf user-line"}}`), 0o600)
	s3, out3, _ := statusLine(t, d, data, []byte(`{"workspace":{"project_dir":"/nonexistent"}}`), "")
	s3.ConfigDir = cfg
	s3.Run(ctx)
	if out3.String() != "user-line" {
		t.Fatalf("settings passthrough %q", out3.String())
	}
	_ = os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"/x/bin/otag statusline --account c1"}}`), 0o600)
	logged := 0
	s4, out4, _ := statusLine(t, d, data, payload, "")
	s4.ConfigDir = cfg
	s4.Log = func(string, ...any) { logged++ }
	s4.Run(ctx)
	if out4.Len() != 0 || logged != 0 {
		t.Fatalf("wrapper without --command: output %q, %d log lines", out4.String(), logged)
	}
	// The user opted their own sessions in (docs/QUOTA.md): the settings statusLine is
	// the wrapper with their command after --command. A otag run's wrapper finds it and
	// runs that command, not otag again.
	_ = os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"statusLine":{"type":"command",`+
		`"command":"/x/bin/otag statusline --account c1 --command 'printf \"%s\" user-own'"}}`), 0o600)
	s6, out6, _ := statusLine(t, d, data, payload, "")
	s6.ConfigDir = cfg
	s6.Log = func(string, ...any) { logged++ }
	s6.Run(ctx)
	if out6.String() != "user-own" || logged != 0 {
		t.Fatalf("opted-in user settings: output %q, %d log lines", out6.String(), logged)
	}
	// Inside a wrapped command, otag statusline does nothing at all.
	s5, out5, opens5 := statusLine(t, d, data, payload, "printf x")
	s5.Getenv = func(k string) string {
		if k == RecursionEnv {
			return "1"
		}
		return ""
	}
	s5.Run(ctx)
	if out5.Len() != 0 || *opens5 != 0 {
		t.Fatal("recursion guard")
	}
}

// An unknown account or a broken payload never breaks the user's status line.
func TestStatusLineNeverFails(t *testing.T) {
	d := testDB(t)
	for _, payload := range [][]byte{[]byte(`{not json`), nil, fixture(t, "claude-statusline.json")} {
		s, out, _ := statusLine(t, d, t.TempDir(), payload, "printf ok")
		s.Account = "nope"
		s.Run(context.Background())
		if out.String() != "ok" {
			t.Fatalf("%q: output %q", payload, out.String())
		}
	}
}

// runSh runs the script the way Claude Code would, plus the recursion marker.
func runSh(script string, stdin []byte) (string, error) {
	cmd := exec.Command("/bin/sh", "-c", "sh "+script)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append(os.Environ(), RecursionEnv+"=1")
	out, err := cmd.Output()
	return string(out), err
}

func TestUnwrapCommand(t *testing.T) {
	for in, want := range map[string]string{
		"sh ~/.claude/statusline-command.sh":                                                              "sh ~/.claude/statusline-command.sh",
		"/opt/otag statusline --account c1":                                                               "",
		"/opt/otag statusline --account c1 --command 'sh ~/.claude/s.sh'":                                 "sh ~/.claude/s.sh",
		`"/My Apps/otag" statusline --command="printf \"a b\""`:                                           `printf "a b"`,
		"otag statusline --command 'otag statusline --command \"echo deep\"'":                             "echo deep",
		"otag statusline --command 'otag statusline --account x'":                                         "",
		"/usr/local/bin/xotag statusline":                                                                 "/usr/local/bin/xotag statusline",
		`printf 'otag statusline is fine as text'`:                                                        `printf 'otag statusline is fine as text'`,
		"otag statusline --command 'otag statusline --command \"otag statusline --command '\\''x'\\''\"'": "x",
	} {
		if got := UnwrapCommand(in); got != want {
			t.Errorf("UnwrapCommand(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsWrapper("'/a b/otag' statusline") || IsWrapper("echo otag") {
		t.Fatal("IsWrapper")
	}
}

// With a database that needs a migration, the statusLine writes only its file; the
// daemon's collector ingests it after migrating.
func TestStatusLineSkipsUnmigratedDatabase(t *testing.T) {
	data := t.TempDir()
	payload := fixture(t, "claude-statusline.json")
	logged := 0
	s := StatusLine{Account: "c1", Harness: "claude", Command: "true", DataDir: data, Payload: payload,
		Now: time.Unix(1791660000, 0),
		OpenDB: func(ctx context.Context) (*db.DB, error) {
			return db.OpenWith(ctx, filepath.Join(data, "missing.db"), db.Options{NoMigrate: true})
		},
		Getenv: func(string) string { return "" }, Stdout: io.Discard, Stderr: io.Discard,
		Log: func(f string, a ...any) { logged++; t.Logf(f, a...) }}
	s.Run(context.Background())
	f, ok := ReadStatusFile(StatusFilePath(data, "c1"))
	if !ok || len(f.Windows) != 2 || !f.RecordedAt.IsZero() || logged != 0 {
		t.Fatalf("file %+v ok %v, %d log lines", f, ok, logged)
	}
	if _, err := os.Stat(filepath.Join(data, "missing.db")); !os.IsNotExist(err) {
		t.Fatal("NoMigrate created the database")
	}
}
