package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	msgs := make([]openaiMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		msgs = append(msgs, openaiMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := openaiRequest{
		Model:     o.model,
		Messages:  msgs,
		MaxTokens: maxTokens,
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

	resp, err := o.chatClient.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("openai API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := limitedReadAll(resp.Body, maxResponseBytes)
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
		Cost:         EstimateCost(o.model, result.Usage.PromptTokens, result.Usage.CompletionTokens),
		Model:        o.model,
		Provider:     "openai",
	}

	return text, usage, nil
}

func (o *OpenAI) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	msgs := make([]openaiMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		msgs = append(msgs, openaiMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := openaiRequest{
		Model:     o.model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    true,
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

	resp, err := o.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai API stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := limitedReadAll(resp.Body, maxResponseBytes)
		resp.Body.Close()
		return nil, fmt.Errorf("openai API error (%d): %s", resp.StatusCode, string(respBody))
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
				var chunk openaiStreamChunk
				if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
					if content := chunk.Choices[0].Delta.Content; content != "" {
						ch <- StreamChunk{Content: content}
					}
				}
			}
		}
	}()

	return ch, nil
}
