package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const ollamaDefaultModel = "llama3.2"

// Ollama implements the Provider interface for local Ollama models.
type Ollama struct {
	baseURL string
	client  *http.Client
}

// NewOllama creates a new Ollama provider.
func NewOllama(baseURL string) *Ollama {
	return &Ollama{
		baseURL: baseURL,
		client:  &http.Client{},
	}
}

func (o *Ollama) Name() string     { return "ollama" }
func (o *Ollama) IsAvailable() bool {
	if o.baseURL == "" {
		return false
	}
	// Quick health check
	resp, err := o.client.Get(o.baseURL + "/api/tags")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type ollamaRequest struct {
	Model    string           `json:"model"`
	Messages []ollamaMessage  `json:"messages"`
	Stream   bool             `json:"stream"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done               bool `json:"done"`
	PromptEvalCount    int  `json:"prompt_eval_count"`
	EvalCount          int  `json:"eval_count"`
}

func (o *Ollama) Chat(ctx context.Context, messages []Message, system string) (string, Usage, error) {
	msgs := make([]ollamaMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := ollamaRequest{
		Model:    ollamaDefaultModel,
		Messages: msgs,
		Stream:   false,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", Usage{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("ollama API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", Usage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("ollama API error (%d): %s", resp.StatusCode, string(respBody))
	}

	var result ollamaResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", Usage{}, fmt.Errorf("parse response: %w", err)
	}

	usage := Usage{
		InputTokens:  result.PromptEvalCount,
		OutputTokens: result.EvalCount,
		Cost:         0, // Local model, free
		Model:        ollamaDefaultModel,
		Provider:     "ollama",
	}

	return result.Message.Content, usage, nil
}

func (o *Ollama) ChatStream(ctx context.Context, messages []Message, system string) (<-chan StreamChunk, error) {
	msgs := make([]ollamaMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := ollamaRequest{
		Model:    ollamaDefaultModel,
		Messages: msgs,
		Stream:   true,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama API stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama API error (%d): %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		decoder := json.NewDecoder(resp.Body)
		for {
			var result ollamaResponse
			if err := decoder.Decode(&result); err != nil {
				if err != io.EOF {
					ch <- StreamChunk{Error: err}
				}
				break
			}
			if result.Message.Content != "" {
				ch <- StreamChunk{Content: result.Message.Content}
			}
			if result.Done {
				ch <- StreamChunk{Done: true}
				return
			}
		}
		ch <- StreamChunk{Done: true}
	}()

	return ch, nil
}
