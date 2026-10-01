package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const restoreJournalName = ".makewand-restore-journal.json"
const restoreLockName = ".makewand-restore.lock"

var ErrRestoreInProgress = errors.New("another restore is in progress")

type restoreInstall struct {
	Name, Tmp, Dst string
	Remove         bool
}
type restoreOriginal struct {
	Destination string `json:"destination"`
	Backup      string `json:"backup,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Mode        uint32 `json:"mode"`
	Existed     bool   `json:"existed"`
}
type restoreJournal struct {
	Version   int               `json:"version"`
	State     string            `json:"state"`
	Originals []restoreOriginal `json:"originals"`
	Installs  []restoreInstall  `json:"installs"`
}

func restoreStateDir(opts Options) string {
	if opts.StateDBPath != "" {
		return filepath.Dir(opts.StateDBPath)
	}
	if opts.AuthConfigPath != "" {
		return filepath.Dir(opts.AuthConfigPath)
	}
	return ""
}

var persistRestoreJournal = writeRestoreJournal

// published distinguishes a failed write from a rename whose directory fsync
// failed. Once published, recovery materials must remain available even when
// the caller cannot establish the durable outcome of that publication.
func writeRestoreJournal(path string, j *restoreJournal) (published bool, err error) {
	data, err := json.Marshal(j)
	if err != nil {
		return false, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".makewand-journal-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, syncDirectory(filepath.Dir(path))
}

func syncRestoreImages(dir string, journal *restoreJournal) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	directories := map[string]bool{}
	add := func(path string) error {
		parent, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return err
		}
		for {
			directories[parent] = true
			if parent == root {
				break
			}
			next := filepath.Dir(parent)
			relative, err := filepath.Rel(root, next)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || next == parent {
				break
			}
			parent = next
		}
		return nil
	}
	for _, original := range journal.Originals {
		if original.Backup != "" {
			if err := add(original.Backup); err != nil {
				return err
			}
		}
	}
	for _, install := range journal.Installs {
		if install.Tmp != "" {
			if err := add(install.Tmp); err != nil {
				return err
			}
		}
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	// Flush newly created descendants before the directories that link them.
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, directory := range ordered {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func allowedRestoreDestination(dst string, opts Options) bool {
	absolute, err := filepath.Abs(dst)
	if err != nil {
		return false
	}
	for _, target := range []string{opts.StateDBPath, opts.AuthConfigPath, opts.StateDBPath + "-wal", opts.StateDBPath + "-shm"} {
		if target == "" || target == "-wal" || target == "-shm" {
			continue
		}
		resolved, err := filepath.Abs(target)
		if err == nil && absolute == resolved {
			return true
		}
	}
	stateDir := restoreStateDir(opts)
	if stateDir == "" {
		return false
	}
	parent, err := filepath.Abs(stateDir)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(parent, absolute)
	if err != nil {
		return false
	}
	return safeArchiveName(filepath.ToSlash(relative)) && filepath.Base(absolute) != restoreJournalName && !strings.HasPrefix(filepath.Base(absolute), ".makewand-")
}

func cleanupRestoreJournal(path string, j *restoreJournal) error {
	for _, original := range j.Originals {
		if original.Backup != "" {
			if err := os.Remove(original.Backup); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	for _, install := range j.Installs {
		if install.Tmp == "" {
			continue
		}
		if err := os.Remove(install.Tmp); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func rollbackRestore(j *restoreJournal) error {
	// Validate every sealed preimage before changing any target.
	for _, original := range j.Originals {
		if original.Existed {
			if err := VerifyFile(original.Backup, original.Hash); err != nil {
				return err
			}
		}
	}
	for _, original := range j.Originals {
		if original.Existed {
			tmp, err := stageBeside(original.Backup, original.Destination)
			if err != nil {
				return err
			}
			if err = os.Chmod(tmp, os.FileMode(original.Mode)); err != nil {
				os.Remove(tmp)
				return err
			}
			if err = os.Rename(tmp, original.Destination); err != nil {
				os.Remove(tmp)
				return err
			}
		} else if err := os.Remove(original.Destination); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := syncDirectory(filepath.Dir(original.Destination)); err != nil {
			return err
		}
	}
	return nil
}

// RecoverRestore rolls back a prepared, interrupted multi-component restore or
// finishes cleanup of a committed one. Call before opening the server DB/auth.
// The caller must have stopped the server; recovery refuses an open database.
func RecoverRestore(opts Options) error {
	dir := restoreStateDir(opts)
	if dir == "" {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(dir, restoreJournalName)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	lock, err := acquireRestoreLock(filepath.Join(dir, restoreLockName))
	if err != nil {
		return err
	}
	defer lock.Close()
	return recoverRestoreLocked(opts)
}

func recoverRestoreLocked(opts Options) error {
	dir := restoreStateDir(opts)
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, restoreJournalName)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe restore journal permissions")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16*1024*1024+1))
	if err != nil {
		return err
	}
	if len(data) > 16*1024*1024 {
		return fmt.Errorf("restore journal too large")
	}
	var journal restoreJournal
	if err = json.Unmarshal(data, &journal); err != nil {
		return err
	}
	if journal.Version != 1 || (journal.State != "prepared" && journal.State != "committed") {
		return fmt.Errorf("unsupported restore journal")
	}
	if opts.StateDBPath != "" {
		busy, err := stateDBInUse(opts.StateDBPath)
		if err != nil {
			return err
		}
		if busy {
			return ErrStateDBInUse
		}
	}
	seen := map[string]bool{}
	for _, original := range journal.Originals {
		if !allowedRestoreDestination(original.Destination, opts) || seen[original.Destination] {
			return fmt.Errorf("unsafe restore journal destination")
		}
		seen[original.Destination] = true
		if original.Existed && (filepath.Dir(original.Backup) != filepath.Dir(original.Destination) || !strings.HasPrefix(filepath.Base(original.Backup), "."+filepath.Base(original.Destination)+".restore-")) {
			return fmt.Errorf("unsafe restore preimage path")
		}
	}
	for _, install := range journal.Installs {
		if !allowedRestoreDestination(install.Dst, opts) || (!install.Remove && (filepath.Dir(install.Tmp) != filepath.Dir(install.Dst) || !strings.HasPrefix(filepath.Base(install.Tmp), "."+filepath.Base(install.Dst)+".restore-"))) {
			return fmt.Errorf("unsafe restore staged path")
		}
	}
	if journal.State == "prepared" {
		if err = rollbackRestore(&journal); err != nil {
			return fmt.Errorf("recover interrupted restore: %w", err)
		}
	}
	return cleanupRestoreJournal(path, &journal)
}

func journaledInstall(opts Options, installs []restoreInstall) error {
	dir := restoreStateDir(opts)
	if dir == "" {
		return fmt.Errorf("restore has no destination")
	}
	path := filepath.Join(dir, restoreJournalName)
	journal := restoreJournal{Version: 1, State: "prepared", Installs: installs}
	destinations := make([]string, 0, len(installs)+2)
	for _, install := range installs {
		destinations = append(destinations, install.Dst)
		if install.Name == archiveStateDBName {
			destinations = append(destinations, install.Dst+"-wal", install.Dst+"-shm")
		}
	}
	discard := func() {
		for _, original := range journal.Originals {
			if original.Backup != "" {
				_ = os.Remove(original.Backup)
			}
		}
		for _, install := range installs {
			_ = os.Remove(install.Tmp)
		}
	}
	for _, dst := range destinations {
		record := restoreOriginal{Destination: dst}
		info, err := os.Lstat(dst)
		if err != nil && !os.IsNotExist(err) {
			discard()
			return err
		}
		if err == nil {
			if !info.Mode().IsRegular() {
				discard()
				return fmt.Errorf("restore target is not a regular file: %s", dst)
			}
			record.Existed = true
			record.Mode = uint32(info.Mode().Perm())
			record.Backup, err = stageBeside(dst, dst)
			if err != nil {
				discard()
				return err
			}
			record.Hash, err = ComputeFileHash(record.Backup)
			if err != nil {
				os.Remove(record.Backup)
				discard()
				return err
			}
		}
		journal.Originals = append(journal.Originals, record)
	}
	if err := syncRestoreImages(dir, &journal); err != nil {
		discard()
		return err
	}
	if published, err := persistRestoreJournal(path, &journal); err != nil {
		if !published {
			discard()
		}
		return err
	}
	fail := func(cause error) error {
		err := rollbackRestore(&journal)
		if err == nil {
			err = cleanupRestoreJournal(path, &journal)
		}
		return errors.Join(cause, err)
	}
	for _, install := range installs {
		if install.Name == archiveStateDBName {
			for _, suffix := range []string{"-wal", "-shm"} {
				if err := os.Remove(install.Dst + suffix); err != nil && !os.IsNotExist(err) {
					return fail(err)
				}
			}
		}
		if install.Remove {
			if err := os.Remove(install.Dst); err != nil && !os.IsNotExist(err) {
				return fail(err)
			}
		} else if err := renameFile(install.Tmp, install.Dst); err != nil {
			return fail(err)
		}
		if err := syncDirectory(filepath.Dir(install.Dst)); err != nil {
			return fail(err)
		}
		if opts.Progress != nil {
			opts.Progress("installed", install.Name)
		}
	}
	journal.State = "committed"
	if _, err := persistRestoreJournal(path, &journal); err != nil {
		// Live components are all installed. The on-disk journal may still be
		// prepared or already committed; recovery must decide from that state.
		// Starting a rollback here could leave mixed pre/postimages if it fails
		// while the published journal says committed.
		return fmt.Errorf("restore commit requires recovery: %w", err)
	}
	return cleanupRestoreJournal(path, &journal)
}
