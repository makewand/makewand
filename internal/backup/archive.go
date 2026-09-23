package backup

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
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

// Restore extracts archivePath, verifies every file against the manifest
// checksum, then atomically moves each component to the target path named in
// opts. The server must be stopped: Restore replaces state.db and clears any
// stale WAL/SHM sidecars so the restored snapshot is not shadowed.
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

	stateDir := ""
	if opts.StateDBPath != "" {
		stateDir = filepath.Dir(opts.StateDBPath)
	} else if opts.AuthConfigPath != "" {
		stateDir = filepath.Dir(opts.AuthConfigPath)
	}

	for _, f := range manifest.Files {
		src := filepath.Join(staging, f.Name)
		dst := targetPath(f.Name, opts, stateDir)
		if dst == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, fmt.Errorf("prepare target dir for %s: %w", f.Name, err)
		}
		if f.Name == archiveStateDBName {
			_ = os.Remove(dst + "-wal")
			_ = os.Remove(dst + "-shm")
		}
		if err := atomicReplace(src, dst); err != nil {
			return nil, fmt.Errorf("install %s: %w", f.Name, err)
		}
	}
	return manifest, nil
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

// atomicReplace installs src at dst atomically, tolerating a cross-filesystem
// staging dir by copying into the destination directory first, then renaming.
func atomicReplace(src, dst string) error {
	tmp := dst + ".restore.tmp"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
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
