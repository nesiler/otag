package workspace

import (
	"errors"
	"fmt"
	"strings"
)

// checkRefName applies the rules of `git check-ref-format` to a full ref name: no
// component starting with "." or ending with ".lock", no "..", "@{", ASCII control
// characters, space, ~ ^ : ? * [ or \, no empty component (so no "//" and no leading or
// trailing "/"), no trailing ".", and not "@".
func checkRefName(name string) error {
	switch {
	case name == "" || name == "@":
		return errors.New("empty or \"@\"")
	case strings.Contains(name, ".."):
		return errors.New(`contains ".."`)
	case strings.Contains(name, "@{"):
		return errors.New(`contains "@{"`)
	case strings.HasSuffix(name, "."):
		return errors.New(`ends with "."`)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return fmt.Errorf("contains %q", r)
		}
	}
	for _, c := range strings.Split(name, "/") {
		switch {
		case c == "":
			return errors.New("has an empty path component (a leading, trailing or double \"/\")")
		case strings.HasPrefix(c, "."):
			return fmt.Errorf("component %q starts with \".\"", c)
		case strings.HasSuffix(c, ".lock"):
			return fmt.Errorf("component %q ends with \".lock\"", c)
		}
	}
	return nil
}

// CheckBranchPrefix reports whether prefix makes valid branch names: prefix followed by
// a task id must pass `git check-ref-format --branch` ("" and "feature/" do,
// "feature//", "-x", "a..b/" and "x.lock/" do not).
func CheckBranchPrefix(prefix string) error {
	if strings.HasPrefix(prefix, "-") {
		return fmt.Errorf("branch prefix %q starts with \"-\"", prefix)
	}
	if strings.HasPrefix(prefix, "refs/") {
		return fmt.Errorf("branch prefix %q: give the branch name without refs/heads/", prefix)
	}
	if err := checkRefName("refs/heads/" + prefix + "x-1a2"); err != nil {
		return fmt.Errorf("branch prefix %q is not valid in a branch name: %v", prefix, err)
	}
	return nil
}

// CheckRef reports whether ref is a plausible ref or commit to hand to git as a base:
// a ref name `git check-ref-format` accepts (one level allowed: "main", "origin/dev",
// "refs/remotes/origin/dev"), a commit id, or HEAD; never something git would read as
// an option.
func CheckRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("ref %q starts with \"-\"", ref)
	}
	if ref == "HEAD" || isSHA(ref) {
		return nil
	}
	if err := checkRefName(ref); err != nil {
		return fmt.Errorf("ref %q: %v", ref, err)
	}
	return nil
}

// isSHA reports whether s looks like a (possibly abbreviated) commit id.
func isSHA(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
