package router

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"weak"

	"github.com/makewand/makewand/execution"
)

type credentialBindingKey struct{}
type credentialFingerprint struct {
	Exists      bool
	Sum         [32]byte
	IdentityRef string
}
type credentialBinding struct {
	Root  string
	Files map[string]credentialFingerprint
}

// Fingerprints live only in this invocation; they are never ledger/evidence fields.
func readCredentialBinding(provider string) (*credentialBinding, error) {
	if provider != "codex" && provider != "claude" {
		return nil, nil
	}
	root := canonicalQuotaRoot(selectedQuotaRoot(provider))
	paths := []string{filepath.Join(root, "auth.json")}
	if provider == "claude" {
		identity := filepath.Join(root, ".claude.json")
		if home, _ := os.UserHomeDir(); root == canonicalQuotaRoot(filepath.Join(home, ".claude")) {
			identity = filepath.Join(home, ".claude.json")
		}
		paths = []string{filepath.Join(root, ".credentials.json"), identity}
	}
	b := &credentialBinding{Root: root, Files: map[string]credentialFingerprint{}}
	for _, path := range paths {
		read := cliReadRegularFile
		if filepath.Base(path) == ".claude.json" {
			read = cliReadIdentityMetadata
		}
		raw, err := read(path)
		// Unavailable credentials are unknown, just like unavailable identity
		// metadata. Copy-time verification still detects a later readable login.
		f := fingerprintCredential(path, raw, err == nil)
		b.Files[canonicalQuotaRoot(path)] = f
	}
	return b, nil
}
func fingerprintCredential(path string, raw []byte, exists bool) credentialFingerprint {
	f := credentialFingerprint{Exists: exists}
	if !exists {
		return f
	}
	if filepath.Base(path) == ".claude.json" {
		// Only the login projection binds state; projects and counters change
		// on normal Claude Code startup and must not invalidate admission.
		var identity struct {
			OAuthAccount struct {
				AccountUUID string `json:"accountUuid"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(raw, &identity) == nil {
			f.IdentityRef = quotaAccountRef("claude", identity.OAuthAccount.AccountUUID)
		}
	} else {
		f.Sum = sha256.Sum256(raw)
	}
	return f
}
func (b *credentialBinding) verify(path string, raw []byte, exists bool) error {
	if b == nil {
		return nil
	}
	expected, ok := b.Files[canonicalQuotaRoot(path)]
	if !ok {
		switch filepath.Base(path) {
		case "auth.json", ".credentials.json", ".claude.json":
			return errors.New("credential source differs from admitted account")
		}
		return nil
	} // auxiliary caches do not establish a login
	got := fingerprintCredential(path, raw, exists)
	if got != expected {
		return errors.New("selected login changed after admission; refusing account switch")
	}
	return nil
}
func bindCredentialAdmission(ctx context.Context, provider string) (context.Context, error) {
	b, err := readCredentialBinding(provider)
	if err != nil {
		return ctx, err
	}
	if b == nil {
		return ctx, nil
	}
	return context.WithValue(ctx, credentialBindingKey{}, b), nil
}
func verifyCredentialBinding(ctx context.Context, provider string) error {
	b, _ := ctx.Value(credentialBindingKey{}).(*credentialBinding)
	if b == nil {
		return nil
	}
	now, err := readCredentialBinding(provider)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(b, now) {
		return errors.New("selected login changed after admission; refusing account switch")
	}
	return nil
}

var cliDirectAccountChecks sync.Map

func freezeDirectCLIAccount(ctx context.Context, provider string, cmd *exec.Cmd) error {
	if provider != "codex" && provider != "claude" {
		return nil
	}
	if err := verifyCredentialBinding(ctx, provider); err != nil {
		return err
	}
	home := cliEnvValue(cmd.Env, "HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	variable := "CODEX_HOME"
	if provider == "claude" {
		variable = "CLAUDE_CONFIG_DIR"
	}
	selected, real, err := cliConfiguredAccountHome(provider, variable, cmd.Env, cmd.Dir, home)
	if err != nil {
		return err
	}
	if selected == "" {
		real = canonicalQuotaRoot(filepath.Join(home, "."+provider))
	}
	if binding, _ := ctx.Value(credentialBindingKey{}).(*credentialBinding); binding != nil && binding.Root != canonicalQuotaRoot(real) {
		return errors.New("credential source differs from admitted account")
	}
	// Preserve user configuration, instructions, hooks and Claude's Keychain
	// namespace. Host execution checks account attribution again after Wait.
	beforeBinding, err := readCredentialBinding(provider)
	if err != nil {
		return err
	}
	if admitted, _ := ctx.Value(credentialBindingKey{}).(*credentialBinding); admitted != nil && !reflect.DeepEqual(admitted, beforeBinding) {
		return errors.New("selected login changed after admission; refusing account switch")
	}
	before := readQuotaIdentity(provider, real)
	key := weak.Make(cmd)
	cliDirectAccountChecks.Store(key, func() error {
		after := readQuotaIdentity(provider, selectedQuotaRoot(provider))
		if before.AccountRef != after.AccountRef || before.CredentialRoot != after.CredentialRoot {
			return &execution.UnknownOutcomeError{Err: errors.New("selected login changed during direct execution")}
		}
		if !before.IdentityKnown {
			afterBinding, err := readCredentialBinding(provider)
			if err != nil || !reflect.DeepEqual(beforeBinding, afterBinding) {
				return &execution.UnknownOutcomeError{Err: errors.New("credentials changed during direct execution without a known account identity")}
			}
		}
		return nil
	})
	runtime.AddCleanup(cmd, func(key weak.Pointer[exec.Cmd]) { cliDirectAccountChecks.Delete(key) }, key)
	return nil
}

func verifyDirectCLIAccount(cmd *exec.Cmd) error {
	if check, ok := cliDirectAccountChecks.LoadAndDelete(weak.Make(cmd)); ok {
		return check.(func() error)()
	}
	return nil
}
