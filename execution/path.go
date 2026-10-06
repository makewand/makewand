package execution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/makewand/makewand/internal/privatefile"
)

// accountingPath pins the directory object, not its pathname. The sidecar lock,
// ledger, temporary replacement and directory sync must use this same root.
type accountingPath struct {
	root *os.Root
	name string
}

func (p *accountingPath) close() { _ = p.root.Close() }

func canonicalAccountingPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Preserve operator-selected aliases, including macOS /tmp and /var and
	// existing leaf aliases. No operation later revisits the original alias.
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(absolute)
	missing := []string{filepath.Base(absolute)}
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(parent) == parent {
			return "", err
		}
		missing = append(missing, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
}

func accountingName(name string) bool {
	return name != "." && filepath.IsLocal(name) && filepath.Base(name) == name
}

func openAccountingPath(path string, create bool) (*accountingPath, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(absolute)
	if !accountingName(name) {
		return nil, fmt.Errorf("execution accounting requires a file name")
	}
	volumeRoot := filepath.VolumeName(absolute) + string(filepath.Separator)
	parent, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(volumeRoot, filepath.Dir(absolute))
	if err != nil || !filepath.IsLocal(relative) {
		_ = parent.Close()
		return nil, fmt.Errorf("execution accounting directory is outside its volume")
	}
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if !accountingName(component) {
				_ = parent.Close()
				return nil, fmt.Errorf("invalid execution accounting directory component")
			}
			info, err := parent.Lstat(component)
			if errors.Is(err, os.ErrNotExist) && create {
				if mkdirErr := parent.Mkdir(component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
					_ = parent.Close()
					return nil, mkdirErr
				}
				info, err = parent.Lstat(component)
			}
			if err != nil {
				_ = parent.Close()
				return nil, err
			}
			if !info.IsDir() || accountingReparse(info) {
				_ = parent.Close()
				return nil, fmt.Errorf("execution accounting directory changed to a non-directory or symlink")
			}
			next, err := parent.OpenRoot(component)
			if err != nil {
				_ = parent.Close()
				return nil, err
			}
			opened, err := next.Stat(".")
			_ = parent.Close()
			if err != nil || !os.SameFile(info, opened) {
				_ = next.Close()
				return nil, fmt.Errorf("execution accounting directory changed while opening")
			}
			parent = next
		}
	}
	return &accountingPath{root: parent, name: name}, nil
}

// A resolved context remembers the parent identity without retaining a handle.
// Reopening it must not reinterpret a newly inserted alias or recreate a
// deleted directory. The returned root then pins that identity for the entire
// transaction, including replacement and directory fsync.
func accountingPathForIdentity(path string, expected os.FileInfo) (*accountingPath, os.FileInfo, error) {
	if expected == nil {
		canonical, err := canonicalAccountingPath(path)
		if err != nil {
			return nil, nil, err
		}
		path = canonical
	}
	p, err := openAccountingPath(path, expected == nil)
	if err != nil {
		return nil, nil, err
	}
	info, err := p.root.Stat(".")
	if err != nil || expected != nil && !os.SameFile(expected, info) {
		p.close()
		return nil, nil, fmt.Errorf("resolved execution accounting directory changed")
	}
	return p, info, nil
}

// openExecutionFile never truncates a file. Existing leaves are compared with
// the opened object before any write or permission change; new leaves use
// O_EXCL so a concurrent symlink cannot redirect creation.
func openExecutionFile(root *os.Root, name string, flags int) (*os.File, error) {
	if !accountingName(name) {
		return nil, fmt.Errorf("invalid execution accounting file name")
	}
	for range 8 {
		before, err := root.Lstat(name)
		create := errors.Is(err, os.ErrNotExist) && flags&os.O_CREATE != 0
		if err != nil && !create {
			return nil, err
		}
		if !create && (!before.Mode().IsRegular() || accountingReparse(before)) {
			return nil, fmt.Errorf("execution accounting requires regular files without symlinks")
		}
		openFlags := flags &^ (os.O_CREATE | os.O_EXCL | os.O_TRUNC)
		if create {
			openFlags |= os.O_CREATE | os.O_EXCL
		}
		file, err := root.OpenFile(name, accountingOpenFlags(openFlags), 0o600)
		if create && errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || accountingReparse(opened) || !create && !os.SameFile(before, opened) {
			_ = file.Close()
			return nil, fmt.Errorf("execution accounting file changed while opening")
		}
		// Existing Python sidecars/ledgers may inherit Windows DACLs. Tighten
		// validates owner and single-link identity on this fixed object before
		// changing privacy, preserving the shared file/byte-lock protocol.
		if err := privatefile.Tighten(file); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	return nil, fmt.Errorf("execution accounting file changed repeatedly while opening")
}
