package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// Wrappers are only unwrapped up to the command word: options of the wrapped command
// are its own, and every value-taking wrapper option is skipped exactly.
func TestWrapperOptionsAndPipes(t *testing.T) {
	wd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(wd, "sub"), 0o755)
	p := Policy{Workdir: wd}
	for _, cmd := range []string{
		// false positives of the earlier whole-line scans
		`env CGO_ENABLED=0 make -C sub`, `env -i git -C sub status`, `git commit -m alias.fix`, `git commit -c HEAD`,
		`env -u HOME -- go test ./...`, `/usr/bin/env -i PATH=/bin sh -c 'go vet ./...'`,
		// wrappers around harmless commands
		`timeout 60 go test ./...`, `timeout -s KILL -k 5 60 make`, `nice -n 10 make`, `nice make`, `stdbuf -oL go test ./... | tee log.txt`,
		`nohup make`, `command -v git`, `exec -a name go test`, `xargs -d '\n' -n1 echo`, `xargs -0 -I {} echo {}`,
		`bash -o pipefail -c 'go test ./... | tee out.txt'`, `git log --oneline || true`, `echo hi | grep h`,
		`bash script.sh < input.txt`, `cat input.txt | bash script.sh`, `bash -c 'cat' <<< 'git push'`, `cat <<'EOF'` + "\ngit status\nEOF",
		`echo $(git status)`, `if true; then go test; fi`, `diff <(git show HEAD:a) a`,
	} {
		if d := p.CheckCommand(cmd); !d.Allow {
			t.Errorf("denied %q: %s", cmd, d.Reason)
		}
	}
	for _, cmd := range []string{
		// env options before the command word, including clusters
		`env -P /usr/bin git push`, `env -iP /usr/bin git push`, `env -u HOME git push`, `env -C /tmp touch x`, `env -iC/tmp touch x`,
		`env --chdir=/tmp touch x`, `env -iS 'git push'`, `env -a name git push`, `env - git push`,
		`exec -a x git push`, `builtin exec git push`, `command git push`, `nohup git push`,
		`timeout 5 git push`, `timeout -s KILL 5 git push`, `timeout --kill-after 1 5 git push`, `timeout --signal=TERM 5 git push`,
		`nice -n 5 git push`, `nice git push`, `nice --adjustment 5 git push`, `stdbuf -o L git push`, `stdbuf -oL -eL git push`,
		`sudo -u root git push`, `time -p git push`,
		// xargs options taking a separate value (GNU and BSD)
		`xargs -d '\n' git push`, `xargs -R 1 -I {} git push`, `xargs -S 255 -I {} git push`, `xargs -E eof git push`,
		`xargs -L 1 git push`, `xargs -tn 1 git push`, `xargs --delimiter , git push`, `xargs --replace git push`, `xargs -0 sh -c`,
		// push by another name
		`git send-pack origin main`, `git http-push x`, `git-send-pack x`, `/usr/libexec/git-core/git-push origin`, `git --config-env foo=BAR push`,
		`git -c alias.x=push x`,
		// scripts fed to a shell through stdin
		`echo 'git push' | bash`, `echo 'git push' | sh -s`, `printf 'git push' |& zsh`, `bash <<< 'git push'`, `sh < script.txt`,
		`cat x | env bash`, `cat x | sudo -u root sh -`, `echo x | sh -c 'bash'`, `echo x | env -S 'bash'`,
		`echo x | find . -maxdepth 0 -exec bash \;`, `cat x | bash -e`, `cat x | bash -o pipefail`,
		// compound commands and substitutions
		`! git push`, `{ git push; }`, `(git push)`, `if true; then git push; fi`, `for x in a; do git push; done`,
		`diff <(git push) x`, `echo $(git push)`, `bash -o pipefail -c 'git push'`,
	} {
		if d := p.CheckCommand(cmd); d.Allow {
			t.Errorf("allowed %q", cmd)
		}
	}
}
