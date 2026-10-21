package router

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

type httpResponseFormat struct {
	Type       string                        `json:"type,omitempty"`
	JSONSchema *httpResponseFormatJSONSchema `json:"json_schema,omitempty"`
}

type httpResponseFormatJSONSchema struct {
	Name   string         `json:"name,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
	Strict bool           `json:"strict,omitempty"`
}

type httpTool struct {
	Type     string           `json:"type,omitempty"`
	Function httpToolFunction `json:"function"`
}

type httpToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type httpToolCall struct {
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type"`
	Function httpToolCallFunction `json:"function"`
}

type httpToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func resolveHTTPModelAlias(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case model == "":
		return ""
	case strings.HasPrefix(model, "claude"):
		return "claude"
	case strings.HasPrefix(model, "gemini"):
		return "gemini"
	case strings.HasPrefix(model, "codex"):
		return "codex"
	case isOpenAIModelName(model):
		return "codex"
	default:
		return model
	}
}

// isOpenAIModelName reports whether a lower-cased model field names an OpenAI
// API model rather than the codex provider or its CLI.
func isOpenAIModelName(model string) bool {
	for _, prefix := range []string{"gpt-", "o1", "o3", "o4", "codex-mini"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// resolveHTTPModel maps a request's model field to the provider that serves it
// and, when that provider is the OpenAI API serving a model the client named,
// the model to call. OpenAI-family names select the codex provider. Without a
// registered "codex" (an API-key-only deployment, where the model factory also
// serves Power mode's codex route through the API) they select "openai"
// instead, and a named OpenAI model is called as named; "codex" itself keeps
// the configured model. Resolution uses the deployment's providers, not a
// token's allowlist, so a name means the same provider for every client.
func (r *Router) resolveHTTPModel(model string) (provider, upstreamModel string) {
	provider = resolveHTTPModelAlias(model)
	if provider != "codex" {
		return provider, ""
	}
	if _, ok := r.openAIAliasProvider(); !ok {
		return provider, ""
	}
	if name := strings.ToLower(strings.TrimSpace(model)); isOpenAIModelName(name) {
		return "openai", name
	}
	return "openai", ""
}

// openAIAliasProvider returns the OpenAI API provider that serves OpenAI-family
// model names: the registered "openai" when no "codex" is registered.
func (r *Router) openAIAliasProvider() (*OpenAI, bool) {
	names := r.registeredProviderNames()
	if containsString(names, "codex") || !containsString(names, "openai") {
		return nil, false
	}
	p, _ := r.getProvider("openai")
	openai, ok := p.(*OpenAI)
	return openai, ok
}

// openAIAliasModels lists the OpenAI models a client can name when openai
// serves OpenAI-family names, in the form a request's model field resolves to
// (resolveHTTPModel): each priced model a request naming it calls as named,
// and the configured one. A configured model outside those families, such as
// an OpenAI-compatible endpoint's own, is not listed; clients reach it as
// "openai".
func (r *Router) openAIAliasModels(openai *OpenAI) []string {
	var models []string
	for _, model := range append(r.routingTables().pricedModels(), strings.ToLower(strings.TrimSpace(openai.model))) {
		if _, upstream := r.resolveHTTPModel(model); upstream == model && !slices.Contains(models, model) {
			models = append(models, model)
		}
	}
	slices.Sort(models)
	return models
}

// withOpenAIModel returns a per-request view of r whose "openai" provider calls
// model, so the upstream call, the route's model and every price derived from
// it (usage, stream estimate, spend guard, reservation ceiling) name the model
// the client asked for, whatever model a routing.json table assigns "openai".
// The view shares r's concurrency permits, and r itself is never modified. Its
// calls honor the openai route's open circuit and rate-limit block but record
// their own outcomes under "openai:<model>": a model the client chose (one
// OpenAI rate-limits on its own, or one the account or endpoint lacks) must not
// close the route for every other request. A model other than the configured
// one must have a price: the spend of a model the server cannot price could
// not be budgeted. A request names the configured model as it names any model
// (trimmed, in any case). A priced name is called as its price entry spells it,
// and the configured model, when it has no price, as configured.
func (r *Router) withOpenAIModel(model string) (*Router, error) {
	p, _ := r.getProvider("openai")
	base, ok := p.(*OpenAI)
	if !ok {
		return nil, unknownHTTPModelError(model)
	}
	if _, priced := r.routingTables().costFor(model); !priced {
		if !strings.EqualFold(model, strings.TrimSpace(base.model)) {
			return nil, &httpStatusError{
				Status:  http.StatusBadRequest,
				Code:    "invalid_request",
				Message: fmt.Sprintf("model %q has no price on this server, so its spend cannot be budgeted; use GET /v1/models for the models it accepts", model),
			}
		}
		model = strings.TrimSpace(base.model)
	}
	bound := &OpenAI{
		apiKey:       base.apiKey,
		baseURL:      base.baseURL,
		model:        model,
		pinModel:     true,
		chatClient:   base.chatClient,
		streamClient: base.streamClient,
	}
	view := r.cloneView()
	if err := view.RegisterProvider("openai", bound, view.getAccessType("openai")); err != nil {
		return nil, err
	}
	view.healthKeys = map[string]string{"openai": "openai:" + model}
	return view, nil
}

// unknownHTTPModelError is the one error for a model field no provider serves,
// streamed or not; it names the model as the client sent it.
func unknownHTTPModelError(model string) *httpStatusError {
	return &httpStatusError{
		Status:  http.StatusBadRequest,
		Code:    "invalid_request",
		Message: fmt.Sprintf("unknown model %q; use GET /v1/models for the provider names and models this server accepts", model),
	}
}

func augmentHTTPSystem(system string, format *httpResponseFormat, tools []httpTool, toolChoice any) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(system) != "" {
		parts = append(parts, strings.TrimSpace(system))
	}
	if len(tools) > 0 {
		toolJSON, _ := json.Marshal(tools)
		instruction := "Available tools are provided as JSON function specs. " +
			"When a tool is required, respond with JSON only in the form " +
			`{"tool_calls":[{"name":"tool_name","arguments":{...}}]}` +
			" and no extra prose. Otherwise answer normally."
		if toolChoice != nil {
			choiceJSON, _ := json.Marshal(toolChoice)
			instruction += "\nTool choice: " + string(choiceJSON)
		}
		instruction += "\nTools: " + string(toolJSON)
		parts = append(parts, instruction)
	}
	if format != nil {
		switch strings.ToLower(strings.TrimSpace(format.Type)) {
		case "json_object":
			parts = append(parts, "Return only a valid JSON object. Do not wrap it in markdown fences or commentary.")
		case "json_schema":
			schemaJSON, _ := json.Marshal(format.JSONSchema)
			parts = append(parts, "Return only JSON that matches this schema exactly: "+string(schemaJSON))
		}
	}
	return strings.Join(parts, "\n\n")
}

func validateHTTPResponseFormat(content string, format *httpResponseFormat) (string, error) {
	content = strings.TrimSpace(content)
	if format == nil {
		return content, nil
	}
	formatType := strings.ToLower(strings.TrimSpace(format.Type))
	if formatType == "" {
		return content, nil
	}
	var value any
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		return "", fmt.Errorf("model output is not valid JSON")
	}
	switch formatType {
	case "json_object":
		if _, ok := value.(map[string]any); !ok {
			return "", fmt.Errorf("model output must be a JSON object")
		}
	case "json_schema":
		if format.JSONSchema == nil || len(format.JSONSchema.Schema) == 0 {
			return "", fmt.Errorf("response_format.json_schema.schema is required")
		}
		if err := validateJSONSchemaValue(value, format.JSONSchema.Schema, "$"); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported response_format type %q", format.Type)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func validateJSONSchemaValue(value any, schema map[string]any, path string) error {
	if schema == nil {
		return nil
	}
	if enumValues, ok := schema["enum"].([]any); ok && len(enumValues) > 0 {
		for _, item := range enumValues {
			if jsonEqual(item, value) {
				return nil
			}
		}
		return fmt.Errorf("%s is not one of the allowed enum values", path)
	}
	if schemaType, ok := schema["type"].(string); ok {
		switch schemaType {
		case "object":
			obj, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s must be an object", path)
			}
			properties, _ := schema["properties"].(map[string]any)
			required := make(map[string]struct{})
			if rawRequired, ok := schema["required"].([]any); ok {
				for _, item := range rawRequired {
					if name, ok := item.(string); ok {
						required[name] = struct{}{}
					}
				}
			}
			for key := range required {
				if _, ok := obj[key]; !ok {
					return fmt.Errorf("%s.%s is required", path, key)
				}
			}
			for key, childValue := range obj {
				childSchema, found := properties[key]
				if !found {
					if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
						return fmt.Errorf("%s.%s is not allowed", path, key)
					}
					continue
				}
				childSchemaMap, _ := childSchema.(map[string]any)
				if err := validateJSONSchemaValue(childValue, childSchemaMap, path+"."+key); err != nil {
					return err
				}
			}
		case "array":
			items, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s must be an array", path)
			}
			itemSchema, _ := schema["items"].(map[string]any)
			for idx, item := range items {
				if err := validateJSONSchemaValue(item, itemSchema, fmt.Sprintf("%s[%d]", path, idx)); err != nil {
					return err
				}
			}
		case "string":
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s must be a string", path)
			}
		case "number":
			if _, ok := value.(float64); !ok {
				return fmt.Errorf("%s must be a number", path)
			}
		case "integer":
			number, ok := value.(float64)
			if !ok || math.Trunc(number) != number {
				return fmt.Errorf("%s must be an integer", path)
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s must be a boolean", path)
			}
		case "null":
			if value != nil {
				return fmt.Errorf("%s must be null", path)
			}
		}
	}
	return nil
}

func jsonEqual(left, right any) bool {
	lb, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rb, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return string(lb) == string(rb)
}

func extractHTTPToolCalls(content string, tools []httpTool) ([]httpToolCall, bool) {
	if len(tools) == 0 {
		return nil, false
	}
	allowed := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &payload); err != nil {
		return nil, false
	}
	rawCalls, ok := payload["tool_calls"]
	if !ok {
		if rawCall, hasSingle := payload["tool_call"]; hasSingle {
			rawCalls = []any{rawCall}
			ok = true
		}
	}
	if !ok {
		return nil, false
	}
	items, ok := rawCalls.([]any)
	if !ok {
		return nil, false
	}
	calls := make([]httpToolCall, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		name := strings.TrimSpace(stringValue(entry["name"]))
		if function, ok := entry["function"].(map[string]any); ok && name == "" {
			name = strings.TrimSpace(stringValue(function["name"]))
			entry = function
		}
		if _, ok := allowed[name]; !ok {
			return nil, false
		}
		argumentsRaw, hasArguments := entry["arguments"]
		if !hasArguments {
			argumentsRaw = map[string]any{}
		}
		var arguments string
		switch value := argumentsRaw.(type) {
		case string:
			arguments = strings.TrimSpace(value)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, false
			}
			arguments = string(encoded)
		}
		calls = append(calls, httpToolCall{
			ID:   fmt.Sprintf("call_%d", time.Now().UnixNano()+int64(len(calls))),
			Type: "function",
			Function: httpToolCallFunction{
				Name:      name,
				Arguments: arguments,
			},
		})
	}
	return calls, len(calls) > 0
}
