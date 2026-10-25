//go:build linux

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewR14_UnrecognizedDepsCommandRefusedBeforeNetwork(t *testing.T) {
	installFakeUserNet(t, fakeUserNetOptions{bwrapTail: "exit 0"})
	project, err := NewProject("r14-unrecognized-deps", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	plan := ExecPlan{
		Kind:    "deps",
		Command: "python3",
		Args:    []string{"-c", "print('inline code must not run with network')"},
	}
	if reason := project.networkFetchUnavailable(plan); reason == "" {
		t.Fatalf("networkFetchUnavailable(%+v) = empty, want refusal for unrecognized deps command", plan)
	}
	fetch, build, cleanup, err := project.planDependencyStages(plan, project.Path)
	if cleanup != nil {
		cleanup()
	}
	var refusal *fetchRefusal
	if !errors.As(err, &refusal) || fetch != nil || build != nil {
		t.Fatalf("planDependencyStages(%+v) = fetch=%+v build=%+v err=%v, want *fetchRefusal", plan, fetch, build, err)
	}

	_, err = project.RunVerificationPlan(context.Background(), plan)
	if !errors.Is(err, ErrDependencyNetworkUnavailable) {
		t.Fatalf("RunVerificationPlan(%+v) err = %v, want ErrDependencyNetworkUnavailable", plan, err)
	}
}

func TestReviewR14_CargoHiddenWorkspaceMemberAndPathDependencyCopied(t *testing.T) {
	project := newNeedProject(t, "Cargo.toml", `[workspace]
members = ["crates/app", ".hidden-member", ".hidden-glob/*"]
resolver = "2"
`)
	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(project.Path, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("crates/app/Cargo.toml", `[package]
name = "app"
version = "0.1.0"
edition = "2021"

[dependencies]
hidden-member = { path = "../../.hidden-member" }
hidden-dep = { path = "../.hidden-dep" }
`)
	mustWrite("crates/app/src/lib.rs", "pub fn app() {}\n")
	mustWrite(".hidden-member/Cargo.toml", `[package]
name = "hidden-member"
version = "0.1.0"
edition = "2021"
`)
	mustWrite(".hidden-member/src/lib.rs", "pub fn secret() {}\n")
	mustWrite(".hidden-glob/sub/Cargo.toml", `[package]
name = "hidden-glob-sub"
version = "0.1.0"
edition = "2021"
`)
	mustWrite(".hidden-glob/sub/src/main.rs", "fn main() {}\n")
	mustWrite("crates/.hidden-dep/Cargo.toml", `[package]
name = "hidden-dep"
version = "0.1.0"
edition = "2021"
`)
	mustWrite("crates/.hidden-dep/src/lib.rs", "pub fn dep() {}\n")

	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"build"}}, project.Path)
	if err != nil {
		t.Fatalf("planDependencyStages failed: %v", err)
	}
	defer cleanup()

	for _, rel := range []string{
		"Cargo.toml",
		"crates/app/Cargo.toml",
		".hidden-member/Cargo.toml",
		".hidden-glob/sub/Cargo.toml",
		"crates/.hidden-dep/Cargo.toml",
	} {
		data, err := fetch.stage.dir.readFile(filepath.FromSlash(rel), maxFetchManifestBytes)
		if err != nil || len(data) == 0 {
			t.Fatalf("expected %s to be copied into fetch dir, got len=%d err=%v", rel, len(data), err)
		}
	}
	for _, stub := range []string{
		"crates/app/src/lib.rs",
		".hidden-member/src/lib.rs",
		".hidden-glob/sub/src/main.rs",
		"crates/.hidden-dep/src/lib.rs",
	} {
		data, err := fetch.stage.dir.readFile(filepath.FromSlash(stub), maxFetchManifestBytes)
		if err != nil || len(data) != 0 {
			t.Fatalf("expected empty target stub at %s in fetch dir, got len=%d err=%v", stub, len(data), err)
		}
	}
	// Unreferenced hidden directories must still not be searched.
	mustWrite(".unreferenced-hidden/Cargo.toml", "[package]\nname = \"ignored\"\n")
	fetch2, _, cleanup2, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "cargo", Args: []string{"build"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if _, err := fetch2.stage.dir.readFile(filepath.FromSlash(".unreferenced-hidden/Cargo.toml"), maxFetchManifestBytes); err == nil {
		t.Fatalf("unreferenced hidden directory Cargo.toml should not be copied into fetch dir")
	} else if !strings.Contains(err.Error(), "no such file") && !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking unreferenced hidden Cargo.toml: %v", err)
	}
}
