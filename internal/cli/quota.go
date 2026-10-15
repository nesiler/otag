package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/quota"
)

type quotaData struct {
	Snapshots []quota.Row       `json:"snapshots"`
	Cooldowns []quota.Cooldown  `json:"cooldowns"`
	Problems  map[string]string `json:"problems,omitempty"` // --refresh: per-account source problems
	Now       time.Time         `json:"now"`
}

// cmdQuota implements `otag quota [--refresh] [--account A] [--all]`.
func cmdQuota(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("quota")
	var refresh, all bool
	var account string
	fs.BoolVar(&refresh, "refresh", false, "run the collectors once before showing")
	fs.StringVar(&account, "account", "", "only this account")
	fs.BoolVar(&all, "all", false, "also snapshots older than 7 days")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	data := quotaData{}
	if refresh {
		c := quota.New(d, quota.Options{Getenv: a.env.Getenv, Account: account})
		if err := c.Collect(ctx); err != nil {
			return result{}, err
		}
		data.Problems = c.Problems()
	}
	data.Now = time.Now().UTC()
	f := quota.Filter{Account: account}
	if !all {
		f.MaxAge = 7 * 24 * time.Hour
	}
	if data.Snapshots, err = quota.List(ctx, d, f, data.Now); err != nil {
		return result{}, err
	}
	if data.Cooldowns, err = quota.Cooldowns(ctx, d, data.Now); err != nil {
		return result{}, err
	}
	if account != "" {
		data.Cooldowns = slices.DeleteFunc(data.Cooldowns, func(c quota.Cooldown) bool { return c.Account != account })
	}
	return result{data: data, human: func(w io.Writer) {
		for _, k := range sortedNames(data.Problems) {
			_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s: %s\n", k, data.Problems[k])
		}
		if len(data.Snapshots) == 0 && len(data.Cooldowns) == 0 {
			_, _ = fmt.Fprintln(w, "no quota data yet (see docs/QUOTA.md; `otag quota --refresh` reads Codex/LiteLLM now)")
			return
		}
		printQuota(w, data.Snapshots, data.Cooldowns, data.Now)
	}}, nil
}

func printQuota(w io.Writer, rows []quota.Row, cds []quota.Cooldown, now time.Time) {
	if len(rows) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ACCOUNT\tWINDOW\tUSED\tRESETS IN\tSOURCE\tAGE")
		for _, r := range rows {
			acct := r.Account
			if r.Profile != "" {
				acct += "/" + r.Profile
			}
			resets := "-"
			if r.ResetsAt != nil {
				if r.ResetPassed {
					resets = "passed"
				} else {
					resets = shortDur(r.ResetsAt.Sub(now))
				}
			}
			age := shortDur(time.Duration(r.AgeSeconds) * time.Second)
			if r.Stale {
				age += " (stale)"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", acct, r.Window, usedText(r), resets, r.Source, age)
		}
		_ = tw.Flush()
	}
	for _, c := range cds {
		who := c.Account
		if c.Profile != "" {
			who = "profile " + c.Profile
		}
		_, _ = fmt.Fprintf(w, "cooldown: %s until %s (in %s): %s\n", who, c.Until.Local().Format("2006-01-02 15:04"),
			shortDur(c.Until.Sub(now)), c.Reason)
	}
}

func usedText(r quota.Row) string {
	var s string
	switch {
	case r.UsedPercent != nil:
		s = fmt.Sprintf("%.0f%%", *r.UsedPercent)
	case r.Used != nil:
		s = fmt.Sprintf("$%.2f", *r.Used)
	default:
		s = "?"
	}
	if r.Used != nil && r.Limit != nil {
		s += fmt.Sprintf(" ($%.2f/$%.0f)", *r.Used, *r.Limit)
	}
	if r.ResetPassed && r.UsedPercent != nil {
		s = "0% (was " + s + ")"
	}
	return s
}

// shortDur prints 2d3h, 3h10m, 12m or 45s.
func shortDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// cmdStatusline implements the hidden `otag statusline --account A [--harness claude]
// [--config-dir D] [--command CMD]`: a statusLine command that records the quota in the
// payload, then runs the user's own statusLine with the same payload. It always exits 0
// and writes problems only to <data>/hook-errors.log (the harness renders stdout).
func cmdStatusline(ctx context.Context, a *app, args []string) int {
	fs := a.flagSet("statusline")
	var s quota.StatusLine
	fs.StringVar(&s.Account, "account", "", "registry account the payload belongs to")
	fs.StringVar(&s.Harness, "harness", "", "claude (the default)")
	fs.StringVar(&s.ConfigDir, "config-dir", "", "Claude config dir whose settings.json has the user's statusLine")
	fs.StringVar(&s.Command, "command", "", "the user's statusLine command to run (default: look it up)")
	logf := func(format string, args ...any) { a.fileLog("statusline: "+format, args...) }
	if _, err := parse(fs, args); err != nil {
		logf("%v", err)
		return ExitOK
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	readCtx, rc := context.WithTimeout(ctx, 2*time.Second)
	payload, err := readPayload(readCtx, a.env.Stdin)
	rc()
	if err != nil {
		logf("read payload: %v", err)
	}
	if s.ConfigDir != "" {
		s.ConfigDir = paths.ExpandHome(s.ConfigDir, a.env.Getenv)
	}
	s.Payload = payload
	s.DataDir = paths.DataDir(a.env.Getenv)
	s.Getenv = a.env.Getenv
	s.Env = os.Environ()
	s.Log = logf
	s.Stdout, s.Stderr = a.env.Stdout, io.Discard
	s.OpenDB = func(ctx context.Context) (*db.DB, error) {
		d, err := db.OpenWith(ctx, a.dbPath(), db.Options{BusyTimeout: 300 * time.Millisecond, NoMigrate: true})
		if err == nil {
			a.db = d
		}
		return d, err
	}
	s.Run(ctx)
	return ExitOK
}

// fileLog appends to <data>/hook-errors.log only (no stderr).
func (a *app) fileLog(format string, args ...any) {
	dir := paths.DataDir(a.env.Getenv)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "hook-errors.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s %s\n", db.Now(), fmt.Sprintf(format, args...))
	_ = f.Close()
}

// quotaSummary is the compact per-account quota line of `otag status`.
func quotaSummary(rows []quota.Row, now time.Time) []string {
	by := map[string][]string{}
	for _, r := range rows {
		if r.ResetPassed || (r.UsedPercent == nil && r.Used == nil) {
			continue
		}
		acct := r.Account
		if r.Profile != "" {
			acct += "/" + r.Profile
		}
		part := r.Window + " " + usedText(r)
		if r.ResetsAt != nil {
			part += " (resets " + shortDur(r.ResetsAt.Sub(now)) + ")"
		}
		if r.Stale {
			part += " stale"
		}
		by[acct] = append(by[acct], part)
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+": "+strings.Join(by[k], ", "))
	}
	return out
}
