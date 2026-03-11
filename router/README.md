# router — Multi-Provider AI Routing Library

`router` is a standalone Go package for intelligent multi-provider AI routing.
It provides Thompson Sampling–based adaptive selection, circuit breaker fault
tolerance, ensemble+judge evaluation, and an OpenAI-compatible HTTP facade.

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
})
if err != nil {
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

```go
// Record quality outcomes to influence future routing
r.RecordQualityOutcome(router.PhaseCode, "claude", true)  // success
r.RecordQualityOutcome(router.PhaseCode, "gemini", false) // failure
```

### Circuit Breaker

Providers that fail repeatedly are temporarily excluded:

- Timeout/deadline errors trip the circuit immediately
- Other errors require reaching a configurable threshold
- Circuit auto-resets after a cooldown period

### Ensemble + Judge (Power Mode)

In Power mode, multiple providers generate responses in parallel.
A cross-model judge selects the best output:

```go
content, usage, result, err := r.ChatBest(ctx, router.PhaseCode, messages, system)
```

### OpenAI-Compatible HTTP Facade

Expose your router as an OpenAI-compatible API server:

```go
r, _ := router.NewRouterFromConfig(rc)
http.ListenAndServe(":8080", r.HTTPHandler())
```

Endpoints:
- `POST /v1/chat/completions` — Chat completions (non-streaming)
- `GET /v1/models` — List available providers
- `GET /health` — Health check

### Strategy Hot-Reload

Watch `routing.json` for changes and merge updates without restart:

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
r.WatchOverrides(ctx, configDir)
```

### Provider Factories

Register factories for dynamic model-specific provider construction:

```go
r.RegisterProviderFactory("claude", func(modelID string) (router.Provider, error) {
    return router.NewClaude(apiKey, modelID), nil
})
```

## Modes

| Mode | Tier | Description |
|------|------|-------------|
| `fast` | Cheap | Lowest latency, prefer subscription/free providers |
| `balanced` | Mid | Good quality/cost ratio, cross-model review |
| `power` | Premium | Best quality, parallel ensemble + judge selection |

## Strategy Customization

Place a `routing.json` in your config directory to override defaults:

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

Only the fields you specify are overridden — absent fields keep their defaults.

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
- Constructor returns `(*Router, error)` instead of `*Router`
- `RegisterProviderFactory` is an instance method, not a package-level function
- Strategy tables are per-instance (safe for multiple Router instances)

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
