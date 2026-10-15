// Command otag coordinates AI coding agents across projects and harnesses.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/nesiler/otag/internal/cli"
)

// Set by the release build (-ldflags "-X main.version=... -X main.commit=... -X main.date=...").
var version, commit, date = "dev", "none", "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	cwd, _ := os.Getwd()
	code := cli.Main(ctx, os.Args[1:], cli.Env{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv, Environ: os.Environ,
		Cwd:   cwd,
		Build: cli.ResolveBuild(cli.Build{Version: version, Commit: commit, Date: date}),
	})
	stop()
	os.Exit(code)
}
