package cli

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
)

// skillText is the Agent Skills file `otag skill install` writes.
//
//go:embed skill/SKILL.md
var skillText []byte

// Ownership is a content hash, not a parse of the file: next to SKILL.md otag writes a
// marker file holding the sha256 of the SKILL.md bytes it wrote.
//
//	owned    marker is a regular file and its hash equals the current SKILL.md
//	modified marker present, hash differs (edited since): install/uninstall need --force
//	foreign  no (or an unreadable) marker: never touched, not even with --force
const (
	skillMarkerFile = ".otag-managed"
	skillMarkerTag  = "otag-skill v1 sha256:"
)

func skillHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// parseSkillMarker returns the hash a marker file holds ("" = malformed).
func parseSkillMarker(b []byte) string {
	s := strings.TrimSpace(string(b))
	h, ok := strings.CutPrefix(s, skillMarkerTag)
	if !ok || len(h) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(h); err != nil {
		return ""
	}
	return h
}

// skillPaths are the places the skill lives under a home directory: the single copy
// (Codex and OpenCode read ~/.agents/skills), its marker, and the symlink Claude reads.
type skillPaths struct{ dir, file, marker, link string }

func skillPathsIn(home string) skillPaths {
	dir := filepath.Join(home, ".agents", "skills", "otag")
	return skillPaths{dir: dir, file: filepath.Join(dir, "SKILL.md"), marker: filepath.Join(dir, skillMarkerFile),
		link: filepath.Join(home, ".claude", "skills", "otag")}
}

// skillState describes a path: missing | ours | outdated (ours, older text) | modified |
// foreign (the symlink is only missing | ours | foreign).
type skillState struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

// fileState classifies the skill directory by its marker.
func (p skillPaths) fileState() (string, error) {
	st, err := os.Lstat(p.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if !st.IsDir() { // a symlink or a file is not ours
		return "foreign", nil
	}
	mst, err := os.Lstat(p.marker)
	if errors.Is(err, fs.ErrNotExist) {
		if ents, rerr := os.ReadDir(p.dir); rerr == nil && len(ents) == 0 {
			return "missing", nil // an empty directory holds nothing of anyone's
		}
		return "foreign", nil
	}
	if err != nil {
		return "", err
	}
	if !mst.Mode().IsRegular() { // never read through a symlink
		return "foreign", nil
	}
	mb, err := os.ReadFile(p.marker)
	if err != nil {
		return "", err
	}
	want := parseSkillMarker(mb)
	if want == "" {
		return "foreign", nil
	}
	fst, err := os.Lstat(p.file)
	if errors.Is(err, fs.ErrNotExist) {
		return "modified", nil // our marker, the file is gone: --force restores it
	}
	if err != nil {
		return "", err
	}
	if !fst.Mode().IsRegular() {
		return "foreign", nil // a symlink or directory is not something otag wrote
	}
	b, err := os.ReadFile(p.file)
	if err != nil {
		return "", err
	}
	switch {
	case skillHash(b) != want:
		return "modified", nil
	case string(b) == string(skillText):
		return "ours", nil
	}
	return "outdated", nil
}

// linkState classifies the Claude symlink.
func (p skillPaths) linkState() (string, error) {
	st, err := os.Lstat(p.link)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		if to, err := os.Readlink(p.link); err == nil && to == p.dir {
			return "ours", nil
		}
	}
	return "foreign", nil
}

func cmdSkill(ctx context.Context, a *app, args []string) (result, error) {
	if len(args) == 0 {
		return result{}, usageErr("usage: otag skill install|uninstall|status [--force] [--dir HOME]")
	}
	fs := a.flagSet("skill " + args[0])
	var home string
	var force bool
	fs.StringVar(&home, "dir", "", "home directory to use instead of $HOME (tests)")
	fs.BoolVar(&force, "force", false, "also replace or remove a skill file edited since otag wrote it")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return result{}, err
	}
	if len(pos) > 0 {
		return result{}, usageErr("usage: otag skill %s [--force] [--dir HOME]", args[0])
	}
	if home == "" {
		if home = a.env.Getenv("HOME"); home == "" {
			return result{}, usageErr("no home directory: set HOME or pass --dir")
		}
	}
	p := skillPathsIn(home)
	var act func(skillPaths, bool) ([]skillState, error)
	switch args[0] {
	case "install":
		act = skillInstall
	case "uninstall":
		act = skillUninstall
	case "status":
		act = func(p skillPaths, _ bool) ([]skillState, error) { return skillStatus(p) }
	default:
		return result{}, usageErr("unknown skill command %q (want install|uninstall|status)", args[0])
	}
	states, err := act(p, force)
	if err != nil {
		return result{}, err
	}
	return result{data: states, human: func(w io.Writer) {
		for _, s := range states {
			_, _ = fmt.Fprintf(w, "%-9s %s\n", s.State, s.Path)
		}
	}}, nil
}

func skillStatus(p skillPaths) ([]skillState, error) {
	f, err := p.fileState()
	if err != nil {
		return nil, err
	}
	l, err := p.linkState()
	if err != nil {
		return nil, err
	}
	return []skillState{{p.file, f}, {p.link, l}}, nil
}

// writeSkillFile writes data to dir/name through an exclusive random temp file and a
// rename: nothing at a predictable name is truncated or followed.
func writeSkillFile(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// refuseSkill returns the CONFLICT for a path otag may not change in state st.
func refuseSkill(path, st string, force bool) error {
	switch {
	case st == "foreign":
		return apperr.New(apperr.Conflict, "%s exists and is not otag's skill; not touching it (--force does not apply)", path)
	case st == "modified" && !force:
		return apperr.New(apperr.Conflict, "%s was edited since otag wrote it; --force replaces or removes it", path)
	}
	return nil
}

// skillInstall writes the skill, its marker and the symlink; it changes nothing when
// either path holds something that is not otag's (or edited, without force).
func skillInstall(p skillPaths, force bool) ([]skillState, error) {
	f, err := p.fileState()
	if err != nil {
		return nil, err
	}
	l, err := p.linkState()
	if err != nil {
		return nil, err
	}
	if err := refuseSkill(p.dir, f, force); err != nil {
		return nil, err
	}
	if err := refuseSkill(p.link, l, force); err != nil {
		return nil, err
	}
	if f != "ours" {
		if err := os.MkdirAll(p.dir, 0o755); err != nil {
			return nil, err
		}
		// Marker first: a failure between the two leaves a valid marker with a missing
		// or different SKILL.md (modified, recoverable with --force), never a
		// marker-less file otag could not claim again.
		if err := writeSkillFile(p.dir, skillMarkerFile, []byte(skillMarkerTag+skillHash(skillText)+"\n")); err != nil {
			return nil, err
		}
		if err := writeSkillFile(p.dir, "SKILL.md", skillText); err != nil {
			return nil, err
		}
	}
	if l == "missing" {
		if err := os.MkdirAll(filepath.Dir(p.link), 0o755); err != nil {
			return nil, err
		}
		if err := os.Symlink(p.dir, p.link); err != nil {
			return nil, err
		}
	}
	return skillStatus(p)
}

// skillUninstall removes only what otag created: the file and marker it wrote (an
// edited file only with force), the directory if then empty, and the symlink if it
// points at that directory.
func skillUninstall(p skillPaths, force bool) ([]skillState, error) {
	f, err := p.fileState()
	if err != nil {
		return nil, err
	}
	if f == "foreign" || f == "modified" {
		if err := refuseSkill(p.dir, f, force); err != nil {
			return nil, err
		}
	}
	l, err := p.linkState()
	if err != nil {
		return nil, err
	}
	if l == "ours" {
		if err := os.Remove(p.link); err != nil {
			return nil, err
		}
	}
	if f != "missing" {
		for _, path := range []string{p.file, p.marker} {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		_ = os.Remove(p.dir) // only when empty
	}
	return skillStatus(p)
}
