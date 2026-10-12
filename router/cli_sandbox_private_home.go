package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"weak"
)

// cliPrivateCredFile is a provider credential file copied into the per-run
// private provider home. Only writeBack files are copied back to the host,
// and only when they changed inside the sandbox.
type cliPrivateCredFile struct {
	rel       string
	writeBack bool
}

var (
	cliPrivateClaudeFiles = []cliPrivateCredFile{{".credentials.json", true}}
	cliPrivateCodexFiles  = []cliPrivateCredFile{
		{"auth.json", true},
		{"version.json", false},
		{"models_cache.json", false},
		{"installation_id", false},
	}
)

// cliPrivateCredentialFiles maps a provider credential directory (an entry of
// cliProviderCredentials) to the files copied into its private copy. Token
// files are written back; identifiers and caches are copied in only.
// Configuration (settings, hooks, MCP servers, instructions) is never copied
// or written back: cliShieldProviderState binds the host copies read-only, so
// a sandboxed agent cannot plant configuration that a later host run trusts.
// Credential directories absent from this map are bound read-only.
var cliPrivateCredentialFiles = map[string][]cliPrivateCredFile{
	".claude":   cliPrivateClaudeFiles,
	".claude-2": cliPrivateClaudeFiles,
	".claude-3": cliPrivateClaudeFiles,
	".codex":    cliPrivateCodexFiles,
	".codex-2":  cliPrivateCodexFiles,
	".codex-3":  cliPrivateCodexFiles,
	".gemini": {
		{"oauth_creds.json", true},
		{"google_accounts.json", true},
		{"flow_auth.json", true},
		{"antigravity-cli/antigravity-oauth-token", true},
		{"installation_id", false},
		{"projects.json", false},
		{"antigravity/installation_id", false},
		{"antigravity-cli/installation_id", false},
	},
	".grok": {
		{"auth.json", true},
		{"agent_id", false},
		{"models_cache.json", false},
		{"version.json", false},
	},
	".aider": {
		{"analytics.json", false},
		{"installs.json", false},
	},
}

// cliPrivateFileEntries are top-level credential files copied into the
// private home without write-back. ~/.claude.json mixes account metadata with
// MCP server and per-project trust configuration, so sandbox edits to it must
// never reach the host.
var cliPrivateFileEntries = map[string]bool{
	".claude.json": true,
}

const cliPrivateCredMaxBytes = 4 << 20

// cliPrivateHomeBaseDir returns the directory under which per-run private
// homes are created. Tests replace it.
var cliPrivateHomeBaseDir = func() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "makewand", "cli-homes")
}

// cliPrivateHomeFinished is a test hook called after write-back and cleanup.
var cliPrivateHomeFinished atomic.Pointer[func(dir string)]

type cliPrivateCred struct {
	hostPath  string // canonical host path written back to
	privPath  string
	writeBack bool
	existed   bool
	orig      []byte
	mode      os.FileMode
}

// cliPrivateHome is a per-run 0700 directory: home is bound as the sandbox
// HOME, codex-home (when used) as the selected CODEX_HOME.
type cliPrivateHome struct {
	dir   string
	home  string
	creds []*cliPrivateCred
	once  sync.Once
}

func newCLIPrivateHome(workspace string) (*cliPrivateHome, error) {
	var dir string
	if base := cliPrivateHomeBaseDir(); base != "" && filepath.IsAbs(base) &&
		!cliPathWithin(workspace, base) && !cliPathWithin(base, workspace) {
		if err := os.MkdirAll(base, 0o700); err == nil {
			dir, _ = os.MkdirTemp(base, "run-")
		}
	}
	if dir == "" {
		// The sandbox's /tmp is a tmpfs, so a host temp directory stays
		// invisible inside it.
		var err error
		dir, err = os.MkdirTemp("", "makewand-cli-home-")
		if err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	h := &cliPrivateHome{dir: dir, home: filepath.Join(dir, "home")}
	if err := os.Mkdir(h.home, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return h, nil
}

func (h *cliPrivateHome) cleanup() {
	h.once.Do(func() {
		_ = os.RemoveAll(h.dir)
		if f := cliPrivateHomeFinished.Load(); f != nil {
			(*f)(h.dir)
		}
	})
}

// addProviderEntry exposes one provider credential entry of the host home
// (hostPath = home/entry) inside the private sandbox HOME.
func (h *cliPrivateHome) addProviderEntry(args []string, provider, home, entry, hostPath string, fi os.FileInfo, workspace string) ([]string, error) {
	files, private := cliPrivateCredentialFiles[entry]
	switch {
	case fi.IsDir() && private:
		privRoot := filepath.Join(h.home, filepath.FromSlash(entry))
		if err := os.MkdirAll(privRoot, 0o700); err != nil {
			return args, err
		}
		for _, f := range files {
			if err := h.copyIn(hostPath, privRoot, f); err != nil {
				return args, err
			}
		}
		// hostPath is also the sandbox path of the private copy.
		return cliShieldProviderState(args, provider, hostPath, workspace, false, true), nil
	case !fi.IsDir() && cliPrivateFileEntries[entry]:
		return args, h.copyIn(home, h.home, cliPrivateCredFile{rel: entry})
	default:
		return cliAppendRoBindIfNotPresent(args, hostPath), nil
	}
}

// addCodexHome prepares a private copy of an explicitly selected CODEX_HOME
// and returns the host directory to bind at realCodexHome.
func (h *cliPrivateHome) addCodexHome(realCodexHome string) (string, error) {
	privCodex := filepath.Join(h.dir, "codex-home")
	if err := os.Mkdir(privCodex, 0o700); err != nil {
		return "", err
	}
	for _, f := range cliPrivateCodexFiles {
		if err := h.copyIn(realCodexHome, privCodex, f); err != nil {
			return "", err
		}
	}
	return privCodex, nil
}

// copyIn copies hostRoot/rel to privRoot/rel and, for write-back files,
// records the snapshot used to detect changes.
func (h *cliPrivateHome) copyIn(hostRoot, privRoot string, f cliPrivateCredFile) error {
	rel := filepath.FromSlash(f.rel)
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("invalid credential path %q", f.rel)
	}
	hostFile := filepath.Join(hostRoot, rel)
	privFile := filepath.Join(privRoot, rel)

	canonical := ""
	var data []byte
	mode := os.FileMode(0o600)
	existed := false
	if fi, err := os.Stat(hostFile); err == nil { //nolint:gosec // G703: provider credential path under the user's home
		if !fi.Mode().IsRegular() || fi.Size() > cliPrivateCredMaxBytes {
			return nil // Not a credential file we manage; leave it out.
		}
		real, err := filepath.EvalSymlinks(hostFile)
		if err != nil {
			return nil
		}
		data, err = cliReadRegularFile(real)
		if err != nil {
			return nil
		}
		canonical = real
		existed = true
		mode = fi.Mode().Perm() & 0o600
		if mode == 0 {
			mode = 0o600
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil
	} else if f.writeBack {
		// A missing token file may be created by a login inside the sandbox;
		// it can be written back only into an existing host directory.
		parent, err := filepath.EvalSymlinks(filepath.Dir(hostFile))
		if err != nil {
			return nil
		}
		canonical = filepath.Join(parent, filepath.Base(hostFile))
	}

	if existed {
		if err := os.MkdirAll(filepath.Dir(privFile), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(privFile, data, 0o600); err != nil {
			return err
		}
	}
	if f.writeBack && canonical != "" {
		h.creds = append(h.creds, &cliPrivateCred{
			hostPath:  canonical,
			privPath:  privFile,
			writeBack: true,
			existed:   existed,
			orig:      data,
			mode:      mode,
		})
	}
	return nil
}

// cliPendingStatusWrites maps a wrapped (not yet started) command to the
// function closing the parent's copy of bwrap's --json-status-fd. The parent
// copy may only be closed after exec has handed the descriptor to the child,
// so the starter calls cliAfterStart on its own goroutine; closing it from the
// watcher would race with (*exec.Cmd).Start reading the descriptor.
var cliPendingStatusWrites sync.Map // weak.Pointer[exec.Cmd] -> func()

// cliAfterStart must be called after a successful Start of a command returned
// by wrapCLICommandWithSandbox. It is a no-op for commands without a private
// home.
func cliAfterStart(cmd *exec.Cmd) {
	if f, ok := cliPendingStatusWrites.LoadAndDelete(weak.Make(cmd)); ok {
		f.(func())()
	}
}

// watch arranges write-back and cleanup after the sandbox exits. statusRead
// is the parent end of bwrap's --json-status-fd. The parent's statusWrite is
// closed by cliAfterStart, or when the wrapped command is garbage collected
// unstarted; the reader then sees EOF once every sandbox process holding the
// descriptor has exited.
func (h *cliPrivateHome) watch(wrapped *exec.Cmd, statusRead, statusWrite *os.File) {
	var closeOnce sync.Once
	closeWrite := func() { closeOnce.Do(func() { _ = statusWrite.Close() }) }
	key := weak.Make(wrapped)
	cliPendingStatusWrites.Store(key, closeWrite)
	runtime.AddCleanup(wrapped, func(key weak.Pointer[exec.Cmd]) {
		if f, ok := cliPendingStatusWrites.LoadAndDelete(key); ok {
			f.(func())()
		}
	}, key)

	go func() {
		started := false
		dec := json.NewDecoder(statusRead)
		for {
			var status map[string]any
			if err := dec.Decode(&status); err != nil {
				break
			}
			if _, ok := status["child-pid"]; ok {
				started = true
			}
		}
		_, _ = io.Copy(io.Discard, statusRead)
		_ = statusRead.Close()
		if started {
			h.writeBack()
		}
		h.cleanup()
	}()
}

// writeBack copies changed credential files from the private home to the
// host, atomically. A file is written back only if the private copy is a
// regular file reached without symlinks, is bounded in size (and valid JSON
// for .json files), and the host file still matches the snapshot taken at
// launch, so a concurrent host refresh is never clobbered.
func (h *cliPrivateHome) writeBack() {
	for _, c := range h.creds {
		if !c.writeBack {
			continue
		}
		data, ok := cliReadPrivateRegular(h.dir, c.privPath)
		if !ok {
			continue
		}
		if c.existed && bytes.Equal(data, c.orig) {
			continue
		}
		if strings.HasSuffix(c.hostPath, ".json") && !json.Valid(data) {
			continue
		}
		current, err := cliReadRegularFile(c.hostPath)
		switch {
		case err == nil:
			if !c.existed || !bytes.Equal(current, c.orig) {
				continue
			}
		case errors.Is(err, os.ErrNotExist):
			if c.existed {
				continue
			}
		default:
			continue
		}
		_ = cliAtomicWriteFile(c.hostPath, data, c.mode)
	}
}

// cliReadPrivateRegular reads path, which must lie under root, refusing
// symlinks in any component below root and non-regular files.
func cliReadPrivateRegular(root, path string) ([]byte, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, false
	}
	cur := root
	parts := strings.Split(rel, string(os.PathSeparator))
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return nil, false
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return nil, false
		}
		if i == len(parts)-1 && (!fi.Mode().IsRegular() || fi.Size() > cliPrivateCredMaxBytes) {
			return nil, false
		}
	}
	data, err := cliReadRegularFile(cur)
	if err != nil {
		return nil, false
	}
	return data, true
}

// cliReadRegularFile reads a bounded regular file.
func cliReadRegularFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: provider credential path chosen by makewand, not request input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, cliPrivateCredMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > cliPrivateCredMaxBytes {
		return nil, fmt.Errorf("%s exceeds credential size limit", path)
	}
	return data, nil
}

func cliAtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".makewand-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}
