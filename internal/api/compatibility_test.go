package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

func TestResponsesRequestConversion(t *testing.T) {
	body := map[string]any{
		"instructions": "Be concise.",
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "What is this?"},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="},
				},
			},
			map[string]any{
				"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`,
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call_1", "output": `{"answer":1}`,
			},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup", "description": "Lookup",
			"parameters": map[string]any{"type": "object"},
		}},
		"tool_choice": map[string]any{"type": "function", "name": "lookup"},
	}
	chat, err := responsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := chat["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages=%d, want 4: %#v", len(messages), messages)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%d, want 1", len(tools))
	}
	choice, _ := chat["tool_choice"].(map[string]any)
	fn, _ := choice["function"].(map[string]any)
	if fn["name"] != "lookup" {
		t.Fatalf("tool choice not converted: %#v", choice)
	}
}

func TestAnthropicRequestConversion(t *testing.T) {
	body := map[string]any{
		"system":     []any{map[string]any{"type": "text", "text": "Be concise."}},
		"max_tokens": float64(128),
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "tool_use", "id": "toolu_1", "name": "lookup",
					"input": map[string]any{"q": "x"},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_1",
					"content": []any{map[string]any{"type": "text", "text": "result"}},
				}},
			},
		},
		"tools": []any{map[string]any{
			"name": "lookup", "description": "Lookup",
			"input_schema": map[string]any{"type": "object"},
		}},
	}
	chat, err := anthropicToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := chat["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages=%d, want 3: %#v", len(messages), messages)
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "toolu_1" {
		t.Fatalf("tool result not converted: %#v", last)
	}
}

func TestCompatibilityEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Run("responses_non_streaming", func(t *testing.T) {
		fx := newTestServer(t)
		resp := doPost(t, fx.server.URL+"/v1/responses", "sk-test-key", map[string]any{
			"model": "gemini-2.5-flash", "input": "Say hello",
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, data)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["object"] != "response" || body["status"] != "completed" {
			t.Fatalf("unexpected response: %#v", body)
		}
		output, _ := body["output"].([]any)
		if len(output) == 0 {
			t.Fatal("missing response output")
		}
	})

	t.Run("responses_fake_stream", func(t *testing.T) {
		fx := newTestServer(t)
		resp := doPost(t, fx.server.URL+"/v1/responses", "sk-test-key", map[string]any{
			"model": "fake-gemini-2.5-flash", "input": "Say hello", "stream": true,
		})
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		stream := string(data)
		for _, want := range []string{
			"event: response.created", "event: response.output_text.delta", "event: response.completed",
		} {
			if !strings.Contains(stream, want) {
				t.Fatalf("missing %q in stream: %s", want, stream)
			}
		}
	})

	t.Run("anthropic_x_api_key", func(t *testing.T) {
		fx := newTestServer(t)
		resp := postWithHeader(t, fx.server.URL+"/v1/messages", "x-api-key", "sk-test-key", map[string]any{
			"model": "gemini-2.5-flash", "max_tokens": 128,
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, data)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["type"] != "message" || body["role"] != "assistant" {
			t.Fatalf("unexpected message: %#v", body)
		}
	})

	t.Run("anthropic_fake_stream", func(t *testing.T) {
		fx := newTestServer(t)
		resp := postWithHeader(t, fx.server.URL+"/v1/messages", "x-api-key", "sk-test-key", map[string]any{
			"model": "fake-gemini-2.5-flash", "max_tokens": 128, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
		})
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		stream := string(data)
		for _, want := range []string{
			"event: message_start", "event: content_block_delta", "event: message_stop",
		} {
			if !strings.Contains(stream, want) {
				t.Fatalf("missing %q in stream: %s", want, stream)
			}
		}
	})

	t.Run("gemini_native_x_goog_api_key", func(t *testing.T) {
		fx := newTestServer(t)
		resp := postWithHeader(
			t,
			fx.server.URL+"/v1beta/models/gemini-2.5-flash:generateContent",
			"x-goog-api-key",
			"sk-test-key",
			map[string]any{
				"contents": []any{map[string]any{
					"role": "user", "parts": []any{map[string]any{"text": "Say hello"}},
				}},
			},
		)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, data)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(anySlice(body["candidates"])) == 0 {
			t.Fatalf("missing Gemini candidates: %#v", body)
		}
	})

	t.Run("gemini_native_retries_unspecified_prompt_block", func(t *testing.T) {
		fx := newTestServer(t)
		var calls atomic.Int32
		retryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if calls.Add(1) == 1 {
				_, _ = w.Write([]byte(
					`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
						`{"promptFeedback":{"blockReason":"BLOCKED_REASON_UNSPECIFIED"}}}}}]}]`,
				))
				return
			}
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					geminiNonStreamingResponse() + `}}}]}]`,
			))
		}))
		defer retryUpstream.Close()
		vertex.SetBatchGraphqlURL(retryUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		resp := postWithHeader(
			t,
			fx.server.URL+"/v1beta/models/gemini-3.6-flash:generateContent",
			"x-goog-api-key",
			"sk-test-key",
			map[string]any{
				"contents": []any{map[string]any{
					"role": "user", "parts": []any{map[string]any{"text": "Say hello"}},
				}},
			},
		)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, data)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Fatalf("upstream calls=%d, want one automatic retry", calls.Load())
		}
		if _, blocked := body["promptFeedback"]; blocked {
			t.Fatalf("successful retry must not return stale promptFeedback: %#v", body)
		}
	})

	t.Run("gemini_native_stream_retries_unspecified_prompt_block", func(t *testing.T) {
		fx := newTestServer(t)
		var calls atomic.Int32
		retryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if calls.Add(1) == 1 {
				_, _ = w.Write([]byte(
					`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
						`{"promptFeedback":{"blockReason":"BLOCKED_REASON_UNSPECIFIED"}}}}}]}]`,
				))
				return
			}
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					geminiNonStreamingResponse() + `}}}]}]`,
			))
		}))
		defer retryUpstream.Close()
		vertex.SetBatchGraphqlURL(retryUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		resp := postWithHeader(
			t,
			fx.server.URL+"/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse",
			"x-goog-api-key",
			"sk-test-key",
			map[string]any{
				"contents": []any{map[string]any{
					"role": "user", "parts": []any{map[string]any{"text": "Say hello"}},
				}},
			},
		)
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		stream := string(data)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, stream)
		}
		if calls.Load() != 2 {
			t.Fatalf("upstream calls=%d, want one automatic retry", calls.Load())
		}
		if !strings.Contains(stream, "Hello") {
			t.Fatalf("successful retry content missing: %s", stream)
		}
		if strings.Contains(stream, "BLOCKED_REASON_UNSPECIFIED") {
			t.Fatalf("stream leaked stale promptFeedback: %s", stream)
		}
	})

	t.Run("gemini_native_reports_persistent_unspecified_prompt_block", func(t *testing.T) {
		fx := newTestServer(t)
		var calls atomic.Int32
		blockedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"promptFeedback":{"blockReason":"BLOCKED_REASON_UNSPECIFIED"}}}}}]}]`,
			))
		}))
		defer blockedUpstream.Close()
		vertex.SetBatchGraphqlURL(blockedUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		resp := postWithHeader(
			t,
			fx.server.URL+"/v1beta/models/gemini-3.6-flash:generateContent",
			"x-goog-api-key",
			"sk-test-key",
			map[string]any{
				"contents": []any{map[string]any{
					"role": "user", "parts": []any{map[string]any{"text": "Say hello"}},
				}},
			},
		)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d, want 503, body=%s", resp.StatusCode, data)
		}
		if calls.Load() != 2 {
			t.Fatalf("upstream calls=%d, want exactly two attempts", calls.Load())
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		errBody, _ := body["error"].(map[string]any)
		if errBody["status"] != "UNAVAILABLE" {
			t.Fatalf("unexpected error body: %#v", body)
		}
	})

	t.Run("gemini_native_stream_reports_persistent_unspecified_prompt_block_once", func(t *testing.T) {
		fx := newTestServer(t)
		var calls atomic.Int32
		blockedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"promptFeedback":{"blockReason":"BLOCKED_REASON_UNSPECIFIED"}}}}}]}]`,
			))
		}))
		defer blockedUpstream.Close()
		vertex.SetBatchGraphqlURL(blockedUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		resp := postWithHeader(
			t,
			fx.server.URL+"/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse",
			"x-goog-api-key",
			"sk-test-key",
			map[string]any{
				"contents": []any{map[string]any{
					"role": "user", "parts": []any{map[string]any{"text": "Say hello"}},
				}},
			},
		)
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		stream := string(data)
		if calls.Load() != 2 {
			t.Fatalf("upstream calls=%d, want exactly two attempts", calls.Load())
		}
		if strings.Count(stream, `"status":"UNAVAILABLE"`) != 1 {
			t.Fatalf("expected one UNAVAILABLE event: %s", stream)
		}
		if strings.Contains(stream, "Upstream returned empty response") {
			t.Fatalf("stream emitted duplicate empty-response error: %s", stream)
		}
	})
}

func postWithHeader(t *testing.T, url, header, key string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
