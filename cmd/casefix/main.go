package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/diag"
	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

const (
	goFixSystemPrompt = "You are a senior Go engineer. Fix compilation/test issues with minimal changes. Output only changed files."
)

type modeReport struct {
	Mode             string `json:"mode"`
	ProjectDir       string `json:"project_dir"`
	Provider         string `json:"provider,omitempty"`
	Fixed            bool   `json:"fixed"`
	AttemptedFix     bool   `json:"attempted_fix"`
	FilesWritten     int    `json:"files_written"`
	BeforePassed     bool   `json:"before_passed"`
	AfterPassed      bool   `json:"after_passed"`
	BeforeTestOutput string `json:"before_test_output,omitempty"`
	AfterTestOutput  string `json:"after_test_output,omitempty"`
	Error            string `json:"error,omitempty"`
}

type roundReport struct {
	Root       string                `json:"root"`
	CaseID     string                `json:"case_id"`
	Generated  time.Time             `json:"generated"`
	FixReports []modeReport          `json:"fix_reports"`
	Verify     map[string]modeReport `json:"verify"`
}

func main() {
	root := flag.String("root", "", "realcases output root (required), e.g. /tmp/makewand-realcases-20260304-124535")
	caseID := flag.String("case", "go-notes-api", "case ID to repair/verify")
	flag.Parse()

	if strings.TrimSpace(*root) == "" {
		diag.Stderr().ErrorText("missing -root")
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		diag.Stderr().ErrorErr("config load failed", err)
		os.Exit(1)
	}

	report := roundReport{
		Root:      *root,
		CaseID:    *caseID,
		Generated: time.Now().UTC(),
		Verify:    map[string]modeReport{},
	}

	fixModes := []model.UsageMode{model.ModeFast}
	for _, m := range fixModes {
		rep := fixMode(cfg, *root, *caseID, m)
		report.FixReports = append(report.FixReports, rep)
	}

	verifyModes := []model.UsageMode{model.ModeFast, model.ModeBalanced, model.ModePower}
	allPass := true
	for _, m := range verifyModes {
		rep := verifyMode(*root, *caseID, m)
		report.Verify[m.String()] = rep
		if !rep.AfterPassed {
			allPass = false
		}
	}

	data, _ := json.MarshalIndent(report, "", "  ")
	reportPath := filepath.Join(*root, fmt.Sprintf("%s-fix-report.json", *caseID))
	_ = os.WriteFile(reportPath, data, 0o644)
	fmt.Printf("REPORT=%s\n", reportPath)

	fmt.Println()
	fmt.Println("=== Fix Summary ===")
	for _, r := range report.FixReports {
		fmt.Printf("%-8s before=%v after=%v attempted=%v fixed=%v files=%d\n",
			r.Mode, r.BeforePassed, r.AfterPassed, r.AttemptedFix, r.Fixed, r.FilesWritten)
	}
	fmt.Println()
	fmt.Println("=== Verify Summary ===")
	for _, mode := range []string{"fast", "balanced", "power"} {
		r := report.Verify[mode]
		status := "FAIL"
		if r.AfterPassed {
			status = "PASS"
		}
		fmt.Printf("%-8s %s\n", mode, status)
	}

	if !allPass {
		os.Exit(1)
	}
}

func fixMode(cfg *config.Config, root, caseID string, mode model.UsageMode) modeReport {
	rep := modeReport{Mode: mode.String()}
	modeDir := filepath.Join(root, mode.String(), caseID)
	projectDir, err := findModuleRoot(modeDir)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	rep.ProjectDir = projectDir

	beforeOut, beforePass := runGoTest(projectDir)
	rep.BeforeTestOutput = beforeOut
	rep.BeforePassed = beforePass
	if beforePass {
		rep.AfterPassed = true
		return rep
	}

	cfgCopy := *cfg
	cfgCopy.UsageMode = mode.String()
	router := model.NewRouter(&cfgCopy)

	projectContext := collectProjectFiles(projectDir, 64*1024)
	prompt := buildModeFixPrompt(mode, beforeOut, projectContext)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	content, usage, result, chatErr := router.ChatBest(ctx, model.PhaseFix,
		[]model.Message{{Role: "user", Content: prompt}}, goFixSystemPrompt)
	rep.AttemptedFix = true
	if chatErr != nil {
		rep.Error = fmt.Sprintf("fix prompt failed: %v", chatErr)
		rep.AfterTestOutput = beforeOut
		rep.AfterPassed = false
		return rep
	}
	if result.Actual != "" {
		rep.Provider = result.Actual
	} else {
		rep.Provider = usage.Provider
	}

	parsed := engine.ParseFilesBestEffort(content)
	if len(parsed.Files) == 0 {
		rep.Error = "fix response did not contain writable files"
		rep.AfterTestOutput = beforeOut
		rep.AfterPassed = false
		return rep
	}

	written, writeErr := writeFiles(projectDir, parsed.Files)
	rep.FilesWritten = written
	if writeErr != nil {
		rep.Error = fmt.Sprintf("write fixed files: %v", writeErr)
	}

	afterOut, afterPass := runGoTest(projectDir)
	rep.AfterTestOutput = afterOut
	rep.AfterPassed = afterPass
	rep.Fixed = !beforePass && afterPass
	return rep
}

func verifyMode(root, caseID string, mode model.UsageMode) modeReport {
	rep := modeReport{Mode: mode.String()}
	modeDir := filepath.Join(root, mode.String(), caseID)
	projectDir, err := findModuleRoot(modeDir)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	rep.ProjectDir = projectDir
	out, pass := runGoTest(projectDir)
	rep.AfterTestOutput = out
	rep.AfterPassed = pass
	return rep
}

func findModuleRoot(base string) (string, error) {
	var found []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "go.mod" {
			found = append(found, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "", fmt.Errorf("go.mod not found under %s", base)
	}
	sort.Strings(found)
	return found[0], nil
}

func runGoTest(dir string) (string, bool) {
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	env := os.Environ()
	env = append(env, "GOCACHE=/tmp/gocache", "GOMODCACHE=/tmp/gomodcache")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func collectProjectFiles(root string, maxBytes int) string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.EqualFold(base, "raw.txt") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".go" || base == "go.mod" || base == "go.sum" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)

	var b strings.Builder
	for _, abs := range files {
		rel, _ := filepath.Rel(root, abs)
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		block := fmt.Sprintf("--- FILE: %s ---\n```\n%s\n```\n\n", filepath.ToSlash(rel), string(data))
		if b.Len()+len(block) > maxBytes {
			b.WriteString("(truncated)\n")
			break
		}
		b.WriteString(block)
	}
	return b.String()
}

func writeFiles(root string, files []engine.ExtractedFile) (int, error) {
	written := 0
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(target, []byte(f.Content), 0o644); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

func buildModeFixPrompt(mode model.UsageMode, testOutput, projectContext string) string {
	common := fmt.Sprintf(
		"The Go project currently fails tests. Fix it with minimal changes.\n\n"+
			"Current go test output:\n```\n%s\n```\n\n"+
			"Project files:\n\n%s\n\n"+
			"Output ONLY changed files using:\n"+
			"--- FILE: path/to/file ---\n```\nfile content\n```\n",
		trimText(testOutput, 6000),
		trimText(projectContext, 32000),
	)

	switch mode {
	case model.ModeFast:
		return common + "\nFast mode template:\n" +
			"- Keep architecture unchanged, patch only what is necessary.\n" +
			"- Do not rely on downloading new dependencies.\n" +
			"- Prefer standard library replacements when possible.\n" +
			"- Ensure go.mod is consistent with actual imports.\n" +
			"- Ensure `go test ./...` passes."
	default:
		return common
	}
}

func trimText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 32 {
		return s[:max]
	}
	return s[:max-16] + "\n...[truncated]"
}
