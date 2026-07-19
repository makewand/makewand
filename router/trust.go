// trust.go — Untrusted-repository capability routing.
//
// When makewand processes an untrusted third-party repository, the generation
// stage must not hand repo content to a provider that runs a repo-aware agent
// process on the host (a confused-deputy risk: instruction files, .mcp.json, or
// prompt injection can steer that host-privileged agent). This file adds a
// capability model — RepoTrust plus the UntrustedRepoCapable interface — and the
// fail-closed routing enforcement that only lets direct HTTP/API providers run
// against an untrusted repo.
package router

import (
	"errors"
	"strings"
	"time"
)

// RepoTrust controls whether repository content is treated as trusted input to
// the generation provider. Untrusted mode only routes to providers that do not
// run a repo-aware agent process on the host.
//
// Backed by int32 so it stores/loads through the Router's atomic.Int32 without a
// widening conversion (the enum has only the two values below).
type RepoTrust int32

const (
	RepoTrustTrusted   RepoTrust = iota // default: existing behavior, all providers eligible
	RepoTrustUntrusted                  // only untrusted-repo-safe providers; fail closed otherwise
)

// String returns the canonical name for the trust level ("trusted"/"untrusted").
func (t RepoTrust) String() string {
	switch t {
	case RepoTrustUntrusted:
		return "untrusted"
	default:
		return "trusted"
	}
}

// ParseRepoTrust parses a trust level from a string. Matching is case-insensitive
// and the input is trimmed. It accepts "trusted" and "untrusted". An empty string
// is treated as the default (RepoTrustTrusted, true) so an unset flag/config value
// resolves to the safe existing behavior; any other unrecognized value returns
// (RepoTrustTrusted, false) so callers can reject it.
func ParseRepoTrust(s string) (RepoTrust, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "trusted":
		return RepoTrustTrusted, true
	case "untrusted":
		return RepoTrustUntrusted, true
	default:
		return RepoTrustTrusted, false
	}
}

// UntrustedRepoCapable is implemented by providers that can declare whether they
// are safe to use against an untrusted repository. A provider that does NOT
// implement this interface is treated as UNSAFE in untrusted mode (fail closed).
type UntrustedRepoCapable interface {
	SafeForUntrustedRepo() bool
}

// ErrNoUntrustedSafeProvider is returned when untrusted-repo mode is active but no
// untrusted-repo-safe provider is available for the requested task/route. Routing
// fails closed rather than falling back to a provider that runs a host agent.
var ErrNoUntrustedSafeProvider = errors.New(
	"untrusted repo mode: no untrusted-repo-safe provider available " +
		"(only direct API providers are allowed; configure an API-key provider " +
		"such as claude/gemini/openai API, or use --repo-trust=trusted)")

// providerSafeForUntrustedRepo reports whether a resolved provider value declares
// itself safe for an untrusted repository. Providers that do not implement
// UntrustedRepoCapable are treated as unsafe (fail closed). The check is a pure
// type assertion on an already-resolved provider value, so it holds no locks and
// makes no subprocess/network calls.
func providerSafeForUntrustedRepo(p Provider) bool {
	capable, ok := p.(UntrustedRepoCapable)
	return ok && capable.SafeForUntrustedRepo()
}

// SetRepoTrust sets the router's repository trust level. Safe to call
// concurrently with routing.
//
// Beyond storing the Router's own trust value, it propagates the change to two
// pieces of already-constructed state so a mid-flight switch to untrusted takes
// effect immediately rather than only for freshly built routers:
//
//   - the concrete quota snapshotter (if the Router holds one), so its next
//     background/forced refresh skips local repo-aware CLI quota probes; and
//   - the availability cache, invalidated so the next Available() re-probes and
//     drops any unsafe provider a prior trusted probe cached.
func (r *Router) SetRepoTrust(t RepoTrust) {
	r.repoTrust.Store(int32(t))

	// Propagate to the concrete quota snapshotter so a later refresh honors the new
	// trust (never execs a local repo-aware CLI to probe quota once untrusted). A
	// fake QuotaController execs nothing, so only *QuotaSnapshotter needs this; the
	// type assertion no-ops otherwise. r.quota is set once at construction and never
	// mutated afterward, so reading it here is race-free.
	if snap, ok := r.quota.(*QuotaSnapshotter); ok {
		snap.SetRepoTrust(t)
	}

	// Invalidate the availability cache so the very next Available() re-probes under
	// the new trust: switching to untrusted must immediately exclude any unsafe
	// provider a prior (trusted) probe cached. Guarded by r.mu, matching
	// RegisterProvider's cache-busting, so it is race-clean with concurrent routing.
	r.mu.Lock()
	r.cachedAvailAt = time.Time{}
	r.mu.Unlock()
}

// RepoTrust returns the router's current repository trust level. Safe to call
// concurrently with routing.
func (r *Router) RepoTrust() RepoTrust {
	return RepoTrust(r.repoTrust.Load())
}

// untrustedRepoSafe reports whether a resolved provider may be used under the
// current trust setting. In trusted mode it always returns true (routing is
// byte-for-byte unchanged); in untrusted mode only providers that implement
// UntrustedRepoCapable and return true pass.
func (r *Router) untrustedRepoSafe(p Provider) bool {
	if r.RepoTrust() != RepoTrustUntrusted {
		return true
	}
	return providerSafeForUntrustedRepo(p)
}

// untrustedRepoSkipDetail formats a trace detail for a provider excluded because
// it is not safe for an untrusted repository.
func untrustedRepoSkipDetail(name string) string {
	return "untrusted repo mode: " + name + " is not untrusted-repo-safe"
}
