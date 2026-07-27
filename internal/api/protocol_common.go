package api

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

type protocolToolCall struct {
	ID        string
	Name      string
	Namespace string
	Arguments string
}

type protocolOutput struct {
	Text              string
	Reasoning         string
	ToolCalls         []protocolToolCall
	Finish            string
	Input             int
	Output            int
	Total             int
	CachedInputTokens int
	ReasoningTokens   int
}

func decodeJSONObject(r io.Reader) (map[string]any, error) {
	var body map[string]any
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("request body must contain exactly one JSON value")
		}
		return nil, err
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, nil
}

func outputFromOAI(resp map[string]any) protocolOutput {
	var out protocolOutput
	choices, _ := resp["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		out.Finish, _ = choice["finish_reason"].(string)
		message, _ := choice["message"].(map[string]any)
		out.Text, _ = message["content"].(string)
		out.Reasoning, _ = message["reasoning_content"].(string)
		for _, raw := range anySlice(message["tool_calls"]) {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			id := stringValue(tc["id"])
			if id == "" {
				id = "call_" + reqID24()
			}
			out.ToolCalls = append(out.ToolCalls, protocolToolCall{
				ID: id, Name: stringValue(fn["name"]), Arguments: jsonString(fn["arguments"]),
			})
		}
	}
	if usage, ok := resp["usage"].(map[string]any); ok {
		out.Input = protocolIntValue(usage["prompt_tokens"])
		out.Output = protocolIntValue(usage["completion_tokens"])
		out.Total = protocolIntValue(usage["total_tokens"])
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			out.CachedInputTokens = protocolIntValue(details["cached_tokens"])
		}
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
			out.ReasoningTokens = protocolIntValue(details["reasoning_tokens"])
		}
	}
	if out.Total == 0 {
		out.Total = out.Input + out.Output
	}
	return out
}

func outputFromGeminiChunk(chunk map[string]any) protocolOutput {
	var out protocolOutput
	candidates := anySlice(chunk["candidates"])
	var candidate map[string]any
	if len(candidates) > 0 {
		candidate, _ = candidates[0].(map[string]any)
		out.Finish = stringValue(candidate["finishReason"])
		content, _ := candidate["content"].(map[string]any)
		for _, raw := range anySlice(content["parts"]) {
			part, _ := raw.(map[string]any)
			if part == nil {
				continue
			}
			if fc, ok := part["functionCall"].(map[string]any); ok && stringValue(fc["name"]) != "" {
				id := stringValue(fc["id"])
				if id == "" {
					id = "call_" + reqID24()
				}
				out.ToolCalls = append(out.ToolCalls, protocolToolCall{
					ID: id, Name: stringValue(fc["name"]), Arguments: jsonString(fc["args"]),
				})
				continue
			}
			text := stringValue(part["text"])
			if text == "" {
				continue
			}
			if protocolBoolValue(part["thought"]) {
				out.Reasoning += text
			} else {
				out.Text += text
			}
		}
	}
	if usage, ok := chunk["usageMetadata"].(map[string]any); ok {
		normalized := transform.ConvertUsageForCandidate(usage, candidate)
		out.Input = protocolIntValue(normalized["prompt_tokens"])
		out.Output = protocolIntValue(normalized["completion_tokens"])
		out.Total = protocolIntValue(normalized["total_tokens"])
		if details, ok := normalized["prompt_tokens_details"].(map[string]any); ok {
			out.CachedInputTokens = protocolIntValue(details["cached_tokens"])
		}
		if details, ok := normalized["completion_tokens_details"].(map[string]any); ok {
			out.ReasoningTokens = protocolIntValue(details["reasoning_tokens"])
		}
	}
	if out.Total == 0 {
		out.Total = out.Input + out.Output
	}
	return out
}

func anySlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	default:
		return []any{x}
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func protocolBoolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func protocolIntValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	if v == nil {
		return "{}"
	}
	data, err := jsonx.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func jsonValue(s string) any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal([]byte(s), &value); err != nil {
		return map[string]any{"raw": s}
	}
	return value
}

func namedSSE(event string, payload map[string]any) string {
	data, err := jsonx.Marshal(payload)
	if err != nil {
		data = []byte(`{"type":"error","error":{"type":"api_error","message":"serialization failed"}}`)
	}
	return "event: " + event + "\ndata: " + string(data) + "\n\n"
}
