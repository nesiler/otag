// Package workspace prepares the directory an agent works in: a git worktree on branch
// otag/<task> outside the repository (mode "worktree"), or the project itself ("same").
// Nothing is written into the project's working tree; `git worktree add` only adds
// metadata under the repository's .git directory.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Modes.
const (
	Worktree = "worktree"
	Same     = "same"
)

// Spec describes the workspace of a task.
type Spec struct {
	Mode        string
	ProjectPath string
	Path        string // worktree path (mode worktree)
	Branch      string // worktree branch (mode worktree)
}

// Result is a prepared workspace.
type Result struct {
	Mode    string `json:"mode"`
	Path    string `json:"path"`
	Branch  string `json:"branch,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	BaseSHA string `json:"base_sha,omitempty"`
	Reused  bool   `json:"reused"` // an existing worktree of the task was reused
	// BranchExisted: the branch predates this call (reused worktree, or recreated from a
	// surviving branch); BaseSHA is then not the task's base and the caller keeps the
	// recorded one (or MergeBase).
	BranchExisted bool `json:"branch_existed"`
}

// BranchFor returns the branch of a task.
func BranchFor(taskID string) string { return "otag/" + taskID }

// Prepare creates (or reuses) the workspace.
func Prepare(ctx context.Context, s Spec) (Result, error) {
	switch s.Mode {
	case Same:
		r := Result{Mode: Same, Path: s.ProjectPath}
		if isRepo(ctx, s.ProjectPath) {
			r.BaseRef, r.BaseSHA = head(ctx, s.ProjectPath)
		}
		return r, nil
	case Worktree:
	default:
		return Result{}, fmt.Errorf("unknown workspace mode %q", s.Mode)
	}
	if !isRepo(ctx, s.ProjectPath) {
		return Result{}, fmt.Errorf("project %s is not a git repository (use workspace = \"same\")", s.ProjectPath)
	}
	r := Result{Mode: Worktree, Path: s.Path, Branch: s.Branch}
	if _, err := os.Stat(s.Path); err == nil {
		if err := checkWorktree(ctx, s); err != nil {
			return Result{}, err
		}
		r.Reused, r.BranchExisted = true, true
		return r, nil
	}
	r.BaseRef, r.BaseSHA = head(ctx, s.ProjectPath)
	if r.BaseSHA == "" {
		return Result{}, fmt.Errorf("project %s has no commits", s.ProjectPath)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return Result{}, err
	}
	// Forget registrations of worktree dirs that were deleted by hand.
	if _, err := git(ctx, s.ProjectPath, "worktree", "prune"); err != nil {
		return Result{}, err
	}
	args := []string{"worktree", "add", "--quiet"}
	if _, err := git(ctx, s.ProjectPath, "show-ref", "--verify", "--quiet", "refs/heads/"+s.Branch); err == nil {
		args = append(args, s.Path, s.Branch) // branch survives from an earlier worktree
		r.BranchExisted = true
	} else {
		args = append(args, "-b", s.Branch, s.Path, r.BaseSHA)
	}
	if _, err := git(ctx, s.ProjectPath, args...); err != nil {
		return Result{}, err
	}
	return r, nil
}

// checkWorktree verifies that an existing path is the root of a linked worktree of the
// project (not the main checkout or a directory inside one) on the branch.
func checkWorktree(ctx context.Context, s Spec) error {
	if err := linkedRoot(ctx, s.ProjectPath, s.Path); err != nil {
		return err
	}
	branch, err := git(ctx, s.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("%s exists but is not a git worktree: %w", s.Path, err)
	}
	if branch != s.Branch {
		return fmt.Errorf("worktree %s is on branch %s, want %s", s.Path, branch, s.Branch)
	}
	return sameRepo(ctx, s.ProjectPath, s.Path)
}

// linkedRoot verifies that the existing dir is the root of a linked worktree of the
// repository at projectPath: listed by `git worktree list`, and not its main checkout.
func linkedRoot(ctx context.Context, projectPath, dir string) error {
	path := realpath(dir)
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%s exists but is not a git worktree: %w", dir, err)
	}
	if realpath(top) != path {
		return fmt.Errorf("%s is inside the checkout %s, not a worktree root", dir, top)
	}
	list, err := git(ctx, projectPath, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	linked := false
	for i, line := range strings.Split(list, "\n") {
		wt, ok := strings.CutPrefix(line, "worktree ")
		if ok && i > 0 && realpath(wt) == path { // the first entry is the main checkout
			linked = true
		}
	}
	if !linked {
		return fmt.Errorf("%s is not a linked worktree of %s", dir, projectPath)
	}
	return nil
}

// sameRepo verifies that dir belongs to the repository at projectPath.
func sameRepo(ctx context.Context, projectPath, dir string) error {
	want, err1 := commonDir(ctx, projectPath)
	got, err2 := commonDir(ctx, dir)
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	if want != got {
		return fmt.Errorf("worktree %s belongs to another repository (%s)", dir, got)
	}
	return nil
}

// DirtyError means a worktree has uncommitted changes (tracked or untracked files).
type DirtyError struct {
	Path   string
	Status string // `git status --porcelain`
}

func (e *DirtyError) Error() string {
	lines := strings.Split(e.Status, "\n")
	more := ""
	if len(lines) > 5 {
		lines, more = lines[:5], fmt.Sprintf(" (+%d more)", len(lines)-5)
	}
	return fmt.Sprintf("worktree %s has uncommitted changes: %s%s", e.Path, strings.Join(lines, "; "), more)
}

// Changes returns the uncommitted changes of the work tree at dir (`git status
// --porcelain`; "" when clean).
func Changes(ctx context.Context, dir string) (string, error) {
	return git(ctx, dir, "status", "--porcelain")
}

// RemoveSpec names the worktree of a task that is to be removed.
type RemoveSpec struct {
	ProjectPath string
	Path        string // the recorded worktree path
	Branch      string // the task's branch: the worktree must still be on it
	// Root is otag's worktree root of the project (<data>/worktrees/<project>): Path,
	// with symlinks resolved, must be inside it.
	Root  string
	Force bool // discard uncommitted changes
}

// Outcomes of Remove.
const (
	Removed = "removed"
	Gone    = "gone" // deleted before (pruned from git's list), or an empty directory left behind
	// Replaced: the path is now another linked worktree of the repository, on another
	// branch. It is not the task's any more and is left alone.
	Replaced = "replaced"
)

// Remove removes the worktree of a task (`git worktree remove`) and returns what it
// found: Removed, Gone or Replaced. Uncommitted changes are a *DirtyError unless
// Force, which discards them. A path that no longer exists is only pruned from git's
// list; an empty directory (what an interrupted removal leaves) is deleted first, as it
// has nothing to lose. A path outside Root, the main checkout, anything that is not a
// linked worktree of the repository, and a worktree with a detached HEAD are refused.
// The branch stays.
func Remove(ctx context.Context, s RemoveSpec) (string, error) {
	state, err := inspect(ctx, s)
	if err != nil {
		return "", err
	}
	switch state {
	case Gone:
		_, err := git(ctx, s.ProjectPath, "worktree", "prune")
		return Gone, err
	case stateEmpty:
		if err := os.Remove(s.Path); err != nil {
			return "", err
		}
		_, err := git(ctx, s.ProjectPath, "worktree", "prune")
		return Gone, err
	case Replaced:
		return Replaced, nil
	}
	st, err := Changes(ctx, s.Path)
	if err != nil {
		return "", err
	}
	args := []string{"worktree", "remove"}
	if st != "" {
		if !s.Force {
			return "", &DirtyError{Path: s.Path, Status: st}
		}
		args = append(args, "--force")
	}
	if _, err := git(ctx, s.ProjectPath, append(args, s.Path)...); err != nil {
		return "", err
	}
	return Removed, nil
}

// Uncommitted returns the uncommitted changes of the task worktree s ("" when it is
// clean, gone or not the task's any more); errors are Remove's refusals.
func Uncommitted(ctx context.Context, s RemoveSpec) (string, error) {
	state, err := inspect(ctx, s)
	if err != nil || state != stateOurs {
		return "", err
	}
	return Changes(ctx, s.Path)
}

const (
	stateEmpty = "empty"
	stateOurs  = "ours"
)

// inspect classifies the worktree path of s: Gone, stateEmpty, Replaced or stateOurs
// (a linked worktree of the repository on s.Branch), or refuses it.
func inspect(ctx context.Context, s RemoveSpec) (string, error) {
	if _, err := os.Lstat(s.Path); errors.Is(err, os.ErrNotExist) {
		return Gone, nil
	}
	real, err := filepath.EvalSymlinks(s.Path)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(s.Root)
	if err != nil {
		return "", fmt.Errorf("worktree root %s: %w", s.Root, err)
	}
	if rel, err := filepath.Rel(root, real); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside otag's worktree root %s; remove it by hand", s.Path, s.Root)
	}
	if entries, err := os.ReadDir(s.Path); err == nil && len(entries) == 0 {
		return stateEmpty, nil
	}
	list, err := worktreeList(ctx, s.ProjectPath)
	if err != nil {
		return "", err
	}
	for i, w := range list {
		if realpath(w.path) != real {
			continue
		}
		switch {
		case i == 0:
			return "", fmt.Errorf("%s is the main checkout of %s", s.Path, s.ProjectPath)
		case w.branch == s.Branch:
			return stateOurs, nil
		case w.branch == "":
			return "", fmt.Errorf("worktree %s has a detached HEAD, not branch %s; remove it by hand", s.Path, s.Branch)
		}
		return Replaced, nil
	}
	return "", fmt.Errorf("%s is not a linked worktree of %s", s.Path, s.ProjectPath)
}

// listedWorktree is an entry of `git worktree list --porcelain`.
type listedWorktree struct {
	path   string
	branch string // without refs/heads/; "" when detached (or bare)
}

// worktreeList lists the worktrees of the repository at projectPath, the main checkout
// first.
func worktreeList(ctx context.Context, projectPath string) ([]listedWorktree, error) {
	out, err := git(ctx, projectPath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []listedWorktree
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			list = append(list, listedWorktree{path: p})
		} else if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok && len(list) > 0 {
			list[len(list)-1].branch = b
		}
	}
	return list, nil
}

// DefaultBranch returns the branch work is merged into: the local branch origin/HEAD
// names, else the branch the project checkout is on ("" when detached).
func DefaultBranch(ctx context.Context, projectPath string) string {
	if ref, err := git(ctx, projectPath, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if _, name, ok := strings.Cut(ref, "/"); ok && BranchExists(ctx, projectPath, name) {
			return name
		}
	}
	ref, _ := git(ctx, projectPath, "symbolic-ref", "-q", "--short", "HEAD")
	return ref
}

// BranchExists reports whether the local branch exists in the repository at projectPath.
func BranchExists(ctx context.Context, projectPath, branch string) bool {
	_, err := git(ctx, projectPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Unmerged counts the commits of branch that target does not contain (0 = merged).
func Unmerged(ctx context.Context, projectPath, branch, target string) (int, error) {
	return unmerged(ctx, projectPath, "refs/heads/"+target+"..refs/heads/"+branch)
}

// unmerged counts the commits of the range a..b.
func unmerged(ctx context.Context, projectPath, rng string) (int, error) {
	out, err := git(ctx, projectPath, "rev-list", "--count", rng)
	if err != nil {
		return 0, err
	}
	var n int
	_, err = fmt.Sscan(out, &n)
	return n, err
}

// DeleteBranch deletes branch if target contains all of its commits (the semantics of
// `git branch -d`, against target instead of the checkout's HEAD). It returns the
// number of unmerged commits; with any, nothing is deleted. Both commits are read once:
// the deletion is one ref transaction that verifies target and deletes branch only if
// both still point at them, so a branch or target that moved meanwhile keeps the
// branch. A symbolic branch ref (it would delete what it points at) and a branch
// checked out in a worktree (the main checkout included) are refused.
func DeleteBranch(ctx context.Context, projectPath, branch, target string) (int, error) {
	if _, err := git(ctx, projectPath, "symbolic-ref", "-q", "refs/heads/"+branch); err == nil {
		return 0, fmt.Errorf("branch %s is a symbolic ref; not deleted", branch)
	}
	oid, err := git(ctx, projectPath, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return 0, err
	}
	targetOID, err := git(ctx, projectPath, "rev-parse", "--verify", "-q", "refs/heads/"+target)
	if err != nil {
		return 0, err
	}
	n, err := unmerged(ctx, projectPath, targetOID+".."+oid)
	if err != nil || n > 0 {
		return n, err
	}
	return 0, deleteBranchAt(ctx, projectPath, branch, oid, target, targetOID)
}

// deleteBranchAt deletes branch if it still points at oid, target still at targetOID,
// and no worktree has branch checked out.
func deleteBranchAt(ctx context.Context, projectPath, branch, oid, target, targetOID string) error {
	list, err := worktreeList(ctx, projectPath)
	if err != nil {
		return err
	}
	for _, w := range list {
		if w.branch == branch {
			return fmt.Errorf("branch %s is checked out in %s", branch, w.path)
		}
	}
	tx := fmt.Sprintf("start\nverify refs/heads/%s %s\ndelete refs/heads/%s %s\ncommit\n", target, targetOID, branch, oid)
	_, err = gitIn(ctx, projectPath, tx, "update-ref", "--no-deref", "--stdin")
	return err
}

func commonDir(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(out); err == nil {
		out = r
	}
	return out, nil
}

// Head returns the commit at HEAD of dir and whether the work tree has uncommitted changes.
func Head(ctx context.Context, dir string) (sha string, dirty bool, err error) {
	if sha, err = git(ctx, dir, "rev-parse", "HEAD"); err != nil {
		return "", false, err
	}
	st, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return sha, false, err
	}
	return sha, st != "", nil
}

// CommitsSince counts commits on HEAD of dir that are not in base.
func CommitsSince(ctx context.Context, dir, base string) (int, error) {
	if base == "" {
		return 0, nil
	}
	out, err := git(ctx, dir, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	var n int
	_, err = fmt.Sscan(out, &n)
	return n, err
}

// MergeBase returns the merge base of branch and the project's HEAD: the best guess of
// a task's base when none was recorded.
func MergeBase(ctx context.Context, projectPath, branch string) string {
	sha, _ := git(ctx, projectPath, "merge-base", branch, "HEAD")
	return sha
}

func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func isRepo(ctx context.Context, dir string) bool {
	_, err := git(ctx, dir, "rev-parse", "--git-dir")
	return err == nil
}

func head(ctx context.Context, dir string) (ref, sha string) {
	sha, _ = git(ctx, dir, "rev-parse", "--verify", "-q", "HEAD")
	ref, err := git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || ref == "" {
		ref = "HEAD"
	}
	return ref, sha
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitIn(ctx, dir, "", args...)
}

// gitIn is git with stdin.
func gitIn(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}
