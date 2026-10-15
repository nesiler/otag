package guard

import (
	"path/filepath"
	"testing"
)

func TestReviewShellWrappersAndPatch(t *testing.T) {
	p := Policy{Workdir: t.TempDir(), Deny: []string{".env*", "infra/prod/**"}}
	for _, cmd := range []string{
		`bash -lc 'git push'`, `sh -ec 'git push'`, `zsh -lic 'touch /etc/evil'`,
		`env -i git push`, `/usr/bin/env -u SECRET FOO=bar git push`, `env --unset=SECRET git push`, `env -S "git push"`, `env --split-string="git push"`,
		`git -c alias.p=push p`, `git -calias.p=push p`, `git --config-env=alias.p=ALIAS p`,
		`xargs git push`, `xargs -I{} -n 1 git push`, `find . -exec git push \;`, `find . -execdir touch ../evil \;`,
		"apply_patch <<'PATCH'\n*** Begin Patch\n   *** Add File: /etc/evil\n+x\n*** End Patch\nPATCH",
		"python -c 'ignored' # *** Begin Patch\n  *** Move to: .env.local",
	} {
		if d := p.CheckCommand(cmd); d.Allow {
			t.Errorf("allowed %q", cmd)
		}
	}
	for _, cmd := range []string{`bash -lc 'git status'`, `env -i git status`, `env -u SECRET FOO=bar touch ok`, `xargs -n 1 git status`, `find . -exec touch ok \;`, `git -c core.pager=cat status`} {
		if d := p.CheckCommand(cmd); !d.Allow {
			t.Errorf("denied %q: %+v", cmd, d)
		}
	}
	for _, header := range []string{"Add File:", "Update File:", "Delete File:", "Move to:"} {
		if d := p.CheckPatch("   *** " + header + " infra/prod/config"); d.Allow {
			t.Fatal(header)
		}
	}
	p.Cwd = "sub"
	if d := p.CheckPatch("*** Begin Patch\n*** Add File: ok"); !d.Allow {
		t.Fatal(d)
	}
	if d := p.CheckPatch("*** Begin Patch\n*** Add File: ../../evil"); d.Allow {
		t.Fatal(d)
	}

}
func TestUnknownWorkspaceNoOpinion(t *testing.T) {
	p := Policy{}
	if !p.CheckWrite(filepath.Join(t.TempDir(), "a")).Allow {
		t.Fatal("unknown root denied write")
	}
}
