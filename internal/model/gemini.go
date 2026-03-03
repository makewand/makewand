package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const geminiDefaultModel = "gemini-2.5-flash"

// Gemini implements the Provider interface for Google's Gemini API.
type Gemini struct {
	apiKey       string
	model        string
	chatClient   *http.Client
	streamClient *http.Client
}

// NewGemini creates a new Gemini provider.
func NewGemini(apiKey, model string) *Gemini {
	if model == "" {
		model = geminiDefaultModel
	}
	return &Gemini{
		apiKey:       apiKey,
		model:        model,
		chatClient:   newAPIClient(),
		streamClient: newStreamClient(),
	}
}

func (g *Gemini) Name() string      { return "gemini" }
func (g *Gemini) IsAvailable() bool { return g.apiKey != "" }

type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	Temperature     float64 `json:"temperature,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

func (g *Gemini) Chat(ctx context.Context, messages []Message, system string, maxTokens int) (string, Usage, error) {
	apiURL := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent",
		url.PathEscape(g.model),
	)

	var contents []geminiContent
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
			}
			continue
		}
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		})
	}

	reqBody := geminiRequest{
		Contents: contents,
		GenerationConfig: &geminiGenerationConfig{
			MaxOutputTokens: maxTokens,
		},
	}
	if system != "" {
		reqBody.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: system}},
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", Usage{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.chatClient.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("gemini API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := limitedReadAll(resp.Body, maxResponseBytes)
	if err != nil {
		return "", Usage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("gemini API error (%d): %s", resp.StatusCode, string(respBody))
	}

	var result geminiResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", Usage{}, fmt.Errorf("parse response: %w", err)
	}

	var b strings.Builder
	if len(result.Candidates) > 0 {
		for _, p := range result.Candidates[0].Content.Parts {
			b.WriteString(p.Text)
		}
	}

	usage := Usage{
		InputTokens:  result.UsageMetadata.PromptTokenCount,
		OutputTokens: result.UsageMetadata.CandidatesTokenCount,
		Cost:         EstimateCost(g.model, result.UsageMetadata.PromptTokenCount, result.UsageMetadata.CandidatesTokenCount),
		Model:        g.model,
		Provider:     "gemini",
	}

	return b.String(), usage, nil
}

func (g *Gemini) ChatStream(ctx context.Context, messages []Message, system string, maxTokens int) (<-chan StreamChunk, error) {
	apiURL := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?alt=sse",
		url.PathEscape(g.model),
	)

	var contents []geminiContent
	for _, m := range messages {
		if m.Role == "system" {
			if system == "" {
				system = m.Content
			}
			continue
		}
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		})
	}

	reqBody := geminiRequest{
		Contents: contents,
		GenerationConfig: &geminiGenerationConfig{
			MaxOutputTokens: maxTokens,
		},
	}
	if system != "" {
		reqBody.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: system}},
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini API stream request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := limitedReadAll(resp.Body, maxResponseBytes)
		resp.Body.Close()
		return nil, fmt.Errorf("gemini API error (%d): %s", resp.StatusCode, string(respBody))
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
				var result geminiResponse
				if json.Unmarshal([]byte(data), &result) == nil && len(result.Candidates) > 0 {
					for _, p := range result.Candidates[0].Content.Parts {
						if p.Text != "" {
							ch <- StreamChunk{Content: p.Text}
						}
					}
				}
			}
		}
	}()

	return ch, nil
}
