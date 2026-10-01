package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxApplyJournalBytes = 16 << 20
const maxApplyBackupBytes = 256 << 20

// ErrStaleApply means recovery found a change made outside the interrupted
// transaction. The journal and every workspace file are preserved for review.
var ErrStaleApply = errors.New("interrupted apply conflicts with workspace changes")

type applyJournalEntry struct {
	Path           string      `json:"path"`
	Existed        bool        `json:"existed"`
	BeforeHash     string      `json:"before_hash,omitempty"`
	BeforeSize     int64       `json:"before_size"`
	BeforeMode     os.FileMode `json:"before_mode"`
	AfterHash      string      `json:"after_hash"`
	AfterSize      int64       `json:"after_size"`
	AfterBlob      string      `json:"after_blob"`
	AfterMode      os.FileMode `json:"after_mode"`
	Blob           string      `json:"blob,omitempty"`
	BeforeSecurity string      `json:"security,omitempty"`
	AfterSecurity  string      `json:"after_security,omitempty"`
	Temp           string      `json:"temp"`
	CreatedDirs    []string    `json:"created_dirs,omitempty"`
}

type applyJournal struct {
	Schema    int                 `json:"schema"`
	Workspace string              `json:"workspace"`
	RootID    string              `json:"root_id"`
	State     string              `json:"state"`
	Entries   []applyJournalEntry `json:"entries"`
}

type applyJournalState struct {
	path          string
	root          *os.Root
	lock          *os.File
	syncDirectory func(string) error
	afterRestore  func(applyJournalEntry) error
	published     bool
}

var openApplyWorkspaceRoot = os.OpenRoot

// Bind the newly opened handle, not just its pathname. A rename ABA can put a
// different directory at path while OpenRoot runs, then restore the original
// name; pathname identity checks alone would still write through the wrong root.
func openBoundApplyRoot(path, identity string) (*os.Root, error) {
	root, err := openApplyWorkspaceRoot(path)
	if err != nil {
		return nil, err
	}
	if err := checkApplyRoot(root, identity); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func checkApplyRoot(root *os.Root, identity string) error {
	current, err := applyOpenRootIdentity(root)
	if err != nil || identity == "" || current != identity {
		return fmt.Errorf("%w: held workspace directory identity changed: %v", ErrStaleApply, err)
	}
	return nil
}

func writeApplyEntry(root *os.Root, state *applyJournalState, journal *applyJournal, index int, content string) error {
	entry := &journal.Entries[index]
	var beforeRename func(*os.File) error
	if runtime.GOOS == "windows" {
		beforeRename = func(file *os.File) error {
			security, err := applyOpenFileSecurity(file)
			if err != nil {
				return err
			}
			if security == "" {
				return fmt.Errorf("missing applied file security")
			}
			if entry.Existed && (entry.BeforeSecurity == "" || security != entry.BeforeSecurity) {
				return fmt.Errorf("%w: applied file security differs from preimage: %s", ErrStaleApply, entry.Path)
			}
			entry.AfterSecurity = security
			return state.write(journal)
		}
	}
	return writeFileModeAt(root, entry.Path, content, &entry.AfterMode, entry.Temp, entry.BeforeSecurity, beforeRename)
}

// ApplyFilesTransactional seals preimages before any workspace writes, serializes
// writers, and records a durable commit only after every replacement is synced.
// validate runs under the same lock before sealing (for trusted acceptance and
// baseline checks). A crash before commit is rolled back on project reopening.
// Ordinary external editors do not join this advisory lock; conflicting edits
// are preserved and stop recovery rather than being silently overwritten.
func (p *Project) ApplyFilesTransactional(ctx context.Context, files []ExtractedFile, validate func() error) error {
	if len(files) == 0 {
		return nil
	}
	state, err := p.openApplyState(ctx, true)
	if err != nil {
		return err
	}
	defer state.close()
	if err := p.recoverApplyLocked(state); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	journal, err := p.prepareApplyJournal(ctx, state, files)
	if err != nil {
		return err
	}
	root, err := openBoundApplyRoot(p.Path, journal.RootID)
	if err != nil {
		return errors.Join(err, state.removePending())
	}
	defer root.Close()
	err = p.validateApplyRecovery(root, state, journal, false)
	if validate != nil {
		if err := validate(); err != nil {
			return errors.Join(err, state.removePending())
		}
	}
	if err != nil {
		// No target has been written yet. Keep the external edit, discard our
		// preimages, and never turn a stale preparation into a rollback.
		return errors.Join(err, state.removePending())
	}
	for i, file := range files {
		if err = ctx.Err(); err == nil {
			err = checkApplyWorkspace(p.Path, journal.RootID)
		}
		if err == nil {
			err = p.validateApplyEntryImage(root, journal.Entries[i], false)
		}
		if err == nil {
			err = writeApplyEntry(root, state, journal, i, file.Content)
		}
		if err == nil {
			err = syncApplyDirectory(filepath.Dir(filepath.Join(p.Path, file.Path)))
		}
		if err != nil {
			rollbackErr := p.recoverApplyLocked(state)
			return errors.Join(fmt.Errorf("apply %s: %w", file.Path, err), rollbackErr)
		}
	}
	if err := checkApplyWorkspace(p.Path, journal.RootID); err != nil {
		return errors.Join(err, p.recoverApplyLocked(state))
	}
	if err := p.validateApplyPostimages(root, journal); err != nil {
		return errors.Join(err, p.recoverApplyLocked(state))
	}
	journal.State = "committed"
	if err := state.write(journal); err != nil {
		return errors.Join(fmt.Errorf("commit apply journal: %w", err), p.recoverApplyLocked(state))
	}
	return state.removePending()
}

// RecoverInterruptedApply returns without creating state when no transaction
// exists. Pending preimages are restored; a committed journal is only removed.
func (p *Project) RecoverInterruptedApply(ctx context.Context) error {
	state, err := p.openApplyState(ctx, false)
	if err != nil || state == nil {
		return err
	}
	defer state.close()
	return p.recoverApplyLocked(state)
}

func canonicalApplyPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor, suffix := abs, ""
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !os.IsNotExist(err) || filepath.Dir(ancestor) == ancestor {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(ancestor), suffix)
		ancestor = filepath.Dir(ancestor)
	}
}

func (p *Project) applyStatePath() (string, string, error) {
	workspace, err := canonicalApplyPath(p.Path)
	if err != nil {
		return "", "", err
	}
	base := strings.TrimSpace(os.Getenv("MAKEWAND_APPLY_STATE_DIR"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("MAKEWAND_CONFIG_DIR"))
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", "", err
			}
			base = filepath.Join(home, ".config", "makewand")
		}
		base = filepath.Join(base, "apply-journals")
	}
	base, err = canonicalApplyPath(base)
	if err != nil {
		return "", "", err
	}
	if isWithinDir(workspace, base) {
		return "", "", fmt.Errorf("apply state must be outside the candidate workspace")
	}
	key := workspace
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(base, hex.EncodeToString(hash[:])), workspace, nil
}

func (p *Project) openApplyState(ctx context.Context, create bool) (*applyJournalState, error) {
	path, _, err := p.applyStatePath()
	if err != nil {
		return nil, err
	}
	if !create {
		// The canonical state path uses trusted config and a hashed workspace
		// key, and applyStatePath rejects state inside the candidate workspace.
		//nolint:gosec // G703: validated external state path, not a candidate-relative filename.
		if _, err := os.Lstat(filepath.Join(path, "pending", "journal.json")); os.IsNotExist(err) {
			// A persisted lost-header diagnostic also needs the lock and fixed
			// state root, even though no journal is available to recover.
			//nolint:gosec // G703: same validated external state path as the journal.
			if _, markerErr := os.Lstat(filepath.Join(path, "pending", "lost-header")); os.IsNotExist(markerErr) {
				return nil, nil
			} else if markerErr != nil {
				return nil, markerErr
			}
		} else if err != nil {
			return nil, err
		}
	}
	//nolint:gosec // G703: canonical external state path validated by applyStatePath above.
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	if err := privateApplyDirectory(path); err != nil {
		return nil, err
	}
	if err := privateApplyDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	lock, err := openApplyLock(filepath.Join(path, "workspace.lock"))
	if err != nil {
		root.Close()
		return nil, err
	}
	state := &applyJournalState{path: path, root: root, lock: lock}
	for {
		acquired, err := tryApplyLock(lock)
		if err != nil {
			state.close()
			return nil, err
		}
		if acquired {
			return state, nil
		}
		select {
		case <-ctx.Done():
			state.close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *applyJournalState) close() {
	if s == nil {
		return
	}
	_ = unlockApply(s.lock)
	_ = s.lock.Close()
	_ = s.root.Close()
}

func applyHash(reader io.Reader) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	return hex.EncodeToString(hash.Sum(nil)), size, err
}

func (p *Project) prepareApplyJournal(ctx context.Context, s *applyJournalState, files []ExtractedFile) (_ *applyJournal, resultErr error) {
	if len(files) > 10000 {
		return nil, fmt.Errorf("apply exceeds 10000-file limit")
	}
	_, workspace, err := p.applyStatePath()
	if err != nil {
		return nil, err
	}
	journal := &applyJournal{Schema: 1, Workspace: workspace, State: "applying"}
	journal.RootID, err = applyWorkspaceIdentity(workspace)
	if err != nil {
		return nil, err
	}
	if err := s.root.Mkdir("pending", 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			// A published header owns its preimages even when the following
			// directory sync failed. Preserve them for the next recovery.
			if _, err := s.root.Lstat(filepath.Join("pending", "journal.json")); os.IsNotExist(err) && !s.published {
				_ = s.removePending()
			}
		}
	}()
	seen := map[string]bool{}
	var backupBytes, afterBytes int64
	root, err := openBoundApplyRoot(workspace, journal.RootID)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Clean(file.Path)
		key := path
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if !filepath.IsLocal(path) || path == "." || seen[key] {
			return nil, fmt.Errorf("invalid or duplicate apply path: %s", file.Path)
		}
		seen[key] = true
		afterBytes += int64(len(file.Content))
		if afterBytes > maxApplyBackupBytes {
			return nil, fmt.Errorf("apply payload exceeds size limit")
		}
		if _, err := p.validatePath(path, true); err != nil {
			return nil, err
		}
		if err := rejectApplyAliases(root, path); err != nil {
			return nil, err
		}
		after, _, err := applyHash(strings.NewReader(file.Content))
		if err != nil {
			return nil, err
		}
		entry := applyJournalEntry{Path: path, AfterHash: after, AfterMode: 0o600,
			Temp: filepath.Join(filepath.Dir(path), ".makewand-apply-"+rand.Text()), CreatedDirs: p.missingParentDirs(path)}
		entry.AfterBlob, entry.AfterSize = fmt.Sprintf("%06d.after", i), int64(len(file.Content))
		if err := writeApplyPostimage(s.root, entry.AfterBlob, file.Content); err != nil {
			return nil, err
		}
		info, err := root.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() > maxApplyBackupBytes-backupBytes {
				return nil, fmt.Errorf("unsupported apply preimage or backup size limit: %s", path)
			}
			entry.Existed, entry.BeforeMode, entry.AfterMode = true, info.Mode().Perm(), info.Mode().Perm()
			if runtime.GOOS == "windows" {
				preimage, err := root.Open(path)
				if err != nil {
					return nil, err
				}
				entry.BeforeSecurity, err = applyOpenFileSecurity(preimage)
				preimage.Close()
				if err != nil {
					return nil, err
				}
			}
			entry.Blob = fmt.Sprintf("%06d.before", i)
			entry.BeforeHash, entry.BeforeSize, err = copyApplyPreimage(root, s.root, path, entry.Blob)
			if err != nil {
				return nil, err
			}
			backupBytes += entry.BeforeSize
			if backupBytes > maxApplyBackupBytes {
				return nil, fmt.Errorf("apply backup exceeds size limit")
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if file.ModeKnown {
			entry.AfterMode = file.Mode.Perm()
		}
		entry.AfterMode = supportedApplyMode(entry.AfterMode)
		journal.Entries = append(journal.Entries, entry)
	}
	if err := s.write(journal); err != nil {
		return nil, err
	}
	return journal, nil
}

func writeApplyPostimage(root *os.Root, name, content string) error {
	file, err := root.OpenFile(filepath.Join("pending", name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.WriteString(content); err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func copyApplyPreimage(source, state *os.Root, path, blob string) (string, int64, error) {
	input, err := source.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return "", 0, fmt.Errorf("apply preimage is not a regular file")
	}
	pathInfo, err := source.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(before, pathInfo) {
		return "", 0, ErrStaleApply
	}
	output, err := state.OpenFile(filepath.Join("pending", blob), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer output.Close()
	hash, size, err := applyHash(io.TeeReader(io.LimitReader(input, maxApplyBackupBytes+1), output))
	if err != nil {
		return "", 0, err
	}
	if size > maxApplyBackupBytes {
		return "", 0, fmt.Errorf("copy apply preimage exceeds size limit")
	}
	after, err := input.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, ErrStaleApply
	}
	return hash, size, output.Sync()
}

func (s *applyJournalState) write(journal *applyJournal) error {
	content, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(content) > maxApplyJournalBytes {
		return fmt.Errorf("apply journal exceeds size limit")
	}
	name := filepath.Join("pending", ".journal-"+rand.Text())
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = s.root.Remove(name) }()
	if _, err = file.Write(content); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := s.root.Rename(name, filepath.Join("pending", "journal.json")); err != nil {
		return err
	}
	s.published = true
	syncDir := s.syncDirectory
	if syncDir == nil {
		syncDir = syncApplyDirectory
	}
	return errors.Join(syncDir(filepath.Join(s.path, "pending")), syncDir(s.path))
}

func (s *applyJournalState) read() (*applyJournal, error) {
	info, err := s.root.Lstat(filepath.Join("pending", "journal.json"))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("apply journal must be a regular file")
	}
	file, err := s.root.Open(filepath.Join("pending", "journal.json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxApplyJournalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxApplyJournalBytes {
		return nil, fmt.Errorf("invalid apply journal size")
	}
	var journal applyJournal
	if err := json.Unmarshal(content, &journal); err != nil {
		return nil, err
	}
	s.published = true
	return &journal, nil
}

func (p *Project) recoverApplyLocked(s *applyJournalState) error {
	journal, err := s.read()
	if os.IsNotExist(err) {
		if _, markerErr := s.root.Lstat(filepath.Join("pending", "lost-header")); !os.IsNotExist(markerErr) {
			return errors.Join(fmt.Errorf("%w: published apply header was lost; evidence preserved at %s", ErrStaleApply, s.path), markerErr)
		}
		if s.published {
			return errors.Join(fmt.Errorf("%w: published apply journal is missing; evidence preserved at %s", ErrStaleApply, s.path), s.markLostHeader())
		}
		// A crash before publishing the header cannot have written the workspace.
		return s.removePending()
	}
	if err != nil {
		return fmt.Errorf("read interrupted apply: %w", err)
	}
	_, workspace, err := p.applyStatePath()
	if err != nil {
		return err
	}
	if journal.Schema != 1 || journal.Workspace != workspace || len(journal.Entries) > 10000 || (journal.State != "applying" && journal.State != "committed" && journal.State != "rolled_back") {
		return fmt.Errorf("invalid interrupted apply journal; preserved at %s", s.path)
	}
	if journal.State != "applying" {
		return s.removePending()
	}
	if err := checkApplyWorkspace(workspace, journal.RootID); err != nil {
		return fmt.Errorf("%w; journal preserved at %s", err, s.path)
	}
	root, err := openBoundApplyRoot(workspace, journal.RootID)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := p.validateApplyRecovery(root, s, journal, true); err != nil {
		return fmt.Errorf("%w; journal preserved at %s", err, s.path)
	}
	for _, entry := range journal.Entries {
		if err := checkApplyWorkspace(workspace, journal.RootID); err != nil {
			return err
		}
		if err := p.validateApplyEntryImage(root, entry, true); err != nil {
			return err
		}
		if err := validateApplyTemp(root, s.root, entry); err != nil {
			return err
		}
		if err := removeApplyTemp(root, entry); err != nil {
			return err
		}
		if err := p.restoreApplyEntry(root, s, entry); err != nil {
			return fmt.Errorf("recover %s: %w; journal preserved at %s", entry.Path, err, s.path)
		}
		if s.afterRestore != nil {
			if err := s.afterRestore(entry); err != nil {
				return err
			}
		}
	}
	// A later restoration or an external writer may have changed an earlier
	// target. Publish success only after checking every actual preimage again.
	if err := checkApplyWorkspace(workspace, journal.RootID); err != nil {
		return fmt.Errorf("%w; journal preserved at %s", err, s.path)
	}
	if err := checkApplyRoot(root, journal.RootID); err != nil {
		return fmt.Errorf("%w; journal preserved at %s", err, s.path)
	}
	if err := p.validateApplyRecovery(root, s, journal, false); err != nil {
		return fmt.Errorf("%w; journal preserved at %s", err, s.path)
	}
	journal.State = "rolled_back"
	if err := s.write(journal); err != nil {
		return err
	}
	return s.removePending()
}

// This diagnostic is written only after observing a previously published
// header disappear. It carries no replacement journal or guessed preimages;
// a fresh process must preserve the remaining evidence rather than treating
// this known fault as an ordinary interruption before initial publication.
func (s *applyJournalState) markLostHeader() error {
	file, err := s.root.OpenFile(filepath.Join("pending", "lost-header"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("persist lost apply header diagnostic: %w", err)
	}
	if _, err = file.WriteString("apply header disappeared after publication; recovery is uncertain\n"); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return fmt.Errorf("flush lost apply header diagnostic: %w", err)
	}
	syncDir := s.syncDirectory
	if syncDir == nil {
		syncDir = syncApplyDirectory
	}
	return errors.Join(syncDir(filepath.Join(s.path, "pending")), syncDir(s.path))
}

func (p *Project) validateApplyRecovery(root *os.Root, s *applyJournalState, journal *applyJournal, allowAfter bool) error {
	seen := map[string]bool{}
	var backupBytes, afterBytes int64
	for i, entry := range journal.Entries {
		if runtime.GOOS == "windows" {
			if entry.Existed {
				if err := validateApplySecurity(entry.BeforeSecurity); err != nil {
					return fmt.Errorf("invalid recovery preimage security: %w", err)
				}
			}
			if entry.AfterSecurity != "" {
				if err := validateApplySecurity(entry.AfterSecurity); err != nil {
					return fmt.Errorf("invalid recovery postimage security: %w", err)
				}
			}
		}
		key := entry.Path
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if !filepath.IsLocal(entry.Path) || filepath.Clean(entry.Path) != entry.Path || entry.Path == "." || seen[key] {
			return fmt.Errorf("invalid recovery path")
		}
		seen[key] = true
		if filepath.Dir(entry.Temp) != filepath.Dir(entry.Path) || !strings.HasPrefix(filepath.Base(entry.Temp), ".makewand-apply-") || len(filepath.Base(entry.Temp)) != len(".makewand-apply-")+26 {
			return fmt.Errorf("invalid recovery temporary path")
		}
		afterBytes += entry.AfterSize
		if entry.AfterBlob != fmt.Sprintf("%06d.after", i) || entry.AfterSize < 0 || afterBytes > maxApplyBackupBytes {
			return fmt.Errorf("invalid recovery postimage")
		}
		if err := validateApplyBlob(s.root, entry.AfterBlob, entry.AfterHash, entry.AfterSize); err != nil {
			return err
		}
		if err := validateApplyTemp(root, s.root, entry); err != nil {
			return err
		}
		if _, err := p.validatePath(entry.Path, true); err != nil {
			return err
		}
		if err := rejectApplyAliases(root, entry.Path); err != nil {
			return err
		}
		for _, dir := range entry.CreatedDirs {
			if !filepath.IsLocal(dir) || dir == "." || !strings.HasPrefix(entry.Path, dir+string(filepath.Separator)) {
				return fmt.Errorf("invalid recovery directory")
			}
		}
		if entry.Existed {
			backupBytes += entry.BeforeSize
			if entry.Blob != fmt.Sprintf("%06d.before", i) || entry.BeforeSize < 0 || entry.BeforeSize > maxApplyBackupBytes {
				return fmt.Errorf("invalid recovery preimage")
			}
			if backupBytes > maxApplyBackupBytes {
				return fmt.Errorf("recovery backup exceeds size limit")
			}
			info, err := s.root.Lstat(filepath.Join("pending", entry.Blob))
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("invalid recovery preimage file")
			}
			blob, err := s.root.Open(filepath.Join("pending", entry.Blob))
			if err != nil {
				return err
			}
			hash, size, hashErr := applyHash(io.LimitReader(blob, maxApplyBackupBytes+1))
			blob.Close()
			if hashErr != nil || size != entry.BeforeSize || hash != entry.BeforeHash {
				return fmt.Errorf("corrupt recovery preimage: %s", entry.Path)
			}
		}
		if err := p.validateApplyEntryImage(root, entry, allowAfter); err != nil {
			return err
		}
	}
	return nil
}

func (p *Project) validateApplyEntryImage(root *os.Root, entry applyJournalEntry, allowAfter bool) error {
	if err := rejectApplyAliases(root, entry.Path); err != nil {
		return err
	}
	info, err := root.Lstat(entry.Path)
	if os.IsNotExist(err) && !entry.Existed {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrStaleApply, entry.Path)
	}
	current, err := root.Open(entry.Path)
	if err != nil {
		return err
	}
	hash, _, hashErr := applyHash(current)
	security, securityErr := applyOpenFileSecurity(current)
	current.Close()
	if hashErr != nil {
		return hashErr
	}
	before := entry.Existed && hash == entry.BeforeHash && info.Mode().Perm() == entry.BeforeMode
	after := allowAfter && hash == entry.AfterHash && info.Mode().Perm() == entry.AfterMode
	if runtime.GOOS == "windows" {
		if securityErr != nil {
			return fmt.Errorf("%w: file security unavailable: %s", ErrStaleApply, entry.Path)
		}
		before = before && entry.BeforeSecurity != "" && security == entry.BeforeSecurity
		after = after && entry.AfterSecurity != "" && security == entry.AfterSecurity
	}
	if !before && !after {
		return fmt.Errorf("%w: %s", ErrStaleApply, entry.Path)
	}
	return nil
}

func validateApplyBlob(root *os.Root, name, expectedHash string, expectedSize int64) error {
	path := filepath.Join("pending", name)
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return fmt.Errorf("invalid recovery image file: %s", name)
	}
	file, err := root.Open(path)
	if err != nil {
		return err
	}
	hash, size, err := applyHash(io.LimitReader(file, maxApplyBackupBytes+1))
	file.Close()
	if err != nil || hash != expectedHash || size != expectedSize {
		return fmt.Errorf("corrupt recovery image: %s", name)
	}
	return nil
}

// A journal owns only a registered scratch file that still contains a prefix
// of its sealed preimage/postimage. An unrelated file occupying the name is a
// conflict, including an O_EXCL creation failure; it must never be deleted.
func validateApplyTemp(root, state *os.Root, entry applyJournalEntry) error {
	info, err := root.Lstat(entry.Temp)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxApplyBackupBytes {
		return fmt.Errorf("%w: recovery temporary path changed", ErrStaleApply)
	}
	file, err := root.Open(entry.Temp)
	if err != nil {
		return err
	}
	current, _, err := applyHash(io.LimitReader(file, maxApplyBackupBytes+1))
	file.Close()
	if err != nil {
		return err
	}
	for _, blob := range []string{entry.AfterBlob, entry.Blob} {
		if blob == "" {
			continue
		}
		file, err := state.Open(filepath.Join("pending", blob))
		if err != nil {
			return err
		}
		hash, size, err := applyHash(io.LimitReader(file, info.Size()))
		file.Close()
		if err == nil && size == info.Size() && hash == current {
			return nil
		}
	}
	return fmt.Errorf("%w: recovery temporary content changed", ErrStaleApply)
}

func checkApplyWorkspace(path, identity string) error {
	current, err := applyWorkspaceIdentity(path)
	if err != nil {
		return err
	}
	if identity == "" || current != identity {
		return fmt.Errorf("%w: workspace directory identity changed", ErrStaleApply)
	}
	return nil
}

func rejectApplyAliases(root *os.Root, path string) error {
	for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
		info, err := root.Lstat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("apply parent must be a real directory: %s", dir)
		}
	}
	return nil
}

func (p *Project) restoreApplyEntry(root *os.Root, s *applyJournalState, entry applyJournalEntry) error {
	if !entry.Existed {
		if err := removeApplyFile(root, entry.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		removeApplyCreatedDirs(root, entry.CreatedDirs)
		return syncApplyDirectory(p.Path)
	}
	source, err := s.root.Open(filepath.Join("pending", entry.Blob))
	if err != nil {
		return err
	}
	defer source.Close()
	name := entry.Temp
	dst, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(name) }()
	if _, err = io.Copy(dst, source); err == nil {
		err = dst.Chmod(entry.BeforeMode)
	}
	if err == nil {
		err = applySetOpenFileSecurity(dst, entry.BeforeSecurity)
	}
	if err == nil {
		err = dst.Sync()
	}
	if err == nil && runtime.GOOS == "windows" {
		security, securityErr := applyOpenFileSecurity(dst)
		if securityErr != nil || entry.BeforeSecurity == "" || security != entry.BeforeSecurity {
			err = fmt.Errorf("%w: restored file security differs: %s", ErrStaleApply, entry.Path)
		}
	}
	err = errors.Join(err, dst.Close())
	if err != nil {
		return err
	}
	if err := replaceApplyFile(root, name, entry.Path); err != nil {
		return err
	}
	return syncApplyDirectory(filepath.Dir(filepath.Join(p.Path, entry.Path)))
}

// Reuse the transaction's opened root. Reopening the pathname here could
// remove directories in a replacement workspace after an external rename.
func removeApplyCreatedDirs(root *os.Root, dirs []string) {
	for _, dir := range dirs {
		info, err := root.Lstat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || root.Remove(dir) != nil {
			return
		}
	}
}

func removeApplyTemp(root *os.Root, entry applyJournalEntry) error {
	info, err := root.Lstat(entry.Temp)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: recovery temporary path is not a regular file", ErrStaleApply)
	}
	return removeApplyFile(root, entry.Temp)
}

func (p *Project) validateApplyPostimages(root *os.Root, journal *applyJournal) error {
	for _, entry := range journal.Entries {
		info, err := root.Lstat(entry.Path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != entry.AfterMode {
			return fmt.Errorf("%w: applied path changed: %s", ErrStaleApply, entry.Path)
		}
		file, err := root.Open(entry.Path)
		if err != nil {
			return err
		}
		hash, _, err := applyHash(file)
		security, securityErr := applyOpenFileSecurity(file)
		file.Close()
		if err != nil || hash != entry.AfterHash {
			return fmt.Errorf("%w: applied content changed: %s", ErrStaleApply, entry.Path)
		}
		if runtime.GOOS == "windows" && (securityErr != nil || entry.AfterSecurity == "" || security != entry.AfterSecurity) {
			return fmt.Errorf("%w: applied security changed: %s", ErrStaleApply, entry.Path)
		}
	}
	return nil
}

func (s *applyJournalState) removePending() error {
	if _, err := s.root.Lstat("pending"); os.IsNotExist(err) {
		s.published = false
		return nil
	} else if err != nil {
		return err
	}
	if err := s.root.RemoveAll("pending"); err != nil {
		return err
	}
	s.published = false
	return syncApplyDirectory(s.path)
}
