package router

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

const openaiAPIURL = "https://api.openai.com/v1/chat/completions"
const openaiDefaultModel = "gpt-4o"

// OpenAI implements the Provider interface for OpenAI's API.
type OpenAI struct {
	apiKey  string
	baseURL string
	model   string
	// pinModel makes every call use model, whatever model routing requests: the
	// instance serves a model a client named (Router.withOpenAIModel), which a
	// routing.json model table for "openai" must not replace.
	pinModel     bool
	chatClient   *http.Client
	streamClient *http.Client
	instanceCostTable
}

// NewOpenAI creates a new OpenAI provider.
func NewOpenAI(apiKey, model string) *OpenAI {
	return NewOpenAIWithBaseURL(apiKey, model, "")
}

// NewOpenAIWithBaseURL uses an explicitly configured API endpoint. The existing
// constructor retains the official endpoint and is source-compatible.
func NewOpenAIWithBaseURL(apiKey, model, baseURL string) *OpenAI {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = openaiAPIURL
	}
	if !strings.HasSuffix(baseURL, "/chat/completions") {
		baseURL += "/chat/completions"
	}
	if model == "" {
		model = openaiDefaultModel
	}
	return &OpenAI{
		apiKey:       apiKey,
		baseURL:      baseURL,
		model:        model,
		chatClient:   newAPIClient(),
		streamClient: newStreamClient(),
	}
}

func (o *OpenAI) Name() string      { return "openai" }
func (o *OpenAI) IsAvailable() bool { return o.apiKey != "" }

// SafeForUntrustedRepo reports that OpenAI is safe against an untrusted repo: it
// is a pure HTTP call to the OpenAI API and runs no local repo-aware agent.
func (o *OpenAI) SafeForUntrustedRepo() bool { return true }

// ReportedModelID names the model a call sends upstream: the router-requested
// model or, when routing requests none (OpenAI has no model table entry), the
// configured one. Stream routes, which carry no usage, are then reported and
// billed at the model actually called.
func (o *OpenAI) ReportedModelID(requestedModelID string) string {
	return o.callModel(requestedModelID)
}

// callModel returns the model a call sends upstream. A "-cli" model ID names a
// subscription CLI's own default, never an API model: the codex route's table
// entry is "codex-cli", priced at $0, and routing requests it when the codex
// route is served by this API because no codex CLI is installed. Such a request
// uses the configured model, as a call routing names no model for does.
func (o *OpenAI) callModel(requested string) string {
	if requested != "" && !o.pinModel && !strings.HasSuffix(requested, "-cli") {
		return requested
	}
	return o.model
}

type openaiRequest struct {
	Model               string          `json:"model"`
	Messages            []openaiMessage `json:"messages"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
}

// newOpenAIRequest builds a chat-completions request whose output is capped at
// maxTokens. OpenAI's reasoning models refuse max_tokens with a 400 and take
// max_completion_tokens, which also counts their hidden reasoning tokens, so
// a call still bills at most maxTokens of output. Every other model keeps
// max_tokens, the field OpenAI-compatible endpoints accept.
func newOpenAIRequest(model string, messages []openaiMessage, maxTokens int, stream bool) openaiRequest {
	req := openaiRequest{Model: model, Messages: messages, Stream: stream}
	if isOpenAIReasoningModel(model) {
		req.MaxCompletionTokens = maxTokens
	} else {
		req.MaxTokens = maxTokens
	}
	return req
}

// isOpenAIReasoningModel reports whether model names one of OpenAI's reasoning
// models: the o-series, GPT-5 and codex-mini (an o4-mini).
func isOpenAIReasoningModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5", "codex-mini"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type openaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

func (o *OpenAI) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	return dispatchChat(ctx, o, func(attemptCtx context.Context) (string, Usage, error) {
		return o.chatUnaccounted(attemptCtx, messages, system, maxTokens)
	})
}

func (o *OpenAI) chatUnaccounted(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	requested, _ := ModelFromContext(ctx)
	model := o.callModel(requested)
	msgs := make([]openaiMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		msgs = append(msgs, openaiMessage(m))
	}

	body, err := marshalProviderJSON("openai", newOpenAIRequest(model, msgs, maxTokens, false))
	if err != nil {
		return "", Usage{}, err
	}
	req, err := newProviderJSONRequest(ctx, "openai", http.MethodPost, o.baseURL, body, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + o.apiKey,
	})
	if err != nil {
		return "", Usage{}, err
	}
	respBody, err := doProviderJSONRequest(o.chatClient, req, "openai", "API request")
	if err != nil {
		return "", Usage{}, err
	}

	var result openaiResponse
	if err := decodeProviderJSON("openai", "parse response", respBody, &result); err != nil {
		return "", Usage{}, err
	}

	var text string
	if len(result.Choices) > 0 {
		text = result.Choices[0].Message.Content
	}

	inputTokens, outputTokens := result.Usage.PromptTokens, result.Usage.CompletionTokens
	measured := inputTokens > 0 || outputTokens > 0
	if !measured {
		// A reply without a usage block is estimated like a stream, not
		// recorded as a measured $0.
		inputTokens, outputTokens = estimateCallTokens(messages, system, text, maxTokens)
	}
	cost, measuredCost := o.priceForCtxWithStatus(ctx, model, inputTokens, outputTokens)
	usage := Usage{
		MeasuredTokens: measured,
		MeasuredCost:   measured && measuredCost,
		InputTokens:    inputTokens,
		OutputTokens:   outputTokens,
		Cost:           cost,
		Model:          model,
		Provider:       "openai",
	}

	return text, usage, nil
}

func (o *OpenAI) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	return dispatchStream(ctx, o, func(attemptCtx context.Context) (<-chan StreamChunk, error) {
		return o.chatStreamUnaccounted(attemptCtx, messages, system, maxTokens)
	})
}

func (o *OpenAI) chatStreamUnaccounted(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	requested, _ := ModelFromContext(ctx)
	model := o.callModel(requested)
	msgs := make([]openaiMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		msgs = append(msgs, openaiMessage(m))
	}

	body, err := marshalProviderJSON("openai", newOpenAIRequest(model, msgs, maxTokens, true))
	if err != nil {
		return nil, err
	}
	req, err := newProviderJSONRequest(ctx, "openai", http.MethodPost, o.baseURL, body, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + o.apiKey,
	})
	if err != nil {
		return nil, err
	}
	resp, err := openProviderStream(o.streamClient, req, "openai", "API stream request")
	if err != nil {
		return nil, err
	}

	ch := streamSSE(ctx, resp.Body, func(data string) []StreamChunk {
		var chunk openaiStreamChunk
		if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
			if content := chunk.Choices[0].Delta.Content; content != "" {
				return []StreamChunk{{Content: content}}
			}
		}
		return nil
	}, func() { resp.Body.Close() })

	return ch, nil
}
