package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Manifest-only dependency fetches (GEC-1021-1, round 5). A networked
// dependency fetch never sees the workspace: the host creates a fresh private
// directory for each fetch outside the workspace and outside the sandbox
// HOME, copies into it only the manifests and lockfiles the ecosystem's fetch
// reads (as regular files, through os.Root handles, refusing symlinks), and
// runs the fetch there with trusted tools resolved outside the workspace.
// What the fetch downloads lands in host-owned caches outside the workspace,
// bound read-write into the fetch and read-only into every later loopback
// step, so no step that runs project code can leave state a later networked
// fetch reuses. Results that belong in the workspace (a lockfile the fetch
// resolved, node_modules) are put there by the host afterwards, again through
// an os.Root handle of the workspace.

// depFetchBaseDir returns the host-owned directory that holds the shared
// dependency caches and the private directory of each fetch. A package
// variable so tests keep it in their own temporary directory.
var depFetchBaseDir = func() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "makewand", "dep-fetch"), nil
}

// The shared caches below depFetchArea.cache, one per ecosystem.
const (
	fetchCacheGo    = "gomod" // GOMODCACHE, cache/vcs included
	fetchCacheCargo = "cargo" // CARGO_HOME: registry index and crates, git checkouts
	fetchCacheNpm   = "npm"   // npm's cache
)

// depFetchArea is the host-owned home of networked dependency fetches: the
// shared caches (cache/) and a private directory per fetch (runs/). Every
// host-side write below it goes through root.
type depFetchArea struct {
	base  string
	root  *os.Root
	cache string
}

// openDepFetchArea opens (creating it, 0700) the dependency fetch area for a
// fetch from workspace. The area must lie outside the workspace, and the
// workspace outside it: a fetch must never see the workspace, and no step
// running project code may write what a fetch reuses.
func openDepFetchArea(workspace string) (*depFetchArea, error) {
	base, err := depFetchBaseDir()
	if err != nil {
		return nil, fmt.Errorf("locate the dependency fetch directory: %w", err)
	}
	if base == "" || !filepath.IsAbs(base) {
		return nil, fmt.Errorf("the dependency fetch directory %q is not an absolute path", base)
	}
	base = filepath.Clean(base)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create the dependency fetch directory: %w", err)
	}
	if overlapping(base, workspace) {
		return nil, fmt.Errorf("the dependency fetch directory %s and the project %s overlap; a networked fetch must see no part of the project", base, workspace)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("open the dependency fetch directory: %w", err)
	}
	area := &depFetchArea{base: base, root: root, cache: filepath.Join(base, "cache")}
	for _, dir := range []string{"runs", "cache", filepath.Join("cache", fetchCacheGo), filepath.Join("cache", fetchCacheCargo), filepath.Join("cache", fetchCacheNpm)} {
		if err := mkdirNoSymlink(root, dir, 0o700); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("create the dependency fetch directory: %w", err)
		}
	}
	return area, nil
}

// overlapping reports whether one of two directories contains the other, as
// written or with symlinks resolved.
func overlapping(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	pairs := [][2]string{{filepath.Clean(a), filepath.Clean(b)}}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		pairs = append(pairs, [2]string{ra, rb})
	}
	for _, p := range pairs {
		if pathWithin(p[0], p[1]) || pathWithin(p[1], p[0]) {
			return true
		}
	}
	return false
}

// mkdirNoSymlink creates rel below root unless it exists as a directory; a
// symlink or a non-directory there is refused, never followed.
func mkdirNoSymlink(root *os.Root, rel string, perm fs.FileMode) error {
	info, err := root.Lstat(rel)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("refusing to use %s: it is not a directory", rel)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := root.Mkdir(rel, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if info, err := root.Lstat(rel); err != nil || !info.IsDir() {
		return fmt.Errorf("refusing to use %s: it is not a directory", rel)
	}
	return nil
}

func (a *depFetchArea) close() {
	if a != nil && a.root != nil {
		_ = a.root.Close()
	}
}

// cacheDir returns the absolute path of the shared cache name.
func (a *depFetchArea) cacheDir(name string) string {
	return filepath.Join(a.cache, name)
}

// fetchDir is the private, manifest-only directory one fetch runs in. Every
// host-side write into it goes through root, the handle it was created
// through; it is never reopened by path.
type fetchDir struct {
	path string
	root *os.Root
	area *depFetchArea
	rel  string // below the area
}

// newFetchDir creates a fresh fetch directory (0700) under a random name.
func (a *depFetchArea) newFetchDir() (*fetchDir, error) {
	for attempt := 0; ; attempt++ {
		rel := filepath.Join("runs", "fetch-"+rand.Text())
		err := a.root.Mkdir(rel, 0o700)
		if errors.Is(err, fs.ErrExist) && attempt < 8 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create the dependency fetch directory: %w", err)
		}
		root, err := a.root.OpenRoot(rel)
		if err != nil {
			_ = a.root.Remove(rel)
			return nil, fmt.Errorf("open the dependency fetch directory: %w", err)
		}
		return &fetchDir{path: filepath.Join(a.base, rel), root: root, area: a, rel: rel}, nil
	}
}

// remove deletes the fetch directory and everything the fetch left in it.
func (d *fetchDir) remove() {
	if d == nil {
		return
	}
	_ = d.root.Close()
	_ = d.area.root.RemoveAll(d.rel)
}

// abs returns the absolute path of rel inside the fetch directory.
func (d *fetchDir) abs(rel string) string {
	return filepath.Join(d.path, filepath.FromSlash(rel))
}

// mkdirAll creates rel and its parents inside the fetch directory.
func (d *fetchDir) mkdirAll(rel string) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		if err := mkdirNoSymlink(d.root, filepath.Join(parts[:i+1]...), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// writeFile creates rel (it must not exist) in the fetch directory with data.
func (d *fetchDir) writeFile(rel string, data []byte, perm fs.FileMode) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if err := d.mkdirAll(filepath.Dir(rel)); err != nil {
		return err
	}
	f, err := d.root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}

// readFile reads a regular file the fetch left in the fetch directory, within
// limit bytes.
func (d *fetchDir) readFile(rel string, limit int64) ([]byte, error) {
	return readRegularNoFollow(d.root, rel, limit)
}

// errFetchFileTooLarge reports a manifest over its copy limit.
var errFetchFileTooLarge = errors.New("larger than makewand copies for a dependency fetch")

// readRegularNoFollow reads the regular file rel below root, within limit
// bytes. A symlink as the file or as any directory on its path, and a file
// that is not regular, are refused rather than followed; a file replaced
// between the check and the open is refused too. A missing file is an error
// wrapping fs.ErrNotExist.
func readRegularNoFollow(root *os.Root, rel string, limit int64) ([]byte, error) {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s: not a path inside the directory", rel)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i := 1; i < len(parts); i++ {
		dir := filepath.Join(parts[:i]...)
		info, err := root.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s: %s is not a directory", rel, dir)
		}
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s: %w (%d bytes)", rel, errFetchFileTooLarge, limit)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s changed while makewand read it", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: %w (%d bytes)", rel, errFetchFileTooLarge, limit)
	}
	return data, nil
}

// copyIntoFetchDir copies the regular file rel of the workspace (read through
// ws, readRegularNoFollow) to the same relative path in the fetch directory.
// A missing file is an error wrapping fs.ErrNotExist.
func copyIntoFetchDir(ws *os.Root, rel string, dst *fetchDir, limit int64) error {
	data, err := readRegularNoFollow(ws, rel, limit)
	if err != nil {
		return err
	}
	return dst.writeFile(rel, data, 0o600)
}

// writeBackWorkspaceFile puts a file the fetch produced (a lockfile, go.mod)
// at rel in the workspace through ws: written beside it and renamed into
// place, so a target that is not a regular file (a symlink included) is
// refused, never followed. A file already holding data is left alone; a new
// one is created 0644, as the tool would have created it.
func writeBackWorkspaceFile(ws *os.Root, rel string, data []byte) error {
	if current, err := readRegularNoFollow(ws, rel, int64(len(data))); err == nil && string(current) == string(data) {
		return nil
	}
	var mode *fs.FileMode
	if _, err := ws.Lstat(rel); errors.Is(err, fs.ErrNotExist) {
		created := fs.FileMode(0o644)
		mode = &created
	}
	return writeFileModeRoot(ws, rel, string(data), mode)
}

// maxFetchTreeBytes and maxFetchTreeEntries bound a tree the host copies out
// of a fetch directory into the workspace (node_modules).
const (
	maxFetchTreeBytes   = 8 << 30
	maxFetchTreeEntries = 1_000_000
)

// placeFetchTree replaces the directory rel of the workspace with the tree the
// fetch left at the same path of its fetch directory: it is copied into a
// fresh directory beside the target through the workspace's os.Root handle
// (symlinks recreated as links, never followed; anything but directories,
// regular files and symlinks skipped) and renamed into place once complete.
func placeFetchTree(src *fetchDir, ws *os.Root, rel string) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if info, err := src.root.Lstat(rel); err != nil || !info.IsDir() {
		return nil // the fetch left nothing to place
	}
	staging := filepath.Join(filepath.Dir(rel), ".makewand-"+filepath.Base(rel)+"-"+rand.Text())
	if err := ws.Mkdir(staging, 0o755); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = ws.RemoveAll(staging)
		}
	}()
	var bytes int64
	entries := 0
	err := fs.WalkDir(src.root.FS(), filepath.ToSlash(rel), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entries++; entries > maxFetchTreeEntries {
			return fmt.Errorf("%s holds more than %d entries", rel, maxFetchTreeEntries)
		}
		sub, _ := filepath.Rel(rel, filepath.FromSlash(path))
		if sub == "." {
			return nil
		}
		target := filepath.Join(staging, sub)
		native := filepath.FromSlash(path)
		switch {
		case d.IsDir():
			return ws.Mkdir(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := src.root.Readlink(native)
			if err != nil {
				return err
			}
			return ws.Symlink(link, target)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if bytes += info.Size(); bytes > maxFetchTreeBytes {
				return fmt.Errorf("%s holds more than %d bytes", rel, int64(maxFetchTreeBytes))
			}
			return copyFetchTreeFile(src.root, native, ws, target, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("place %s in the project: %w", rel, err)
	}
	if err := ws.RemoveAll(rel); err != nil {
		return fmt.Errorf("place %s in the project: %w", rel, err)
	}
	if err := ws.Rename(staging, rel); err != nil {
		return fmt.Errorf("place %s in the project: %w", rel, err)
	}
	keep = true
	return nil
}

// copyFetchTreeFile copies one regular file of a fetch tree.
func copyFetchTreeFile(src *os.Root, from string, dst *os.Root, to string, perm fs.FileMode) error {
	in, err := src.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if info, err := in.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	out, err := dst.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}

// fetchPathEnv returns the PATH of a networked fetch: the sandbox PATH without
// any entry that is relative, lies in the workspace (the sandbox HOME
// included) or reaches into it through a symlink. A workspace .venv/bin or
// node_modules/.bin therefore never supplies a fetch command.
func fetchPathEnv(workspace string) string {
	var kept []string
	for _, dir := range filepath.SplitList(sandboxPathEnv()) {
		if dir == "" || !filepath.IsAbs(dir) || insideWorkspace(workspace, dir) {
			continue
		}
		kept = append(kept, filepath.Clean(dir))
	}
	return strings.Join(kept, string(filepath.ListSeparator))
}

// insideWorkspace reports whether path lies in the workspace, as written or
// with symlinks resolved.
func insideWorkspace(workspace, path string) bool {
	if workspace == "" {
		return false
	}
	if pathWithin(filepath.Clean(workspace), filepath.Clean(path)) {
		return true
	}
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		realWorkspace = filepath.Clean(workspace)
	}
	if real, err := filepath.EvalSymlinks(path); err == nil && pathWithin(realWorkspace, real) {
		return true
	}
	return false
}

// fetchToolPath resolves, on the host, the executable a networked fetch runs
// for name: the first one on the fetch PATH (fetchPathEnv) that neither lies
// in the workspace nor reaches into it through a symlink or its #!
// interpreter. When none does it returns name itself: the fetch sandbox then
// looks it up on the same PATH, with the workspace hidden, and fails as a
// missing tool does.
func fetchToolPath(name, workspace string) string {
	pathEnv := fetchPathEnv(workspace)
	return trustedToolPath(name, workspace, pathEnv)
}

// trustedToolPath also serves host-side probes, which must never execute a
// workspace tool even when the subsequent sandbox command is loopback-only.
func trustedToolPath(name, workspace, pathEnv string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" || !filepath.IsAbs(dir) || insideWorkspace(workspace, dir) {
			continue
		}
		candidate := lookPathIn(name, dir)
		if candidate == "" || !trustedFetchExecutable(candidate, workspace, pathEnv) {
			continue
		}
		return candidate
	}
	return name
}

// trustedFetchExecutable reports whether running bin cannot run a file of the
// workspace: no hop of its symlink chain lies there, nor the interpreter its
// #! line names.
func trustedFetchExecutable(bin, workspace, pathEnv string) bool {
	seen := map[string]bool{}
	for range 16 {
		if seen[bin] {
			return false
		}
		seen[bin] = true
		for _, hop := range symlinkChain(bin) {
			if insideWorkspace(workspace, hop) {
				return false
			}
		}
		interpreter := scriptInterpreterAny(bin, pathEnv)
		if interpreter == "" {
			return true
		}
		bin = interpreter
	}
	return false
}

// scriptInterpreterAny returns the absolute interpreter the #! line of script
// names (through env too), or "".
func scriptInterpreterAny(script, pathEnv string) string {
	f, err := os.Open(script) //nolint:gosec // G304: an executable resolved from the sandbox PATH
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 256)
	n, _ := io.ReadFull(f, head)
	line, _, _ := strings.Cut(string(head[:n]), "\n")
	interpreter, ok := strings.CutPrefix(line, "#!")
	fields := strings.Fields(interpreter)
	if !ok || len(fields) == 0 {
		return ""
	}
	exe := fields[0]
	if filepath.Base(exe) == "env" {
		for _, arg := range fields[1:] {
			if !strings.HasPrefix(arg, "-") {
				return lookPathIn(arg, pathEnv)
			}
		}
		return ""
	}
	return exe
}

// fetchSpec is the sandbox of one networked fetch command.
type fetchSpec struct {
	workspace string // hidden from the fetch ("" when there is none)
	dir       string // the fetch directory: read-write, the working directory
	cache     string // the shared dependency caches, read-write ("" for none)
	readOnly  []string
	pathEnv   string // fetchPathEnv
	env       []string
}

// wrapFetchCommand wraps a networked fetch command in bubblewrap. Like every
// verification step it gets a read-only root, tmpfs /tmp, the host HOME
// hidden (toolchains re-bound read-only), a cleared environment and its own
// network namespace (the user-mode network is attached by the caller). Unlike
// a loopback step it binds no part of the workspace: the workspace path is
// covered by an empty tmpfs, and the only writable paths are the fetch
// directory (its working directory) and the shared dependency caches. HOME is
// a fresh, empty directory and every tool's config-file location points at
// nothing (networkedFetchHomeArgs).
func wrapFetchCommand(bwrapPath string, spec fetchSpec, command string, args []string) (string, []string, error) {
	layoutRef := spec.workspace
	if layoutRef == "" {
		layoutRef = spec.dir
	}
	layout := verifyHomeLayout(layoutRef, spec.pathEnv, true)
	wrapped := append(sandboxSeccompArgs(),
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run",
	)
	wrapped = append(wrapped, sandboxMaskedRoots(layoutRef)...)
	wrapped = append(wrapped, layout.beforeWorkspace...)
	if spec.workspace != "" {
		// No part of the workspace is visible, wherever it lies.
		wrapped = append(wrapped, "--tmpfs", filepath.Clean(spec.workspace))
	}
	wrapped = append(wrapped, layout.afterWorkspace...)
	for _, path := range spec.readOnly {
		wrapped = append(wrapped, "--ro-bind", path, path)
	}
	if spec.cache != "" {
		wrapped = append(wrapped, "--bind", spec.cache, spec.cache)
	}
	wrapped = append(wrapped,
		"--bind", spec.dir, spec.dir,
		"--dir", fetchSandboxHome,
		"--chdir", spec.dir,
		"--clearenv",
		"--setenv", "PATH", spec.pathEnv,
		"--setenv", "HOME", fetchSandboxHome,
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "NO_COLOR", "1",
		"--setenv", "MAKEWAND_SANDBOX", "1",
	)
	wrapped = append(wrapped, layout.setenvArgs()...)
	wrapped = append(wrapped, networkedFetchHomeArgs()...)
	for _, setting := range spec.env {
		if key, value, ok := strings.Cut(setting, "="); ok {
			wrapped = append(wrapped, "--setenv", key, value)
		}
	}
	wrapped = append(wrapped, "--unshare-net")
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM"} {
		if value := strings.TrimSpace(verifyGetenv(key)); value != "" {
			wrapped = append(wrapped, "--setenv", key, value)
		}
	}
	wrapped = append(wrapped, command)
	wrapped = append(wrapped, args...)
	return bwrapPath, wrapped, nil
}

// execFetchCommand runs one fetch command in its fetch sandbox. With
// networked it gets the user-mode network; attached is false when that could
// not be set up or attached, in which case nothing ran with the network and
// network says why. Without networked it runs loopback-only.
func (p *Project) execFetchCommand(ctx context.Context, env verifyExecEnvironment, spec fetchSpec, command string, args []string, networked bool) (result *ExecResult, network sandboxNetwork, attached bool, err error) {
	none := sandboxNetwork{kind: sandboxNetworkNone}
	wrappedCmd, wrappedArgs, err := wrapFetchCommand(env.bwrapPath, spec, command, args)
	if err != nil {
		return nil, none, false, fmt.Errorf("verification sandbox: %w", err)
	}
	// bubblewrap itself starts in the fetch directory, not the workspace.
	runner := &Project{Name: p.Name, Path: spec.dir}
	if !networked {
		result, err := runner.execSandboxed(ctx, wrappedCmd, wrappedArgs, nil)
		return result, none, true, err
	}
	userNet, network := prepareUserNetwork(wrappedArgs)
	if userNet == nil {
		return nil, network, false, nil
	}
	defer userNet.release()
	result, err = runner.execSandboxed(ctx, wrappedCmd, insertBeforeCommand(wrappedArgs, len(args), userNet.bwrapArgs()...), userNet)
	if err != nil || userNet.attached {
		return result, network, true, err
	}
	return nil, userNet.network(), false, nil
}
