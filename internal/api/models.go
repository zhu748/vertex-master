package api

import "strings"

// 本文件实现模型清单端点所依赖的工具函数。

type requestedModelResolver interface {
	FakePrefixes() []string
	ResolveModelName(string) string
}

// stripFakePrefix 检测并剥离假流式前缀，返回 (实际模型名, 是否假流式)。
func stripFakePrefix(model string, fakePrefixes []string) (string, bool) {
	for _, p := range fakePrefixes {
		if strings.HasPrefix(model, p) {
			return model[len(p):], true
		}
	}
	return model, false
}

// resolveRequestedModel 同时支持把 fake 前缀写在请求名和别名目标上。
//
// 过去各 handler 会先剥 fake 前缀、到最终出站层才解析 alias_map。若用户把
// "fakeGemini36" 映射到 "fake-gemini-3.6-flash"，fake 前缀因此既不会启用
// 假流式，也会作为上游模型名发送。这里统一顺序，并继续支持
// "fake-my-alias" -> "gemini-3.6-flash"。
func resolveRequestedModel(model string, resolver requestedModelResolver) (string, bool) {
	if resolver == nil {
		return model, false
	}
	model, requestedFake := stripFakePrefix(model, resolver.FakePrefixes())
	resolved := resolver.ResolveModelName(model)
	resolved, aliasedFake := stripFakePrefix(resolved, resolver.FakePrefixes())
	if aliasedFake {
		// The alias target itself contains fake-, so it cannot be left for the
		// normal outbound alias resolver (which would send that prefix as part
		// of the upstream model name).
		return resolved, true
	}
	// Preserve ordinary aliases here. The transform layer resolves them for the
	// upstream request, while compatibility responses continue to show the
	// client-facing alias exactly as before.
	return model, requestedFake
}

// supportedGenerationMethods 返回模型详情里声明的支持方法（本代理统一支持这三种）。
func supportedGenerationMethods() []any {
	return []any{"generateContent", "streamGenerateContent", "countTokens"}
}

// geminiModelInfo 构造单个 Gemini 模型详情对象（供 get_model_info / list_models_gemini 用）。
func geminiModelInfo(name string) map[string]any {
	return map[string]any{
		"name":                       "models/" + name,
		"version":                    name,
		"displayName":                name,
		"description":                "Vertex AI Studio anonymous model",
		"inputTokenLimit":            1048576,
		"outputTokenLimit":           65536,
		"supportedGenerationMethods": supportedGenerationMethods(),
	}
}
