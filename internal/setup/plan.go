package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/registry"
)

// File is one file setup writes.
type File struct {
	Path   string `json:"path"`
	Action string `json:"action"`           // create | append | replace
	Backup string `json:"backup,omitempty"` // replace: the old file is copied here first
	// Content is the whole new file; Added is what it adds (append: the new block).
	Content []byte `json:"-"`
	Added   string `json:"-"`
}

// File actions.
const (
	Create  = "create"
	Append  = "append"
	Replace = "replace"
)

// RegistryFile is the path of registry.toml in dir.
func RegistryFile(dir string) string { return filepath.Join(dir, "registry.toml") }

// ProjectFile is the path of a project's TOML in dir.
func ProjectFile(dir, name string) string { return filepath.Join(dir, "projects", name+".toml") }

// LoadExisting loads the registry in dir. exists is false when registry.toml is not
// there (cfg is then nil and err nil).
func LoadExisting(dir string, getenv paths.Getenv) (cfg *registry.Config, exists bool, err error) {
	if _, err := os.Stat(RegistryFile(dir)); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	cfg, err = registry.Load(dir, getenv)
	return cfg, true, err
}

// Files lists what setup writes for c: registry.toml (created, replaced after a
// backup with Force, or appended to) and the project's file. It checks the choices;
// Preflight then loads the result as `otag sync` would.
func (b Base) Files(c Choices, now time.Time) ([]File, error) {
	var files []File
	reg := RegistryFile(b.ConfigDir)
	active := c.Active()
	switch {
	case !b.RegistryExists || b.Force:
		if len(active) == 0 {
			return nil, apperr.New(apperr.Invalid, "choose at least one agent profile")
		}
		f := File{Path: reg, Action: Create, Content: []byte(RenderRegistry(b, c, now))}
		f.Added = string(f.Content)
		if b.RegistryExists {
			f.Action, f.Backup = Replace, reg+".bak-"+now.Format("20060102-150405")
		}
		files = append(files, f)
	case len(active) > 0:
		old, err := os.ReadFile(reg)
		if err != nil {
			return nil, err
		}
		add := RenderAppend(b, c, now)
		body := string(old)
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		files = append(files, File{Path: reg, Action: Append, Content: []byte(body + add), Added: add})
	}
	if p := c.Project; p != nil {
		if err := b.CheckProject(*p, c); err != nil {
			return nil, err
		}
		path := ProjectFile(b.ConfigDir, p.Name)
		if _, err := os.Lstat(path); err == nil {
			return nil, apperr.New(apperr.Conflict, "%s already exists", path)
		}
		body := RenderProject(*p, b.Home, now)
		files = append(files, File{Path: path, Action: Create, Content: []byte(body), Added: body})
	}
	return files, nil
}

// CheckProject checks a proposed project's fields.
func (b Base) CheckProject(p Project, c Choices) error {
	var probs []string
	bad := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }
	switch {
	case !ValidName(p.Name):
		bad("name %q: use lower-case letters, digits, '.', '_' and '-'", p.Name)
	case b.Existing != nil && slices.ContainsFunc(b.Existing.Projects, func(e registry.Project) bool { return e.Name == p.Name }):
		bad("a project named %s already exists", p.Name)
	}
	switch {
	case !ValidPrefix(p.Prefix):
		bad("prefix %q: a lower-case letter, then up to 9 letters or digits", p.Prefix)
	case b.PrefixTaken(p.Prefix, p.Name):
		bad("prefix %q is used by another project", p.Prefix)
	}
	if !filepath.IsAbs(p.Path) {
		bad("path %q must be absolute", p.Path)
	} else if b.RepoRegistered(p.Path) {
		bad("%s is already registered", p.Path)
	}
	if p.Workspace != "worktree" && p.Workspace != "same" {
		bad("workspace %q must be worktree or same", p.Workspace)
	}
	if p.Guard != "light" && p.Guard != "strict" {
		bad("guard %q must be light or strict", p.Guard)
	}
	refs := b.Refs(c)
	for _, f := range []struct{ key, name string }{
		{"coordinator_profile", p.Coordinator}, {"default_profile", p.Default}, {"review_profile", p.Review},
	} {
		if f.name != "" && !slices.ContainsFunc(refs, func(r Ref) bool { return r.Name == f.name }) {
			bad("%s: no profile %q", f.key, f.name)
		}
	}
	if len(probs) > 0 {
		return apperr.New(apperr.Invalid, "%s", strings.Join(probs, "; "))
	}
	return nil
}

// Preflight loads the configuration as it will be after files are written (a copy in
// a temporary directory), so nothing is written that `otag sync` would refuse.
func Preflight(configDir string, files []File, getenv paths.Getenv) (*registry.Config, error) {
	tmp, err := os.MkdirTemp("", "otag-setup-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.MkdirAll(filepath.Join(tmp, "projects"), 0o700); err != nil {
		return nil, err
	}
	copyIn := func(from, to string) error {
		b, err := os.ReadFile(from)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o600)
	}
	if err := copyIn(RegistryFile(configDir), RegistryFile(tmp)); err != nil {
		return nil, err
	}
	existing, _ := filepath.Glob(filepath.Join(configDir, "projects", "*.toml"))
	for _, f := range existing {
		if err := copyIn(f, filepath.Join(tmp, "projects", filepath.Base(f))); err != nil {
			return nil, err
		}
	}
	for _, f := range files {
		rel, err := filepath.Rel(configDir, f.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("%s is outside %s", f.Path, configDir)
		}
		if err := os.WriteFile(filepath.Join(tmp, rel), f.Content, 0o600); err != nil {
			return nil, err
		}
	}
	cfg, err := registry.Load(tmp, getenv)
	if err != nil {
		// The messages name the temporary copy; name the real directory instead.
		if e, ok := apperr.As(err); ok {
			return nil, apperr.New(e.Code, "%s", strings.ReplaceAll(e.Message, tmp, configDir))
		}
		return nil, err
	}
	for i, w := range cfg.Warnings {
		cfg.Warnings[i] = strings.ReplaceAll(w, tmp, configDir)
	}
	cfg.Dir = configDir
	return cfg, nil
}

// Write writes files: the backup of a replaced file first (a new file, never
// overwritten), then each file through a temporary file and a rename, so a reader
// sees the old or the new content, never a part.
func Write(files []File) error {
	for _, f := range files {
		dir := filepath.Dir(f.Path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if f.Action == Create {
			if _, err := os.Lstat(f.Path); err == nil {
				return apperr.New(apperr.Conflict, "%s already exists", f.Path)
			}
		}
		if f.Backup != "" {
			if err := backup(f.Path, f.Backup); err != nil {
				return fmt.Errorf("back up %s: %w", f.Path, err)
			}
		}
		if err := WriteAtomic(f.Path, f.Content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func backup(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// WriteAtomic writes data to path through a temporary file in the same directory
// and a rename.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
