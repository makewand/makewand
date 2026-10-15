package router

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Final round 2 (go-p3): the credential lock never follows a symlink planted
// at its path, as the Python engine's _credential_lock does not; and the CLI
// message catalog holds only what the engines read, documented as they read
// it.

// A symlink at the lock path is refused, not followed: the file it points to
// is neither created nor locked, and the lock is taken unlocked, as the
// Python engine takes it.
func TestCredentialLockRefusesASymlink_Final2(t *testing.T) {
	host := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(host, []byte(`{"access_token":"a1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.lock")
	if err := os.Symlink(target, cliCredentialLockPath(host)); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	unlock, ok := cliLockCredential(host)
	if !ok {
		t.Fatal("the lock reported another write-back")
	}
	unlock()
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the lock followed the symlink to %s (%v)", target, err)
	}
	if runtime.GOOS != "windows" { // where Lstat reports a symlink as one
		if info, err := os.Lstat(cliCredentialLockPath(host)); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the planted symlink was replaced: %v %v", info, err)
		}
	}
}

// A write-back still writes the refreshed token past a symlinked lock path
// (unlocked, the identity re-check before the rename still applies), and
// never through it.
func TestWriteBackPastASymlinkedLock_Final2(t *testing.T) {
	old := `{"access_token":"a1","refresh_token":"r1"}`
	refreshed := `{"access_token":"a2","refresh_token":"r1"}`
	h, host, priv := writebackFixHome(t, "auth.json", &old)
	if err := os.WriteFile(priv, []byte(refreshed), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.lock")
	if err := os.Symlink(target, cliCredentialLockPath(host)); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if warnings := h.writeBack(); len(warnings) != 0 {
		t.Fatalf("write-back warned: %q", warnings)
	}
	if data, _ := os.ReadFile(host); string(data) != refreshed {
		t.Fatalf("host credential = %q, want the refreshed token", data)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the write-back created %s through the symlinked lock (%v)", target, err)
	}
}

// cliCatalogFile is the embedded catalog as JSON.
func cliCatalogFile(t *testing.T) map[string]any {
	t.Helper()
	var file map[string]any
	if err := json.Unmarshal(cliMessageCatalogJSON, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// Every lines entry of the catalog is read (loadCLIMessageCatalog): the
// markdown fence marks are not catalog data, both engines use ``` and ~~~.
func TestCLIMessageCatalogLinesAreAllRead_Final2(t *testing.T) {
	lines, _ := cliCatalogFile(t)["lines"].(map[string]any)
	read := []string{"caused_by", "caused_by_entry", "glog_header", "lead_in", "marked_bracket", "marker", "name_separator", "top_level", "tracing"}
	if got := slices.Sorted(maps.Keys(lines)); !slices.Equal(got, read) {
		t.Fatalf("catalog lines = %q, want exactly the entries the engines read %q", got, read)
	}
}

// The catalog's _doc describes how this engine compiles its patterns: without
// (?i), each ASCII letter spelled in both cases (cliCatalogRegexp).
func TestCLIMessageCatalogDocDescribesGoCompilation_Final2(t *testing.T) {
	doc, _ := json.Marshal(cliCatalogFile(t)["_doc"])
	var text []string
	if err := json.Unmarshal(doc, &text); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(text, "\n")
	if strings.Contains(all, "(?i)^(?:") || !strings.Contains(all, "cliCatalogRegexp") || !strings.Contains(all, "without (?i)") {
		t.Fatalf("the catalog _doc does not describe Go's compilation:\n%s", all)
	}
	if strings.Contains(all, "lines.fence") {
		t.Fatal("the catalog _doc still names lines.fence")
	}
}

// answer_streams is read: a selection where every candidate line counts
// ('lines', also where a mode has none) never reads a capture that holds the
// model's answer, and only stdout and stderr are streams.
func TestCLIMessageCatalogRefusesLinesSelectionOverAnswer_Final2(t *testing.T) {
	if _, err := loadCLIMessageCatalog(cliMessageCatalogJSON); err != nil {
		t.Fatalf("the embedded catalog does not load: %v", err)
	}
	for _, tc := range []struct {
		name     string
		provider string
		edit     func(entry map[string]any)
		want     string
	}{
		{"merged lines over an answer stream", "grok", func(e map[string]any) { e["selection"].(map[string]any)["merged"] = "lines" }, "selection.merged"},
		{"merged selection missing", "grok", func(e map[string]any) { delete(e["selection"].(map[string]any), "merged") }, "selection.merged"},
		{"apart lines over an own answer stream", "muse", func(e map[string]any) { e["selection"].(map[string]any)["apart"] = "lines" }, "selection.apart"},
		{"unknown stream", "claude", func(e map[string]any) { e["answer_streams"] = []any{"stdin"} }, "unknown stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := cliCatalogFile(t)
			tc.edit(file["providers"].(map[string]any)[tc.provider].(map[string]any))
			data, err := json.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadCLIMessageCatalog(data); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "providers."+tc.provider+".") {
				t.Fatalf("load = %v, want a refusal naming providers.%s.%s", err, tc.provider, tc.want)
			}
		})
	}
}
