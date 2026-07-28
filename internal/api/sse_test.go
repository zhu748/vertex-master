package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
