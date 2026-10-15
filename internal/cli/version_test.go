package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime/debug"
	"testing"
)

func runVersion(t *testing.T, b Build, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, Env{
		Stdout: &out, Stderr: &errb, Cwd: t.TempDir(), Build: b,
		Getenv: func(string) string { return "" },
	})
	return code, out.String()
}

func TestVersionCommand(t *testing.T) {
	b := Build{Version: "0.1.0", Commit: "abc1234", Date: "2026-10-15T00:00:00Z"}
	want := "otag 0.1.0 (abc1234, 2026-10-15T00:00:00Z)\n"
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if code, out := runVersion(t, b, args...); code != ExitOK || out != want {
			t.Fatalf("%v: exit %d, %q", args, code, out)
		}
	}
	code, out := runVersion(t, b, "--json", "version")
	var e struct {
		OK   bool  `json:"ok"`
		Data Build `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil || code != ExitOK || !e.OK || e.Data != b {
		t.Fatalf("--json version: exit %d, %q, %v", code, out, err)
	}
	if code, out := runVersion(t, Build{}, "version"); code != ExitOK || out != "otag dev (none, unknown)\n" {
		t.Fatalf("defaults: exit %d, %q", code, out)
	}
	if code, _ := runVersion(t, b, "version", "extra"); code != ExitUsage {
		t.Fatalf("extra argument: exit %d", code)
	}
}

func TestResolveBuild(t *testing.T) {
	read := func(version string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: version}, Settings: settings}, true
		}
	}
	injected := Build{Version: "0.1.0", Commit: "c", Date: "d"}
	if got := resolveBuild(injected, read("v9.9.9")); got != injected {
		t.Fatalf("ldflags build must win: %+v", got)
	}
	// go install pkg@v0.1.0: no ldflags, module version known.
	if got := resolveBuild(Build{}, read("v0.1.0")); got.Version != "0.1.0" || got.Commit != "none" {
		t.Fatalf("go install: %+v", got)
	}
	// local build: (devel), VCS data from the settings.
	got := resolveBuild(Build{}, read("(devel)",
		debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-10-01T00:00:00Z"}))
	if got.Version != "dev" || got.Commit != "0123456789ab" || got.Date != "2026-10-01T00:00:00Z" {
		t.Fatalf("devel: %+v", got)
	}
	if got := resolveBuild(Build{}, func() (*debug.BuildInfo, bool) { return nil, false }); got != (Build{"dev", "none", "unknown"}) {
		t.Fatalf("no build info: %+v", got)
	}
}
