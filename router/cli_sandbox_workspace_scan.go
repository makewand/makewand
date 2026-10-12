package router

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/makewand/makewand/internal/fsperm"
)

// cliWorkspaceScanLimit bounds the workspace walk (MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT
// overrides it). A writable session whose scan hits it fails closed instead of
// silently leaving entries unchecked.
var cliWorkspaceScanLimit = 1000000

func cliWorkspaceScanLimitValue() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT"))); err == nil && v > 0 {
		return v
	}
	return cliWorkspaceScanLimit
}

const cliGitPointerMaxBytes = 4096

// cliWorkspaceScan lists the workspace entries the provider sandbox must
// protect: git metadata (directories, gitfiles, symlinked .git entries) and
// unix sockets. node_modules and other dependency trees are scanned too,
// since a socket there is as reachable as anywhere else.
type cliWorkspaceScan struct {
	gitDirs   []string
	gitFiles  []string
	gitLinks  []string
	sockets   []string
	truncated bool
}

// cliScanWorkspace walks workspace without following symlinks. .git
// directories are recorded but not descended into: they are bound read-only
// as a whole, which also covers .git/modules and .git/worktrees.
func cliScanWorkspace(workspace string) cliWorkspaceScan {
	var scan cliWorkspaceScan
	count := 0
	limit := cliWorkspaceScanLimitValue()
	_ = filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != workspace {
				// A directory another user owns and we cannot enter is out of
				// the sandbox's reach too; one we own could be chmod-ed back
				// from inside, so skipping it leaves the scan incomplete.
				if errors.Is(err, fs.ErrPermission) && !fsperm.UntraversableForeignDir(path) {
					scan.truncated = true
				}
				return filepath.SkipDir
			}
			return nil
		}
		count++
		if count > limit {
			scan.truncated = true
			return filepath.SkipAll
		}
		if path == workspace {
			return nil
		}
		typ := d.Type()
		if d.Name() == ".git" {
			switch {
			case typ&fs.ModeSymlink != 0:
				scan.gitLinks = append(scan.gitLinks, path)
			case typ.IsDir():
				scan.gitDirs = append(scan.gitDirs, path)
				return filepath.SkipDir
			case typ.IsRegular():
				scan.gitFiles = append(scan.gitFiles, path)
			}
			return nil
		}
		if typ&fs.ModeSocket != 0 {
			scan.sockets = append(scan.sockets, path)
		}
		return nil
	})
	return scan
}

// cliWorkspaceProtectionArgs returns bwrap mounts, applied after the
// workspace bind, that make git metadata inside the workspace read-only and
// mask unix sockets with /dev/null. Git metadata outside the workspace is
// either covered by the read-only root bind or hidden by a mask, so only
// paths inside the workspace need a read-only bind. For a gitfile, the file
// itself, its gitdir and the gitdir's commondir are protected.
func cliWorkspaceProtectionArgs(workspace string, scan cliWorkspaceScan, writable bool) ([]string, error) {
	var args []string
	addRO := func(p string) {
		if p != workspace && cliPathWithin(workspace, p) {
			args = cliAppendRoBindIfNotPresent(args, p)
		}
	}
	if writable {
		if len(scan.gitLinks) > 0 {
			// A symlink cannot be pinned by a mount: the agent could replace
			// it with a directory carrying hooks or config that host git trusts.
			return nil, fmt.Errorf("workspace contains a symlinked .git entry (%s) that cannot be protected in a writable sandbox", scan.gitLinks[0])
		}
		for _, dir := range scan.gitDirs {
			addRO(dir)
		}
		for _, file := range scan.gitFiles {
			addRO(file)
			gitDir, ok := cliResolveGitPointer(filepath.Dir(file), file, "gitdir:")
			if !ok {
				continue
			}
			addRO(gitDir)
			if common, ok := cliResolveGitPointer(gitDir, filepath.Join(gitDir, "commondir"), ""); ok {
				addRO(common)
			}
			if modules := filepath.Join(gitDir, "modules"); cliIsDir(modules) {
				addRO(modules)
			}
		}
	}
	for _, sock := range scan.sockets {
		args = append(args, "--ro-bind", "/dev/null", sock)
	}
	return args, nil
}

// cliResolveGitPointer reads a git pointer file (a gitfile with prefix
// "gitdir:", or commondir with no prefix), resolves it relative to base and
// returns the canonical existing directory.
func cliResolveGitPointer(base, file, prefix string) (string, bool) {
	fi, err := os.Lstat(file)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(file) //nolint:gosec // G304: git pointer file inside the sandbox workspace
	if err != nil {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, cliGitPointerMaxBytes))
	_ = f.Close()
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if prefix != "" {
		if !strings.HasPrefix(line, prefix) {
			return "", false
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	}
	if line == "" {
		return "", false
	}
	target := filepath.FromSlash(line)
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil || !cliIsDir(real) {
		return "", false
	}
	return filepath.Clean(real), true
}

func cliIsDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
