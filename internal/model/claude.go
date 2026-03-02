package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const claudeAPIURL = "https://api.anthropic.com/v1/messages"
const claudeModel = "claude-sonnet-4-20250514"

// Claude implements the Provider interface for Anthropic's Claude API.
type Claude struct {
	apiKey string
	client *http.Client
}

// NewClaude creates a new Claude provider.
func NewClaude(apiKey string) *Claude {
	return &Claude{
		apiKey: apiKey,
		client: &http.Client{},
	}
}

func (c *Claude) Name() string       { return "claude" }
func (c *Claude) IsAvailable() bool   { return c.apiKey != "" }

type claudeRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []claudeMessage  `json:"messages"`
	Stream    bool             `json:"stream,omitempty"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *Claude) Chat(ctx context.Context, messages []Message, system string) (string, Usage, error) {
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
		Model:     claudeModel,
		MaxTokens: 8192,
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

	resp, err := c.client.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("claude API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
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

	var text string
	for _, c := range result.Content {
		text += c.Text
	}

	usage := Usage{
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		Cost:         estimateClaudeCost(result.Usage.InputTokens, result.Usage.OutputTokens),
		Model:        claudeModel,
		Provider:     "claude",
	}

	return text, usage, nil
}

func (c *Claude) ChatStream(ctx context.Context, messages []Message, system string) (<-chan StreamChunk, error) {
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
		Model:     claudeModel,
		MaxTokens: 8192,
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

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude API stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("claude API error (%d): %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		c.readSSEStream(resp.Body, ch)
	}()

	return ch, nil
}

func (c *Claude) readSSEStream(r io.Reader, ch chan<- StreamChunk) {
	buf := make([]byte, 4096)
	var accumulated string

	for {
		n, err := r.Read(buf)
		if n > 0 {
			accumulated += string(buf[:n])
			// Process complete SSE events
			for {
				idx := strings.Index(accumulated, "\n\n")
				if idx == -1 {
					break
				}
				event := accumulated[:idx]
				accumulated = accumulated[idx+2:]

				// Parse SSE event
				for _, line := range strings.Split(event, "\n") {
					if strings.HasPrefix(line, "data: ") {
						data := strings.TrimPrefix(line, "data: ")
						if data == "[DONE]" {
							ch <- StreamChunk{Done: true}
							return
						}
						var sseData struct {
							Type  string `json:"type"`
							Delta struct {
								Type string `json:"type"`
								Text string `json:"text"`
							} `json:"delta"`
						}
						if json.Unmarshal([]byte(data), &sseData) == nil {
							if sseData.Type == "content_block_delta" && sseData.Delta.Text != "" {
								ch <- StreamChunk{Content: sseData.Delta.Text}
							}
							if sseData.Type == "message_stop" {
								ch <- StreamChunk{Done: true}
								return
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				ch <- StreamChunk{Error: err}
			}
			ch <- StreamChunk{Done: true}
			return
		}
	}
}

// estimateClaudeCost estimates cost in USD based on token counts.
// Sonnet 4: $3/MTok input, $15/MTok output
func estimateClaudeCost(input, output int) float64 {
	return float64(input)/1_000_000*3.0 + float64(output)/1_000_000*15.0
}
