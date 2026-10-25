//go:build linux

package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPip3FetchUsesPip3InterpreterForSiteConfigAndMarkers(t *testing.T) {
	swapVerifyIsolationVars(t)
	pipHostConfigFiles = nil

	makePrefix := func(name, indexURL string) (binDir, pythonPath string) {
		root := t.TempDir()
		binDir = filepath.Join(root, "bin")
		if err := os.MkdirAll(binDir, 0o700); err != nil {
			t.Fatal(err)
		}
		pythonPath = filepath.Join(binDir, "python3")
		if err := os.WriteFile(pythonPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test executable stub
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "pip.conf"), []byte("[global]\nindex-url = "+indexURL+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(binDir, name)
		if err := os.WriteFile(script, []byte("#!"+pythonPath+"\n"), 0o700); err != nil { //nolint:gosec // test executable stub
			t.Fatal(err)
		}
		return binDir, pythonPath
	}

	pipBinDir, pipPython := makePrefix("pip", "https://wrong-pip.example.invalid/simple")
	pip3BinDir, pip3Python := makePrefix("pip3", "https://right-pip3.example.invalid/simple")
	pathEnv := pipBinDir + string(os.PathListSeparator) + pip3BinDir + string(os.PathListSeparator) + "/usr/bin:/bin"
	verifyGetenv = func(key string) string {
		if key == "PATH" {
			return pathEnv
		}
		return os.Getenv(key)
	}

	project := newNeedProject(t,
		"requirements.txt", "pkg @ https://example.invalid/pkg-1.0.whl ; python_version == '3.13'\n")
	fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "pip3", Args: []string{"install", "example-package"}}, project.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if fetch == nil {
		t.Fatal("missing fetch stage")
	}
	joined := strings.Join(fetch.Args, " ")
	if !strings.Contains(joined, "--index-url https://right-pip3.example.invalid/simple") {
		t.Fatalf("pip3 fetch did not use pip3 site configuration: %v", fetch.Args)
	}
	if strings.Contains(joined, "wrong-pip.example.invalid") {
		t.Fatalf("pip3 fetch leaked pip site configuration: %v", fetch.Args)
	}

	pipMarkerEnvironment = func(python string) pipMarkerEnv {
		switch python {
		case pipPython:
			return pipMarkerEnv{"python_version": "3.11"}
		case pip3Python:
			return pipMarkerEnv{"python_version": "3.13"}
		default:
			return nil
		}
	}
	got := project.pipUnfetchedRemoteTarget(ExecPlan{Command: "pip3", Args: []string{"install", "-r", "requirements.txt"}})
	if !strings.Contains(got, "https://example.invalid/pkg-1.0.whl") {
		t.Fatalf("pip3 marker evaluation did not use pip3 interpreter: got %q", got)
	}
}

func TestPipUnreadableRequirementOrConstraintFilesRefuseBeforeDownload(t *testing.T) {
	for _, flag := range []string{"-r", "-c"} {
		for _, shape := range []string{"missing", "outside", "symlink", "oversized", "directory", "too_deep"} {
			t.Run(flag+"/"+shape, func(t *testing.T) {
				project := newNeedProject(t, "requirements.txt", "example-package==1.0\n")
				nested := "nested.txt"
				switch shape {
				case "missing":
					nested = "does-not-exist.txt"
				case "outside":
					outside := filepath.Join(t.TempDir(), "outside.txt")
					writeFile(t, outside, "--index-url https://private.example.invalid/simple\n")
					nested = "../" + filepath.Base(outside)
				case "symlink":
					target := filepath.Join(t.TempDir(), "secret.txt")
					writeFile(t, target, "--index-url https://private.example.invalid/simple\n")
					if err := os.Symlink(target, filepath.Join(project.Path, nested)); err != nil {
						t.Fatal(err)
					}
				case "oversized":
					writeFile(t, filepath.Join(project.Path, nested), "--index-url https://private.example.invalid/simple\n"+strings.Repeat("#", maxReadFileSize))
				case "directory":
					if err := os.Mkdir(filepath.Join(project.Path, nested), 0o700); err != nil {
						t.Fatal(err)
					}
				case "too_deep":
					for i := 0; i < 10; i++ {
						cur := fmt.Sprintf("step-%d.txt", i)
						next := fmt.Sprintf("step-%d.txt", i+1)
						writeFile(t, filepath.Join(project.Path, cur), flag+" "+next+"\n")
					}
					writeFile(t, filepath.Join(project.Path, "step-10.txt"), "--index-url https://private.example.invalid/simple\n")
					nested = "step-0.txt"
				}
				writeFile(t, filepath.Join(project.Path, "requirements.txt"), "example-package==1.0\n"+flag+" "+nested+"\n")
				fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "pip", Args: []string{"install", "-r", "requirements.txt"}}, project.Path)
				if cleanup != nil {
					defer cleanup()
				}
				var refusal *fetchRefusal
				if !errors.As(err, &refusal) || fetch != nil {
					t.Fatalf("unreadable %s (%s) did not refuse networked fetch: fetch=%+v err=%v", flag, shape, fetch, err)
				}
			})
		}
	}
}

func TestNpmFetchCannotReenableAuditOrFundTraffic(t *testing.T) {
	for _, flag := range []string{
		"--audit=true", "--audit=1", "--audit=", "--audit",
		"--fund=true", "--fund=1", "--fund=", "--fund",
	} {
		if npmNetworkedOption(flag) {
			t.Fatalf("networked npm fetch accepted %q", flag)
		}
		installArgs := npmInstallArgs([]string{"ci", flag}, true)
		if slices.Contains(installArgs, flag) || !slices.Contains(installArgs, "--no-audit") || !slices.Contains(installArgs, "--no-fund") {
			t.Fatalf("unsafe audit/fund flag survived npmInstallArgs: %q", installArgs)
		}
		resArgs := npmResolutionArgs([]string{"install", flag})
		if slices.Contains(resArgs, flag) || !slices.Contains(resArgs, "--no-audit") || !slices.Contains(resArgs, "--no-fund") {
			t.Fatalf("unsafe audit/fund flag survived npmResolutionArgs: %q", resArgs)
		}
	}
	for _, safe := range []string{"--no-audit", "--audit=false", "--no-fund", "--fund=false"} {
		if !npmNetworkedOption(safe) {
			t.Fatalf("networked npm fetch rejected safe flag %q", safe)
		}
		installArgs := npmInstallArgs([]string{"ci", safe}, true)
		if slices.Contains(installArgs, "--audit=false") || slices.Contains(installArgs, "--fund=false") {
			t.Fatalf("redundant audit/fund value flag was not normalized to --no-audit/--no-fund: %q", installArgs)
		}
	}
}

func TestGoWorkspaceVersionSpecificReplaceOverridesUnversionedGoModReplace(t *testing.T) {
	swapVerifyIsolationVars(t)
	verifyGetenv = os.Getenv

	t.Run("selected version replaced in go.work suppresses unversioned external go.mod replace", func(t *testing.T) {
		project := newNeedProject(t,
			"go.work", "go 1.26.10\nuse ./app\nreplace example.invalid/lib v1.0.0 => ./libs/local\n",
			"app/go.mod", "module example.invalid/app\ngo 1.26.10\nrequire example.invalid/lib v1.0.0\nreplace example.invalid/lib => ../../outside\n",
			"libs/local/go.mod", "module example.invalid/lib\ngo 1.26.10\n",
			"libs/local/value.go", "package local\nfunc Value() int { return 1 }\n")
		fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "go", Args: []string{"mod", "download"}}, project.Path)
		if err != nil {
			t.Fatalf("overridden go.mod replacement caused false refusal: %v", err)
		}
		defer cleanup()
		if fetch == nil {
			t.Fatal("missing fetch stage")
		}
		if _, err := os.Stat(fetch.stage.dir.abs("libs/local/go.mod")); err != nil {
			t.Fatalf("workspace replacement module was omitted: %v", err)
		}
		if _, err := os.Stat(fetch.stage.dir.abs("libs/local/value.go")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("download copied project code: %v", err)
		}
	})

	t.Run("multi-module workspace uses MVS-selected version for override check", func(t *testing.T) {
		for _, workReplaceVer := range []string{"v1.2.0", "v1.0.0"} {
			t.Run("work_replaces_"+workReplaceVer, func(t *testing.T) {
				project := newNeedProject(t,
					"go.work", "go 1.26.10\nuse (\n\t./app1\n\t./app2\n)\nreplace example.invalid/lib "+workReplaceVer+" => ./libs/local\n",
					"app1/go.mod", "module example.invalid/app1\ngo 1.26.10\nrequire example.invalid/lib v1.0.0\nreplace example.invalid/lib => ../../outside\n",
					"app2/go.mod", "module example.invalid/app2\ngo 1.26.10\nrequire example.invalid/lib v1.2.0\n",
					"libs/local/go.mod", "module example.invalid/lib\ngo 1.26.10\n")
				fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Command: "go", Args: []string{"mod", "download"}}, project.Path)
				if cleanup != nil {
					defer cleanup()
				}
				if workReplaceVer == "v1.2.0" {
					if err != nil || fetch == nil {
						t.Fatalf("workspace replacement for MVS winner v1.2.0 was refused: fetch=%+v err=%v", fetch, err)
					}
				} else {
					var refusal *fetchRefusal
					if !errors.As(err, &refusal) {
						t.Fatalf("non-selected workspace replacement %s suppressed active external replacement: err=%v", strconv.Quote(workReplaceVer), err)
					}
				}
			})
		}
	})
}
