package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func agyTestDoc(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("settings JSON: %v (%s)", err, data)
	}
	return doc
}

func agyTestRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func lastRoBindSource(args []string, dest string) string {
	var src string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+2] == dest {
			src = args[i+1]
		}
	}
	return src
}

func TestSanitizedAgySettingsBytesPreserveHost(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "settings.json")
	original := []byte("{\"useG1Credits\": true, \"theme\": \"dark\", \"label\": \"甲\", \"nested\": {\"n\": 2}}\n")
	if err := os.WriteFile(good, original, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := cliSanitizedAgySettingsBytes(good)
	if err != nil {
		t.Fatal(err)
	}
	doc := agyTestDoc(t, out)
	nested, _ := doc["nested"].(map[string]any)
	if doc["useG1Credits"] != false || doc["theme"] != "dark" || doc["label"] != "甲" || !agyTestNumber(nested, "n", 2) {
		t.Fatalf("sanitized = %s", out)
	}
	if !bytes.Equal(agyTestRead(t, good), original) {
		t.Fatal("host settings bytes changed")
	}

	for _, body := range []string{"", "not-json", "[]", "null", "true", "{\"a\": 1} trailing"} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := cliSanitizedAgySettingsBytes(path)
		if err != nil {
			t.Fatal(err)
		}
		doc := agyTestDoc(t, out)
		if len(doc) != 1 || doc["useG1Credits"] != false {
			t.Fatalf("%q -> %s", body, out)
		}
		if got := string(agyTestRead(t, path)); got != body {
			t.Fatalf("host bytes changed for %q: %q", body, got)
		}
	}

	missing := filepath.Join(dir, "missing.json")
	out, err = cliSanitizedAgySettingsBytes(missing)
	if err != nil {
		t.Fatal(err)
	}
	if doc := agyTestDoc(t, out); len(doc) != 1 || doc["useG1Credits"] != false {
		t.Fatalf("missing host file -> %s", out)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing host settings file was created")
	}
}

func TestAgyPrivateHomeForcesCreditsOffAndPreservesHost(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	gemini := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(filepath.Join(gemini, "antigravity-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootBody := []byte("{\"useG1Credits\": true, \"other\": 1}\n")
	cliBody := []byte("{\"useG1Credits\": true, \"theme\": \"dark\", \"model\": \"x\"}\n")
	rootPath := filepath.Join(gemini, "settings.json")
	cliPath := filepath.Join(gemini, "antigravity-cli", "settings.json")
	if err := os.WriteFile(rootPath, rootBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, cliBody, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "real.json")
	outsideBody := []byte("{\"useG1Credits\": true, \"theme\": \"light\"}\n")
	if err := os.WriteFile(outside, outsideBody, 0o600); err != nil {
		t.Fatal(err)
	}
	// A second home exercises the symlink read: the link target is the host
	// file that must stay byte-for-byte identical.
	linkHome := t.TempDir()
	linkGemini := filepath.Join(linkHome, ".gemini")
	if err := os.MkdirAll(linkGemini, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkGemini, "settings.json")); err != nil {
		t.Fatal(err)
	}

	h, err := newCLIPrivateHome(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)
	h.homeBound = true
	fi, err := os.Stat(gemini)
	if err != nil {
		t.Fatal(err)
	}
	args, err := h.addProviderEntry(nil, "agy", home, ".gemini", gemini, fi, ws)
	if err != nil {
		t.Fatal(err)
	}
	privRoot := filepath.Join(h.home, ".gemini")
	rootDoc := agyTestDoc(t, agyTestRead(t, filepath.Join(privRoot, "settings.json")))
	cliDoc := agyTestDoc(t, agyTestRead(t, filepath.Join(privRoot, "antigravity-cli", "settings.json")))
	if rootDoc["useG1Credits"] != false || !agyTestNumber(rootDoc, "other", 1) {
		t.Fatalf("root settings = %#v", rootDoc)
	}
	if cliDoc["useG1Credits"] != false || cliDoc["theme"] != "dark" || cliDoc["model"] != "x" {
		t.Fatalf("cli settings = %#v", cliDoc)
	}
	for _, rel := range cliAgySettingsRels {
		dest := filepath.Join(gemini, filepath.FromSlash(rel))
		src := filepath.Join(privRoot, filepath.FromSlash(rel))
		if got := lastRoBindSource(args, dest); got != src {
			t.Fatalf("%s last ro-bind source = %s, want %s", rel, got, src)
		}
	}
	for _, cred := range h.creds {
		if strings.HasSuffix(filepath.ToSlash(cred.hostPath), "settings.json") || strings.HasSuffix(filepath.ToSlash(cred.privPath), "settings.json") {
			t.Fatalf("settings registered for write-back: %+v", cred)
		}
	}
	if err := os.WriteFile(filepath.Join(privRoot, "settings.json"), []byte("{\"useG1Credits\": true, \"pwned\": 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privRoot, "antigravity-cli", "settings.json"), []byte("{\"useG1Credits\": true, \"pwned\": 2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeBack()
	if got := agyTestRead(t, rootPath); !bytes.Equal(got, rootBody) {
		t.Fatalf("host root settings changed: %q", got)
	}
	if got := agyTestRead(t, cliPath); !bytes.Equal(got, cliBody) {
		t.Fatalf("host cli settings changed: %q", got)
	}

	linkFI, err := os.Stat(linkGemini)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.addProviderEntry(nil, "gemini", linkHome, ".gemini", linkGemini, linkFI, ws); err != nil {
		t.Fatal(err)
	}
	if got := agyTestRead(t, outside); !bytes.Equal(got, outsideBody) {
		t.Fatalf("symlink target changed: %q", got)
	}
	linked := agyTestDoc(t, agyTestRead(t, filepath.Join(h.home, ".gemini", "settings.json")))
	if linked["useG1Credits"] != false || linked["theme"] != "light" {
		t.Fatalf("symlink settings = %#v", linked)
	}
}

func TestAgyPrivateHomeWithoutHostSettingsStillDisablesCredits(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	gemini := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(gemini, 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := newCLIPrivateHome(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)
	h.homeBound = true
	fi, err := os.Stat(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.addProviderEntry(nil, "antigravity", home, ".gemini", gemini, fi, ws); err != nil {
		t.Fatal(err)
	}
	for _, rel := range cliAgySettingsRels {
		doc := agyTestDoc(t, agyTestRead(t, filepath.Join(h.home, ".gemini", filepath.FromSlash(rel))))
		if len(doc) != 1 || doc["useG1Credits"] != false {
			t.Fatalf("%s = %#v", rel, doc)
		}
		if _, err := os.Stat(filepath.Join(gemini, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("host %s was created", rel)
		}
	}

	// No ~/.gemini at all. Writable sessions still present the minimal document
	// inside the private home and do not create the host directory.
	home2 := t.TempDir()
	absent := filepath.Join(home2, ".gemini")
	h2, err := newCLIPrivateHome(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.cleanup)
	h2.homeBound = true
	if _, err := h2.addAbsentProviderEntry(nil, "agy", ".gemini", absent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("absent host .gemini was created")
	}
	doc := agyTestDoc(t, agyTestRead(t, filepath.Join(h2.home, ".gemini", "antigravity-cli", "settings.json")))
	if doc["useG1Credits"] != false {
		t.Fatalf("absent root settings = %#v", doc)
	}

	// A read-only session that cannot mount the missing directory refuses
	// instead of launching without useG1Credits=false.
	h3, err := newCLIPrivateHome(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h3.cleanup)
	if _, err := h3.addAbsentProviderEntry(nil, "agy", ".gemini", absent); err == nil || !strings.Contains(err.Error(), "cannot mount sanitized agy settings") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("refused launch created host .gemini")
	}

	h4, err := newCLIPrivateHome(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h4.cleanup)
	mounted, err := h4.addAbsentProviderEntry([]string{"--tmpfs", home2}, "agy", ".gemini", absent)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h4.home, ".gemini", "settings.json")
	if got := lastRoBindSource(mounted, filepath.Join(absent, "settings.json")); got != want {
		t.Fatalf("read-only absent root ro-bind = %s, want %s", got, want)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("read-only plan created host .gemini")
	}
}

func TestAgySettingsWriteErrorRefusesStart(t *testing.T) {
	old := cliWritePrivateNoFollowFn
	cliWritePrivateNoFollowFn = func(string, []byte) error { return errors.New("disk full") }
	t.Cleanup(func() { cliWritePrivateNoFollowFn = old })

	ws := t.TempDir()
	home := t.TempDir()
	gemini := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(filepath.Join(gemini, "antigravity-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootBody := []byte("{\"useG1Credits\": true, \"theme\": \"dark\"}\n")
	cliBody := []byte("{\"useG1Credits\": true, \"model\": \"x\"}\n")
	rootPath := filepath.Join(gemini, "settings.json")
	cliPath := filepath.Join(gemini, "antigravity-cli", "settings.json")
	if err := os.WriteFile(rootPath, rootBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, cliBody, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := newCLIPrivateHome(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)
	h.homeBound = true
	fi, err := os.Stat(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.addProviderEntry(nil, "agy", home, ".gemini", gemini, fi, ws); err == nil || !strings.Contains(err.Error(), "cannot write private agy settings") {
		t.Fatalf("err = %v", err)
	}
	if got := agyTestRead(t, rootPath); !bytes.Equal(got, rootBody) {
		t.Fatalf("host root settings changed: %q", got)
	}
	if got := agyTestRead(t, cliPath); !bytes.Equal(got, cliBody) {
		t.Fatalf("host cli settings changed: %q", got)
	}

	if runtime.GOOS != "linux" {
		return
	}
	auditFixMockBwrap(t)
	cmd := exec.Command("/usr/bin/true")
	cmd.Dir = ws
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	if _, err := wrapCLICommandWithSandbox(context.Background(), "agy", cmd); err == nil || !strings.Contains(err.Error(), "cannot write private agy settings") {
		t.Fatalf("sandbox start err = %v", err)
	}
	if got := agyTestRead(t, rootPath); !bytes.Equal(got, rootBody) {
		t.Fatalf("host root settings changed after refused start: %q", got)
	}
	if got := agyTestRead(t, cliPath); !bytes.Equal(got, cliBody) {
		t.Fatalf("host cli settings changed after refused start: %q", got)
	}
}

func TestAgySandboxPlanReadsSanitizedSettings(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private provider homes are part of the Linux bubblewrap mount plan")
	}
	auditFixMockBwrap(t)
	for _, provider := range []string{"agy", "gemini", "antigravity"} {
		for _, readOnly := range []bool{false, true} {
			name := provider
			if readOnly {
				name += "-readonly"
			}
			t.Run(name, func(t *testing.T) {
				home, ws := canonicalTempDir(t), canonicalTempDir(t)
				gemini := filepath.Join(home, ".gemini")
				if err := os.MkdirAll(filepath.Join(gemini, "antigravity-cli"), 0o700); err != nil {
					t.Fatal(err)
				}
				rootBody := []byte("{\"useG1Credits\": true, \"other\": 1, \"label\": \"甲\"}\n")
				cliBody := []byte("{\"useG1Credits\": true, \"theme\": \"dark\", \"model\": \"x\"}\n")
				rootPath := filepath.Join(gemini, "settings.json")
				cliPath := filepath.Join(gemini, "antigravity-cli", "settings.json")
				if err := os.WriteFile(rootPath, rootBody, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cliPath, cliBody, 0o600); err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if readOnly {
					ctx = ContextWithTask(ctx, TaskReview)
				}
				cmd := exec.Command("/usr/bin/true")
				cmd.Dir = ws
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
				wrapped, err := wrapCLICommandWithSandbox(ctx, provider, cmd)
				if err != nil {
					t.Fatal(err)
				}
				var privRoot string
				if readOnly {
					privRoot = cliPrivateHomeSource(t, wrapped.Args, gemini)
				} else {
					privRoot = filepath.Join(cliPrivateHomeSource(t, wrapped.Args, home), ".gemini")
				}
				rootDoc := agyTestDoc(t, agyTestRead(t, filepath.Join(privRoot, "settings.json")))
				cliDoc := agyTestDoc(t, agyTestRead(t, filepath.Join(privRoot, "antigravity-cli", "settings.json")))
				if rootDoc["useG1Credits"] != false || rootDoc["label"] != "甲" {
					t.Fatalf("root = %#v", rootDoc)
				}
				if cliDoc["useG1Credits"] != false || cliDoc["theme"] != "dark" || cliDoc["model"] != "x" {
					t.Fatalf("cli = %#v", cliDoc)
				}
				for _, rel := range []string{"settings.json", "antigravity-cli/settings.json"} {
					dest := filepath.Join(gemini, filepath.FromSlash(rel))
					src := filepath.Join(privRoot, filepath.FromSlash(rel))
					if got := lastRoBindSource(wrapped.Args, dest); got != src {
						t.Fatalf("%s last ro-bind = %s, want %s", rel, got, src)
					}
				}
				if got := agyTestRead(t, rootPath); !bytes.Equal(got, rootBody) {
					t.Fatalf("host root changed: %q", got)
				}
				if got := agyTestRead(t, cliPath); !bytes.Equal(got, cliBody) {
					t.Fatalf("host cli changed: %q", got)
				}
			})
		}
	}
}

func TestAgyLiveSandboxReadsCreditsOff(t *testing.T) {
	auditFixRequireLiveBwrap(t)
	home, ws := canonicalTempDir(t), canonicalTempDir(t)
	gemini := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(filepath.Join(gemini, "antigravity-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootBody := []byte("{\"useG1Credits\": true, \"other\": 1}\n")
	cliBody := []byte("{\"useG1Credits\": true, \"theme\": \"dark\", \"model\": \"x\"}\n")
	rootPath := filepath.Join(gemini, "settings.json")
	cliPath := filepath.Join(gemini, "antigravity-cli", "settings.json")
	if err := os.WriteFile(rootPath, rootBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliPath, cliBody, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `printf 'ROOT_BEGIN\n'
cat "$HOME/.gemini/settings.json"
printf '\nROOT_END\n'
printf 'CLI_BEGIN\n'
cat "$HOME/.gemini/antigravity-cli/settings.json"
printf '\nCLI_END\n'
if printf x > "$HOME/.gemini/settings.json" 2>/dev/null; then echo ROOT_WRITABLE; fi
if printf x > "$HOME/.gemini/antigravity-cli/settings.json" 2>/dev/null; then echo CLI_WRITABLE; fi
echo DONE`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = ws
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
	wrapped, err := wrapCLICommandWithSandbox(context.Background(), "agy", cmd)
	if err != nil {
		t.Fatal(err)
	}
	out := auditFixRunAndWaitPrivateHome(t, wrapped)
	if !strings.Contains(out, "DONE") || strings.Contains(out, "WRITABLE") {
		t.Fatalf("sandbox output: %q", out)
	}
	rootDoc := agyTestDoc(t, []byte(sectionBetween(t, out, "ROOT_BEGIN\n", "\nROOT_END\n")))
	cliDoc := agyTestDoc(t, []byte(sectionBetween(t, out, "CLI_BEGIN\n", "\nCLI_END\n")))
	if rootDoc["useG1Credits"] != false || !agyTestNumber(rootDoc, "other", 1) {
		t.Fatalf("sandbox root settings = %#v", rootDoc)
	}
	if cliDoc["useG1Credits"] != false || cliDoc["theme"] != "dark" || cliDoc["model"] != "x" {
		t.Fatalf("sandbox cli settings = %#v", cliDoc)
	}
	if got := agyTestRead(t, rootPath); !bytes.Equal(got, rootBody) {
		t.Fatalf("host root settings changed: %q", got)
	}
	if got := agyTestRead(t, cliPath); !bytes.Equal(got, cliBody) {
		t.Fatalf("host cli settings changed: %q", got)
	}
}

func agyTestNumber(doc map[string]any, key string, want float64) bool {
	if doc == nil {
		return false
	}
	switch n := doc[key].(type) {
	case float64:
		return n == want
	case json.Number:
		f, err := n.Float64()
		return err == nil && f == want
	default:
		return false
	}
}

func sectionBetween(t *testing.T, text, start, end string) string {
	t.Helper()
	i := strings.Index(text, start)
	if i < 0 {
		t.Fatalf("missing %q in %q", start, text)
	}
	rest := text[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("missing %q in %q", end, text)
	}
	return rest[:j]
}

func agyWriteHostSettings(t *testing.T, home string, bodies [2]string) {
	t.Helper()
	for i, rel := range cliAgySettingsRels {
		if bodies[i] == "-" {
			continue // absent
		}
		path := filepath.Join(home, ".gemini", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(bodies[i]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Without bubblewrap agy reads the host settings, so an authorized host
// launch is allowed only when both files already prove useG1Credits=false.
func TestAgyHostExecWithoutBwrapNeedsCreditsOff(t *testing.T) {
	auditFixMissingBwrap(t)
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	auth := UnsafeHostExecAuth{Acknowledged: true, Source: "test", Audit: func(string, []string, string) {}}
	ctx := ContextWithUnsafeHostExecAuth(context.Background(), auth)
	off := `{"useG1Credits": false, "theme": "dark"}`
	cases := []struct {
		name     string
		provider string
		bodies   [2]string
		ok       bool
	}{
		{"both off", "agy", [2]string{off, off}, true},
		{"cli file on", "agy", [2]string{off, `{"useG1Credits": true}`}, false},
		{"key missing", "agy", [2]string{off, `{"theme": "dark"}`}, false},
		{"string false", "agy", [2]string{off, `{"useG1Credits": "false"}`}, false},
		{"not an object", "agy", [2]string{"[]", off}, false},
		{"cli file absent", "agy", [2]string{off, "-"}, false},
		{"gemini alias", "gemini", [2]string{"-", "-"}, false},
		{"antigravity alias", "antigravity", [2]string{`{"useG1Credits": true}`, off}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			agyWriteHostSettings(t, home, tc.bodies)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
			cmd := exec.Command("agy", "-p", "x")
			cmd.Env = append(os.Environ(), "HOME="+home)
			wrapped, err := wrapCLICommandWithSandbox(ctx, tc.provider, cmd)
			if tc.ok {
				if err != nil || wrapped != cmd {
					t.Fatalf("credits proven off but launch refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "useG1Credits") {
				t.Fatalf("host launch not refused with a useG1Credits reason: %v", err)
			}
			for i, rel := range cliAgySettingsRels {
				path := filepath.Join(home, ".gemini", filepath.FromSlash(rel))
				data, readErr := os.ReadFile(path)
				if tc.bodies[i] == "-" {
					if readErr == nil {
						t.Fatalf("%s was created on the host", path)
					}
				} else if string(data) != tc.bodies[i] {
					t.Fatalf("host settings changed: %s = %q", path, data)
				}
			}
		})
	}
	// Other providers keep the existing host-exec behaviour.
	t.Setenv("HOME", t.TempDir())
	if wrapped, err := wrapCLICommandWithSandbox(ctx, "claude", exec.Command("claude", "-p", "x")); err != nil || wrapped == nil {
		t.Fatalf("claude host launch changed: %v", err)
	}
}

// With bubblewrap but no usable HOME there is no private home to hold the
// sanitized settings, so agy must not start.
func TestAgySandboxRefusedWithoutMountedSettings(t *testing.T) {
	auditFixMockBwrap(t)
	workspace := t.TempDir()
	for _, home := range []string{"/", ""} {
		cmd := exec.Command("agy", "-p", "x")
		cmd.Dir = workspace
		env := []string{"PATH=" + os.Getenv("PATH")}
		if home != "" {
			env = append(env, "HOME="+home)
		}
		cmd.Env = env
		_, err := wrapCLICommandWithSandbox(context.Background(), "agy", cmd)
		if err == nil || !strings.Contains(err.Error(), "useG1Credits=false") {
			t.Fatalf("HOME=%q: agy started without sanitized settings: %v", home, err)
		}
	}
}

// Config-dir variables could point agy away from the ~/.gemini settings the
// credit guards check, so agy launches drop them and keep other keys.
func TestAgyLaunchDropsConfigDirEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "HOME=/home/u", "GEMINI_API_KEY=k", "GOOGLE_CLOUD_PROJECT=p",
		"GEMINI_CONFIG_DIR=/home/u/alt", "AGY_CONFIG_DIR=/home/u/alt", "antigravity_config_dir=/home/u/alt",
	}
	for _, provider := range []string{"agy", "gemini", "antigravity", "agy-cli"} {
		got := strings.Join(sanitizeCLIEnv(provider, env), "\n")
		for _, dropped := range []string{"GEMINI_CONFIG_DIR=", "AGY_CONFIG_DIR=", "antigravity_config_dir="} {
			if strings.Contains(got, dropped) {
				t.Fatalf("%s kept %s: %q", provider, dropped, got)
			}
		}
		for _, kept := range []string{"GEMINI_API_KEY=k", "GOOGLE_CLOUD_PROJECT=p", "HOME=/home/u"} {
			if !strings.Contains(got, kept) {
				t.Fatalf("%s dropped %s: %q", provider, kept, got)
			}
		}
	}
}

// A custom provider wrapping the agy binary under another name gets the
// same host guard; other binaries under that name are unaffected.
func TestAgyHostGuardRecognizesAgyBinaryUnderCustomName(t *testing.T) {
	auditFixMissingBwrap(t)
	t.Setenv("MAKEWAND_UNSAFE_HOST_EXEC", "1")
	auth := UnsafeHostExecAuth{Acknowledged: true, Source: "test", Audit: func(string, []string, string) {}}
	ctx := ContextWithUnsafeHostExecAuth(context.Background(), auth)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, bin := range []string{"agy", "/opt/bin/agy", "agy.exe", `C:\Tools\Antigravity.EXE`, "gemini-cli"} {
		cmd := exec.Command(bin, "-p", "x")
		cmd.Args[0] = bin
		if _, err := wrapCLICommandWithSandbox(ctx, "myagent", cmd); err == nil || !strings.Contains(err.Error(), "useG1Credits") {
			t.Fatalf("%s under a custom name ran on the host without the credits guard: %v", bin, err)
		}
	}
	for _, bin := range []string{"my-agent", "/usr/bin/agyfoo", "geminiish"} {
		cmd := exec.Command(bin, "-p", "x")
		cmd.Args[0] = bin
		if _, err := wrapCLICommandWithSandbox(ctx, "myagent", cmd); err != nil {
			t.Fatalf("%s is not agy but was refused: %v", bin, err)
		}
	}
}

// A non-absolute HOME (Git Bash on Windows, or a relative value) is skipped;
// the guard then reads the real user home directory.
func TestAgyHostGuardSkipsNonAbsoluteHome(t *testing.T) {
	home := t.TempDir()
	agyWriteHostSettings(t, home, [2]string{`{"useG1Credits": false}`, `{"useG1Credits": false}`})
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, childHome := range []string{"relative/home", "c/Users/u"} {
		if err := cliAgyHostCreditsDisabled([]string{"HOME=" + childHome}); err != nil {
			t.Fatalf("HOME=%q: %v", childHome, err)
		}
	}
	other := t.TempDir()
	if err := cliAgyHostCreditsDisabled([]string{"HOME=" + other}); err == nil || !strings.Contains(err.Error(), other) {
		t.Fatalf("an absolute child HOME without settings must still be checked: %v", err)
	}
}
