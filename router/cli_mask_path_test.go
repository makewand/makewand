package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCredentialMaskRejectsNonAllowlistedEntriesBeforeLookup(t *testing.T) {
	for _, entry := range []string{"../account", "/etc/passwd", "unknown", ".ssh/nested", "."} {
		if _, err := cliHomeMaskTarget(filepath.Join(t.TempDir(), "missing"), entry); err == nil || !strings.Contains(err.Error(), "invalid credential mask entry") {
			t.Fatalf("non-allowlisted entry %q reached lookup: %v", entry, err)
		}
	}
}

func TestCredentialMaskResolvesFileDirectoryAndParentAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs Windows privileges")
	}
	root := t.TempDir()
	home, workspace := filepath.Join(root, "home"), filepath.Join(root, "work")
	outside, file := filepath.Join(root, "outside"), filepath.Join(root, "token")
	for _, dir := range []string{home, workspace, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{outside, ".ssh"}, {file, ".netrc"}, {outside, ".local"}} {
		if err := os.Symlink(pair[0], filepath.Join(home, pair[1])); err != nil {
			t.Fatal(err)
		}
	}
	keyrings := filepath.Join(outside, "share", "keyrings")
	if err := os.MkdirAll(keyrings, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ entry, target, flag string }{{".ssh", outside, "--tmpfs"}, {".netrc", file, "--ro-bind"}, {".local/share/keyrings", keyrings, "--tmpfs"}} {
		authorized, err := cliHomeMaskTarget(home, tc.entry)
		if err != nil {
			t.Fatal(err)
		}
		args, err := maskSensitivePath(nil, authorized, workspace)
		if err != nil || len(args) == 0 || args[0] != tc.flag || args[len(args)-1] != tc.target {
			t.Fatalf("%s canonical mask = %v, %v", tc.entry, args, err)
		}
	}
	t.Run("literal_backslash_in_Unix_alias", func(t *testing.T) {
		aliasHome, account := t.TempDir(), filepath.Join(t.TempDir(), `literal\account`)
		if err := os.Mkdir(account, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(account, filepath.Join(aliasHome, ".ssh")); err != nil {
			t.Fatal(err)
		}
		authorized, err := cliHomeMaskTarget(aliasHome, ".ssh")
		if err != nil {
			t.Fatal(err)
		}
		args, err := maskSensitivePath(nil, authorized, workspace)
		if err != nil || len(args) != 2 || args[1] != account {
			t.Fatalf("literal Unix alias mask = %v, %v", args, err)
		}
	})
}

func TestCredentialMaskResolvesParentNavigationPhysically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs Windows privileges")
	}
	root := t.TempDir()
	home, a, b := filepath.Join(root, "home"), filepath.Join(root, "home", "a"), filepath.Join(root, "b")
	for _, dir := range []string{a, filepath.Join(a, "secret"), filepath.Join(b, "dir"), filepath.Join(b, "secret")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(b, "dir"), filepath.Join(a, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/link/../secret", filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	authorized, err := cliHomeMaskTarget(home, ".ssh")
	if err != nil {
		t.Fatal(err)
	}
	args, err := maskSensitivePath(nil, authorized, filepath.Join(root, "work"))
	if err != nil || len(args) != 2 || args[0] != "--tmpfs" || args[1] != filepath.Join(b, "secret") {
		t.Fatalf("physical alias mask = %v, %v", args, err)
	}
}

func TestCredentialMaskMissingPathsAndCycles(t *testing.T) {
	if runtime.GOOS == "windows" {
		for _, tc := range []struct {
			link string
			want bool
		}{
			{`C:\account`, true}, {`account\nested`, true}, {`..\account`, true},
			{`\account`, false}, {`/account`, false}, {`C:account`, false},
		} {
			if got := cliMaskLinkIsUnambiguous(tc.link); got != tc.want {
				t.Fatalf("Windows alias %q unambiguous = %v, want %v", tc.link, got, tc.want)
			}
		}
	}
	home := t.TempDir()
	authorized, err := cliHomeMaskTarget(home, ".ssh")
	if err != nil {
		t.Fatal(err)
	}
	if args, err := maskSensitivePath([]string{"unchanged"}, authorized, t.TempDir()); err != nil || strings.Join(args, " ") != "unchanged" {
		t.Fatalf("missing entry = %v, %v", args, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(".ssh", filepath.Join(home, ".ssh")); err != nil {
			t.Fatal(err)
		}
		if _, err := maskSensitivePath(nil, authorized, t.TempDir()); err == nil {
			t.Fatal("cyclic credential alias was silently left readable")
		}
	}
}

func TestCredentialMaskUnavailableParentFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires non-root Unix directory permissions")
	}
	home, workspace := t.TempDir(), t.TempDir()
	blocked := filepath.Join(home, ".local")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	old := cliBwrapLookup
	cliBwrapLookup = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	t.Cleanup(func() { cliBwrapLookup = old })
	cmd := exec.Command("claude", "--version")
	cmd.Dir, cmd.Env = workspace, []string{"HOME=" + home}
	if _, err := wrapCLICommandWithSandbox(context.Background(), "claude", cmd); err == nil || !strings.Contains(err.Error(), "cannot be safely inspected") {
		t.Fatalf("inaccessible credential parent did not fail closed: %v", err)
	}
}
