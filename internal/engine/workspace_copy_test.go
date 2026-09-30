package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceCopyFallbackResetsPartialClone(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "target")
	content := bytes.Repeat([]byte("baseline content\n"), 40)
	if err := os.WriteFile(src, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0751); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	failedClone := func(in, out *os.File) error {
		if _, err := out.Write(bytes.Repeat([]byte("tail"), 1000)); err != nil {
			return err
		}
		if _, err := in.Seek(17, io.SeekStart); err != nil {
			return err
		}
		return errors.New("partial clone failure")
	}
	if err := copyWorkspaceFile(in, dst, 0751, func(in, out *os.File) error { return copyFileContentsWith(in, out, failedClone) }); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, copied) {
		t.Fatal("fallback retained a partial tail or skipped source bytes")
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != sourceInfo.Mode().Perm() {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestCloneWorkspaceIsolatesInPlaceWritesAndSecrets(t *testing.T) {
	t.Setenv("MAKEWAND_WORKSPACE_REFLINK", "1")
	dir := t.TempDir()
	files := map[string]string{"script.sh": "original baseline\n", ".env": "SYNTHETIC_SECRET=x", "nested/.env.local": "SYNTHETIC_SECRET=y", ".aws/credentials": "synthetic", ".ssh/id_rsa": "synthetic", ".env.example": "NAME=placeholder", "nested/.env.template": "NAME=placeholder", ".env.sample": "NAME=placeholder", ".cargo/credentials.toml": "synthetic"}
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, 0751); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("synthetic outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "external")); err != nil {
		t.Logf("symlink check unavailable on this platform: %v", err)
	}
	p, err := OpenProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.CloneToTemp()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(first.Path)
	second, err := p.CloneToTemp()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(second.Path)
	for _, path := range []string{".env", "nested/.env.local", ".aws/credentials", ".ssh/id_rsa", ".cargo/credentials.toml", "external"} {
		if _, err := os.Lstat(filepath.Join(first.Path, path)); !os.IsNotExist(err) {
			t.Fatalf("secret or symlink copied: %s", path)
		}
	}
	for _, path := range []string{".env.example", "nested/.env.template", ".env.sample"} {
		if _, err := os.Stat(filepath.Join(first.Path, path)); err != nil {
			t.Fatalf("template missing: %s", path)
		}
	}
	originalInfo, _ := os.Stat(filepath.Join(p.Path, "script.sh"))
	clonedInfo, _ := os.Stat(filepath.Join(first.Path, "script.sh"))
	if os.SameFile(originalInfo, clonedInfo) {
		t.Fatal("workspace input shares a hardlink")
	}
	if clonedInfo.Mode().Perm() != originalInfo.Mode().Perm() {
		t.Fatal("executable permissions lost")
	}
	f, err := os.OpenFile(filepath.Join(first.Path, "script.sh"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("CHANGED"), 0)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{p.Path, second.Path} {
		data, err := os.ReadFile(filepath.Join(root, "script.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != files["script.sh"] {
			t.Fatal("candidate write modified baseline or sibling")
		}
	}
	if err := os.Chmod(filepath.Join(first.Path, "script.sh"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(p.Path, "script.sh"))
	if info.Mode().Perm() != originalInfo.Mode().Perm() {
		t.Fatal("candidate chmod modified baseline")
	}
}

func TestReflinkCreatesIndependentInodeWhenSupported(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "clone")
	if err := os.WriteFile(src, bytes.Repeat([]byte("baseline"), 1024), 0600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := tryReflink(in, out); err != nil {
		t.Skipf("native reflink unavailable on test filesystem: %v", err)
	}
	a, _ := in.Stat()
	b, _ := out.Stat()
	if os.SameFile(a, b) {
		t.Fatal("native clone used shared inode")
	}
	if _, err := out.WriteAt([]byte("changed"), 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("baseline")) {
		t.Fatal("native copy-on-write mutation reached source")
	}
}

func TestWorkspaceCopyCachesUnsupportedFilesystem(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "clone")
	if err := os.WriteFile(src, []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if !reflinkUnsupported(tryReflink(in, out)) {
		t.Skip("filesystem supports reflink or has a transient capability failure")
	}
	sourceInfo, err := in.Stat()
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := out.Stat()
	if err != nil {
		t.Fatal(err)
	}
	capabilities := make(workspaceCopyCapabilities)
	if err := capabilities.copy(in, out, sourceInfo, targetInfo); err != nil {
		t.Fatal(err)
	}
	sourceDevice, sourceOK := workspaceCopyDevice(sourceInfo)
	targetDevice, targetOK := workspaceCopyDevice(targetInfo)
	if !sourceOK || !targetOK || !capabilities[workspaceCopyDevicePair{sourceDevice, targetDevice}] {
		t.Fatal("unsupported device pair was not retained for the next file")
	}
}

func TestWorkspaceCopyHonorsCancellationBetweenFiles(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte("baseline"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := OpenProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clone, err := p.cloneToTempContext(ctx, func(in, out *os.File) error {
		cancel()
		return ordinaryFileCopy(in, out)
	})
	if !errors.Is(err, context.Canceled) || clone != nil {
		t.Fatalf("canceled copy returned clone=%v, err=%v", clone, err)
	}
}

func TestWorkspaceReflinkModesPreserveIsolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := OpenProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"0", "auto", "1"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MAKEWAND_WORKSPACE_REFLINK", mode)
			clone, err := p.CloneToTemp()
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(clone.Path)
			file, err := os.OpenFile(filepath.Join(clone.Path, "input.txt"), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteAt([]byte("changed!"), 0)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("candidate write: %v; close: %v", writeErr, closeErr)
			}
			data, err := os.ReadFile(filepath.Join(dir, "input.txt"))
			if err != nil || string(data) != "baseline" {
				t.Fatalf("mode %s modified source: %q, %v", mode, data, err)
			}
		})
	}
	t.Setenv("MAKEWAND_WORKSPACE_REFLINK", "invalid")
	if clone, err := p.CloneToTemp(); err == nil || clone != nil {
		t.Fatal("invalid copy configuration was accepted")
	}
}

func BenchmarkWorkspaceClone(b *testing.B) {
	for _, shape := range []struct {
		name        string
		files, size int
	}{{"LargeFiles", 8, 2 << 20}, {"SmallFiles", 512, 8 << 10}} {
		b.Run(shape.name, func(b *testing.B) {
			dir := b.TempDir()
			payload := bytes.Repeat([]byte("x"), shape.size)
			for i := 0; i < shape.files; i++ {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("input-%04d.bin", i)), payload, 0600); err != nil {
					b.Fatal(err)
				}
			}
			p, err := OpenProject(dir)
			if err != nil {
				b.Fatal(err)
			}
			for _, strategy := range []struct {
				name, mode string
			}{{"Ordinary", "0"}, {"AutoReflink", "auto"}, {"ForcedReflink", "1"}} {
				b.Run(strategy.name, func(b *testing.B) {
					b.Setenv("MAKEWAND_WORKSPACE_REFLINK", strategy.mode)
					b.SetBytes(int64(shape.files * shape.size))
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						clone, err := p.CloneToTemp()
						if err != nil {
							b.Fatal(err)
						}
						b.StopTimer()
						os.RemoveAll(clone.Path)
						b.StartTimer()
					}
				})
			}
		})
	}
}
