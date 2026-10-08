package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/serverdb"
)

const maxPendingApprovalBytes = 16 << 20

// PendingApproval is recovery evidence, never an execution authorization.
// TrustedAcceptanceRecord and its parent-only authority are intentionally absent.
type PendingApproval struct {
	Schema           int                          `json:"schema"`
	ID               string                       `json:"id"`
	Workspace        string                       `json:"workspace"`
	RootID           string                       `json:"root_id"`
	BaselineDigest   string                       `json:"baseline_digest"`
	Baseline         map[string]PendingFileRecord `json:"baseline"`
	Expected         map[string]PendingFileRecord `json:"expected"`
	Files            []ExtractedFile              `json:"files,omitempty"`
	PayloadDigest    string                       `json:"payload_digest"`
	Kind             string                       `json:"kind"`
	Phase            int                          `json:"phase"`
	Title            string                       `json:"title"`
	Details          string                       `json:"details"`
	Plan             *ExecPlan                    `json:"plan,omitempty"`
	CreatedAt        string                       `json:"created_at"`
	RecoveredApplied bool                         `json:"-"`
}

type PendingFileRecord struct {
	Hash string      `json:"hash"`
	Size int64       `json:"size"`
	Mode os.FileMode `json:"mode"`
}

type pendingApprovalStore struct {
	db    *sql.DB
	lock  *os.File
	guard *os.File
	root  *os.Root
}

func pendingApprovalPath(project string) (string, string, error) {
	workspace, err := canonicalApplyPath(project)
	if err != nil {
		return "", "", err
	}
	base, err := config.ConfigDir()
	if err != nil {
		return "", "", err
	}
	base, err = canonicalApplyPath(filepath.Join(base, "pending-approvals"))
	if err != nil {
		return "", "", err
	}
	if isWithinDir(workspace, base) {
		return "", "", fmt.Errorf("pending approval state must be outside the workspace")
	}
	key := workspace
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(base, hex.EncodeToString(sum[:])), workspace, nil
}

func openPendingApprovalStore(ctx context.Context, project string, create bool) (_ *pendingApprovalStore, resultErr error) {
	path, _, err := pendingApprovalPath(project)
	if err != nil {
		return nil, err
	}
	if !create {
		//nolint:gosec // G703: canonical external config directory plus a SHA256 workspace key, validated by pendingApprovalPath.
		if _, err := os.Lstat(filepath.Join(path, "approvals.sqlite")); os.IsNotExist(err) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	}
	//nolint:gosec // G703: pendingApprovalPath confines recovery state outside the candidate workspace using trusted configuration.
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	for _, directory := range []string{filepath.Dir(path), path} {
		if err := privateApplyDirectory(directory); err != nil {
			return nil, err
		}
	}
	s := &pendingApprovalStore{}
	defer func() {
		if resultErr != nil {
			s.close()
		}
	}()
	s.root, err = os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	s.lock, err = openApplyLock(filepath.Join(path, "approvals.lock"))
	if err != nil {
		return nil, err
	}
	for {
		acquired, err := tryApplyLock(s.lock)
		if err != nil {
			return nil, err
		}
		if acquired {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The database and its sidecars are confined to a private directory. Keep
	// the regular database identity open while SQLite accesses its pathname.
	s.guard, err = openApplyLock(filepath.Join(path, "approvals.sqlite"))
	if err != nil {
		return nil, err
	}
	if err := privatePendingFile(s.guard); err != nil {
		return nil, err
	}
	// Windows traverse privileges can bypass directory ACLs; tighten existing
	// explicit sidecar ACLs on their fixed regular identities as well.
	for _, suffix := range []string{"-wal", "-shm"} {
		name := filepath.Join(path, "approvals.sqlite"+suffix)
		if _, err := s.root.Lstat("approvals.sqlite" + suffix); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		sidecar, err := openApplyLock(name)
		if err != nil {
			return nil, err
		}
		err = privatePendingFile(sidecar)
		_ = sidecar.Close()
		if err != nil {
			return nil, err
		}
	}
	s.db, err = serverdb.Open(filepath.Join(path, "approvals.sqlite"))
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
		return nil, err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, err
	}
	if version > 1 {
		return nil, fmt.Errorf("unsupported pending approval schema %d", version)
	}
	if version == 0 {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			return nil, err
		}
		defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
		_, err = conn.ExecContext(ctx, `CREATE TABLE approvals (id TEXT PRIMARY KEY, body BLOB NOT NULL, digest TEXT NOT NULL, state TEXT NOT NULL, diagnostic TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE TABLE approval_audit (seq INTEGER PRIMARY KEY AUTOINCREMENT, approval_id TEXT NOT NULL, event TEXT NOT NULL, detail TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE candidate_attempts (seq INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL, body BLOB NOT NULL, digest TEXT NOT NULL, recorded_at TEXT NOT NULL);
PRAGMA user_version=1`)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *pendingApprovalStore) close() {
	if s == nil {
		return
	}
	if s.db != nil {
		_ = s.db.Close()
	}
	if s.guard != nil {
		_ = s.guard.Close()
	}
	if s.lock != nil {
		_ = unlockApply(s.lock)
		_ = s.lock.Close()
	}
	if s.root != nil {
		_ = s.root.Close()
	}
}

func approvalWorkspaceManifest(workspace string) (map[string]PendingFileRecord, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	manifest := make(map[string]PendingFileRecord)
	var total int64
	//nolint:gosec // G703: read-only enumeration of the user-selected workspace; every file read uses the held os.Root below.
	err = filepath.WalkDir(workspace, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		if item.Name() == ".git" || filepath.ToSlash(relative) == sandboxHomeRelPath {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if item.IsDir() {
			return nil
		}
		info, err := root.Lstat(relative)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink pending approval symlink: %w", err)
			}
			sum := sha256.Sum256([]byte("symlink:" + target))
			manifest[filepath.ToSlash(relative)] = PendingFileRecord{
				Hash: hex.EncodeToString(sum[:]),
				Size: int64(len(target)),
				Mode: info.Mode().Perm(),
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("pending approval input must be regular or symlink: %s", relative)
		}
		file, err := root.Open(relative)
		if err != nil {
			return err
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return fmt.Errorf("pending approval input identity changed: %s", relative)
		}
		hash, size, err := applyHash(io.LimitReader(file, (512<<20)-total+1))
		after, statErr := file.Stat()
		_ = file.Close()
		if err != nil || statErr != nil || size != opened.Size() || after.ModTime() != opened.ModTime() || after.Mode() != opened.Mode() {
			return fmt.Errorf("pending approval input changed while reading: %s", relative)
		}
		total += size
		if total > 512<<20 || len(manifest) >= 100000 {
			return fmt.Errorf("pending approval baseline exceeds its input limit")
		}
		manifest[filepath.ToSlash(relative)] = PendingFileRecord{Hash: hash, Size: size, Mode: opened.Mode().Perm()}
		return nil
	})
	return manifest, err
}

func approvalBaselineDigest(baseline map[string]PendingFileRecord) string {
	paths := make([]string, 0, len(baseline))
	for p := range baseline {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		r := baseline[p]
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%03o\n", p, r.Hash, r.Size, r.Mode)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func pendingPayloadDigest(files []ExtractedFile, plan *ExecPlan) string {
	data, _ := json.Marshal(struct {
		Files []ExtractedFile `json:"files"`
		Plan  *ExecPlan       `json:"plan"`
	}{files, plan})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SavePendingApproval durably seals the complete baseline before a prompt or
// an automatic write is scheduled. Existing records remain as diagnostic audit.
func (p *Project) SavePendingApproval(ctx context.Context, files []ExtractedFile, kind string, phase int, title, details string, plan *ExecPlan) (*PendingApproval, error) {
	files = append([]ExtractedFile(nil), files...)
	if kind == "file_write" && len(files) == 0 {
		return nil, fmt.Errorf("pending file approval requires a nonempty payload")
	}
	if plan != nil {
		owned := *plan
		owned.Args = append([]string(nil), plan.Args...)
		plan = &owned
	}
	_, workspace, err := pendingApprovalPath(p.Path)
	if err != nil {
		return nil, err
	}
	rootID, err := applyWorkspaceIdentity(workspace)
	if err != nil {
		return nil, err
	}
	baseline, err := approvalWorkspaceManifest(workspace)
	if err != nil {
		return nil, err
	}
	digest := approvalBaselineDigest(baseline)
	expected := make(map[string]PendingFileRecord, len(baseline)+len(files))
	for path, value := range baseline {
		expected[path] = value
	}
	seen := map[string]bool{}
	for _, file := range files {
		if _, err := p.validatePath(file.Path, true); err != nil {
			return nil, err
		}
		path := filepath.ToSlash(filepath.Clean(file.Path))
		if seen[path] {
			return nil, fmt.Errorf("duplicate pending candidate path: %s", path)
		}
		seen[path] = true
		mode := os.FileMode(0o600)
		if before, ok := baseline[path]; ok {
			mode = before.Mode
		}
		if file.ModeKnown {
			mode = file.Mode
		}
		sum := sha256.Sum256([]byte(file.Content))
		expected[path] = PendingFileRecord{Hash: hex.EncodeToString(sum[:]), Size: int64(len(file.Content)), Mode: supportedApplyMode(mode)}
	}
	if err := checkApplyWorkspace(workspace, rootID); err != nil {
		return nil, err
	}
	afterBaseline, err := approvalWorkspaceManifest(workspace)
	if err != nil || approvalBaselineDigest(afterBaseline) != digest {
		return nil, fmt.Errorf("workspace changed while sealing pending approval")
	}
	record := &PendingApproval{Schema: 1, ID: rand.Text(), Workspace: workspace, RootID: rootID,
		BaselineDigest: digest, Baseline: baseline, Expected: expected, Files: append([]ExtractedFile(nil), files...),
		PayloadDigest: pendingPayloadDigest(files, plan), Kind: kind, Phase: phase, Title: title, Details: details,
		Plan: plan, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	body, err := json.Marshal(record)
	if err != nil || len(body) > maxPendingApprovalBytes {
		return nil, fmt.Errorf("pending approval exceeds its persistence limit")
	}
	s, err := openPendingApprovalStore(ctx, p.Path, true)
	if err != nil {
		return nil, err
	}
	defer s.close()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	if _, err := conn.ExecContext(ctx, `UPDATE approvals SET state='superseded' WHERE state IN ('pending','authorized','invalidated')`); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if _, err := conn.ExecContext(ctx, `INSERT INTO approvals(id,body,digest,state,created_at) VALUES(?,?,?,'pending',?)`, record.ID, body, hex.EncodeToString(sum[:]), record.CreatedAt); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO approval_audit(approval_id,event,detail,recorded_at) VALUES(?,'pending',?,?)`, record.ID, record.Kind, record.CreatedAt); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	return record, nil
}

func decodePendingApproval(body []byte, digest string) (*PendingApproval, error) {
	sum := sha256.Sum256(body)
	if len(body) > maxPendingApprovalBytes || hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("pending approval record checksum mismatch")
	}
	var record PendingApproval
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if record.Schema != 1 || record.ID == "" || record.Baseline == nil || record.Expected == nil || record.PayloadDigest != pendingPayloadDigest(record.Files, record.Plan) {
		return nil, fmt.Errorf("pending approval payload or schema changed")
	}
	return &record, nil
}

func (s *pendingApprovalStore) transition(ctx context.Context, id, state, detail string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	query := `UPDATE approvals SET state=?,diagnostic=? WHERE id=? AND state IN ('pending','authorized','invalidated')`
	if state == "applied" || state == "failed" {
		// A completion is bound to its original record and may arrive after a
		// newer candidate superseded it. Retain both decisions in the audit.
		query = `UPDATE approvals SET state=?,diagnostic=? WHERE id=?`
	}
	result, err := conn.ExecContext(ctx, query, state, detail, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return fmt.Errorf("pending approval is no longer active")
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO approval_audit(approval_id,event,detail,recorded_at) VALUES(?,?,?,?)`, id, state, detail, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

// LoadPendingApproval only recovers evidence. A complete postimage means an
// apply committed before its completion message arrived; it is never replayed.
func (p *Project) LoadPendingApproval(ctx context.Context) (*PendingApproval, error) {
	s, err := openPendingApprovalStore(ctx, p.Path, false)
	if err != nil || s == nil {
		return nil, err
	}
	defer s.close()
	var id, digest, state, diagnostic string
	var body []byte
	err = s.db.QueryRowContext(ctx, `SELECT id,body,digest,state,diagnostic FROM approvals WHERE state IN ('pending','authorized','invalidated') ORDER BY created_at DESC LIMIT 1`).Scan(&id, &body, &digest, &state, &diagnostic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if state == "invalidated" {
		return nil, fmt.Errorf("pending approval recovery blocked: %s", diagnostic)
	}
	record, err := decodePendingApproval(body, digest)
	if err == nil && state == "authorized" && record.Kind != "file_write" {
		diagnostic := "A command was authorized before interruption. Inspect its effects and start a new explicit command; saved command approvals are never replayed."
		if err := s.transition(ctx, id, "failed", diagnostic); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s", diagnostic)
	}
	if err == nil {
		err = p.ValidatePendingApproval(ctx, record, record.Files)
	}
	if err == nil {
		return record, nil
	}
	// A checksum/payload/identity failure may never be reinterpreted as success.
	if record != nil && checkApplyWorkspace(p.Path, record.RootID) == nil && record.Kind == "file_write" {
		manifest, snapshotErr := approvalWorkspaceManifest(p.Path)
		if snapshotErr == nil && reflect.DeepEqual(manifest, record.Expected) {
			if err := s.transition(ctx, id, "applied", "complete postimage found after process interruption"); err != nil {
				return nil, err
			}
			record.RecoveredApplied = true
			return record, nil
		}
	}
	_ = s.transition(ctx, id, "invalidated", err.Error())
	return nil, err
}

// ValidatePendingApproval is called inside ApplyFilesTransactional's validate
// callback, under the workspace transaction lock. It takes no apply lock.
func (p *Project) ValidatePendingApproval(ctx context.Context, record *PendingApproval, files []ExtractedFile) error {
	if record == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	workspace, err := canonicalApplyPath(p.Path)
	if err != nil || workspace != record.Workspace {
		return fmt.Errorf("pending approval workspace location changed")
	}
	if err := checkApplyWorkspace(workspace, record.RootID); err != nil {
		return err
	}
	if pendingPayloadDigest(files, record.Plan) != record.PayloadDigest {
		return fmt.Errorf("pending approval payload changed after sealing")
	}
	currentBaseline, err := approvalWorkspaceManifest(workspace)
	if err != nil || approvalBaselineDigest(currentBaseline) != record.BaselineDigest {
		return fmt.Errorf("pending approval baseline changed; inspect preserved recovery evidence")
	}
	return nil
}

// ValidatePendingApprovalForApply checks both the immutable disk evidence and
// its current decision under the independent approvals lock. Another process
// may have denied or superseded a queued write since this App authorized it.
func (p *Project) ValidatePendingApprovalForApply(ctx context.Context, record *PendingApproval, files []ExtractedFile) error {
	if record == nil {
		return nil
	}
	s, err := openPendingApprovalStore(ctx, p.Path, false)
	if err != nil || s == nil {
		return fmt.Errorf("pending approval store unavailable: %v", err)
	}
	defer s.close()
	var body []byte
	var digest, state string
	if err := s.db.QueryRowContext(ctx, `SELECT body,digest,state FROM approvals WHERE id=?`, record.ID).Scan(&body, &digest, &state); err != nil {
		return err
	}
	if _, err := decodePendingApproval(body, digest); err != nil {
		return err
	}
	current, err := json.Marshal(record)
	if err != nil || !bytes.Equal(body, current) || state != "authorized" {
		return fmt.Errorf("pending approval authorization was changed, denied or superseded")
	}
	return p.ValidatePendingApproval(ctx, record, files)
}

// PendingApprovalPlanMatches binds the actual command and every argument to
// the persisted payload. Empty argument slices have one canonical encoding.
func PendingApprovalPlanMatches(record *PendingApproval, plan *ExecPlan) bool {
	if record == nil {
		return true
	}
	if plan == nil {
		return false
	}
	owned := *plan
	owned.Args = append([]string(nil), plan.Args...)
	return record.PayloadDigest == pendingPayloadDigest(nil, &owned)
}

func (p *Project) ValidatePendingApprovalPlanForApply(ctx context.Context, record *PendingApproval, plan ExecPlan) error {
	if !PendingApprovalPlanMatches(record, &plan) {
		return fmt.Errorf("pending approval command payload changed after sealing")
	}
	return p.ValidatePendingApprovalForApply(ctx, record, nil)
}

// TransitionPendingApproval commits user decisions and completion as a separate
// journal. The approvals lock is never the workspace apply lock.
func (p *Project) TransitionPendingApproval(ctx context.Context, record *PendingApproval, state, detail string) error {
	if record == nil {
		return nil
	}
	switch state {
	case "authorized", "denied", "applied", "failed":
	default:
		return fmt.Errorf("invalid pending approval transition: %s", state)
	}
	s, err := openPendingApprovalStore(ctx, p.Path, false)
	if err != nil || s == nil {
		return fmt.Errorf("pending approval store unavailable: %v", err)
	}
	defer s.close()
	var body []byte
	var digest string
	if err := s.db.QueryRowContext(ctx, `SELECT body,digest FROM approvals WHERE id=?`, record.ID).Scan(&body, &digest); err != nil {
		return err
	}
	_, err = decodePendingApproval(body, digest)
	current, marshalErr := json.Marshal(record)
	if err != nil || marshalErr != nil || !bytes.Equal(body, current) {
		return fmt.Errorf("pending approval persisted evidence changed")
	}
	return s.transition(ctx, record.ID, state, detail)
}

// RecordCandidateAttempts retains outcome/usage/digest audit without importing
// any in-process acceptance authority or discarded candidate contents.
func (p *Project) RecordCandidateAttempts(ctx context.Context, selection CandidateSelection) error {
	type attemptAudit struct {
		TaskID, ID, Requested, Provider, Status, ArtifactDigest string
		Strength                                                int
		Usage                                                   any
		ErrorKind                                               *string
	}
	audits := make([]attemptAudit, 0, len(selection.Attempts))
	for _, attempt := range selection.Attempts {
		audits = append(audits, attemptAudit{TaskID: attempt.TaskID, ID: attempt.ID, Requested: attempt.Requested,
			Provider: attempt.Provider, Status: string(attempt.Status), ArtifactDigest: attempt.ArtifactDigest,
			Strength: attempt.Strength, Usage: attempt.Usage, ErrorKind: attempt.ErrorKind})
	}
	body, err := json.Marshal(audits)
	if err != nil || len(body) > maxPendingApprovalBytes {
		return fmt.Errorf("candidate attempt audit exceeds its persistence limit")
	}
	s, err := openPendingApprovalStore(ctx, p.Path, true)
	if err != nil {
		return err
	}
	defer s.close()
	sum := sha256.Sum256(body)
	_, err = s.db.ExecContext(ctx, `INSERT INTO candidate_attempts(task_id,body,digest,recorded_at) VALUES(?,?,?,?)`, selection.TaskID, body, hex.EncodeToString(sum[:]), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
