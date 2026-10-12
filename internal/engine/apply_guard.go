package engine

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrApplyConflict reports that a workspace file changed between preimage
// sealing and replacement (for example, an editor saved concurrently). Both
// versions are kept: the external version stays at the target path and the
// candidate content is moved to ApplyConflictError.ConflictPath.
var ErrApplyConflict = errors.New("apply target changed concurrently")

// ApplyConflictError carries the sibling path that preserves the candidate.
// It matches both ErrApplyConflict and ErrStaleApply with errors.Is.
type ApplyConflictError struct {
	Path         string // workspace-relative target, holding the external version
	ConflictPath string // workspace-relative sibling holding the candidate ("" if it could not be moved)
	Detail       string
}

func (e *ApplyConflictError) Error() string {
	msg := fmt.Sprintf("%v: %s", ErrApplyConflict, e.Path)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	if e.ConflictPath != "" {
		msg += "; external version kept at target, candidate preserved at " + e.ConflictPath
	}
	return msg
}

func (e *ApplyConflictError) Unwrap() []error { return []error{ErrApplyConflict, ErrStaleApply} }

// applyReplaceGuard describes the image the replaced path must still hold.
// existed=false requires the target to be absent; otherwise the swapped-out
// file must be a regular file whose content hash is one of accept.
type applyReplaceGuard struct {
	existed bool
	accept  []string
}

// errApplyAtomicUnsupported means the platform or filesystem cannot perform an
// atomic exchange / no-replace rename; callers fall back to verify-then-rename.
var errApplyAtomicUnsupported = errors.New("atomic exchange rename unsupported")

// Test seams. applyBeforeGuardedReplace runs after the last preimage check and
// immediately before the guarded rename, simulating a concurrent editor save.
var (
	applyBeforeGuardedReplace func(destination string)
	applyRenameExchange       = platformApplyRenameExchange
	applyRenameNoReplace      = platformApplyRenameNoReplace
)

func (g *applyReplaceGuard) matches(root *os.Root, path string) (bool, error) {
	info, err := root.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	file, err := root.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return false, err
	}
	hash, _, err := applyHash(file)
	if err != nil {
		return false, err
	}
	for _, want := range g.accept {
		if want != "" && hash == want {
			return true, nil
		}
	}
	return false, nil
}

// guardedReplaceApplyFile atomically replaces destination with source only if
// destination still holds the guarded image.
//
// Linux: an existing target is swapped with renameat2(RENAME_EXCHANGE); the
// file that came out is hashed and, on mismatch, swapped back. A new target is
// created with RENAME_NOREPLACE so a concurrently created file is never
// clobbered. During the exchange window the journal's Temp slot holds the
// swapped-out file; recovery accepts it only when it equals the sealed
// preimage/postimage (validateApplyTemp), otherwise evidence is preserved.
//
// Other platforms, or filesystems rejecting renameat2 (ENOSYS/EINVAL), verify
// the target immediately before a plain rename. That narrows, but does not
// close, the window in which an external save can be overwritten.
func guardedReplaceApplyFile(root *os.Root, source, destination string, guard *applyReplaceGuard) error {
	if guard == nil {
		return replaceApplyFile(root, source, destination)
	}
	if hook := applyBeforeGuardedReplace; hook != nil {
		hook(destination)
	}
	if !guard.existed {
		err := applyRenameNoReplace(root, source, destination)
		if errors.Is(err, errApplyAtomicUnsupported) {
			if _, statErr := root.Lstat(destination); statErr == nil {
				return preserveApplyConflict(root, source, destination, "target was created concurrently")
			} else if !os.IsNotExist(statErr) {
				return statErr
			}
			return replaceApplyFile(root, source, destination)
		}
		if errors.Is(err, fs.ErrExist) {
			return preserveApplyConflict(root, source, destination, "target was created concurrently")
		}
		return err
	}
	err := applyRenameExchange(root, source, destination)
	if errors.Is(err, errApplyAtomicUnsupported) {
		ok, verifyErr := guard.matches(root, destination)
		if verifyErr != nil {
			return verifyErr
		}
		if !ok {
			return preserveApplyConflict(root, source, destination, "target differs from sealed preimage")
		}
		return replaceApplyFile(root, source, destination)
	}
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := root.Lstat(source); statErr == nil {
			return preserveApplyConflict(root, source, destination, "target was removed concurrently")
		}
		return err
	}
	if err != nil {
		return err
	}
	// source now holds the file that was at destination.
	ok, verifyErr := guard.matches(root, source)
	if verifyErr == nil && ok {
		return nil // caller removes the swapped-out preimage (sealed in the journal)
	}
	if swapErr := applyRenameExchange(root, source, destination); swapErr != nil {
		return &ApplyConflictError{Path: destination, Detail: fmt.Sprintf(
			"swap-back failed (%v); candidate is at the target and the external version is preserved at %s", swapErr, source)}
	}
	detail := "target differs from sealed preimage"
	if verifyErr != nil {
		detail = fmt.Sprintf("target could not be verified: %v", verifyErr)
	}
	return preserveApplyConflict(root, source, destination, detail)
}

// preserveApplyConflict moves the candidate at source to a fresh sibling so the
// caller's temporary cleanup cannot delete it. The target is left untouched.
func preserveApplyConflict(root *os.Root, source, destination, detail string) error {
	conflict := filepath.Join(filepath.Dir(destination), filepath.Base(destination)+".makewand-conflict-"+rand.Text())
	err := applyRenameNoReplace(root, source, conflict)
	if errors.Is(err, errApplyAtomicUnsupported) {
		if _, statErr := root.Lstat(conflict); os.IsNotExist(statErr) {
			err = replaceApplyFile(root, source, conflict)
		} else {
			err = fmt.Errorf("conflict path unavailable: %v", statErr)
		}
	}
	if err != nil {
		return &ApplyConflictError{Path: destination, Detail: fmt.Sprintf("%s; candidate kept at %s (%v)", detail, source, err)}
	}
	return &ApplyConflictError{Path: destination, ConflictPath: conflict, Detail: detail}
}

// applyConflictKeepsTemp reports whether the temporary path may now hold data
// that must not be deleted (an external version after a failed swap-back, or
// a candidate that could not be moved aside).
func applyConflictKeepsTemp(err error) bool {
	var conflict *ApplyConflictError
	return errors.As(err, &conflict) && conflict.ConflictPath == ""
}
