//go:build linux

package engine

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GEC-1021-1 round 4 (review of the round-3 fix of PR #91).

// A [project] dependency with an environment marker is a strict PEP 508 named
// requirement, so the networked fetch receives it like the same line in a
// requirements file: pip evaluates the marker itself. Dropping it left the
// offline install without the wheel whenever the marker held on the
// verification host, failing a legitimate project as a candidate failure.
func TestPipProjectDependencyMarkersReachFetch_GEC1021R4(t *testing.T) {
	marked := []string{`tomli>=2 ; python_version < "3.11"`, `colorama ; sys_platform != "win32"`}
	project := newNeedProject(t, "pyproject.toml",
		"[build-system]\nrequires = [\"setuptools>=61\"]\nbuild-backend = \"setuptools.build_meta\"\n\n"+
			"[project]\nname = \"x\"\nversion = \"0\"\n"+
			"dependencies = ['tomli>=2 ; python_version < \"3.11\"', 'colorama ; sys_platform != \"win32\"', \"requests>=2\", "+
			"'evil @ https://a.example/e.tgz ; python_version > \"3\"', 'git+https://a.example/r.git ; python_version > \"3\"']\n",
		"requirements.txt", marked[0]+"\n")
	for _, args := range [][]string{{"install", "--user", "-e", "."}, {"install", "--user", "-r", "requirements.txt", "-e", "."}} {
		fetch, _, cleanup, err := project.planDependencyStages(ExecPlan{Kind: "deps", Command: "pip", Args: args}, project.Path)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if fetch == nil {
			t.Fatalf("pip %v: fetch = nil, want a download", args)
		}
		for _, want := range append([]string{"setuptools>=61", "requests>=2"}, marked...) {
			if !slices.Contains(fetch.Args, want) {
				t.Errorf("pip %v: fetch = %q, want %q (markers are pip's to evaluate)", args, fetch.Args, want)
			}
		}
		forbidBuildableFetchTargets(t, fetch)
	}
}

// Live: a VCS or URL dependency the project declares is never handed to the
// networked fetch, so the loopback install cannot obtain it and fails. That is
// the environment limitation of fetch-only networking the owner decided on,
// with a message naming the dependency - decided from what the project
// declares, never from the install's output. Without such a dependency the
// same failure stays the candidate's (TestForgedBuildOutputStaysCandidateFailure_GEC1021R2).
func TestDeclaredRemoteDependencyIsEnvironment_GEC1021R4(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	// A fake pip whose download succeeds (the named requirements) and whose
	// offline install fails, as real pip does when it cannot clone or download
	// the excluded dependency without network.
	const pip = `if sys.argv[1] == 'download':
    raise SystemExit(0)
print('ERROR: could not obtain a dependency', file=sys.stderr)
raise SystemExit(1)
`
	for _, tc := range []struct {
		name  string
		files []ExtractedFile
		args  []string
		want  string
	}{
		{"requirements VCS reference",
			[]ExtractedFile{{Path: "requirements.txt", Content: "six==1.16.0\ngit+https://example.invalid/r.git#egg=r\n"}},
			[]string{"install", "--user", "-r", "requirements.txt"}, "git+https://example.invalid/r.git#egg=r"},
		{"requirements direct reference",
			[]ExtractedFile{{Path: "requirements.txt", Content: "six==1.16.0\npkg @ https://example.invalid/pkg-1.0.tar.gz\n"}},
			[]string{"install", "--user", "-r", "requirements.txt"}, "pkg @ https://example.invalid/pkg-1.0.tar.gz"},
		{"pyproject direct reference",
			[]ExtractedFile{{Path: "pyproject.toml", Content: "[project]\nname = \"x\"\nversion = \"0\"\ndependencies = [\"six\", \"pkg @ git+https://example.invalid/r.git\"]\n"}},
			[]string{"install", "--user", "-e", "."}, "pkg @ git+https://example.invalid/r.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := sandboxDepsProject(t)
			tools := outsideTools(t) // a fetch never runs a workspace executable
			if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
				t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
			}
			writePipFixture(t, filepath.Join(tools, "pip"), pip)
			if err := project.WriteFiles(tc.files); err != nil {
				t.Fatal(err)
			}
			result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: tc.args})
			if !errorIsDepNetworkUnavailable(err) {
				t.Fatalf("err = %v, want ErrDependencyNetworkUnavailable for the excluded dependency", err)
			}
			if result == nil || !strings.Contains(result.Note, tc.want) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("note = %q, err = %v, want both to name %q", noteOf(result), err, tc.want)
			}
		})
	}
}

// Live: the declarations are read before the build runs project code, so a
// build that adds a VCS reference to the requirements and then fails stays the
// candidate's failure, as forged output does.
func TestRemoteDependencyAddedByBuildStaysCandidate_GEC1021R4(t *testing.T) {
	if testing.Short() {
		t.Skip("live sandbox test")
	}
	project := sandboxDepsProject(t)
	tools := outsideTools(t) // a fetch never runs a workspace executable
	if !VerificationIsolationActive(UnsafeHostExecAuthorization{}) {
		t.Skipf("bubblewrap isolation unavailable: %v", RestrictedExecIsolationError(UnsafeHostExecAuthorization{}))
	}
	const pip = `if sys.argv[1] == 'download':
    raise SystemExit(0)
with open('requirements.txt', 'a') as f:
    f.write('git+https://example.invalid/r.git#egg=r\n')
raise SystemExit(1)
`
	writePipFixture(t, filepath.Join(tools, "pip"), pip)
	if err := project.WriteFiles([]ExtractedFile{{Path: "requirements.txt", Content: "six==1.16.0\n"}}); err != nil {
		t.Fatal(err)
	}
	result, err := project.RunVerificationPlan(context.Background(), ExecPlan{Kind: "deps", Command: "pip", Args: []string{"install", "--user", "-r", "requirements.txt"}})
	if errorIsDepNetworkUnavailable(err) {
		t.Fatalf("a dependency the build itself declared must not earn the environment verdict: %v", err)
	}
	if err != nil {
		t.Fatalf("RunVerificationPlan: %v", err)
	}
	if result == nil || result.ExitCode == 0 {
		t.Fatalf("result = %+v, want the failing build reported as the candidate's own failure", result)
	}
	if content, _ := project.ReadFile("requirements.txt"); !strings.Contains(content, "git+") {
		t.Fatalf("the fake build did not rewrite requirements.txt (%q), so the test proves nothing", content)
	}
}

// The declared-dependency fact behind TestDeclaredRemoteDependencyIsEnvironment_GEC1021R4:
// only a reference that needs a download from a location the project names
// counts, wherever it is declared; a local one (a path, file:, git+file:) or a
// build requirement pip does not install (--no-build-isolation) does not.
func TestPipUnfetchedRemoteTarget_GEC1021R4(t *testing.T) {
	pyproject := func(deps, requires string) string {
		return "[build-system]\nrequires = [" + requires + "]\nbuild-backend = \"b\"\n\n[project]\nname = \"x\"\nversion = \"0\"\ndependencies = [" + deps + "]\n"
	}
	for _, tc := range []struct {
		name  string
		files []string
		args  []string
		want  string
	}{
		{"named only", []string{"requirements.txt", "six==1.16.0\nflask ; python_version > \"3\"\n"},
			[]string{"install", "-r", "requirements.txt"}, ""},
		{"VCS in requirements", []string{"requirements.txt", "six\nhg+https://h.example/r#egg=r\n"},
			[]string{"install", "-r", "requirements.txt"}, "hg+https://h.example/r#egg=r (requirements.txt)"},
		{"URL in a nested requirement file", []string{"requirements.txt", "-r more.txt\n", "more.txt", "https://h.example/p-1.0.whl\n"},
			[]string{"install", "-r", "requirements.txt"}, "https://h.example/p-1.0.whl (more.txt)"},
		{"direct reference with a marker", []string{"requirements.txt", "p @ https://h.example/p.tgz ; python_version > \"3\"\n"},
			[]string{"install", "-r", "requirements.txt"}, "p @ https://h.example/p.tgz ; python_version > \"3\" (requirements.txt)"},
		{"editable VCS on the command line", nil,
			[]string{"install", "-e", "git+ssh://git@h.example/r.git#egg=r"}, "-e git+ssh://git@h.example/r.git#egg=r (the pip command line)"},
		{"pyproject dependency", []string{"pyproject.toml", pyproject(`"six", "p @ https://h.example/p.tgz"`, `"setuptools"`)},
			[]string{"install", "-e", "."}, "p @ https://h.example/p.tgz (dependencies in pyproject.toml)"},
		{"pyproject build requirement", []string{"pyproject.toml", pyproject(`"six"`, `"b @ git+https://h.example/b.git"`)},
			[]string{"install", "-e", "."}, "b @ git+https://h.example/b.git (build requirement for pyproject.toml)"},
		{"build requirement pip does not install", []string{"pyproject.toml", pyproject(`"six"`, `"b @ git+https://h.example/b.git"`)},
			[]string{"install", "--no-build-isolation", "-e", "."}, ""},
		{"local references", []string{"requirements.txt", "./sub\nfile:./sub\np @ file:///tmp/p\ngit+file:///tmp/r#egg=r\n-e git+file:///tmp/r#egg=r\n", "sub/setup.py", ""},
			[]string{"install", "-r", "requirements.txt"}, ""},
		{"dynamic dependencies are unknown", []string{"pyproject.toml", "[project]\nname = \"x\"\nversion = \"0\"\ndynamic = [\"dependencies\"]\ndependencies = [\"p @ https://h.example/p.tgz\"]\n"},
			[]string{"install", "-e", "."}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := newNeedProject(t, tc.files...)
			if got := project.pipUnfetchedRemoteTarget(ExecPlan{Kind: "deps", Command: "pip", Args: tc.args}); got != tc.want {
				t.Fatalf("pipUnfetchedRemoteTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

func noteOf(result *ExecResult) string {
	if result == nil {
		return ""
	}
	return result.Note
}

// forbidBuildableFetchTargets fails when a networked pip fetch carries a
// buildable target: a URL, a VCS reference or a direct reference.
func forbidBuildableFetchTargets(t *testing.T, fetch *ExecPlan) {
	t.Helper()
	for _, arg := range fetch.Args {
		if strings.Contains(arg, "://") || strings.Contains(arg, "@") || strings.HasPrefix(arg, "git+") {
			t.Fatalf("networked fetch must not carry a buildable target, got %q in %q", arg, fetch.Args)
		}
	}
}
