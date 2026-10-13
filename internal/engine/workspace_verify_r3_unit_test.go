//go:build linux

package engine

import (
	"strings"
	"testing"
)

func TestLockedCargoPlan_R3(t *testing.T) {
	for _, tc := range []struct {
		args, want []string
		locked     bool
	}{
		{[]string{"fetch"}, []string{"fetch", "--locked"}, true},
		{[]string{"test", "--", "--nocapture"}, []string{"test", "--locked", "--", "--nocapture"}, true},
		{[]string{"build", "--frozen"}, []string{"build", "--frozen"}, true},
		{[]string{"check", "--tests"}, []string{"check", "--tests"}, false},
	} {
		plan := ExecPlan{Kind: "deps", Command: "cargo", Args: tc.args}
		if got := lockedCargoPlan(plan, tc.locked); !slicesEqual(got.Args, tc.want) || !slicesEqual(plan.Args, tc.args) {
			t.Errorf("lockedCargoPlan(%q, %v) = %q, want %q (input unchanged)", tc.args, tc.locked, got.Args, tc.want)
		}
	}
	if got := lockedCargoPlan(ExecPlan{Command: "npm", Args: []string{"install"}}, true); !slicesEqual(got.Args, []string{"install"}) {
		t.Errorf("npm plan changed: %q", got.Args)
	}
}

// Only a manifest certainly without dependencies skips the fetch.
func TestCargoManifestDeclaresDependencies_R3(t *testing.T) {
	pkg := "[package]\nname = \"fv\"\nversion = \"0.1.0\"\nedition = \"2021\"\ndescription = \"a C# port\"\n\n"
	for _, tc := range []struct {
		name, manifest string
		want           bool
	}{
		{"no dependency tables", pkg, false},
		{"empty dependency tables", pkg + "[dependencies]\n\n[dev-dependencies]\n# none yet\n[build-dependencies]\n", false},
		{"other tables", pkg + "[features]\ndefault = []\n\n[profile.release]\nlto = true\n\n[[bin]]\nname = \"fv\"\npath = \"src/main.rs\"\n", false},
		{"dependency", pkg + "[dependencies]\nitoa = \"1\"\n", true},
		{"dev-dependency", pkg + "[dev-dependencies]\nproptest = { version = \"1\" }\n", true},
		{"build-dependency", pkg + "[build-dependencies]\ncc = \"1\"\n", true},
		{"underscore spelling", pkg + "[dev_dependencies]\nproptest = \"1\"\n", true},
		{"target-specific", pkg + "[target.'cfg(unix)'.dependencies]\nlibc = \"0.2\"\n", true},
		{"dependency table", pkg + "[dependencies.serde]\nversion = \"1\"\n", true},
		{"target dependency table", pkg + "[target.\"x86_64-pc-windows-msvc\".dependencies.winapi]\nversion = \"0.3\"\n", true},
		{"dotted key", "dependencies.serde = \"1\"\n" + pkg, true},
		{"inline table under a target", pkg + "[target.'cfg(unix)']\ndependencies = { libc = \"0.2\" }\n", true},
		{"spaced header", pkg + "[ dependencies ]\nitoa = \"1\"\n", true},
		{"workspace", "[workspace]\nmembers = [\"a\", \"b\"]\n", true},
		{"workspace member", pkg[:len(pkg)-1] + "workspace = \"..\"\n", true},
		{"patch", pkg + "[patch.crates-io]\nitoa = { path = \"../itoa\" }\n", true},
		{"comment only mentions it", pkg + "# [dependencies]\n# itoa = \"1\"\n", false},
	} {
		if got := cargoManifestDeclaresDependencies(tc.manifest); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A stale Cargo.lock is reported as such in both of cargo's wordings; other
// failures keep their output.
func TestVerificationFailureDetailStaleCargoLockfile_R3(t *testing.T) {
	const want = "dependencies changed but the lockfile was not updated: Cargo.lock does not match Cargo.toml (cargo: "
	cargo := ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"fetch", "--locked"}}
	for _, stderr := range []string{
		"error: cannot update the lock file /ws/Cargo.lock because --locked was passed to prevent this\nhelp: to generate the lock file without accessing the network, remove the --locked flag and use --offline instead.",
		"error: the lock file /ws/Cargo.lock needs to be updated but --locked was passed to prevent this\nIf you want to try to generate the lock file without accessing the network, remove the --locked flag and use --offline instead.",
	} {
		if got := verificationFailureDetail(cargo, &ExecResult{ExitCode: 101, Stderr: stderr}); !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "); update Cargo.lock together with Cargo.toml") {
			t.Errorf("detail = %q, want the stale lockfile reported", got)
		}
	}
	other := &ExecResult{ExitCode: 101, Stderr: "error[E0425]: cannot find value `x` in this scope"}
	if got := verificationFailureDetail(cargo, other); got != other.Stderr {
		t.Errorf("detail = %q, want the output", got)
	}
	npm := &ExecResult{ExitCode: 1, Stderr: "the lock file x needs to be updated but --locked was passed to prevent this"}
	if got := verificationFailureDetail(ExecPlan{Command: "npm"}, npm); got != npm.Stderr {
		t.Errorf("npm detail = %q, want the output", got)
	}
}
