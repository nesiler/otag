package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/run"
)

// Config is the [daemon] section of <config>/otag.toml. Every key is optional.
type Config struct {
	Tick              time.Duration            // loop interval; default 3s
	DeadGrace         time.Duration            // a pane must stay dead this long before the run is reaped; default 5s
	StallAfter        time.Duration            // running with no event this long = stalled; default 20m
	StallAfterHarness map[string]time.Duration // per-harness override of StallAfter
	// ToolStallAfter replaces the stall limit while a tool call runs (more tool starts
	// than ends in the current turn): a long command or build emits no events; default 45m.
	ToolStallAfter time.Duration
	// StallPaneActivity counts output of the run's pane (tmux window_activity) as a
	// heartbeat. Off by default: a TUI that animates a spinner would never stall.
	StallPaneActivity bool
	UnansweredAfter   time.Duration // waiting this long = one "unanswered" notification; default 10m
	// LaunchTimeout ends a queued or starting run that has no pane recorded after this
	// long (its launcher was killed) as launch_failed; default 5m.
	LaunchTimeout  time.Duration
	MaxAttempts    int           // runs per fallback chain before the task is blocked; default 3
	QuotaThreshold float64       // quota used_percent that makes a profile unavailable; default 90
	QuotaInterval  time.Duration // how often the quota collectors run; default 60s
	FallbackOn     []string      // ended reasons that start a fallback run; default quota, launch_failed
	DesktopNotify  bool          // show notifications with osascript; default true
	// EnvPassthrough names extra environment variables `otag daemon install` copies
	// into the launch agent (API keys for OpenCode {env:VAR}, ...); a trailing * is a
	// prefix match. Read at install time only.
	EnvPassthrough []string
	// RawPayloads stores hook payloads themselves (secrets redacted, long strings
	// clipped) for debugging; off, raw_hook_payloads keeps a compact extract of ids and
	// kinds only. Read by the hook bridge on every hook.
	RawPayloads bool
	// RawPayloadRetention deletes raw_hook_payloads rows older than this; 0 keeps
	// them. Default 7 days.
	RawPayloadRetention time.Duration

	// [messages] section.

	// ParkAfter parks an idle run whose question has been unanswered this long (the
	// session closes, the answer resumes it); 0 = only `otag ask --park` parks. Default 30m.
	ParkAfter time.Duration
	// IdleAfter: a child or review run idle this long without an open question gets a
	// reminder (at most 2, then the human is told); 0 = off. Default 10m.
	IdleAfter time.Duration
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		Tick:            3 * time.Second,
		DeadGrace:       5 * time.Second,
		StallAfter:      20 * time.Minute,
		ToolStallAfter:  45 * time.Minute,
		UnansweredAfter: 10 * time.Minute,
		LaunchTimeout:   5 * time.Minute,
		MaxAttempts:     3,
		QuotaThreshold:  run.DefaultQuotaThreshold,
		QuotaInterval:   time.Minute,
		FallbackOn:      []string{run.ReasonQuota, run.ReasonLaunchFailed},
		DesktopNotify:   true,

		RawPayloadRetention: 7 * 24 * time.Hour,

		ParkAfter: 30 * time.Minute,
		IdleAfter: 10 * time.Minute,
	}
}

// stallAfter is the stall limit for a harness; inTool: a tool call is running.
func (c Config) stallAfter(harness string, inTool bool) time.Duration {
	d := c.StallAfter
	if h, ok := c.StallAfterHarness[harness]; ok && h > 0 {
		d = h
	}
	if inTool {
		d = max(d, c.ToolStallAfter)
	}
	return d
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\*?$`)

// duration decodes a TOML string such as "90s" or "10m".
type duration struct{ time.Duration }

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("negative duration %s", b)
	}
	d.Duration = v
	return nil
}

type fileConfig struct {
	Daemon struct {
		Tick              *duration           `toml:"tick"`
		DeadGrace         *duration           `toml:"dead_grace"`
		StallAfter        *duration           `toml:"stall_after"`
		StallAfterHarness map[string]duration `toml:"stall_after_harness"`
		ToolStallAfter    *duration           `toml:"tool_stall_after"`
		StallPaneActivity *bool               `toml:"stall_pane_activity"`
		UnansweredAfter   *duration           `toml:"unanswered_after"`
		LaunchTimeout     *duration           `toml:"launch_timeout"`
		MaxAttempts       *int                `toml:"max_attempts"`
		QuotaThreshold    *float64            `toml:"quota_threshold"`
		QuotaInterval     *duration           `toml:"quota_interval"`
		FallbackOn        *[]string           `toml:"fallback_on"`
		DesktopNotify     *bool               `toml:"desktop_notifications"`
		EnvPassthrough    *[]string           `toml:"env_passthrough"`
		RawPayloads       *bool               `toml:"raw_payloads"`
		RawRetention      *duration           `toml:"raw_payload_retention"`
	} `toml:"daemon"`
	// Messages holds the [messages] keys the daemon uses; the section's other keys
	// belong to the messaging step and the hook bridge.
	Messages struct {
		ParkAfter *duration `toml:"park_after"`
		IdleAfter *duration `toml:"idle_after"`
	} `toml:"messages"`
}

// ConfigFile is the daemon's config file in the config dir.
func ConfigFile(configDir string) string { return filepath.Join(configDir, "otag.toml") }

// LoadConfig reads the [daemon] section of path over the defaults; a missing file
// means defaults. Unknown keys under [daemon] are returned as warnings; other sections
// belong to other parts of otag.
func LoadConfig(path string) (Config, []string, error) {
	c := DefaultConfig()
	var fc fileConfig
	md, err := toml.DecodeFile(path, &fc)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil, nil
	}
	if err != nil {
		return c, nil, apperr.New(apperr.ConfigInvalid, "%s: %v", path, err)
	}
	var warnings []string
	for _, k := range md.Undecoded() {
		if len(k) > 0 && k[0] == "daemon" {
			warnings = append(warnings, fmt.Sprintf("%s: unknown key %q", filepath.Base(path), k.String()))
		}
	}
	f := fc.Daemon
	set := func(dst *time.Duration, src *duration) {
		if src != nil {
			*dst = src.Duration
		}
	}
	set(&c.Tick, f.Tick)
	set(&c.DeadGrace, f.DeadGrace)
	set(&c.StallAfter, f.StallAfter)
	set(&c.ToolStallAfter, f.ToolStallAfter)
	set(&c.UnansweredAfter, f.UnansweredAfter)
	set(&c.LaunchTimeout, f.LaunchTimeout)
	set(&c.QuotaInterval, f.QuotaInterval)
	set(&c.RawPayloadRetention, f.RawRetention)
	set(&c.ParkAfter, fc.Messages.ParkAfter)
	set(&c.IdleAfter, fc.Messages.IdleAfter)
	if f.RawPayloads != nil {
		c.RawPayloads = *f.RawPayloads
	}
	if len(f.StallAfterHarness) > 0 {
		c.StallAfterHarness = map[string]time.Duration{}
		for h, d := range f.StallAfterHarness {
			c.StallAfterHarness[h] = d.Duration
		}
	}
	if f.MaxAttempts != nil {
		c.MaxAttempts = *f.MaxAttempts
	}
	if f.QuotaThreshold != nil {
		c.QuotaThreshold = *f.QuotaThreshold
	}
	if f.FallbackOn != nil {
		c.FallbackOn = *f.FallbackOn
	}
	if f.DesktopNotify != nil {
		c.DesktopNotify = *f.DesktopNotify
	}
	if f.EnvPassthrough != nil {
		c.EnvPassthrough = *f.EnvPassthrough
	}
	if f.StallPaneActivity != nil {
		c.StallPaneActivity = *f.StallPaneActivity
	}
	return c, warnings, c.validate(path)
}

func (c Config) validate(path string) error {
	bad := func(format string, args ...any) error {
		return apperr.New(apperr.ConfigInvalid, "%s: [daemon] %s", path, fmt.Sprintf(format, args...))
	}
	switch {
	case c.Tick < 100*time.Millisecond:
		return bad("tick must be at least 100ms")
	case c.StallAfter <= 0 || c.UnansweredAfter <= 0 || c.ToolStallAfter <= 0:
		return bad("stall_after, tool_stall_after and unanswered_after must be positive")
	case c.LaunchTimeout < 30*time.Second:
		return bad("launch_timeout must be at least 30s")
	case c.QuotaInterval < time.Second:
		return bad("quota_interval must be at least 1s")
	case c.MaxAttempts < 1:
		return bad("max_attempts must be at least 1")
	case c.QuotaThreshold <= 0 || c.QuotaThreshold > 100:
		return bad("quota_threshold must be in (0, 100]")
	}
	for _, v := range c.EnvPassthrough {
		if !envName.MatchString(v) {
			return bad("env_passthrough: %q is not a variable name (a trailing * matches a prefix)", v)
		}
	}
	for _, r := range c.FallbackOn {
		if !slices.Contains([]string{run.ReasonQuota, run.ReasonLaunchFailed, run.ReasonStalled, run.ReasonCrashed}, r) {
			return bad("fallback_on: %q is not one of quota, launch_failed, stalled, crashed", r)
		}
	}
	return nil
}
