package api

import (
	"encoding/json"
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

func TestOutputFromGeminiChunkPreservesPartOrderAndInput(t *testing.T) {
	chunk := map[string]any{
		"candidates": []any{map[string]any{
			"finishReason": "STOP",
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "answer-1"},
				map[string]any{"text": "thought-1", "thought": true},
				map[string]any{"functionCall": map[string]any{
					"id": "call_1", "name": "lookup", "args": map[string]any{"q": "x"},
				}},
				map[string]any{"text": "answer-2"},
				map[string]any{"text": "thought-2", "thought": true},
			}},
		}},
	}
	before, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}

	out := outputFromGeminiChunk(chunk)
	if out.Text != "answer-1answer-2" || out.Reasoning != "thought-1thought-2" || out.Finish != "STOP" {
		t.Fatalf("part channels/order changed: %+v", out)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "call_1" || out.ToolCalls[0].Name != "lookup" ||
		out.ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Fatalf("tool call changed: %+v", out.ToolCalls)
	}
	after, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("input chunk was mutated:\n before: %s\n after:  %s", before, after)
	}
}

func TestNormalizeStreamingGeminiUsageForRikkaHub(t *testing.T) {
	data := map[string]any{
		"usageMetadata": map[string]any{"totalTokenCount": float64(84)},
	}
	lastCandidateTokenCount := 8
	normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)

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

func TestNormalizeStreamingGeminiUsageRecordsOnlyCandidateTokenCount(t *testing.T) {
	data := map[string]any{"candidates": []any{map[string]any{
		"tokenCount": "8",
		"content": map[string]any{"parts": []any{
			map[string]any{"text": strings.Repeat("large-output", 1024)},
		}},
	}}}
	lastCandidateTokenCount := 0
	normalizeStreamingGeminiUsage(data, &lastCandidateTokenCount)
	if lastCandidateTokenCount != 8 {
		t.Fatalf("candidate usage snapshot=%d, want 8", lastCandidateTokenCount)
	}

	usageOnly := map[string]any{"usageMetadata": map[string]any{"totalTokenCount": float64(84)}}
	normalizeStreamingGeminiUsage(usageOnly, &lastCandidateTokenCount)
	usage := usageOnly["usageMetadata"].(map[string]any)
	if usage["promptTokenCount"] != 76 || usage["candidatesTokenCount"] != 8 {
		t.Fatalf("minimal candidate snapshot did not complete usage: %#v", usage)
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
	if anthropic.out.Input != 10 || anthropic.out.Output != 25 || anthropic.out.Total != 35 ||
		anthropic.out.CachedInputTokens != 4 || anthropic.out.ReasoningTokens != 5 {
		t.Fatalf("Anthropic 流丢失独立 usage 帧: %+v", anthropic.out)
	}
}

func TestAnthropicMessageUsagePreservesCacheAndThinkingDetails(t *testing.T) {
	out := protocolOutput{
		Input: 10, Output: 25, Total: 35, CachedInputTokens: 4, ReasoningTokens: 5,
	}
	message := anthropicMessage("claude-test", "msg_test", out)
	usage := message["usage"].(map[string]any)
	if usage["input_tokens"] != 6 || usage["cache_read_input_tokens"] != 4 ||
		usage["cache_creation_input_tokens"] != 0 || usage["output_tokens"] != 25 {
		t.Fatalf("Anthropic cache usage mapping=%#v", usage)
	}
	details, ok := usage["output_tokens_details"].(map[string]any)
	if !ok || details["thinking_tokens"] != 5 {
		t.Fatalf("Anthropic thinking usage mapping=%#v", usage)
	}

	recorder := httptest.NewRecorder()
	state := &anthropicStreamState{sw: newSSEWriter(recorder, "text/event-stream")}
	state.consume(out)
	state.finish()
	stream := recorder.Body.String()
	for _, want := range []string{
		`"input_tokens":6`, `"cache_read_input_tokens":4`, `"output_tokens":25`,
		`"output_tokens_details":{"thinking_tokens":5}`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("Anthropic message_delta missing %q: %s", want, stream)
		}
	}
}

func TestAnthropicUsageClampsInvalidBreakdowns(t *testing.T) {
	usage := anthropicUsage(protocolOutput{
		Input: 3, Output: 2, CachedInputTokens: 5, ReasoningTokens: 7,
	})
	if usage["input_tokens"] != 0 || usage["cache_read_input_tokens"] != 3 || usage["output_tokens"] != 2 {
		t.Fatalf("clamped Anthropic usage=%#v", usage)
	}
	details := usage["output_tokens_details"].(map[string]any)
	if details["thinking_tokens"] != 2 {
		t.Fatalf("clamped thinking details=%#v", details)
	}

	empty := anthropicUsage(protocolOutput{Input: -1, Output: -2, CachedInputTokens: -3, ReasoningTokens: -4})
	if empty["input_tokens"] != 0 || empty["cache_read_input_tokens"] != 0 || empty["output_tokens"] != 0 {
		t.Fatalf("negative Anthropic usage=%#v", empty)
	}
	if _, exists := empty["output_tokens_details"]; exists {
		t.Fatalf("zero thinking details should be omitted: %#v", empty)
	}
}

func TestProtocolOutputAccumulatorPreservesContentAndMetadata(t *testing.T) {
	var accumulator protocolOutputAccumulator
	accumulator.Add(protocolOutput{
		Text: "hello ", Reasoning: "think ",
		ToolCalls:         []protocolToolCall{{ID: "call_1", Name: "lookup", Arguments: `{}`}},
		Finish:            "stop",
		Input:             11,
		Output:            12,
		Total:             23,
		CachedInputTokens: 3,
		ReasoningTokens:   4,
	})
	accumulator.Add(protocolOutput{Text: "world", Reasoning: "again", Output: 13, Total: 24})
	out := accumulator.Output()
	if out.Text != "hello world" || out.Reasoning != "think again" {
		t.Fatalf("accumulated content=%+v", out)
	}
	if len(out.ToolCalls) != 1 || out.Finish != "stop" || out.Input != 11 || out.Output != 13 ||
		out.Total != 24 || out.CachedInputTokens != 3 || out.ReasoningTokens != 4 {
		t.Fatalf("accumulated metadata=%+v", out)
	}
	if repeated := accumulator.Output(); repeated.Text != out.Text || repeated.Reasoning != out.Reasoning {
		t.Fatalf("repeated output changed content: %+v", repeated)
	}
	accumulator.Add(protocolOutput{Text: "!", Reasoning: "!"})
	continued := accumulator.Output()
	if continued.Text != "hello world!" || continued.Reasoning != "think again!" {
		t.Fatalf("content after Output/add cycle=%+v", continued)
	}
}

func TestResponsesStreamKeepsTextAcrossToolSeparatedBlocks(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := &responsesStreamState{
		sw: newSSEWriter(recorder, "text/event-stream"), id: "resp_test", model: "gemini-test",
		request: map[string]any{},
	}
	state.consume(protocolOutput{Text: "before"})
	firstOutput := state.output()
	if firstOutput.Text != "before" || state.output().Text != "before" {
		t.Fatalf("repeated output before tool=%+v", firstOutput)
	}
	state.consume(protocolOutput{ToolCalls: []protocolToolCall{{ID: "call_1", Name: "lookup", Arguments: `{}`}}})
	state.consume(protocolOutput{Text: "after"})
	if firstOutput.Text != "before" {
		t.Fatalf("later writes changed prior output snapshot: %+v", firstOutput)
	}
	state.finish()

	if state.out.Text != "beforeafter" {
		t.Fatalf("aggregate Responses text=%q", state.out.Text)
	}
	stream := recorder.Body.String()
	if !strings.Contains(stream, `"text":"before"`) || !strings.Contains(stream, `"text":"after"`) {
		t.Fatalf("tool-separated text blocks missing from stream: %s", stream)
	}
}

func TestAnthropicStreamOutputAcrossThinkingBlocks(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := &anthropicStreamState{
		sw: newSSEWriter(recorder, "text/event-stream"), id: "msg_test", model: "gemini-test",
	}
	state.consume(protocolOutput{Reasoning: "first"})
	firstOutput := state.output()
	if firstOutput.Reasoning != "first" || state.output().Reasoning != "first" {
		t.Fatalf("repeated first thinking output=%+v", firstOutput)
	}
	state.consume(protocolOutput{Text: "answer"})
	state.consume(protocolOutput{Reasoning: " second"})
	state.consume(protocolOutput{Reasoning: " third"})
	if firstOutput.Reasoning != "first" {
		t.Fatalf("later writes changed prior reasoning snapshot: %+v", firstOutput)
	}
	state.finish()

	if state.out.Reasoning != "first second third" || state.out.Text != "answer" {
		t.Fatalf("thinking blocks were not aggregated in order: %+v", state.out)
	}
	stream := recorder.Body.String()
	if strings.Count(stream, `"type":"signature_delta"`) != 2 {
		t.Fatalf("each thinking block should have one signature: %s", stream)
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
	if state.out.Reasoning != "first second" || state.out.Text != "answer" {
		t.Fatalf("Anthropic aggregate content=%+v", state.out)
	}

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
