package remotesession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStoreCleanupPreservesSessionsAndNonTemporaryEntries(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save("saved", []byte("committed")); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "session-orphan.tmp")
	other := filepath.Join(dir, "operator.tmp")
	matchedDir := filepath.Join(dir, "session-directory.tmp")
	for _, path := range []string{orphan, other} {
		if err := os.WriteFile(path, []byte("keep unless orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(matchedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "session-link.tmp")
	hasLink := os.Symlink(outside, link) == nil
	// A new store performs crash housekeeping even before the first save.
	store = NewStore(dir)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains after reopen: %v", err)
	}
	if err := store.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := store.Cleanup(); err != nil {
		t.Fatal("idempotent cleanup:", err)
	}
	for _, path := range []string{other, matchedDir, outside} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cleanup changed %s: %v", path, err)
		}
	}
	if hasLink {
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("cleanup changed symlink: %v", err)
		}
	}
	if got, err := store.Load("saved"); err != nil || string(got) != "committed" {
		t.Fatalf("saved session changed: %q, %v", got, err)
	}
	assertStorePrivateFiles(t, dir)
}

// This subprocess can pause at a real temporary-file/lock boundary or execute
// actual Save calls. A separate process is needed to prove OS lock release on
// SIGKILL, rather than merely testing a mutex shared by Store instances.
func TestStoreProcessHelper(t *testing.T) {
	mode := os.Getenv("MAKEWAND_STORE_TEST_MODE")
	if mode == "" {
		t.Skip("subprocess fixture")
	}
	dir := os.Getenv("MAKEWAND_STORE_TEST_DIR")
	ready := os.Getenv("MAKEWAND_STORE_TEST_READY")
	store := NewStore(dir)
	if mode == "saving" {
		if err := publishStoreChildReady(ready, []byte("ready")); err != nil {
			t.Fatal(err)
		}
		values := [][]byte{bytes.Repeat([]byte("a"), 4<<20), bytes.Repeat([]byte("b"), 4<<20)}
		for i := 0; ; i++ {
			if err := store.Save("shared", values[i%len(values)]); err != nil {
				t.Fatal(err)
			}
		}
	}
	lock, err := openStoreLock(filepath.Join(dir, ".session.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lockStoreHandle(lock, false); err != nil {
		t.Fatal(err)
	}
	temporary, err := os.CreateTemp(dir, "session-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer temporary.Close()
	if _, err := temporary.Write([]byte("active writer")); err != nil {
		t.Fatal(err)
	}
	if err := temporary.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := publishStoreChildReady(ready, []byte(temporary.Name())); err != nil {
		t.Fatal(err)
	}
	for {
		// #nosec G703 -- ready and its release marker are private t.TempDir paths supplied by the test parent.
		if _, err := os.Stat(ready + ".release"); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := store.pathFor("active")
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G703 -- both generated temporary name and hashed session basename belong to the test parent's private t.TempDir.
	if err := os.Rename(temporary.Name(), target); err != nil {
		t.Fatal("active temporary file was removed:", err)
	}
	if err := syncStoreDirectory(dir); err != nil {
		t.Fatal(err)
	}
}

type storeChild struct {
	command *exec.Cmd
	ready   string
	output  bytes.Buffer
	stopped bool
}

// Publish only after the whole marker has been written and closed. WriteFile
// creates a visible empty file before writing its payload, so existence alone
// cannot prove that the child has published its temporary-file path.
func publishStoreChildReady(ready string, payload []byte) error {
	file, err := os.CreateTemp(filepath.Dir(ready), ".store-ready-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		// #nosec G703 -- CreateTemp returned this marker basename inside the parent's private t.TempDir.
		_ = os.Remove(file.Name())
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// #nosec G703 -- both marker paths belong to the test parent's private t.TempDir; ready is fresh for this child.
	return os.Rename(file.Name(), ready)
}

func waitStoreChildReady(ctx context.Context, ready, dir, mode string) error {
	for {
		// #nosec G703 -- ready is the parent's private subprocess coordination path.
		payload, err := os.ReadFile(ready)
		// A complete atomic marker can briefly be unavailable to an opener on
		// Windows. Retry only sharing/lock violations within the original ctx;
		// permissions, other I/O errors and invalid paths remain fatal.
		windowsMarkerBusy := runtime.GOOS == "windows" &&
			(errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33)))
		if err != nil && !os.IsNotExist(err) && !windowsMarkerBusy {
			return err
		}
		if err == nil {
			if mode == "saving" && bytes.Equal(payload, []byte("ready")) {
				return nil
			}
			path := string(payload)
			name := filepath.Base(path)
			if mode == "paused" && filepath.Dir(path) == dir && strings.HasPrefix(name, "session-") && strings.HasSuffix(name, ".tmp") {
				// #nosec G703 -- path is constrained to the child's CreateTemp pattern inside the parent's private store directory.
				info, err := os.Stat(path)
				if err == nil && info.Mode().IsRegular() {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStoreChildReadinessWaitsForCompletePayload(t *testing.T) {
	dir := t.TempDir()
	temporary, err := os.CreateTemp(dir, "session-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"paused", "saving"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			if err := os.WriteFile(ready, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- waitStoreChildReady(ctx, ready, dir, mode) }()
			assertWaiting := func(phase string) {
				t.Helper()
				select {
				case err := <-done:
					t.Fatalf("readiness accepted %s marker: %v", phase, err)
				case <-time.After(50 * time.Millisecond):
				}
			}
			assertWaiting("empty")
			payload := []byte(temporary.Name())
			if mode == "saving" {
				payload = []byte("ready")
			}
			if err := os.WriteFile(ready, payload[:len(payload)-1], 0o600); err != nil {
				t.Fatal(err)
			}
			assertWaiting("partial")
			if err := os.WriteFile(ready, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("readiness did not accept the complete marker")
			}
			published := filepath.Join(t.TempDir(), "ready")
			if err := publishStoreChildReady(published, payload); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(published); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("published marker is incomplete: %q, %v", got, err)
			}
		})
	}
}

func TestStoreChildReadinessRejectsNonTransientReadError(t *testing.T) {
	ready := t.TempDir() // A directory is not a valid readable marker.
	_, expected := os.ReadFile(ready)
	if expected == nil {
		t.Fatal("fixture directory unexpectedly read as a marker")
	}
	var pathError *os.PathError
	if !errors.As(expected, &pathError) {
		t.Fatalf("fixture did not produce a filesystem error: %v", expected)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitStoreChildReady(ctx, ready, t.TempDir(), "saving") }()
	select {
	case err := <-done:
		if !errors.Is(err, pathError.Err) {
			t.Fatalf("readiness replaced the original non-transient error: %v, want %v", err, expected)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness retried a non-transient filesystem error")
	}
}

func startStoreChild(t *testing.T, dir, mode string) *storeChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := &storeChild{ready: filepath.Join(t.TempDir(), "ready")}
	child.command = exec.Command(executable, "-test.run=^TestStoreProcessHelper$", "-test.timeout=30s")
	child.command.Env = append(os.Environ(), "MAKEWAND_STORE_TEST_MODE="+mode, "MAKEWAND_STORE_TEST_DIR="+dir, "MAKEWAND_STORE_TEST_READY="+child.ready)
	child.command.Stdout = &child.output
	child.command.Stderr = &child.output
	if err := child.command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !child.stopped {
			_ = child.command.Process.Kill()
			_ = child.command.Wait()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitStoreChildReady(ctx, child.ready, dir, mode); err != nil {
		t.Fatal("child readiness failed:", err)
	}
	return child
}

func (child *storeChild) kill(t *testing.T) {
	t.Helper()
	if err := child.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.command.Wait(); err == nil {
		t.Fatal("child exited normally instead of being killed")
	}
	child.stopped = true
}

func TestStoreCleanupWaitsForActiveWriterAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	child := startStoreChild(t, dir, "paused")
	temporary, err := os.ReadFile(child.ready)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir) // The nonblocking cleanup must skip the active writer.
	if err := store.Save("another", []byte("independent")); err != nil {
		t.Fatal("shared writer did not proceed:", err)
	}
	// #nosec G703 -- temporary is the trusted child fixture's CreateTemp name in the parent's private test directory.
	if got, err := os.ReadFile(string(temporary)); err != nil || string(got) != "active writer" {
		t.Fatalf("another writer removed an active temp: %q, %v", got, err)
	}
	done := make(chan error, 1)
	go func() { done <- store.Cleanup() }()
	select {
	case err := <-done:
		t.Fatalf("cleanup bypassed another process's active writer: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(child.ready+".release", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := child.command.Wait(); err != nil {
		t.Fatalf("active writer failed: %v\n%s", err, child.output.String())
	}
	child.stopped = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not resume after writer exited")
	}
	if got, err := store.Load("active"); err != nil || string(got) != "active writer" {
		t.Fatalf("writer did not commit complete data: %q, %v", got, err)
	}
}

func TestStoreCleanupReapsTemporaryFileAfterSIGKILL(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save("previous", []byte("previously acknowledged")); err != nil {
		t.Fatal(err)
	}
	child := startStoreChild(t, dir, "paused")
	temporary, err := os.ReadFile(child.ready)
	if err != nil {
		t.Fatal(err)
	}
	child.kill(t)
	// #nosec G703 -- temporary is the trusted child fixture's CreateTemp name in the parent's private test directory.
	if _, err := os.Stat(string(temporary)); err != nil {
		t.Fatalf("fault fixture left no orphan: %v", err)
	}
	reopened := NewStore(dir)
	// #nosec G703 -- temporary is the trusted child fixture's CreateTemp name in the parent's private test directory.
	if _, err := os.Stat(string(temporary)); !os.IsNotExist(err) {
		t.Fatalf("reopen did not reap crashed writer: %v", err)
	}
	if got, err := reopened.Load("previous"); err != nil || string(got) != "previously acknowledged" {
		t.Fatalf("recovery changed acknowledged data: %q, %v", got, err)
	}
	if err := reopened.Save("next", []byte("after restart")); err != nil {
		t.Fatal(err)
	}
	assertStorePrivateFiles(t, dir)
}

func TestStoreSaveSIGKILLRecoversCompletePayload(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save("shared", []byte("baseline")); err != nil {
		t.Fatal(err)
	}
	child := startStoreChild(t, dir, "saving")
	deadline := time.Now().Add(5 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		files, err := filepath.Glob(filepath.Join(dir, "session-*.tmp"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) > 0 {
			observed = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !observed {
		t.Fatal("did not observe a real Save in progress")
	}
	child.kill(t)
	reopened := NewStore(dir)
	got, err := reopened.Load("shared")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("baseline")) && !bytes.Equal(got, bytes.Repeat([]byte("a"), 4<<20)) && !bytes.Equal(got, bytes.Repeat([]byte("b"), 4<<20)) {
		t.Fatalf("interrupted Save recovered a partial payload of %d bytes", len(got))
	}
	files, err := filepath.Glob(filepath.Join(dir, "session-*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatalf("crash temporaries remain: %v, %v", files, err)
	}
}

func TestStoreConcurrentInstancesAndCleanup(t *testing.T) {
	dir := t.TempDir()
	stores := make([]*Store, 8)
	allowed := make(map[string]bool, len(stores))
	values := make([][]byte, len(stores))
	for i := range stores {
		stores[i] = NewStore(dir)
		values[i] = []byte(fmt.Sprintf("writer-%d:%s", i, strings.Repeat("x", 8192)))
		allowed[string(values[i])] = true
	}
	if err := stores[0].Save("shared", values[0]); err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, len(stores)+1)
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				id := fmt.Sprintf("writer-%d-session-%d", i, j)
				if err := store.Save(id, values[i]); err != nil {
					failures <- err
					return
				}
				got, err := store.Load(id)
				if err != nil || !bytes.Equal(got, values[i]) {
					failures <- fmt.Errorf("distinct session mismatch: %v", err)
					return
				}
				if err := store.Save("shared", values[i]); err != nil {
					failures <- err
					return
				}
				got, err = store.Load("shared")
				if err != nil || !allowed[string(got)] {
					failures <- fmt.Errorf("incomplete shared session: %v", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			if err := stores[0].Cleanup(); err != nil {
				failures <- err
				return
			}
		}
	}()
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	assertStorePrivateFiles(t, dir)
	files, err := filepath.Glob(filepath.Join(dir, "session-*.tmp"))
	if err != nil || len(files) != 0 {
		t.Fatalf("unexpected temp files after concurrent saves: %v, %v", files, err)
	}
}

func TestStoreRejectsSymlinkLock(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".session.lock")); err != nil {
		t.Skip("symlink creation unavailable:", err)
	}
	if err := NewStore(dir).Save("one", []byte("content")); err == nil {
		t.Fatal("accepted a symlink lock file")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "external" {
		t.Fatalf("changed external lock target: %q, %v", got, err)
	}
}

func TestStoreLockNormalizesPermissionsAndSpecialBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".session.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o660|os.ModeSetuid|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(dir).Save("private", []byte("content")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("lock mode=%s, want private regular file without special bits", info.Mode())
	}
}

func assertStorePrivateFiles(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() && info.Mode().Perm() != 0o600 {
			t.Errorf("file %s mode=%o, want 600", entry.Name(), info.Mode().Perm())
		}
	}
}
