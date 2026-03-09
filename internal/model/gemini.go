package model

import (
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

	body, err := marshalProviderJSON("gemini", reqBody)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := newProviderJSONRequest(ctx, "gemini", http.MethodPost, apiURL, body, map[string]string{
		"Content-Type":   "application/json",
		"x-goog-api-key": g.apiKey,
	})
	if err != nil {
		return "", Usage{}, err
	}
	respBody, err := doProviderJSONRequest(g.chatClient, req, "gemini", "API request")
	if err != nil {
		return "", Usage{}, err
	}

	var result geminiResponse
	if err := decodeProviderJSON("gemini", "parse response", respBody, &result); err != nil {
		return "", Usage{}, err
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

	body, err := marshalProviderJSON("gemini", reqBody)
	if err != nil {
		return nil, err
	}
	req, err := newProviderJSONRequest(ctx, "gemini", http.MethodPost, apiURL, body, map[string]string{
		"Content-Type":   "application/json",
		"x-goog-api-key": g.apiKey,
	})
	if err != nil {
		return nil, err
	}
	resp, err := openProviderStream(g.streamClient, req, "gemini", "API stream request")
	if err != nil {
		return nil, err
	}

	ch := streamSSE(ctx, resp.Body, func(data string) []StreamChunk {
		var result geminiResponse
		if json.Unmarshal([]byte(data), &result) != nil || len(result.Candidates) == 0 {
			return nil
		}
		var chunks []StreamChunk
		for _, p := range result.Candidates[0].Content.Parts {
			if p.Text != "" {
				chunks = append(chunks, StreamChunk{Content: p.Text})
			}
		}
		return chunks
	}, func() { resp.Body.Close() })

	return ch, nil
}
