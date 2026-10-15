package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckCommandRelativeCwd(t *testing.T) {
	wd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(wd, "infra", "prod"), 0o755)
	p := Policy{Workdir: wd, Deny: []string{"infra/prod/**"}}
	sub := p
	sub.Cwd = "infra/prod"
	abs := p
	abs.Cwd = filepath.Join(wd, "infra")
	for _, c := range []struct {
		p    Policy
		cmd  string
		want bool
	}{
		{p, "touch main.tf", true},
		{sub, "touch main.tf", false},          // the tool runs in infra/prod
		{abs, "touch prod/main.tf", false},     // absolute cwd
		{abs, "touch ../README.md", true},      // back inside, not denied
		{sub, "touch ../../../escape", false},  // outside the workspace
		{p, "cd infra/prod && touch x", false}, // cd moves the cwd
		{p, "cd infra; cd prod; echo x > y", false},
		{p, "cd infra && touch a", true},
		{p, "cd $DIR && touch a", false}, // cannot follow: relative writes denied
		{p, "cd ~ && rm -rf x", false},
		{p, "cd $DIR && touch /dev/null", true},
		{p, "cd $DIR && go test ./...", true}, // no write: still allowed
		{p, `bash -c "cd infra/prod; touch z"`, false},
	} {
		if got := c.p.CheckCommand(c.cmd).Allow; got != c.want {
			t.Errorf("cwd %q: CheckCommand(%q) = %v, want %v", c.p.Cwd, c.cmd, got, c.want)
		}
	}
	if !p.Inside("infra/prod") || !p.Inside(wd) || p.Inside("..") || p.Inside("/tmp") {
		t.Error("Inside")
	}
	// CheckWrite of file tools is unchanged: relative to Workdir.
	if !p.CheckWrite("main.tf").Allow || p.CheckWrite("infra/prod/x").Allow {
		t.Error("CheckWrite")
	}
}
