//go:build linux

package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GEC-1021-1 round 3 (review of the round-2 fix): the sandbox CARGO_HOME is
// the networked fetch's writable cache, but its cargo config files are not
// cache. makewand never writes them, a loopback step running project code can,
// and a credential provider they name still runs under cargoFetchEnv (verified
// on cargo 1.93). The fetch shadows those files and nothing else in the
// sandbox HOME.
func TestNetworkedFetchMasksSandboxCargoConfig_GEC1021R3(t *testing.T) {
	sandboxHomeMasks := func(t *testing.T, ws string) map[string]string {
		t.Helper()
		masks, err := sandboxWorkspaceSecretMasks(ws)
		if err != nil {
			t.Fatal(err)
		}
		home := filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath))
		out := map[string]string{}
		for target, source := range secretMaskTargets(masks) {
			if strings.HasPrefix(target, home+string(filepath.Separator)) {
				out[target] = source
			}
		}
		return out
	}

	t.Run("config files are shadowed, caches are not", func(t *testing.T) {
		ws := t.TempDir()
		cargoHome := filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath), ".cargo")
		writeFile(t, filepath.Join(cargoHome, "config.toml"), "[net]\nretry = 2\n")
		writeFile(t, filepath.Join(cargoHome, "config"), "[net]\nretry = 2\n")
		writeFile(t, filepath.Join(cargoHome, "registry", "cache", "x-1.0.0.crate"), "cache")

		// The masks must cover the CARGO_HOME the fetch actually uses.
		if env := cargoFetchEnv(ws); !slices.Contains(env, "CARGO_HOME="+cargoHome) {
			t.Fatalf("cargo fetch env = %v, want CARGO_HOME=%s", env, cargoHome)
		}
		masked := sandboxHomeMasks(t, ws)
		for _, file := range []string{filepath.Join(cargoHome, "config.toml"), filepath.Join(cargoHome, "config")} {
			if masked[file] != os.DevNull {
				t.Errorf("%s masked with %q, want %s", file, masked[file], os.DevNull)
			}
		}
		if len(masked) != 2 {
			t.Errorf("sandbox HOME masks = %v, want only the two CARGO_HOME config files", masked)
		}
	})

	t.Run("no config files, no mount points", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath), ".cargo", "registry", "cache", "x-1.0.0.crate"), "cache")
		if masked := sandboxHomeMasks(t, ws); len(masked) != 0 {
			t.Fatalf("sandbox HOME masks = %v, want none (bwrap must not create mount points)", masked)
		}
	})

	t.Run("a non-directory .cargo holds no config", func(t *testing.T) {
		ws := t.TempDir()
		writeFile(t, filepath.Join(ws, filepath.FromSlash(sandboxHomeRelPath), ".cargo"), "not a directory")
		if masked := sandboxHomeMasks(t, ws); len(masked) != 0 {
			t.Fatalf("sandbox HOME masks = %v, want none", masked)
		}
	})
}

// secretMaskTargets maps each path a mask list shadows to what shadows it:
// "tmpfs" for a directory, the bound source (os.DevNull) for a file.
func secretMaskTargets(masks []string) map[string]string {
	masked := map[string]string{}
	for i := 0; i < len(masks); {
		switch {
		case masks[i] == "--tmpfs" && i+1 < len(masks):
			masked[masks[i+1]] = "tmpfs"
			i += 2
		case masks[i] == "--ro-bind" && i+2 < len(masks):
			masked[masks[i+2]] = masks[i+1]
			i += 3
		default:
			i++
		}
	}
	return masked
}
