# router — Multi-Provider AI Routing Library

`router` is a standalone Go package for intelligent multi-provider AI routing.
It provides Thompson Sampling–based adaptive selection, circuit breaker fault
tolerance, ensemble+judge evaluation, and an OpenAI-compatible subset HTTP facade.

## Quick Start

```go
import "github.com/makewand/makewand/router"

// Create a router with pre-constructed providers
r, err := router.NewRouterFromConfig(router.RouterConfig{
    Providers: map[string]router.ProviderEntry{
        "claude": {
            Provider: router.NewClaudeCLI("/usr/local/bin/claude"),
            Access:   router.AccessSubscription,
        },
        "gemini": {
            Provider: router.NewGeminiCLI("/usr/local/bin/gemini"),
            Access:   router.AccessSubscription,
        },
    },
    UsageMode: "balanced", // "fast", "balanced", or "power"
    ConfigDir: "",         // optional: directory holding routing.json overrides
})
if err != nil {
    // Invalid routing.json overrides or unusable embedded defaults.
    log.Fatal(err)
}

// Route and chat
content, usage, result, err := r.Chat(ctx, router.TaskCode,
    []router.Message{{Role: "user", Content: "Write a hello world"}}, "")
```

## Features

### Adaptive Routing (Thompson Sampling)

Each provider accumulates quality signals (successes/failures) per build phase.
A Beta distribution is sampled to rank providers, allowing observed performance
to gradually override the static strategy table order.

Only recorded quality outcomes count as evidence — request counts do not.
Ordinary `Chat`/`ChatStream`/HTTP traffic never calls `RecordQualityOutcome`,
so without it the static strategy order is kept as-is. Two candidates are only
reordered by sampling once at least one of them has 3 or more quality outcomes
for the phase; below that, their static order stands.

```go
// Record quality outcomes to influence future routing
r.RecordQualityOutcome(router.PhaseCode, "claude", true)  // success
r.RecordQualityOutcome(router.PhaseCode, "gemini", false) // failure
```

### Circuit Breaker

Providers that fail repeatedly are temporarily excluded:

- Timeout/deadline errors trip the circuit immediately
- Other errors require reaching a configurable threshold
- After the cooldown exactly one half-open probe request is admitted; only
  that probe's success closes the circuit. A late success from a request that
  was already in flight when the circuit opened is ignored, so it cannot cut
  the cooldown short.

### Ensemble + Judge (Power Mode)

In Power mode, multiple providers generate responses in parallel.
A cross-model judge selects the best output:

```go
content, usage, result, err := r.ChatBest(ctx, router.PhaseCode, messages, system)
```

### OpenAI-Compatible Subset HTTP Facade

Expose your router as an OpenAI-compatible API server. Bind to loopback and
require a token. `HTTPHandler()` with no options serves every endpoint
**without authentication**:

```go
r, err := router.NewRouterFromConfig(rc)
if err != nil {
    log.Fatal(err)
}
h := r.HTTPHandler(router.HTTPHandlerOptions{
    BearerToken: os.Getenv("MAKEWAND_TOKEN"), // clients send "Authorization: Bearer <token>"
})
log.Fatal(http.ListenAndServe("127.0.0.1:8080", h))
```

Endpoints:
- `POST /v1/chat/completions` — Chat completions (`stream=true` supported)
- `POST /v1/responses` — Responses API subset (`stream=true` supported)
- `GET /v1/models` — List available providers
- `GET /health` — Health check

The HTTP facade accepts a provider name from `/v1/models` in the `model` field
to force a specific provider. It also accepts common alias families such as
`gpt-*`, `o1*`, `o3*`, and `o4*` for Codex-backed routing, plus `claude*`,
`gemini*`, and `codex*`. `max_tokens` and `temperature` are accepted but
currently ignored for compatibility. `response_format` supports `json_object`
and a pragmatic subset of `json_schema`; `tools` and `tool_choice` provide a
basic function-calling compatibility layer. When wrapped by `makewand serve`,
each response also carries `X-Request-Id` for tracing and can be paired with
the server-side usage ledger, admin APIs, embedded `/admin` console, and
`/metrics` endpoint.

#### Authentication (`HTTPHandlerOptions`)

| Option | Effect |
|--------|--------|
| none (`HTTPHandler()`) | **No authentication.** Anyone who can reach the listener can call every endpoint. Use it only on a trusted loopback socket, or behind your own auth middleware. |
| `BearerToken` | One shared token. Requests need `Authorization: Bearer <token>`, and the token grants every scope. |
| `Authorizer` | A `serverauth.RequestAuthorizer` (for example `serverauth.NewAuthorizer`, or the SQLite token store) with per-token scopes (`chat:invoke` for `/v1/chat/completions` and `/v1/responses`, `models:read` for `/v1/models`), provider allowlists, allowed modes, rate limits and cost budgets. It takes precedence over `BearerToken`. |

`GET /health` is always unauthenticated. Other options: `AuditLogger`,
`UsageLogger`, `UsageReader`/`TeamStore` (org/project budgets),
`BudgetReservationUSD`, `StrictAccounting` and `StatsDir`. They are documented
on the `HTTPHandlerOptions` type. `makewand serve` always configures an
authorizer and refuses to start without one.

#### Security model: remote messages drive local CLIs

When the router has local CLI providers (`NewClaudeCLI`, `NewCodexCLI`,
`NewGeminiCLI`, `NewAgyCLI`, `NewCommandCLI`), **every chat message from a
token holder runs that CLI on the serving host**. The CLI runs as the server's
OS user and uses that user's credentials, environment and subscription quota.
The provider CLIs are agents. makewand starts `claude -p` without any
tool-restriction flags, `gemini` with `--sandbox false`, and only `codex exec`
in a read-only sandbox. A remote prompt, including prompt injection in content
it forwards, can therefore steer the agent into reading files the server user
can reach and including them in the reply.

The facade limits exposure to host state. Every HTTP request is marked with
`router.ContextWithRemoteOrigin`, and for such requests:

- review requests never run `codex review --uncommitted` (a command that
  ignores the prompt and reviews the server's working tree). Codex runs the
  caller's prompt with `codex exec` in a read-only sandbox instead;
- CLIs never inherit the server process's working directory. Without an
  explicit `router.ContextWithWorkDir`, each CLI invocation runs in its own
  new, empty, private temporary directory, which is deleted afterwards.

The agents still run with the server user's privileges. For tokens you hand to
anyone you would not give a shell account:

- serve **API providers** only (`NewClaude`, `NewGemini`, `NewOpenAI`, or
  `NewRemoteHTTP`). They send prompts over HTTP, to a vendor API or, for
  `NewRemoteHTTP`, to another makewand server, and execute nothing locally;
- or run the server under a dedicated low-privilege user, container or VM with
  no access to your source trees or credentials.

If you expose `Router.Chat` & co. through your own network front-end instead of
`HTTPHandler`, wrap each request context with `router.ContextWithRemoteOrigin`
to get the same isolation.

### Strategy Hot-Reload (deprecated)

> **Deprecated.** `WatchOverrides` is a library-only opt-in with no makewand
> entrypoint wired to it, and is scheduled for removal in v0.3. The CLI and
> server load `routing.json` once at startup; restart to apply changes. New
> embedders should not depend on it.

Library embedders that still want polling can watch `routing.json` and merge
updates without restart:

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
r.WatchOverrides(ctx, configDir) // deprecated; removed in v0.3
```

Reloads apply only to this Router instance. Each reload re-applies
`routing.json` over the built-in defaults, so deleting a field (or the whole
file) reverts it to the default. Every candidate is validated before it is
swapped in. An invalid `routing.json` keeps the previous tables active and
emits a `reload_error` trace event.

### Provider Factories

Register factories for dynamic model-specific provider construction:

```go
r.RegisterProviderFactory("claude", func(modelID string) (router.Provider, error) {
    return router.NewClaude(apiKey, modelID), nil
})
```

The instances a factory builds are cached per `(provider, model)` on the Router
and shared with its per-request views, such as HTTP requests that carry a
`mode` or a token provider allowlist. Each model therefore gets one provider,
and one HTTP transport, reused across requests. Calling
`RegisterProviderFactory` again for the same name clears that name's cache.

### Untrusted Repositories (`RepoTrust`)

Local CLI providers run a repo-aware agent on the host. If the repository they
work on is untrusted, its instruction files, `.mcp.json` or prompt injection
can steer that agent. Untrusted mode only routes to providers that do not run
a host agent, and fails closed when none is available:

```go
r, err := router.NewRouterFromConfig(router.RouterConfig{
    Providers: providers,
    RepoTrust: router.RepoTrustUntrusted, // set at construction, before any background work
})
// or at runtime:
r.SetRepoTrust(router.RepoTrustUntrusted)
trust := r.RepoTrust()                          // current level
level, ok := router.ParseRepoTrust("untrusted") // "trusted" | "untrusted" | "" (default trusted)
```

- A provider opts in by implementing `router.UntrustedRepoCapable`
  (`SafeForUntrustedRepo() bool`). The direct API providers and
  `NewRemoteHTTP` return true. `CLIProvider` returns false. A provider that
  does not implement the interface counts as unsafe.
- In untrusted mode, routing, fallback, ensemble, `Get`, `Available`, and the
  health and quota probes skip unsafe providers **before probing them**, so no
  unsafe CLI is ever executed. When no safe provider remains, the call fails
  with `router.ErrNoUntrustedSafeProvider` rather than silently falling back.
- Per-request views such as HTTP mode/allowlist clones inherit the trust level.

### Spend Policy (`api_policy`) Is Not Enforced by the Library

makewand's `api_policy` setting (`subscription_only`, the default, or
`allow_paid`) is enforced by makewand's own config adapter, not by this
package. Under `subscription_only` the adapter drops paid API keys and
API-access custom providers when it builds the Router, so its factories can
only produce subscription providers. Inside the `router` package, `AccessType` (`AccessSubscription`,
`AccessAPI`, …) is only an ordering key and a pricing switch, not an
admission control. Every paid provider an embedder adds is used:

- providers passed in `RouterConfig.Providers`,
- providers added with `RegisterProvider(name, p, router.AccessAPI)`,
- providers returned by a `RegisterProviderFactory` factory.

To enforce a subscription-only policy in your own program, do not register
paid providers. The HTTP facade's per-token cost budgets (`Authorizer`) and
team budgets limit spend, but they do not replace that policy.

## Modes

| Mode | Tier | Description |
|------|------|-------------|
| `fast` | Cheap | Lowest latency, prefer subscription/free providers |
| `balanced` | Mid | Good quality/cost ratio, cross-model review |
| `power` | Premium | Best quality, parallel ensemble + judge selection |

## Strategy Customization

Place a `routing.json` in the directory passed as `RouterConfig.ConfigDir`
(or call `r.LoadUserOverrides(dir)`) to override defaults:

```json
{
  "strategies": {
    "balanced": {
      "code": {"tier": "mid", "providers": ["claude", "gemini"]},
      "review": {"tier": "mid", "providers": ["gemini", "claude"]}
    }
  }
}
```

Only the fields you specify are overridden — absent fields keep their
defaults, down to individual `(provider, tier)` and `(mode, task)` keys.
Overrides are always merged over the **built-in defaults**, never over the
current tables. Each successful `r.LoadUserOverrides(dir)` call replaces the
overrides applied before, including those loaded from `RouterConfig.ConfigDir`
at construction. Only the most recently loaded `routing.json` is in effect, so
put everything you want to combine into one file. A missing `routing.json` is
not an error and leaves the tables unchanged. Overrides are validated on load:
`NewRouterFromConfig` returns the error, and `r.LoadUserOverrides` leaves the
previous tables unchanged on failure. Overrides are scoped to the Router
instance that loaded them, so two Routers with different config directories
never affect each other.

## Migration from internal/model

If you were importing `internal/model`, switch to importing `router` directly:

```go
// Before (internal — not importable by external packages)
import "github.com/makewand/makewand/internal/model"
r := model.NewRouter(cfg)

// After (public library API)
import "github.com/makewand/makewand/router"
r, err := router.NewRouterFromConfig(router.RouterConfig{
    Providers: map[string]router.ProviderEntry{...},
    UsageMode: "balanced",
})
```

Key differences:
- `NewRouterFromConfig` takes `RouterConfig` (no config package dependency)
- Constructor returns `(*Router, error)` — invalid overrides fail construction
- `RegisterProviderFactory` is an instance method — `(*Router).RegisterProviderFactory`.
  Factories are strictly per-instance; there is no package-level registry, so two
  Routers can build the same provider name from different configs without ever
  affecting one another
- Strategy tables are per-instance, deep-copied from immutable package
  defaults (safe for multiple Router instances and parallel tests)

## Implementing a Custom Provider

```go
type MyProvider struct{}

func (p *MyProvider) Name() string          { return "myprovider" }
func (p *MyProvider) IsAvailable() bool     { return true }
func (p *MyProvider) Chat(ctx context.Context, messages []router.Message, system string, maxTokens int) (string, router.Usage, error) {
    // Your implementation
    return "response", router.Usage{}, nil
}
func (p *MyProvider) ChatStream(ctx context.Context, messages []router.Message, system string, maxTokens int) (<-chan router.StreamChunk, error) {
    // Your streaming implementation
    ch := make(chan router.StreamChunk, 1)
    ch <- router.StreamChunk{Content: "response", Done: true}
    close(ch)
    return ch, nil
}
```

Register it:

```go
r.RegisterProvider("myprovider", &MyProvider{}, router.AccessAPI)
```
