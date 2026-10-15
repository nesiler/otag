package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

// A light run's settings have no sandbox block (a profile's own is kept as is); the
// hooks, the hard denies and disableAllHooks false stay.
func TestPrepareLight(t *testing.T) {
	spec := testSpec()
	spec.Guard = guard.Light
	ls, err := New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(fileMap(ls)[settingsFile], &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s["sandbox"]; ok {
		t.Fatalf("light settings have a sandbox: %v", s["sandbox"])
	}
	deny, _ := s["permissions"].(map[string]any)["deny"].([]any)
	if s["disableAllHooks"] != false || s["hooks"].(map[string]any)["PreToolUse"] == nil ||
		!slices.Contains(deny, any("Bash(git push *)")) {
		t.Fatalf("light settings lost the guard: %s", fileMap(ls)[settingsFile])
	}
	if !slices.Contains(ls.Argv, "--dangerously-skip-permissions") {
		t.Fatalf("argv: %v", ls.Argv)
	}
	spec.Profile.Args = []string{"--settings", `{"sandbox":{"enabled":false}}`}
	if ls, err = New().Prepare(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fileMap(ls)[settingsFile]), `"enabled": false`) {
		t.Fatalf("profile sandbox: %s", fileMap(ls)[settingsFile])
	}
}

// The PreToolUse guard of a light run: push and writes outside are refused, a
// sed -i outside, a Go cache write and commands it cannot read are not.
func TestGuardLight(t *testing.T) {
	wd := t.TempDir()
	home, _ := os.UserHomeDir()
	hc := adapter.HookContext{RunID: "al-1-r1", Workdir: wd, Guard: guard.Light}
	a := New()
	pre := func(tool string, input any) []byte {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"session_id": "s", "tool_name": tool, "tool_input": input,
			"tool_use_id": "toolu_x", "cwd": wd})
		res, err := a.ParseEvent(hc, "PreToolUse", raw)
		if err != nil {
			t.Fatal(err)
		}
		return res.Output
	}
	for _, c := range []struct {
		tool  string
		input any
	}{
		{"Bash", map[string]any{"command": "git push --force origin HEAD"}},
		{"Bash", map[string]any{"command": "cat .env"}},
		{"Write", map[string]any{"file_path": filepath.Join(home, "otag-light-outside.txt")}},
		{"Edit", map[string]any{"file_path": filepath.Join(wd, ".env.local")}},
		{"Read", map[string]any{"file_path": filepath.Join(wd, ".env")}},
	} {
		if out := pre(c.tool, c.input); !strings.Contains(string(out), `"deny"`) {
			t.Errorf("light allowed %s %v", c.tool, c.input)
		}
	}
	for _, c := range []struct {
		tool  string
		input any
	}{
		{"Bash", map[string]any{"command": "sed -i '' s/a/b/ " + filepath.Join(home, "otag-light-outside.txt")}},
		{"Bash", map[string]any{"command": "go test ./... && mkdir -p ~/Library/Caches/go-build/aa"}},
		{"Bash", "not an object"},
		{"Write", map[string]any{"file_path": filepath.Join(wd, "main.go")}},
	} {
		if out := pre(c.tool, c.input); out != nil {
			t.Errorf("light refused %s %v: %s", c.tool, c.input, out)
		}
	}
}
