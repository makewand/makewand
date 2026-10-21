package router

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
		b.Files[canonicalQuotaRoot(path)] = fingerprintCredential(path, raw, err == nil)
	}
	return b, nil
}

// fingerprintCredential binds one credential file of a login: the account it
// names (credentialAccountRef) and, for a token file, the hash of its bytes.
func fingerprintCredential(path string, raw []byte, exists bool) credentialFingerprint {
	f := credentialFingerprint{Exists: exists}
	if !exists {
		return f
	}
	f.IdentityRef = credentialAccountRef(path, raw)
	// Only the login projection of .claude.json binds state; projects and
	// counters change on normal Claude Code startup and must not invalidate
	// admission.
	if filepath.Base(path) != ".claude.json" {
		f.Sum = sha256.Sum256(raw)
	}
	return f
}

// admits reports whether got, a later read of the file bound as f, still
// holds the admitted login: the file still exists or is still missing, it
// names the same account, and its tokens are the bound ones unless the file
// names that account itself. A codex refresh beside the account_id it keeps is
// provably the same account's. A token file that names no account cannot
// prove whose tokens it holds: Claude keeps its tokens apart from the
// accountUuid in .claude.json, and another account's tokens can land beside
// an identity file that does not show the switch yet. A refresh by the lease
// holder a call waited for is no change: the call binds its login once it
// holds the lease (bindLeasedCredentials).
func (f credentialFingerprint) admits(got credentialFingerprint) bool {
	return f.Exists == got.Exists && f.IdentityRef == got.IdentityRef && (f.Sum == got.Sum || f.IdentityRef != "")
}

// admits reports whether now, a later read of the selected login, still holds
// the login b bound (credentialFingerprint.admits for each of its files).
func (b *credentialBinding) admits(now *credentialBinding) bool {
	if now == nil || b.Root != now.Root || len(b.Files) != len(now.Files) {
		return false
	}
	for path, f := range b.Files {
		if got, ok := now.Files[path]; !ok || !f.admits(got) {
			return false
		}
	}
	return true
}

// credentialAccountRef is the account reference of the login file at path
// (codex auth.json, Claude .claude.json; quotaAccountID), "" for none.
func credentialAccountRef(path string, raw []byte) string {
	switch filepath.Base(path) {
	case "auth.json":
		return quotaAccountRef("codex", quotaAccountID("codex", raw))
	case ".claude.json":
		return quotaAccountRef("claude", quotaAccountID("claude", raw))
	}
	return ""
}

// accountRef is the bound login's account reference as readQuotaIdentity
// gives it: the account the login names, or its credential root's unknown one.
func (b *credentialBinding) accountRef(provider string) string {
	for _, f := range b.Files {
		if f.IdentityRef != "" {
			return f.IdentityRef
		}
	}
	return unknownQuotaIdentity(provider, b.Root).AccountRef
}

func (b *credentialBinding) verify(path string, raw []byte, exists bool) error {
	if b == nil {
		return nil
	}
	expected, ok := b.Files[canonicalQuotaRoot(path)]
	if !ok {
		switch filepath.Base(path) {
		case "auth.json", ".credentials.json", ".claude.json":
			return fmt.Errorf("credential source differs from admitted account; %w", errLoginSwitch)
		}
		return nil
	} // auxiliary caches do not establish a login
	if !expected.admits(fingerprintCredential(path, raw, exists)) {
		return fmt.Errorf("selected login changed after admission; %w", errLoginSwitch)
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

// bindLeasedCredentials binds ctx to provider's login as it is once the attempt
// holds its account lease: the lease holder it waited for may have refreshed
// that login's tokens, and launch preparation compares against the current
// ones. admitted is the login's account reference at admission
// (readQuotaIdentity); another account now is a login switch during the wait,
// refused before launch.
func bindLeasedCredentials(ctx context.Context, provider, admitted string) (context.Context, error) {
	ctx, err := bindCredentialAdmission(ctx, provider)
	if err != nil {
		return ctx, fmt.Errorf("%w: %w", errExecutionAccounting, err)
	}
	if b, _ := ctx.Value(credentialBindingKey{}).(*credentialBinding); b != nil && b.accountRef(provider) != admitted {
		return ctx, fmt.Errorf("%w: selected login changed while waiting for its account lease; %w", errExecutionAdmission, errLoginSwitch)
	}
	return ctx, nil
}

// verifyCredentialBinding reads provider's selected login again and refuses
// one that no longer holds the login ctx bound (credentialBinding.admits).
func verifyCredentialBinding(ctx context.Context, provider string) error {
	b, _ := ctx.Value(credentialBindingKey{}).(*credentialBinding)
	if b == nil {
		return nil
	}
	now, err := readCredentialBinding(provider)
	if err != nil {
		return err
	}
	if !b.admits(now) {
		return fmt.Errorf("selected login changed after admission; %w", errLoginSwitch)
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
		return fmt.Errorf("credential source differs from admitted account; %w", errLoginSwitch)
	}
	// Preserve user configuration, instructions, hooks and Claude's Keychain
	// namespace. Host execution checks account attribution again after Wait.
	beforeBinding, err := readCredentialBinding(provider)
	if err != nil {
		return err
	}
	if admitted, _ := ctx.Value(credentialBindingKey{}).(*credentialBinding); admitted != nil && !admitted.admits(beforeBinding) {
		return fmt.Errorf("selected login changed after admission; %w", errLoginSwitch)
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
