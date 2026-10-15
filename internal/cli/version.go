package cli

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
)

// Build identifies the binary. The release build injects it with -ldflags
// (-X main.version, main.commit, main.date); ResolveBuild fills the gaps for
// builds without ldflags such as `go install`.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// ResolveBuild replaces unset build values (empty, "dev", "none", "unknown")
// with what runtime/debug.ReadBuildInfo knows: the module version of a
// `go install pkg@version` build and the VCS revision and time of a local build.
func ResolveBuild(b Build) Build {
	return resolveBuild(b, debug.ReadBuildInfo)
}

func resolveBuild(b Build, read func() (*debug.BuildInfo, bool)) Build {
	if b.Version == "" {
		b.Version = "dev"
	}
	if b.Commit == "" {
		b.Commit = "none"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	if b.Version != "dev" {
		return b // injected by the release build
	}
	info, ok := read()
	if !ok {
		return b
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		b.Version = strings.TrimPrefix(v, "v")
	}
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision" && b.Commit == "none" && s.Value != "":
			b.Commit = s.Value
			if len(b.Commit) > 12 {
				b.Commit = b.Commit[:12]
			}
		case s.Key == "vcs.time" && b.Date == "unknown" && s.Value != "":
			b.Date = s.Value
		}
	}
	return b
}

func (b Build) String() string {
	return fmt.Sprintf("otag %s (%s, %s)", b.Version, b.Commit, b.Date)
}

func cmdVersion(_ context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("version")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) > 0 {
		return result{}, usageErr("version takes no arguments")
	}
	// The binary resolves its build in main (ResolveBuild); only defaults here.
	b := resolveBuild(a.env.Build, func() (*debug.BuildInfo, bool) { return nil, false })
	return result{data: b, human: func(w io.Writer) { _, _ = fmt.Fprintln(w, b) }}, nil
}
