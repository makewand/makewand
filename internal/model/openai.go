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

const openaiAPIURL = "https://api.openai.com/v1/chat/completions"
const openaiModel = "gpt-4o"

// OpenAI implements the Provider interface for OpenAI's API.
type OpenAI struct {
	apiKey string
	client *http.Client
}

// NewOpenAI creates a new OpenAI provider.
func NewOpenAI(apiKey string) *OpenAI {
	return &OpenAI{
		apiKey: apiKey,
		client: &http.Client{},
	}
}

func (o *OpenAI) Name() string     { return "openai" }
func (o *OpenAI) IsAvailable() bool { return o.apiKey != "" }

type openaiRequest struct {
	Model    string          `json:"model"`
	Messages []openaiMessage `json:"messages"`
	Stream   bool            `json:"stream,omitempty"`
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

func (o *OpenAI) Chat(ctx context.Context, messages []Message, system string) (string, Usage, error) {
	msgs := make([]openaiMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		msgs = append(msgs, openaiMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := openaiRequest{
		Model:    openaiModel,
		Messages: msgs,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", Usage{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", openaiAPIURL, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("openai API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", Usage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("openai API error (%d): %s", resp.StatusCode, string(respBody))
	}

	var result openaiResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", Usage{}, fmt.Errorf("parse response: %w", err)
	}

	var text string
	if len(result.Choices) > 0 {
		text = result.Choices[0].Message.Content
	}

	usage := Usage{
		InputTokens:  result.Usage.PromptTokens,
		OutputTokens: result.Usage.CompletionTokens,
		Cost:         estimateOpenAICost(result.Usage.PromptTokens, result.Usage.CompletionTokens),
		Model:        openaiModel,
		Provider:     "openai",
	}

	return text, usage, nil
}

func (o *OpenAI) ChatStream(ctx context.Context, messages []Message, system string) (<-chan StreamChunk, error) {
	msgs := make([]openaiMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		msgs = append(msgs, openaiMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := openaiRequest{
		Model:    openaiModel,
		Messages: msgs,
		Stream:   true,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", openaiAPIURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai API stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai API error (%d): %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		buf := make([]byte, 4096)
		var accumulated string

		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				accumulated += string(buf[:n])
				for {
					idx := strings.Index(accumulated, "\n\n")
					if idx == -1 {
						break
					}
					event := accumulated[:idx]
					accumulated = accumulated[idx+2:]

					for _, line := range strings.Split(event, "\n") {
						if strings.HasPrefix(line, "data: ") {
							data := strings.TrimPrefix(line, "data: ")
							if data == "[DONE]" {
								ch <- StreamChunk{Done: true}
								return
							}
							var chunk struct {
								Choices []struct {
									Delta struct {
										Content string `json:"content"`
									} `json:"delta"`
								} `json:"choices"`
							}
							if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
								if content := chunk.Choices[0].Delta.Content; content != "" {
									ch <- StreamChunk{Content: content}
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
	}()

	return ch, nil
}

// estimateOpenAICost estimates cost in USD for GPT-4o.
// GPT-4o: $2.50/MTok input, $10/MTok output
func estimateOpenAICost(input, output int) float64 {
	return float64(input)/1_000_000*2.5 + float64(output)/1_000_000*10.0
}
