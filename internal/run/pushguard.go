package run

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/nesiler/otag/internal/workspace"
)

// pushBlock is git config, given through the environment, that makes every push of a
// run fail inside git itself, whatever spelling, script or tool started it: an empty
// pushInsteadOf prefix rewrites every push URL to the otag-blocked scheme, and that
// scheme may never be used. Fetch URLs are not rewritten. It is a backstop behind the
// guard (both levels), not a boundary: an explicit remote.<name>.pushurl, or a longer
// matching pushInsteadOf in the user's config, wins over it. No hook and no
// core.hooksPath is set: the project's own hooks keep working. `otag task push`, the
// only push otag makes, leaves it out of its own git (workspace.Push).
var pushBlock = workspace.PushBlock

// gitConfigEnvVersion is the first git that reads GIT_CONFIG_COUNT/KEY_n/VALUE_n.
const gitConfigEnvVersion = 2031

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// gitVersion returns git's version as major*1000+minor, 0 when unknown; a variable
// for tests.
var gitVersion = func(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "version").Output()
	if err != nil {
		return 0
	}
	m := gitVersionRe.FindSubmatch(out)
	if m == nil {
		return 0
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	return major*1000 + minor
}

// addPushBlock appends pushBlock to the GIT_CONFIG_* slots env already has (from the
// inherited environment, a passthrough or the account/profile env), never replacing
// them. It does nothing when git is older than 2.31 or the existing count is not a
// number (git would refuse the environment either way).
func addPushBlock(ctx context.Context, env map[string]string) {
	n := 0
	if c, ok := env["GIT_CONFIG_COUNT"]; ok && c != "" {
		var err error
		if n, err = strconv.Atoi(c); err != nil || n < 0 {
			return
		}
	}
	if gitVersion(ctx) < gitConfigEnvVersion {
		return
	}
	for _, kv := range pushBlock {
		i := strconv.Itoa(n)
		env["GIT_CONFIG_KEY_"+i], env["GIT_CONFIG_VALUE_"+i] = kv[0], kv[1]
		n++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(n)
}
