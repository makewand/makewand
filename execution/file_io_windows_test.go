//go:build windows

package execution

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/makewand/makewand/internal/privatefile"
	"golang.org/x/sys/windows"
)

func TestWindowsAccountingReaderAllowsReplacement(t *testing.T) {
	path := layeredFixture(t)
	err := withLockedFile(t.Context(), path, nil, func(p *accountingPath) error {
		if err := writeLedgerFile(p, map[string]any{"image": "old"}); err != nil {
			return err
		}
		reader, err := openExecutionFile(p.root, p.name, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer reader.Close()
		if err := writeLedgerFile(p, map[string]any{"image": "new"}); err != nil {
			return err
		}
		oldImage, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		newImage, err := readLedgerBytes(p)
		if err != nil {
			return err
		}
		if !bytes.Contains(oldImage, []byte(`"old"`)) || !bytes.Contains(newImage, []byte(`"new"`)) {
			return fmt.Errorf("replacement did not preserve reader images: old=%s new=%s", oldImage, newImage)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWindowsAccountingSharingRetryClassification(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		calls := 0
		err := retryAccountingIO(func() error {
			calls++
			if calls < 4 {
				return &os.PathError{Op: "open", Path: "calls.json", Err: windows.ERROR_SHARING_VIOLATION}
			}
			return nil
		})
		if err != nil || calls != 4 {
			t.Fatalf("transient sharing failure: calls=%d err=%v", calls, err)
		}
	})
	for _, cause := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED, windows.ERROR_LOCK_VIOLATION} {
		t.Run(cause.Error(), func(t *testing.T) {
			calls := 0
			err := retryAccountingIO(func() error {
				calls++
				return fmt.Errorf("ledger I/O: %w", cause)
			})
			wantCalls := 1
			if errors.Is(cause, windows.ERROR_SHARING_VIOLATION) {
				wantCalls = 4
			}
			if calls != wantCalls || !errors.Is(err, cause) || ErrorStatus(err) != InternalError || errors.Is(err, ErrConcurrencyBusy) || errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("I/O failure misclassified or retried without bound: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWindowsPrivateFileSecurityHandleSharesDelete(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.OpenFile("calls.json", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// An existing DELETE-access handle must be compatible with Tighten's
	// security handle. No path is reopened while testing the sharing contract.
	handle, _, callErr := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile").Call(
		file.Fd(), uintptr(windows.DELETE),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE), 0)
	if windows.Handle(handle) == windows.InvalidHandle {
		t.Fatal(callErr)
	}
	defer windows.CloseHandle(windows.Handle(handle))
	if err := privatefile.Tighten(file); err != nil {
		t.Fatal(err)
	}
}
