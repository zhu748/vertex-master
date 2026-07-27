package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutputFromGeminiChunkUsageOnly(t *testing.T) {
	out := outputFromGeminiChunk(map[string]any{
		"usageMetadata": map[string]any{
			"promptTokenCount":        float64(10),
			"candidatesTokenCount":    float64(20),
			"thoughtsTokenCount":      float64(5),
			"cachedContentTokenCount": float64(4),
			"totalTokenCount":         float64(35),
		},
	})
	if out.Input != 10 || out.Output != 25 || out.Total != 35 ||
		out.CachedInputTokens != 4 || out.ReasoningTokens != 5 {
		t.Fatalf("usage-only Gemini 帧转换错误: %+v", out)
	}
}

func TestOutputFromGeminiChunkUsesRealCandidateCountWithoutUsage(t *testing.T) {
	out := outputFromGeminiChunk(map[string]any{
		"candidates": []any{map[string]any{
			"tokenCount": "8",
			"content": map[string]any{
				"role": "model", "parts": []any{map[string]any{"text": "hello"}},
			},
		}},
	})
	if out.Input != 0 || out.Output != 8 || out.Total != 0 {
		t.Fatalf("candidate tokenCount 应只作为真实输出统计，不能估算输入或总量: %+v", out)
	}
}

func TestNormalizeStreamingGeminiUsageForRikkaHub(t *testing.T) {
	data := map[string]any{
		"usageMetadata": map[string]any{"totalTokenCount": float64(84)},
	}
	lastCandidate := map[string]any{"tokenCount": "8"}
	normalizeStreamingGeminiUsage(data, &lastCandidate)

	candidates, ok := data["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		t.Fatalf("metadata-only usage 帧没有补空 candidate: %#v", data)
	}
	usage := data["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != 76 || usage["candidatesTokenCount"] != 8 || usage["totalTokenCount"] != float64(84) {
		t.Fatalf("Gemini usage 分项未补齐: %#v", usage)
	}
	content := candidates[0].(map[string]any)["content"].(map[string]any)
	if parts := content["parts"].([]any); len(parts) != 0 {
		t.Fatalf("兼容 candidate 不应重复正文: %#v", candidates[0])
	}
}

func TestProtocolStreamStatesConsumeUsageOnlyFrame(t *testing.T) {
	usage := protocolOutput{
		Input: 10, Output: 25, Total: 35, CachedInputTokens: 4, ReasoningTokens: 5,
	}

	responses := &responsesStreamState{}
	responses.consume(usage)
	if responses.out.Input != 10 || responses.out.Output != 25 || responses.out.Total != 35 ||
		responses.out.CachedInputTokens != 4 || responses.out.ReasoningTokens != 5 {
		t.Fatalf("Responses 流丢失独立 usage 帧: %+v", responses.out)
	}
	response := buildResponsesResponse(map[string]any{}, "m", "resp_test", responses.out)
	responseUsage := response["usage"].(map[string]any)
	inputDetails := responseUsage["input_tokens_details"].(map[string]any)
	outputDetails := responseUsage["output_tokens_details"].(map[string]any)
	if inputDetails["cached_tokens"] != 4 || outputDetails["reasoning_tokens"] != 5 {
		t.Fatalf("Responses usage 细分统计丢失: %#v", responseUsage)
	}

	anthropic := &anthropicStreamState{}
	anthropic.consume(usage)
	if anthropic.out.Input != 10 || anthropic.out.Output != 25 || anthropic.out.Total != 35 {
		t.Fatalf("Anthropic 流丢失独立 usage 帧: %+v", anthropic.out)
	}
}

func TestAnthropicStreamCoalescesThinkingDeltas(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := &anthropicStreamState{
		sw: newSSEWriter(recorder, "text/event-stream"), id: "msg_test", model: "gemini-test",
	}
	state.consume(protocolOutput{Reasoning: "first "})
	state.consume(protocolOutput{Reasoning: "second"})
	state.consume(protocolOutput{Text: "answer"})
	state.finish()

	stream := recorder.Body.String()
	if strings.Count(stream, `"type":"thinking_delta"`) != 2 {
		t.Fatalf("两个思考增量应保留在同一个块中: %s", stream)
	}
	if strings.Count(stream, `"type":"signature_delta"`) != 1 {
		t.Fatalf("连续思考块只能发送一个完整签名: %s", stream)
	}
	if strings.Count(stream, "event: content_block_start") != 2 ||
		strings.Count(stream, "event: content_block_stop") != 2 {
		t.Fatalf("应只有一个 thinking 块和一个 text 块: %s", stream)
	}
}
