package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RemoteHTTPProvider proxies chat requests to a remote makewand server.
type RemoteHTTPProvider struct {
	baseURL    string
	token      string
	chatClient *http.Client
}

// NewRemoteHTTP creates a provider backed by a remote makewand HTTP server.
func NewRemoteHTTP(baseURL, token string) *RemoteHTTPProvider {
	return &RemoteHTTPProvider{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
	}
}

func (p *RemoteHTTPProvider) Name() string      { return "remote" }
func (p *RemoteHTTPProvider) IsAvailable() bool { return p != nil && p.baseURL != "" && p.token != "" }

func (p *RemoteHTTPProvider) client() *http.Client {
	if p != nil && p.chatClient != nil {
		return p.chatClient
	}
	return http.DefaultClient
}

func (p *RemoteHTTPProvider) Chat(ctx context.Context, messages []Message, system string, _ int) (string, Usage, error) {
	if !p.IsAvailable() {
		return "", Usage{}, fmt.Errorf("remote provider is not configured")
	}

	reqMessages := make([]httpMessage, 0, len(messages)+1)
	if strings.TrimSpace(system) != "" {
		reqMessages = append(reqMessages, httpMessage{Role: "system", Content: system})
	}
	for _, msg := range messages {
		reqMessages = append(reqMessages, httpMessage{Role: msg.Role, Content: msg.Content})
	}

	payload, err := json.Marshal(httpChatRequest{
		Mode:     remoteUsageMode(ctx),
		Messages: reqMessages,
	})
	if err != nil {
		return "", Usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)

	resp, err := p.client().Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return "", Usage{}, fmt.Errorf("remote provider error: %s", msg)
	}

	var chatResp httpChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", Usage{}, err
	}
	if len(chatResp.Choices) == 0 {
		return "", Usage{}, fmt.Errorf("remote provider returned no choices")
	}

	return chatResp.Choices[0].Message.Content, Usage{
		InputTokens:  chatResp.Usage.PromptTokens,
		OutputTokens: chatResp.Usage.CompletionTokens,
		Model:        chatResp.Model,
		Provider:     "remote",
	}, nil
}

func (p *RemoteHTTPProvider) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	if !p.IsAvailable() {
		return nil, fmt.Errorf("remote provider is not configured")
	}

	reqMessages := make([]httpMessage, 0, len(messages)+1)
	if strings.TrimSpace(system) != "" {
		reqMessages = append(reqMessages, httpMessage{Role: "system", Content: system})
	}
	for _, msg := range messages {
		reqMessages = append(reqMessages, httpMessage{Role: msg.Role, Content: msg.Content})
	}

	payload, err := json.Marshal(httpChatRequest{
		Mode:     remoteUsageMode(ctx),
		Messages: reqMessages,
		Stream:   true,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("remote provider error: %s", msg)
	}

	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		ch := make(chan StreamChunk, 2)
		go func() {
			defer close(ch)
			defer resp.Body.Close()
			var chatResp httpChatResponse
			if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
				ch <- StreamChunk{Error: err, Done: true}
				return
			}
			if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.Content != "" {
				ch <- StreamChunk{Content: chatResp.Choices[0].Message.Content}
			}
			ch <- StreamChunk{Done: true}
		}()
		return ch, nil
	}

	return streamSSE(ctx, resp.Body, func(data string) []StreamChunk {
		var payload httpStreamResponse
		if err := json.Unmarshal([]byte(data), &payload); err == nil {
			chunks := make([]StreamChunk, 0, len(payload.Choices))
			for _, choice := range payload.Choices {
				if choice.Delta.Content != "" {
					chunks = append(chunks, StreamChunk{Content: choice.Delta.Content})
				}
				if choice.FinishReason != "" {
					chunks = append(chunks, StreamChunk{Done: true})
				}
			}
			if len(chunks) > 0 {
				return chunks
			}
		}

		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &failure); err == nil && strings.TrimSpace(failure.Error.Message) != "" {
			return []StreamChunk{{Error: fmt.Errorf("remote provider error: %s", failure.Error.Message), Done: true}}
		}
		return nil
	}, func() { resp.Body.Close() }), nil
}

func remoteUsageMode(ctx context.Context) string {
	mode, ok := UsageModeFromContext(ctx)
	if !ok {
		return ""
	}
	return mode.String()
}
