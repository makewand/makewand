package remotesession

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	quotaAlice = "org:0:|proj:0:|user:9:usr_alice"
	quotaBob   = "org:0:|proj:0:|user:7:usr_bob"
)

func quotaKind(err error) QuotaKind {
	var quotaErr *QuotaError
	if errors.As(err, &quotaErr) {
		return quotaErr.Kind
	}
	return 0
}

func assertNoSessionTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "session-*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files remain: %v, %v", files, err)
	}
}

// go-server-1021-3: Store.Save did no accounting, so one sessions:write token
// could fill the disk the state DB lives on with fresh sessions. A store with
// a quota caps the sessions each owner keeps; other owners are unaffected.
func TestStoreQuotaCapsSessionsPerOwner(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 3})
	for i := range 3 {
		if err := store.SaveForOwner(quotaAlice, fmt.Sprintf("ws-%d", i), []byte("alice")); err != nil {
			t.Fatal(err)
		}
	}
	err := store.SaveForOwner(quotaAlice, "ws-3", []byte("alice"))
	if quotaKind(err) != QuotaSessions {
		t.Fatalf("fourth session: err = %v, want a session-count QuotaError", err)
	}
	if _, err := store.LoadForOwner(quotaAlice, "ws-3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused session was stored: %v", err)
	}
	assertNoSessionTemps(t, dir)
	// Replacing a stored session does not add one.
	if err := store.SaveForOwner(quotaAlice, "ws-1", []byte("alice, updated")); err != nil {
		t.Fatalf("replacement at the limit: %v", err)
	}
	for i := range 3 {
		if err := store.SaveForOwner(quotaBob, fmt.Sprintf("ws-%d", i), []byte("bob")); err != nil {
			t.Fatalf("other owner refused: %v", err)
		}
	}
	// Deleting a session frees its slot.
	if err := store.DeleteForOwner(quotaAlice, "ws-0"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForOwner(quotaAlice, "ws-3", []byte("alice")); err != nil {
		t.Fatalf("save after delete: %v", err)
	}
	assertStorePrivateFiles(t, dir)
}

func TestStoreQuotaCapsBytesPerOwner(t *testing.T) {
	store := NewStoreWithQuota(t.TempDir(), Quota{MaxBytesPerOwner: 1000})
	if err := store.SaveForOwner(quotaAlice, "a", bytes.Repeat([]byte("a"), 600)); err != nil {
		t.Fatal(err)
	}
	err := store.SaveForOwner(quotaAlice, "b", bytes.Repeat([]byte("b"), 500))
	var quotaErr *QuotaError
	if !errors.As(err, &quotaErr) || quotaErr.Kind != QuotaBytes || quotaErr.Limit != 1000 || quotaErr.Usage != 1100 {
		t.Fatalf("err = %#v, want a byte QuotaError at 1100 of 1000", err)
	}
	if err := store.SaveForOwner(quotaAlice, "b", bytes.Repeat([]byte("b"), 400)); err != nil {
		t.Fatalf("save up to the limit: %v", err)
	}
	if err := store.SaveForOwner(quotaBob, "a", bytes.Repeat([]byte("c"), 1000)); err != nil {
		t.Fatalf("other owner refused: %v", err)
	}
	// The unscoped namespace (single-user and identity-less tokens) is an
	// owner of its own.
	if err := store.Save("a", bytes.Repeat([]byte("d"), 1000)); err != nil {
		t.Fatalf("unscoped owner refused: %v", err)
	}
	if err := store.Save("b", []byte("d")); quotaKind(err) != QuotaBytes {
		t.Fatalf("unscoped owner over its bytes: %v", err)
	}
}

// A replacement counts only its size difference, and a save that adds neither
// a session nor bytes is allowed even for an owner over a lowered limit.
func TestStoreQuotaCountsReplacementsByTheirDifference(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 2, MaxBytesPerOwner: 1000})
	for _, size := range []int{900, 950, 1000} {
		if err := store.SaveForOwner(quotaAlice, "a", bytes.Repeat([]byte("a"), size)); err != nil {
			t.Fatalf("replacement with %d bytes: %v", size, err)
		}
	}
	if err := store.SaveForOwner(quotaAlice, "b", []byte("b")); quotaKind(err) != QuotaBytes {
		t.Fatalf("new byte over the limit: %v", err)
	}
	if err := store.SaveForOwner(quotaAlice, "a", bytes.Repeat([]byte("a"), 400)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForOwner(quotaAlice, "b", bytes.Repeat([]byte("b"), 600)); err != nil {
		t.Fatalf("save into the freed bytes: %v", err)
	}

	lowered := NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 1, MaxBytesPerOwner: 500})
	if err := lowered.SaveForOwner(quotaAlice, "b", bytes.Repeat([]byte("b"), 300)); err != nil {
		t.Fatalf("shrinking replacement over a lowered limit: %v", err)
	}
	if err := lowered.SaveForOwner(quotaAlice, "b", bytes.Repeat([]byte("b"), 301)); quotaKind(err) != QuotaBytes {
		t.Fatalf("growing replacement over a lowered limit: %v", err)
	}
	if err := lowered.SaveForOwner(quotaAlice, "c", []byte("c")); quotaKind(err) != QuotaSessions {
		t.Fatalf("new session over a lowered limit: %v", err)
	}
}

// The ledger follows the directory: a session removed outside the store frees
// its quota, and a session stored before accounting began counts at its size
// once saved again, never as a new session.
func TestStoreQuotaLedgerFollowsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	unlimited := NewStore(dir)
	if err := unlimited.SaveForOwner(quotaAlice, "legacy", bytes.Repeat([]byte("l"), 700)); err != nil {
		t.Fatal(err)
	}
	store := NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 2, MaxBytesPerOwner: 1000})
	if err := store.SaveForOwner(quotaAlice, "new", bytes.Repeat([]byte("n"), 300)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForOwner(quotaAlice, "legacy", bytes.Repeat([]byte("l"), 701)); quotaKind(err) != QuotaBytes {
		t.Fatalf("growing a legacy session past the bytes: %v", err)
	}
	if err := store.SaveForOwner(quotaAlice, "legacy", bytes.Repeat([]byte("l"), 700)); err != nil {
		t.Fatalf("updating a legacy session: %v", err)
	}
	if err := store.SaveForOwner(quotaAlice, "third", []byte("t")); quotaKind(err) != QuotaSessions {
		t.Fatalf("third session: %v", err)
	}
	path, err := store.sessionPath(quotaAlice, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForOwner(quotaAlice, "third", bytes.Repeat([]byte("t"), 700)); err != nil {
		t.Fatalf("save after the session file was removed: %v", err)
	}
}

// The MinFreeBytes floor refuses writes that would leave the data disk with
// less free space, counting a replacement's full new size.
func TestStoreQuotaFreeSpaceFloor(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithQuota(dir, Quota{MinFreeBytes: 1000})
	free := uint64(1500)
	var freeErr error
	store.freeSpace = func(got string) (uint64, error) {
		if got != dir {
			t.Errorf("free space probed for %q, want %q", got, dir)
		}
		return free, freeErr
	}
	if err := store.Save("a", bytes.Repeat([]byte("a"), 500)); err != nil {
		t.Fatalf("save down to the floor: %v", err)
	}
	err := store.Save("a", bytes.Repeat([]byte("a"), 501))
	var quotaErr *QuotaError
	if !errors.As(err, &quotaErr) || quotaErr.Kind != QuotaFreeSpace || quotaErr.Limit != 1000 || quotaErr.Usage != 1500 {
		t.Fatalf("err = %#v, want a free-space QuotaError", err)
	}
	if got, err := store.Load("a"); err != nil || len(got) != 500 {
		t.Fatalf("refused write changed the session: %d bytes, %v", len(got), err)
	}
	assertNoSessionTemps(t, dir)
	// Another owner hits the same floor: it protects the disk, not an owner.
	if err := store.SaveForOwner(quotaBob, "b", bytes.Repeat([]byte("b"), 501)); quotaKind(err) != QuotaFreeSpace {
		t.Fatalf("other owner below the floor: %v", err)
	}
	freeErr = errFreeSpaceUnsupported
	if err := store.Save("b", bytes.Repeat([]byte("b"), 2000)); err != nil {
		t.Fatalf("unsupported platform: %v", err)
	}
	freeErr = errors.New("statfs failed")
	if err := store.Save("c", []byte("c")); err == nil || quotaKind(err) != 0 {
		t.Fatalf("probe failure: %v, want a plain error", err)
	}
}

// Concurrent saves for one owner cannot all pass the check: exactly the limit
// is admitted.
func TestStoreQuotaAdmitsExactlyTheLimitUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	stores := []*Store{NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 5}), NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 5})}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, refused := 0, 0
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := stores[i%len(stores)].SaveForOwner(quotaAlice, fmt.Sprintf("ws-%d", i), []byte("x"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case quotaKind(err) == QuotaSessions:
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admitted != 5 || refused != 19 {
		t.Fatalf("admitted %d and refused %d of 24, want 5 and 19", admitted, refused)
	}
	assertNoSessionTemps(t, dir)
}

// A save holds its owner's accounting lock from the ledger read until the
// session is published and synced. The lock is per owner, so one account's
// saves, however slow (a stuck fsync on a network disk), never stall another
// account's; one store-wide lock queued every account behind them.
func TestStoreQuotaLockIsPerOwner(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 5})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := store.lockQuota(quotaAlice)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { held.Close() }) }
	var wg sync.WaitGroup
	t.Cleanup(func() { release(); wg.Wait() })
	save := func(owner string) <-chan error {
		done := make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			done <- store.SaveForOwner(owner, "ws", []byte(owner))
		}()
		return done
	}
	alice, bob := save(quotaAlice), save(quotaBob)
	select {
	case err := <-bob:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bob's save waited for alice's accounting lock")
	}
	select {
	case err := <-alice:
		t.Fatalf("alice's save ran while her accounting lock was held: %v", err)
	default:
	}
	release()
	if err := <-alice; err != nil {
		t.Fatal(err)
	}
	if got, err := store.LoadForOwner(quotaAlice, "ws"); err != nil || string(got) != quotaAlice {
		t.Fatalf("alice's session = %q, %v", got, err)
	}
}

// An unreadable ledger fails its owner's saves closed until it is removed, and
// the error, which the client sees, names the file but not the server's data
// directory. Other owners are unaffected.
func TestStoreQuotaUnreadableLedgerFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreWithQuota(dir, Quota{MaxSessionsPerOwner: 5})
	if err := store.SaveForOwner(quotaAlice, "a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	ledger := store.ledgerPath(quotaAlice)
	if err := os.WriteFile(ledger, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := store.SaveForOwner(quotaAlice, "b", []byte("b"))
	if err == nil || quotaKind(err) != 0 || !strings.Contains(err.Error(), filepath.Base(ledger)) || strings.Contains(err.Error(), dir) {
		t.Fatalf("err = %v, want a plain error naming %s without its directory", err, filepath.Base(ledger))
	}
	if _, err := store.LoadForOwner(quotaAlice, "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session saved past an unreadable ledger: %v", err)
	}
	if err := store.SaveForOwner(quotaBob, "b", []byte("b")); err != nil {
		t.Fatalf("other owner: %v", err)
	}
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForOwner(quotaAlice, "b", []byte("b")); err != nil {
		t.Fatalf("save after the ledger was removed: %v", err)
	}
}
