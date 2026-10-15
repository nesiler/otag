// Package registry loads the desired-state TOML (accounts, profiles, projects),
// validates it and syncs it into the database.
package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/guard"
	"github.com/nesiler/otag/internal/paths"
)

// Harnesses supported by otag.
var Harnesses = []string{"claude", "codex", "opencode"}

// FakeHarness is the scripted test/demo harness (internal/adapter/fake). It is valid in
// the registry but is not a CLI that doctor looks for.
const FakeHarness = "fake"

// ValidHarness reports whether h may be used by an account.
func ValidHarness(h string) bool { return h == FakeHarness || slices.Contains(Harnesses, h) }

var (
	nameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	prefixRe      = regexp.MustCompile(`^[a-z][a-z0-9]{0,9}$`)
	passthroughRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\*?$`)
)

// Account is a harness login/config directory; quota lives here.
type Account struct {
	Harness   string            `toml:"harness" json:"harness"`
	ConfigDir string            `toml:"config_dir" json:"config_dir,omitempty"`
	Command   string            `toml:"command" json:"command,omitempty"`
	Env       map[string]string `toml:"env" json:"env,omitempty"`
	// Quota tunes the quota collectors (internal/quota); nil = defaults.
	Quota *AccountQuota `toml:"quota" json:"quota,omitempty"`
}

// AccountQuota is the optional [accounts.<name>.quota] table.
type AccountQuota struct {
	Disabled bool `toml:"disabled" json:"disabled,omitempty"`
	// OAuthUsage (claude) polls the undocumented OAuth usage endpoint with the token
	// Claude Code keeps in the Keychain (read-only, never refreshed). Off by default.
	OAuthUsage bool `toml:"oauth_usage" json:"oauth_usage,omitempty"`
	// LiteLLMProvider (opencode) names the provider in opencode.json whose /key/info is
	// read; default "litellm" or the only provider mentioning LiteLLM.
	LiteLLMProvider string `toml:"litellm_provider" json:"litellm_provider,omitempty"`
}

// Profile is account + model + effort + env + mode, with an ordered fallback list.
type Profile struct {
	Account     string            `toml:"account" json:"account"`
	Model       string            `toml:"model" json:"model,omitempty"`
	Effort      string            `toml:"effort" json:"effort,omitempty"`
	Mode        string            `toml:"mode" json:"mode"`
	Tags        []string          `toml:"tags" json:"tags,omitempty"`
	Fallback    []string          `toml:"fallback" json:"fallback,omitempty"`
	Env         map[string]string `toml:"env" json:"env,omitempty"`
	Args        []string          `toml:"args" json:"args,omitempty"`
	Description string            `toml:"description" json:"description,omitempty"`
	// EnvPassthrough names variables of otag's own environment a run of this profile
	// gets (a trailing * matches a prefix), e.g. a LiteLLM key an OpenCode config
	// reads with {env:VAR}. Runs start with a clean environment otherwise.
	EnvPassthrough []string `toml:"env_passthrough" json:"env_passthrough,omitempty"`
}

// Project is one projects/<name>.toml file.
type Project struct {
	Name               string   `toml:"name"`
	Path               string   `toml:"path"`
	Prefix             string   `toml:"prefix"`
	CoordinatorProfile string   `toml:"coordinator_profile"`
	DefaultProfile     string   `toml:"default_profile"`
	ReviewProfile      string   `toml:"review_profile"`
	Workspace          string   `toml:"workspace"`
	Deny               []string `toml:"deny"`
	// Guard is the guard level of the project's runs: light (default) | strict
	// (internal/guard, docs/ADAPTERS.md "Threat model").
	Guard string `toml:"guard"`
	// Raw holds every key of the file, including extra ones, for the config column.
	Raw  map[string]any `toml:"-"`
	File string         `toml:"-"`
}

// Config is the full desired state.
type Config struct {
	Dir      string             `toml:"-"`
	Accounts map[string]Account `toml:"accounts"`
	Profiles map[string]Profile `toml:"profiles"`
	Projects []Project          `toml:"-"`
	// Warnings are non-fatal findings (unknown keys, missing project dirs).
	Warnings []string `toml:"-"`
}

// Load reads <dir>/registry.toml and <dir>/projects/*.toml, then validates.
func Load(dir string, getenv paths.Getenv) (*Config, error) {
	cfg := &Config{Dir: dir}
	regPath := filepath.Join(dir, "registry.toml")
	md, err := toml.DecodeFile(regPath, cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, apperr.New(apperr.ConfigInvalid, "%s not found", regPath)
		}
		return nil, apperr.New(apperr.ConfigInvalid, "%s: %v", regPath, err)
	}
	for _, k := range md.Undecoded() {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("registry.toml: unknown key %q", k.String()))
	}

	// A missing projects/ dir means no projects; any other read error must abort,
	// otherwise sync would retire every project.
	projDir := filepath.Join(dir, "projects")
	entries, err := os.ReadDir(projDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, apperr.New(apperr.ConfigInvalid, "read %s: %v", projDir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			files = append(files, filepath.Join(projDir, e.Name()))
		}
	}
	sort.Strings(files)
	for _, f := range files {
		p, err := loadProject(f)
		if err != nil {
			return nil, err
		}
		cfg.Projects = append(cfg.Projects, p)
	}
	cfg.normalize(getenv)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// A project inside otag's worktree root could work in a task's worktree (or under
	// it) while `otag task deliver` removes it.
	root := paths.WorktreeRoot(getenv)
	for _, p := range cfg.Projects {
		if paths.Within(p.Path, root) {
			return nil, apperr.New(apperr.ConfigInvalid, "project %s (%s): path %s is inside otag's worktree root %s",
				p.Name, filepath.Base(p.File), p.Path, root)
		}
	}
	return cfg, nil
}

// LoadProjects reads only <dir>/projects/*.toml (no validation against the
// registry): what `otag init --force` keeps while it replaces registry.toml.
func LoadProjects(dir string, getenv paths.Getenv) ([]Project, error) {
	files, err := filepath.Glob(filepath.Join(dir, "projects", "*.toml"))
	if err != nil {
		return nil, err
	}
	cfg := &Config{Dir: dir}
	for _, f := range files {
		p, err := loadProject(f)
		if err != nil {
			return nil, err
		}
		cfg.Projects = append(cfg.Projects, p)
	}
	cfg.normalize(getenv)
	return cfg.Projects, nil
}

func loadProject(file string) (Project, error) {
	var p Project
	b, err := os.ReadFile(file)
	if err != nil {
		return p, apperr.New(apperr.ConfigInvalid, "%v", err)
	}
	if _, err := toml.NewDecoder(bytes.NewReader(b)).Decode(&p); err != nil {
		return p, apperr.New(apperr.ConfigInvalid, "%s: %v", file, err)
	}
	if _, err := toml.NewDecoder(bytes.NewReader(b)).Decode(&p.Raw); err != nil {
		return p, apperr.New(apperr.ConfigInvalid, "%s: %v", file, err)
	}
	p.File = file
	if p.Name == "" {
		p.Name = strings.TrimSuffix(filepath.Base(file), ".toml")
	}
	return p, nil
}

func (c *Config) normalize(getenv paths.Getenv) {
	for name, a := range c.Accounts {
		a.ConfigDir = paths.ExpandHome(a.ConfigDir, getenv)
		a.Command = paths.ExpandHome(a.Command, getenv) // "~/bin/oc"; a bare name stays a PATH lookup
		c.Accounts[name] = a
	}
	for name, p := range c.Profiles {
		if p.Mode == "" {
			p.Mode = "interactive"
		}
		c.Profiles[name] = p
	}
	for i := range c.Projects {
		p := &c.Projects[i]
		p.Path = filepath.Clean(paths.ExpandHome(p.Path, getenv))
		if p.Workspace == "" {
			p.Workspace = "worktree"
		}
		if p.Guard == "" {
			p.Guard = string(guard.DefaultLevel)
		}
		if p.Raw == nil {
			p.Raw = map[string]any{}
		}
		p.Raw["name"], p.Raw["path"], p.Raw["workspace"], p.Raw["guard"] = p.Name, p.Path, p.Workspace, p.Guard
		if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
			c.Warnings = append(c.Warnings, fmt.Sprintf("project %s: path %s is not a directory", p.Name, p.Path))
		}
	}
}

// Validate checks references, enums and duplicates. Fallback lists are not
// transitive (a run only walks its own profile's list), so mutual fallbacks
// such as c1 <-> c2 are allowed.
func (c *Config) Validate() error {
	var probs []string
	bad := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	for _, name := range sortedKeys(c.Accounts) {
		a := c.Accounts[name]
		if !nameRe.MatchString(name) {
			bad("account %q: invalid name", name)
		}
		if !ValidHarness(a.Harness) {
			bad("account %s: harness %q must be one of %s", name, a.Harness, strings.Join(Harnesses, "|"))
		}
		if q := a.Quota; q != nil {
			if q.OAuthUsage && a.Harness != "claude" {
				bad("account %s: quota.oauth_usage is only for claude accounts", name)
			}
			if q.LiteLLMProvider != "" && a.Harness != "opencode" {
				bad("account %s: quota.litellm_provider is only for opencode accounts", name)
			}
		}
	}
	for _, name := range sortedKeys(c.Profiles) {
		p := c.Profiles[name]
		if !nameRe.MatchString(name) {
			bad("profile %q: invalid name", name)
		}
		if _, ok := c.Accounts[p.Account]; !ok {
			bad("profile %s: unknown account %q", name, p.Account)
		}
		if p.Mode != "interactive" && p.Mode != "headless" {
			bad("profile %s: mode %q must be interactive|headless", name, p.Mode)
		}
		for _, v := range p.EnvPassthrough {
			if !passthroughRe.MatchString(v) || v == "PATH" || strings.HasPrefix(v, "OTAG_") {
				bad("profile %s: env_passthrough %q is not a variable name (a trailing * matches a prefix; PATH and OTAG_* are otag's)", name, v)
			}
		}
		seen := map[string]bool{}
		for _, fb := range p.Fallback {
			switch {
			case fb == name:
				bad("profile %s: falls back to itself", name)
			case seen[fb]:
				bad("profile %s: duplicate fallback %q", name, fb)
			default:
				if _, ok := c.Profiles[fb]; !ok {
					bad("profile %s: unknown fallback profile %q", name, fb)
				}
			}
			seen[fb] = true
		}
	}

	names, prefixes := map[string]string{}, map[string]string{}
	for _, p := range c.Projects {
		where := fmt.Sprintf("project %s (%s)", p.Name, filepath.Base(p.File))
		if !nameRe.MatchString(p.Name) {
			bad("%s: invalid name", where)
		}
		if other, ok := names[p.Name]; ok {
			bad("%s: duplicate project name (also in %s)", where, other)
		}
		names[p.Name] = filepath.Base(p.File)
		if !filepath.IsAbs(p.Path) {
			bad("%s: path %q must be absolute", where, p.Path)
		}
		if !prefixRe.MatchString(p.Prefix) {
			bad("%s: prefix %q must match %s", where, p.Prefix, prefixRe)
		} else if other, ok := prefixes[p.Prefix]; ok {
			bad("%s: duplicate prefix %q (also used by %s)", where, p.Prefix, other)
		}
		prefixes[p.Prefix] = p.Name
		if p.Workspace != "worktree" && p.Workspace != "same" {
			bad("%s: workspace %q must be worktree|same", where, p.Workspace)
		}
		if !slices.Contains(guard.Levels, p.Guard) {
			bad("%s: guard %q must be %s", where, p.Guard, strings.Join(guard.Levels, "|"))
		}
		for field, ref := range map[string]string{
			"coordinator_profile": p.CoordinatorProfile,
			"default_profile":     p.DefaultProfile,
			"review_profile":      p.ReviewProfile,
		} {
			if _, ok := c.Profiles[ref]; ref != "" && !ok {
				bad("%s: %s references unknown profile %q", where, field, ref)
			}
		}
	}
	if len(probs) > 0 {
		sort.Strings(probs)
		return apperr.New(apperr.ConfigInvalid, "invalid configuration:\n  %s", strings.Join(probs, "\n  "))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hashJSON returns the JSON encoding of v and its sha256. Map keys are sorted by
// encoding/json, which makes the hash stable.
func hashJSON(v any) (string, string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:]), nil
}
