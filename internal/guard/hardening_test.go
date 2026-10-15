package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// The bypasses an independent review found (git refs and aliases, commands hidden in
// substitutions, in-place writers, otag's own state, links), each refused; and the
// everyday forms next to them still allowed.
func TestHardeningProbes(t *testing.T) {
	root := t.TempDir()
	wd := filepath.Join(root, "wt")
	outside := filepath.Join(root, "outside")
	data := filepath.Join(root, "data")
	for _, d := range []string{wd, outside, data} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile(filepath.Join(outside, "file"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(wd, "source"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(wd, ".env"), []byte("SECRET=1"), 0o644)
	db := filepath.Join(data, "otag.db")
	_ = os.WriteFile(db, nil, 0o600)
	p := Policy{Workdir: wd, Deny: []string{".env*"}, Branch: "otag/x-1", DB: db, TmuxSocket: "otag",
		OtagBin: "/opt/bin/otag"}
	o := outside + "/file"
	for cmd, want := range map[string]bool{
		// git: aliases, command-running features, refs and worktrees
		"git config alias.p push && git p":       false,
		"git -c alias.p=push p":                  false,
		"git submodule foreach git push":         false,
		"git rebase --exec 'git push' main":      false,
		"git rebase -x 'make test' main":         false,
		"git rebase -i main":                     true,
		"git -C " + outside + " reset --hard":    false,
		"git --git-dir=" + outside + " log":      false,
		"git --work-tree " + outside + " status": false,
		"GIT_DIR=" + outside + " git log":        false,
		"git -C sub status":                      true,
		"git worktree remove --force ../other":   false,
		"git worktree prune":                     false,
		"git worktree list":                      true,
		"git branch -f main HEAD":                false,
		"git branch -D main":                     false,
		"git branch -M main old":                 false,
		"git branch -f otag/x-1 HEAD~1":          true,
		"git branch feature":                     true,
		"git branch -a":                          true,
		"git checkout -B main":                   false,
		"git switch -C otag/x-1":                 true,
		"git update-ref refs/heads/main HEAD":    false,
		"git config --global user.name x":        false,
		"git config user.name x":                 true,
		"git commit -m 'fix the push button'":    true,
		// hidden in eval, substitutions and shell payloads
		"eval 'git push'":                     false,
		"eval \"git $SUB origin\"":            true, // nothing to see
		"echo `git push`":                     false,
		"echo $(git push origin)":             false,
		"bash -c \"$(echo git push)\"":        false,
		"sh -c 'git update-ref refs/x HEAD'":  false,
		"echo 'costs $5; push it later'":      true,
		"python -c \"open('" + o + "','w')\"": true, // not parsed: the sandbox's job
		// in-place writers
		"sed -i '' s/a/b/ " + o:                        false,
		"sed -i.bak -e s/a/b/ " + o:                    false,
		"sed -i s/a/b/ source":                         true,
		"sed -n '/^x/p' " + o:                          true,
		"perl -pi -e s/a/b/ " + o:                      false,
		"perl -ne print " + o:                          true,
		"dd if=/dev/zero of=" + o:                      false,
		"curl -o " + o + " https://example.invalid":    false,
		"curl -sSLo " + o + " https://example.invalid": false,
		"curl -o out.txt https://example.invalid":      true,
		"wget -O " + o + " https://example.invalid":    false,
		"cp --target-directory=" + outside + " source": false,
		"cp -t " + outside + " source":                 false,
		"cp source copy":                               true,
		"tee " + o:                                     false,
		// links
		"ln -s " + outside + " link && echo x > link/file": false,
		"ln -s " + outside + " link2":                      true,
		"ln " + o + " hard && echo x > hard":               false,
		"ln .env alias":                                    false,
		// otag's own state
		"sqlite3 " + db + " 'update runs set status=1'": false,
		"sqlite3 $OTAG_DB .tables":                      false,
		"cp " + db + "-wal /tmp/x":                      false,
		"sqlite3 app.db .tables":                        true,
		"tmux -L otag send-keys -t %1 x":                false,
		"tmux send-keys -t %1 x":                        false,
		"tmux -L mine new -d":                           true,
		"otag --run other done x":                       false,
		"otag done 'did it'":                            true,
		"otag task me":                                  true,
		"otag task list":                                true,
		"otag task cancel x-2":                          false,
		"otag task deliver x-2 --delete-branch":         false,
		"otag task list --deliverable":                  true,
		"otag run stop x-1-r1":                          false,
		"otag daemon uninstall":                         false,
		"otag event claude PreToolUse":                  false,
		"/opt/bin/otag sync":                            false,
		// messaging: the CLI limits send and reply to the child's own scope
		"otag ask 'which db?'":     true,
		"otag inbox --wait":        true,
		"otag reply 42 'sqlite'":   true,
		"otag send coord 'fyi'":    true,
		"otag needs":               false,
		"otag coord alpha":         false,
		"otag review x-1 --accept": true, // the CLI allows it to review runs only (run.Review)
		"otag --run other inbox":   false,
		"otag run resume x-1":      false,
		// a child cannot shed its run identity to pass for the human
		"OTAG_RUN_ID= otag reply 42 x":               false,
		"OTAG_RUN_TOKEN=x otag send human hi":        false,
		"env -u OTAG_RUN_ID otag send human hi":      false,
		"env --unset=OTAG_RUN_ID otag send human hi": false,
		"env -i PATH=/bin otag send human hi":        false,
		"env - otag needs":                           false,
		"unset OTAG_RUN_ID; otag send human hi":      false,
		"unset OTAG_RUN_TOKEN":                       false,
		"export OTAG_RUN_ID=":                        false,
		"OTAG_RUN_ID=":                               false,
		"env FOO=1 otag inbox":                       true,
		"timeout 10 otag inbox --wait":               true,
		"unset FOO; otag inbox":                      true,
		// secrets
		"cat .env":            false,
		"grep SECRET .env":    false,
		"grep -r .env src":    true, // a pattern, not a file
		"wc -l < .env":        false,
		"cat README.md":       true,
		"cat " + outside:      true,
		"git push origin x":   false,
		"touch " + o:          false,
		"echo hi > notes.txt": true,
	} {
		if d := p.CheckCommand(cmd); d.Allow != want {
			t.Errorf("CheckCommand(%q) = %+v, want allow=%v", cmd, d, want)
		}
	}
}

// Links: a dangling symlink is judged by where it leads, a loop is refused, and a file
// with other hard links is not written.
func TestCheckWriteLinks(t *testing.T) {
	root := t.TempDir()
	wd := filepath.Join(root, "wt")
	outside := filepath.Join(root, "outside")
	_ = os.MkdirAll(wd, 0o755)
	_ = os.MkdirAll(outside, 0o755)
	_ = os.Symlink(filepath.Join(outside, "new"), filepath.Join(wd, "dangling"))
	_ = os.Symlink("../outside/rel", filepath.Join(wd, "relative"))
	_ = os.Symlink("inside-target", filepath.Join(wd, "inward"))
	_ = os.Symlink("loop-b", filepath.Join(wd, "loop-a"))
	_ = os.MkdirAll(filepath.Join(outside, "sub"), 0o755)
	_ = os.MkdirAll(filepath.Join(wd, "sub"), 0o755)
	_ = os.Symlink(filepath.Join(outside, "sub"), filepath.Join(wd, "outdir"))
	_ = os.Symlink("loop-a", filepath.Join(wd, "loop-b"))
	_ = os.WriteFile(filepath.Join(wd, "one"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(outside, "shared"), nil, 0o644)
	_ = os.Link(filepath.Join(outside, "shared"), filepath.Join(wd, "hard"))
	p := Policy{Workdir: wd, Deny: []string{".env*"}}
	for target, want := range map[string]bool{
		"dangling":            false,
		"relative":            false,
		"inward":              true,
		"loop-a":              false,
		"one":                 true,
		"hard":                false,
		"new/dir/x":           true,
		"dangling/..":         false, // the parent of the target, as the kernel resolves it
		"sub/../../wt/one":    true,
		"outdir/../file":      false, // link/.. is outside/, not the workspace
		"outdir/../../wt/one": true,  // ...and back in again
	} {
		if d := p.CheckWrite(target); d.Allow != want {
			t.Errorf("CheckWrite(%q) = %+v, want allow=%v", target, d, want)
		}
	}
	for _, cmd := range []string{"echo x > outdir/../file", "cd outdir/.. && touch file", "touch ./outdir/../file"} {
		if d := p.CheckCommand(cmd); d.Allow {
			t.Errorf("%q allowed: it writes outside through the link", cmd)
		}
	}
	if d := p.CheckRead("dangling"); !d.Allow {
		t.Errorf("read outside refused: %+v", d)
	}
	_ = os.Symlink(".env.local", filepath.Join(wd, "config"))
	if d := p.CheckRead("config"); d.Allow {
		t.Error("read of a deny glob through a symlink allowed")
	}
}

// Unknown (MCP) tools: object input only, paths checked as writes, commands as
// commands, and an executor whose command otag cannot see is refused.
func TestCheckToolArgs(t *testing.T) {
	wd := t.TempDir()
	p := Policy{Workdir: wd}
	for c, want := range map[[2]string]bool{
		{"mcp__fs__write_file", `{"path":"/outside/x","content":"y"}`}:             false,
		{"mcp__fs__write_file", `{"path":"` + wd + `/x"}`}:                         true,
		{"mcp__fs__write_file", `"just a string"`}:                                 false,
		{"mcp__fs__write_file", `[1,2]`}:                                           false,
		{"mcp__term__write_to_terminal", `{"command":"rm /outside/file"}`}:         false,
		{"mcp__term__write_to_terminal", `{"command":"git push origin main"}`}:     false,
		{"mcp__term__write_to_terminal", `{"command":"ls -la"}`}:                   true,
		{"mcp__proc__spawn", `{"cmd":["git","update-ref","refs/heads/main","x"]}`}: false,
		{"mcp__proc__spawn", `{"cmd":{"weird":true}}`}:                             false,
		{"mcp__runner__exec", `{"input":"opaque"}`}:                                false,
		{"mcp__web__fetch", `{"url":"https://example.com"}`}:                       true,
		{"mcp__web__fetch", ``}:                                                    true,
	} {
		if d := p.CheckToolArgs(c[0], []byte(c[1])); d.Allow != want {
			t.Errorf("CheckToolArgs(%s, %s) = %+v, want allow=%v", c[0], c[1], d, want)
		}
	}
}
