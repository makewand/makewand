package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
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
// Non-localhost hosts are blocked unless MAKEWAND_OLLAMA_ALLOW_REMOTE=1 is set.
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
		if !ollamaAllowRemote() {
			return fmt.Errorf("ollama URL host %q is not localhost — set MAKEWAND_OLLAMA_ALLOW_REMOTE=1 to allow remote hosts", host)
		}
	}
	return nil
}

func ollamaAllowRemote() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MAKEWAND_OLLAMA_ALLOW_REMOTE")))
	return v == "1" || v == "true" || v == "yes"
}

// NewOllama creates a new Ollama provider.
func NewOllama(baseURL, model string) *Ollama {
	if model == "" {
		model = ollamaDefaultModel
	}
	if err := validateOllamaURL(baseURL); err != nil {
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

	body, err := marshalProviderJSON("ollama", reqBody)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := newProviderJSONRequest(ctx, "ollama", http.MethodPost, o.baseURL+"/api/chat", body, map[string]string{
		"Content-Type": "application/json",
	})
	if err != nil {
		return "", Usage{}, err
	}
	respBody, err := doProviderJSONRequest(o.chatClient, req, "ollama", "API request")
	if err != nil {
		return "", Usage{}, err
	}

	var result ollamaResponse
	if err := decodeProviderJSON("ollama", "parse response", respBody, &result); err != nil {
		return "", Usage{}, err
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

	body, err := marshalProviderJSON("ollama", reqBody)
	if err != nil {
		return nil, err
	}
	req, err := newProviderJSONRequest(ctx, "ollama", http.MethodPost, o.baseURL+"/api/chat", body, map[string]string{
		"Content-Type": "application/json",
	})
	if err != nil {
		return nil, err
	}
	resp, err := openProviderStream(o.streamClient, req, "ollama", "API stream request")
	if err != nil {
		return nil, err
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
					ch <- StreamChunk{Error: wrapResponseReadError("ollama", "stream decode", err)}
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
