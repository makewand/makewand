package router

import (
	"encoding/json"
	"fmt"
	"math"
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
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return "codex"
	default:
		return model
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
		arguments := "{}"
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
