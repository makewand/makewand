package engine

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Protected paths (see isProtectedWritePath: .git at any depth, CI
// definitions, Makefile, scripts/*.sh, .makewand/rules.md) must not change
// while a restricted plan runs in the workspace. Generated file writes are
// already refused for them; this closes the same gap for code the plans
// execute (tests, setup scripts, build scripts):
//
//   - existing protected entries are bound read-only inside the sandbox, so
//     ordinary writes fail outright;
//   - before and after every run the protected set is snapshotted, and any
//     difference (a new protected file, a rewritten or deleted one - also via
//     tricks the read-only binds cannot stop, such as renaming a parent
//     directory) is rolled back and the run is reported as failed.

// ErrProtectedPathsModified is wrapped by the error RunVerificationPlan returns
// when an executed command changed protected paths.
var ErrProtectedPathsModified = errors.New("command modified protected paths")

type protectedPathEntry struct {
	mode       fs.FileMode // full mode: type bits + permissions
	size       int64
	sum        [sha256.Size]byte
	content    []byte // regular files up to maxReadFileSize
	link       string // symlink target
	opaque     bool   // .git directories: tracked by identity only, never descended
	restorable bool
}

func (e protectedPathEntry) equal(o protectedPathEntry) bool {
	return e.mode == o.mode && e.size == o.size && e.sum == o.sum && e.link == o.link && e.opaque == o.opaque
}

type protectedPathSnapshot struct {
	root    string
	entries map[string]protectedPathEntry // slash-separated workspace-relative paths
}

// snapshotProtectedPaths records every protected path currently present in the
// workspace rooted at root.
func snapshotProtectedPaths(root string) (*protectedPathSnapshot, error) {
	tree, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	snap := &protectedPathSnapshot{root: root, entries: map[string]protectedPathEntry{}}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		name := d.Name()
		if d.IsDir() && strings.EqualFold(name, ".git") {
			info, err := d.Info()
			if err != nil {
				return err
			}
			snap.entries[filepath.ToSlash(rel)] = protectedPathEntry{mode: info.Mode(), opaque: true}
			return filepath.SkipDir
		}
		if d.IsDir() && (shouldIgnoreSet[name] || strings.EqualFold(name, ".makewand")) {
			if strings.EqualFold(rel, ".makewand") {
				if err := snap.record(tree, ".makewand/rules.md"); err != nil {
					return err
				}
			}
			return filepath.SkipDir
		}
		if isProtectedWritePath(rel) {
			return snap.record(tree, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// record captures one protected path (missing paths are simply not recorded).
func (s *protectedPathSnapshot) record(tree *os.Root, rel string) error {
	native := filepath.FromSlash(rel)
	info, err := tree.Lstat(native)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect protected path %s: %w", rel, err)
	}
	entry := protectedPathEntry{mode: info.Mode(), restorable: true}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		entry.link, err = tree.Readlink(native)
		if err != nil {
			return fmt.Errorf("read protected symlink %s: %w", rel, err)
		}
	case info.Mode().IsRegular():
		entry.size = info.Size()
		f, err := tree.Open(native)
		if err != nil {
			return fmt.Errorf("read protected path %s: %w", rel, err)
		}
		h := sha256.New()
		var buf bytes.Buffer
		var w io.Writer = h
		if info.Size() <= maxReadFileSize {
			w = io.MultiWriter(h, &buf)
		}
		_, copyErr := io.Copy(w, f)
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("read protected path %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		copy(entry.sum[:], h.Sum(nil))
		if info.Size() <= maxReadFileSize {
			entry.content = buf.Bytes()
		} else {
			entry.restorable = false
		}
	case info.IsDir():
	default:
		entry.restorable = false
	}
	s.entries[rel] = entry
	return nil
}

// readOnlyBinds returns absolute paths of existing protected regular files and
// real directories to bind read-only inside the sandbox. Symlinks are never
// bound (bubblewrap would follow them to wherever they point), and entries
// inside an already bound directory are skipped.
func (s *protectedPathSnapshot) readOnlyBinds() []string {
	if s == nil {
		return nil
	}
	rels := make([]string, 0, len(s.entries))
	for rel, e := range s.entries {
		if e.mode.IsRegular() || e.mode.IsDir() {
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	var dirs []string
	var out []string
	for _, rel := range rels {
		nested := false
		for _, dir := range dirs {
			if strings.HasPrefix(rel, dir+"/") {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		if s.entries[rel].mode.IsDir() {
			dirs = append(dirs, rel)
		}
		out = append(out, filepath.Join(s.root, filepath.FromSlash(rel)))
	}
	return out
}

// changedAgainst lists the protected paths that differ between s and after.
func (s *protectedPathSnapshot) changedAgainst(after *protectedPathSnapshot) []string {
	var changed []string
	for rel, before := range s.entries {
		if now, ok := after.entries[rel]; !ok || !before.equal(now) {
			changed = append(changed, rel)
		}
	}
	for rel := range after.entries {
		if _, ok := s.entries[rel]; !ok {
			changed = append(changed, rel)
		}
	}
	sort.Strings(changed)
	return changed
}

// enforce compares the workspace with the snapshot, rolls back any change to a
// protected path and reports it. It returns nil when nothing changed.
func (s *protectedPathSnapshot) enforce() error {
	if s == nil {
		return nil
	}
	after, err := snapshotProtectedPaths(s.root)
	if err != nil {
		return fmt.Errorf("%w: cannot re-inspect protected paths after the run: %v", ErrProtectedPathsModified, err)
	}
	changed := s.changedAgainst(after)
	if len(changed) == 0 {
		return nil
	}
	restoreErr := s.restore(after, changed)
	if restoreErr == nil {
		if final, err := snapshotProtectedPaths(s.root); err != nil {
			restoreErr = err
		} else if still := s.changedAgainst(final); len(still) > 0 {
			restoreErr = fmt.Errorf("still differs after rollback: %s", strings.Join(still, ", "))
		}
	}
	if restoreErr != nil {
		return fmt.Errorf("%w (%s); rollback failed, restore these paths manually: %v", ErrProtectedPathsModified, strings.Join(changed, ", "), restoreErr)
	}
	return fmt.Errorf("%w (%s); the changes were rolled back and the run counts as failed", ErrProtectedPathsModified, strings.Join(changed, ", "))
}

// restore rolls the listed paths back to the snapshot state using
// root-confined operations, so a path swapped for a symlink cannot redirect
// the rollback outside the workspace.
func (s *protectedPathSnapshot) restore(after *protectedPathSnapshot, changed []string) error {
	tree, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer tree.Close()

	var errs []error
	// Remove protected paths that did not exist before (parents first, so the
	// nested entries of a removed tree need no separate work).
	for _, rel := range changed {
		if _, existed := s.entries[rel]; existed {
			continue
		}
		if _, ok := after.entries[rel]; !ok {
			continue
		}
		if err := tree.RemoveAll(filepath.FromSlash(rel)); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", rel, err))
		}
	}
	// Recreate or rewrite entries that existed before; changed is sorted, so
	// directories are handled before the files inside them.
	for _, rel := range changed {
		before, existed := s.entries[rel]
		if !existed {
			continue
		}
		if err := restoreProtectedEntry(tree, rel, before); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func restoreProtectedEntry(tree *os.Root, rel string, before protectedPathEntry) error {
	native := filepath.FromSlash(rel)
	if before.opaque {
		return fmt.Errorf("%s was moved or replaced and cannot be reconstructed", rel)
	}
	if !before.restorable {
		return fmt.Errorf("%s changed and no copy was kept to restore it", rel)
	}
	if err := ensureRealParentDirs(tree, native); err != nil {
		return fmt.Errorf("restore %s: %w", rel, err)
	}
	switch {
	case before.mode.IsDir():
		if info, err := tree.Lstat(native); err == nil && info.IsDir() {
			return tree.Chmod(native, before.mode.Perm())
		}
		if err := tree.RemoveAll(native); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		if err := tree.Mkdir(native, before.mode.Perm()); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		return nil
	case before.mode&fs.ModeSymlink != 0:
		if err := tree.RemoveAll(native); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		if err := tree.Symlink(before.link, native); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		return nil
	default:
		if err := tree.RemoveAll(native); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		f, err := tree.OpenFile(native, os.O_CREATE|os.O_EXCL|os.O_WRONLY, before.mode.Perm())
		if err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		_, writeErr := f.Write(before.content)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return fmt.Errorf("restore %s: %w", rel, errors.Join(writeErr, closeErr))
		}
		if err := tree.Chmod(native, before.mode.Perm()); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		return nil
	}
}

// ensureRealParentDirs makes every parent of rel a real directory, replacing
// symlinks or files that were planted in their place.
func ensureRealParentDirs(tree *os.Root, rel string) error {
	parent := filepath.Dir(rel)
	if parent == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := tree.Lstat(current)
		if err == nil && info.IsDir() {
			continue
		}
		if err == nil {
			if err := tree.RemoveAll(current); err != nil {
				return err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := tree.Mkdir(current, 0o755); err != nil {
			return err
		}
	}
	return nil
}
