package transform

import (
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// ensureGenCfg 返回 geminiPayload["generationConfig"]（不存在则创建）。
func ensureGenCfg(geminiPayload map[string]any) map[string]any {
	gc, ok := geminiPayload["generationConfig"].(map[string]any)
	if !ok {
		gc = map[string]any{}
		geminiPayload["generationConfig"] = gc
	}
	return gc
}

// buildGenerationConfig 构建 generationConfig。第二个返回值表示结果仍与输入共享，
// 调用方在需要删除兼容字段时必须先复制。
func buildGenerationConfig(geminiPayload map[string]any) (map[string]any, bool) {
	if ugc, ok := geminiPayload["generationConfig"].(map[string]any); ok {
		if generationConfigCanPassThrough(ugc) {
			return ugc, true
		}
		return convertToGeminiFormat(ugc), false
	}
	if ugc, ok := geminiPayload["generation_config"].(map[string]any); ok {
		if generationConfigCanPassThrough(ugc) {
			return ugc, true
		}
		return convertToGeminiFormat(ugc), false
	}
	return nil, true
}

func generationConfigCanPassThrough(cfg map[string]any) bool {
	for key := range cfg {
		if strings.ContainsRune(key, '_') {
			return false
		}
		switch key {
		case "thinkingConfig", "imageConfig", "speechConfig", "audioTimestamp", "routingConfig", "responseSchema", "topK":
			return false
		}
	}
	return true
}

// applyModelGenerationCompatibility removes parameters deprecated by the
// Gemini 3.6 family. The public API currently ignores these sampling fields
// and documents that future revisions may reject them, so stripping them keeps
// OpenAI clients with legacy defaults compatible with the new model.
func applyModelGenerationCompatibility(model string, cfg map[string]any) {
	if !isGemini36Model(model) {
		return
	}
	for _, key := range []string{"temperature", "topP", "topK", "candidateCount"} {
		delete(cfg, key)
	}
	if thinking, ok := cfg["thinkingConfig"].(map[string]any); ok {
		delete(thinking, "thinkingBudget")
		if len(thinking) == 0 {
			delete(cfg, "thinkingConfig")
		}
	}
}

func isGemini36Model(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "gemini-3.6-")
}

// convertToGeminiFormat 把 generationConfig 转为 Gemini 期望格式。
func convertToGeminiFormat(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		camelKey := SnakeToCamel(k)
		switch camelKey {
		case "thinkingConfig":
			if vm, ok := v.(map[string]any); ok {
				tc, _ := camelizeNested(vm).(map[string]any)
				if lvl, ok := tc["thinkingLevel"].(string); ok {
					tc["thinkingLevel"] = strings.ToUpper(lvl)
				}
				out[camelKey] = tc
				continue
			}
			out[camelKey] = v
		case "imageConfig", "speechConfig", "audioTimestamp", "routingConfig":
			if vm, ok := v.(map[string]any); ok {
				out[camelKey] = camelizeNested(vm)
				continue
			}
			out[camelKey] = v
		case "responseSchema":
			out[camelKey] = toNativeSchema(v)
		case "topK":
			out[camelKey] = clampTopK(v)
		default:
			out[camelKey] = v
		}
	}
	return out
}

// clampTopK 把 topK 限制到 ≤63。
func clampTopK(v any) any {
	switch n := v.(type) {
	case float64:
		if n > 63 {
			return 63
		}
		return int(n)
	case int:
		if n > 63 {
			return 63
		}
		return n
	default:
		return v
	}
}

// camelizeNested 深度遍历键 camelCase 化。
func camelizeNested(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[SnakeToCamel(k)] = camelizeNested(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = camelizeNested(item)
		}
		return out
	default:
		return v
	}
}

type vertexSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

var defaultVertexSafetySettings = func() []vertexSafetySetting { //nolint:gochecknoglobals
	out := make([]vertexSafetySetting, len(safetyCategories))
	for index, category := range safetyCategories {
		out[index] = vertexSafetySetting{Category: category, Threshold: "BLOCK_NONE"}
	}
	return out
}()

// buildSafetySettings 构建默认安全设置。
func buildSafetySettings(cfg config.ConfigProvider) []vertexSafetySetting {
	return buildSafetySettingsFromMap(cfg.SafetySettings())
}

func buildSafetySettingsFromMap(configured map[string]string) []vertexSafetySetting {
	if len(configured) == 0 {
		// BuildVertexVariables 发布后只读；共享结构体切片避免每个请求重建
		// 5 个动态 map，且外部包无法断言未导出的元素类型后修改它。
		return defaultVertexSafetySettings
	}
	out := make([]vertexSafetySetting, len(safetyCategories))
	for index, cat := range safetyCategories {
		threshold := "BLOCK_NONE"
		if t, ok := configured[cat]; ok && t != "" {
			threshold = t
		}
		out[index] = vertexSafetySetting{Category: cat, Threshold: threshold}
	}
	return out
}
