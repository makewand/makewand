//go:build linux

package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCargoFetchPreservesInferredTargetsWithoutSource(t *testing.T) {
	for _, kind := range []string{"example", "test", "bench"} {
		for _, suffix := range []string{"sample.rs", "sample/main.rs"} {
			t.Run(kind+"/"+suffix, func(t *testing.T) {
				base := map[string]string{"example": "examples", "test": "tests", "bench": "benches"}[kind]
				target := base + "/" + suffix
				project := newNeedProject(t,
					"Cargo.toml", "[package]\nname='target-fixture'\nversion='0.1.0'\nedition='2021'\n[["+kind+"]]\nname='sample'\n",
					"src/lib.rs", "pub fn value() {}\n", target, "compile_error!(\"source must never enter fetch\");\n")
				fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "cargo", Args: []string{"build"}}, project.Path)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				if fetch == nil {
					t.Fatal("missing manifest-only fetch")
				}
				data, err := os.ReadFile(fetch.stage.dir.abs(target))
				if err != nil || len(data) != 0 {
					t.Fatalf("inferred target must be an empty stub: %q, %v", data, err)
				}
				cargo, err := exec.LookPath("cargo")
				if err != nil {
					t.Log("cargo unavailable; manifest/stub preservation checked")
					return
				}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				//nolint:gosec // installed cargo, fixed dependency-free manifest, offline, no build.
				cmd := exec.CommandContext(ctx, cargo, "fetch", "--offline", "--manifest-path", fetch.stage.dir.abs("Cargo.toml"))
				cmd.Dir = fetch.stage.dir.path
				cmd.Env = append(os.Environ(), "CARGO_HOME="+t.TempDir(), "CARGO_NET_OFFLINE=true", "RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER=")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("cargo rejected a previously valid manifest: %v %s", err, out)
				}
			})
		}
	}
}

func TestPipFetchPreservesParenthesizedRequirements(t *testing.T) {
	project := newNeedProject(t,
		"requirements.txt", "alpha (>=2)\nbeta[extra] (>=1, <3) ; python_version >= '3'\n",
		"pyproject.toml", "[project]\nname='fixture'\nversion='1'\ndependencies=['gamma (==1.*)']\n")
	install, ok := project.parsePipInstall([]string{"install", "-r", "requirements.txt", "."})
	if !ok {
		t.Fatal("fixture install did not parse")
	}
	args, targets, err := project.pipDownloadArgs(install, "/wheelhouse", nil)
	if err != nil || !targets {
		t.Fatalf("download planning = %v %t %v", args, targets, err)
	}
	for _, want := range []string{"alpha>=2", "beta[extra]>=1, <3 ; python_version >= '3'", "gamma==1.*"} {
		if !slices.Contains(args, want) {
			t.Errorf("valid requirement %q disappeared: %v", want, args)
		}
	}
	for _, unsafe := range []string{"alpha (file:/tmp/pkg)", "alpha (>=1) @ https://example.invalid/x", "alpha (>=1) trailing"} {
		if _, ok := pipNamedRequirementText(unsafe); ok {
			t.Errorf("unsupported target accepted: %q", unsafe)
		}
	}
}

func TestPipUnreadableConfigurationRefusesBeforeDownload(t *testing.T) {
	for _, location := range []string{"pip.conf", "pip.ini", sandboxHomeRelPath + "/.pip/pip.conf", sandboxHomeRelPath + "/.config/pip/pip.conf"} {
		for _, shape := range []string{"symlink", "oversized", "directory"} {
			t.Run(location+"/"+shape, func(t *testing.T) {
				project := newNeedProject(t)
				path := filepath.Join(project.Path, filepath.FromSlash(location))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				switch shape {
				case "symlink":
					target := filepath.Join(t.TempDir(), "private.conf")
					writeFile(t, target, "[global]\nindex-url=https://private.example.invalid/simple\n")
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				case "oversized":
					writeFile(t, path, "[global]\nindex-url=https://private.example.invalid/simple\n"+strings.Repeat("#", maxReadFileSize))
				case "directory":
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				_, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "pip", Args: []string{"install", "example-package"}}, project.Path)
				if cleanup != nil {
					defer cleanup()
				}
				var refusal *fetchRefusal
				if !errors.As(err, &refusal) || !strings.Contains(refusal.reason, "pip configuration") {
					t.Fatalf("unreadable source configuration silently fell back to PyPI: %v", err)
				}
			})
		}
	}
}

func TestGoFetchIncludesWorkspaceReplacementTargets(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGetenv = os.Getenv
	for _, replace := range []string{"replace example.invalid/lib => ./libs/local\n", "replace (\n example.invalid/lib v1.0.0 => ./libs/local\n)\n"} {
		t.Run(replace, func(t *testing.T) {
			project := newNeedProject(t,
				"go.work", "go 1.26.10\nuse ./app\n"+replace,
				"app/go.mod", "module example.invalid/app\ngo 1.26.10\nrequire example.invalid/lib v1.0.0\n",
				"libs/local/go.mod", "module example.invalid/lib\ngo 1.26.10\n",
				"libs/local/value.go", "package local\nfunc Value() int { return 1 }\n")
			fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "go", Args: []string{"mod", "download"}}, project.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if fetch == nil {
				t.Fatal("missing fetch")
			}
			if _, err := os.Stat(fetch.stage.dir.abs("libs/local/go.mod")); err != nil {
				t.Fatalf("workspace replacement module was omitted: %v", err)
			}
			if _, err := os.Stat(fetch.stage.dir.abs("libs/local/value.go")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("download copied project code: %v", err)
			}
		})
	}
	project := newNeedProject(t, "go.work", "go 1.26.10\nuse ./app\nreplace example.invalid/lib => ../outside\n", "app/go.mod", "module example.invalid/app\ngo 1.26.10\n")
	_, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "go", Args: []string{"mod", "download"}}, project.Path)
	if cleanup != nil {
		defer cleanup()
	}
	var refusal *fetchRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("external workspace replacement was not refused: %v", err)
	}
}

func TestGoWorkspaceQuotedPathsAndReplacementPrecedence(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGetenv = os.Getenv
	project := newNeedProject(t,
		"go.work", "go 1.26.10\nuse \"./app dir\"\nreplace example.invalid/lib => \"./libs/local dir\"\n",
		"app dir/go.mod", "module example.invalid/app\ngo 1.26.10\nrequire example.invalid/lib v1.0.0\nreplace example.invalid/lib => ../../outside\n",
		"libs/local dir/go.mod", "module example.invalid/lib\ngo 1.26.10\n")
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "go", Args: []string{"mod", "download"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, name := range []string{"app dir/go.mod", "libs/local dir/go.mod"} {
		if _, err := os.Stat(fetch.stage.dir.abs(name)); err != nil {
			t.Errorf("quoted workspace path was lost: %s %v", name, err)
		}
	}
}

func TestMalformedGoWorkspaceNeverGetsANetworkedFetch(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGetenv = os.Getenv
	project := newNeedProject(t, "go.work", "go 1.26.10\nuse (\n broken syntax =>\n", "go.mod", "module example.invalid/app\ngo 1.26.10\n")
	fetch, build, cleanup, err := project.planDependencyStages(ExecPlan{Command: "go", Args: []string{"mod", "download"}}, project.Path)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || fetch != nil || build == nil || build.Command != "go" {
		t.Fatalf("unparsed workspace must report its error loopback-only, not as an outage: fetch=%+v build=%+v err=%v", fetch, build, err)
	}
}
