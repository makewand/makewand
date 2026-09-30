package engine

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type workspaceCopyDevicePair struct{ source, target uint64 }

// Auto mode uses only known copy-on-write filesystems. Unknown filesystems
// retain the ordinary-copy path; an explicit opt-in can probe them safely.
func selectWorkspaceCopier(sourceRoot, targetRoot string) (func(*os.File, *os.File) error, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MAKEWAND_WORKSPACE_REFLINK"))) {
	case "0", "false", "off":
		return ordinaryFileCopy, nil
	case "1", "true", "on":
		return nil, nil
	case "", "auto":
		if workspaceReflinkFilesystem(sourceRoot, targetRoot) {
			return nil, nil
		}
		return ordinaryFileCopy, nil
	default:
		return nil, fmt.Errorf("MAKEWAND_WORKSPACE_REFLINK must be auto, 1, or 0")
	}
}

// Capability failures are cached only for this workspace copy and device pair.
// An unsupported filesystem should not require four failed/reset syscalls per
// file, and a nested mount must still get its own chance to use copy-on-write.
type workspaceCopyCapabilities map[workspaceCopyDevicePair]bool

func (c workspaceCopyCapabilities) copy(in, out *os.File, source, target os.FileInfo) error {
	// Reflinks create separate inodes; hardlinks are never used for mutable inputs.
	sourceDevice, sourceOK := workspaceCopyDevice(source)
	targetDevice, targetOK := workspaceCopyDevice(target)
	pair := workspaceCopyDevicePair{sourceDevice, targetDevice}
	if sourceOK && targetOK && c[pair] {
		return ordinaryFileCopy(in, out)
	}
	return copyFileContentsWith(in, out, func(in, out *os.File) error {
		err := tryReflink(in, out)
		if sourceOK && targetOK && reflinkUnsupported(err) {
			c[pair] = true
		}
		return err
	})
}

func copyFileContentsWith(in, out *os.File, clone func(*os.File, *os.File) error) error {
	if clone(in, out) == nil {
		return nil
	}
	// A failed clone may have changed offsets or written partial data. Reset all
	// state before the complete ordinary copy, including removal of any tail.
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := out.Truncate(0); err != nil {
		return err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return ordinaryFileCopy(in, out)
}

func ordinaryFileCopy(in, out *os.File) error {
	_, err := io.Copy(out, in)
	return err
}

func copyWorkspaceFile(in *os.File, dst string, mode fs.FileMode, copyContents func(*os.File, *os.File) error) (err error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	if err = copyContents(in, out); err != nil {
		return err
	}
	// Preserve executable/read-only bits despite the process umask.
	return out.Chmod(mode)
}

// Match secret locations explicitly rather than broad file extensions that
// would also discard public certificates and unrelated project fixtures.
func isWorkspaceSecretPath(path string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i, part := range parts {
		part = strings.ToLower(part)
		switch part {
		case ".ssh", ".aws", ".azure", ".gnupg", ".kube", ".docker", ".codex", ".claude", ".claude.json", ".gemini", ".password-store", ".git-credentials", ".netrc", ".npmrc", ".pypirc", ".pgpass", ".vault-token":
			return true
		}
		if part == ".env" || strings.HasPrefix(part, ".env.") {
			if part != ".env.example" && part != ".env.sample" && part != ".env.template" {
				return true
			}
		}
		if part == ".cargo" && i+1 < len(parts) && (strings.EqualFold(parts[i+1], "credentials") || strings.EqualFold(parts[i+1], "credentials.toml")) {
			return true
		}
	}
	return false
}
