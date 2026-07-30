package api

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type oaiOnlyResponseConverter struct {
	transform.ResponseConverter
}

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

func TestOutputFromCanonicalTextStreamDataAppliesPrefill(t *testing.T) {
	filter := transform.NewAssistantPrefillStreamFilter("Alice:")
	first := outputFromCanonicalTextStreamData(vertex.CanonicalTextStreamData{
		Text: "Ali", FinishReason: transform.FinishReasonUnspecified,
	}, filter)
	if first.Text != "" || first.Finish != transform.FinishReasonUnspecified {
		t.Fatalf("partial canonical prefill=%+v", first)
	}
	second := outputFromCanonicalTextStreamData(vertex.CanonicalTextStreamData{
		Text: "ce: hello", FinishReason: "STOP",
	}, filter)
	if second.Text != " hello" || second.Finish != "STOP" {
		t.Fatalf("completed canonical prefill=%+v", second)
	}
}

func TestProtocolIntValueRejectsInvalidUsageCounts(t *testing.T) {
	for _, value := range []any{
		float64(1.5),
		float64(-1),
		math.NaN(),
		math.Inf(1),
		math.MaxFloat64,
		float64(math.MaxInt) + 1,
		int64(-1),
		"-1",
	} {
		if got := protocolIntValue(value); got != 0 {
			t.Errorf("protocolIntValue(%v)=%d, want 0", value, got)
		}
	}

	for value, want := range map[any]int{
		42:          42,
		int64(42):   42,
		float64(42): 42,
		" 42 ":      42,
	} {
		if got := protocolIntValue(value); got != want {
			t.Errorf("protocolIntValue(%v)=%d, want %d", value, got, want)
		}
	}
}

func TestNormalizeProtocolUsageDoesNotOverflow(t *testing.T) {
	out := normalizeProtocolUsage(protocolOutput{Input: math.MaxInt, Output: 1})
	if out.Total != 0 {
		t.Fatalf("overflowing usage total=%d, want 0", out.Total)
	}

	out = outputFromGeminiChunkWithUsage(
		map[string]any{},
		transform.NormalizedUsage{PromptTokens: math.MaxInt, CompletionTokens: 1},
		true,
	)
	if out.Total != 0 {
		t.Fatalf("overflowing Gemini usage total=%d, want 0", out.Total)
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

func TestAnthropicCanonicalToolInputMatchesParsedWireFormat(t *testing.T) {
	geminiResponse := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{
				"functionCall": map[string]any{
					"id": "call_1", "name": "lookup",
					"args": map[string]any{"z": "<tag>", "a": map[string]any{"value": float64(1)}},
				},
			}}},
		}},
	}
	out := outputFromOAI(transform.GeminiJSONToOAIJSON(geminiResponse, "gemini-test"))
	if len(out.ToolCalls) != 1 || !out.ToolCalls[0].argumentsCanonical {
		t.Fatalf("locally serialized arguments were not marked canonical: %+v", out.ToolCalls)
	}

	fast, err := jsonx.Marshal(anthropicMessage("claude-test", "msg_fast", out))
	if err != nil {
		t.Fatal(err)
	}
	legacyOut := out
	legacyOut.ToolCalls = append([]protocolToolCall(nil), out.ToolCalls...)
	legacyOut.ToolCalls[0].argumentsCanonical = false
	legacy, err := jsonx.Marshal(anthropicMessage("claude-test", "msg_fast", legacyOut))
	if err != nil {
		t.Fatal(err)
	}
	if string(fast) != string(legacy) {
		t.Fatalf("canonical raw input changed Anthropic wire JSON:\n fast:   %s\n legacy: %s", fast, legacy)
	}

	rawOut := outputFromOAI(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"tool_calls": []any{map[string]any{
			"id": "call_2",
			"function": map[string]any{
				"name": "lookup", "arguments": `{"z":1,"a":2}`,
			},
		}}},
	}}})
	if len(rawOut.ToolCalls) != 1 || rawOut.ToolCalls[0].argumentsCanonical {
		t.Fatalf("client-provided argument string must keep the parsed compatibility path: %+v", rawOut.ToolCalls)
	}
}

func TestOutputFromResponseConverterMatchesOAICompatibilityPath(t *testing.T) {
	geminiResponse := map[string]any{
		"candidates": []any{map[string]any{
			"finishReason": "STOP",
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "thinking", "thought": true},
				map[string]any{"text": "answer"},
				map[string]any{"functionCall": map[string]any{
					"id": "toolu_1", "name": "lookup",
					"args": map[string]any{"z": "<tag>", "a": float64(1)},
				}},
			}},
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":        float64(20),
			"candidatesTokenCount":    float64(5),
			"thoughtsTokenCount":      float64(3),
			"cachedContentTokenCount": float64(4),
			"totalTokenCount":         float64(28),
		},
	}
	converter := transform.DefaultResponseConverter()
	legacy := outputFromOAI(converter.ToOAI(geminiResponse, "gemini-test"))
	direct := outputFromResponseConverter(converter, geminiResponse, "gemini-test")
	if !reflect.DeepEqual(direct, legacy) {
		t.Fatalf("canonical response path changed output:\n direct: %#v\n legacy: %#v", direct, legacy)
	}

	raw := outputFromResponseConverterWithRawArguments(converter, geminiResponse, "gemini-test")
	if len(raw.ToolCalls) != 1 ||
		len(raw.ToolCalls[0].argumentsRaw) == 0 ||
		raw.ToolCalls[0].argumentsValue != nil ||
		raw.ToolCalls[0].Arguments != "" {
		t.Fatalf("raw argument fast path was not preserved: %#v", raw.ToolCalls)
	}
	rawWire, err := jsonx.Marshal(anthropicMessage("claude-test", "msg_direct", raw))
	if err != nil {
		t.Fatal(err)
	}
	legacyWire, err := jsonx.Marshal(anthropicMessage("claude-test", "msg_direct", legacy))
	if err != nil {
		t.Fatal(err)
	}
	if string(rawWire) != string(legacyWire) {
		t.Fatalf("raw argument fast path changed Anthropic wire JSON:\n direct: %s\n legacy: %s", rawWire, legacyWire)
	}

	fallbackConverter := oaiOnlyResponseConverter{ResponseConverter: converter}
	if _, ok := any(fallbackConverter).(transform.CanonicalResponseConverter); ok {
		t.Fatal("fallback converter unexpectedly exposes canonical fast path")
	}
	fallback := outputFromResponseConverter(fallbackConverter, geminiResponse, "gemini-test")
	if !reflect.DeepEqual(fallback, legacy) {
		t.Fatalf("custom converter fallback changed output:\n fallback: %#v\n legacy:   %#v", fallback, legacy)
	}
	rawFallback := outputFromResponseConverterWithRawArguments(
		fallbackConverter, geminiResponse, "gemini-test",
	)
	if !reflect.DeepEqual(rawFallback, legacy) {
		t.Fatalf("raw custom converter fallback changed output:\n fallback: %#v\n legacy:   %#v", rawFallback, legacy)
	}
}

func TestOutputFromCanonicalResponseRawArgumentsFallback(t *testing.T) {
	arguments := map[string]any{"limit": 1}
	out := outputFromCanonicalResponse(transform.CanonicalResponse{
		ToolCalls: []transform.CanonicalToolCall{{
			ID:        "toolu_1",
			Name:      "lookup",
			Arguments: arguments,
		}},
	}, false)
	if len(out.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(out.ToolCalls))
	}
	toolCall := out.ToolCalls[0]
	if len(toolCall.argumentsRaw) != 0 ||
		toolCall.argumentsValue == nil ||
		toolCall.Arguments != "" ||
		toolCall.argumentsCanonical {
		t.Fatalf("unsupported argument value did not use compatibility path: %#v", toolCall)
	}
	if !reflect.DeepEqual(toolCall.argumentsValue, arguments) {
		t.Fatalf("fallback arguments = %#v, want %#v", toolCall.argumentsValue, arguments)
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
	if response.Usage == nil || response.Usage.InputTokensDetails.CachedTokens != 4 ||
		response.Usage.OutputTokensDetails.ReasoningTokens != 5 {
		t.Fatalf("Responses usage 细分统计丢失: %#v", response.Usage)
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
	usage := &message.Usage
	if usage.InputTokens != 6 || usage.CacheReadInputTokens != 4 ||
		usage.CacheCreationInputTokens != 0 || usage.OutputTokens != 25 {
		t.Fatalf("Anthropic cache usage mapping=%#v", usage)
	}
	if usage.OutputTokensDetails == nil || usage.OutputTokensDetails.ThinkingTokens != 5 {
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
	if usage.InputTokens != 0 || usage.CacheReadInputTokens != 3 || usage.OutputTokens != 2 {
		t.Fatalf("clamped Anthropic usage=%#v", usage)
	}
	if usage.OutputTokensDetails == nil || usage.OutputTokensDetails.ThinkingTokens != 2 {
		t.Fatalf("clamped thinking details=%#v", usage.OutputTokensDetails)
	}

	empty := anthropicUsage(protocolOutput{Input: -1, Output: -2, CachedInputTokens: -3, ReasoningTokens: -4})
	if empty.InputTokens != 0 || empty.CacheReadInputTokens != 0 || empty.OutputTokens != 0 {
		t.Fatalf("negative Anthropic usage=%#v", empty)
	}
	if empty.OutputTokensDetails != nil {
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
