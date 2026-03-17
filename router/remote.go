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
	ch := make(chan StreamChunk, 2)
	go func() {
		defer close(ch)
		content, _, err := p.Chat(ctx, messages, system, maxTokens)
		if err != nil {
			ch <- StreamChunk{Error: err, Done: true}
			return
		}
		if content != "" {
			ch <- StreamChunk{Content: content}
		}
		ch <- StreamChunk{Done: true}
	}()
	return ch, nil
}
