package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		glob, rel string
		want      bool
	}{
		{".env*", ".env", true},
		{".env*", "config/.env.local", true},
		{".env*", "src/env.go", false},
		{"*.pem", "certs/a.pem", true},
		{"infra/prod/**", "infra/prod/main.tf", true},
		{"infra/prod/**", "infra/prod", true},
		{"infra/prod/**", "x/infra/prod/main.tf", false},
		{"infra/prod", "infra/prod/a/b", true},
		{"/secrets/*.key", "secrets/a.key", true},
		{"**/fixtures/*.json", "a/b/fixtures/x.json", true},
		{"**/fixtures/*.json", "fixtures/x.json", true},
		{"docs/", "docs/readme.md", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := Match(c.glob, c.rel); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.glob, c.rel, got)
		}
	}
}

func TestCheckWrite(t *testing.T) {
	root := t.TempDir()
	wd := filepath.Join(root, "wt")
	outside := filepath.Join(root, "outside")
	_ = os.MkdirAll(filepath.Join(wd, "src"), 0o755)
	_ = os.MkdirAll(outside, 0o755)
	_ = os.Symlink(outside, filepath.Join(wd, "escape"))
	p := Policy{Workdir: wd, Deny: []string{".env*", "infra/prod/**"}, AllowOutside: []string{filepath.Join(root, "tmp")}}
	for target, want := range map[string]bool{
		"src/main.go":                         true,
		filepath.Join(wd, "new/dir/file.go"):  true,
		"../outside/x":                        false,
		filepath.Join(outside, "x"):           false,
		"escape/x":                            false, // symlink out of the workspace
		".env.local":                          false,
		"infra/prod/main.tf":                  false,
		".git/config":                         false,
		filepath.Join(root, "tmp", "scratch"): true,
		"/dev/null":                           true,
	} {
		if d := p.CheckWrite(target); d.Allow != want {
			t.Errorf("CheckWrite(%q) = %+v, want allow=%v", target, d, want)
		}
	}
}

func TestCheckCommand(t *testing.T) {
	wd := t.TempDir()
	p := Policy{Workdir: wd, Deny: []string{".env*"}, DenyCommands: []string{"npm publish*"}}
	for cmd, want := range map[string]bool{
		"go test ./...":                             true,
		"git commit -m 'x; git push'":               true,
		"git push origin main":                      false,
		"git -C ../other -c user.name=x push":       false,
		"FOO=1 git push":                            false,
		"make && git push --force":                  false,
		`bash -c "echo hi; git push"`:               false,
		"echo x > out.txt 2>&1":                     true,
		"echo x >> /tmp/../etc/hosts":               false,
		"echo x 2>/dev/null >/dev/null":             true,
		"cat a | tee -a .env":                       false,
		"rm -rf ../":                                false,
		"rm -rf build/":                             true,
		"cp README.md /usr/local/x":                 false,
		"ln -s /etc/passwd link":                    true,
		"npm publish --tag next":                    false,
		"echo 'npm publish'":                        true,
		"grep -r 'push' . &> log.txt":               true,
		"printf '%s\\n' a > \"" + wd + "/../evil\"": false,
	} {
		if d := p.CheckCommand(cmd); d.Allow != want {
			t.Errorf("CheckCommand(%q) = %+v, want allow=%v", cmd, d, want)
		}
	}
}

func TestCheckCommandCwd(t *testing.T) {
	wd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(wd, "sub"), 0o755)
	p := Policy{Workdir: wd}
	for cmd, want := range map[string]bool{
		"cd /tmp && touch otag-sentinel":           false,
		"cd sub && touch x && cd .. && touch y":    true,
		"cd sub; cd ../.. ; touch x":               false,
		"cd " + wd + "/sub && echo x > out":        true,
		"cd && touch x":                            false, // home
		"cd ~/somewhere && touch x":                false,
		"cd $SOMEWHERE && touch x":                 false, // unknown directory
		"cd $SOMEWHERE && touch " + wd + "/x":      true,  // absolute paths still work
		"cd - ; echo x > f":                        false,
		"touch ~/x":                                false,
		"echo x > $HOME/x":                         false,
		"echo x > \"$(pwd)/x\"":                    false,
		`bash -c "cd /tmp; rm -rf build"`:          false,
		"pushd /tmp && rm -f x":                    false,
		"cd sub && git push":                       false,
		"cd sub && go test ./... > /dev/null 2>&1": true,
	} {
		if d := p.CheckCommand(cmd); d.Allow != want {
			t.Errorf("CheckCommand(%q) = %+v, want allow=%v", cmd, d, want)
		}
	}
	// The harness's cwd is the starting point; the boundary stays the workspace.
	in := Policy{Workdir: wd, Cwd: filepath.Join(wd, "sub")}
	if d := in.CheckCommand("touch ../x"); !d.Allow {
		t.Errorf("cwd inside: %+v", d)
	}
	if d := in.CheckCommand("touch ../../x"); d.Allow {
		t.Error("cwd inside: escaped with ../..")
	}
	out := Policy{Workdir: wd, Cwd: os.TempDir()}
	if d := out.CheckCommand("touch x"); d.Allow {
		t.Error("cwd outside the workspace: relative write allowed")
	}
	if d := out.CheckWrite(filepath.Join(wd, "a")); !d.Allow {
		t.Errorf("absolute write inside with cwd outside: %+v", d)
	}
}
