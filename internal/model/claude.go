package model

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

const claudeAPIURL = "https://api.anthropic.com/v1/messages"
const claudeDefaultModel = "claude-sonnet-4-20250514"

// maxResponseBytes is the maximum response body size (10 MB).
const maxResponseBytes = 10 << 20

// Claude implements the Provider interface for Anthropic's Claude API.
type Claude struct {
	apiKey       string
	model        string
	chatClient   *http.Client
	streamClient *http.Client
}

// NewClaude creates a new Claude provider.
func NewClaude(apiKey, model string) *Claude {
	if model == "" {
		model = claudeDefaultModel
	}
	return &Claude{
		apiKey:       apiKey,
		model:        model,
		chatClient:   newAPIClient(),
		streamClient: newStreamClient(),
	}
}

func (c *Claude) Name() string      { return "claude" }
func (c *Claude) IsAvailable() bool { return c.apiKey != "" }

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []claudeMessage `json:"messages"`
	Stream    bool            `json:"stream,omitempty"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeContentBlock struct {
	Text string `json:"text"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type claudeResponse struct {
	Content []claudeContentBlock `json:"content"`
	Usage   claudeUsage          `json:"usage"`
}

type claudeStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

func (c *Claude) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	msgs := make([]claudeMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
			}
			continue
		}
		msgs = append(msgs, claudeMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := claudeRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
	}

	body, err := marshalProviderJSON("claude", reqBody)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := newProviderJSONRequest(ctx, "claude", http.MethodPost, claudeAPIURL, body, map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         c.apiKey,
		"anthropic-version": "2023-06-01",
	})
	if err != nil {
		return "", Usage{}, err
	}
	respBody, err := doProviderJSONRequest(c.chatClient, req, "claude", "API request")
	if err != nil {
		return "", Usage{}, err
	}

	var result claudeResponse
	if err := decodeProviderJSON("claude", "parse response", respBody, &result); err != nil {
		return "", Usage{}, err
	}

	var b strings.Builder
	for _, block := range result.Content {
		b.WriteString(block.Text)
	}

	usage := Usage{
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		Cost:         EstimateCost(c.model, result.Usage.InputTokens, result.Usage.OutputTokens),
		Model:        c.model,
		Provider:     "claude",
	}

	return b.String(), usage, nil
}

func (c *Claude) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	msgs := make([]claudeMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
			}
			continue
		}
		msgs = append(msgs, claudeMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := claudeRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
		Stream:    true,
	}

	body, err := marshalProviderJSON("claude", reqBody)
	if err != nil {
		return nil, err
	}
	req, err := newProviderJSONRequest(ctx, "claude", http.MethodPost, claudeAPIURL, body, map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         c.apiKey,
		"anthropic-version": "2023-06-01",
	})
	if err != nil {
		return nil, err
	}
	resp, err := openProviderStream(c.streamClient, req, "claude", "API stream request")
	if err != nil {
		return nil, err
	}

	ch := streamSSE(ctx, resp.Body, func(data string) []StreamChunk {
		var event claudeStreamEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			return nil
		}
		if event.Type == "message_stop" {
			return []StreamChunk{{Done: true}}
		}
		if event.Type == "content_block_delta" && event.Delta.Text != "" {
			return []StreamChunk{{Content: event.Delta.Text}}
		}
		return nil
	}, func() { resp.Body.Close() })

	return ch, nil
}
