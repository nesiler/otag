package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/guard"
)

// The plugin's questions under a light run: pushes, edits outside the workspace and
// secrets are refused; commands elsewhere and writes the guard does not parse pass.
func TestEvaluateLight(t *testing.T) {
	a := New()
	work := t.TempDir()
	home, _ := os.UserHomeDir()
	outside := filepath.Join(home, "otag-light-outside")
	hc := adapter.HookContext{RunID: "r1", Status: "running", Workdir: work, Guard: guard.Light}
	eval := func(e evaluation) bool {
		t.Helper()
		e.SessionID = "ses_a"
		b, _ := json.Marshal(e)
		res, err := a.ParseEvent(hc, KindEvaluate, b)
		if err != nil {
			t.Fatal(err)
		}
		return res.Output == nil
	}
	for _, e := range []evaluation{
		{Action: "shell", Tool: "shell", Resources: []string{"git push origin HEAD"}},
		{Action: "edit", Tool: "write", Resources: []string{filepath.Join(outside, "x")}},
		{Action: "edit", Tool: "edit", Resources: []string{".env"}},
		{Action: "read", Tool: "read", Resources: []string{filepath.Join(work, ".env.local")}},
		{Action: "external_directory", Tool: "edit", Resources: []string{outside + "/*"}},
	} {
		if eval(e) {
			t.Errorf("light allowed %+v", e)
		}
	}
	for _, e := range []evaluation{
		{Action: "shell", Tool: "shell", Workdir: outside, Resources: []string{"ls"}},
		{Action: "shell", Tool: "shell", Resources: []string{"sed -i s/a/b/ " + filepath.Join(outside, "f")}},
		{Action: "shell", Tool: "shell", Resources: []string{"go build ./..."}},
		{Action: "edit", Tool: "write", Resources: []string{"main.go"}},
	} {
		if !eval(e) {
			t.Errorf("light refused %+v", e)
		}
	}
}
