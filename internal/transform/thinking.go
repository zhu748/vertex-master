package transform

import (
	"math"
	"strings"
)

// fakeModelPrefixes 是假流式模型前缀（中文 + ASCII）。
// 与 internal/config.fakePrefixes 保持一致；这里独立声明避免 transform 包
// 反向依赖 config 包，同时让 thinkingKindForModel 自包含、可独立测试。
//
//nolint:gochecknoglobals // Read-only prefix list
var fakeModelPrefixes = []string{"fake-", "假流式-"}

// stripFakePrefixes 防御性剥离 fake- / 假流式- 前缀。
// 正常路径下 api 层（resolveRequestedModel）已经剥离过，这里只是兜底，
// 确保即使有代码路径绕过 api 层直接调用 transform，假流式变体也能正确归一化。
func stripFakePrefixes(model string) string {
	for _, p := range fakeModelPrefixes {
		model = strings.TrimPrefix(model, p)
	}
	return model
}

// thinkingModelKind 描述 GenerateContent API 中不同模型接受的思考控制形态。
// Gemini 2.5 使用 token budget；Gemini 3 使用离散 level，且部分图像模型
// 的思考行为固定或只开放两个等级。
type thinkingModelKind uint8

const (
	thinkingModelUnknown thinkingModelKind = iota
	thinkingModelBudgetPro
	thinkingModelBudgetFlash
	thinkingModelBudgetFlashLite
	thinkingModelLevelsAll
	thinkingModelLevelsNoMinimal
	thinkingModelLevelsMinimalHigh
	thinkingModelLevelsLowHigh
	thinkingModelFixed
	thinkingModelUnsupported
)

func thinkingKindForModel(model string) thinkingModelKind {
	model = strings.ToLower(strings.TrimSpace(model))
	// 防御性剥离假流式前缀，确保 fake-gemini-3.6-flash / 假流式-gemini-2.5-pro
	// 等变体也能匹配到正确的模型 kind。
	model = stripFakePrefixes(model)
	switch {
	case strings.HasPrefix(model, "gemini-2.5-flash-image"):
		return thinkingModelUnsupported
	case strings.HasPrefix(model, "gemini-2.5-flash-lite"):
		return thinkingModelBudgetFlashLite
	case strings.HasPrefix(model, "gemini-2.5-flash"):
		return thinkingModelBudgetFlash
	case strings.HasPrefix(model, "gemini-2.5-pro"):
		return thinkingModelBudgetPro
	case strings.HasPrefix(model, "gemini-3.1-flash-lite-image"),
		strings.HasPrefix(model, "gemini-3.1-flash-image"):
		return thinkingModelLevelsMinimalHigh
	case strings.HasPrefix(model, "gemini-3-pro-image"):
		return thinkingModelFixed
	case strings.HasPrefix(model, "gemini-3.1-pro"):
		return thinkingModelLevelsNoMinimal
	case strings.HasPrefix(model, "gemini-3-pro-preview"):
		return thinkingModelLevelsLowHigh
	case strings.HasPrefix(model, "gemini-3.6-flash"),
		strings.HasPrefix(model, "gemini-3.5-flash-lite"),
		strings.HasPrefix(model, "gemini-3.5-flash"),
		strings.HasPrefix(model, "gemini-3.1-flash-lite"),
		strings.HasPrefix(model, "gemini-3-flash"):
		return thinkingModelLevelsAll
	default:
		return thinkingModelUnknown
	}
}

// normalizeThinkingConfigForModel 把兼容协议或原生请求的思考控制归一为
// 目标模型在 GenerateContent API 中实际支持的字段。
func normalizeThinkingConfigForModel(model string, thinking map[string]any) bool {
	kind := thinkingKindForModel(model)
	switch kind {
	case thinkingModelUnknown:
		return len(thinking) > 0
	case thinkingModelUnsupported:
		return false
	case thinkingModelFixed:
		// Gemini 3 Pro Image 始终思考，但官方没有开放强度控制。
		delete(thinking, "thinkingLevel")
		delete(thinking, "thinkingBudget")
		return len(thinking) > 0
	case thinkingModelBudgetPro, thinkingModelBudgetFlash, thinkingModelBudgetFlashLite:
		normalizeThinkingBudgetModel(kind, thinking)
		return len(thinking) > 0
	case thinkingModelLevelsAll, thinkingModelLevelsNoMinimal,
		thinkingModelLevelsMinimalHigh, thinkingModelLevelsLowHigh:
		normalizeThinkingLevelModel(kind, thinking)
		return len(thinking) > 0
	default:
		return len(thinking) > 0
	}
}

func normalizeThinkingBudgetModel(kind thinkingModelKind, thinking map[string]any) {
	level, hasLevel := canonicalThinkingLevel(thinking["thinkingLevel"])
	budget, hasBudget := integerThinkingBudget(thinking["thinkingBudget"])

	if hasLevel {
		budget = budgetForThinkingLevel(kind, level)
		hasBudget = true
	}
	delete(thinking, "thinkingLevel")
	if !hasBudget {
		delete(thinking, "thinkingBudget")
		return
	}
	thinking["thinkingBudget"] = clampThinkingBudget(kind, budget)
}

func normalizeThinkingLevelModel(kind thinkingModelKind, thinking map[string]any) {
	level, hasLevel := canonicalThinkingLevel(thinking["thinkingLevel"])
	if !hasLevel {
		if budget, ok := integerThinkingBudget(thinking["thinkingBudget"]); ok {
			level, hasLevel = thinkingLevelForBudget(budget)
		}
	}
	delete(thinking, "thinkingBudget")
	if !hasLevel {
		delete(thinking, "thinkingLevel")
		return
	}
	thinking["thinkingLevel"] = supportedThinkingLevel(kind, level)
}

func canonicalThinkingLevel(value any) (string, bool) {
	level, ok := value.(string)
	if !ok {
		return "", false
	}
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "NONE", "DISABLED", "OFF":
		return "NONE", true
	case "MINIMAL":
		return "MINIMAL", true
	case "LOW":
		return "LOW", true
	case "MEDIUM":
		return "MEDIUM", true
	case "HIGH", "XHIGH", "MAX":
		return "HIGH", true
	default:
		return "", false
	}
}

func budgetForThinkingLevel(kind thinkingModelKind, level string) int64 {
	switch level {
	case "NONE":
		if kind == thinkingModelBudgetPro {
			// 2.5 Pro 无法关闭；128 是官方允许的最小预算。
			return 128
		}
		return 0
	case "MINIMAL", "LOW":
		return 1024
	case "MEDIUM":
		return 8192
	default:
		return 24576
	}
}

func clampThinkingBudget(kind thinkingModelKind, budget int64) int64 {
	if budget == -1 {
		return budget
	}
	switch kind {
	case thinkingModelBudgetPro:
		return min(max(budget, int64(128)), int64(32768))
	case thinkingModelBudgetFlashLite:
		if budget <= 0 {
			return 0
		}
		return min(max(budget, int64(512)), int64(24576))
	default:
		return min(max(budget, int64(0)), int64(24576))
	}
}

func thinkingLevelForBudget(budget int64) (string, bool) {
	switch {
	case budget == -1:
		// 旧 budget 的 -1 表示动态思考；Gemini 3 的 HIGH 是动态档。
		return "HIGH", true
	case budget <= 0:
		return "NONE", true
	case budget <= 1024:
		return "LOW", true
	case budget <= 8192:
		return "MEDIUM", true
	default:
		return "HIGH", true
	}
}

func supportedThinkingLevel(kind thinkingModelKind, level string) string {
	switch kind {
	case thinkingModelLevelsNoMinimal:
		if level == "NONE" || level == "MINIMAL" {
			return "LOW"
		}
		return level
	case thinkingModelLevelsMinimalHigh:
		if level == "NONE" || level == "MINIMAL" || level == "LOW" {
			return "MINIMAL"
		}
		return "HIGH"
	case thinkingModelLevelsLowHigh:
		if level == "NONE" || level == "MINIMAL" || level == "LOW" {
			return "LOW"
		}
		return "HIGH"
	default:
		if level == "NONE" {
			return "MINIMAL"
		}
		return level
	}
}

func integerThinkingBudget(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int8:
		return int64(number), true
	case int16:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case uint:
		if uint64(number) > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	case uint8:
		return int64(number), true
	case uint16:
		return int64(number), true
	case uint32:
		return int64(number), true
	case uint64:
		if number > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	case float32:
		value64 := float64(number)
		if math.IsNaN(value64) || math.IsInf(value64, 0) || math.Trunc(value64) != value64 ||
			value64 < math.MinInt64 || value64 >= math.MaxInt64 {
			return 0, false
		}
		return int64(value64), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number ||
			number < math.MinInt64 || number >= math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	default:
		return 0, false
	}
}
