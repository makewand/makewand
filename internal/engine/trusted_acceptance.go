package engine

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
)

// TrustedAcceptanceSpec defines black-box observations judged by Makewand's
// parent process. It is user policy, never discovered from project metadata.
// Candidate stdout is application data and cannot issue an acceptance verdict.
// Exact comparisons establish only the specified behavior, not arbitrary code
// safety or completeness of a user's test coverage.
type TrustedAcceptanceSpec struct {
	Schema         int                     `json:"schema"`
	Command        string                  `json:"command"`
	Args           []string                `json:"args"`
	Cases          []TrustedAcceptanceCase `json:"cases"`
	Timeout        string                  `json:"timeout,omitempty"`
	MaxOutputBytes int                     `json:"max_output_bytes,omitempty"`
}

type TrustedAcceptanceCase struct {
	Name     string `json:"name"`
	Stdin    string `json:"stdin"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// TrustedAcceptanceRecord is produced only by the parent driver. It binds the
// successful observations to both the full workspace and the delivered patch.
// This is an in-process authorization record, not a transferable signature.
type TrustedAcceptanceRecord struct {
	Schema          int    `json:"schema"`
	Driver          string `json:"driver"`
	SpecDigest      string `json:"spec_digest"`
	WorkspaceDigest string `json:"workspace_digest"`
	BaselineDigest  string `json:"baseline_digest"`
	ArtifactDigest  string `json:"artifact_digest"`
	CasesPassed     int    `json:"cases_passed"`
	Isolated        bool   `json:"isolated"`
	authority       []byte // parent-only MAC; JSON cannot import approval
}

type trustedAcceptanceKey struct{}
type frozenTrustedAcceptance struct {
	spec          TrustedAcceptanceSpec
	digest        string
	hiddenFile    string
	timeout       time.Duration
	commandPath   string
	commandDigest string
}

// ContextWithTrustedAcceptance snapshots an explicit SDK policy immediately.
// Subsequent caller changes to its slices cannot change the frozen contract.
// A malformed policy is retained as an error and rejected before generation.
func ContextWithTrustedAcceptance(ctx context.Context, spec TrustedAcceptanceSpec) context.Context {
	frozen, err := freezeTrustedAcceptance(spec, "")
	return context.WithValue(ctx, trustedAcceptanceKey{}, trustedAcceptanceContext{frozen: frozen, err: err})
}

type trustedAcceptanceContext struct {
	frozen *frozenTrustedAcceptance
	err    error
}

const (
	trustedAcceptanceMaxSpecBytes = 1 << 20
	trustedAcceptanceDefaultLimit = 64 << 10
	trustedAcceptanceMaxCases     = 256
)

func freezeTrustedAcceptance(spec TrustedAcceptanceSpec, hiddenFile string) (*frozenTrustedAcceptance, error) {
	if spec.Schema != 1 {
		return nil, fmt.Errorf("trusted acceptance schema must be 1")
	}
	if err := validateCommandName(spec.Command); err != nil {
		return nil, fmt.Errorf("trusted acceptance command: %w", err)
	}
	if _, ok := restrictedAllowedCommands[spec.Command]; !ok {
		return nil, fmt.Errorf("trusted acceptance command %q is blocked by execution policy", spec.Command)
	}
	if len(spec.Cases) == 0 || len(spec.Cases) > trustedAcceptanceMaxCases {
		return nil, fmt.Errorf("trusted acceptance requires 1–%d cases", trustedAcceptanceMaxCases)
	}
	if spec.MaxOutputBytes == 0 {
		spec.MaxOutputBytes = trustedAcceptanceDefaultLimit
	}
	if spec.MaxOutputBytes < 1 || spec.MaxOutputBytes > maxOutputBytes {
		return nil, fmt.Errorf("trusted acceptance output limit must be 1–%d bytes", maxOutputBytes)
	}
	duration := 30 * time.Second
	if spec.Timeout != "" {
		var err error
		duration, err = time.ParseDuration(spec.Timeout)
		if err != nil || duration <= 0 || duration > RestrictedPlanTimeout {
			return nil, fmt.Errorf("trusted acceptance timeout must be positive and at most %s", RestrictedPlanTimeout)
		}
	}
	spec.Timeout = duration.String()
	names := map[string]bool{}
	for _, c := range spec.Cases {
		if strings.TrimSpace(c.Name) == "" || names[c.Name] {
			return nil, fmt.Errorf("trusted acceptance case names must be nonempty and unique")
		}
		names[c.Name] = true
		if c.ExitCode < 0 || c.ExitCode > 255 {
			return nil, fmt.Errorf("trusted acceptance exit code must be 0–255")
		}
		if len(c.Stdout)+len(c.Stderr) > spec.MaxOutputBytes {
			return nil, fmt.Errorf("trusted acceptance expected output exceeds the capture limit")
		}
	}
	encoded, err := json.Marshal(spec)
	if err != nil || len(encoded) > trustedAcceptanceMaxSpecBytes {
		return nil, fmt.Errorf("trusted acceptance specification exceeds %d bytes", trustedAcceptanceMaxSpecBytes)
	}
	// JSON round-trip provides an owned copy, including args and case slices.
	var owned TrustedAcceptanceSpec
	if err := json.Unmarshal(encoded, &owned); err != nil {
		return nil, err
	}
	resolved, err := exec.LookPath(spec.Command)
	if err != nil {
		return nil, fmt.Errorf("trusted acceptance runtime is unavailable: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil || !filepath.IsAbs(resolved) {
		return nil, fmt.Errorf("trusted acceptance runtime must resolve to an absolute regular file")
	}
	commandDigest, err := trustedCommandDigest(resolved)
	if err != nil {
		return nil, err
	}
	// Runtime selection is parent policy, not a second PATH lookup inside the
	// candidate cwd. Bind both its path and bytes to the normalized contract.
	h := sha256.New()
	_, _ = h.Write(encoded)
	fmt.Fprintf(h, "\x00%s\x00%s", resolved, commandDigest)
	return &frozenTrustedAcceptance{spec: owned, digest: hex.EncodeToString(h.Sum(nil)), hiddenFile: hiddenFile, timeout: duration, commandPath: resolved, commandDigest: commandDigest}, nil
}

// prepareTrustedAcceptance loads environment policy once, before generation or
// candidate execution. An explicit SDK policy replaces environment discovery.
func prepareTrustedAcceptance(ctx context.Context, project *Project) (context.Context, error) {
	if state, ok := ctx.Value(trustedAcceptanceKey{}).(trustedAcceptanceContext); ok {
		if state.err != nil {
			return ctx, state.err
		}
		return ctx, validateAcceptanceRuntimeLocation(project, state.frozen)
	}
	path := strings.TrimSpace(os.Getenv("MAKEWAND_TRUSTED_ACCEPTANCE_FILE"))
	if path == "" {
		return context.WithValue(ctx, trustedAcceptanceKey{}, trustedAcceptanceContext{}), nil
	}
	if !filepath.IsAbs(path) {
		return ctx, fmt.Errorf("MAKEWAND_TRUSTED_ACCEPTANCE_FILE must be an absolute path outside the project")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ctx, fmt.Errorf("resolve trusted acceptance file: %w", err)
	}
	if project != nil {
		projectRoot, err := filepath.EvalSymlinks(project.Path)
		if err != nil {
			return ctx, err
		}
		if pathWithin(projectRoot, canonical) {
			return ctx, fmt.Errorf("trusted acceptance file must be outside the project")
		}
	}
	info, err := os.Lstat(path) //nolint:gosec // G703: explicit user policy path; canonical project exclusion is checked before reading.
	if err != nil || !info.Mode().IsRegular() {
		return ctx, fmt.Errorf("trusted acceptance file must be a regular file, not a symlink")
	}
	file, err := os.Open(canonical) //nolint:gosec // G703: user-authorized configuration outside the project, with fixed regular-file identity checked below.
	if err != nil {
		return ctx, fmt.Errorf("open trusted acceptance file: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return ctx, fmt.Errorf("trusted acceptance file changed while being opened")
	}
	identity, err := checkpointIdentity(canonical, opened)
	if err != nil || identity.Nlink != 1 {
		return ctx, fmt.Errorf("trusted acceptance file must have one private regular-file identity; hardlinks cannot protect the oracle")
	}
	data, err := io.ReadAll(io.LimitReader(file, trustedAcceptanceMaxSpecBytes+1))
	if err != nil || len(data) > trustedAcceptanceMaxSpecBytes {
		return ctx, fmt.Errorf("trusted acceptance file cannot exceed %d bytes", trustedAcceptanceMaxSpecBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var spec TrustedAcceptanceSpec
	if err := decoder.Decode(&spec); err != nil {
		return ctx, fmt.Errorf("decode trusted acceptance file: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ctx, fmt.Errorf("trusted acceptance file must contain exactly one JSON object")
	}
	frozen, err := freezeTrustedAcceptance(spec, canonical)
	if err == nil {
		err = validateAcceptanceRuntimeLocation(project, frozen)
	}
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, trustedAcceptanceKey{}, trustedAcceptanceContext{frozen: frozen}), nil
}

func acceptancePolicy(ctx context.Context) *frozenTrustedAcceptance {
	state, _ := ctx.Value(trustedAcceptanceKey{}).(trustedAcceptanceContext)
	return state.frozen
}

// wrapTrustedAcceptanceCommand adds a read-only workspace and hides the policy
// asset. Expected results remain solely in the parent process. HOME/tool caches
// use a private tmpfs so a candidate cannot leave host-visible state for the next
// case. No host-execution escape hatch can authorize this driver.
func wrapTrustedAcceptanceCommand(bwrapPath, workspace string, policy *frozenTrustedAcceptance) (string, []string) {
	command, args := wrapVerificationCommand(bwrapPath, workspace, policy.commandPath, policy.spec.Args, false, nil)
	// Existing wrapper args end with the command and its arguments. Inserting
	// before that suffix retains all namespace, HOME and credential masks.
	cut := len(args) - len(policy.spec.Args) - 1
	wrapped := append([]string(nil), args[:cut]...)
	wrapped = append(wrapped,
		"--ro-bind", workspace, workspace,
		"--tmpfs", filepath.Join(workspace, filepath.FromSlash(sandboxHomeRelPath)),
		"--setenv", "PYTHONDONTWRITEBYTECODE", "1",
		"--setenv", "PYTHONPYCACHEPREFIX", "/tmp/makewand-bytecode",
		"--setenv", "GOCACHE", "/tmp/makewand-go-cache",
		"--setenv", "CARGO_TARGET_DIR", "/tmp/makewand-cargo-target",
	)
	if policy.hiddenFile != "" {
		wrapped = append(wrapped, "--ro-bind", os.DevNull, policy.hiddenFile)
	}
	wrapped = append(wrapped, args[cut:]...)
	return command, wrapped
}

func (p *Project) runTrustedAcceptance(ctx context.Context, policy *frozenTrustedAcceptance, workspaceDigest, artifactDigest string) (*TrustedAcceptanceRecord, string, bool) {
	if policy == nil {
		return nil, "", false
	}
	env, err := resolveVerifyExecEnvironment(UnsafeHostExecAuthorization{})
	if err != nil || env.mode != verifyExecIsolated {
		return nil, "trusted acceptance requires working sandbox isolation; host execution cannot grant Strength 2", true
	}
	actualRuntime, runtimeErr := trustedCommandDigest(policy.commandPath)
	if runtimeErr != nil || actualRuntime != policy.commandDigest {
		return nil, "trusted acceptance runtime changed before execution", true
	}
	if err := os.MkdirAll(filepath.Join(p.Path, filepath.FromSlash(sandboxHomeRelPath)), 0o700); err != nil {
		return nil, fmt.Sprintf("create trusted acceptance sandbox home: %v", err), true
	}
	ctx, cancel := context.WithTimeout(ctx, policy.timeout)
	defer cancel()
	for _, test := range policy.spec.Cases {
		result, err := p.runTrustedAcceptanceCase(ctx, env.bwrapPath, policy, test.Stdin)
		if err != nil {
			return nil, fmt.Sprintf("trusted acceptance case %q could not execute: %v", test.Name, err), true
		}
		if isBwrapSetupFailure(result) {
			return nil, fmt.Sprintf("trusted acceptance sandbox could not start: %s", firstLine(result.Stderr)), true
		}
		if result.StdoutDroppedBytes > 0 || result.StderrDroppedBytes > 0 || len(result.Stdout)+len(result.Stderr) > policy.spec.MaxOutputBytes {
			return nil, fmt.Sprintf("trusted acceptance case %q exceeded the output limit", test.Name), false
		}
		if result.ExitCode != test.ExitCode || result.Stdout != test.Stdout || result.Stderr != test.Stderr {
			// Never echo expected values into logs that a later candidate sees.
			return nil, fmt.Sprintf("trusted acceptance case %q did not match the required behavior", test.Name), false
		}
	}
	actualRuntime, runtimeErr = trustedCommandDigest(policy.commandPath)
	if runtimeErr != nil || actualRuntime != policy.commandDigest {
		return nil, "trusted acceptance runtime changed during execution", true
	}
	return &TrustedAcceptanceRecord{
		Schema: 1, Driver: "makewand-stdio-v1", SpecDigest: policy.digest,
		WorkspaceDigest: workspaceDigest, ArtifactDigest: artifactDigest,
		CasesPassed: len(policy.spec.Cases), Isolated: true,
	}, "", false
}

func (p *Project) runTrustedAcceptanceCase(ctx context.Context, bwrapPath string, policy *frozenTrustedAcceptance, input string) (*ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy.spec.Command == "go" {
		if err := checkSandboxGoToolchain(p.Path, verifyHomeLayout(p.Path, sandboxPathEnv()).goVersion); err != nil {
			return nil, err
		}
	}
	command, args := wrapTrustedAcceptanceCommand(bwrapPath, p.Path, policy)
	cmd := exec.Command(command, args...)
	cmd.Dir = p.Path
	// bwrap --clearenv constructs the candidate environment. No expected
	// values, policy paths or provider secrets are passed to the child.
	cmd.Env = []string{"PATH=" + sandboxPathEnv()}
	cmd.Stdin = strings.NewReader(input)
	setProcessGroup(cmd)
	cmd.WaitDelay = time.Second
	capture := processjob.NewCapture(policy.spec.MaxOutputBytes, func() { killProcessGroup(cmd) })
	cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
	started := time.Now()
	cleanupProcess, err := processjob.Start(cmd)
	if err != nil {
		return nil, err
	}
	defer cleanupProcess()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessGroup(cmd)
		case <-done:
		}
	}()
	waitErr := cmd.Wait()
	close(done)
	// Even a normally exiting leader must not leave writers or descendants
	// alive. bwrap's PID namespace also reaps candidate descendants on exit.
	killProcessGroup(cmd)
	stdoutDropped, stderrDropped := capture.DroppedBytes()
	result := &ExecResult{Stdout: capture.StdoutString(), Stderr: capture.StderrString(), Duration: time.Since(started), StdoutDroppedBytes: stdoutDropped, StderrDroppedBytes: stderrDropped}
	if capture.Exceeded() {
		result.ExitCode = -1
		return result, &execution.UnknownOutcomeError{Err: fmt.Errorf("trusted acceptance output exceeded its combined %d-byte limit", policy.spec.MaxOutputBytes)}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return result, waitErr
		}
		result.ExitCode = exitErr.ExitCode()
	}
	return result, nil
}

func (p *Project) applyTrustedAcceptance(ctx context.Context, report *CandidateVerification, before, baseline string, files []ExtractedFile) {
	policy := acceptancePolicy(ctx)
	if policy == nil || report.IntegrityError != "" || report.IsolationError != "" || report.EnvironmentError || report.QuickCheckError != "" || report.DepsError != "" || report.TestsError != "" {
		return
	}
	// A missing local test plan may be replaced by explicitly configured
	// independent behavioral acceptance. Failed local checks are never bypassed.
	if !report.Passed && !report.NoTestPlan {
		return
	}
	if !report.Isolated {
		// A previous candidate-controlled local process running on the host
		// could already have read the oracle or tampered with external assets.
		// A later isolated black-box invocation cannot restore that boundary.
		report.Passed = false
		report.Strength = 0
		report.AcceptanceError = "trusted acceptance requires all candidate checks to remain isolated; unsafe host execution cannot grant Strength 2"
		report.EnvironmentError = true
		return
	}
	actual, digestErr := AcceptanceInputDigest(p.Path)
	if digestErr != nil || actual != before {
		report.Passed = false
		report.Strength = 0
		report.AcceptanceError = "local checks changed execution inputs outside the sealed candidate; include those inputs in the candidate and verify again"
		return
	}
	record, failure, environment := p.runTrustedAcceptance(ctx, policy, before, calculateFilesDigest(files))
	if failure != "" {
		report.Passed = false
		report.Strength = 0
		report.AcceptanceError = failure
		report.EnvironmentError = environment
		return
	}
	if record != nil {
		after, err := AcceptanceInputDigest(p.Path)
		if err != nil || after != before {
			report.Passed = false
			report.Strength = 0
			report.AcceptanceError = "trusted acceptance inputs changed during execution"
			return
		}
		record.BaselineDigest = baseline
		if err := authorizeAcceptanceRecord(record); err != nil {
			report.Passed = false
			report.Strength = 0
			report.AcceptanceError = err.Error()
			report.EnvironmentError = true
			return
		}
		report.Passed = true
		report.Strength = 2
		report.NoTestsRan = false
		report.Acceptance = record
		report.EvidenceLimit = "independent acceptance passed for the configured behavioral cases"
	}
}

// AcceptanceInputDigest covers regular workspace execution inputs, including
// dependency/cache-named directories. Git administration and the sandbox HOME
// are excluded: the former is never imported, the latter is freshly masked for
// every independent case. Symlinks/special files fail closed rather than leave
// unsealed targets outside this tree.
func AcceptanceInputDigest(root string) (string, error) {
	tree, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer tree.Close()
	h := sha256.New()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error { //nolint:gosec // Enumerate the user-selected workspace; file contents are read through the held os.Root below.
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.Name() == ".git" || filepath.ToSlash(rel) == sandboxHomeRelPath {
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
			return fmt.Errorf("trusted acceptance input is not a regular file: %s", rel)
		}
		f, err := tree.Open(rel)
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = f.Close()
			return fmt.Errorf("trusted acceptance input changed during reading: %s", rel)
		}
		fmt.Fprintf(h, "%s\x00%03o\x00%d\x00", filepath.ToSlash(rel), opened.Mode().Perm(), opened.Size())
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

var acceptanceAuthority struct {
	once sync.Once
	key  [32]byte
	err  error
}

func acceptanceRecordMAC(record *TrustedAcceptanceRecord) ([]byte, error) {
	acceptanceAuthority.once.Do(func() {
		_, acceptanceAuthority.err = rand.Read(acceptanceAuthority.key[:])
	})
	if acceptanceAuthority.err != nil {
		return nil, fmt.Errorf("initialize trusted acceptance authority: %w", acceptanceAuthority.err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, acceptanceAuthority.key[:])
	_, _ = mac.Write(encoded)
	return mac.Sum(nil), nil
}

func authorizeAcceptanceRecord(record *TrustedAcceptanceRecord) error {
	mac, err := acceptanceRecordMAC(record)
	if err == nil {
		record.authority = mac
	}
	return err
}

// ValidateTrustedAcceptance must run immediately before automatic application.
// It rejects imported/fabricated records, changed policy, stale baseline inputs
// and altered delivered files. The record authorizes one exact input state;
// human approval may still choose an unverified candidate through a separate
// path. A caller's application transaction must prevent concurrent writers.
func (p *Project) ValidateTrustedAcceptance(ctx context.Context, files []ExtractedFile, record *TrustedAcceptanceRecord) error {
	if record == nil || record.Schema != 1 || record.Driver != "makewand-stdio-v1" || !record.Isolated || record.CasesPassed < 1 || record.WorkspaceDigest == "" || record.BaselineDigest == "" {
		return fmt.Errorf("independent acceptance record is missing or incomplete")
	}
	mac, err := acceptanceRecordMAC(record)
	if err != nil || !hmac.Equal(record.authority, mac) {
		return fmt.Errorf("independent acceptance record was not issued by this trusted parent")
	}
	ctx, err = prepareTrustedAcceptance(ctx, p)
	if err != nil {
		return err
	}
	policy := acceptancePolicy(ctx)
	if policy == nil || policy.digest != record.SpecDigest || len(policy.spec.Cases) != record.CasesPassed {
		return fmt.Errorf("trusted acceptance policy changed after verification")
	}
	commandDigest, err := trustedCommandDigest(policy.commandPath)
	if err != nil || commandDigest != policy.commandDigest {
		return fmt.Errorf("trusted acceptance runtime changed after verification")
	}
	if !VerifiedFilesMatch(files, record.ArtifactDigest) {
		return fmt.Errorf("candidate payload changed after independent acceptance")
	}
	baseline, err := AcceptanceInputDigest(p.Path)
	if err != nil {
		return err
	}
	if baseline != record.BaselineDigest {
		return fmt.Errorf("workspace inputs changed after independent acceptance; verify again")
	}
	return nil
}

func validateAcceptanceRuntimeLocation(project *Project, policy *frozenTrustedAcceptance) error {
	if project == nil || policy == nil {
		return nil
	}
	root, err := filepath.EvalSymlinks(project.Path)
	if err != nil {
		return err
	}
	if pathWithin(root, policy.commandPath) {
		return fmt.Errorf("trusted acceptance runtime must be outside the project")
	}
	return nil
}

func trustedCommandDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open trusted acceptance runtime: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("trusted acceptance runtime must be a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("read trusted acceptance runtime: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// TrustedAcceptanceRecordMatches cheaply checks parent authority and payload
// binding for UI approval routing. It does not replace the transaction's
// ValidateTrustedAcceptance check of the current policy and full baseline.
func TrustedAcceptanceRecordMatches(files []ExtractedFile, record *TrustedAcceptanceRecord) bool {
	if record == nil || record.Schema != 1 || record.Driver != "makewand-stdio-v1" || !record.Isolated || record.CasesPassed < 1 || record.WorkspaceDigest == "" || record.BaselineDigest == "" {
		return false
	}
	mac, err := acceptanceRecordMAC(record)
	return err == nil && hmac.Equal(record.authority, mac) && VerifiedFilesMatch(files, record.ArtifactDigest)
}
