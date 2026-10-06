//go:build windows

package remotesession

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStoreWindowsReaderAllowsReplacementAndDeletion(t *testing.T) {
	store := NewStore(t.TempDir())
	old := bytes.Repeat([]byte("old complete image\n"), 4096)
	updated := bytes.Repeat([]byte("new complete image\n"), 4096)
	if err := store.Save("shared", old); err != nil {
		t.Fatal(err)
	}
	path, err := store.pathFor("shared")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := openSessionRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// Holding the actual Load handle makes the previous sharing violation
	// deterministic, rather than depending on short overlapping ReadFile calls.
	if err := store.Save("shared", updated); err != nil {
		t.Fatal("replace while the previous image is being read:", err)
	}
	if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, old) {
		t.Fatalf("previous reader changed: bytes=%d err=%v", len(got), err)
	}
	if got, err := store.Load("shared"); err != nil || !bytes.Equal(got, updated) {
		t.Fatalf("new reader did not get complete replacement: bytes=%d err=%v", len(got), err)
	}
	current, err := openSessionRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err := store.Delete("shared"); err != nil {
		t.Fatal("delete while current image is being read:", err)
	}
	if _, err := store.Load("shared"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load after deletion with readers still open: %v", err)
	}
	recreated := bytes.Repeat([]byte("recreated complete image\n"), 4096)
	if err := store.Save("shared", recreated); err != nil {
		t.Fatal("recreate while deleted image is being read:", err)
	}
	if got, err := store.Load("shared"); err != nil || !bytes.Equal(got, recreated) {
		t.Fatalf("new reader did not get complete recreated image: bytes=%d err=%v", len(got), err)
	}
	if got, err := io.ReadAll(current); err != nil || !bytes.Equal(got, updated) {
		t.Fatalf("deleted image reader changed: bytes=%d err=%v", len(got), err)
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, old) {
		t.Fatalf("original image reader changed after recreation: bytes=%d err=%v", len(got), err)
	}
}

func TestStoreWindowsReadPreservesOpenErrorsAndLongPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	_, err := readSessionFile(path)
	var pathError *os.PathError
	if !os.IsNotExist(err) || !errors.As(err, &pathError) || pathError.Op != "open" || pathError.Path != path {
		t.Fatalf("missing-file error differs from os.ReadFile: %v", err)
	}
	parent := t.TempDir()
	for i := 0; len(parent) < 270; i++ {
		parent = filepath.Join(parent, strings.Repeat("segment", 8))
	}
	// The test creates through an explicit extended path so this read-helper
	// regression does not depend on machine-wide Win32 long-path policy.
	store := NewStore(`\\?\` + parent)
	want := []byte("long path complete image")
	if err := store.Save("long", want); err != nil {
		t.Fatal(err)
	}
	longPath, err := store.pathFor("long")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readSessionFile(strings.TrimPrefix(longPath, `\\?\`)); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("long path read failed: %q %v", got, err)
	}
}

func TestStoreWindowsReadersRetainEachPublishedImage(t *testing.T) {
	store := NewStore(t.TempDir())
	path, err := store.pathFor("shared")
	if err != nil {
		t.Fatal(err)
	}
	var readers []*os.File
	var images [][]byte
	defer func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}()
	for index := range 16 {
		image := bytes.Repeat([]byte{byte(index)}, 64*1024)
		if err := store.Save("shared", image); err != nil {
			t.Fatalf("save image %d with earlier readers open: %v", index, err)
		}
		reader, err := openSessionRead(path)
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, reader)
		images = append(images, image)
		if got, err := store.Load("shared"); err != nil || !bytes.Equal(got, image) {
			t.Fatalf("new Load image %d: bytes=%d err=%v", index, len(got), err)
		}
	}
	for index, reader := range readers {
		if got, err := io.ReadAll(reader); err != nil || !bytes.Equal(got, images[index]) {
			t.Fatalf("retired reader image %d: bytes=%d err=%v", index, len(got), err)
		}
	}
}

// A real Windows handle with no sharing makes the transient marker-open error
// deterministic. The readiness guard remains bounded and must not treat a
// blocked reader as successful readiness.
func TestStoreWindowsChildReadinessWaitsForSharingRelease(t *testing.T) {
	for _, release := range []bool{true, false} {
		name := "released"
		if !release {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			if err := publishStoreChildReady(ready, []byte("ready")); err != nil {
				t.Fatal(err)
			}
			utf16, err := syscall.UTF16PtrFromString(ready)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := syscall.CreateFile(utf16, syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if handle != syscall.InvalidHandle {
					_ = syscall.CloseHandle(handle)
				}
			}()
			if _, err := os.ReadFile(ready); !errors.Is(err, syscall.Errno(32)) {
				t.Fatalf("exclusive handle did not cause the actual sharing violation: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if !release {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer deadlineCancel()
			}
			done := make(chan error, 1)
			go func() { done <- waitStoreChildReady(ctx, ready, filepath.Dir(ready), "saving") }()
			if !release {
				select {
				case err := <-done:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("held sharing violation bypassed the genuine context deadline: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("readiness did not terminate after the context expired")
				}
				return
			}
			select {
			case err := <-done:
				t.Fatalf("readiness completed while the marker was exclusively held: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if err := syscall.CloseHandle(handle); err != nil {
				t.Fatal(err)
			}
			handle = syscall.InvalidHandle
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("readiness failed after the real sharing handle was released:", err)
				}
			case <-ctx.Done():
				t.Fatal("readiness did not accept the complete released marker")
			}
			if got, err := os.ReadFile(ready); err != nil || !bytes.Equal(got, []byte("ready")) {
				t.Fatalf("readiness changed the complete marker: %q, %v", got, err)
			}
		})
	}
}
