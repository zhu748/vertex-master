package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transform"
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

func TestResponsesTextContentPassesThroughFullConversion(t *testing.T) {
	content := []any{
		map[string]any{"type": "input_text", "text": "one"},
		map[string]any{"type": "output_text", "text": "two"},
		map[string]any{"type": "text", "text": "three"},
	}
	convertedContent, err := responseContentToChat(content)
	if err != nil {
		t.Fatal(err)
	}
	passed := convertedContent.([]any)
	if &passed[0] != &content[0] {
		t.Fatal("canonical Responses text content should reuse the read-only input slice")
	}

	chat, err := responsesToChatRequest(map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	chat["model"] = "gemini-test"
	_, payload, err := transform.ConvertChatRequest(chat, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	contents := payload["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 3 || parts[0].(map[string]any)["text"] != "one" ||
		parts[1].(map[string]any)["text"] != "two" || parts[2].(map[string]any)["text"] != "three" {
		t.Fatalf("Responses text parts were not preserved through Gemini conversion: %#v", parts)
	}
}

func TestResponsesAndAnthropicGemini36AssistantPrefill(t *testing.T) {
	tests := []struct {
		name string
		chat func(t *testing.T) map[string]any
	}{
		{
			name: "responses",
			chat: func(t *testing.T) map[string]any {
				t.Helper()
				chat, err := responsesToChatRequest(map[string]any{
					"input": []any{
						map[string]any{"type": "message", "role": "user", "content": "Continue"},
						map[string]any{"type": "message", "role": "assistant", "content": []any{
							map[string]any{"type": "output_text", "text": "ABC"},
						}},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				return chat
			},
		},
		{
			name: "anthropic",
			chat: func(t *testing.T) map[string]any {
				t.Helper()
				chat, err := anthropicToChatRequest(map[string]any{
					"max_tokens": float64(64),
					"messages": []any{
						map[string]any{"role": "user", "content": "Continue"},
						map[string]any{"role": "assistant", "content": []any{
							map[string]any{"type": "text", "text": "ABC"},
						}},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				return chat
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chat := tc.chat(t)
			chat["model"] = "gemini-3.6-flash"
			_, payload, err := transform.ConvertChatRequest(
				chat,
				config.StaticProvider(config.DefaultConfig()),
			)
			if err != nil {
				t.Fatal(err)
			}
			if got := transform.AssistantPrefillFromPayload(payload); got != "ABC" {
				t.Fatalf("%s 预填充未进入共享 Gemini 3.6 适配: %q", tc.name, got)
			}
			contents := payload["contents"].([]any)
			if got := contents[len(contents)-1].(map[string]any)["role"]; got != "user" {
				t.Fatalf("%s 转换后仍以 model 结尾: %v", tc.name, got)
			}
		})
	}
}

func TestResponsesGemini36AssistantBlocksPreserveExactPrefill(t *testing.T) {
	chat, err := responsesToChatRequest(map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "Continue"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "A"},
				map[string]any{"type": "output_text", "text": "B"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	chat["model"] = "gemini-3.6-flash"
	_, payload, err := transform.ConvertChatRequest(
		chat,
		config.StaticProvider(config.DefaultConfig()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := transform.AssistantPrefillFromPayload(payload); got != "AB" {
		t.Fatalf("Responses assistant 文本块之间不得插入分隔符: %q", got)
	}
	contents := anySlice(payload["contents"])
	prefill := contents[len(contents)-2].(map[string]any)
	parts := anySlice(prefill["parts"])
	if len(parts) != 2 ||
		stringValue(parts[0].(map[string]any)["text"]) != "A" ||
		stringValue(parts[1].(map[string]any)["text"]) != "B" {
		t.Fatalf("Responses assistant 文本块未精确保留: %#v", prefill)
	}
}

func TestAnthropicGemini36DisabledThinkingUsesSupportedMinimum(t *testing.T) {
	chat, err := anthropicToChatRequest(map[string]any{
		"max_tokens": float64(64),
		"thinking":   map[string]any{"type": "disabled"},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	chat["model"] = "gemini-3.6-flash"
	model, payload, err := transform.ConvertChatRequest(
		chat,
		config.StaticProvider(config.DefaultConfig()),
	)
	if err != nil {
		t.Fatal(err)
	}
	vars := transform.BuildVertexVariables(
		model,
		payload,
		config.StaticProvider(config.DefaultConfig()),
	)
	thinking := vars["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if got := thinking["thinkingLevel"]; got != "MINIMAL" {
		t.Fatalf("Claude thinking.disabled 在 Gemini 3.6 应降级为 MINIMAL，got %v", got)
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

func TestResponsesNamespaceToolChoiceUsesFlattenedName(t *testing.T) {
	body := map[string]any{
		"model": "gemini-test",
		"input": "hello",
		"tools": []any{map[string]any{
			"type": "namespace", "name": "mcp__demo",
			"tools": []any{map[string]any{
				"type": "function", "name": "lookup",
				"parameters": map[string]any{"type": "object"},
			}},
		}},
		"tool_choice": map[string]any{
			"type": "function", "namespace": "mcp__demo", "name": "lookup",
		},
	}

	chat, err := responsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	choice := chat["tool_choice"].(map[string]any)
	function := choice["function"].(map[string]any)
	if got := function["name"]; got != "mcp__demo__lookup" {
		t.Fatalf("namespace tool_choice 未展平: %#v", choice)
	}

	_, payload, err := transform.ConvertChatRequest(
		chat,
		config.StaticProvider(config.DefaultConfig()),
	)
	if err != nil {
		t.Fatalf("展平后的 namespace tool_choice 应通过完整转换: %v", err)
	}
	toolConfig := payload["toolConfig"].(map[string]any)
	callingConfig := toolConfig["functionCallingConfig"].(map[string]any)
	allowed := callingConfig["allowedFunctionNames"].([]any)
	if len(allowed) != 1 || allowed[0] != "mcp__demo__lookup" {
		t.Fatalf("Gemini allowedFunctionNames 未保留展平工具名: %#v", callingConfig)
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

func TestAnthropicPureTextUserContentPassesThroughToChat(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "first"},
		map[string]any{
			"type": "text", "text": "second",
			"cache_control": map[string]any{"type": "ephemeral"},
		},
	}
	chat, err := anthropicToChatRequest(map[string]any{
		"max_tokens": float64(128),
		"messages":   []any{map[string]any{"role": "user", "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	convertedContent := messages[0].(map[string]any)["content"].([]any)
	if len(convertedContent) != len(content) || &convertedContent[0] != &content[0] {
		t.Fatal("pure Anthropic text content was copied instead of passed through")
	}

	_, payload, err := transform.ConvertChatRequest(chat, config.StaticProvider(config.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	contents := payload["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "first" ||
		parts[1].(map[string]any)["text"] != "second" {
		t.Fatalf("Anthropic text blocks were not preserved through Gemini conversion: %#v", parts)
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

	t.Run("chat_fake_stream_preserves_length_finish_reason", func(t *testing.T) {
		fx := newTestServer(t)
		truncatedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"},` +
					`"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":2,` +
					`"candidatesTokenCount":3,"totalTokenCount":5}}}}}]}]`,
			))
		}))
		defer truncatedUpstream.Close()
		vertex.SetBatchGraphqlURL(truncatedUpstream.URL)
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		resp := doPost(t, fx.server.URL+"/v1/chat/completions", "sk-test-key", map[string]any{
			"model": "fake-gemini-2.5-flash",
			"messages": []any{map[string]any{
				"role": "user", "content": "Write a long answer",
			}},
			"stream": true,
		})
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		stream := string(data)
		if !strings.Contains(stream, `"finish_reason":"length"`) {
			t.Fatalf("假流式必须保留上游 MAX_TOKENS，got %s", stream)
		}
		if strings.Contains(stream, `"finish_reason":"stop"`) {
			t.Fatalf("截断响应不得伪装为正常结束，got %s", stream)
		}
	})

	t.Run("chat_fake_stream_preserves_tool_calls", func(t *testing.T) {
		fx := newTestServer(t)
		toolUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup",` +
					`"args":{"q":"x"}}}],"role":"model"},"finishReason":"STOP"}],` +
					`"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,` +
					`"totalTokenCount":5}}}}}]}]`,
			))
		}))
		defer toolUpstream.Close()
		vertex.SetBatchGraphqlURL(toolUpstream.URL)
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		resp := doPost(t, fx.server.URL+"/v1/chat/completions", "sk-test-key", map[string]any{
			"model": "fake-gemini-2.5-flash",
			"messages": []any{map[string]any{
				"role": "user", "content": "Use lookup",
			}},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "lookup",
					"parameters": map[string]any{
						"type": "object",
					},
				},
			}},
			"stream": true,
		})
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		stream := string(data)
		for _, want := range []string{
			`"tool_calls":[`,
			`"name":"lookup"`,
			`"arguments":"{\"q\":\"x\"}"`,
			`"finish_reason":"tool_calls"`,
		} {
			if !strings.Contains(stream, want) {
				t.Fatalf("假流式丢失工具语义 %q: %s", want, stream)
			}
		}
		if strings.Contains(stream, `"finish_reason":"stop"`) {
			t.Fatalf("工具响应不得伪装为普通文本结束: %s", stream)
		}
	})

	t.Run("gemini_fake_stream_preserves_parts_and_finish_reason", func(t *testing.T) {
		fx := newTestServer(t)
		partsUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"candidates":[{"content":{"parts":[{"text":"planning","thought":true,` +
					`"thoughtSignature":"sig"},{"functionCall":{"name":"lookup","args":{"q":"x"}}}],` +
					`"role":"model"},"finishReason":"MAX_TOKENS"}],"usageMetadata":` +
					`{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}}}}]}]`,
			))
		}))
		defer partsUpstream.Close()
		vertex.SetBatchGraphqlURL(partsUpstream.URL)
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		resp := doPost(
			t,
			fx.server.URL+"/v1beta/models/fake-gemini-2.5-flash:streamGenerateContent?alt=sse",
			"sk-test-key",
			map[string]any{"contents": []any{map[string]any{
				"role": "user", "parts": []any{map[string]any{"text": "Use lookup"}},
			}}},
		)
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		stream := string(data)
		for _, want := range []string{
			`"thought":true`,
			`"thoughtSignature":"sig"`,
			`"functionCall"`,
			`"name":"lookup"`,
			`"finishReason":"MAX_TOKENS"`,
		} {
			if !strings.Contains(stream, want) {
				t.Fatalf("Gemini 假流丢失响应部件 %q: %s", want, stream)
			}
		}
		if strings.Contains(stream, `"finishReason":"STOP"`) {
			t.Fatalf("Gemini 截断响应不得伪装为 STOP: %s", stream)
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

	t.Run("gemini36_prefill_protocol_matrix", func(t *testing.T) {
		fx := newTestServer(t)
		captures := make(chan map[string]any, 16)
		prefillUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if stringValue(request["operationName"]) == "CountTokens" {
				writeTestCountTokensResponse(w, 1)
				return
			}
			variables, _ := request["variables"].(map[string]any)
			captures <- variables
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"candidates":[{"content":{"parts":[{"text":"ABCDEF"}],"role":"model"},` +
					`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,` +
					`"candidatesTokenCount":6,"totalTokenCount":16}}}}}]}]`,
			))
		}))
		defer prefillUpstream.Close()
		vertex.SetBatchGraphqlURL(prefillUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		t.Cleanup(func() {
			vertex.SetBatchGraphqlURL(fx.mockUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
		})

		chatBody := func(model string, stream bool) map[string]any {
			return map[string]any{
				"model": model,
				"messages": []any{
					map[string]any{"role": "user", "content": "Continue"},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "text", "text": "ABC"},
					}},
				},
				"reasoning_effort": "none",
				"temperature":      0.7,
				"top_p":            0.9,
				"stream":           stream,
			}
		}
		responsesBody := func(model string, stream bool) map[string]any {
			return map[string]any{
				"model": model,
				"reasoning": map[string]any{
					"effort": "none",
				},
				"input": []any{
					map[string]any{"type": "message", "role": "user", "content": "Continue"},
					map[string]any{"type": "message", "role": "assistant", "content": []any{
						map[string]any{"type": "output_text", "text": "ABC"},
					}},
				},
				"stream": stream,
			}
		}
		anthropicBody := func(model string, stream bool) map[string]any {
			return map[string]any{
				"model": model, "max_tokens": 64,
				"thinking": map[string]any{"type": "disabled"},
				"messages": []any{
					map[string]any{"role": "user", "content": "Continue"},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "text", "text": "ABC"},
					}},
				},
				"stream": stream,
			}
		}
		geminiBody := func(stream bool) map[string]any {
			return map[string]any{
				"contents": []any{
					map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Continue"}}},
					map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ABC"}}},
				},
				"generationConfig": map[string]any{
					"temperature": 0.7,
					"thinkingConfig": map[string]any{
						"thinkingBudget": 0,
					},
				},
			}
		}

		tests := []struct {
			name   string
			url    string
			header string
			key    string
			body   map[string]any
		}{
			{name: "chat non-stream", url: "/v1/chat/completions", header: "Authorization", key: "Bearer sk-test-key", body: chatBody("gemini-3.6-flash", false)},
			{name: "chat stream", url: "/v1/chat/completions", header: "Authorization", key: "Bearer sk-test-key", body: chatBody("gemini-3.6-flash", true)},
			{name: "chat aggregate", url: "/v1/chat/completions", header: "Authorization", key: "Bearer sk-test-key", body: chatBody("fake-gemini-3.6-flash", true)},
			{name: "responses non-stream", url: "/v1/responses", header: "Authorization", key: "Bearer sk-test-key", body: responsesBody("gemini-3.6-flash", false)},
			{name: "responses stream", url: "/v1/responses", header: "Authorization", key: "Bearer sk-test-key", body: responsesBody("gemini-3.6-flash", true)},
			{name: "responses aggregate", url: "/v1/responses", header: "Authorization", key: "Bearer sk-test-key", body: responsesBody("fake-gemini-3.6-flash", true)},
			{name: "anthropic non-stream", url: "/v1/messages", header: "x-api-key", key: "sk-test-key", body: anthropicBody("gemini-3.6-flash", false)},
			{name: "anthropic stream", url: "/v1/messages", header: "x-api-key", key: "sk-test-key", body: anthropicBody("gemini-3.6-flash", true)},
			{name: "anthropic aggregate", url: "/v1/messages", header: "x-api-key", key: "sk-test-key", body: anthropicBody("fake-gemini-3.6-flash", true)},
			{name: "gemini non-stream", url: "/v1beta/models/gemini-3.6-flash:generateContent", header: "x-goog-api-key", key: "sk-test-key", body: geminiBody(false)},
			{name: "gemini stream", url: "/v1beta/models/gemini-3.6-flash:streamGenerateContent?alt=sse", header: "x-goog-api-key", key: "sk-test-key", body: geminiBody(true)},
			{name: "gemini aggregate", url: "/v1beta/models/fake-gemini-3.6-flash:streamGenerateContent?alt=sse", header: "x-goog-api-key", key: "sk-test-key", body: geminiBody(true)},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				resp := postWithHeader(t, fx.server.URL+tc.url, tc.header, tc.key, tc.body)
				data, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d body=%s", resp.StatusCode, data)
				}
				responseText := string(data)
				remaining := responseText
				continuationInOrder := true
				for _, piece := range []string{"D", "E", "F"} {
					index := strings.Index(remaining, piece)
					if index < 0 {
						continuationInOrder = false
						break
					}
					remaining = remaining[index+len(piece):]
				}
				if !continuationInOrder || strings.Contains(responseText, "ABC") {
					t.Fatalf("客户端必须只收到新续写 DEF，got %s", responseText)
				}

				var variables map[string]any
				select {
				case variables = <-captures:
				case <-time.After(time.Second):
					t.Fatal("未捕获到 Vertex 出站请求")
				}
				assertGemini36PrefillVariables(t, variables)
			})
		}
	})

	t.Run("gemini_native_strips_unspecified_prompt_block", func(t *testing.T) {
		// 与 c6f6b65 行为对齐：上游返回 promptFeedback.blockReason=BLOCKED_REASON_UNSPECIFIED
		// 时不再自动重试。匿名 Gemini 端点经常在正常响应里附带该字段，重试反而会
		// 提前 abort 流、让客户端拿不到后续真正的内容 chunk。
		fx := newTestServer(t)
		var calls atomic.Int32
		blockedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if testOperationName(r) == "CountTokens" {
				writeTestCountTokensResponse(w, 1)
				return
			}
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
		blockedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if testOperationName(r) == "CountTokens" {
				writeTestCountTokensResponse(w, 1)
				return
			}
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
		blockedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if testOperationName(r) == "CountTokens" {
				writeTestCountTokensResponse(w, 1)
				return
			}
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
		blockedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if testOperationName(r) == "CountTokens" {
				writeTestCountTokensResponse(w, 1)
				return
			}
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

	t.Run("rikkahub_stream_uses_exact_counttokens_when_generation_omits_usage", func(t *testing.T) {
		fx := newTestServer(t)
		exactCountUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if testOperationName(r) == "CountTokens" {
				count := 76
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				variables, _ := request["variables"].(map[string]any)
				contents := anySlice(variables["contents"])
				if len(contents) > 0 {
					first, _ := contents[0].(map[string]any)
					if stringValue(first["role"]) == "model" {
						count = 8
					}
				}
				writeTestCountTokensResponse(w, count)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(
				`[{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` +
					`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},` +
					`"finishReason":"STOP"}]}}}}]}]`,
			))
		}))
		defer exactCountUpstream.Close()
		vertex.SetBatchGraphqlURL(exactCountUpstream.URL + "/batchGraphql?key=test&prettyPrint=false")
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
			!strings.Contains(nativeStream, `"totalTokenCount":84`) ||
			!strings.Contains(nativeStream, `"parts":[]`) {
			t.Fatalf("Gemini 原生流没有补发精确 CountTokens usage: %s", nativeStream)
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
			t.Fatalf("OpenAI 流没有补发精确 CountTokens usage: %s", oaiStream)
		}

		responsesResp := doPost(t, fx.server.URL+"/v1/responses", "sk-test-key", map[string]any{
			"model": "gemini-3.6-flash", "input": "hello",
		})
		var responsesBody map[string]any
		if err := json.NewDecoder(responsesResp.Body).Decode(&responsesBody); err != nil {
			responsesResp.Body.Close()
			t.Fatal(err)
		}
		responsesResp.Body.Close()
		responsesUsage, _ := responsesBody["usage"].(map[string]any)
		if responsesResp.StatusCode != http.StatusOK ||
			protocolIntValue(responsesUsage["input_tokens"]) != 76 ||
			protocolIntValue(responsesUsage["output_tokens"]) != 8 ||
			protocolIntValue(responsesUsage["total_tokens"]) != 84 {
			t.Fatalf("Responses API 没有补齐精确 CountTokens usage: %#v", responsesBody)
		}

		responsesStreamResp := doPost(t, fx.server.URL+"/v1/responses", "sk-test-key", map[string]any{
			"model": "gemini-3.6-flash", "input": "hello", "stream": true,
		})
		responsesStreamData, _ := io.ReadAll(responsesStreamResp.Body)
		responsesStreamResp.Body.Close()
		responsesStream := string(responsesStreamData)
		if responsesStreamResp.StatusCode != http.StatusOK ||
			!strings.Contains(responsesStream, `"input_tokens":76`) ||
			!strings.Contains(responsesStream, `"output_tokens":8`) ||
			!strings.Contains(responsesStream, `"total_tokens":84`) {
			t.Fatalf("Responses 流没有补齐精确 CountTokens usage: %s", responsesStream)
		}

		anthropicRequest := map[string]any{
			"model": "gemini-3.6-flash", "max_tokens": 64,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}
		anthropicResp := postWithHeader(
			t, fx.server.URL+"/v1/messages", "x-api-key", "sk-test-key", anthropicRequest,
		)
		var anthropicBody map[string]any
		if err := json.NewDecoder(anthropicResp.Body).Decode(&anthropicBody); err != nil {
			anthropicResp.Body.Close()
			t.Fatal(err)
		}
		anthropicResp.Body.Close()
		anthropicUsage, _ := anthropicBody["usage"].(map[string]any)
		if anthropicResp.StatusCode != http.StatusOK ||
			protocolIntValue(anthropicUsage["input_tokens"]) != 76 ||
			protocolIntValue(anthropicUsage["output_tokens"]) != 8 {
			t.Fatalf("Anthropic API 没有补齐精确 CountTokens usage: %#v", anthropicBody)
		}

		anthropicRequest["stream"] = true
		anthropicStreamResp := postWithHeader(
			t, fx.server.URL+"/v1/messages", "x-api-key", "sk-test-key", anthropicRequest,
		)
		anthropicStreamData, _ := io.ReadAll(anthropicStreamResp.Body)
		anthropicStreamResp.Body.Close()
		anthropicStream := string(anthropicStreamData)
		if anthropicStreamResp.StatusCode != http.StatusOK ||
			!strings.Contains(anthropicStream, `"input_tokens":76`) ||
			!strings.Contains(anthropicStream, `"output_tokens":8`) {
			t.Fatalf("Anthropic 流没有补齐精确 CountTokens usage: %s", anthropicStream)
		}
	})
}

func TestExplicitCountTokenEndpointsPropagateUpstreamErrors(t *testing.T) {
	fixture := newTestServer(t)
	errorUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(
			`[{"results":[{"data":{"ui":{"countTokensV2":null}},"errors":[{` +
				`"message":"Publisher model was not found","extensions":{"status":{"code":5,` +
				`"message":"Publisher model was not found"}}}]}]}]`,
		))
	}))
	defer errorUpstream.Close()
	vertex.SetBatchGraphqlURL(errorUpstream.URL)

	tests := []struct {
		name         string
		path         string
		header       string
		wantContains string
		body         map[string]any
	}{
		{
			name: "anthropic",
			path: "/v1/messages/count_tokens", header: "x-api-key",
			wantContains: `"type":"not_found_error"`,
			body: map[string]any{
				"model": "missing-model", "messages": []any{
					map[string]any{"role": "user", "content": "hello"},
				},
			},
		},
		{
			name: "gemini",
			path: "/v1beta/models/missing-model:countTokens", header: "x-goog-api-key",
			wantContains: "Publisher model was not found",
			body: map[string]any{"contents": []any{map[string]any{
				"role": "user", "parts": []any{map[string]any{"text": "hello"}},
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := postWithHeader(
				t, fixture.server.URL+test.path, test.header, "sk-test-key", test.body,
			)
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusNotFound ||
				!strings.Contains(string(raw), test.wantContains) {
				t.Fatalf("status=%d body=%s", response.StatusCode, raw)
			}
		})
	}
}

func assertGemini36PrefillVariables(t *testing.T, variables map[string]any) {
	t.Helper()
	if stringValue(variables["model"]) != "gemini-3.6-flash" {
		t.Fatalf("出站模型错误: %#v", variables["model"])
	}
	contents := anySlice(variables["contents"])
	if len(contents) == 0 {
		t.Fatal("出站 contents 为空")
	}
	last, _ := contents[len(contents)-1].(map[string]any)
	if stringValue(last["role"]) != "user" {
		t.Fatalf("Gemini 3.6 出站请求仍以 model 结束: %#v", contents)
	}
	if len(contents) < 2 {
		t.Fatalf("Gemini 3.6 出站请求未保留 model 预填充: %#v", contents)
	}
	prefillTurn, _ := contents[len(contents)-2].(map[string]any)
	if stringValue(prefillTurn["role"]) != "model" {
		t.Fatalf("Gemini 3.6 出站请求改变了预填充角色: %#v", contents)
	}
	prefillParts := anySlice(prefillTurn["parts"])
	if len(prefillParts) != 1 ||
		stringValue(prefillParts[0].(map[string]any)["text"]) != "ABC" {
		t.Fatalf("出站请求未精确保留预填充: %#v", prefillTurn)
	}
	lastParts := anySlice(last["parts"])
	const expectedNudge = "Continue the immediately preceding assistant response. " +
		"Output only its continuation; do not repeat, explain, or restart it."
	if len(lastParts) != 1 ||
		stringValue(lastParts[0].(map[string]any)["text"]) != expectedNudge {
		t.Fatalf("出站请求缺少安全续写 nudge: %#v", last)
	}
	if _, leaked := variables["__vproxy_assistant_prefill"]; leaked {
		t.Fatal("内部预填充元数据泄漏到 Vertex 请求")
	}
	generation, _ := variables["generationConfig"].(map[string]any)
	for _, key := range []string{"temperature", "topP", "topK", "candidateCount"} {
		if _, exists := generation[key]; exists {
			t.Fatalf("Gemini 3.6 出站请求仍包含 %s: %#v", key, generation)
		}
	}
	thinking, ok := generation["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("客户端的最低思考意图未转换为 thinkingConfig: %#v", generation)
	}
	if _, exists := thinking["thinkingBudget"]; exists {
		t.Fatalf("Gemini 3.6 出站请求仍包含 thinkingBudget: %#v", thinking)
	}
	if level := stringValue(thinking["thinkingLevel"]); level != "MINIMAL" {
		t.Fatalf("客户端的 NONE/disabled/零预算应转换为 MINIMAL: %#v", thinking)
	}
}

func testOperationName(r *http.Request) string {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		return ""
	}
	return stringValue(request["operationName"])
}

func writeTestCountTokensResponse(w http.ResponseWriter, count int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = fmt.Fprintf(w, `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":%d}}}}]}]`, count)
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
