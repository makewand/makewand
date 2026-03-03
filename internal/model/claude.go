package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", Usage{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.chatClient.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("claude API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := limitedReadAll(resp.Body, maxResponseBytes)
	if err != nil {
		return "", Usage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("claude API error (%d): %s", resp.StatusCode, string(respBody))
	}

	var result claudeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", Usage{}, fmt.Errorf("parse response: %w", err)
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

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude API stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := limitedReadAll(resp.Body, maxResponseBytes)
		resp.Body.Close()
		return nil, fmt.Errorf("claude API error (%d): %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		dataCh := make(chan string, 64)
		errCh := make(chan error, 1)
		go func() {
			defer close(dataCh)
			errCh <- readSSE(ctx, resp.Body, dataCh)
			close(errCh)
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-errCh:
				if !ok {
					errCh = nil
					continue
				}
				errCh = nil
				if err != nil {
					ch <- StreamChunk{Error: err}
					return
				}
			case data, ok := <-dataCh:
				if !ok {
					ch <- StreamChunk{Done: true}
					return
				}
				var event claudeStreamEvent
				if json.Unmarshal([]byte(data), &event) == nil {
					if event.Type == "content_block_delta" && event.Delta.Text != "" {
						ch <- StreamChunk{Content: event.Delta.Text}
					}
					if event.Type == "message_stop" {
						ch <- StreamChunk{Done: true}
						return
					}
				}
			}
		}
	}()

	return ch, nil
}
