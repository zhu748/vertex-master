package transform

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
)

var benchmarkSSELineResult string                      //nolint:gochecknoglobals
var benchmarkRealtimeChunkResult []string              //nolint:gochecknoglobals
var benchmarkExtractedToolCalls []any                  //nolint:gochecknoglobals
var benchmarkPreparedOpenAIStream openAIStreamPrepared //nolint:gochecknoglobals

type streamArgumentsJSONProbe struct {
	calls *int
}

func (probe streamArgumentsJSONProbe) MarshalJSON() ([]byte, error) {
	(*probe.calls)++
	return []byte(`{"custom":"<ok>"}`), nil
}

func BenchmarkSSELine(b *testing.B) {
	payload := map[string]any{
		"id":      "chatcmpl-benchmark",
		"object":  "chat.completion.chunk",
		"created": int64(1234567890),
		"model":   "gemini-benchmark",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": strings.Repeat("x", 256)},
			"finish_reason": nil,
		}},
	}
	b.ReportAllocs()
	for range b.N {
		benchmarkSSELineResult = sseLine(payload)
	}
}

func BenchmarkConvertRealtimeChunkMultiEvent(b *testing.B) {
	chunk := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"text": strings.Repeat("r", 128), "thought": true},
				map[string]any{"text": strings.Repeat("x", 256)},
			}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     100,
			"candidatesTokenCount": 50,
			"totalTokenCount":      150,
		},
	}
	b.ReportAllocs()
	for range b.N {
		benchmarkRealtimeChunkResult = ConvertRealtimeChunk(chunk, "gemini-benchmark", "benchmark", true)
	}
}

func BenchmarkConvertRealtimeChunkText(b *testing.B) {
	chunk := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"text": strings.Repeat("x", 256)},
			}},
			"finishReason": FinishReasonUnspecified,
		}},
	}
	b.ReportAllocs()
	for range b.N {
		benchmarkRealtimeChunkResult = ConvertRealtimeChunk(chunk, "gemini-benchmark", "benchmark", false)
	}
}

func BenchmarkExtractPartsToolCalls(b *testing.B) {
	parts := make([]any, 16)
	for index := range parts {
		parts[index] = map[string]any{"functionCall": map[string]any{
			"id": "call_benchmark", "name": "lookup",
			"args": map[string]any{"query": "benchmark", "index": float64(index)},
		}}
	}
	for _, test := range []struct {
		name      string
		forStream bool
	}{
		{name: "stream", forStream: true},
		{name: "non_stream", forStream: false},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if test.forStream {
					_, benchmarkExtractedToolCalls, _ = ExtractParts(parts, true)
				} else {
					_, benchmarkExtractedToolCalls, _ = extractResponseParts(parts)
				}
				if len(benchmarkExtractedToolCalls) != len(parts) {
					b.Fatal("unexpected tool call count")
				}
			}
		})
	}
}

func BenchmarkPrepareOpenAIStreamToolCalls(b *testing.B) {
	parts := make([]any, 16)
	for index := range parts {
		parts[index] = map[string]any{"functionCall": map[string]any{
			"id": "call_benchmark", "name": "lookup",
			"args": map[string]any{"query": "benchmark", "index": float64(index)},
		}}
	}
	chunk := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": parts},
	}}}
	b.ReportAllocs()
	for range b.N {
		benchmarkPreparedOpenAIStream = prepareOpenAIStreamChunk(chunk, false)
		if len(benchmarkPreparedOpenAIStream.toolCalls) != len(parts) {
			b.Fatal("unexpected tool call count")
		}
	}
}

func TestMarshalToolArgumentsFastPathAndFallback(t *testing.T) {
	for _, arguments := range []any{
		map[string]any{
			"z": "<中文>",
			"a": []any{float64(1), true, nil},
		},
		map[string]any{"integer": 1},
	} {
		want, err := jsonx.MarshalString(arguments)
		if err != nil {
			t.Fatal(err)
		}
		if got := marshalToolArguments(arguments); got != want {
			t.Fatalf("tool arguments changed:\n got: %s\nwant: %s", got, want)
		}
	}

	calls := 0
	custom := map[string]any{"probe": streamArgumentsJSONProbe{calls: &calls}}
	if got := marshalToolArguments(custom); got != `{"probe":{"custom":"<ok>"}}` || calls != 1 {
		t.Fatalf("custom Marshaler fallback failed: got=%s calls=%d", got, calls)
	}
	if got := marshalToolArguments(map[string]any{"channel": make(chan int)}); got != "" {
		t.Fatalf("encoding failure = %q, want empty compatibility value", got)
	}
}

func TestOpenAIStreamTypedToolCallsMatchLegacyWire(t *testing.T) {
	parts := []any{
		map[string]any{"functionCall": map[string]any{
			"id": "call_1", "name": "lookup",
			"args": map[string]any{"z": "<中文>", "a": float64(1)},
		}},
		map[string]any{"functionCall": map[string]any{
			"id": "call_2", "name": "lookup",
			"args": map[string]any{"nested": []any{true, nil, "value"}},
		}},
	}
	_, legacy, _ := ExtractParts(parts, true)
	for index, toolCall := range legacy {
		if _, ok := toolCall.(map[string]any); !ok {
			t.Fatalf("public compatibility tool call %d changed type: %T", index, toolCall)
		}
	}

	chunk := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": parts},
	}}}
	typed := prepareOpenAIStreamChunk(chunk, false).toolCalls
	for index := range typed {
		typed[index].Index = index
	}
	got, err := jsonx.MarshalString(typed)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonx.MarshalString(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("typed stream tool calls changed wire JSON:\n got: %s\nwant: %s", got, want)
	}
}

// 真流式增量转换：首帧带 role delta，内容帧带 content delta，UNSPECIFIED 不发 finish。
func TestConvertRealtimeChunk_FirstAndContent(t *testing.T) {
	chunk := map[string]any{"candidates": []any{
		map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "Hi"}}, "role": "model"},
			"finishReason": "FINISH_REASON_UNSPECIFIED",
		},
	}}
	events := ConvertRealtimeChunk(chunk, "gemini-3.1-flash", "req123", true)

	// 期望：role 事件 + content 事件，无 finish 事件（UNSPECIFIED 被过滤）。
	if len(events) != 2 {
		t.Fatalf("events=%d, want 2\n%v", len(events), events)
	}
	if !strings.Contains(events[0], `"role":"assistant"`) {
		t.Errorf("首帧应含 role delta: %s", events[0])
	}
	if !strings.Contains(events[1], `"content":"Hi"`) {
		t.Errorf("内容帧应含 content: %s", events[1])
	}
	for _, e := range events {
		if strings.Contains(e, `"finish_reason":"stop"`) || strings.Contains(e, `"finish_reason":"length"`) {
			t.Errorf("🔴 UNSPECIFIED 绝不能发真实 finish_reason（截断血泪教训）: %s", e)
		}
	}
}

// 红线：UNSPECIFIED 时 finish_reason 只能是 null（在 role 事件里），不能是真实终止值。
func TestConvertRealtimeChunk_UnspecifiedNoFinishEvent(t *testing.T) {
	chunk := map[string]any{"candidates": []any{
		map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "x"}}, "role": "model"},
			"finishReason": "FINISH_REASON_UNSPECIFIED",
		},
	}}
	events := ConvertRealtimeChunk(chunk, "m", "r", false)
	// 非首帧、只有内容 → 只发 content 事件，绝无 finish 事件。
	if len(events) != 1 {
		t.Fatalf("events=%d, want 1（只 content）\n%v", len(events), events)
	}
	if strings.Contains(events[0], `"finish_reason":"`) {
		t.Errorf("UNSPECIFIED 不应发任何带值的 finish_reason: %s", events[0])
	}
}

// 真实 finishReason（STOP）发 finish 事件，usage 使用独立空 choices 统计帧。
func TestConvertRealtimeChunk_RealFinishWithUsage(t *testing.T) {
	chunk := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "done"}}, "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount": float64(10), "candidatesTokenCount": float64(5), "totalTokenCount": float64(15),
		},
	}
	events := ConvertRealtimeChunk(chunk, "m", "r", false)
	// content 事件 + finish 事件 + usage 事件。
	if len(events) != 3 {
		t.Fatalf("events=%d, want 3\n%v", len(events), events)
	}
	if strings.Contains(events[0], `"usage"`) || strings.Contains(events[1], `"usage"`) {
		t.Fatalf("复用事件对象时 usage 泄漏到前序帧: %v", events)
	}
	finishEvt := events[1]
	if !strings.Contains(finishEvt, `"finish_reason":"stop"`) {
		t.Errorf("应发 finish_reason=stop: %s", finishEvt)
	}
	usageEvt := events[2]
	if !strings.Contains(usageEvt, `"choices":[]`) ||
		!strings.Contains(usageEvt, `"usage"`) ||
		!strings.Contains(usageEvt, `"total_tokens":15`) {
		t.Errorf("应发送独立 usage 统计帧: %s", usageEvt)
	}
}

func TestOpenAIStreamEncoderMatchesCompatibilityOutput(t *testing.T) {
	encoder := NewOpenAIStreamEncoder("m", "r")
	chunks := []struct {
		chunk   map[string]any
		isFirst bool
	}{
		{
			isFirst: true,
			chunk: map[string]any{
				"candidates": []any{map[string]any{
					"content": map[string]any{"parts": []any{
						map[string]any{"text": "thinking", "thought": true},
						map[string]any{"text": "answer"},
					}},
					"finishReason": "STOP",
				}},
				"usageMetadata": map[string]any{
					"promptTokenCount": 11, "candidatesTokenCount": 7, "totalTokenCount": 18,
				},
			},
		},
		{
			chunk: map[string]any{"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "next"}}},
				"finishReason": FinishReasonUnspecified,
			}}},
		},
	}
	for index, test := range chunks {
		var got []string
		result, ok := encoder.Emit(test.chunk, test.isFirst, func(payload any) bool {
			got = append(got, sseLine(payload))
			return true
		})
		if !ok {
			t.Fatalf("chunk %d unexpectedly aborted", index)
		}
		want := ConvertRealtimeChunk(test.chunk, "m", "r", test.isFirst)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("chunk %d direct events differ:\n got: %q\nwant: %q", index, got, want)
		}
		if index == 0 && (!result.HasContent || !result.HasFinish) {
			t.Fatalf("first chunk flags=%+v, want content and finish", result)
		}
		if index == 1 {
			if !result.HasContent || result.HasFinish {
				t.Fatalf("second chunk flags=%+v, want content without finish", result)
			}
			if strings.Contains(got[0], `"usage"`) {
				t.Fatalf("usage leaked from prior chunk: %s", got[0])
			}
		}
	}
}

func TestOpenAIStreamEncoderTextFastPathMatchesMapPath(t *testing.T) {
	for _, test := range []struct {
		name         string
		text         string
		finishReason string
		isFirst      bool
	}{
		{name: "first unspecified", text: "hello", finishReason: FinishReasonUnspecified, isFirst: true},
		{name: "real finish", text: "done", finishReason: "STOP"},
		{name: "finish only", finishReason: "MAX_TOKENS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			chunk := map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{
					"parts": []any{map[string]any{"text": test.text}},
					"role":  "model",
				},
				"finishReason": test.finishReason,
			}}}
			mapEncoder := NewOpenAIStreamEncoder("m", "r")
			textEncoder := NewOpenAIStreamEncoder("m", "r")
			var mapEvents, textEvents []string
			mapResult, mapOK := mapEncoder.Emit(chunk, test.isFirst, func(payload any) bool {
				mapEvents = append(mapEvents, sseLine(payload))
				return true
			})
			textResult, textOK := textEncoder.EmitText(
				test.text,
				test.finishReason,
				test.isFirst,
				func(payload any) bool {
					textEvents = append(textEvents, sseLine(payload))
					return true
				},
			)
			if mapOK != textOK || mapResult != textResult || !reflect.DeepEqual(mapEvents, textEvents) {
				t.Fatalf(
					"text fast path differs:\n map ok=%v result=%+v events=%q\ntext ok=%v result=%+v events=%q",
					mapOK, mapResult, mapEvents, textOK, textResult, textEvents,
				)
			}
		})
	}
}

func TestConvertRealtimeChunk_UsageOnlyFrame(t *testing.T) {
	chunk := map[string]any{
		"usageMetadata": map[string]any{
			"promptTokenCount": float64(11), "candidatesTokenCount": float64(7), "totalTokenCount": float64(18),
		},
	}
	events := ConvertRealtimeChunk(chunk, "m", "r", false)
	if len(events) != 1 {
		t.Fatalf("usage-only frame events=%d, want 1: %v", len(events), events)
	}
	if !strings.Contains(events[0], `"choices":[]`) || !strings.Contains(events[0], `"total_tokens":18`) {
		t.Fatalf("usage-only frame 未转换成 OpenAI 统计帧: %s", events[0])
	}
}

func TestConvertRealtimeChunk_UsageDetailsFallback(t *testing.T) {
	chunk := map[string]any{
		"usageMetadata": map[string]any{
			"totalTokenCount": "84",
			"promptTokensDetails": []any{
				map[string]any{"modality": "TEXT", "tokenCount": "76"},
			},
			"candidatesTokensDetails": []any{
				map[string]any{"modality": "TEXT", "tokens": "8"},
			},
		},
	}
	events := ConvertRealtimeChunk(chunk, "m", "r", false)
	if len(events) != 1 ||
		!strings.Contains(events[0], `"prompt_tokens":76`) ||
		!strings.Contains(events[0], `"completion_tokens":8`) ||
		!strings.Contains(events[0], `"total_tokens":84`) {
		t.Fatalf("usage details 未生成 RikkaHub 可识别的分项统计: %v", events)
	}
}

// MAX_TOKENS → length。
func TestConvertRealtimeChunk_MaxTokensLength(t *testing.T) {
	chunk := map[string]any{"candidates": []any{map[string]any{
		"content":      map[string]any{"parts": []any{map[string]any{"text": "y"}}, "role": "model"},
		"finishReason": "MAX_TOKENS",
	}}}
	events := ConvertRealtimeChunk(chunk, "m", "r", false)
	if len(events) == 0 {
		t.Fatalf("events is empty")
	}
	last := events[len(events)-1]
	if !strings.Contains(last, `"finish_reason":"length"`) {
		t.Errorf("MAX_TOKENS → length: %s", last)
	}
}

// SSE 行格式：data: {json}\n\n。
func TestSseLine_Format(t *testing.T) {
	line := sseLine(map[string]any{"a": 1})
	if !strings.HasPrefix(line, "data: ") {
		t.Errorf("SSE 行应以 'data: ' 开头: %q", line)
	}
	if !strings.HasSuffix(line, "\n\n") {
		t.Errorf("SSE 行应以 \\n\\n 结尾: %q", line)
	}
}

// 关 HTML 转义（红线⑥）：SSE 行里的 < > & 不应被转义。
func TestSseLine_NoHTMLEscape(t *testing.T) {
	line := sseLine(map[string]any{"x": "a<b>&c"})
	if !strings.Contains(line, "a<b>&c") {
		t.Errorf("SSE 应关 HTML 转义（红线⑥）: %q", line)
	}
}

// 工具调用流式：tool_calls delta 带 index 字段（_extract_parts for_stream=True）。
func TestConvertRealtimeChunk_ToolCall(t *testing.T) {
	chunk := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{
			map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "SF"}}},
		}, "role": "model"},
		"finishReason": "STOP",
	}}}
	events := ConvertRealtimeChunk(chunk, "m", "r", false)
	var toolEvt string
	for _, e := range events {
		// 找 delta 里带 tool_calls 数组的事件（避免误匹配 finish 事件里的 "finish_reason":"tool_calls"）。
		if strings.Contains(e, `"tool_calls":[`) {
			toolEvt = e
		}
	}
	if toolEvt == "" {
		t.Fatalf("应有 tool_calls 事件\n%v", events)
	}
	if !strings.Contains(toolEvt, `"index":0`) {
		t.Errorf("流式 tool_call 应带 index（M18）: %s", toolEvt)
	}
	if !strings.Contains(toolEvt, `"get_weather"`) {
		t.Errorf("tool_call 应含函数名: %s", toolEvt)
	}
	// STOP + 有 tool_call → finish_reason=tool_calls。
	if len(events) == 0 {
		t.Fatalf("events is empty")
	}
	last := events[len(events)-1]
	if !strings.Contains(last, `"finish_reason":"tool_calls"`) {
		t.Errorf("有工具调用应 finish_reason=tool_calls: %s", last)
	}
}

func TestOpenAIStreamEncoderTracksToolCallsAcrossFrames(t *testing.T) {
	encoder := NewOpenAIStreamEncoder("m", "r")
	var events []string
	emit := func(payload any) bool {
		events = append(events, sseLine(payload))
		return true
	}

	for index, name := range []string{"first_tool", "second_tool"} {
		chunk := map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{
				"functionCall": map[string]any{"name": name, "args": map[string]any{}},
			}}, "role": "model"},
		}}}
		if _, ok := encoder.Emit(chunk, index == 0, emit); !ok {
			t.Fatalf("tool frame %d emit failed", index)
		}
	}
	finish := map[string]any{"candidates": []any{map[string]any{
		"content":      map[string]any{"parts": []any{}, "role": "model"},
		"finishReason": "STOP",
	}}}
	if _, ok := encoder.Emit(finish, false, emit); !ok {
		t.Fatal("finish frame emit failed")
	}

	var toolEvents []string
	for _, event := range events {
		if strings.Contains(event, `"tool_calls":[`) {
			toolEvents = append(toolEvents, event)
		}
	}
	if len(toolEvents) != 2 ||
		!strings.Contains(toolEvents[0], `"index":0`) ||
		!strings.Contains(toolEvents[1], `"index":1`) {
		t.Fatalf("跨帧工具索引必须连续: %v", toolEvents)
	}
	if !strings.Contains(events[len(events)-1], `"finish_reason":"tool_calls"`) {
		t.Fatalf("STOP 独立末帧必须保留累计工具语义: %v", events)
	}
}
