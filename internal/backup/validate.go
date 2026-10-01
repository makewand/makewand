package backup

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// validateSQLite reads a staged database without creating a WAL or changing its
// journal mode. A checksum alone does not prove a database is internally valid.
func validateSQLite(path string) (map[string]int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	db, err := sql.Open("sqlite", uri.String()+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return nil, fmt.Errorf("database integrity: %w", err)
	}
	for rows.Next() {
		var result string
		if err = rows.Scan(&result); err != nil {
			rows.Close()
			return nil, err
		}
		if result != "ok" {
			rows.Close()
			return nil, fmt.Errorf("database integrity: %s", result)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		rows.Close()
		return nil, fmt.Errorf("database contains foreign key violations")
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	schemas := map[string]int{}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return nil, err
	}
	if version > 2 {
		return nil, fmt.Errorf("unsupported database schema version %d", version)
	}
	schemas["sqlite_user_version"] = version
	var metadata int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_versions'`).Scan(&metadata); err != nil {
		return nil, err
	}
	if metadata != 0 {
		rows, err = db.Query(`SELECT component,version FROM schema_versions`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var component string
			var ver int
			if err = rows.Scan(&component, &ver); err != nil {
				return nil, err
			}
			if component != "usage" || ver < 0 || ver > 2 {
				return nil, fmt.Errorf("unsupported %s schema version %d", component, ver)
			}
			schemas[component] = ver
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	return schemas, nil
}

func safeArchiveName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".makewand-") || strings.ContainsAny(name, "\\:\x00") || path.IsAbs(name) || path.Clean(name) != name {
		return false
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return len(parts) == 1 || parts[0] == "sessions"
}

func verifyStaging(staging string, manifest *Manifest) error {
	if len(manifest.Files) == 0 {
		return fmt.Errorf("empty backup manifest")
	}
	names := map[string]bool{archiveManifest: true}
	for _, f := range manifest.Files {
		if !safeArchiveName(f.Name) || f.Name == archiveManifest || names[f.Name] {
			return fmt.Errorf("unsafe or duplicate manifest entry: %q", f.Name)
		}
		names[f.Name] = true
		if f.Error != "" || f.IsDir {
			return fmt.Errorf("invalid backup entry %s", f.Name)
		}
		path := filepath.Join(staging, f.Name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != f.Size {
			return fmt.Errorf("backup size/type mismatch for %s", f.Name)
		}
		if err = VerifyFile(path, f.Hash); err != nil {
			return err
		}
		if f.Name == archiveStateDBName {
			actual, err := validateSQLite(path)
			if err != nil {
				return err
			}
			if manifest.DatabaseSchema != nil {
				if len(actual) != len(manifest.DatabaseSchema) {
					return fmt.Errorf("database schema manifest mismatch")
				}
				for key, version := range actual {
					if expected, ok := manifest.DatabaseSchema[key]; !ok || expected != version {
						return fmt.Errorf("database schema manifest mismatch: %s", key)
					}
				}
			}
		}
	}
	return filepath.WalkDir(staging, func(file string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(staging, file)
		if err != nil {
			return err
		}
		if !names[filepath.ToSlash(relative)] {
			return fmt.Errorf("unmanifested archive entry %q", relative)
		}
		return nil
	})
}
