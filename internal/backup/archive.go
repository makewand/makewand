package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/makewand/makewand/serverdb"
)

// Options names the sources (for Create) or targets (for Restore) of a backup.
// An empty path skips that component.
type Options struct {
	// StateDBPath is the SQLite state database. On backup it is snapshotted with
	// VACUUM INTO for a transaction-consistent copy regardless of WAL state.
	StateDBPath string
	// AuthConfigPath is the JSON auth config (e.g. /etc/makewand/server_auth.json),
	// which under the systemd layout lives outside the state directory.
	AuthConfigPath string
	// ExtraFiles are additional files (audit.jsonl, usage.jsonl, ...) archived and
	// restored by basename into the state database's directory.
	ExtraFiles []string
	// ExtraDirectories includes the server sessions tree, rejecting symlinks.
	ExtraDirectories []string
	// Progress observes durable restore milestones. It runs synchronously and
	// should return promptly; it grants no ability to alter the install plan.
	Progress func(phase, component string)
}

const (
	archiveStateDBName = "state.db"
	archiveAuthName    = "server_auth.json"
	archiveManifest    = "manifest.json"

	// MaxArchiveEntrySize limits a single restored file to 512MB to prevent decompression bombs.
	MaxArchiveEntrySize = 512 * 1024 * 1024
	// MaxArchiveTotalSize limits the total uncompressed size across all entries to 2GB.
	MaxArchiveTotalSize = 2 * 1024 * 1024 * 1024
	// MaxArchiveEntryCount limits the total number of entries to prevent inode exhaustion.
	MaxArchiveEntryCount = 10000
)

var (
	maxArchiveEntrySize  int64 = MaxArchiveEntrySize
	maxArchiveTotalSize  int64 = MaxArchiveTotalSize
	maxArchiveEntryCount int   = MaxArchiveEntryCount
)

// SnapshotSQLite writes a transaction-consistent copy of the SQLite database at
// srcPath to destPath using VACUUM INTO. Unlike copying the live file, this is
// safe while the server is writing: it never mixes partial transactions and
// folds any WAL contents into the snapshot. destPath must not already exist.
func SnapshotSQLite(srcPath, destPath string) error {
	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("stat source db: %w", err)
	}
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear snapshot target: %w", err)
	}
	db, err := serverdb.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer db.Close()
	// VACUUM INTO takes a string literal, not a bind parameter; quote and escape
	// the operator-provided destination path.
	stmt := "VACUUM INTO '" + strings.ReplaceAll(destPath, "'", "''") + "'" //nolint:gosec // G202: VACUUM INTO cannot bind a parameter; destPath is operator-supplied and single-quotes are escaped.
	if _, err := db.Exec(stmt); err != nil {
		return fmt.Errorf("vacuum into snapshot: %w", err)
	}
	return nil
}

// Create builds a verified backup archive (tar.gz) at archivePath from the
// components named in opts. It returns the manifest describing the archive.
func Create(archivePath string, opts Options) (*Manifest, error) {
	staging, err := os.MkdirTemp("", "makewand-backup-")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	type entry struct{ name, path string }
	var staged []entry

	if strings.TrimSpace(opts.StateDBPath) != "" {
		dst := filepath.Join(staging, archiveStateDBName)
		if err := SnapshotSQLite(opts.StateDBPath, dst); err != nil {
			return nil, err
		}
		staged = append(staged, entry{archiveStateDBName, dst})
	}
	if strings.TrimSpace(opts.AuthConfigPath) != "" {
		if _, err := os.Stat(opts.AuthConfigPath); err != nil {
			return nil, fmt.Errorf("auth source: %w", err)
		} else {
			dst := filepath.Join(staging, archiveAuthName)
			if err := copyFile(opts.AuthConfigPath, dst); err != nil {
				return nil, err
			}
			staged = append(staged, entry{archiveAuthName, dst})
		}
	}
	for _, extra := range opts.ExtraFiles {
		extra = strings.TrimSpace(extra)
		if extra == "" {
			continue
		}
		if _, err := os.Stat(extra); err != nil {
			return nil, fmt.Errorf("extra source: %w", err)
		}
		name := filepath.Base(extra)
		if name == archiveStateDBName || name == archiveAuthName || name == archiveManifest {
			return nil, fmt.Errorf("extra file %q collides with a reserved archive name", name)
		}
		dst := filepath.Join(staging, name)
		if err := copyFile(extra, dst); err != nil {
			return nil, err
		}
		staged = append(staged, entry{name, dst})
	}
	for _, directory := range opts.ExtraDirectories {
		if filepath.Base(filepath.Clean(directory)) != "sessions" {
			return nil, fmt.Errorf("unsupported backup directory: %s", directory)
		}
		if _, err := os.Stat(directory); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := filepath.WalkDir(directory, func(path string, item fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("backup directory contains symlink: %s", path)
			}
			if item.IsDir() {
				return nil
			}
			if !item.Type().IsRegular() {
				return fmt.Errorf("backup directory contains special file: %s", path)
			}
			relative, err := filepath.Rel(directory, path)
			if err != nil {
				return err
			}
			name := "sessions/" + filepath.ToSlash(relative)
			if !safeArchiveName(name) {
				return fmt.Errorf("unsafe backup path %s", name)
			}
			destination := filepath.Join(staging, filepath.FromSlash(name))
			if err = os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			if err = copyFile(path, destination); err != nil {
				return err
			}
			staged = append(staged, entry{name, destination})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(staged) == 0 {
		return nil, fmt.Errorf("nothing to back up: no state-db, auth-config, or extra files found")
	}

	namesSeen := make(map[string]bool)
	for _, item := range staged {
		if namesSeen[item.name] {
			return nil, fmt.Errorf("duplicate backup entry %q", item.name)
		}
		namesSeen[item.name] = true
	}
	if len(staged) > maxArchiveEntryCount-1 {
		return nil, fmt.Errorf("backup entry count exceeds archive limit")
	}
	var totalSize int64
	b := NewBackup()
	if len(opts.ExtraDirectories) > 0 {
		b.manifest.Directories = []string{"sessions"}
	}
	for _, s := range staged {
		if s.name == archiveStateDBName {
			schemas, err := validateSQLite(s.path)
			if err != nil {
				return nil, err
			}
			b.manifest.DatabaseSchema = schemas
		}
		info, err := os.Stat(s.path)
		if err != nil {
			return nil, fmt.Errorf("stat staged %s: %w", s.name, err)
		}
		if info.Size() > maxArchiveEntrySize {
			return nil, fmt.Errorf("backup entry %s exceeds archive size limit", s.name)
		}
		totalSize += info.Size()
		if totalSize > maxArchiveTotalSize {
			return nil, fmt.Errorf("backup exceeds archive total size limit")
		}
		b.AddFile(s.name, info)
		hash, err := ComputeFileHash(s.path)
		if err != nil {
			return nil, fmt.Errorf("hash staged %s: %w", s.name, err)
		}
		if err := b.SetFileHash(s.name, hash); err != nil {
			return nil, err
		}
	}
	if err := b.Finalize(); err != nil {
		return nil, err
	}
	if err := b.WriteManifest(filepath.Join(staging, archiveManifest)); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(staged)+1)
	for _, s := range staged {
		names = append(names, s.name)
	}
	names = append(names, archiveManifest)
	if err := writeTarGz(archivePath, staging, names); err != nil {
		return nil, err
	}
	return b.manifest, nil
}

// ErrStateDBInUse is returned by Restore when the target state database is
// still open by another connection (typically a running server).
var ErrStateDBInUse = errors.New("state database is in use")

// renameFile is os.Rename; a package variable so tests can inject failures.
var renameFile = os.Rename

// Restore extracts archivePath, verifies every file against the manifest
// checksum, then installs each component at the target path named in opts.
//
// Preconditions and ordering:
//   - The server must be stopped. Restore refuses (ErrStateDBInUse) while any
//     connection still has the target state database open, detected through
//     SQLite's own WAL-index lock.
//   - Every component is first copied next to its destination; no live path
//     is touched until all copies succeeded.
//   - state.db is replaced atomically. Its -wal/-shm sidecars are moved aside
//     before the swap and deleted only after the new database is in place; if
//     the swap fails they are moved back, so the old database keeps the
//     transactions still held in its WAL. After a successful swap they are
//     removed so they cannot be replayed onto the restored snapshot.
func Restore(archivePath string, opts Options) (*Manifest, error) {
	staging, err := os.MkdirTemp("", "makewand-restore-")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	if err := extractTarGz(archivePath, staging); err != nil {
		return nil, err
	}
	manifest, err := LoadManifest(filepath.Join(staging, archiveManifest))
	if err != nil {
		return nil, err
	}
	// Validate hashes, completeness and database structure before any live write.
	if err := verifyStaging(staging, manifest); err != nil {
		return nil, err
	}

	if opts.StateDBPath != "" {
		inUse, err := stateDBInUse(opts.StateDBPath)
		if err != nil {
			return nil, fmt.Errorf("check whether %s is in use: %w", opts.StateDBPath, err)
		}
		if inUse {
			return nil, fmt.Errorf("%w: %s is open by another process (is the makewand server still running?); stop it before restoring", ErrStateDBInUse, opts.StateDBPath)
		}
	}

	stateDir := ""
	if opts.StateDBPath != "" {
		stateDir = filepath.Dir(opts.StateDBPath)
	} else if opts.AuthConfigPath != "" {
		stateDir = filepath.Dir(opts.AuthConfigPath)
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := acquireRestoreLock(filepath.Join(stateDir, restoreLockName))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := recoverRestoreLocked(opts); err != nil {
		return nil, err
	}

	// Phase 1: stage every component beside its destination.
	var installs []restoreInstall
	discardStaged := func() {
		for _, in := range installs {
			_ = os.Remove(in.Tmp)
		}
	}
	for _, f := range manifest.Files {
		if !safeArchiveName(f.Name) {
			discardStaged()
			return nil, fmt.Errorf("unsafe manifest file entry: %q", f.Name)
		}
		src := filepath.Join(staging, f.Name)
		dst := targetPath(f.Name, opts, stateDir)
		if dst == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			discardStaged()
			return nil, fmt.Errorf("prepare target dir for %s: %w", f.Name, err)
		}
		tmp, err := stageBeside(src, dst)
		if err != nil {
			discardStaged()
			return nil, fmt.Errorf("install %s: %w", f.Name, err)
		}
		installs = append(installs, restoreInstall{Name: f.Name, Tmp: tmp, Dst: dst})
	}

	for _, directory := range manifest.Directories {
		if directory != "sessions" {
			discardStaged()
			return nil, fmt.Errorf("unsupported restore directory %q", directory)
		}
		tree := filepath.Join(stateDir, "sessions")
		planned := make(map[string]bool)
		for _, install := range installs {
			planned[install.Dst] = true
		}
		err := filepath.WalkDir(tree, func(path string, item fs.DirEntry, walkErr error) error {
			if os.IsNotExist(walkErr) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if item.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("restore sessions target contains symlink")
			}
			if item.IsDir() {
				return nil
			}
			if !item.Type().IsRegular() {
				return fmt.Errorf("restore sessions target contains special file")
			}
			if !planned[path] {
				relative, err := filepath.Rel(stateDir, path)
				if err != nil {
					return err
				}
				installs = append(installs, restoreInstall{Name: filepath.ToSlash(relative), Dst: path, Remove: true})
			}
			return nil
		})
		if err != nil {
			discardStaged()
			return nil, err
		}
	}
	destinations := make(map[string]bool)
	for _, install := range installs {
		absolute, err := filepath.Abs(install.Dst)
		if err != nil || destinations[absolute] || strings.HasPrefix(filepath.Base(absolute), ".makewand-") {
			discardStaged()
			return nil, fmt.Errorf("duplicate or reserved restore target %s", install.Dst)
		}
		destinations[absolute] = true
	}

	// Phase 2: durably journal preimages, then install or recover as a unit.
	if err := journaledInstall(opts, installs); err != nil {
		return nil, err
	}
	return manifest, nil
}

// stageBeside copies src into a new uniquely named temporary file in dst's
// directory (so the final rename stays on one filesystem) and returns its path.
func stageBeside(src, dst string) (string, error) {
	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".restore-*")
	if err != nil {
		return "", err
	}
	tmp := out.Name()
	in, err := os.Open(src)
	if err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	_, copyErr := io.Copy(out, in)
	in.Close()
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("stage %s: %w", dst, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// targetPath maps a canonical archive entry name to its restore destination.
func targetPath(name string, opts Options, stateDir string) string {
	switch name {
	case archiveStateDBName:
		return opts.StateDBPath
	case archiveAuthName:
		return opts.AuthConfigPath
	default:
		if stateDir == "" {
			return ""
		}
		if !safeArchiveName(name) {
			return ""
		}
		return filepath.Join(stateDir, filepath.FromSlash(name))
	}
}

func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup source is not a regular file: %s", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if written, err := io.Copy(out, io.LimitReader(in, maxArchiveEntrySize+1)); err != nil || written > maxArchiveEntrySize {
		out.Close()
		return fmt.Errorf("copy %s failed or exceeded archive size limit: %v", src, err)
	}
	return out.Close()
}

func writeTarGz(archivePath, dir string, names []string) error {
	f, err := os.CreateTemp(filepath.Dir(archivePath), ".makewand-backup-*")
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer f.Close()
	temporary := f.Name()
	defer os.Remove(temporary)
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)

	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", name, err)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("header %s: %w", name, err)
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header %s: %w", name, err)
		}
		in, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", name, err)
		}
		if _, err := io.Copy(tw, in); err != nil {
			in.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
		in.Close()
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, archivePath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(archivePath))
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath) //nolint:gosec // Operator-provided archive source; this read grants no destination path.
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var (
		totalExtracted int64
		entryCount     int
		seen           = make(map[string]bool)
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		entryCount++
		if entryCount > maxArchiveEntryCount {
			return fmt.Errorf("archive entry count exceeds limit of %d", maxArchiveEntryCount)
		}
		// Flat archive only: reject any path separators or traversal (zip-slip).
		name := hdr.Name
		if !safeArchiveName(name) || seen[name] {
			return fmt.Errorf("unsafe archive entry: %q", hdr.Name)
		}
		seen[name] = true
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported archive entry type: %q", name)
		}
		dst := filepath.Join(destDir, filepath.FromSlash(name)) //nolint:gosec // G305: name is validated above to be a bare basename (no separators or "..").
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // Canonical allowlisted archive name under the new private staging directory.
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		lr := io.LimitReader(tr, maxArchiveEntrySize+1)
		//nolint:gosec // G110: extraction is bounded by maxArchiveEntrySize and maxArchiveTotalSize.
		written, err := io.Copy(out, lr)
		if err != nil {
			out.Close()
			return fmt.Errorf("extract %s: %w", name, err)
		}
		if written > maxArchiveEntrySize {
			out.Close()
			_ = os.Remove(dst) //nolint:gosec // Only the just-created, validated private staging path is removed.
			return fmt.Errorf("extract %s: entry exceeds max allowed size of %d bytes", name, maxArchiveEntrySize)
		}
		totalExtracted += written
		if totalExtracted > maxArchiveTotalSize {
			out.Close()
			_ = os.Remove(dst) //nolint:gosec // Only the just-created, validated private staging path is removed.
			return fmt.Errorf("archive exceeds total uncompressed size limit of %d bytes", maxArchiveTotalSize)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close %s: %w", name, err)
		}
	}
	return nil
}
