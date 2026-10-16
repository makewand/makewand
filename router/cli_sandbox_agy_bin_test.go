package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// agyBinRunAndWait runs a sandboxed agy command and waits for its private home
// cleanup. Writable runs bind the private HOME itself; review runs keep a
// tmpfs HOME and bind the private .gemini (run-*/home/.gemini) below it.
func agyBinRunAndWait(t *testing.T, wrapped *exec.Cmd, review bool) string {
	t.Helper()
	if !review {
		return auditFixRunAndWaitPrivateHome(t, wrapped)
	}
	home := filepath.Clean(cliEnvValue(wrapped.Env, "HOME"))
	dir := filepath.Dir(filepath.Dir(cliPrivateHomeSource(t, wrapped.Args, filepath.Join(home, ".gemini"))))
	done := make(chan struct{}, 1)
	hook := func(finished string) {
		if finished == dir {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}
	old := cliPrivateHomeFinished.Swap(&hook)
	t.Cleanup(func() { cliPrivateHomeFinished.Store(old) })
	out, err := runWrappedCLIForTest(wrapped)
	if err != nil {
		t.Fatalf("sandbox run failed: %v %s (args=%v)", err, out, wrapped.Args)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("private home cleanup did not run after the sandbox exited")
	}
	return string(out)
}

// agy writes a helper script to ~/.gemini/antigravity-cli/bin/agentapi before
// every run_command. That directory used to be mounted read-only, so every
// sandboxed agy command failed. It is now a tmpfs seeded with the shipped
// host files (read-only, agentapi excluded); the tmpfs dies with the sandbox.

func agyBinHasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func agyBinSeed(t *testing.T, home string) string {
	t.Helper()
	bin := filepath.Join(home, ".gemini", "antigravity-cli", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	webm := []byte("WEBM\x00ENCODER\xff")
	// #nosec G306 -- fixture must keep its executable bit
	if err := os.WriteFile(filepath.Join(bin, "webm_encoder"), webm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "helper.sh"), []byte("#!/bin/sh\necho hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G306 -- fixture must keep its executable bit
	if err := os.WriteFile(filepath.Join(bin, "agentapi"), []byte("stale host copy\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("webm_encoder", filepath.Join(bin, "link-to-encoder")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "subdir", "inner"), []byte("inner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	builtin := filepath.Join(home, ".gemini", "antigravity-cli", "builtin")
	if err := os.MkdirAll(builtin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(builtin, "tool"), []byte("builtin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin
}

func agyBinWrap(t *testing.T, provider, ws string, review bool) *exec.Cmd {
	t.Helper()
	ctx := context.Background()
	if review {
		ctx = ContextWithTask(ctx, TaskReview)
	}
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = ws
	wrapped, err := wrapCLICommandWithSandbox(ctx, provider, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}

func TestAgyBinOverlayMountPlan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	withoutCLITestHooks(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	bin := agyBinSeed(t, home)
	webm := filepath.Join(bin, "webm_encoder")
	helper := filepath.Join(bin, "helper.sh")
	agentapi := filepath.Join(bin, "agentapi")

	for _, review := range []bool{false, true} {
		wrapped := agyBinWrap(t, "agy", ws, review)
		args := wrapped.Args
		if !agyBinHasPair(args, "--tmpfs", bin) {
			t.Fatalf("review=%v: bin %s has no tmpfs: %v", review, bin, args)
		}
		if !auditFixHasTriple(args, "--ro-bind", webm, webm) {
			t.Fatalf("review=%v: webm_encoder not bound read-only: %v", review, args)
		}
		if !auditFixHasTriple(args, "--ro-bind", helper, helper) {
			t.Fatalf("review=%v: helper.sh not bound read-only: %v", review, args)
		}
		for i := 0; i+2 < len(args); i++ {
			if args[i] == "--ro-bind" && (args[i+1] == agentapi || args[i+2] == agentapi) {
				t.Fatalf("review=%v: agentapi mounted from the host: %v", review, args)
			}
			if args[i] == "--ro-bind" && (args[i+1] == bin || args[i+2] == bin) {
				t.Fatalf("review=%v: whole bin directory bound (read-only): %v", review, args)
			}
			for _, p := range []string{filepath.Join(bin, "link-to-encoder"), filepath.Join(bin, "subdir")} {
				if args[i] == "--ro-bind" && (args[i+1] == p || args[i+2] == p) {
					t.Fatalf("review=%v: non-regular bin entry %s bound: %v", review, p, args)
				}
			}
		}
		// Other protected paths stay read-only without a tmpfs.
		builtin := filepath.Join(home, ".gemini", "antigravity-cli", "builtin")
		if !auditFixHasTriple(args, "--ro-bind", builtin, builtin) {
			t.Fatalf("review=%v: builtin not bound read-only: %v", review, args)
		}
		if agyBinHasPair(args, "--tmpfs", builtin) {
			t.Fatalf("review=%v: builtin has a tmpfs: %v", review, args)
		}
		// The sanitized settings are still what agy reads.
		hostSettings := filepath.Join(home, ".gemini", "settings.json")
		src := lastRoBindSource(args, hostSettings)
		if src == "" || src == hostSettings {
			t.Fatalf("review=%v: sanitized settings not shadowing %s: %v", review, hostSettings, args)
		}
	}

	// Other providers are unaffected.
	grokBin := filepath.Join(home, ".grok", "bin")
	if err := os.MkdirAll(grokBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grokBin, "grok"), []byte("tool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapped := agyBinWrap(t, "grok", ws, false)
	if !auditFixHasTriple(wrapped.Args, "--ro-bind", grokBin, grokBin) {
		t.Fatalf("grok bin not bound read-only: %v", wrapped.Args)
	}
	if agyBinHasPair(wrapped.Args, "--tmpfs", grokBin) {
		t.Fatalf("grok bin has a tmpfs: %v", wrapped.Args)
	}
}

func TestAgyBinOverlayMissingBin(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	withoutCLITestHooks(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	gemini := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(gemini, 0o700); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	bin := filepath.Join(gemini, "antigravity-cli", "bin")
	for _, review := range []bool{false, true} {
		wrapped := agyBinWrap(t, "agy", ws, review)
		if !agyBinHasPair(wrapped.Args, "--tmpfs", bin) {
			t.Fatalf("review=%v: missing bin has no empty tmpfs: %v", review, wrapped.Args)
		}
		for i := 0; i+2 < len(wrapped.Args); i++ {
			if wrapped.Args[i] == "--ro-bind" &&
				(wrapped.Args[i+1] == bin || wrapped.Args[i+2] == bin ||
					strings.HasPrefix(wrapped.Args[i+1], bin+string(os.PathSeparator)) ||
					strings.HasPrefix(wrapped.Args[i+2], bin+string(os.PathSeparator))) {
				t.Fatalf("review=%v: missing bin seeded from the host: %v", review, wrapped.Args)
			}
		}
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Fatal("missing host bin was created")
	}
}

func TestAgyBinOverlayAbsentGemini(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	withoutCLITestHooks(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	absent := filepath.Join(home, ".gemini")
	bin := filepath.Join(absent, "antigravity-cli", "bin")
	for _, review := range []bool{false, true} {
		wrapped := agyBinWrap(t, "agy", ws, review)
		if !agyBinHasPair(wrapped.Args, "--tmpfs", bin) {
			t.Fatalf("review=%v: absent root has no empty bin tmpfs: %v", review, wrapped.Args)
		}
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("absent host .gemini was created")
	}
}

func TestAgyBinOverlayLive(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	withoutCLITestHooks(t)
	for _, review := range []bool{false, true} {
		home, ws := t.TempDir(), t.TempDir()
		bin := agyBinSeed(t, home)
		webmPath := filepath.Join(bin, "webm_encoder")
		webmBefore, err := os.ReadFile(webmPath)
		if err != nil {
			t.Fatal(err)
		}
		webmFI, err := os.Stat(webmPath)
		if err != nil {
			t.Fatal(err)
		}
		listingBefore := agyBinListing(t, bin)
		script := `set -u; BIN="$HOME/.gemini/antigravity-cli/bin"; ` +
			`printf '#!/bin/sh\nexec "agy" agentapi "$@"\n' > "$BIN/agentapi"; ` +
			`chmod +x "$BIN/agentapi"; ` +
			`echo "AGENTAPI:"; cat "$BIN/agentapi"; ` +
			`echo "WEBM:"; cat "$BIN/webm_encoder"; ` +
			`echo DONE`
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Dir = ws
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
		ctx := context.Background()
		if review {
			ctx = ContextWithTask(ctx, TaskReview)
		}
		wrapped, err := wrapCLICommandWithSandbox(ctx, "agy", cmd)
		if err != nil {
			t.Fatal(err)
		}
		out := agyBinRunAndWait(t, wrapped, review)
		if !strings.Contains(string(out), "DONE") {
			t.Fatalf("review=%v: sandbox run failed: %q", review, out)
		}
		if !strings.Contains(string(out), `exec "agy" agentapi`) {
			t.Fatalf("review=%v: agentapi not readable in the sandbox: %q", review, out)
		}
		if !strings.Contains(string(out), "WEBM:\nWEBM\x00ENCODER\xff") {
			t.Fatalf("review=%v: shipped bin file not readable in the sandbox: %q", review, out)
		}
		if got, err := os.ReadFile(webmPath); err != nil || string(got) != string(webmBefore) {
			t.Fatalf("review=%v: host webm_encoder changed: %q %v", review, got, err)
		}
		if fi, err := os.Stat(webmPath); err != nil || fi.Mode().Perm() != webmFI.Mode().Perm() {
			t.Fatalf("review=%v: host webm_encoder mode changed: %v %v", review, fi, err)
		}
		if got, err := os.ReadFile(filepath.Join(bin, "agentapi")); err != nil || string(got) != "stale host copy\n" {
			t.Fatalf("review=%v: host agentapi changed: %q %v", review, got, err)
		}
		if listing := agyBinListing(t, bin); strings.Join(listing, "\x00") != strings.Join(listingBefore, "\x00") {
			t.Fatalf("review=%v: host bin listing changed: %v", review, listing)
		}
	}
}

func agyBinListing(t *testing.T, bin string) []string {
	t.Helper()
	entries, err := os.ReadDir(bin)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
