package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
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
}

const (
	archiveStateDBName = "state.db"
	archiveAuthName    = "server_auth.json"
	archiveManifest    = "manifest.json"
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
		if _, err := os.Stat(opts.AuthConfigPath); err == nil {
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
			continue
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
	if len(staged) == 0 {
		return nil, fmt.Errorf("nothing to back up: no state-db, auth-config, or extra files found")
	}

	b := NewBackup()
	for _, s := range staged {
		info, err := os.Stat(s.path)
		if err != nil {
			return nil, fmt.Errorf("stat staged %s: %w", s.name, err)
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
	// Verify before touching any live path so a corrupt archive aborts cleanly.
	for _, f := range manifest.Files {
		if f.Error != "" {
			return nil, fmt.Errorf("archive records a backup-time error for %s: %s", f.Name, f.Error)
		}
		if err := VerifyFile(filepath.Join(staging, f.Name), f.Hash); err != nil {
			return nil, err
		}
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

	// Phase 1: stage every component beside its destination.
	type install struct{ name, tmp, dst string }
	var installs []install
	discardStaged := func() {
		for _, in := range installs {
			_ = os.Remove(in.tmp)
		}
	}
	for _, f := range manifest.Files {
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
		installs = append(installs, install{name: f.Name, tmp: tmp, dst: dst})
	}

	// Phase 2: swap each staged copy into place.
	for i, in := range installs {
		var err error
		if in.name == archiveStateDBName {
			err = replaceStateDB(in.tmp, in.dst)
		} else {
			err = renameFile(in.tmp, in.dst)
		}
		if err != nil {
			for _, rest := range installs[i:] {
				_ = os.Remove(rest.tmp)
			}
			return nil, fmt.Errorf("install %s: %w", in.name, err)
		}
	}
	return manifest, nil
}

// replaceStateDB swaps the staged database into dst. The old -wal/-shm files
// are parked under unique names first and only deleted once the swap has
// succeeded; on failure they are put back untouched.
func replaceStateDB(tmp, dst string) error {
	type parked struct{ orig, aside string }
	var sidecars []parked
	unpark := func() error {
		var errs []error
		for _, p := range sidecars {
			if err := renameFile(p.aside, p.orig); err != nil {
				errs = append(errs, fmt.Errorf("put back %s (kept at %s): %w", p.orig, p.aside, err))
			}
		}
		return errors.Join(errs...)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		orig := dst + suffix
		if _, err := os.Lstat(orig); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return errors.Join(fmt.Errorf("inspect %s: %w", orig, err), unpark())
		}
		aside := orig + ".pre-restore-" + rand.Text()
		if err := renameFile(orig, aside); err != nil {
			return errors.Join(fmt.Errorf("move aside %s: %w", orig, err), unpark())
		}
		sidecars = append(sidecars, parked{orig: orig, aside: aside})
	}
	if err := renameFile(tmp, dst); err != nil {
		return errors.Join(fmt.Errorf("rename into place: %w", err), unpark())
	}
	// The restored snapshot is in place; the parked sidecars belong to the
	// replaced database and must never be replayed onto it.
	for _, p := range sidecars {
		_ = os.Remove(p.aside)
	}
	return nil
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
		return filepath.Join(stateDir, name)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return out.Close()
}

func writeTarGz(archivePath, dir string, names []string) error {
	f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer f.Close()
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
	return nil
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
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

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		// Flat archive only: reject any path separators or traversal (zip-slip).
		name := hdr.Name
		if name != filepath.Base(name) || strings.Contains(name, "..") {
			return fmt.Errorf("unsafe archive entry: %q", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst := filepath.Join(destDir, name) //nolint:gosec // G305: name is validated above to be a bare basename (no separators or "..").
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // G110: archives are operator-owned local backups, not untrusted input.
			out.Close()
			return fmt.Errorf("extract %s: %w", name, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close %s: %w", name, err)
		}
	}
	return nil
}
