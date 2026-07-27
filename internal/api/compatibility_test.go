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

func TestResponsesRequestConversionGroupsParallelFunctionCalls(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "run both"},
			map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}},
			map[string]any{
				"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a"}`,
			},
			map[string]any{
				"type": "function_call", "call_id": "call_2", "name": "read_file", "arguments": `{"path":"b"}`,
			},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "A"},
			map[string]any{"type": "function_call_output", "call_id": "call_2", "output": "B"},
		},
	}
	chat, err := responsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("并行调用应转换为 user + assistant(2 calls) + 2 tool，got %#v", messages)
	}
	assistant := messages[1].(map[string]any)
	toolCalls, _ := assistant["tool_calls"].([]any)
	if assistant["role"] != "assistant" || len(toolCalls) != 2 {
		t.Fatalf("并行 function_call 未合并: %#v", assistant)
	}
}

func TestResponsesRequestConversionSupportsCodexNamespaceAndHostedTools(t *testing.T) {
	body := map[string]any{
		"input": "hello",
		"tools": []any{
			map[string]any{
				"type": "namespace", "name": "mcp__demo", "description": "Demo tools",
				"tools": []any{map[string]any{
					"type": "function", "name": "lookup", "description": "Lookup",
					"parameters": map[string]any{"type": "object"},
				}},
			},
			map[string]any{"type": "namespace", "name": "collaboration"},
			map[string]any{"type": "web_search"},
		},
	}
	chat, err := responsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("应只保留展平后的 namespace 子工具: %#v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "mcp__demo__lookup" {
		t.Fatalf("namespace 工具名未展平: %#v", fn)
	}
	mappings := chat[responsesNamespaceToolsKey].(map[string]responsesNamespacedTool)
	out := protocolOutput{ToolCalls: []protocolToolCall{{Name: "mcp__demo__lookup"}}}
	restoreResponsesToolNamespaces(&out, mappings)
	if out.ToolCalls[0].Namespace != "mcp__demo" || out.ToolCalls[0].Name != "lookup" {
		t.Fatalf("namespace 工具回调未还原: %+v", out.ToolCalls[0])
	}
}

func TestAnthropicMessagesAcceptsClaudeCodeSystemRole(t *testing.T) {
	chat, err := anthropicToChatRequest(map[string]any{
		"max_tokens": float64(128),
		"messages": []any{
			map[string]any{"role": "system", "content": "CLI context"},
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("Claude Code system role 未保留: %#v", messages)
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
		usage, _ := body["usage"].(map[string]any)
		if usage["input_tokens"] != float64(10) ||
			usage["output_tokens"] != float64(20) ||
			usage["total_tokens"] != float64(30) {
			t.Fatalf("Responses 非流式 usage 不正确: %#v", usage)
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
			`"input_tokens":10`, `"output_tokens":20`, `"total_tokens":30`,
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
		usage, _ := body["usage"].(map[string]any)
		if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(20) {
			t.Fatalf("Anthropic 非流式 usage 不正确: %#v", usage)
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
			"event: message_start", "event: content_block_delta", "event: message_delta", "event: message_stop",
			`"input_tokens":10`, `"output_tokens":20`,
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

	t.Run("gemini_native_strips_unspecified_prompt_block", func(t *testing.T) {
		// 与 c6f6b65 行为对齐：上游返回 promptFeedback.blockReason=BLOCKED_REASON_UNSPECIFIED
		// 时不再自动重试。匿名 Gemini 端点经常在正常响应里附带该字段，重试反而会
		// 提前 abort 流、让客户端拿不到后续真正的内容 chunk。
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
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, data)
		}
		if calls.Load() != 1 {
			t.Fatalf("upstream calls=%d, want exactly one (no semantic retry)", calls.Load())
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		// 修复后：BLOCKED_REASON_UNSPECIFIED 应被删除，不出现在响应中。
		if feedback, ok := body["promptFeedback"].(map[string]any); ok {
			if reason, _ := feedback["blockReason"].(string); reason != "" {
				t.Fatalf("expected blockReason stripped, got %q in %#v", reason, body)
			}
		}
	})

	t.Run("gemini_native_stream_strips_unspecified_prompt_block", func(t *testing.T) {
		// 流式同样不再做语义重试。上游发什么就透传什么 —— 匿名 Gemini 经常
		// 先发一个只含 promptFeedback 的 metadata 帧，后面才是真正的内容帧；
		// 之前的语义重试会在第一帧就 return false 中断流，导致后续内容拿不到。
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
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, stream)
		}
		if calls.Load() != 1 {
			t.Fatalf("upstream calls=%d, want exactly one (no semantic retry)", calls.Load())
		}
		// 修复后：BLOCKED_REASON_UNSPECIFIED 不应出现在流式输出中。
		if strings.Contains(stream, "BLOCKED_REASON_UNSPECIFIED") {
			t.Fatalf("expected blockReason stripped from stream, got: %s", stream)
		}
	})

	t.Run("gemini_native_strips_persistent_unspecified_prompt_block", func(t *testing.T) {
		// 修复后：上游持续只返回 promptFeedback（无 candidates）时，直接透传给客户端，
		// 不再 503，由客户端自行判断。这与 c6f6b65 行为一致。
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
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d, want 200 (pass-through), body=%s", resp.StatusCode, data)
		}
		if calls.Load() != 1 {
			t.Fatalf("upstream calls=%d, want exactly one (no semantic retry)", calls.Load())
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		// 修复后：BLOCKED_REASON_UNSPECIFIED 应被删除，不出现在响应中。
		if feedback, ok := body["promptFeedback"].(map[string]any); ok {
			if reason, _ := feedback["blockReason"].(string); reason != "" {
				t.Fatalf("expected blockReason stripped, got %q in %#v", reason, body)
			}
		}
	})

	t.Run("gemini_native_stream_strips_persistent_unspecified_prompt_block", func(t *testing.T) {
		// 修复后：流式持续只有 promptFeedback 帧时，原样把帧透传给客户端，
		// 不再插入 UNAVAILABLE 错误事件。这与 c6f6b65 行为一致。
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
		if calls.Load() != 1 {
			t.Fatalf("upstream calls=%d, want exactly one (no semantic retry)", calls.Load())
		}
		if strings.Contains(stream, `"status":"UNAVAILABLE"`) {
			t.Fatalf("should not emit UNAVAILABLE event after fix: %s", stream)
		}
		// 修复后：BLOCKED_REASON_UNSPECIFIED 不应出现在流中。
		if strings.Contains(stream, "BLOCKED_REASON_UNSPECIFIED") {
			t.Fatalf("expected blockReason stripped from stream, got: %s", stream)
		}
	})

	t.Run("rikkahub_stream_usage_compatibility", func(t *testing.T) {
		fx := newTestServer(t)
		usageUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},` +
					`"finishReason":"STOP","tokenCount":8}]}}}}]}]` +
					`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"usageMetadata":{"totalTokenCount":84}}}}}]}]`,
			))
		}))
		defer usageUpstream.Close()
		vertex.SetBatchGraphqlURL(usageUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		nativeResp := postWithHeader(
			t,
			fx.server.URL+"/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse",
			"x-goog-api-key",
			"sk-test-key",
			map[string]any{"contents": []any{map[string]any{
				"role": "user", "parts": []any{map[string]any{"text": "hello"}},
			}}},
		)
		nativeData, _ := io.ReadAll(nativeResp.Body)
		nativeResp.Body.Close()
		nativeStream := string(nativeData)
		if nativeResp.StatusCode != http.StatusOK ||
			!strings.Contains(nativeStream, `"promptTokenCount":76`) ||
			!strings.Contains(nativeStream, `"candidatesTokenCount":8`) ||
			!strings.Contains(nativeStream, `"parts":[]`) {
			t.Fatalf("Gemini 原生流未生成 RikkaHub 可读取的 usage candidate: %s", nativeStream)
		}

		oaiResp := doPost(t, fx.server.URL+"/v1/chat/completions", "sk-test-key", map[string]any{
			"model": "gemini-3.6-flash", "stream": true,
			"stream_options": map[string]any{"include_usage": true},
			"messages":       []any{map[string]any{"role": "user", "content": "hello"}},
		})
		oaiData, _ := io.ReadAll(oaiResp.Body)
		oaiResp.Body.Close()
		oaiStream := string(oaiData)
		if oaiResp.StatusCode != http.StatusOK ||
			!strings.Contains(oaiStream, `"prompt_tokens":76`) ||
			!strings.Contains(oaiStream, `"completion_tokens":8`) ||
			!strings.Contains(oaiStream, `"total_tokens":84`) ||
			!strings.Contains(oaiStream, `"choices":[{"delta":{},"finish_reason":null,"index":0}]`) {
			t.Fatalf("OpenAI 流未生成 RikkaHub 可读取的分项 usage: %s", oaiStream)
		}
	})

	t.Run("stream_uses_real_candidate_token_count_without_usage_metadata", func(t *testing.T) {
		fx := newTestServer(t)
		noUsageUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},` +
					`"finishReason":"STOP","tokenCount":8}]}}}}]}]`,
			))
		}))
		defer noUsageUpstream.Close()
		vertex.SetBatchGraphqlURL(noUsageUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		nativeResp := postWithHeader(
			t,
			fx.server.URL+"/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse",
			"x-goog-api-key",
			"sk-test-key",
			map[string]any{"contents": []any{map[string]any{
				"role": "user", "parts": []any{map[string]any{"text": "hello"}},
			}}},
		)
		nativeData, _ := io.ReadAll(nativeResp.Body)
		nativeResp.Body.Close()
		nativeStream := string(nativeData)
		if nativeResp.StatusCode != http.StatusOK ||
			!strings.Contains(nativeStream, `"promptTokenCount":0`) ||
			!strings.Contains(nativeStream, `"candidatesTokenCount":8`) ||
			!strings.Contains(nativeStream, `"totalTokenCount":0`) ||
			!strings.Contains(nativeStream, `"parts":[]`) {
			t.Fatalf("Gemini 原生流没有透传真实 candidate tokenCount: %s", nativeStream)
		}

		oaiResp := doPost(t, fx.server.URL+"/v1/chat/completions", "sk-test-key", map[string]any{
			"model": "gemini-3.6-flash", "stream": true,
			"stream_options": map[string]any{"include_usage": true},
			"messages":       []any{map[string]any{"role": "user", "content": "hello"}},
		})
		oaiData, _ := io.ReadAll(oaiResp.Body)
		oaiResp.Body.Close()
		oaiStream := string(oaiData)
		if oaiResp.StatusCode != http.StatusOK ||
			!strings.Contains(oaiStream, `"prompt_tokens":0`) ||
			!strings.Contains(oaiStream, `"completion_tokens":8`) ||
			!strings.Contains(oaiStream, `"total_tokens":0`) ||
			!strings.Contains(oaiStream, `"choices":[{"delta":{},"finish_reason":null,"index":0}]`) {
			t.Fatalf("OpenAI 流没有透传真实 candidate tokenCount: %s", oaiStream)
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
