package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"weak"
)

// cliPrivateCredFile is a provider credential file copied into the per-run
// private provider home. Only writeBack files are merged back into the host
// file, and only the token-refresh fields of a same-account refresh (see
// writeBack).
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
// files are merged back (see writeBack); identifiers and caches are copied in
// only.
// Configuration (settings, hooks, MCP servers, instructions) is never copied
// or written back: cliShieldProviderState binds the host copies read-only, so
// a sandboxed agent cannot plant configuration that a later host run trusts.
// agy settings are the exception, handled by writeAgySanitizedSettings: a
// sanitized copy is written into the private home and bound read-only, and
// the host file is only read. Credential directories absent from this map
// are bound read-only.
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
		// A browser cookie jar (a JSON array of name/value/domain/path/
		// expires/... cookies): its values are opaque session cookies that
		// cannot be tied to the host account, and no provider CLI refreshes
		// it, so it is copied in only.
		{"flow_auth.json", false},
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

// cliAgySettingsRels are the agy settings files forced to useG1Credits=false.
// Python keeps the same pair (AGY_SETTINGS_RELS): ~/.gemini/settings.json and
// the antigravity-cli file the CLI actually reads.
var cliAgySettingsRels = []string{
	"settings.json",
	"antigravity-cli/settings.json",
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

// cliWritePrivateNoFollowFn writes one file inside a private home. Tests
// replace it to force the fail-closed path.
var cliWritePrivateNoFollowFn = cliWritePrivateNoFollow

// cliPrivateHomeFinished is a test hook called after write-back and cleanup.
var cliPrivateHomeFinished atomic.Pointer[func(dir string)]

// cliPrivateHomeWarnings is a test hook called with the warnings of the
// write-back after a sandbox exited, before cleanup.
var cliPrivateHomeWarnings atomic.Pointer[func(dir string, warnings []string)]

type cliPrivateCred struct {
	hostPath  string // canonical host path written back to
	privPath  string
	writeBack bool
	existed   bool
	orig      []byte
	mode      os.FileMode
}

// cliPrivateHome is a per-run 0700 directory. Writable runs bind home as the
// sandbox HOME (homeBound); read-only runs keep an empty tmpfs HOME and bind
// each private provider directory below home at its host path. codex-home
// (when used) is bound as the selected CODEX_HOME.
type cliPrivateHome struct {
	dir       string
	home      string
	homeBound bool
	creds     []*cliPrivateCred
	once      sync.Once
	// agySettingsMounted records that sanitized agy settings were written
	// and bound; an agy launch without them is refused.
	agySettingsMounted bool
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
		if !h.homeBound {
			args = append(args, "--bind", privRoot, hostPath)
		}
		// hostPath is also the sandbox path of the private copy. The host
		// settings file is bound read-only first; an agy launch then shadows
		// it with the sanitized copy.
		args = cliShieldProviderState(args, provider, hostPath, workspace, false, true)
		return h.overlayAgySanitizedSettings(args, provider, entry, privRoot, hostPath)
	case !fi.IsDir() && cliPrivateFileEntries[entry]:
		if !h.homeBound {
			return cliAppendRoBindIfNotPresent(args, hostPath), nil
		}
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

// addAbsentProviderEntry handles a provider credential directory the host
// lacks (hostPath = home/entry): its write-back files are watched in a
// private copy, so a login made inside the sandbox is reported after the run
// instead of vanishing with the sandbox HOME. It is never written back (there
// is no host account to verify it against) and nothing is created on the
// host. Writable runs find the directory in the private HOME; read-only runs
// bind an empty private directory at hostPath, where bwrap can create that
// mount point inside the HOME tmpfs (cliMountPointCreatable).
//
// An agy launch whose .gemini directory is absent still gets sanitized
// settings (useG1Credits=false). If those files cannot be mounted where the
// CLI reads them, the launch is refused.
func (h *cliPrivateHome) addAbsentProviderEntry(args []string, provider, entry, hostPath string) ([]string, error) {
	files := cliPrivateCredentialFiles[entry]
	sanitize := cliProviderForcesAgyCreditsOff(provider) && entry == ".gemini"
	if !sanitize && !slices.ContainsFunc(files, func(f cliPrivateCredFile) bool { return f.writeBack }) {
		return args, nil
	}
	privRoot := filepath.Join(h.home, filepath.FromSlash(entry))
	if !h.homeBound {
		if !cliMountPointCreatable(args, hostPath) {
			if sanitize {
				return args, fmt.Errorf("refusing to start: cannot mount sanitized agy settings at %s", hostPath)
			}
			return args, nil
		}
		if err := os.MkdirAll(privRoot, 0o700); err != nil {
			return args, err
		}
		args = append(args, "--bind", privRoot, hostPath)
	}
	for _, f := range files {
		if !f.writeBack {
			continue
		}
		if err := h.copyIn(hostPath, privRoot, f); err != nil {
			return args, err
		}
	}
	if !sanitize {
		return args, nil
	}
	return h.overlayAgySanitizedSettings(args, provider, entry, privRoot, hostPath)
}

// cliMountPointCreatable reports whether bwrap can create the missing mount
// point path after the mounts in args: the last of them covering its parent
// directory is a tmpfs (the empty sandbox HOME), not a read-only or host bind.
func cliMountPointCreatable(args []string, path string) bool {
	parent := filepath.Dir(filepath.Clean(path))
	covering := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bind", "--ro-bind", "--dev-bind", "--symlink", "--setenv":
			if i+2 < len(args) {
				if args[i] != "--setenv" && cliPathWithin(args[i+2], parent) {
					covering = args[i]
				}
				i += 2
			}
		case "--tmpfs", "--proc", "--dev":
			if i+1 < len(args) {
				if cliPathWithin(args[i+1], parent) {
					covering = args[i]
				}
				i++
			}
		}
	}
	return covering == "--tmpfs"
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
	// Credential paths are absolute host paths built by makewand; refuse
	// anything relative or carrying parent references outright.
	if !filepath.IsAbs(hostFile) || strings.Contains(hostFile, "..") {
		return fmt.Errorf("invalid credential path %q", hostFile)
	}

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
		// A missing token file may be created by a login inside the sandbox,
		// also in a directory the host lacks; writeBack reports it but never
		// writes it back (there is no host account to verify it against).
		canonical = hostFile
		if parent, err := filepath.EvalSymlinks(filepath.Dir(hostFile)); err == nil {
			canonical = filepath.Join(parent, filepath.Base(hostFile))
		}
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

// cliProviderForcesAgyCreditsOff reports whether this launch reads agy
// settings and must see useG1Credits=false. Python's ProviderHome aliases
// gemini to agy; antigravity uses the same ~/.gemini directory here.
func cliProviderForcesAgyCreditsOff(provider string) bool {
	switch strings.TrimSuffix(strings.ToLower(strings.TrimSpace(provider)), "-cli") {
	case "agy", "gemini", "antigravity":
		return true
	default:
		return false
	}
}

// overlayAgySanitizedSettings writes sanitized settings into privRoot and
// read-only binds them at hostRoot. A non-agy provider or a different entry
// is left unchanged. A write failure refuses the launch; the host file is
// never opened for writing.
func (h *cliPrivateHome) overlayAgySanitizedSettings(args []string, provider, entry, privRoot, hostRoot string) ([]string, error) {
	if !cliProviderForcesAgyCreditsOff(provider) || entry != ".gemini" {
		return args, nil
	}
	if err := h.writeAgySanitizedSettings(privRoot, hostRoot); err != nil {
		return args, err
	}
	h.agySettingsMounted = true
	return cliRoBindAgySanitizedSettings(args, privRoot, hostRoot), nil
}

// cliAgyHostCreditsDisabled returns nil only when both host agy settings
// files under every home an unsandboxed agy may read prove useG1Credits is
// false. Without a sandbox there is no sanitized copy, and a missing,
// unreadable or unproven file could let agy spend G1 credits.
func cliAgyHostCreditsDisabled(childEnv []string) error {
	var homes []string
	if h := cliEnvValue(childEnv, "HOME"); h != "" {
		homes = append(homes, filepath.Clean(h))
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" && !slices.Contains(homes, filepath.Clean(h)) {
		homes = append(homes, filepath.Clean(h))
	}
	if len(homes) == 0 {
		return errors.New("cannot locate the home directory holding agy settings")
	}
	for _, home := range homes {
		for _, rel := range cliAgySettingsRels {
			path := filepath.Join(home, ".gemini", filepath.FromSlash(rel))
			raw, ok := cliReadAgyHostSettings(path)
			if !ok {
				return fmt.Errorf("%s is missing or unreadable", path)
			}
			obj, ok := cliParseAgySettingsObject(raw)
			if !ok {
				return fmt.Errorf("%s is not a JSON object", path)
			}
			if on, isBool := obj["useG1Credits"].(bool); !isBool || on {
				return fmt.Errorf("%s does not set \"useG1Credits\": false", path)
			}
		}
	}
	return nil
}

// writeAgySanitizedSettings writes both agy settings files into privRoot.
// Missing, unreadable, or non-object host files become a minimal
// {"useG1Credits": false} document. Other keys of a JSON object are kept.
func (h *cliPrivateHome) writeAgySanitizedSettings(privRoot, hostRoot string) error {
	if h == nil || h.dir == "" || !cliPathWithin(h.dir, privRoot) {
		return fmt.Errorf("refusing to start: private agy settings path is outside the private home")
	}
	if hostRoot != "" && cliPathWithin(hostRoot, privRoot) {
		return fmt.Errorf("refusing to start: private agy settings path is inside the host provider directory")
	}
	for _, rel := range cliAgySettingsRels {
		relPath := filepath.FromSlash(rel)
		if !filepath.IsLocal(relPath) {
			return fmt.Errorf("refusing to start: invalid agy settings path %q", rel)
		}
		dst := filepath.Join(privRoot, relPath)
		if !cliPathWithin(privRoot, dst) {
			return fmt.Errorf("refusing to start: agy settings path %q escapes the private root", rel)
		}
		hostFile := ""
		if hostRoot != "" {
			hostFile = filepath.Join(hostRoot, relPath)
			if dst == hostFile {
				return fmt.Errorf("refusing to start: agy settings destination is the host file")
			}
		}
		content, err := cliSanitizedAgySettingsBytes(hostFile)
		if err != nil {
			return fmt.Errorf("refusing to start: cannot prepare private agy settings (%s): %w", rel, err)
		}
		if err := cliWritePrivateNoFollowFn(dst, content); err != nil {
			return fmt.Errorf("refusing to start: cannot write private agy settings (%s): %w", rel, err)
		}
	}
	return nil
}

// cliSanitizedAgySettingsBytes is the host agy settings document with
// useG1Credits forced to false. A missing, unreadable, or non-object file
// yields a minimal document. The host file is not modified.
func cliSanitizedAgySettingsBytes(hostPath string) ([]byte, error) {
	doc := map[string]any{}
	if raw, ok := cliReadAgyHostSettings(hostPath); ok {
		if obj, ok := cliParseAgySettingsObject(raw); ok {
			doc = obj
		}
	}
	doc["useG1Credits"] = false
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cliReadAgyHostSettings reads a host settings file, following a symlink the
// way Python's _read_host_regular does. Missing, non-regular, oversized, and
// unreadable files return ok=false so the caller writes a minimal document.
func cliReadAgyHostSettings(path string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, false
	}
	data, err := cliReadRegularFile(real)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil, false
	}
	return data, true
}

// cliParseAgySettingsObject parses one JSON object. Trailing junk and any
// non-object value are rejected so the caller falls back to a minimal document.
func cliParseAgySettingsObject(raw []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	obj, ok := parsed.(map[string]any)
	if !ok || obj == nil {
		return nil, false
	}
	return obj, true
}

// cliWritePrivateNoFollow writes content to path inside a directory makewand
// created. A symlink or other non-regular file at path is refused. The bytes
// are written to a new temp file and renamed into place, so the write does
// not follow a symlink at path.
func cliWritePrivateNoFollow(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agy-settings-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := cliReplacePrivateFile(tmpName, path); err != nil {
		return err
	}
	done = true
	_ = os.Chmod(path, 0o600)
	return nil
}

// cliReplacePrivateFile moves src onto dst. Windows will not rename onto an
// existing file, so a regular file already at dst is removed first. A symlink
// at dst is never followed.
func cliReplacePrivateFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if fi, statErr := os.Lstat(dst); statErr != nil {
		return err
	} else if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", dst)
	} else if rmErr := os.Remove(dst); rmErr != nil {
		return rmErr
	}
	return os.Rename(src, dst)
}

// cliPendingStatusWrites maps a wrapped (not yet started) command to the
// function closing the parent's copy of bwrap's --json-status-fd. The parent
// copy may only be closed after exec has handed the descriptor to the child,
// so the starter calls cliAfterStart on its own goroutine; closing it from the
// watcher would race with (*exec.Cmd).Start reading the descriptor.
var cliPendingStatusWrites sync.Map // weak.Pointer[exec.Cmd] -> func()

// cliPendingWriteBacks maps a wrapped command to the progress of its private
// home's write-back (cliAfterWait).
var cliPendingWriteBacks sync.Map // weak.Pointer[exec.Cmd] -> *cliPendingWriteBack

// cliPendingWriteBack tracks the write-back of one private home: written is
// closed once the credential write-back has finished (or was not needed),
// done once the private home has been removed as well.
type cliPendingWriteBack struct {
	dir           string
	written, done chan struct{}
}

// cliWriteBackSlack is what cliAfterWait allows a private home beyond one
// credential lock wait (cliCredentialLockWait). Tests shorten it.
var cliWriteBackSlack = 5 * time.Second

// cliWriteBackWait bounds how long cliAfterWait waits for a private home. A
// write-back takes milliseconds; the bound covers a write-back that waits for
// the credential lock of another makewand write-back, while a sandbox process
// that never lets go of the status descriptor (stuck in an uninterruptible
// sleep, say) or a hung filesystem cannot hang the run.
func cliWriteBackWait() time.Duration {
	return cliCredentialLockWait + cliWriteBackSlack
}

// cliAfterStart must be called after a successful Start of a command returned
// by wrapCLICommandWithSandbox. It is a no-op for commands without a private
// home.
func cliAfterStart(cmd *exec.Cmd) {
	if f, ok := cliPendingStatusWrites.LoadAndDelete(weak.Make(cmd)); ok {
		f.(func())()
	}
}

// cliAfterWait must be called once Wait has returned for a command returned
// by wrapCLICommandWithSandbox, and after a failed Start of one (a command
// that never ran needs no write-back; its private home is removed at once).
// It blocks until the credential write-back and the removal of the command's
// private home have finished, so a refreshed token is on the host before the
// run returns: a process that exits right after the run, or a run started
// next, would otherwise miss it or copy in the tokens the refresh rotated.
// The wait is bounded (cliWriteBackWait); a write-back still running then is
// reported and finishes in the background. It returns at once for commands
// without a private home and when called again.
func cliAfterWait(cmd *exec.Cmd) {
	cliAfterStart(cmd) // the parent's status descriptor, should a caller have skipped it
	v, ok := cliPendingWriteBacks.LoadAndDelete(weak.Make(cmd))
	if !ok {
		return
	}
	pending := v.(*cliPendingWriteBack)
	wait := cliWriteBackWait()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-pending.done:
		return
	case <-timer.C:
	}
	select {
	case <-pending.written:
		// Only the removal of the private home is still running.
	default:
		fmt.Fprintf(os.Stderr, "makewand: WARNING: the credential write-back of sandbox home %s has not finished after %s "+
			"(a sandbox process may still be running); continuing without it, so a token the provider refreshed in the sandbox "+
			"reaches the host only once it finishes\n", pending.dir, wait)
	}
}

// watch arranges write-back and cleanup after the sandbox exits. statusRead
// is the parent end of bwrap's --json-status-fd. The parent's statusWrite is
// closed by cliAfterStart, or when the wrapped command is garbage collected
// unstarted; the reader then sees EOF once every sandbox process holding the
// descriptor has exited. cliAfterWait waits for the write-back.
func (h *cliPrivateHome) watch(wrapped *exec.Cmd, statusRead, statusWrite *os.File) {
	var closeOnce sync.Once
	closeWrite := func() { closeOnce.Do(func() { _ = statusWrite.Close() }) }
	key := weak.Make(wrapped)
	cliPendingStatusWrites.Store(key, closeWrite)
	pending := &cliPendingWriteBack{dir: h.dir, written: make(chan struct{}), done: make(chan struct{})}
	cliPendingWriteBacks.Store(key, pending)
	runtime.AddCleanup(wrapped, func(key weak.Pointer[exec.Cmd]) {
		if f, ok := cliPendingStatusWrites.LoadAndDelete(key); ok {
			f.(func())()
		}
		cliPendingWriteBacks.Delete(key)
	}, key)

	go func() {
		defer close(pending.done)
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
			warnings := h.writeBack()
			if f := cliPrivateHomeWarnings.Load(); f != nil {
				(*f)(h.dir, warnings)
			}
		}
		close(pending.written)
		h.cleanup()
	}()
}

// writeBack merges changed credential files from the private home into the
// host files, atomically. A file is considered only if the private copy is a
// regular file reached without symlinks and bounded in size, and the host
// file still matches the snapshot taken at launch, so a concurrent host
// refresh is never clobbered (checked again right before the rename, see
// cliReplaceHostCredential). Only a same-account token refresh reaches the
// host (cliMergeCredential), so a sandboxed agent can neither swap the host
// account nor redirect it or log it out. It returns the warnings printed for
// refused, failed or partly ignored changes, and for a copy that existed at
// launch but is deleted, replaced by a non-regular file, oversized or
// unreadable: it may have held a rotated refresh token.
func (h *cliPrivateHome) writeBack() []string {
	var warnings []string
	for _, c := range h.creds {
		if !c.writeBack {
			continue
		}
		data, problem := cliReadPrivateCredential(h.dir, c.privPath)
		switch {
		case problem == "":
		case c.existed:
			warnings = append(warnings, cliWarnCredential("not writing back sandbox credential %s: the sandbox copy %s, so a token refresh made in the sandbox cannot be verified; host copy kept (if the provider rotated its tokens, log in again on the host)",
				c.hostPath, cliPrivateCopyProblemText[problem]))
			continue
		case problem != "missing":
			warnings = append(warnings, cliWarnCredential("not writing back sandbox credential %s: the host had no such credential at launch and the sandbox copy %s (log in on the host instead)",
				c.hostPath, cliPrivateCopyProblemText[problem]))
			continue
		default:
			continue // absent at launch and still absent: nothing happened
		}
		if c.existed && bytes.Equal(data, c.orig) {
			continue
		}
		current, err := cliReadHostCredential(c.hostPath)
		if unchanged := (c.existed && err == nil && bytes.Equal(current.data, c.orig)) ||
			(!c.existed && errors.Is(err, os.ErrNotExist)); !unchanged {
			warnings = append(warnings, cliWarnCredential("host credential %s changed during the run; host copy kept, sandbox copy not written back", c.hostPath))
			continue
		}
		m := cliMergeCredential(c.orig, data, c.existed)
		if len(m.problems) > 0 {
			warnings = append(warnings, cliWarnCredential("not writing back sandbox credential %s: %s; host copy kept", c.hostPath, m.problemText()))
			continue
		}
		outcome := "host copy kept"
		if m.data != nil {
			switch err := cliReplaceHostCredential(c.hostPath, m.data, c.mode, current); {
			case err == nil:
				outcome = "refreshed tokens written back"
			case errors.Is(err, errCLICredentialChanged):
				warnings = append(warnings, cliWarnCredential("host credential %s changed during the run; host copy kept, sandbox copy not written back", c.hostPath))
			case errors.Is(err, errCLICredentialLocked):
				warnings = append(warnings, cliWarnCredential("host credential %s is being written back by another makewand process (lock wait timed out); host copy kept", c.hostPath))
			default:
				warnings = append(warnings, cliWarnCredential("failed to write back sandbox credential %s: %v; host copy kept", c.hostPath, err))
			}
		}
		if len(m.ignored) > 0 {
			warnings = append(warnings, cliWarnCredential("sandbox credential %s: ignored added or removed fields %s (only token refreshes are written back); %s",
				c.hostPath, cliCredentialFieldList(m.ignored), outcome))
		}
	}
	return warnings
}

// cliWarnCredential prints a credential write-back warning about hostPath
// and returns it.
func cliWarnCredential(format, hostPath string, args ...any) string {
	msg := fmt.Sprintf(format, append([]any{hostPath}, args...)...)
	fmt.Fprintf(os.Stderr, "makewand: WARNING: %s\n", msg)
	return msg
}

// cliCredentialMaxJSONDepth bounds the nesting accepted in credential JSON.
const cliCredentialMaxJSONDepth = 64

var (
	// cliCredentialProtectedKeyRe matches the normalized keys of fields a
	// token refresh never changes: e-mails, API keys and other secrets,
	// credentials outside the refresh allowlist, endpoints, auth modes and
	// account identifiers.
	cliCredentialProtectedKeyRe = regexp.MustCompile(`email|apikey|secret|passw|privatekey|clientid|^key$|token|cookie|credential` +
		`|url$|uri$|issuer|endpoint|audience|^(?:iss|aud|host|hostname|domain)$` +
		`|mode$|method$|mechanism|obtainedvia|granttype|authtype` +
		`|^(?:sub|subject|uid|id|uuid|user|account|org|organization|team|tenant|principal|owner)$` +
		`|(?:account|user|org|organization|team|tenant|principal|owner|subject)(?:id|uuid|guid)$`)
	// cliCredentialBenignKeyRe matches metadata that is never protected, even
	// where its name says token or URL: scopes, profile pictures, times.
	cliCredentialBenignKeyRe = regexp.MustCompile(`scope|avatar|image|picture|photo|icon|expir|lastrefresh`)
	cliCredentialEmailRe     = regexp.MustCompile(`^[^@\x00-\x20]+@[^@\x00-\x20]+\.[^@\x00-\x20]+$`)
	cliCredentialURLRe       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^\x00-\x20]+$`)
	cliCredentialJWTRe       = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*$`)
	cliCredentialRFC3339Re   = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt]([0-9]{2}):([0-9]{2}):([0-9]{2})` +
		`(?:\.([0-9]{1,9}))?(?:[Zz]|([+-])([0-9]{2}):([0-9]{2}))$`)
	// cliCredentialAccountClaims are the JWT claims naming the account; they
	// are compared at any depth of the claims (e.g. inside the
	// https://api.openai.com/auth object).
	cliCredentialAccountClaims = map[string]bool{"sub": true, "email": true, "user_id": true, "principal_id": true,
		"team_id": true, "account_id": true, "chatgpt_account_id": true, "chatgpt_user_id": true, "chatgpt_account_user_id": true}
	// cliCredentialStaticRefreshPrefixes mark refresh tokens that a refresh
	// never rotates: Google's (golang.org/x/oauth2 and google-auth-library
	// keep the old one), so a different one comes from a new login.
	cliCredentialStaticRefreshPrefixes = []string{"1//"}
)

// cliCredentialProblemText maps a refusal code to its warning text: the
// first %s is the field path, a second one the detail.
var cliCredentialProblemText = map[string]string{
	"absent":             "the host had no such credential at launch; a login made inside the sandbox is not written back (log in on the host instead)",
	"host-not-object":    "the host copy is not a JSON object, so the account cannot be verified",
	"sandbox-not-object": "the sandbox copy is not a valid JSON object",
	"remove":             "the sandbox copy removes %s (logout or tampering)",
	"clear":              "the sandbox copy clears or retypes %s (logout or tampering)",
	"protected":          "the sandbox copy changes %s, which a token refresh never changes (account swap or redirection)",
	"jwt-shape":          "the sandbox copy swaps %s between a JWT and an opaque token",
	"jwt-account":        "the sandbox copy holds %s of another account (claim %s differs)",
	"token-type":         "the sandbox copy changes the token type %s",
	"bad-expiry":         "the sandbox copy has an unreadable time in %s",
	"backwards":          "the sandbox copy moves %s backwards (stale or replayed credential)",
	"refresh-only":       "the sandbox copy changes the refresh token %s without a new access token (token substitution)",
	"no-refresh-token":   "the sandbox copy changes the access token %s although the credential holds no refresh token (a new login, not a refresh)",
	"google-refresh":     "the sandbox copy changes the Google refresh token %s, which a refresh never rotates (a new login)",
}

// cliCredentialMerge is the outcome of cliMergeCredential.
type cliCredentialMerge struct {
	data     []byte                 // bytes to write to the host; nil: nothing to write
	problems []cliCredentialProblem // why nothing may be written
	ignored  []string               // protected fields added or removed in the sandbox copy
}

// cliCredentialProblem is one reason for refusing a write-back: a code of
// cliCredentialProblemText, the field path and an optional detail.
type cliCredentialProblem struct{ path, code, detail string }

// problemText renders at most five problems for a warning.
func (m cliCredentialMerge) problemText() string {
	const maxListed = 5
	var texts []string
	for i, p := range m.problems {
		if i == maxListed {
			texts = append(texts, "...")
			break
		}
		text := cliCredentialProblemText[p.code]
		args := []any{p.path, p.detail}[:strings.Count(text, "%s")]
		texts = append(texts, fmt.Sprintf(text, args...))
	}
	return strings.Join(texts, "; ")
}

// cliMergeCredential merges the sandbox copy cur of a credential into the
// host document orig (existed false: absent at launch). Only token-refresh
// leaves of host objects that hold tokens are copied: access, refresh and id
// tokens (grok's access JWT "key" included) and, as part of a refresh of
// their object or of an object below it, the token type and the expiry and
// last-refresh times. The merge has problems, and nothing is written, unless
// the change is a same-account refresh:
//   - either document is not a strict JSON object, or the host had none;
//   - a token or an object holding tokens is removed, emptied or retyped;
//   - a protected field (cliCredentialProtectedKeyRe: e-mail, key, endpoint,
//     auth mode, account id; e-mail and URL values) changes value;
//   - a JWT-shaped token becomes opaque or vice versa, or a JWT's account
//     claims (cliCredentialAccountClaims) change;
//   - a time moves backwards or is unreadable, or the token type changes;
//   - a refresh token changes without the access token next to it, an access
//     token changes in an object without a refresh token (a new login), or a
//     Google refresh token, which a refresh never rotates, changes.
//
// mintedKeys names keys whose value the provider CLI mints itself from the
// login (the Python engine's muse api_key; none of this engine's providers
// has one). Only inside a host object in the login shape
// (cliCredentialMintShape: an OAuth access token, no key mechanism) is such a
// key minted: a changed one is a re-mint and is copied on its own, under the
// token rules (a non-empty string, same JWT shape and account claims); a
// removed one keeps the host value. Anywhere else (a saved or pasted API key)
// it stays a protected field.
//
// Other fields keep their host value: benign metadata (plan, rate tier,
// profile, scopes) silently, protected fields the sandbox adds or removes are
// listed in ignored. Opaque tokens cannot be tied to an account offline.
func cliMergeCredential(orig, cur []byte, existed bool, mintedKeys ...string) cliCredentialMerge {
	refuse := func(code string) cliCredentialMerge {
		return cliCredentialMerge{problems: []cliCredentialProblem{{code: code}}}
	}
	if !existed {
		return refuse("absent")
	}
	oldDoc, err := cliDecodeCredentialJSON(orig)
	oldObj, ok := oldDoc.(map[string]any)
	if err != nil || !ok {
		return refuse("host-not-object")
	}
	newDoc, err := cliDecodeCredentialJSON(cur)
	newObj, ok := newDoc.(map[string]any)
	if err != nil || !ok {
		return refuse("sandbox-not-object")
	}
	w := &cliCredentialWalk{minted: map[string]bool{}}
	for _, k := range mintedKeys {
		w.minted[cliCredentialNormKey(k)] = true
	}
	w.walk(oldObj, newObj, nil)
	for _, c := range w.changes {
		w.check(c)
	}
	inRefresh := map[string]bool{} // objects refreshed, or above a refreshed one
	for _, g := range w.groups {
		if g.refreshChanged != "" && g.accessChanged == "" {
			w.problems = append(w.problems, cliCredentialProblem{g.refreshChanged, "refresh-only", ""})
		}
		if g.accessChanged != "" && !g.hasRefresh {
			w.problems = append(w.problems, cliCredentialProblem{g.accessChanged, "no-refresh-token", ""})
		}
		if g.staticRefresh {
			w.problems = append(w.problems, cliCredentialProblem{g.refreshChanged, "google-refresh", ""})
		}
		if g.accessChanged != "" {
			for i := 0; i <= len(g.path); i++ {
				inRefresh[cliCredentialPathKey(g.path[:i])] = true
			}
		}
	}
	m := cliCredentialMerge{ignored: cliSortedUnique(w.ignored)}
	if len(w.problems) > 0 {
		sort.Slice(w.problems, func(i, j int) bool {
			a, b := w.problems[i], w.problems[j]
			if a.path != b.path {
				return a.path < b.path
			}
			if a.code != b.code {
				return a.code < b.code
			}
			return a.detail < b.detail
		})
		m.problems = w.problems
		return m
	}
	merged := cliCredentialClone(oldObj).(map[string]any)
	for _, c := range w.changes {
		if c.applies(inRefresh) {
			cliCredentialObjectAt(merged, c.obj)[c.key] = c.cur
		}
	}
	switch {
	case cliCredentialSameValue(merged, oldObj):
	case cliCredentialSameValue(merged, newObj):
		m.data = cur // keep the provider's own formatting
	default:
		if m.data, err = cliEncodeCredential(merged, orig); err != nil {
			return refuse("sandbox-not-object")
		}
	}
	return m
}

// cliCredentialWalk collects the differences between the host and sandbox
// credential documents.
type cliCredentialWalk struct {
	minted   map[string]bool // normalized minted keys (see cliMergeCredential)
	changes  []cliCredentialChange
	groups   []*cliCredentialGroup
	problems []cliCredentialProblem
	ignored  []string
}

// cliCredentialChange is a changed token-refresh leaf of the host document.
type cliCredentialChange struct {
	obj      []string // path of the host object holding the leaf
	key      string
	kind     string // see cliCredentialKind; "minted" for a minted key
	old, cur any
	present  bool // the sandbox copy has the key
}

// cliCredentialGroup is a host object holding an access or refresh token.
type cliCredentialGroup struct {
	path                          []string
	hasRefresh, staticRefresh     bool
	accessChanged, refreshChanged string // path of the first changed token, "" if none
}

func (w *cliCredentialWalk) problem(path []string, code, detail string) {
	w.problems = append(w.problems, cliCredentialProblem{cliCredentialPath(path), code, detail})
}

func (w *cliCredentialWalk) group(path []string) *cliCredentialGroup {
	key := cliCredentialPathKey(path)
	for _, g := range w.groups {
		if cliCredentialPathKey(g.path) == key {
			return g
		}
	}
	g := &cliCredentialGroup{path: path}
	w.groups = append(w.groups, g)
	return g
}

// walk compares the host object old with the sandbox object cur at path.
func (w *cliCredentialWalk) walk(old, cur map[string]any, path []string) {
	for _, k := range cliSortedCredentialKeys(old) {
		ov := old[k]
		p := cliCredentialAppend(path, k)
		nv, present := cur[k]
		kind := cliCredentialKind(k, ov)
		if s, ok := ov.(string); kind == "" && ok && s != "" && w.minted[cliCredentialNormKey(k)] && cliCredentialMintShape(old) {
			kind = "minted"
		}
		if kind == "" {
			om, oldIsObj := ov.(map[string]any)
			nm, curIsObj := nv.(map[string]any)
			switch {
			case oldIsObj && curIsObj:
				w.walk(om, nm, p)
			case oldIsObj && cliCredentialHoldsTokens(om):
				w.problem(p, "remove", "")
			case !present:
				if cliCredentialProtected(k, ov) {
					w.ignored = append(w.ignored, cliCredentialPath(p))
				}
			case !cliCredentialSameValue(ov, nv) && cliCredentialProtected(k, ov, nv):
				w.problem(p, "protected", "")
			}
			continue
		}
		changed := !present || !cliCredentialSameValue(ov, nv)
		if kind == "access" || kind == "refresh" {
			g := w.group(path)
			g.hasRefresh = g.hasRefresh || kind == "refresh"
			switch {
			case !changed:
			case kind == "access":
				if g.accessChanged == "" {
					g.accessChanged = cliCredentialPath(p)
				}
			default:
				if g.refreshChanged == "" {
					g.refreshChanged = cliCredentialPath(p)
				}
				g.staticRefresh = g.staticRefresh || cliCredentialStaticRefresh(ov.(string))
			}
		}
		if changed {
			w.changes = append(w.changes, cliCredentialChange{obj: path, key: k, kind: kind, old: ov, cur: nv, present: present})
		}
	}
	for _, k := range cliSortedCredentialKeys(cur) {
		if _, ok := old[k]; !ok && cliCredentialProtected(k, cur[k]) {
			w.ignored = append(w.ignored, cliCredentialPath(cliCredentialAppend(path, k)))
		}
	}
}

// check records the problems of one changed token-refresh leaf.
func (w *cliCredentialWalk) check(c cliCredentialChange) {
	path := cliCredentialAppend(c.obj, c.key)
	if !c.present && c.kind != "access" && c.kind != "refresh" {
		return // dropped: the host value is kept
	}
	switch c.kind {
	case "id":
		if c.cur == nil || c.cur == "" {
			return // a refresh without an id token keeps the host's
		}
	case "time":
		_, oldNum := c.old.(json.Number)
		_, curNum := c.cur.(json.Number)
		_, curText := c.cur.(string)
		before, okBefore := cliCredentialInstantOf(c.old)
		after, okAfter := cliCredentialInstantOf(c.cur)
		switch {
		case oldNum != curNum || (!curNum && !curText) || c.cur == "":
			w.problem(path, "clear", "")
		case !okBefore || !okAfter:
			w.problem(path, "bad-expiry", "")
		case after.before(before):
			w.problem(path, "backwards", "")
		}
		return
	}
	cur, ok := c.cur.(string)
	if !c.present || !ok || cur == "" {
		w.problem(path, "clear", "")
		return
	}
	old := c.old.(string)
	if c.kind == "type" {
		if cliCredentialASCIILower(cur) != cliCredentialASCIILower(old) {
			w.problem(path, "token-type", "")
		}
		return
	}
	oldClaims, oldJWT := cliCredentialJWTClaimsOf(old)
	newClaims, newJWT := cliCredentialJWTClaimsOf(cur)
	switch {
	case oldJWT != newJWT:
		w.problem(path, "jwt-shape", "")
	case oldJWT:
		if claim := cliCredentialAccountMismatch(oldClaims, newClaims, ""); claim != "" {
			w.problem(path, "jwt-account", claim)
		}
	}
}

// applies reports whether a checked change is copied into the merged host
// document: tokens always, minted keys unless removed, other leaves only as
// part of a refresh of their object or of an object below it, and never a
// dropped id token.
func (c cliCredentialChange) applies(inRefresh map[string]bool) bool {
	switch {
	case c.kind == "access" || c.kind == "refresh":
		return true
	case !c.present:
		return false
	case c.kind == "minted":
		return true
	case !inRefresh[cliCredentialPathKey(c.obj)]:
		return false
	case c.kind == "id":
		s, ok := c.cur.(string)
		return ok && s != ""
	}
	return true
}

// cliCredentialKind returns the token-refresh role of a host leaf: "access",
// "refresh", "id", "type" (token type) or "time" (expiry or last refresh),
// or "" for every other field.
func cliCredentialKind(key string, v any) string {
	nk := cliCredentialNormKey(key)
	s, isText := v.(string)
	isText = isText && s != ""
	if nk == "expiry" || nk == "expirydate" || nk == "lastrefresh" || strings.HasSuffix(nk, "expiresat") {
		if _, isNum := v.(json.Number); isNum || isText {
			return "time"
		}
		return ""
	}
	if !isText {
		return ""
	}
	switch nk {
	case "accesstoken":
		return "access"
	case "key":
		if _, ok := cliCredentialJWTClaimsOf(s); ok {
			return "access"
		}
	case "refreshtoken":
		return "refresh"
	case "idtoken":
		return "id"
	case "tokentype":
		return "type"
	}
	return ""
}

// cliCredentialNormKey returns key in ASCII lower case without "_" and "-",
// so expires_at and expiresAt compare equal.
func cliCredentialNormKey(key string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '_' || r == '-':
			return -1
		case 'A' <= r && r <= 'Z':
			return r + 'a' - 'A'
		}
		return r
	}, key)
}

func cliCredentialASCIILower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

func cliCredentialStaticRefresh(token string) bool {
	for _, prefix := range cliCredentialStaticRefreshPrefixes {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	return false
}

// cliCredentialProtected reports whether a field holds something a token
// refresh never changes (cliCredentialProtectedKeyRe); e-mail and URL values
// and nested protected fields count as well.
func cliCredentialProtected(key string, values ...any) bool {
	nk := cliCredentialNormKey(key)
	if cliCredentialBenignKeyRe.MatchString(nk) {
		return false
	}
	if cliCredentialProtectedKeyRe.MatchString(nk) {
		return true
	}
	for _, v := range values {
		if cliCredentialProtectedValue(v) {
			return true
		}
	}
	return false
}

func cliCredentialProtectedValue(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if cliCredentialProtected(k, x) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if cliCredentialProtectedValue(x) {
				return true
			}
		}
	case string:
		return cliCredentialEmailRe.MatchString(v) || cliCredentialURLRe.MatchString(v)
	}
	return false
}

// cliCredentialHoldsTokens reports whether a host object holds an access or
// refresh token at any depth.
func cliCredentialHoldsTokens(obj map[string]any) bool {
	for k, v := range obj {
		if kind := cliCredentialKind(k, v); kind == "access" || kind == "refresh" {
			return true
		}
		if m, ok := v.(map[string]any); ok && cliCredentialHoldsTokens(m) {
			return true
		}
	}
	return false
}

// cliCredentialMintShape reports whether the host object obj is in the login
// shape in which the provider CLI mints its keys itself (muse's OAuth
// record): it holds an OAuth access token, and neither its mechanism nor
// obtained_via names a key. A key saved with `muse auth set --api-key-stdin`
// or pasted in the pasted_key lane (mechanism api_key, obtained_via
// pasted_key) is the user's own and stays protected.
func cliCredentialMintShape(obj map[string]any) bool {
	hasAccess := false
	for k, v := range obj {
		s, isText := v.(string)
		isText = isText && s != ""
		switch cliCredentialNormKey(k) {
		case "accesstoken":
			hasAccess = hasAccess || isText
		case "mechanism", "obtainedvia":
			if !isText || strings.Contains(cliCredentialNormKey(s), "key") {
				return false
			}
		}
	}
	return hasAccess
}

// cliCredentialAccountMismatch returns the first account claim of old (at
// any depth) that cur lacks or changes, or "" for the same account.
func cliCredentialAccountMismatch(old, cur map[string]any, prefix string) string {
	for _, k := range cliSortedCredentialKeys(old) {
		v := old[k]
		if cliCredentialAccountClaims[k] {
			if cv, ok := cur[k]; !ok || !cliCredentialSameValue(v, cv) {
				return prefix + k
			}
		} else if om, ok := v.(map[string]any); ok {
			cm, _ := cur[k].(map[string]any)
			if claim := cliCredentialAccountMismatch(om, cm, prefix+k+"."); claim != "" {
				return claim
			}
		}
	}
	return ""
}

// cliCredentialInstant is a comparable expiry or last-refresh time: a JSON
// number (num), or an RFC 3339 string (sec, nsec since the Unix epoch).
type cliCredentialInstant struct {
	num       float64
	sec, nsec int64
}

func (a cliCredentialInstant) before(b cliCredentialInstant) bool {
	if a.num != b.num {
		return a.num < b.num
	}
	if a.sec != b.sec {
		return a.sec < b.sec
	}
	return a.nsec < b.nsec
}

func cliCredentialInstantOf(v any) (cliCredentialInstant, bool) {
	switch v := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsInf(f, 0) {
			return cliCredentialInstant{}, false
		}
		return cliCredentialInstant{num: f}, true
	case string:
		return cliCredentialRFC3339(v)
	}
	return cliCredentialInstant{}, false
}

// cliCredentialRFC3339 parses an RFC 3339 date-time (with "T", at most
// nanosecond precision and a "Z" or numeric offset).
func cliCredentialRFC3339(s string) (cliCredentialInstant, bool) {
	m := cliCredentialRFC3339Re.FindStringSubmatch(s)
	if m == nil {
		return cliCredentialInstant{}, false
	}
	n := func(i int) int64 {
		v, _ := strconv.ParseInt(m[i], 10, 64)
		return v
	}
	year, month, day, hour, minute, second := n(1), n(2), n(3), n(4), n(5), n(6)
	monthDays := [...]int64{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
		monthDays[1] = 29
	}
	if year < 1 || month < 1 || month > 12 || day < 1 || day > monthDays[month-1] || hour > 23 || minute > 59 || second > 59 {
		return cliCredentialInstant{}, false
	}
	var offset int64
	if m[8] != "" {
		if n(9) > 23 || n(10) > 59 {
			return cliCredentialInstant{}, false
		}
		offset = (n(9)*60 + n(10)) * 60
		if m[8] == "-" {
			offset = -offset
		}
	}
	// Days since 1970-01-01 (H. Hinnant's days_from_civil).
	y, mp := year, month+9
	if month <= 2 {
		y--
	} else {
		mp = month - 3
	}
	era := y / 400
	yoe := y - era*400
	doe := yoe*365 + yoe/4 - yoe/100 + (153*mp+2)/5 + day - 1
	days := era*146097 + doe - 719468
	var nsec int64
	if m[7] != "" {
		nsec, _ = strconv.ParseInt(m[7]+strings.Repeat("0", 9-len(m[7])), 10, 64)
	}
	return cliCredentialInstant{sec: days*86400 + hour*3600 + minute*60 + second - offset, nsec: nsec}, true
}

// cliCredentialJWTClaimsOf returns the claims of a JWT-shaped string. The
// signature is not verified: the claims are only compared with the host copy.
func cliCredentialJWTClaimsOf(s string) (map[string]any, bool) {
	if !cliCredentialJWTRe.MatchString(s) {
		return nil, false
	}
	parts := strings.Split(s, ".")
	header, ok := cliDecodeCredentialJWTPart(parts[0])
	if !ok {
		return nil, false
	}
	if _, ok := header["alg"]; !ok {
		return nil, false
	}
	return cliDecodeCredentialJWTPart(parts[1])
}

func cliDecodeCredentialJWTPart(seg string) (map[string]any, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return nil, false
	}
	v, err := cliDecodeCredentialJSON(raw)
	if err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// cliDecodeCredentialJSON decodes one strict JSON value: valid UTF-8, no
// duplicate object keys, no lone surrogate escapes, bounded nesting. Numbers
// stay json.Number, so identifiers compare exactly.
func cliDecodeCredentialJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("invalid UTF-8")
	}
	if cliCredentialLoneSurrogate(data) {
		return nil, errors.New("lone surrogate escape")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := cliDecodeCredentialJSONValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// cliCredentialLoneSurrogate reports whether the JSON text data escapes a
// UTF-16 surrogate outside a high-low pair, such as "\ud800" alone.
// encoding/json reads it as U+FFFD while Python's json keeps a lone surrogate
// that no UTF-8 encoder accepts, so both engines refuse such a document.
// Outside strings valid JSON has no backslash, so string state is not tracked.
func cliCredentialLoneSurrogate(data []byte) bool {
	unit := func(i int) (rune, bool) { // the \uXXXX escape at data[i:]
		if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
		return rune(v), err == nil
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		r, ok := unit(i)
		switch {
		case !ok:
			i++ // another escape such as \\ or \": skip the escaped byte
		case r >= 0xd800 && r < 0xdc00: // high surrogate: a low one must follow
			if low, ok := unit(i + 6); !ok || low < 0xdc00 || low > 0xdfff {
				return true
			}
			i += 11
		case r >= 0xdc00 && r <= 0xdfff:
			return true
		default:
			i += 5
		}
	}
	return false
}

func cliDecodeCredentialJSONValue(dec *json.Decoder, depth int) (any, error) {
	if depth > cliCredentialMaxJSONDepth {
		return nil, errors.New("JSON nesting too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string, json.Number, bool or nil
	}
	switch delim {
	case '{':
		obj := map[string]any{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, dup := obj[key]; dup {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			if obj[key], err = cliDecodeCredentialJSONValue(dec, depth+1); err != nil {
				return nil, err
			}
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := cliDecodeCredentialJSONValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected %q", delim)
}

// cliEncodeCredential serializes a merged credential document like the host
// file orig: indented when orig spans several lines, with orig's trailing
// newline. Keys come out sorted.
func cliEncodeCredential(doc map[string]any, orig []byte) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if bytes.Contains(bytes.TrimRight(orig, " \t\r\n"), []byte("\n")) {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if bytes.HasSuffix(orig, []byte("\n")) {
		out = append(out, '\n')
	}
	return out, nil
}

func cliCredentialClone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = cliCredentialClone(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = cliCredentialClone(x)
		}
		return out
	}
	return v
}

// cliCredentialObjectAt returns the object at path, which must exist.
func cliCredentialObjectAt(doc map[string]any, path []string) map[string]any {
	for _, k := range path {
		doc = doc[k].(map[string]any)
	}
	return doc
}

// cliCredentialSameValue compares decoded JSON values by type and value.
func cliCredentialSameValue(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !cliCredentialSameValue(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			if y, ok := bv[k]; !ok || !cliCredentialSameValue(x, y) {
				return false
			}
		}
		return true
	}
	return false
}

func cliSortedCredentialKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func cliSortedUnique(items []string) []string {
	sort.Strings(items)
	return slices.Compact(items)
}

func cliCredentialAppend(path []string, key string) []string {
	return append(path[:len(path):len(path)], key)
}

// cliCredentialPath renders a field path such as tokens.id_token.
func cliCredentialPath(path []string) string {
	return strings.Join(path, ".")
}

// cliCredentialPathKey identifies an object path unambiguously.
func cliCredentialPathKey(path []string) string {
	var b strings.Builder
	for _, k := range path {
		fmt.Fprintf(&b, "%d:%s", len(k), k)
	}
	return b.String()
}

// cliCredentialFieldList names at most five fields for a warning.
func cliCredentialFieldList(fields []string) string {
	const maxListed = 5
	if len(fields) > maxListed {
		return strings.Join(fields[:maxListed], ", ") + ", ..."
	}
	return strings.Join(fields, ", ")
}

// cliPrivateCopyProblemText maps why a private credential copy cannot be
// read at write-back (cliReadPrivateCredential) to its warning text.
var cliPrivateCopyProblemText = map[string]string{
	"missing":     "was deleted",
	"not-regular": "is a symlink, FIFO, directory or other non-regular file",
	"oversized":   "exceeds the " + strconv.Itoa(cliPrivateCredMaxBytes>>20) + " MiB credential size limit",
	"unreadable":  "cannot be read",
}

// cliReadPrivateCredential reads path, which must lie under root, refusing
// symlinks in any component below root and non-regular files. It returns the
// content, or why it cannot: "missing", "not-regular" (at the file or on its
// path), "oversized" or "unreadable" (cliPrivateCopyProblemText).
func cliReadPrivateCredential(root, path string) ([]byte, string) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, "unreadable"
	}
	cur := root
	parts := strings.Split(rel, string(os.PathSeparator))
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		last := i == len(parts)-1
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, "missing"
		case err != nil:
			return nil, "unreadable"
		case fi.Mode()&os.ModeSymlink != 0, !last && !fi.IsDir(), last && !fi.Mode().IsRegular():
			return nil, "not-regular"
		case last && fi.Size() > cliPrivateCredMaxBytes:
			return nil, "oversized"
		}
	}
	data, err := cliReadRegularFile(cur)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, "missing"
	case err != nil:
		return nil, "unreadable"
	}
	return data, ""
}

// cliReadRegularFile reads a bounded regular file.
func cliReadRegularFile(path string) ([]byte, error) {
	data, _, err := cliReadRegularFileInfo(path)
	return data, err
}

// cliReadRegularFileInfo reads a bounded regular file and returns the
// FileInfo of the file it opened.
func cliReadRegularFileInfo(path string) ([]byte, os.FileInfo, error) {
	if !filepath.IsAbs(path) || strings.Contains(path, "..") {
		return nil, nil, fmt.Errorf("invalid credential path %q", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: provider credential path chosen by makewand, not request input
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, cliPrivateCredMaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > cliPrivateCredMaxBytes {
		return nil, nil, fmt.Errorf("%s exceeds credential size limit", path)
	}
	return data, fi, nil
}

// cliHostCredential is a host credential file as read for a write-back.
type cliHostCredential struct {
	info os.FileInfo
	data []byte
}

// cliReadHostCredential reads the regular host credential file at path; a
// symlink there is refused, not followed.
func cliReadHostCredential(path string) (cliHostCredential, error) {
	lfi, err := os.Lstat(path)
	if err != nil {
		return cliHostCredential{}, err
	}
	if !lfi.Mode().IsRegular() {
		return cliHostCredential{}, fmt.Errorf("%s is not a regular file", path)
	}
	data, fi, err := cliReadRegularFileInfo(path)
	if err != nil {
		return cliHostCredential{}, err
	}
	if !os.SameFile(lfi, fi) {
		return cliHostCredential{}, fmt.Errorf("%s was replaced while it was read", path)
	}
	return cliHostCredential{info: fi, data: data}, nil
}

// unchangedSince reports whether c is still the file read as before: the same
// device and inode, size, modification time and content.
func (c cliHostCredential) unchangedSince(before cliHostCredential) bool {
	return c.info != nil && before.info != nil && os.SameFile(c.info, before.info) &&
		c.info.Size() == before.info.Size() && c.info.ModTime().Equal(before.info.ModTime()) &&
		bytes.Equal(c.data, before.data)
}

var (
	errCLICredentialChanged = errors.New("host credential changed since it was read")
	errCLICredentialLocked  = errors.New("host credential locked by another makewand write-back")
)

// cliCredentialLockWait bounds the wait for another makewand write-back of
// the same host credential file (cliLockCredential). Tests shorten it.
var cliCredentialLockWait = 10 * time.Second

// cliCredentialLockPath returns the sidecar lock file of the host credential
// file path, which is never renamed or removed; the Python engine uses the
// same one (makewand/sandbox.py _credential_lock_path).
func cliCredentialLockPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".makewand-lock")
}

// cliLockCredential serializes makewand's own write-backs of the host
// credential file path across processes and both engines: an exclusive
// flock(2) (LockFileEx on Windows) on its sidecar lock file, which the Python
// engine takes as well (_credential_lock). ok is false when another
// write-back held it for cliCredentialLockWait. Where no lock can be taken
// (the lock file cannot be created or is a symlink, which is never followed,
// see openCredentialLock; a filesystem without flock) it returns ok unlocked,
// as the Python engine does: the identity re-check before the rename still
// applies.
func cliLockCredential(path string) (unlock func(), ok bool) {
	f, err := openCredentialLock(cliCredentialLockPath(path))
	if err != nil {
		return func() {}, true
	}
	locked := make(chan error, 1)
	go func() { locked <- lockUserHandle(f, true) }()
	select {
	case err := <-locked:
		if err != nil {
			_ = f.Close()
			return func() {}, true
		}
		return func() { _ = unlockUserHandle(f); _ = f.Close() }, true
	case <-time.After(cliCredentialLockWait):
		// The blocked lock call returns once the holder unlocks: release
		// the lock and the file then.
		go func() {
			if <-locked == nil {
				_ = unlockUserHandle(f)
			}
			_ = f.Close()
		}()
		return nil, false
	}
}

// cliReplaceHostCredential atomically replaces the host credential file at
// path with data unless it is no longer read, the file the merge read. The
// data is written and synced to a temporary file first; then, holding the
// credential lock (cliLockCredential), the host file's device, inode, size,
// modification time and content are compared with read immediately before
// the rename.
func cliReplaceHostCredential(path string, data []byte, mode os.FileMode, read cliHostCredential) error {
	if !filepath.IsAbs(path) || strings.Contains(path, "..") {
		return fmt.Errorf("invalid credential path %q", path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".makewand-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
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
	unlock, ok := cliLockCredential(path)
	if !ok {
		return errCLICredentialLocked
	}
	defer unlock()
	if now, err := cliReadHostCredential(path); err != nil || !now.unchangedSince(read) {
		return errCLICredentialChanged
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}
