package engine

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// verificationContract is selected from the baseline, never from candidate
// additions. Mixed projects retain their baseline's documented runner priority.
// Additional languages need separate acceptance, not an implicit runner switch.
type verificationContract struct {
	deps     *ExecPlan
	tests    *ExecPlan
	hasTests bool
	goTests  map[string]bool
}

func (p *Project) baselineVerificationContract() (verificationContract, error) {
	deps, err := p.DetectInstallPlan()
	if err != nil {
		return verificationContract{}, err
	}
	tests, err := p.DetectTestPlan()
	if err != nil {
		return verificationContract{}, err
	}
	c := verificationContract{deps: deps, tests: tests, hasTests: p.baselineTrustedTests()}
	if tests != nil && tests.Command == "go" {
		c.goTests, err = p.baselineGoTests()
	}
	return c, err
}

// baselineGoTests extracts eligible, top-level tests without executing any
// candidate code. Unresolvable/conditional suites remain conservative: missing
// expected results prevent successful verification, never grant extra trust.
func (p *Project) baselineGoTests() (map[string]bool, error) {
	content, err := p.ReadFile("go.mod")
	if err != nil {
		return nil, err
	}
	module := ""
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			module = strings.Trim(fields[1], "\"")
			break
		}
	}
	if module == "" {
		return nil, fmt.Errorf("cannot determine baseline Go module")
	}
	expected := map[string]bool{}
	err = filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(p.Path, path)
		if err != nil {
			return err
		}
		if shouldIgnore(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// ./... does not cross nested module boundaries.
			if rel != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		match, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return err
		}
		if !match {
			return nil
		}
		node, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		pkg := module
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
			pkg += "/" + dir
		}
		for _, decl := range node.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil || fn.Name.Name == "TestMain" {
				continue
			}
			name := fn.Name.Name
			if goTestName(name, "Test") || goTestName(name, "Fuzz") {
				expected[pkg+"\x00"+name] = true
			}
		}
		return nil
	})
	return expected, err
}

func goTestName(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	for _, r := range rest {
		return !unicode.IsLower(r)
	}
	return true
}

type goTestEvent struct {
	Action  string
	Package string
	Test    string
}

func parseGoTestEvents(stdout string) []goTestEvent {
	var events []goTestEvent
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 4096), maxOutputBytes)
	for scanner.Scan() {
		var event goTestEvent
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			events = append(events, event)
		}
	}
	if scanner.Err() != nil {
		return nil
	}
	return events
}

// This establishes useful diagnostic evidence, not authenticity: a hostile
// test process can also forge the tool protocol. It must never grant Strength 2.
func goBaselineTestsPassed(expected map[string]bool, result *ExecResult) bool {
	if result == nil || result.ExitCode != 0 || len(expected) == 0 {
		return false
	}
	started, passed, packages := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, event := range parseGoTestEvents(result.Stdout) {
		if event.Action == "fail" {
			return false
		}
		if event.Test == "" {
			if event.Action == "pass" {
				packages[event.Package] = true
			}
			continue
		}
		key := event.Package + "\x00" + event.Test
		if event.Action == "run" {
			started[key] = true
		}
		if event.Action == "pass" && started[key] {
			passed[key] = true
		}
	}
	for key := range expected {
		pkg, _, _ := strings.Cut(key, "\x00")
		if !passed[key] || !packages[pkg] {
			return false
		}
	}
	return true
}

func snapshotCandidateFiles(p *Project, files []ExtractedFile) ([]ExtractedFile, error) {
	seen := map[string]bool{}
	sealed := make([]ExtractedFile, 0, len(files))
	for _, f := range files {
		path := filepath.Clean(f.Path)
		if seen[path] {
			continue
		}
		seen[path] = true
		full, err := p.validatePath(path, false)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("candidate input is not a regular file: %s", path)
		}
		content, err := p.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sealed = append(sealed, ExtractedFile{Path: path, Content: content, Mode: info.Mode().Perm(), ModeKnown: true})
	}
	sort.Slice(sealed, func(i, j int) bool { return sealed[i].Path < sealed[j].Path })
	return sealed, nil
}

// workspaceInputDigest seals all project inputs, including files outside the
// patch. Tool caches are outside this input manifest; newly generated project
// files are changes and require a new verification pass.
func workspaceInputDigest(root string) (string, error) {
	tree, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer tree.Close()
	h := sha256.New()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if shouldIgnore(rel) || d.IsDir() && (d.Name() == ".pytest_cache" || d.Name() == "target") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := tree.Lstat(rel)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("verification input is not a regular file: %s", rel)
		}
		fmt.Fprintf(h, "%s\x00%03o\x00%d\x00", filepath.ToSlash(rel), info.Mode().Perm(), info.Size())
		f, err := tree.Open(rel)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifiedFilesMatch checks a sealed payload before writing it. The digest
// includes permissions; rendering/reparsing markdown is not a lossless channel.
func VerifiedFilesMatch(files []ExtractedFile, digest string) bool {
	return digest != "" && calculateFilesDigest(files) == digest
}
