package model

import (
	"context"
	"fmt"

	"github.com/makewand/makewand/internal/config"
)

// TaskType categorizes what kind of AI task is being performed.
type TaskType int

const (
	TaskAnalyze  TaskType = iota // requirements analysis, planning
	TaskCode                     // code generation, implementation
	TaskReview                   // code review, bug finding
	TaskExplain                  // explanation for non-programmers
	TaskFix                      // error diagnosis and fixing
)

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`    // "user", "assistant", "system"
	Content string `json:"content"`
}

// StreamChunk is a piece of streaming response.
type StreamChunk struct {
	Content string
	Done    bool
	Error   error
}

// Usage tracks token usage and cost for a single request.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Cost         float64
	Model        string
	Provider     string
}

// Provider defines the interface all model providers must implement.
type Provider interface {
	Name() string
	IsAvailable() bool
	Chat(ctx context.Context, messages []Message, system string) (string, Usage, error)
	ChatStream(ctx context.Context, messages []Message, system string) (<-chan StreamChunk, error)
}

// Router selects the best model provider for a given task.
type Router struct {
	cfg       *config.Config
	providers map[string]Provider
}

// NewRouter creates a new model router with configured providers.
func NewRouter(cfg *config.Config) *Router {
	r := &Router{
		cfg:       cfg,
		providers: make(map[string]Provider),
	}

	// Register available providers
	if cfg.ClaudeAPIKey != "" {
		r.providers["claude"] = NewClaude(cfg.ClaudeAPIKey)
	}
	if cfg.GeminiAPIKey != "" {
		r.providers["gemini"] = NewGemini(cfg.GeminiAPIKey)
	}
	if cfg.OpenAIAPIKey != "" {
		r.providers["openai"] = NewOpenAI(cfg.OpenAIAPIKey)
	}
	if cfg.OllamaURL != "" {
		r.providers["ollama"] = NewOllama(cfg.OllamaURL)
	}

	return r
}

// Route selects the best provider for a given task type.
func (r *Router) Route(task TaskType) (Provider, error) {
	var modelName string

	switch task {
	case TaskAnalyze, TaskExplain:
		modelName = r.cfg.AnalysisModel
	case TaskCode, TaskFix:
		modelName = r.cfg.CodingModel
	case TaskReview:
		modelName = r.cfg.ReviewModel
	default:
		modelName = r.cfg.DefaultModel
	}

	// Try the preferred model
	if p, ok := r.providers[modelName]; ok && p.IsAvailable() {
		return p, nil
	}

	// Fallback: try any available provider
	for _, p := range r.providers {
		if p.IsAvailable() {
			return p, nil
		}
	}

	return nil, fmt.Errorf("no AI model available; configure one with 'makewand setup'")
}

// Get returns a specific provider by name.
func (r *Router) Get(name string) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("model provider %q not configured", name)
	}
	if !p.IsAvailable() {
		return nil, fmt.Errorf("model provider %q is not available", name)
	}
	return p, nil
}

// Available returns all available provider names.
func (r *Router) Available() []string {
	var names []string
	for name, p := range r.providers {
		if p.IsAvailable() {
			names = append(names, name)
		}
	}
	return names
}

// Chat sends a message using the best provider for the given task type.
func (r *Router) Chat(ctx context.Context, task TaskType, messages []Message, system string) (string, Usage, error) {
	p, err := r.Route(task)
	if err != nil {
		return "", Usage{}, err
	}
	return p.Chat(ctx, messages, system)
}

// ChatStream sends a message and streams the response.
func (r *Router) ChatStream(ctx context.Context, task TaskType, messages []Message, system string) (<-chan StreamChunk, string, error) {
	p, err := r.Route(task)
	if err != nil {
		return nil, "", err
	}
	ch, err := p.ChatStream(ctx, messages, system)
	return ch, p.Name(), err
}
