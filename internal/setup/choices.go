package setup

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nesiler/otag/internal/registry"
)

// Profile tiers: they order fallbacks and pick a project's default profiles.
const (
	TierSmart  = "smart"  // coordinator and hard tasks
	TierWorker = "worker" // the default for children
	TierFast   = "fast"   // quick, cheap tasks
)

// Account is a proposed account: one per harness, with no config_dir (each adapter
// then uses the harness's own default login: ~/.claude, ~/.codex or $CODEX_HOME, and
// OpenCode's normal ~/.config/opencode).
type Account struct {
	Name    string `json:"name"`
	Harness string `json:"harness"`
	On      bool   `json:"on"`
}

// Profile is a proposed profile.
type Profile struct {
	Name        string   `json:"name"`
	Harness     string   `json:"harness"`
	Account     string   `json:"account"`
	Model       string   `json:"model,omitempty"`  // "" = the harness's default model
	Effort      string   `json:"effort,omitempty"` // "" = the harness's default effort
	Tier        string   `json:"tier"`
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description,omitempty"`
	Fallback    []string `json:"fallback,omitempty"` // wired by Choices.Wire
	On          bool     `json:"on"`
}

// Project is a proposed projects/<name>.toml.
type Project struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Prefix      string   `json:"prefix"`
	Workspace   string   `json:"workspace"`
	Guard       string   `json:"guard"`
	Deny        []string `json:"deny"`
	Coordinator string   `json:"coordinator_profile,omitempty"`
	Default     string   `json:"default_profile,omitempty"`
	Review      string   `json:"review_profile,omitempty"`
}

// DefaultDeny is the deny list of a new project.
var DefaultDeny = []string{".env*", "*.pem"}

// Choices is what setup will write: the defaults (Base.Defaults), then the wizard's
// or the flags' edits.
type Choices struct {
	Accounts []Account `json:"accounts"` // one per harness offered
	Profiles []Profile `json:"profiles"` // the presets of every offered harness
	Project  *Project  `json:"project,omitempty"`
	Daemon   bool      `json:"daemon"` // install the launch agent
	Skill    bool      `json:"skill"`  // install the agent skill
}

// Base is what setup starts from.
type Base struct {
	ConfigDir string
	Home      string
	Detect    Detection
	// Existing is the registry kept on disk; new accounts and profiles are appended to
	// it. Nil: there is none, or Force replaces it.
	Existing *registry.Config
	// RegistryExists: registry.toml is on disk (with Force it is backed up and replaced).
	RegistryExists bool
	Force          bool
	// Known maps every project the database knows, retired ones too, to its prefix: a
	// new project never takes the prefix of another project's old task ids, and a
	// project added again gets its old one back.
	Known map[string]string
	// DaemonInstalled / SkillInstalled: offered again only when false.
	DaemonInstalled bool
	SkillInstalled  bool
}

// presets are the profiles proposed for each harness.
func presets(h, account string) []Profile {
	p := func(name, model, effort, tier, desc string, tags ...string) Profile {
		return Profile{Name: name, Harness: h, Account: account, Model: model, Effort: effort, Tier: tier,
			Tags: tags, Description: desc, On: true}
	}
	switch h {
	case "claude":
		return []Profile{
			p("claude-opus", "opus", "high", TierSmart, "Coordinator and hard tasks", "smart", "coordinator"),
			p("claude-sonnet", "sonnet", "", TierWorker, "Default worker", "worker"),
			p("claude-haiku", "haiku", "", TierFast, "Fast, simple tasks", "fast", "cheap"),
		}
	case "codex":
		return []Profile{
			p("codex", "", "high", TierSmart, "Hard tasks and reviews", "smart", "review"),
			p("codex-medium", "", "medium", TierWorker, "Everyday tasks at medium effort", "worker"),
		}
	case "opencode":
		return []Profile{p("opencode", "", "", TierWorker, "OpenCode with its default model", "worker")}
	}
	return nil
}

// Efforts are the effort values the wizard offers per harness ("" = the harness's
// default). OpenCode has no effort setting of its own.
func Efforts(h string) []string {
	switch h {
	case "claude":
		return []string{"", "low", "medium", "high", "xhigh", "max"}
	case "codex":
		return []string{"", "minimal", "low", "medium", "high", "xhigh"}
	}
	return nil
}

// Offered lists the harnesses setup proposes: those on PATH that no existing account
// uses.
func (b Base) Offered() []string {
	var out []string
	for _, h := range b.Detect.Found() {
		if !b.hasHarness(h) {
			out = append(out, h)
		}
	}
	return out
}

func (b Base) hasHarness(h string) bool {
	if b.Existing == nil {
		return false
	}
	for _, a := range b.Existing.Accounts {
		if a.Harness == h {
			return true
		}
	}
	return false
}

// RepoRegistered reports whether an existing project already uses dir.
func (b Base) RepoRegistered(dir string) bool {
	if b.Existing == nil || dir == "" {
		return false
	}
	for _, p := range b.Existing.Projects {
		if SamePath(p.Path, dir) {
			return true
		}
	}
	return false
}

// SamePath reports whether a and b name the same directory (symlinks resolved, case
// ignored as APFS does).
func SamePath(a, b string) bool {
	norm := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return strings.ToLower(filepath.Clean(p)) // APFS ignores case
	}
	return norm(a) == norm(b)
}

// Defaults are the proposed choices: every offered harness with its presets, the
// repository of the current directory as a project (when it is not registered yet),
// the daemon when it is not installed, no skill.
func (b Base) Defaults() Choices {
	var c Choices
	for _, h := range b.Offered() {
		name := b.uniqueAccount(h)
		c.Accounts = append(c.Accounts, Account{Name: name, Harness: h, On: true})
		for _, p := range presets(h, name) {
			p.Name = b.uniqueProfile(p.Name, c.Profiles)
			c.Profiles = append(c.Profiles, p)
		}
	}
	c.Wire(b)
	if b.Detect.Repo != "" && !b.RepoRegistered(b.Detect.Repo) {
		p := b.NewProject(b.Detect.Repo, c)
		c.Project = &p
	}
	c.Daemon = !b.DaemonInstalled
	return c
}

func (b Base) uniqueAccount(h string) string {
	return unique(h, func(n string) bool {
		_, ok := b.existingAccounts()[n]
		return ok
	})
}

func (b Base) uniqueProfile(name string, added []Profile) string {
	return unique(name, func(n string) bool {
		if _, ok := b.existingProfiles()[n]; ok {
			return true
		}
		return slices.ContainsFunc(added, func(p Profile) bool { return p.Name == n })
	})
}

func (b Base) existingAccounts() map[string]registry.Account {
	if b.Existing == nil {
		return nil
	}
	return b.Existing.Accounts
}

func (b Base) existingProfiles() map[string]registry.Profile {
	if b.Existing == nil {
		return nil
	}
	return b.Existing.Profiles
}

// unique is name, else name-2, name-3, ... (the first that is not taken).
func unique(name string, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", name, i); !taken(n) {
			return n
		}
	}
}

// AccountOn reports whether the account of harness h is chosen.
func (c Choices) AccountOn(h string) bool {
	for _, a := range c.Accounts {
		if a.Harness == h {
			return a.On
		}
	}
	return false
}

// Active lists the profiles that will be written: chosen, of a chosen account.
func (c Choices) Active() []Profile {
	var out []Profile
	for _, p := range c.Profiles {
		if p.On && c.AccountOn(p.Harness) {
			out = append(out, p)
		}
	}
	return out
}

// Ref is a profile a project or a fallback list may name.
type Ref struct {
	Name, Harness, Tier string
}

// Refs lists the profiles that exist after setup: the existing ones (sorted by name),
// then the active new ones.
func (b Base) Refs(c Choices) []Ref {
	var out []Ref
	if b.Existing != nil {
		for _, name := range sortedKeys(b.Existing.Profiles) {
			p := b.Existing.Profiles[name]
			out = append(out, Ref{Name: name, Harness: b.Existing.Accounts[p.Account].Harness, Tier: tierOf(p)})
		}
	}
	for _, p := range c.Active() {
		out = append(out, Ref{Name: p.Name, Harness: p.Harness, Tier: p.Tier})
	}
	return out
}

// tierOf guesses the tier of an existing profile from its tags.
func tierOf(p registry.Profile) string {
	switch {
	case slices.Contains(p.Tags, "coordinator") || slices.Contains(p.Tags, "smart"):
		return TierSmart
	case slices.Contains(p.Tags, "fast") || slices.Contains(p.Tags, "cheap"):
		return TierFast
	}
	return TierWorker
}

var tierRank = map[string]int{TierSmart: 0, TierWorker: 1, TierFast: 2}

// Wire sets the fallback list of every new profile: for each other harness, its
// profile of the nearest tier (smart, then the tier below), then the next lower tier
// of the profile's own harness. claude-opus falls back to codex, then claude-sonnet.
// Lists name only profiles that exist after setup; existing profiles keep theirs.
func (c *Choices) Wire(b Base) {
	refs := b.Refs(*c)
	harnesses := []string{}
	for _, r := range refs {
		if !slices.Contains(harnesses, r.Harness) {
			harnesses = append(harnesses, r.Harness)
		}
	}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		p.Fallback = nil
		if !p.On || !c.AccountOn(p.Harness) {
			continue
		}
		for _, h := range harnesses {
			if h == p.Harness {
				continue
			}
			if r, ok := nearest(refs, h, p.Tier, func(Ref) bool { return true }); ok {
				p.Fallback = append(p.Fallback, r.Name)
			}
		}
		// The same harness, one tier lower.
		if r, ok := nearest(refs, p.Harness, p.Tier, func(r Ref) bool {
			return r.Name != p.Name && tierRank[r.Tier] > tierRank[p.Tier]
		}); ok {
			p.Fallback = append(p.Fallback, r.Name)
		}
	}
}

// nearest is the profile of harness h whose tier is closest to tier (ties: the
// smarter one, then the first listed) among those ok accepts.
func nearest(refs []Ref, h, tier string, ok func(Ref) bool) (Ref, bool) {
	best, found := Ref{}, false
	dist := func(r Ref) int {
		d := tierRank[r.Tier] - tierRank[tier]
		if d < 0 {
			return -2*d + 1 // a smarter profile is a worse match than an equally distant cheaper one
		}
		return 2 * d
	}
	for _, r := range refs {
		if r.Harness != h || !ok(r) {
			continue
		}
		if !found || dist(r) < dist(best) {
			best, found = r, true
		}
	}
	return best, found
}

// NewProject proposes a project for the git checkout dir.
func (b Base) NewProject(dir string, c Choices) Project {
	name := b.uniqueProjectName(SanitizeName(filepath.Base(dir)))
	p := Project{Name: name, Path: dir, Prefix: b.UniquePrefix(name), Workspace: "worktree", Guard: "light",
		Deny: slices.Clone(DefaultDeny)}
	p.Coordinator, p.Default, p.Review = DefaultProfiles(b.Refs(c))
	return p
}

// DefaultProfiles picks a project's coordinator (a Claude profile, the smartest),
// default (a Claude worker, else any worker, else the first) and review profile (of
// another harness than the default when there is one; else another profile).
func DefaultProfiles(refs []Ref) (coord, def, review string) {
	claude := func(r Ref) bool { return r.Harness == "claude" }
	if r, ok := pick(refs, claude, TierSmart); ok {
		coord = r.Name
	}
	var d Ref
	var ok bool
	if d, ok = pick(refs, claude, TierWorker); !ok {
		if d, ok = pick(refs, func(Ref) bool { return true }, TierWorker); !ok {
			return coord, "", ""
		}
	}
	def = d.Name
	if r, ok := pick(refs, func(r Ref) bool { return r.Harness != d.Harness }, TierSmart); ok {
		review = r.Name
	} else if r, ok := pick(refs, func(r Ref) bool { return r.Name != d.Name }, TierSmart); ok {
		review = r.Name
	}
	return coord, def, review
}

// pick is the first ref ok accepts with the given tier, else the one nearest to it.
func pick(refs []Ref, ok func(Ref) bool, tier string) (Ref, bool) {
	best, found := Ref{}, false
	for _, r := range refs {
		if !ok(r) {
			continue
		}
		if !found || abs(tierRank[r.Tier]-tierRank[tier]) < abs(tierRank[best.Tier]-tierRank[tier]) {
			best, found = r, true
		}
	}
	return best, found
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

var (
	// The rules of internal/registry: names and prefixes.
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	prefixRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,9}$`)
	nonName  = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// ValidName reports whether s is a valid account, profile or project name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidPrefix reports whether s is a valid task id prefix.
func ValidPrefix(s string) bool { return prefixRe.MatchString(s) }

// SanitizeName turns a directory name into a project name: lower case, runs of other
// characters as "-", no leading or trailing punctuation ("project" when nothing is
// left).
func SanitizeName(s string) string {
	s = nonName.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-._")
	if s == "" {
		return "project"
	}
	return s
}

func (b Base) uniqueProjectName(name string) string {
	return unique(name, func(n string) bool {
		if b.Existing == nil {
			return false
		}
		return slices.ContainsFunc(b.Existing.Projects, func(p registry.Project) bool { return p.Name == n })
	})
}

// PrefixTaken reports whether a project other than name (in the registry, or any the
// database knows) uses prefix.
func (b Base) PrefixTaken(prefix, name string) bool {
	for n, p := range b.Known {
		if p == prefix && n != name {
			return true
		}
	}
	return b.Existing != nil && slices.ContainsFunc(b.Existing.Projects, func(p registry.Project) bool {
		return p.Prefix == prefix && p.Name != name
	})
}

// UniquePrefix proposes a short task id prefix for project name: its old prefix when
// the database knows the project, else the name itself when it has at most four
// letters and digits, else its first three, made longer (then numbered) until no
// other project uses it.
func (b Base) UniquePrefix(name string) string {
	if old, ok := b.Known[name]; ok && ValidPrefix(old) && !b.PrefixTaken(old, name) {
		return old
	}
	var s strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' && s.Len() > 0 {
			s.WriteRune(r)
		}
	}
	base := s.String()
	if base == "" {
		base = "p"
	}
	base = base[:min(len(base), 10)]
	var cands []string
	if len(base) <= 4 {
		cands = append(cands, base)
	}
	for n := min(3, len(base)); n <= len(base); n++ {
		cands = append(cands, base[:n])
	}
	for _, c := range cands {
		if !b.PrefixTaken(c, name) {
			return c
		}
	}
	stem := base[:min(len(base), 3)]
	for i := 2; ; i++ {
		if c := fmt.Sprintf("%s%d", stem, i); len(c) <= 10 && !b.PrefixTaken(c, name) {
			return c
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
