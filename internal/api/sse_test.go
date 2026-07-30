package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

type responsesStaticJSONProbe struct {
	calls *int
}

func (probe responsesStaticJSONProbe) MarshalJSON() ([]byte, error) {
	(*probe.calls)++
	return []byte(`{"custom":"<ok>"}`), nil
}

type failingStreamResponseWriter struct {
	writes    int
	failAfter int
}

func (w *failingStreamResponseWriter) Header() http.Header { return nil }
func (w *failingStreamResponseWriter) WriteHeader(int)     {}
func (w *failingStreamResponseWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("client disconnected")
	}
	return len(data), nil
}

type deadlineStreamResponseWriter struct {
	header    http.Header
	deadlines []time.Time
	body      strings.Builder
}

func (w *deadlineStreamResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *deadlineStreamResponseWriter) WriteHeader(int) {}

func (w *deadlineStreamResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *deadlineStreamResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestSSEWriterRefreshesSlidingWriteDeadlineThroughMiddleware(t *testing.T) {
	underlying := &deadlineStreamResponseWriter{}
	wrapped := &statusWriter{ResponseWriter: underlying}
	sw := newSSEWriter(wrapped, "text/event-stream")

	started := time.Now()
	if !sw.writeData(map[string]any{"text": "first"}) || !sw.write("data: [DONE]\n\n") {
		t.Fatal("SSE writes unexpectedly failed")
	}
	if len(underlying.deadlines) != 2 {
		t.Fatalf("write deadline refreshes=%d, want 2", len(underlying.deadlines))
	}
	for _, deadline := range underlying.deadlines {
		if deadline.Before(started.Add(sseWriteTimeout-time.Second)) ||
			deadline.After(time.Now().Add(sseWriteTimeout+time.Second)) {
			t.Fatalf("unexpected sliding deadline %v", deadline)
		}
	}
}

func TestSSESerializationPreservesFramingAndHTML(t *testing.T) {
	payload := map[string]any{"text": "<b>你好</b> & ok"}
	const wantData = "data: {\"text\":\"<b>你好</b> & ok\"}\n\n"
	if got := sseEvent(payload); got != wantData {
		t.Fatalf("sseEvent=%q, want %q", got, wantData)
	}
	gemini := &GeminiHandler{}
	if got := gemini.geminiSSE(payload); got != wantData {
		t.Fatalf("geminiSSE=%q, want %q", got, wantData)
	}
	if got, want := namedSSE("message", payload),
		"event: message\ndata: {\"text\":\"<b>你好</b> & ok\"}\n\n"; got != want {
		t.Fatalf("namedSSE=%q, want %q", got, want)
	}
}

func TestSSESerializationFailureFallback(t *testing.T) {
	bad := map[string]any{"unsupported": func() {}}
	if got, want := sseEvent(bad), "data: {}\n\n"; got != want {
		t.Fatalf("sseEvent fallback=%q, want %q", got, want)
	}
	if got, want := namedSSE("error", bad),
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"serialization failed\"}}\n\n"; got != want {
		t.Fatalf("namedSSE fallback=%q, want %q", got, want)
	}
}

func TestSSEWriterNamedSerializationAndFallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload any
		want    string
	}{
		{
			name:    "valid",
			payload: map[string]any{"text": "<b>你好</b> & ok"},
			want:    "event: message\ndata: {\"text\":\"<b>你好</b> & ok\"}\n\n",
		},
		{
			name:    "serialization failure",
			payload: map[string]any{"unsupported": func() {}},
			want:    "event: message\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"serialization failed\"}}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if ok := newSSEWriter(recorder, "text/event-stream").writeNamed("message", test.payload); !ok {
				t.Fatal("writeNamed returned false")
			}
			if got := recorder.Body.String(); got != test.want {
				t.Fatalf("writeNamed=%q, want %q", got, test.want)
			}
		})
	}
}

func TestSSEWriterDataSerializationAndFallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload any
		want    string
	}{
		{
			name:    "valid",
			payload: map[string]any{"text": "<b>你好</b> & ok"},
			want:    "data: {\"text\":\"<b>你好</b> & ok\"}\n\n",
		},
		{
			name:    "serialization failure",
			payload: map[string]any{"unsupported": func() {}},
			want:    "data: {}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if ok := newSSEWriter(recorder, "text/event-stream").writeData(test.payload); !ok {
				t.Fatal("writeData returned false")
			}
			if got := recorder.Body.String(); got != test.want {
				t.Fatalf("writeData=%q, want %q", got, test.want)
			}
		})
	}
}

func TestOpenAIUsageTailMatchesCompatibilityJSON(t *testing.T) {
	out := protocolOutput{
		Input: 128, Output: 40, Total: 168, CachedInputTokens: 16, ReasoningTokens: 8,
	}
	for _, compatChoice := range []bool{false, true} {
		name := "empty choices"
		choices := []any{}
		if compatChoice {
			name = "RikkaHub compatibility choice"
			choices = []any{map[string]any{
				"delta": map[string]any{}, "finish_reason": nil, "index": 0,
			}}
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if !writeOAIStreamUsageValues(
				newSSEWriter(recorder, "text/event-stream"), out,
				"gemini<&中文>", "req<&中文>", 1234567890, compatChoice,
			) {
				t.Fatal("writeOAIStreamUsageValues returned false")
			}
			want := sseEvent(map[string]any{
				"choices": choices,
				"created": int64(1234567890),
				"id":      "chatcmpl-req<&中文>",
				"model":   "gemini<&中文>",
				"object":  "chat.completion.chunk",
				"usage": map[string]any{
					"completion_tokens":         40,
					"completion_tokens_details": map[string]any{"reasoning_tokens": 8},
					"prompt_tokens":             128,
					"prompt_tokens_details":     map[string]any{"cached_tokens": 16},
					"total_tokens":              168,
				},
			})
			if got := recorder.Body.String(); got != want {
				t.Fatalf("typed usage tail=%q, compatibility JSON=%q", got, want)
			}
		})
	}
}

func TestGeminiTextStreamEncoderMatchesGenericSSE(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
	}{
		{
			name: "plain text",
			data: map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"text": "<b>中文</b>"}}, "role": "model"},
			}}},
		},
		{
			name: "finish and index",
			data: map[string]any{"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "done"}}, "role": "model"},
				"finishReason": "STOP",
				"index":        float64(0),
			}}},
		},
		{
			name: "explicit false thought",
			data: map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{
					"text": "answer", "thought": false, "thoughtSignature": "",
				}}, "role": "model"},
				"index": 0,
			}}},
		},
		{
			name: "thinking with signature",
			data: map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{
					"text": "reason", "thought": true, "thoughtSignature": "sig",
				}}, "role": "model"},
				"index": float64(0),
			}}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			var encoder geminiTextStreamEncoder
			encoder.init()
			if !encoder.writeData(&sseWriter{w: recorder}, test.data) {
				t.Fatal("writeData returned false")
			}
			if got, want := recorder.Body.String(), sseEvent(test.data); got != want {
				t.Fatalf("typed Gemini SSE=%q, generic SSE=%q", got, want)
			}
		})
	}
}

func TestGeminiTextStreamEncoderRejectsExtendedShapes(t *testing.T) {
	tests := []map[string]any{
		{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{"text": "x"}}, "role": "model"},
			"index":   1,
		}}},
		{"candidates": []any{map[string]any{
			"content":       map[string]any{"parts": []any{map[string]any{"text": "x"}}, "role": "model"},
			"safetyRatings": []any{},
		}}},
		{"candidates": []any{map[string]any{
			"content": map[string]any{
				"parts": []any{map[string]any{"text": "x", "unexpected": true}}, "role": "model",
			},
		}}},
		{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{"text": "x"}}, "role": "model"},
		}}, "usageMetadata": map[string]any{"totalTokenCount": 1}},
	}

	var encoder geminiTextStreamEncoder
	encoder.init()
	for _, data := range tests {
		if encoder.prepare(data) {
			t.Fatalf("extended Gemini frame unexpectedly matched typed path: %#v", data)
		}
	}
}

func TestGeminiTextStreamEncoderResetsOptionalFields(t *testing.T) {
	var encoder geminiTextStreamEncoder
	encoder.init()
	recorder := httptest.NewRecorder()
	sw := &sseWriter{w: recorder}
	rich := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{
			"text": "reason", "thought": true, "thoughtSignature": "sig",
		}}, "role": "model"},
		"finishReason": "STOP",
		"index":        float64(0),
	}}}
	plain := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{"text": "answer"}}, "role": "model"},
	}}}
	if !encoder.writeData(sw, rich) || !encoder.writeData(sw, plain) {
		t.Fatal("writeData returned false")
	}
	if got, want := recorder.Body.String(), sseEvent(rich)+sseEvent(plain); got != want {
		t.Fatalf("reused typed Gemini SSE=%q, want %q", got, want)
	}
}

func TestGeminiTextStreamEncoderCanonicalMatchesCleanedMap(t *testing.T) {
	var encoder geminiTextStreamEncoder
	encoder.init()
	recorder := httptest.NewRecorder()
	sw := &sseWriter{w: recorder}
	if !encoder.writeCanonical(sw, "done", "", "STOP", false, false) ||
		!encoder.writeCanonical(sw, "next", "", "", true, true) {
		t.Fatal("canonical write returned false")
	}
	want := sseEvent(map[string]any{"candidates": []any{map[string]any{
		"content":      map[string]any{"parts": []any{map[string]any{"text": "done"}}, "role": "model"},
		"finishReason": "STOP",
	}}})
	want += sseEvent(map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{
			"text": "next", "thought": false, "thoughtSignature": "",
		}}, "role": "model"},
		"index": float64(0),
	}}})
	if got := recorder.Body.String(); got != want {
		t.Fatalf("canonical Gemini SSE=%q, generic SSE=%q", got, want)
	}
}

func TestGeminiTextStreamEncoderCanonicalPreservesPrefillTailPart(t *testing.T) {
	var encoder geminiTextStreamEncoder
	encoder.init()
	recorder := httptest.NewRecorder()
	sw := &sseWriter{w: recorder}
	if !encoder.writeCanonical(sw, "", "Ali", "STOP", false, true) {
		t.Fatal("canonical write returned false")
	}
	want := sseEvent(map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{
			map[string]any{"text": "", "thought": false, "thoughtSignature": ""},
			map[string]any{"text": "Ali"},
		}, "role": "model"},
		"finishReason": "STOP",
	}}})
	if got := recorder.Body.String(); got != want {
		t.Fatalf("canonical Gemini prefill tail SSE=%q, generic SSE=%q", got, want)
	}
}

func TestTypedProtocolDeltasPreserveFieldsAndHTML(t *testing.T) {
	const text = "<b>你好</b> & ok"

	anthropicRecorder := httptest.NewRecorder()
	anthropic := anthropicStreamState{
		sw:       newSSEWriter(anthropicRecorder, "text/event-stream"),
		openType: "text",
	}
	anthropic.consume(protocolOutput{Text: text})
	anthropicStream := anthropicRecorder.Body.String()
	for _, want := range []string{
		"event: content_block_delta",
		`"type":"content_block_delta"`,
		`"type":"text_delta"`,
		`"text":"<b>你好</b> & ok"`,
	} {
		if !strings.Contains(anthropicStream, want) {
			t.Fatalf("Anthropic typed delta missing %q: %s", want, anthropicStream)
		}
	}

	responsesRecorder := httptest.NewRecorder()
	responses := responsesStreamState{
		sw:       newSSEWriter(responsesRecorder, "text/event-stream"),
		textID:   "msg_test",
		textOpen: true,
	}
	responses.consume(protocolOutput{Text: text})
	responsesStream := responsesRecorder.Body.String()
	for _, want := range []string{
		"event: response.output_text.delta",
		`"type":"response.output_text.delta"`,
		`"sequence_number":1`,
		`"delta":"<b>你好</b> & ok"`,
		`"logprobs":[]`,
	} {
		if !strings.Contains(responsesStream, want) {
			t.Fatalf("Responses typed delta missing %q: %s", want, responsesStream)
		}
	}
}

func TestAnthropicTypedBlockBoundaryEventsMatchGenericSSE(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*anthropicStreamState) anthropicContentBlockStart
		generic map[string]any
	}{
		{
			name: "text",
			prepare: func(state *anthropicStreamState) anthropicContentBlockStart {
				return anthropicContentBlockStart{Text: &state.emptyString, Type: "text"}
			},
			generic: map[string]any{"type": "text", "text": ""},
		},
		{
			name: "thinking",
			prepare: func(state *anthropicStreamState) anthropicContentBlockStart {
				return anthropicContentBlockStart{
					Signature: &state.emptyString, Thinking: &state.emptyString, Type: "thinking",
				}
			},
			generic: map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		},
		{
			name: "tool",
			prepare: func(state *anthropicStreamState) anthropicContentBlockStart {
				state.toolID = "toolu_<中文>"
				state.toolName = "lookup&find"
				return anthropicContentBlockStart{
					ID: &state.toolID, Input: &state.emptyObject, Name: &state.toolName, Type: "tool_use",
				}
			},
			generic: map[string]any{
				"type": "tool_use", "id": "toolu_<中文>", "name": "lookup&find", "input": map[string]any{},
			},
		},
		{
			name: "tool with empty identifiers",
			prepare: func(state *anthropicStreamState) anthropicContentBlockStart {
				return anthropicContentBlockStart{
					ID: &state.toolID, Input: &state.emptyObject, Name: &state.toolName, Type: "tool_use",
				}
			},
			generic: map[string]any{"type": "tool_use", "id": "", "name": "", "input": map[string]any{}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			state := anthropicStreamState{sw: &sseWriter{w: recorder}, index: 7}
			state.emitContentBlockStart(test.prepare(&state))
			want := namedSSE("content_block_start", map[string]any{
				"content_block": test.generic, "index": 7, "type": "content_block_start",
			})
			if got := recorder.Body.String(); got != want {
				t.Fatalf("typed start event=%q, generic event=%q", got, want)
			}

			recorder.Body.Reset()
			state.emitContentBlockStop()
			want = namedSSE("content_block_stop", map[string]any{
				"index": 7, "type": "content_block_stop",
			})
			if got := recorder.Body.String(); got != want {
				t.Fatalf("typed stop event=%q, generic event=%q", got, want)
			}
		})
	}
}

func TestAnthropicTypedLifecycleEventsMatchGenericSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := anthropicStreamState{
		sw: &sseWriter{w: recorder}, id: "msg_<中文>", model: "gemini<&>",
		out: protocolOutput{
			Finish: "length", Input: 10, Output: 5, CachedInputTokens: 4, ReasoningTokens: 2,
		},
	}
	state.start()
	state.finish()

	startUsage := map[string]any{
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		"input_tokens": 0, "output_tokens": 0,
	}
	want := namedSSE("message_start", map[string]any{
		"message": map[string]any{
			"content": []any{}, "id": "msg_<中文>", "model": "gemini<&>", "role": "assistant",
			"stop_reason": nil, "stop_sequence": nil, "type": "message", "usage": startUsage,
		},
		"type": "message_start",
	})
	deltaUsage := map[string]any{
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 4,
		"input_tokens": 6, "output_tokens": 5,
		"output_tokens_details": map[string]any{"thinking_tokens": 2},
	}
	want += namedSSE("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": "max_tokens", "stop_sequence": nil},
		"type":  "message_delta", "usage": deltaUsage,
	})
	want += namedSSE("message_stop", map[string]any{"type": "message_stop"})
	if got := recorder.Body.String(); got != want {
		t.Fatalf("typed Anthropic lifecycle=%q, generic lifecycle=%q", got, want)
	}

	recorder.Body.Reset()
	failure := vertex.NewInvalidArgumentError("bad <参数>")
	state = anthropicStreamState{sw: &sseWriter{w: recorder}}
	state.fail(failure)
	want = namedSSE("error", map[string]any{
		"error": map[string]any{
			"message": vertex.FriendlyErrorMessage(failure), "type": anthropicErrorType(failure),
		},
		"type": "error",
	})
	if got := recorder.Body.String(); got != want {
		t.Fatalf("typed Anthropic error=%q, generic error=%q", got, want)
	}
}

func TestAnthropicTypedCompletedMessageMatchesGenericJSON(t *testing.T) {
	tests := []struct {
		name string
		out  protocolOutput
	}{
		{name: "empty text", out: protocolOutput{}},
		{
			name: "thinking text and tools",
			out: protocolOutput{
				Reasoning: "<思考> & reason", Text: "<完成> & answer",
				ToolCalls: []protocolToolCall{
					{ID: "toolu_1", Name: "lookup", Arguments: `{"q":"<北京>"}`},
					{ID: "toolu_2", Name: "empty", Arguments: ""},
					{ID: "toolu_3", Name: "invalid", Arguments: "{broken"},
				},
				Finish: "tool_calls", Input: 10, Output: 6, CachedInputTokens: 3, ReasoningTokens: 2,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := anthropicMessage("gemini<&>", "msg_<中文>", test.out)
			content := make([]any, 0, len(message.Content))
			if test.out.Reasoning != "" {
				content = append(content, map[string]any{
					"signature": thinkingSignature(test.out.Reasoning),
					"thinking":  test.out.Reasoning,
					"type":      "thinking",
				})
			}
			if test.out.Text != "" || len(test.out.ToolCalls) == 0 {
				content = append(content, map[string]any{"text": test.out.Text, "type": "text"})
			}
			for _, call := range test.out.ToolCalls {
				content = append(content, map[string]any{
					"id": call.ID, "input": jsonValue(call.Arguments), "name": call.Name, "type": "tool_use",
				})
			}
			usage := map[string]any{
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     min(max(test.out.CachedInputTokens, 0), max(test.out.Input, 0)),
				"input_tokens":                max(test.out.Input-test.out.CachedInputTokens, 0),
				"output_tokens":               max(test.out.Output, 0),
			}
			if thinking := min(max(test.out.ReasoningTokens, 0), max(test.out.Output, 0)); thinking > 0 {
				usage["output_tokens_details"] = map[string]any{"thinking_tokens": thinking}
			}
			generic := map[string]any{
				"content": content, "id": "msg_<中文>", "model": "gemini<&>", "role": "assistant",
				"stop_reason":   anthropicStopReason(test.out.Finish, len(test.out.ToolCalls) > 0),
				"stop_sequence": nil, "type": "message", "usage": usage,
			}
			if got, want := namedSSE("message", message), namedSSE("message", generic); got != want {
				t.Fatalf("typed Anthropic message=%q, generic message=%q", got, want)
			}
		})
	}
}

func TestResponsesTypedFunctionCallEventsMatchGenericSSE(t *testing.T) {
	tests := []struct {
		name string
		call protocolToolCall
	}{
		{
			name: "namespaced",
			call: protocolToolCall{
				ID: "call_<中文>", Name: "lookup&find", Namespace: "mcp__demo", Arguments: `{"q":"<北京>"}`,
			},
		},
		{name: "empty optional and identifiers", call: protocolToolCall{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			state := responsesStreamState{sw: &sseWriter{w: recorder}, sequence: 10, outputIndex: 3}
			const itemID = "fc_<item>"
			state.emitFunctionCallItem("response.output_item.added", "in_progress", itemID, test.call, "")
			state.emitFunctionCallArguments("response.function_call_arguments.delta", itemID, test.call.Arguments)
			state.emitFunctionCallArguments("response.function_call_arguments.done", itemID, test.call.Arguments)
			state.emitFunctionCallItem("response.output_item.done", "completed", itemID, test.call, test.call.Arguments)

			item := func(status, arguments string) map[string]any {
				value := map[string]any{
					"arguments": arguments,
					"call_id":   test.call.ID,
					"id":        itemID,
					"name":      test.call.Name,
					"status":    status,
					"type":      "function_call",
				}
				if test.call.Namespace != "" {
					value["namespace"] = test.call.Namespace
				}
				return value
			}
			want := namedSSE("response.output_item.added", map[string]any{
				"item": item("in_progress", ""), "output_index": 3,
				"sequence_number": 11, "type": "response.output_item.added",
			})
			want += namedSSE("response.function_call_arguments.delta", map[string]any{
				"delta": test.call.Arguments, "item_id": itemID, "output_index": 3,
				"sequence_number": 12, "type": "response.function_call_arguments.delta",
			})
			want += namedSSE("response.function_call_arguments.done", map[string]any{
				"arguments": test.call.Arguments, "item_id": itemID, "output_index": 3,
				"sequence_number": 13, "type": "response.function_call_arguments.done",
			})
			want += namedSSE("response.output_item.done", map[string]any{
				"item": item("completed", test.call.Arguments), "output_index": 3,
				"sequence_number": 14, "type": "response.output_item.done",
			})
			if got := recorder.Body.String(); got != want {
				t.Fatalf("typed Responses function events=%q, generic events=%q", got, want)
			}
		})
	}
}

func TestResponsesTypedTextBoundaryEventsMatchGenericSSE(t *testing.T) {
	for _, text := range []string{"<b>中文</b> & done", ""} {
		t.Run(text, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			state := responsesStreamState{
				sw: &sseWriter{w: recorder}, sequence: 20, outputIndex: 3, textID: "msg_<中文>",
			}
			state.emitTextBlockStart()
			state.emitTextBlockDone(text)

			emptyPart := map[string]any{
				"annotations": []any{}, "logprobs": []any{}, "text": "", "type": "output_text",
			}
			part := map[string]any{
				"annotations": []any{}, "logprobs": []any{}, "text": text, "type": "output_text",
			}
			want := namedSSE("response.output_item.added", map[string]any{
				"item": map[string]any{
					"content": []any{}, "id": "msg_<中文>", "role": "assistant",
					"status": "in_progress", "type": "message",
				},
				"output_index": 3, "sequence_number": 21, "type": "response.output_item.added",
			})
			want += namedSSE("response.content_part.added", map[string]any{
				"content_index": 0, "item_id": "msg_<中文>", "output_index": 3,
				"part": emptyPart, "sequence_number": 22, "type": "response.content_part.added",
			})
			want += namedSSE("response.output_text.done", map[string]any{
				"annotations": []any{}, "content_index": 0, "item_id": "msg_<中文>",
				"logprobs": []any{}, "output_index": 3, "sequence_number": 23,
				"text": text, "type": "response.output_text.done",
			})
			want += namedSSE("response.content_part.done", map[string]any{
				"content_index": 0, "item_id": "msg_<中文>", "output_index": 3,
				"part": part, "sequence_number": 24, "type": "response.content_part.done",
			})
			want += namedSSE("response.output_item.done", map[string]any{
				"item": map[string]any{
					"content": []any{part}, "id": "msg_<中文>", "role": "assistant",
					"status": "completed", "type": "message",
				},
				"output_index": 3, "sequence_number": 25, "type": "response.output_item.done",
			})
			if got := recorder.Body.String(); got != want {
				t.Fatalf("typed Responses text events=%q, generic events=%q", got, want)
			}
		})
	}
}

func TestResponsesTypedEventStateIsAllocatedOnDemand(t *testing.T) {
	t.Run("text only", func(t *testing.T) {
		state := responsesStreamState{
			sw: &sseWriter{w: httptest.NewRecorder()}, textID: "msg_test", textOpen: true,
		}
		state.consume(protocolOutput{Text: "hello"})
		if state.textEvents == nil {
			t.Fatal("text stream did not initialize its reusable event state")
		}
		if state.functionEvents != nil {
			t.Fatal("text-only stream initialized function-call event state")
		}
	})

	t.Run("function only", func(t *testing.T) {
		state := responsesStreamState{sw: &sseWriter{w: httptest.NewRecorder()}}
		state.consume(protocolOutput{ToolCalls: []protocolToolCall{{
			ID: "call_test", Name: "lookup", Arguments: `{}`,
		}}})
		if state.functionEvents == nil {
			t.Fatal("function-call stream did not initialize its reusable event state")
		}
		if state.textEvents != nil {
			t.Fatal("function-only stream initialized text event state")
		}
	})

	t.Run("mixed", func(t *testing.T) {
		state := responsesStreamState{sw: &sseWriter{w: httptest.NewRecorder()}}
		state.consume(protocolOutput{Text: "before", ToolCalls: []protocolToolCall{{
			ID: "call_test", Name: "lookup", Arguments: `{}`,
		}}})
		if state.textEvents == nil || state.functionEvents == nil {
			t.Fatal("mixed stream must retain both reusable event states")
		}
	})
}

func TestResponsesTypedLifecycleEventsMatchGenericSSE(t *testing.T) {
	request := map[string]any{
		"instructions":         "<系统提示>",
		"max_output_tokens":    256,
		"metadata":             map[string]any{"trace": "<&>"},
		"parallel_tool_calls":  false,
		"previous_response_id": "resp_previous",
		"reasoning":            map[string]any{"effort": "high", "summary": nil},
		"store":                true,
		"temperature":          0.25,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                0.9,
		"truncation":           "disabled",
	}
	tests := []struct {
		name   string
		event  string
		status string
		out    protocolOutput
		error  *responsesResponseError
	}{
		{name: "in progress", event: "response.created", status: "in_progress"},
		{
			name: "completed", event: "response.completed",
			out: protocolOutput{Text: "<完成>", Input: 10, Output: 4, Total: 14, CachedInputTokens: 2},
		},
		{
			name: "incomplete", event: "response.incomplete",
			out: protocolOutput{Text: "partial", Finish: "length", Input: 10, Output: 4, Total: 14},
		},
		{
			name: "failed", event: "response.failed", status: "failed",
			error: &responsesResponseError{Code: "upstream_error", Message: "失败 <&>"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			state := responsesStreamState{
				sw: &sseWriter{w: recorder}, id: "resp_<中文>", model: "gemini<&>",
				request: request, sequence: 9,
			}
			var output []any
			if test.status == "in_progress" {
				output = []any{}
			}
			response := state.responseObject(test.status, test.out, output)
			if test.error != nil {
				state.lifecycleEventState().err = *test.error
				response.Error = &state.lifecycleEventState().err
			}
			state.emitResponse(test.event, response)

			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var genericResponse map[string]any
			if err := json.Unmarshal(encoded, &genericResponse); err != nil {
				t.Fatal(err)
			}
			want := namedSSE(test.event, map[string]any{
				"response": genericResponse, "sequence_number": 10, "type": test.event,
			})
			if got := recorder.Body.String(); got != want {
				t.Fatalf("typed lifecycle event=%q, generic event=%q", got, want)
			}
			if len(genericResponse) != 23 {
				t.Fatalf("Responses lifecycle field count=%d, want 23: %#v", len(genericResponse), genericResponse)
			}

			switch test.event {
			case "response.created":
				if genericResponse["status"] != "in_progress" || genericResponse["completed_at"] != nil ||
					genericResponse["usage"] != nil || len(genericResponse["output"].([]any)) != 0 {
					t.Fatalf("invalid in-progress response: %#v", genericResponse)
				}
			case "response.completed":
				usage := genericResponse["usage"].(map[string]any)
				if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(4) ||
					usage["total_tokens"] != float64(14) {
					t.Fatalf("invalid completed usage: %#v", usage)
				}
			case "response.incomplete":
				details := genericResponse["incomplete_details"].(map[string]any)
				if details["reason"] != "max_output_tokens" || genericResponse["status"] != "incomplete" {
					t.Fatalf("invalid incomplete response: %#v", genericResponse)
				}
			case "response.failed":
				failure := genericResponse["error"].(map[string]any)
				if failure["code"] != test.error.Code || failure["message"] != test.error.Message {
					t.Fatalf("invalid failed response: %#v", genericResponse)
				}
			}
		})
	}
}

func TestResponsesInitialLifecycleEncodingMatchesGenericSSE(t *testing.T) {
	tests := []struct {
		name      string
		request   map[string]any
		wantReuse bool
	}{
		{
			name:      "rich request",
			wantReuse: true,
			request: map[string]any{
				"instructions": "<系统提示>\u2028",
				"metadata":     map[string]any{"trace": "<&>", "source": "测试"},
				"reasoning":    map[string]any{"effort": "high", "summary": nil},
				"text":         map[string]any{"format": map[string]any{"type": "text"}},
				"tool_choice":  map[string]any{"type": "auto"},
				"tools": []any{map[string]any{
					"type": "function", "name": "lookup<&>",
					"parameters": map[string]any{"type": "object"},
				}},
			},
		},
		{
			name: "serialization failure",
			request: map[string]any{
				"temperature": func() any {
					cyclic := map[string]any{}
					cyclic["self"] = cyclic
					return cyclic
				}(),
			},
		},
		{
			name: "large response fallback",
			request: map[string]any{
				"instructions": strings.Repeat("x", maxReusableResponsesInitialJSONBytes),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actualRecorder := httptest.NewRecorder()
			actual := responsesStreamState{
				sw: &sseWriter{w: actualRecorder}, sequence: 8,
				request: test.request, model: "gemini<&>", id: "resp_<中文>",
			}
			response := actual.responseObject(
				"in_progress",
				protocolOutput{},
				[]any{},
			)
			if got := canReuseResponsesInitialJSON(response); got != test.wantReuse {
				t.Fatalf("canReuseResponsesInitialJSON()=%v, want %v", got, test.wantReuse)
			}
			actual.emitInitialResponses(response)

			genericRecorder := httptest.NewRecorder()
			generic := responsesStreamState{
				sw: &sseWriter{w: genericRecorder}, sequence: 8,
			}
			generic.emitResponse("response.created", response)
			if !generic.streamFailed() {
				generic.emitResponse("response.in_progress", response)
			}

			if got, want := actualRecorder.Body.String(), genericRecorder.Body.String(); got != want {
				t.Fatalf("cached initial lifecycle=%q, generic lifecycle=%q", got, want)
			}
			if actual.sequence != generic.sequence {
				t.Fatalf("cached sequence=%d, generic sequence=%d", actual.sequence, generic.sequence)
			}
		})
	}
}

func TestResponsesContentFilterProducesIncompleteLifecycle(t *testing.T) {
	response := buildResponsesResponse(
		map[string]any{},
		"gemini-test",
		"resp_test",
		protocolOutput{Text: "partial", Finish: "content_filter"},
	)
	if response.Status != "incomplete" || response.IncompleteDetails == nil ||
		response.IncompleteDetails.Reason != "content_filter" {
		t.Fatalf("content filter lifecycle=%#v", response)
	}
	item, ok := response.Output[0].(*responsesCompletedTextMessageItem)
	if !ok || item.Status != "incomplete" {
		t.Fatalf("content-filter output item must be incomplete: %#v", response.Output)
	}

	recorder := httptest.NewRecorder()
	state := responsesStreamState{
		sw:      newSSEWriter(recorder, "text/event-stream"),
		id:      "resp_stream",
		model:   "gemini-test",
		request: map[string]any{},
	}
	state.start()
	state.consume(protocolOutput{Text: "partial"})
	state.consume(protocolOutput{Finish: "content_filter"})
	state.finish()
	stream := recorder.Body.String()
	for _, want := range []string{
		"event: response.incomplete",
		`"reason":"content_filter"`,
		`"status":"incomplete"`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("content-filter stream missing %q: %s", want, stream)
		}
	}
}

func TestResponsesLifecycleReusesStaticResponseFields(t *testing.T) {
	request := map[string]any{
		"metadata":  map[string]any{"trace": "<中文>"},
		"reasoning": map[string]any{"effort": "high", "summary": nil},
		"text":      map[string]any{"format": map[string]any{"type": "text"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup",
		}},
	}
	state := responsesStreamState{
		sw: &sseWriter{w: httptest.NewRecorder()}, id: "resp_test", model: "gemini-test", request: request,
	}
	initial := state.responseObject("in_progress", protocolOutput{}, []any{})
	createdAt := initial.CreatedAt
	for name, value := range map[string]any{
		"metadata":  initial.Metadata,
		"reasoning": initial.Reasoning,
		"text":      initial.Text,
		"tools":     initial.Tools,
	} {
		if _, ok := value.(json.RawMessage); !ok {
			t.Fatalf("%s was not cached as raw JSON: %T", name, value)
		}
	}

	items := []any{map[string]any{"id": "msg_test", "type": "message"}}
	completed := state.responseObject("", protocolOutput{Input: 10, Output: 4, Total: 14}, items)
	if completed != initial {
		t.Fatal("lifecycle response object was replaced instead of reused")
	}
	if completed.CreatedAt != createdAt || completed.Status != "completed" || completed.Usage == nil ||
		completed.Usage.TotalTokens != 14 || len(completed.Output) != 1 {
		t.Fatalf("reused lifecycle response lost dynamic state: %#v", completed)
	}
	if _, ok := completed.Tools.(json.RawMessage); !ok {
		t.Fatalf("cached static tools were rebuilt: %T", completed.Tools)
	}
}

func TestCacheResponsesStaticJSONFastPathAndFallback(t *testing.T) {
	standard := map[string]any{
		"z": "<中文>",
		"a": []any{float64(1), true, nil},
	}
	want, err := jsonx.Marshal(standard)
	if err != nil {
		t.Fatal(err)
	}
	cachedValue := cacheResponsesStaticJSON(standard)
	cached, ok := cachedValue.(json.RawMessage)
	if !ok {
		t.Fatalf("standard JSON was not cached: %T", cachedValue)
	}
	if string(cached) != string(want) {
		t.Fatalf("cached JSON changed wire bytes:\n got: %s\nwant: %s", cached, want)
	}

	tool := map[string]any{
		"type": "function", "name": "lookup",
		"parameters": map[string]any{"type": "object"},
	}
	tools := make([]any, 16)
	for index := range tools {
		tools[index] = tool
	}
	want, err = jsonx.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	cached, ok = cacheResponsesStaticJSON(tools).(json.RawMessage)
	if !ok || string(cached) != string(want) {
		t.Fatalf("cached tool array changed wire bytes:\n got: %s\nwant: %s", cached, want)
	}

	calls := 0
	custom := map[string]any{"probe": responsesStaticJSONProbe{calls: &calls}}
	cached, ok = cacheResponsesStaticJSON(custom).(json.RawMessage)
	if !ok || string(cached) != `{"probe":{"custom":"<ok>"}}` || calls != 1 {
		t.Fatalf("custom Marshaler fallback failed: cached=%s calls=%d", cached, calls)
	}

	unsupported := map[string]any{"channel": make(chan int)}
	if _, ok := cacheResponsesStaticJSON(unsupported).(map[string]any); !ok {
		t.Fatal("encoding failure did not preserve the original value")
	}
	if _, ok := cacheResponsesStaticJSON(map[string]any{}).(map[string]any); !ok {
		t.Fatal("empty static objects should not be encoded")
	}
}

func TestResponsesTypedCompletedOutputItemsMatchGenericJSON(t *testing.T) {
	tests := []struct {
		name string
		out  protocolOutput
	}{
		{name: "empty text", out: protocolOutput{}},
		{
			name: "text and tools",
			out: protocolOutput{
				Text: "<完成> & ok",
				ToolCalls: []protocolToolCall{
					{ID: "call_plain", Name: "lookup", Arguments: `{"q":"plain"}`},
					{
						ID: "call_namespaced", Name: "search<&>", Namespace: "mcp__中文",
						Arguments: `{"q":"<北京>"}`,
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			items := responseOutputItems(test.out)
			generic := make([]any, 0, len(items))
			index := 0
			if test.out.Text != "" || len(test.out.ToolCalls) == 0 {
				message, ok := items[index].(*responsesCompletedTextMessageItem)
				if !ok {
					t.Fatalf("text output item type=%T", items[index])
				}
				part := map[string]any{
					"annotations": []any{}, "logprobs": []any{}, "text": test.out.Text, "type": "output_text",
				}
				generic = append(generic, map[string]any{
					"content": []any{part}, "id": message.ID, "role": "assistant",
					"status": "completed", "type": "message",
				})
				index++
			}
			for _, call := range test.out.ToolCalls {
				item, ok := items[index].(*responsesFunctionCallItem)
				if !ok {
					t.Fatalf("function output item type=%T", items[index])
				}
				value := map[string]any{
					"arguments": call.Arguments, "call_id": call.ID, "id": item.ID,
					"name": call.Name, "status": "completed", "type": "function_call",
				}
				if call.Namespace != "" {
					value["namespace"] = call.Namespace
				}
				generic = append(generic, value)
				index++
			}
			if got, want := namedSSE("output", items), namedSSE("output", generic); got != want {
				t.Fatalf("typed completed output=%q, generic output=%q", got, want)
			}
		})
	}
}

func TestProtocolStreamStatesStopAfterClientWriteFailure(t *testing.T) {
	t.Run("Anthropic immediate failure", func(t *testing.T) {
		writer := &failingStreamResponseWriter{}
		state := anthropicStreamState{sw: &sseWriter{w: writer}, id: "msg_test", model: "model"}
		state.start()
		if state.connected() {
			t.Fatal("Anthropic state should remember the failed client write")
		}
		state.consume(protocolOutput{Text: "ignored", ToolCalls: []protocolToolCall{{ID: "call"}}})
		if writer.writes != 1 || state.openType != "" || state.index != 0 || len(state.out.ToolCalls) != 0 {
			t.Fatalf("Anthropic continued after disconnect: writes=%d open=%q index=%d tools=%d",
				writer.writes, state.openType, state.index, len(state.out.ToolCalls))
		}
		if state.emitPing() {
			t.Fatal("Anthropic ping should stop after a client disconnect")
		}
	})

	t.Run("Anthropic mid-block failure", func(t *testing.T) {
		writer := &failingStreamResponseWriter{failAfter: 1}
		state := anthropicStreamState{sw: &sseWriter{w: writer}}
		state.consume(protocolOutput{ToolCalls: []protocolToolCall{{
			ID: "call", Name: "lookup", Arguments: `{}`,
		}}})
		if state.connected() || writer.writes != 2 || state.index != 0 || len(state.out.ToolCalls) != 0 {
			t.Fatalf("Anthropic tool block continued after disconnect: writes=%d index=%d tools=%d",
				writer.writes, state.index, len(state.out.ToolCalls))
		}
	})

	t.Run("Responses immediate failure", func(t *testing.T) {
		writer := &failingStreamResponseWriter{}
		state := responsesStreamState{sw: &sseWriter{w: writer}}
		state.emit("response.created", map[string]any{})
		if !state.streamFailed() {
			t.Fatal("Responses state should remember the failed client write")
		}
		state.consume(protocolOutput{Text: "ignored", ToolCalls: []protocolToolCall{{ID: "call"}}})
		if writer.writes != 1 || state.outputIndex != 0 || len(state.items) != 0 || len(state.out.ToolCalls) != 0 {
			t.Fatalf("Responses continued after disconnect: writes=%d index=%d items=%d tools=%d",
				writer.writes, state.outputIndex, len(state.items), len(state.out.ToolCalls))
		}
	})

	t.Run("Responses mid-block failure", func(t *testing.T) {
		writer := &failingStreamResponseWriter{failAfter: 1}
		state := responsesStreamState{sw: &sseWriter{w: writer}}
		state.consume(protocolOutput{Text: "ignored"})
		if !state.streamFailed() || writer.writes != 2 || state.text.Len() != 0 ||
			state.outputIndex != 0 || len(state.items) != 0 {
			t.Fatalf("Responses text block continued after disconnect: writes=%d text=%d index=%d items=%d",
				writer.writes, state.text.Len(), state.outputIndex, len(state.items))
		}
	})
}
