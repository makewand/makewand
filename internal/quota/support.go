// support.go — the few helpers quota reads shared with the archived router.
package quota

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RepoTrust controls whether repository content is treated as trusted input to
// the generation provider. Untrusted mode only routes to providers that do not
// run a repo-aware agent process on the host.
//
// Backed by int32 so it stores/loads through the Router's atomic.Int32 without a
// widening conversion (the enum has only the two values below).
type RepoTrust int32

const (
	RepoTrustTrusted   RepoTrust = iota // default: every quota source may run
	RepoTrustUntrusted                  // quota sources that exec a local CLI are skipped
)

type httpStatusError struct {
	Status  int
	Code    string
	Message string
}

func (e *httpStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

const cliPrivateCredMaxBytes = 4 << 20

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

// Construct mask targets only from a fixed credential entry in HOME or from
// cliConfiguredCodexHome's dedicated-account authorization, never directly from
// a request pathname.
type cliCredentialMaskTarget struct{ path string }

func cliMaskLinkIsUnambiguous(link string) bool {
	if os.PathSeparator != '\\' || filepath.IsAbs(link) {
		return true
	}
	// A Windows drive-relative or rooted-but-not-absolute target cannot be
	// interpreted relative to the pinned parent without selecting another path.
	return filepath.VolumeName(link) == "" && (len(link) == 0 || !os.IsPathSeparator(link[0]))
}

// cliInspectMaskTarget follows authorized credential aliases through pinned
// parents. The volume root is a namespace anchor, not an account allowlist.
// Every filesystem operation receives a single local component; symlink target
// navigation is handled physically rather than cleaned across another symlink.
func cliInspectMaskTarget(target cliCredentialMaskTarget) (string, os.FileInfo, error) {
	path, err := filepath.Abs(target.path)
	if err != nil {
		return "", nil, err
	}
	var roots []*os.Root
	var paths []string
	closeRoots := func() {
		for _, root := range roots {
			_ = root.Close()
		}
		roots, paths = nil, nil
	}
	defer closeRoots()
	openAnchor := func(path string) ([]string, error) {
		closeRoots()
		anchor := filepath.VolumeName(path) + string(os.PathSeparator)
		root, err := os.OpenRoot(anchor)
		if err != nil {
			return nil, err
		}
		roots, paths = []*os.Root{root}, []string{anchor}
		return strings.Split(strings.TrimPrefix(path, anchor), string(os.PathSeparator)), nil
	}
	parts, err := openAnchor(path)
	if err != nil {
		return "", nil, err
	}
	links := 0
	for len(parts) != 0 {
		component := parts[0]
		parts = parts[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(roots) > 1 {
				_ = roots[len(roots)-1].Close()
				roots, paths = roots[:len(roots)-1], paths[:len(paths)-1]
			}
			continue
		}
		if !filepath.IsLocal(component) || filepath.Base(component) != component {
			return "", nil, errors.New("invalid credential path component")
		}
		parent := roots[len(roots)-1]
		info, err := parent.Lstat(component)
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", nil, errors.New("too many credential symbolic links")
			}
			link, err := parent.Readlink(component)
			if err != nil {
				return "", nil, err
			}
			if !cliMaskLinkIsUnambiguous(link) {
				return "", nil, errors.New("ambiguous credential symbolic link target")
			}
			var linkParts []string
			if filepath.IsAbs(link) {
				linkParts, err = openAnchor(link)
				if err != nil {
					return "", nil, err
				}
			} else {
				linkParts = strings.Split(link, string(os.PathSeparator))
			}
			parts = append(linkParts, parts...)
			continue
		}
		resolved := filepath.Join(paths[len(paths)-1], component)
		if len(parts) == 0 {
			return resolved, info, nil
		}
		if !info.IsDir() {
			return "", nil, errors.New("credential path parent is not a directory")
		}
		next, err := parent.OpenRoot(component)
		if err != nil {
			return "", nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = next.Close()
			return "", nil, errors.New("credential path changed during inspection")
		}
		roots, paths = append(roots, next), append(paths, resolved)
	}
	info, err := roots[len(roots)-1].Stat(".")
	return paths[len(paths)-1], info, err
}
