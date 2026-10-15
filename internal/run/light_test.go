package run

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/adapter/claude"
	"github.com/nesiler/otag/internal/guard"
	"github.com/nesiler/otag/internal/workspace"
)

// A light run inherits otag's environment (tools that need GOPATH, DOCKER_HOST, ...)
// without secrets and otag's own variables; a strict run inherits nothing. Inherited
// variables stay out of the run spec, so the adapters' scrub lists still apply.
func TestLightRunEnv(t *testing.T) {
	t.Setenv("GOPATH", "/go")
	t.Setenv("DOCKER_HOST", "unix:///d.sock")
	t.Setenv("SSH_AUTH_SOCK", "/agent.sock")
	t.Setenv("ANTHROPIC_BASE_URL", "http://gateway")
	t.Setenv("LITELLM_KEY", "lk")
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "NPM_TOKEN", "AWS_PROFILE", "OPENAI_API_KEY",
		"MY_SECRET", "DB_PASSWORD", "SMTP_PASSWD_FILE", "OTAG_DB", "OTAG_RUN_TOKEN"} {
		t.Setenv(k, "x")
	}
	m := &Manager{Getenv: os.Getenv}
	p := &plan{guard: guard.Light}
	p.rev.Profile.EnvPassthrough = []string{"LITELLM_KEY"}
	env := m.inheritedEnv(p)
	for _, k := range []string{"GOPATH", "DOCKER_HOST", "SSH_AUTH_SOCK", "ANTHROPIC_BASE_URL"} {
		if env[k] == "" {
			t.Errorf("light run lost %s", k)
		}
	}
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "NPM_TOKEN", "AWS_PROFILE", "OPENAI_API_KEY", "MY_SECRET",
		"DB_PASSWORD", "SMTP_PASSWD_FILE", "OTAG_DB", "OTAG_RUN_TOKEN", "PATH", "LITELLM_KEY"} {
		if _, ok := env[k]; ok {
			t.Errorf("light run inherited %s", k)
		}
	}
	// The secret-named passthrough still comes through runEnv.
	if m.runEnv(p)["LITELLM_KEY"] != "lk" {
		t.Error("passthrough lost")
	}
	if got := m.inheritedEnv(&plan{guard: guard.Strict}); len(got) != 0 {
		t.Errorf("strict run inherited %v", got)
	}

	// Inherited variables are not the run's: Claude's scrub list still drops a
	// gateway URL, which would move the run to another account.
	spec := m.runSpec(p, workspace.Result{Path: t.TempDir()})
	if _, ok := spec.Env["ANTHROPIC_BASE_URL"]; ok || spec.Guard != guard.Light {
		t.Fatalf("run spec: guard %q env %v", spec.Guard, spec.Env)
	}
	spec.RunDir, spec.OtagBin = t.TempDir(), "/opt/otag"
	ls, err := claude.New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(ls.Argv, " "), "-u ANTHROPIC_BASE_URL") {
		t.Fatalf("inherited ANTHROPIC_BASE_URL not scrubbed: %v", ls.Argv)
	}
}
