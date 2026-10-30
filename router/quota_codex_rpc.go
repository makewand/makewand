package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/processjob"
	"github.com/makewand/makewand/internal/proxyconfig"
)

// This context flag disables only active metadata reads. Passive, account-bound
// session observations remain readable in an untrusted repository.
type quotaNoCLIContext struct{}

var codexAppServerHelp = regexp.MustCompile(`(?m)^\s*app-server\b`)

func quotaCodexExecutable() (string, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if real, e := filepath.EvalSymlinks(path); e == nil {
		path = real
	}
	// A repository-provided executable must not become an implicit host quota
	// reader. The actual command also runs in an empty temporary directory.
	cwd, _ := os.Getwd()
	for dir := cwd; dir != ""; dir = filepath.Dir(dir) {
		if _, e := os.Stat(filepath.Join(dir, ".git")); e == nil {
			if relative, e := filepath.Rel(dir, path); e == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", errors.New("repository-local Codex cannot read host quota")
			}
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return path, nil
}

func quotaCodexEnv(root string) ([]string, error) {
	defaults, err := proxyconfig.Load()
	if err != nil {
		return nil, err
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "CODEX_HOME=") {
			env = append(env, entry)
		}
	}
	return proxyconfig.Apply(append(env, "CODEX_HOME="+root), defaults), nil
}

func readCodexQuotaRPC(ctx context.Context, identity ProviderQuota) (ProviderQuota, error) {
	q := identity
	if !q.IdentityKnown || ctx.Value(quotaNoCLIContext{}) == true {
		return q, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	bin, err := quotaCodexExecutable()
	if err != nil {
		return q, err
	}
	// Compare the opaque credential image after the read as well as the plain
	// account binding. No token field is decoded, logged, hashed or persisted.
	authPath := filepath.Join(q.CredentialRoot, "auth.json")
	authInfo, err := cliInspectIdentityMetadata(authPath)
	if err != nil {
		return q, err
	}
	auth, err := cliReadIdentityMetadata(authPath)
	if err != nil || quotaAccountRef("codex", quotaAccountID("codex", auth)) != q.AccountRef {
		return identity, errors.New("codex login changed before quota read")
	}
	dir, err := os.MkdirTemp("", "makewand-codex-quota-")
	if err != nil {
		return q, err
	}
	defer os.RemoveAll(dir)
	env, err := quotaCodexEnv(q.CredentialRoot)
	if err != nil {
		return q, err
	}
	// Old CLI releases can interpret unknown subcommands as prompts. A help
	// read must advertise app-server before any protocol command is launched.
	help := exec.CommandContext(ctx, bin, "--help")
	help.Dir, help.Env, help.WaitDelay = dir, env, time.Second
	setCLIProcessGroup(help)
	help.Cancel = func() error { killCLIProcess(help); return nil }
	helpCapture := processjob.NewCapture(64<<10, func() { killCLIProcess(help) })
	help.Stdout, help.Stderr = helpCapture.Stdout(), helpCapture.Stderr()
	helpCleanup, err := processjob.StartSandboxed(help)
	if err != nil {
		return q, err
	}
	err = help.Wait()
	killCLIProcess(help)
	helpCleanup()
	if err != nil || helpCapture.Exceeded() || !codexAppServerHelp.Match(helpCapture.StdoutBytes()) {
		return q, errors.New("codex does not advertise read-only app-server metadata")
	}
	cmd := exec.CommandContext(ctx, bin, "app-server", "--listen", "stdio://")
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, time.Second
	setCLIProcessGroup(cmd)
	cmd.Cancel = func() error { killCLIProcess(cmd); return nil }
	input, err := cmd.StdinPipe()
	if err != nil {
		return q, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return q, err
	}
	stderr := processjob.NewCapture(64<<10, func() { killCLIProcess(cmd) })
	cmd.Stderr = stderr.Stderr()
	cleanup, err := processjob.StartSandboxed(cmd)
	if err != nil {
		_ = input.Close()
		_ = output.Close()
		return q, err
	}
	defer cleanup()
	defer killCLIProcess(cmd)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer func() { _ = input.Close(); killCLIProcess(cmd); <-waited }()
	encoder := json.NewEncoder(input)
	decoder := json.NewDecoder(io.LimitReader(output, 1<<20))
	query := func(method string, id int, params any) (json.RawMessage, error) {
		if err := encoder.Encode(map[string]any{"method": method, "id": id, "params": params}); err != nil {
			return nil, err
		}
		for n := 0; n < 128; n++ {
			var response struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := decoder.Decode(&response); err != nil {
				return nil, err
			}
			if response.ID != nil && *response.ID == id {
				if len(response.Error) > 0 || len(response.Result) == 0 {
					return nil, errors.New("codex quota metadata request refused")
				}
				return response.Result, nil
			}
		}
		return nil, errors.New("codex quota metadata exceeded notification limit")
	}
	if _, err := query("initialize", 1, map[string]any{"clientInfo": map[string]string{"name": "makewand_quota", "version": "1"}}); err != nil {
		return q, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return q, err
	}
	// This fixed whitelist contains no thread/turn, login, credit or tool method.
	raw, err := query("account/rateLimits/read", 2, map[string]any{})
	if err != nil {
		return q, err
	}
	after, err := cliReadIdentityMetadata(authPath)
	afterInfo, infoErr := cliInspectIdentityMetadata(authPath)
	if err != nil || infoErr != nil || !quotaAuthVersionEqual(authInfo, afterInfo) ||
		!bytes.Equal(auth, after) || !quotaMatches(q, readQuotaIdentity("codex", q.CredentialRoot)) {
		return identity, errors.New("codex login changed during quota read")
	}
	return parseCodexQuotaRPC(raw, q, time.Now())
}

// Restored bytes alone cannot detect an A -> B -> A login switch while the
// server is reading credentials. Include file identity and version metadata.
func quotaAuthVersionEqual(before, after os.FileInfo) bool {
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return false
	}
	dev, ino, changed, versioned := quotaIdentityFileVersion(before)
	nextDev, nextIno, nextChanged, nextVersioned := quotaIdentityFileVersion(after)
	return versioned == nextVersioned && (!versioned || (dev == nextDev && ino == nextIno && changed.Equal(nextChanged)))
}

func parseCodexQuotaRPC(raw []byte, identity ProviderQuota, observed time.Time) (ProviderQuota, error) {
	type window struct {
		UsedPercent *float64 `json:"usedPercent"`
		Minutes     int      `json:"windowDurationMins"`
		ResetsAt    int64    `json:"resetsAt"`
	}
	type bucket struct {
		LimitID   string  `json:"limitId"`
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
	}
	var response struct {
		Default *bucket           `json:"rateLimits"`
		Buckets map[string]bucket `json:"rateLimitsByLimitId"`
	}
	q := identity
	if err := json.Unmarshal(raw, &response); err != nil {
		return q, errors.New("invalid Codex quota metadata")
	}
	if len(response.Buckets) == 0 && response.Default != nil {
		id := response.Default.LimitID
		if id == "" {
			id = "codex" // backward-compatible single-bucket response
		}
		response.Buckets = map[string]bucket{id: *response.Default}
	}
	if len(response.Buckets) > 32 {
		return q, errors.New("too many Codex quota buckets")
	}
	for id, b := range response.Buckets {
		if id == "" || len(id) > 200 || (b.LimitID != "" && b.LimitID != id) {
			continue
		}
		for _, w := range []*window{b.Primary, b.Secondary} {
			if w == nil || w.UsedPercent == nil || w.Minutes <= 0 || w.Minutes > 525600 {
				continue
			}
			reset := time.Unix(w.ResetsAt, 0)
			if !reset.After(observed) {
				continue
			}
			scope, key := "account", "|"+strconv.Itoa(w.Minutes)
			if id != "codex" {
				// A metered bucket ID is not a model ID. Retain unknown scope;
				// never promote it to an account-wide or guessed model guard.
				scope, key = "unknown", "bucket:"+id+"|"+strconv.Itoa(w.Minutes)
			}
			q.Windows = append(q.Windows, QuotaWindow{PoolRef: q.PoolRef, LimitID: id, Scope: scope,
				WindowKey: key, WindowMinutes: w.Minutes, UsedPercent: *w.UsedPercent, SourceAt: observed, ResetAt: reset})
		}
	}
	q.Authed, q.SourceAt = true, observed
	return currentQuotaWindows(q), nil
}
