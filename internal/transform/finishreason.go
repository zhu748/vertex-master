package transform

import "strings"

// FinishReasonMap 把 Gemini finishReason 映射到 OpenAI finish_reason。
var FinishReasonMap = map[string]string{ //nolint:gochecknoglobals
	"STOP":                     "stop",
	"MAX_TOKENS":               "length",
	"SAFETY":                   "content_filter",
	"RECITATION":               "content_filter",
	"PROHIBITED_CONTENT":       "content_filter",
	"TOOL_CALLS":               "tool_calls",
	"MALFORMED_FUNCTION_CALL":  "tool_calls",
	"BLOCKLIST":                "content_filter",
	"SPII":                     "content_filter",
	"IMAGE_SAFETY":             "content_filter",
	"IMAGE_PROHIBITED_CONTENT": "content_filter",
	"OTHER":                    "stop",
}

// MapFinishReason 把 Gemini finishReason 转 OpenAI finish_reason。
func MapFinishReason(finish string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	if finish == "" {
		return "stop"
	}
	if v, ok := FinishReasonMap[strings.ToUpper(strings.TrimSpace(finish))]; ok {
		return v
	}
	return "stop"
}
