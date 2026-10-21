package remotesession

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Quota bounds what a Store keeps. A zero field disables that limit.
type Quota struct {
	// MaxSessionsPerOwner caps how many sessions one owner (sessionOwnerKey)
	// stores.
	MaxSessionsPerOwner int
	// MaxBytesPerOwner caps the total size of one owner's sessions.
	MaxBytesPerOwner int64
	// MinFreeBytes refuses a save that would leave less free space than this on
	// the session directory's filesystem, which `makewand serve` shares with
	// its state database by default.
	MinFreeBytes int64
}

// Defaults `makewand serve` applies unless configured otherwise.
const (
	DefaultMaxSessionsPerOwner       = 1000
	DefaultMaxBytesPerOwner    int64 = 256 << 20
	DefaultMinFreeBytes        int64 = 1 << 30
)

// DefaultQuota returns the limits `makewand serve` applies by default.
func DefaultQuota() Quota {
	return Quota{
		MaxSessionsPerOwner: DefaultMaxSessionsPerOwner,
		MaxBytesPerOwner:    DefaultMaxBytesPerOwner,
		MinFreeBytes:        DefaultMinFreeBytes,
	}
}

func (q Quota) perOwner() bool { return q.MaxSessionsPerOwner > 0 || q.MaxBytesPerOwner > 0 }

// QuotaKind names the limit that refused a save.
type QuotaKind int

const (
	QuotaSessions QuotaKind = iota + 1
	QuotaBytes
	QuotaFreeSpace
)

// QuotaError reports a save refused by a Quota limit. Nothing was written.
type QuotaError struct {
	Kind QuotaKind
	// Limit is the configured limit: a session count, or bytes.
	Limit int64
	// Usage is the owner's session count for QuotaSessions, the owner's total
	// after the refused save for QuotaBytes, and the filesystem's free bytes
	// for QuotaFreeSpace.
	Usage int64
}

// Error is the message for the client, which sees its own account's usage but
// not the state of the server's disk (see detail).
func (e *QuotaError) Error() string {
	switch e.Kind {
	case QuotaSessions:
		return fmt.Sprintf("session quota exceeded: this account stores %d sessions and may keep %d; delete sessions it no longer needs before saving a new one", e.Usage, e.Limit)
	case QuotaBytes:
		return fmt.Sprintf("session storage quota exceeded: saving would bring this account's sessions to %s of the %s it may store; delete sessions it no longer needs or save a smaller session", formatBytes(e.Usage), formatBytes(e.Limit))
	default:
		return "session storage is full: the server keeps a free-space reserve on its data disk; ask the operator to free space"
	}
}

// detail is Error with what only the operator sees, in the audit log: the
// free-space floor and the free space that was left.
func (e *QuotaError) detail() string {
	if e.Kind != QuotaFreeSpace {
		return e.Error()
	}
	return fmt.Sprintf("%s (floor %s, %s free)", e.Error(), formatBytes(e.Limit), formatBytes(e.Usage))
}

func formatBytes(n int64) string {
	switch {
	case n < 1<<20:
		return fmt.Sprintf("%d bytes", n)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
}

// errFreeSpaceUnsupported means the platform cannot report free space; the
// MinFreeBytes floor is then not enforced.
var errFreeSpaceUnsupported = errors.New("free space is not available on this platform")

// checkFreeSpace refuses a write of incoming bytes that would leave less than
// MinFreeBytes free. A replacement writes its whole new image before the old
// file is removed, so the full size counts here, unlike in the owner quota.
func (s *Store) checkFreeSpace(incoming int) error {
	if s.quota.MinFreeBytes <= 0 {
		return nil
	}
	freeSpace := s.freeSpace
	if freeSpace == nil {
		freeSpace = diskFreeBytes
	}
	free, err := freeSpace(s.dir)
	if errors.Is(err, errFreeSpaceUnsupported) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check free space for session storage: %w", err)
	}
	need := uint64(s.quota.MinFreeBytes) + uint64(incoming) //nolint:gosec // G115: both are non-negative.
	if free < need {
		return &QuotaError{Kind: QuotaFreeSpace, Limit: s.quota.MinFreeBytes, Usage: int64(min(free, math.MaxInt64))} //nolint:gosec // G115: bounded by min.
	}
	return nil
}

// ownerLedger records the sessions one owner stores, by file name, with their
// sizes. Session files are named by a hash of the owner and workspace, so the
// owner cannot be recovered from the directory itself.
type ownerLedger struct {
	Sessions map[string]int64 `json:"sessions"`
}

// ownerDigest names an owner's accounting files.
func ownerDigest(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])
}

// ledgerPath names an owner's ledger. Its "quota-" prefix cannot collide with
// a session file, whose name is a bare hex digest.
func (s *Store) ledgerPath(owner string) string {
	return filepath.Join(s.dir, "quota-"+ownerDigest(owner)+".json")
}

// quotaLockPath names the lock that serializes an owner's accounting. Like the
// store lock, it is permanent.
func (s *Store) quotaLockPath(owner string) string {
	return filepath.Join(s.dir, ".quota-"+ownerDigest(owner)+".lock")
}

// lockQuota serializes owner's accounting across goroutines and processes. The
// lock is per owner, as the ledger is, so one account's saves, held from the
// ledger read until the session is published and synced, never queue another
// account's. Lock order is the store lock, then this one; Cleanup takes only
// the store lock.
func (s *Store) lockQuota(owner string) (*os.File, error) {
	lock, err := openStoreLock(s.quotaLockPath(owner))
	if err != nil {
		return nil, err
	}
	if err := lockStoreHandle(lock, true); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// loadLedger reads an owner's ledger and reconciles it with the directory:
// entries whose file is gone are dropped and sizes are re-read. Saves record
// a session before publishing it, so a crash can only leave entries this
// repairs, never an unrecorded session. Callers hold the quota lock.
func (s *Store) loadLedger(owner string) (ownerLedger, error) {
	ledger := ownerLedger{Sessions: map[string]int64{}}
	path := s.ledgerPath(owner)
	// #nosec G304 G703 -- ledgerPath hashes the owner key into a basename in the operator-configured storage root.
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ledger, nil
	}
	if err != nil {
		return ledger, err
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		return ledger, fmt.Errorf("session quota ledger %s in the sessions directory is unreadable; remove it to reset this account's accounting: %w", filepath.Base(path), err)
	}
	if ledger.Sessions == nil {
		ledger.Sessions = map[string]int64{}
	}
	for name := range ledger.Sessions {
		if !isSessionFileName(name) {
			delete(ledger.Sessions, name)
			continue
		}
		// #nosec G703 -- name passed isSessionFileName (a lowercase sha256 hex basename) and is joined to the operator-configured storage root.
		info, err := os.Lstat(filepath.Join(s.dir, name))
		if os.IsNotExist(err) {
			delete(ledger.Sessions, name)
			continue
		}
		if err != nil {
			return ledger, err
		}
		if !info.Mode().IsRegular() {
			delete(ledger.Sessions, name)
			continue
		}
		ledger.Sessions[name] = info.Size()
	}
	return ledger, nil
}

// isSessionFileName reports whether name is a session file pathFor produces.
func isSessionFileName(name string) bool {
	digest, ok := strings.CutSuffix(name, ".json")
	if !ok || len(digest) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil && strings.ToLower(digest) == digest
}

// writeLedger atomically replaces an owner's ledger. The temporary file uses
// the session temporary pattern, so Cleanup reaps it after a crash. Callers
// hold the shared store lock and the quota lock.
func (s *Store) writeLedger(owner string, ledger ownerLedger) error {
	data, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	return s.publish(data, s.ledgerPath(owner))
}

// admit checks a save of size bytes as session file name against the owner's
// ledger. Only growth is refused: replacing a session changes the total by the
// size difference, and a save that does not add a session or bytes is always
// allowed, even for an owner over a lowered limit.
func (q Quota) admit(ledger ownerLedger, name string, size int64) error {
	var total int64
	for _, recorded := range ledger.Sessions {
		total += recorded
	}
	previous, replacing := ledger.Sessions[name]
	if !replacing && q.MaxSessionsPerOwner > 0 && len(ledger.Sessions) >= q.MaxSessionsPerOwner {
		return &QuotaError{Kind: QuotaSessions, Limit: int64(q.MaxSessionsPerOwner), Usage: int64(len(ledger.Sessions))}
	}
	after := total - previous + size
	if q.MaxBytesPerOwner > 0 && size > previous && after > q.MaxBytesPerOwner {
		return &QuotaError{Kind: QuotaBytes, Limit: q.MaxBytesPerOwner, Usage: after}
	}
	return nil
}
