package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// PushBlock is the git config, given through the environment, that makes every push
// of a run fail inside git itself (internal/run sets it on every run): an empty
// pushInsteadOf prefix rewrites every push URL to the otag-blocked scheme, which may
// never be used. Push removes it again for the one push otag makes itself.
var PushBlock = [][2]string{
	{"url.otag-blocked://.pushInsteadOf", ""},
	{"protocol.otag-blocked.allow", "never"},
}

// PushSpec is one push of a task branch.
type PushSpec struct {
	ProjectPath string
	Remote      string
	Branch      string // the local branch; pushed to the remote branch of the same name
	// Lease, when set, forces the push with --force-with-lease: the remote branch must
	// still be at *Lease ("" = it must not exist). Nil is a plain (fast-forward) push.
	Lease *string
}

// PushResult is a successful push.
type PushResult struct {
	SHA    string `json:"sha"`    // the commit the remote branch now has
	Output string `json:"output"` // git's porcelain report
}

// PushRejected means the remote refused the update: not a fast-forward, or a lease
// that no longer holds. Nothing changed on the remote.
type PushRejected struct{ Output string }

func (e *PushRejected) Error() string {
	return "the remote rejected the push: " + oneLineOf(e.Output)
}

// Push pushes the commit the local branch is at to refs/heads/<branch> of the remote
// (`git push <remote> <sha>:refs/heads/<branch>`): never another ref, no tags, no
// submodules. The commit is read first, so the pushed commit is exactly the one
// returned even when the branch moves meanwhile. The project's hooks run; otag's push
// block (PushBlock) is left out of git's environment.
func Push(ctx context.Context, s PushSpec) (PushResult, error) {
	if err := CheckRef(s.Remote); err != nil {
		return PushResult{}, fmt.Errorf("remote: %w", err)
	}
	sha, err := BranchSHA(ctx, s.ProjectPath, s.Branch)
	if err != nil {
		return PushResult{}, err
	}
	args := []string{"push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no"}
	if s.Lease != nil {
		args = append(args, "--force-with-lease=refs/heads/"+s.Branch+":"+*s.Lease)
	}
	args = append(args, "--", s.Remote, sha+":refs/heads/"+s.Branch)
	out, errOut, err := gitEnv(ctx, s.ProjectPath, pushEnv(os.Environ()), args...)
	if err != nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "!") {
				return PushResult{}, &PushRejected{Output: strings.TrimSpace(out + "\n" + errOut)}
			}
		}
		msg := strings.TrimSpace(errOut)
		if msg == "" {
			msg = err.Error()
		}
		return PushResult{}, fmt.Errorf("git push %s %s: %s", s.Remote, s.Branch, msg)
	}
	return PushResult{SHA: sha, Output: strings.TrimSpace(out)}, nil
}

// BranchSHA returns the commit the local branch points at.
func BranchSHA(ctx context.Context, projectPath, branch string) (string, error) {
	sha, err := git(ctx, projectPath, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}")
	if err != nil || sha == "" {
		return "", fmt.Errorf("branch %s does not exist in %s", branch, projectPath)
	}
	return sha, nil
}

// RemoteBranch returns the commit the remote's branch is at ("" when it has no such
// branch); it asks the remote (`git ls-remote`).
func RemoteBranch(ctx context.Context, projectPath, remote, branch string) (string, error) {
	if err := CheckRef(remote); err != nil {
		return "", fmt.Errorf("remote: %w", err)
	}
	out, errOut, err := gitEnv(ctx, projectPath, pushEnv(os.Environ()), "ls-remote", "--heads", "--", remote,
		"refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s: %s", remote, strings.TrimSpace(errOut))
	}
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(line, "\t"); ok && ref == "refs/heads/"+branch {
			return sha, nil
		}
	}
	return "", nil
}

// pushEnv returns env without otag's push block: the GIT_CONFIG_KEY_n/VALUE_n pairs
// of PushBlock are dropped and the rest renumbered. Anything it cannot read is left as
// it is (git then refuses the push, which is the safe side).
func pushEnv(env []string) []string {
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	n, err := strconv.Atoi(vars["GIT_CONFIG_COUNT"])
	if err != nil || n <= 0 {
		return env
	}
	blocked := func(k, v string) bool {
		for _, b := range PushBlock {
			if strings.EqualFold(k, b[0]) && v == b[1] {
				return true
			}
		}
		return false
	}
	var keep [][2]string
	for i := range n {
		k, v := vars["GIT_CONFIG_KEY_"+strconv.Itoa(i)], vars["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
		if !blocked(k, v) {
			keep = append(keep, [2]string{k, v})
		}
	}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_COUNT" || strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	for i, kv := range keep {
		out = append(out, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	if len(keep) > 0 {
		out = append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(len(keep)))
	}
	return out
}

// gitEnv runs git in dir with the environment env and returns stdout and stderr.
func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return strings.TrimSpace(out.String()), errb.String(), err
}

func oneLineOf(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
