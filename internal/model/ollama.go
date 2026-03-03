package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const ollamaDefaultModel = "gemma3:4b"

// Ollama implements the Provider interface for local Ollama models.
type Ollama struct {
	baseURL      string
	model        string
	chatClient   *http.Client
	streamClient *http.Client
	healthClient *http.Client

	// Cached availability with TTL
	mu            sync.Mutex
	cachedAvail   bool
	cachedAvailAt time.Time
}

const ollamaAvailCacheTTL = 30 * time.Second

// validateOllamaURL checks that the base URL is a valid http/https URL.
// Returns an error for invalid schemes or empty hosts.
func validateOllamaURL(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid ollama URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("ollama URL must use http or https scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("ollama URL has empty host")
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		log.Printf("warning: ollama URL host %q is not localhost — ensure this is intentional", host)
	}
	return nil
}

// NewOllama creates a new Ollama provider.
func NewOllama(baseURL, model string) *Ollama {
	if model == "" {
		model = ollamaDefaultModel
	}
	if err := validateOllamaURL(baseURL); err != nil {
		log.Printf("ollama: %v — provider disabled", err)
		baseURL = "" // disable provider
	}
	return &Ollama{
		baseURL:      baseURL,
		model:        model,
		chatClient:   newAPIClient(),
		streamClient: newStreamClient(),
		healthClient: newHealthCheckClient(),
	}
}

func (o *Ollama) Name() string { return "ollama" }

func (o *Ollama) IsAvailable() bool {
	if o.baseURL == "" {
		return false
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if time.Since(o.cachedAvailAt) < ollamaAvailCacheTTL {
		return o.cachedAvail
	}

	resp, err := o.healthClient.Get(o.baseURL + "/api/tags")
	if err != nil {
		o.cachedAvail = false
		o.cachedAvailAt = time.Now()
		return false
	}
	resp.Body.Close()

	o.cachedAvail = resp.StatusCode == http.StatusOK
	o.cachedAvailAt = time.Now()
	return o.cachedAvail
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  *ollamaOptions  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	NumPredict int `json:"num_predict,omitempty"`
}

type ollamaResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done            bool `json:"done"`
	PromptEvalCount int  `json:"prompt_eval_count"`
	EvalCount       int  `json:"eval_count"`
}

func (o *Ollama) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	msgs := make([]ollamaMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := ollamaRequest{
		Model:    o.model,
		Messages: msgs,
		Stream:   false,
		Options:  &ollamaOptions{NumPredict: maxTokens},
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

	resp, err := o.chatClient.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("ollama API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := limitedReadAll(resp.Body, maxResponseBytes)
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
		Cost:         EstimateCost(o.model, result.PromptEvalCount, result.EvalCount),
		Model:        o.model,
		Provider:     "ollama",
	}

	return result.Message.Content, usage, nil
}

func (o *Ollama) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	msgs := make([]ollamaMessage, 0, len(messages)+1)
	if system != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	reqBody := ollamaRequest{
		Model:    o.model,
		Messages: msgs,
		Stream:   true,
		Options:  &ollamaOptions{NumPredict: maxTokens},
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

	resp, err := o.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama API stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := limitedReadAll(resp.Body, maxResponseBytes)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama API error (%d): %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		decoder := json.NewDecoder(resp.Body)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var result ollamaResponse
			if err := decoder.Decode(&result); err != nil {
				if err != io.EOF {
					ch <- StreamChunk{Error: err}
				}
				break
			}
			if result.Message.Content != "" {
				select {
				case ch <- StreamChunk{Content: result.Message.Content}:
				case <-ctx.Done():
					return
				}
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
