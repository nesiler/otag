package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nesiler/otag/internal/apperr"
)

// The names otag gives a config file while it changes it. A crash can leave them
// behind; Leftovers finds them and Recover undoes the change.
const (
	// asideMark: registry.toml moved aside during a replace (.registry.toml.otag-old).
	asideMark = ".otag-old"
	// removingMark: a project file moved aside by `otag project remove` until its
	// project is retired (projects/.<name>.toml.otag-removing).
	removingMark = ".otag-removing"
	// tempMark: a new file's content before it is renamed into place; never the only
	// copy of anything.
	tempMark = ".otag-tmp-"
)

// AsideName is where a replace moves path while it puts the new file in place.
func AsideName(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+asideMark)
}

// RemovingName is where `otag project remove` keeps a project file until the
// project is retired.
func RemovingName(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+removingMark)
}

// Leftover is a file an interrupted otag change left aside.
type Leftover struct {
	Aside    string `json:"aside"`    // the file moved aside
	Original string `json:"original"` // where it belongs
	// Both: the original exists again too (written after the move): otag cannot tell
	// which one is wanted and changes nothing until one is deleted.
	Both bool `json:"both"`
}

// Leftovers lists the moved-aside files in the config dir and its projects dir.
// It only reads.
func Leftovers(dir string) ([]Leftover, error) {
	var out []Leftover
	for _, d := range []string{dir, filepath.Join(dir, "projects")} {
		entries, err := os.ReadDir(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, ".") || e.IsDir() {
				continue
			}
			var orig string
			for _, mark := range []string{asideMark, removingMark} {
				if base, ok := strings.CutSuffix(name[1:], mark); ok && base != "" {
					orig = base
				}
			}
			if orig == "" {
				continue
			}
			l := Leftover{Aside: filepath.Join(d, name), Original: filepath.Join(d, orig)}
			if _, err := os.Lstat(l.Original); err == nil {
				l.Both = true
			}
			out = append(out, l)
		}
	}
	return out, nil
}

// Recover undoes interrupted changes; the caller holds the config lock. A file moved
// aside whose original is missing goes back (a no-replace rename); the returned notes
// say which. When both exist it touches neither and refuses (CONFLICT, naming both):
// the user decides which one stays. Temporary files of unfinished writes are removed.
func Recover(dir string) ([]string, error) {
	ls, err := Leftovers(dir)
	if err != nil {
		return nil, err
	}
	var notes []string
	var both []string
	for _, l := range ls {
		if l.Both {
			both = append(both, fmt.Sprintf("%s and %s", l.Aside, l.Original))
			continue
		}
		if err := renameNoReplace(l.Aside, l.Original); err != nil {
			return notes, err
		}
		what := "an interrupted change"
		if strings.HasSuffix(l.Aside, removingMark) {
			what = "an interrupted `otag project remove` (run it again)"
		}
		notes = append(notes, fmt.Sprintf("restored %s from %s (%s)", l.Original, l.Aside, what))
	}
	for _, d := range []string{dir, filepath.Join(dir, "projects")} {
		tmps, _ := filepath.Glob(filepath.Join(d, ".*"+tempMark+"*"))
		for _, t := range tmps {
			_ = os.Remove(t)
		}
	}
	if len(both) > 0 {
		return notes, apperr.New(apperr.Conflict, "an interrupted otag change left two versions: %s. Keep the one you want "+
			"under the second name and delete the other; otag changes the config again only then", strings.Join(both, "; "))
	}
	return notes, nil
}

// errLeftover is the refusal when a file to move aside already has a leftover.
func errLeftover(aside, path string) error {
	return apperr.New(apperr.Conflict, "an interrupted otag change left %s next to %s: keep the version you want as %s "+
		"and delete the other", aside, path, path)
}
