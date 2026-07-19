package router

import (
	"context"
	"encoding/json"
	"net/http"
)

const openaiAPIURL = "https://api.openai.com/v1/chat/completions"
const openaiDefaultModel = "gpt-4o"

// OpenAI implements the Provider interface for OpenAI's API.
type OpenAI struct {
	apiKey       string
	model        string
	chatClient   *http.Client
	streamClient *http.Client
	instanceCostTable
}

// NewOpenAI creates a new OpenAI provider.
func NewOpenAI(apiKey, model string) *OpenAI {
	if model == "" {
		model = openaiDefaultModel
	}
	return &OpenAI{
		apiKey:       apiKey,
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

type openaiRequest struct {
	Model     string          `json:"model"`
	Messages  []openaiMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
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
	model := o.model
	if requested, ok := ModelFromContext(ctx); ok && requested != "" {
		model = requested
	}
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

	reqBody := openaiRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
	}

	body, err := marshalProviderJSON("openai", reqBody)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := newProviderJSONRequest(ctx, "openai", http.MethodPost, openaiAPIURL, body, map[string]string{
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

	usage := Usage{
		InputTokens:  result.Usage.PromptTokens,
		OutputTokens: result.Usage.CompletionTokens,
		Cost:         o.priceForCtx(ctx, model, result.Usage.PromptTokens, result.Usage.CompletionTokens),
		Model:        model,
		Provider:     "openai",
	}

	return text, usage, nil
}

func (o *OpenAI) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	model := o.model
	if requested, ok := ModelFromContext(ctx); ok && requested != "" {
		model = requested
	}
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

	reqBody := openaiRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    true,
	}

	body, err := marshalProviderJSON("openai", reqBody)
	if err != nil {
		return nil, err
	}
	req, err := newProviderJSONRequest(ctx, "openai", http.MethodPost, openaiAPIURL, body, map[string]string{
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
