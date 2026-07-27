package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
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

func mergeProtocolOutput(dst *protocolOutput, chunk protocolOutput) {
	if dst == nil {
		return
	}
	dst.Text += chunk.Text
	dst.Reasoning += chunk.Reasoning
	dst.ToolCalls = append(dst.ToolCalls, chunk.ToolCalls...)
	if chunk.Finish != "" {
		dst.Finish = chunk.Finish
	}
	if chunk.Input > 0 {
		dst.Input = chunk.Input
	}
	if chunk.Output > 0 {
		dst.Output = chunk.Output
	}
	if chunk.Total > 0 {
		dst.Total = chunk.Total
	}
	if chunk.CachedInputTokens > 0 {
		dst.CachedInputTokens = chunk.CachedInputTokens
	}
	if chunk.ReasoningTokens > 0 {
		dst.ReasoningTokens = chunk.ReasoningTokens
	}
}

// completeProtocolUsage 只补齐上游缺失的 token 字段。匿名 Vertex 流有时完全
// 不返回 usageMetadata；这时用项目已有的本地计数器生成近似值，使客户端至少能
// 展示输入/输出用量。上游给出的非零统计始终优先，不会被估算值覆盖。
func completeProtocolUsage(
	ctx context.Context,
	vc *vertex.VertexAIClient,
	model string,
	payload map[string]any,
	out protocolOutput,
) protocolOutput {
	if out.Input == 0 && out.Total > out.Output && out.Output > 0 {
		out.Input = out.Total - out.Output
	}
	if out.Output == 0 && out.Total > out.Input && out.Input > 0 {
		out.Output = out.Total - out.Input
	}

	if out.Input == 0 && vc != nil {
		contents := make([]any, 0, len(anySlice(payload["contents"]))+1)
		if systemInstruction, ok := payload["systemInstruction"].(map[string]any); ok {
			contents = append(contents, systemInstruction)
		}
		contents = append(contents, anySlice(payload["contents"])...)
		out.Input = vc.CountTokens(ctx, model, contents)
	}

	if out.Output == 0 && vc != nil {
		var generated strings.Builder
		generated.WriteString(out.Reasoning)
		generated.WriteString(out.Text)
		for _, call := range out.ToolCalls {
			generated.WriteString(call.Name)
			generated.WriteString(call.Arguments)
		}
		if generated.Len() > 0 {
			out.Output = vc.CountTokens(ctx, model, []any{map[string]any{
				"role":  "model",
				"parts": []any{map[string]any{"text": generated.String()}},
			}})
			if out.Output == 0 {
				out.Output = 1
			}
		}
	}

	if out.Total == 0 || out.Total < out.Input+out.Output {
		out.Total = out.Input + out.Output
	}
	return out
}

func applyOAIUsage(out protocolOutput, response map[string]any) {
	usage, _ := response["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
		response["usage"] = usage
	}
	if protocolIntValue(usage["prompt_tokens"]) == 0 {
		usage["prompt_tokens"] = out.Input
	}
	if protocolIntValue(usage["completion_tokens"]) == 0 {
		usage["completion_tokens"] = out.Output
	}
	if protocolIntValue(usage["total_tokens"]) == 0 {
		usage["total_tokens"] = out.Total
	}
	if out.CachedInputTokens > 0 {
		details, _ := usage["prompt_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
			usage["prompt_tokens_details"] = details
		}
		details["cached_tokens"] = out.CachedInputTokens
	}
	if out.ReasoningTokens > 0 {
		details, _ := usage["completion_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
			usage["completion_tokens_details"] = details
		}
		details["reasoning_tokens"] = out.ReasoningTokens
	}
}

func applyGeminiUsage(out protocolOutput, response map[string]any) {
	usage, _ := response["usageMetadata"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
		response["usageMetadata"] = usage
	}
	if protocolIntValue(usage["promptTokenCount"]) == 0 {
		usage["promptTokenCount"] = out.Input
	}
	if protocolIntValue(usage["candidatesTokenCount"])+protocolIntValue(usage["thoughtsTokenCount"]) == 0 {
		usage["candidatesTokenCount"] = out.Output
	}
	if protocolIntValue(usage["totalTokenCount"]) == 0 {
		usage["totalTokenCount"] = out.Total
	}
	if out.CachedInputTokens > 0 && protocolIntValue(usage["cachedContentTokenCount"]) == 0 {
		usage["cachedContentTokenCount"] = out.CachedInputTokens
	}
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
