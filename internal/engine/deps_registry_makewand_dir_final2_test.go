//go:build linux

package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Final round 2 (makewand-dir): a directory named .makewand, at the root or
// nested, is the project's like any other when the go command prints its
// strings: generated files may write Go files there, a replace directive or an
// import may name it, and the strings of those files are collected and
// removed before the go command's output counts as a failed request. Only
// the sandbox HOME makewand creates (.makewand/sandbox-home) is skipped, and
// generated files can no longer write there.

// What go 1.27 prints, offline, for a forged import path in a Go file of a
// directory named .makewand: reached through a replace directive (root or
// nested directory) or imported as a package of the main module.
const (
	goForgedDotMakewandReplaced = "go: example.com/fv imports\n\texample.com/x imports\n\t" + goForgedText + `: import path "Get \"https://proxy.golang.org/example.com/x/@v/list\": dial tcp: lookup proxy.golang.org: no such host" should not have @version`
	goForgedDotMakewandPackage  = "go: example.com/fv imports\n\texample.com/fv/sub/.makewand/p imports\n\t" + goForgedText + `: import path "Get \"https://proxy.golang.org/example.com/x/@v/list\": dial tcp: lookup proxy.golang.org: no such host" should not have @version`
)

// dotMakewandForgeries are the project files of each way a project puts the
// forged text into a .makewand directory, with what the go command prints.
func dotMakewandForgeries() []struct {
	name   string
	files  []string
	stderr string
} {
	gomod := "module example.com/fv\n\ngo 1.22\n"
	importX := "package main\n\nimport _ \"example.com/x\"\n\nfunc main() {}\n"
	forgedImport := "package x\n\nimport _ `" + goForgedText + "`\n"
	return []struct {
		name   string
		files  []string
		stderr string
	}{
		{"root .makewand replacement", []string{
			"go.mod", gomod + "\nrequire example.com/x v1.0.0\n\nreplace example.com/x => ./.makewand/x\n",
			"main.go", importX,
			".makewand/x/go.mod", "module example.com/x\n\ngo 1.22\n",
			".makewand/x/x.go", forgedImport,
		}, goForgedDotMakewandReplaced},
		{"nested .makewand replacement", []string{
			"go.mod", gomod + "\nrequire example.com/x v1.0.0\n\nreplace example.com/x => ./sub/.makewand/x\n",
			"main.go", importX,
			"sub/.makewand/x/go.mod", "module example.com/x\n\ngo 1.22\n",
			"sub/.makewand/x/x.go", forgedImport,
		}, goForgedDotMakewandReplaced},
		{"nested .makewand package", []string{
			"go.mod", gomod,
			"main.go", "package main\n\nimport _ \"example.com/fv/sub/.makewand/p\"\n\nfunc main() {}\n",
			"sub/.makewand/p/p.go", "package p\n\nimport _ `" + goForgedText + "`\n",
		}, goForgedDotMakewandPackage},
	}
}

// The go command's output that quotes a Go file of a .makewand directory is
// no evidence of a failed request; the go command's own report still is.
func TestGoProxyEvidenceCollectsDotMakewandStrings_Final2(t *testing.T) {
	tidy := ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}}
	for _, tc := range dotMakewandForgeries() {
		t.Run(tc.name, func(t *testing.T) {
			result := &ExecResult{ExitCode: 1, Stderr: tc.stderr}
			if goProxyFailure(result) == "" {
				t.Fatal("the output does not look like a failed request; the case tests nothing")
			}
			project := newNeedProject(t, tc.files...)
			if got := project.registryFailureEvidence(tidy, result); got != "" {
				t.Fatalf("evidence = %q, want none: the report is a string of the project's .makewand directory", got)
			}
			if got := project.registryFailureEvidence(tidy, &ExecResult{ExitCode: 1, Stderr: goGenuineProxyFailure}); got == "" {
				t.Fatal("the go command's own report of a failed request is no longer evidence")
			}
		})
	}
}

// End to end with outbound network (the host, under the acknowledged unsafe
// opt-in) and a registry that does not answer: a failed `go mod tidy` whose
// report is the project's own .makewand text is the install's failure, not
// an environment problem, and makewand checks no registry for it.
func TestGoForgeryInDotMakewandIsCandidateFailure_Final2(t *testing.T) {
	for _, tc := range dotMakewandForgeries() {
		t.Run(tc.name, func(t *testing.T) {
			unsetProxyEnv(t)
			down := closedAddr(t)
			proxied := refuseNonLocalRequests(t)
			var files []ExtractedFile
			for i := 0; i+1 < len(tc.files); i += 2 {
				files = append(files, ExtractedFile{Path: tc.files[i], Content: tc.files[i+1]})
			}
			project := newHostDepsProject(t, files...)
			fakeToolPrinting(t, "go", tc.stderr, 1)
			oldProbe := sandboxGoEnvProbe
			sandboxGoEnvProbe = func(string) (hostGoEnv, error) { return hostGoEnv{GOPROXY: "https://" + down}, nil }
			t.Cleanup(func() { sandboxGoEnvProbe = oldProbe })
			result, err := project.RunRestrictedPlan(context.Background(), ExecPlan{Kind: "deps", Command: "go", Args: []string{"mod", "tidy"}})
			if errors.Is(err, ErrDependencyNetworkUnavailable) || err != nil {
				t.Fatalf("err = %v, want the install's own failure", err)
			}
			if result == nil || result.ExitCode != 1 {
				t.Fatalf("result = %+v, want the failed install", result)
			}
			if got := proxied(); len(got) != 0 {
				t.Fatalf("a registry check contacted %q", got)
			}
		})
	}
}

// Only makewand's own sandbox HOME is skipped when the project's strings are
// collected: the strings of every other .makewand directory count.
func TestGoProjectStringsSkipOnlyTheSandboxHome_Final2(t *testing.T) {
	cached := `Get "https://cached.example/x/@v/list": cached module`
	planted := `Get "https://planted.example/x/@v/list": planted`
	nested := `Get "https://nested.example/x/@v/list": nested`
	project := newNeedProject(t,
		"go.mod", "module example.com/fv\n\ngo 1.22\n",
		filepath.ToSlash(filepath.Join(sandboxHomeRelPath, "go", "pkg", "mod", "example.com", "x@v1.0.0", "x.go")), "package x\n\nvar s = `"+cached+"`\n",
		".makewand/x/x.go", "package x\n\nvar s = `"+planted+"`\n",
		"sub/.makewand/sandbox-home/y/y.go", "package y\n\nvar s = `"+nested+"`\n",
	)
	strs, ok := project.goProjectStrings()
	if !ok {
		t.Fatal("the project strings could not all be collected")
	}
	for _, want := range []string{planted, nested} {
		if !slices.Contains(strs, want) {
			t.Fatalf("strings %q miss %q", strs, want)
		}
	}
	if slices.Contains(strs, cached) {
		t.Fatalf("strings %q hold the sandbox HOME's module cache", strs)
	}
}

// Generated files can write anywhere in a .makewand directory but makewand's
// own sandbox HOME (and rules.md), whatever the case of its name.
func TestGeneratedFilesCannotWriteTheSandboxHome_Final2(t *testing.T) {
	for _, tc := range []struct {
		path      string
		protected bool
	}{
		{".makewand/sandbox-home", true},
		{".makewand/sandbox-home/x/go.mod", true},
		{".makewand/sandbox-home/go/pkg/mod/cache/download/example.com/x/@v/v1.0.0.mod", true},
		{".makewand/sandbox-home/.npmrc", true},
		{".MAKEWAND/Sandbox-Home/x.go", true},
		{".makewand/rules.md", true},
		{".makewand/x/x.go", false},
		{".makewand/sandbox-homes/x.go", false},
		{"sub/.makewand/sandbox-home/x.go", false},
		{"sandbox-home/x.go", false},
	} {
		if got := isProtectedWritePath(filepath.FromSlash(tc.path)); got != tc.protected {
			t.Errorf("isProtectedWritePath(%q) = %v, want %v", tc.path, got, tc.protected)
		}
	}
	project := newNeedProject(t, "go.mod", "module example.com/fv\n\ngo 1.22\n")
	err := project.WriteFile(".makewand/sandbox-home/x/go.mod", "module example.com/x\n")
	if err == nil || !strings.Contains(err.Error(), "protected path") {
		t.Fatalf("WriteFile into the sandbox HOME = %v, want it refused", err)
	}
	if err := project.WriteFile(".makewand/x/x.go", "package x\n"); err != nil {
		t.Fatalf("WriteFile into another .makewand directory: %v", err)
	}
}
