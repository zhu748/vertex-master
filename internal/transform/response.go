package transform

import (
	"math"
	"strings"
	"time"
)

// GeminiJSONToCanonicalResponse extracts the protocol-neutral response fields
// without first allocating an OpenAI response map.
func GeminiJSONToCanonicalResponse(geminiResp map[string]any) CanonicalResponse {
	candidate := firstCandidate(geminiResp)
	text, toolCalls, reasoning := extractCanonicalResponseParts(candidateParts(candidate))
	finish, _ := candidate["finishReason"].(string)

	response := CanonicalResponse{
		Text:      text,
		Reasoning: reasoning,
		ToolCalls: toolCalls,
		Finish:    normalizedResponseFinish(finish, len(toolCalls)),
	}
	if usageMeta, ok := geminiResp["usageMetadata"].(map[string]any); ok {
		response.Usage = NormalizeUsageForCandidate(usageMeta, candidate)
	}
	return response
}

func normalizedResponseFinish(finish string, toolCallCount int) string {
	if finish != "" {
		return MapFinishReason(finish, toolCallCount > 0)
	}
	if toolCallCount > 0 {
		return "tool_calls"
	}
	return "stop"
}

// GeminiJSONToOAIJSON 把 Gemini 非流式响应转为 OpenAI ChatCompletion JSON。
func GeminiJSONToOAIJSON(geminiResp map[string]any, model string) map[string]any {
	candidate := firstCandidate(geminiResp)
	parts := candidateParts(candidate)
	finish, _ := candidate["finishReason"].(string)

	text, toolCalls, reasoning := extractResponseParts(parts)

	oaiFinish := normalizedResponseFinish(finish, len(toolCalls))

	message := map[string]any{"role": "assistant"}
	if text != "" {
		message["content"] = text
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	result := map[string]any{
		"id":      "chatcmpl-" + reqID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": oaiFinish,
		}},
	}
	if usageMeta, ok := geminiResp["usageMetadata"].(map[string]any); ok {
		result["usage"] = ConvertUsageForCandidate(usageMeta, candidate)
	}
	return result
}

// GeminiResponsesToOAIJSON 把 N 个 Gemini 非流式响应聚合成一个含 N 个 choice 的 OAI 响应。
func GeminiResponsesToOAIJSON(geminiResponses []map[string]any, model string) map[string]any {
	choices := make([]any, 0, len(geminiResponses))
	totalPrompt, totalCompletion, totalTokens := 0, 0, 0
	anyUsage := false

	for idx, resp := range geminiResponses {
		candidate := firstCandidate(resp)
		parts := candidateParts(candidate)
		finish, _ := candidate["finishReason"].(string)
		text, toolCalls, reasoning := extractResponseParts(parts)

		oaiFinish := normalizedResponseFinish(finish, len(toolCalls))

		message := map[string]any{"role": "assistant"}
		if text != "" {
			message["content"] = text
		} else {
			message["content"] = nil
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		if reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		choices = append(choices, map[string]any{
			"index": idx, "message": message, "finish_reason": oaiFinish,
		})

		if usageMeta, ok := resp["usageMetadata"].(map[string]any); ok {
			anyUsage = true
			u := NormalizeUsageForCandidate(usageMeta, candidate)
			totalPrompt += u.PromptTokens
			totalCompletion += u.CompletionTokens
			totalTokens += u.TotalTokens
		}
	}

	result := map[string]any{
		"id":      "chatcmpl-" + reqID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": choices,
	}
	if anyUsage {
		if totalTokens == 0 {
			totalTokens = totalPrompt + totalCompletion
		}
		result["usage"] = map[string]any{
			"prompt_tokens":     totalPrompt,
			"completion_tokens": totalCompletion,
			"total_tokens":      totalTokens,
		}
	}
	return result
}

// ConvertUsage 把 Gemini usageMetadata 转 OpenAI usage。
func ConvertUsage(meta map[string]any) map[string]any {
	return ConvertUsageForCandidate(meta, nil)
}

// NormalizedUsage is the allocation-free value representation shared by
// protocol adapters before a concrete OpenAI-compatible JSON map is needed.
type NormalizedUsage struct {
	PromptTokens          int
	CompletionTokens      int
	TotalTokens           int
	CachedInputTokens     int
	PromptAudioTokens     int
	PromptTextTokens      int
	ReasoningTokens       int
	CompletionImageTokens int
	CompletionAudioTokens int
	CompletionTextTokens  int
}

// OpenAIUsage is the typed wire representation used by streaming adapters.
// Field order matches encoding/json's sorted map-key order to preserve the
// existing JSON bytes while avoiding map reflection and allocation.
type OpenAIUsage struct {
	CompletionTokens        int                           `json:"completion_tokens"`
	CompletionTokensDetails *OpenAICompletionTokenDetails `json:"completion_tokens_details,omitempty"`
	PromptTokens            int                           `json:"prompt_tokens"`
	PromptTokensDetails     *OpenAIPromptTokenDetails     `json:"prompt_tokens_details,omitempty"`
	TotalTokens             int                           `json:"total_tokens"`

	completionTokensDetails OpenAICompletionTokenDetails `json:"-"`
	promptTokensDetails     OpenAIPromptTokenDetails     `json:"-"`
}

type OpenAICompletionTokenDetails struct {
	AudioTokens     int `json:"audio_tokens,omitempty"`
	ImageTokens     int `json:"image_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	TextTokens      int `json:"text_tokens,omitempty"`
}

type OpenAIPromptTokenDetails struct {
	AudioTokens  int `json:"audio_tokens,omitempty"`
	CachedTokens int `json:"cached_tokens,omitempty"`
	TextTokens   int `json:"text_tokens,omitempty"`
}

// FillOpenAIUsage writes the normalized counters into reusable typed storage.
func (u NormalizedUsage) FillOpenAIUsage(dst *OpenAIUsage) {
	if dst == nil {
		return
	}
	*dst = OpenAIUsage{
		CompletionTokens: u.CompletionTokens,
		PromptTokens:     u.PromptTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.ReasoningTokens > 0 || u.CompletionImageTokens > 0 ||
		u.CompletionAudioTokens > 0 || u.CompletionTextTokens > 0 {
		dst.completionTokensDetails = OpenAICompletionTokenDetails{
			AudioTokens:     u.CompletionAudioTokens,
			ImageTokens:     u.CompletionImageTokens,
			ReasoningTokens: u.ReasoningTokens,
			TextTokens:      u.CompletionTextTokens,
		}
		dst.CompletionTokensDetails = &dst.completionTokensDetails
	}
	if u.CachedInputTokens > 0 || u.PromptAudioTokens > 0 || u.PromptTextTokens > 0 {
		dst.promptTokensDetails = OpenAIPromptTokenDetails{
			AudioTokens: u.PromptAudioTokens, CachedTokens: u.CachedInputTokens, TextTokens: u.PromptTextTokens,
		}
		dst.PromptTokensDetails = &dst.promptTokensDetails
	}
}

// ConvertUsageForCandidate 把 Gemini usageMetadata 转成 OpenAI usage，并利用候选项
// tokenCount 补齐部分匿名/预览模型只返回 totalTokenCount 时缺失的输出分项。
func ConvertUsageForCandidate(meta, candidate map[string]any) map[string]any {
	return NormalizeUsageForCandidate(meta, candidate).OpenAIMap()
}

// NormalizeUsageForCandidate reads Gemini usage metadata into a value type.
// It intentionally does not construct temporary maps so high-frequency stream
// consumers can inspect accounting without allocating on every frame.
func NormalizeUsageForCandidate(meta, candidate map[string]any) NormalizedUsage {
	promptDetailList := usageDetailValues(meta, "promptTokensDetails", "prompt_tokens_details")
	toolDetailList := usageDetailValues(meta, "toolUsePromptTokensDetails", "tool_use_prompt_tokens_details")
	candidateDetailList := usageDetailValues(meta, "candidatesTokensDetails", "candidates_tokens_details")

	promptBase := usageCount(meta, "promptTokenCount", "prompt_token_count", "prompt_tokens")
	if promptBase == 0 {
		promptBase = sumUsageDetails(promptDetailList)
	}
	toolPrompt := usageCount(meta, "toolUsePromptTokenCount", "tool_use_prompt_token_count")
	if toolPrompt == 0 {
		toolPrompt = sumUsageDetails(toolDetailList)
	}

	candidateTokens := usageCount(meta, "candidatesTokenCount", "candidates_token_count", "completion_tokens")
	if candidateTokens == 0 {
		candidateTokens = sumUsageDetails(candidateDetailList)
	}
	if candidateTokens == 0 && candidate != nil {
		candidateTokens = usageCount(candidate, "tokenCount", "token_count")
	}
	thoughts := usageCount(meta, "thoughtsTokenCount", "thoughts_token_count")

	prompt := addUsageCounts(promptBase, toolPrompt)
	completion := addUsageCounts(candidateTokens, thoughts)
	total := usageCount(meta, "totalTokenCount", "total_token_count", "total_tokens")
	if total == 0 {
		total = addUsageCounts(prompt, completion)
	} else {
		// 有些预览模型只给 total + 单侧计数。用精确总数反推缺失侧，避免
		// RikkaHub 只看到 total_tokens 却把输入/输出显示为 0/0。
		if prompt == 0 && completion > 0 && total >= completion {
			prompt = total - completion
		}
		if completion == 0 && prompt > 0 && total >= prompt {
			completion = total - prompt
		}
	}
	result := NormalizedUsage{
		PromptTokens:      prompt,
		CompletionTokens:  completion,
		TotalTokens:       total,
		CachedInputTokens: usageCount(meta, "cachedContentTokenCount", "cached_content_token_count"),
		ReasoningTokens:   thoughts,
	}

	for _, raw := range promptDetailList {
		d, _ := raw.(map[string]any)
		if d == nil {
			continue
		}
		count := usageDetailCount(d)
		modality := toString(d["modality"])
		switch {
		case strings.EqualFold(modality, "AUDIO"):
			result.PromptAudioTokens += count
		case strings.EqualFold(modality, "TEXT"):
			result.PromptTextTokens += count
		}
	}

	for _, raw := range candidateDetailList {
		d, _ := raw.(map[string]any)
		if d == nil {
			continue
		}
		count := usageDetailCount(d)
		modality := toString(d["modality"])
		switch {
		case strings.EqualFold(modality, "IMAGE"):
			result.CompletionImageTokens += count
		case strings.EqualFold(modality, "AUDIO"):
			result.CompletionAudioTokens += count
		case strings.EqualFold(modality, "TEXT"):
			result.CompletionTextTokens += count
		}
	}

	return result
}

// OpenAIMap materializes the wire-compatible OpenAI usage object only at the
// serialization boundary.
func (u NormalizedUsage) OpenAIMap() map[string]any {
	result := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.CachedInputTokens > 0 || u.PromptAudioTokens > 0 || u.PromptTextTokens > 0 {
		promptDetails := make(map[string]any, 3)
		if u.CachedInputTokens > 0 {
			promptDetails["cached_tokens"] = u.CachedInputTokens
		}
		if u.PromptAudioTokens > 0 {
			promptDetails["audio_tokens"] = u.PromptAudioTokens
		}
		if u.PromptTextTokens > 0 {
			promptDetails["text_tokens"] = u.PromptTextTokens
		}
		result["prompt_tokens_details"] = promptDetails
	}
	if u.ReasoningTokens > 0 || u.CompletionImageTokens > 0 || u.CompletionAudioTokens > 0 || u.CompletionTextTokens > 0 {
		completionDetails := make(map[string]any, 4)
		if u.ReasoningTokens > 0 {
			completionDetails["reasoning_tokens"] = u.ReasoningTokens
		}
		if u.CompletionImageTokens > 0 {
			completionDetails["image_tokens"] = u.CompletionImageTokens
		}
		if u.CompletionAudioTokens > 0 {
			completionDetails["audio_tokens"] = u.CompletionAudioTokens
		}
		if u.CompletionTextTokens > 0 {
			completionDetails["text_tokens"] = u.CompletionTextTokens
		}
		result["completion_tokens_details"] = completionDetails
	}
	return result
}

func usageCount(values map[string]any, keys ...string) int {
	for _, key := range keys {
		if count := numOf(values[key]); count != 0 {
			return count
		}
	}
	return 0
}

func usageDetailValues(meta map[string]any, keys ...string) []any {
	for _, key := range keys {
		if details, ok := meta[key].([]any); ok && len(details) > 0 {
			return details
		}
	}
	return nil
}

func usageDetailCount(detail map[string]any) int {
	return usageCount(detail, "tokenCount", "tokens", "token_count")
}

func sumUsageDetails(details []any) int {
	total := 0
	for _, raw := range details {
		if detail, ok := raw.(map[string]any); ok {
			count := usageDetailCount(detail)
			if total > math.MaxInt-count {
				return 0
			}
			total += count
		}
	}
	return total
}

func addUsageCounts(left, right int) int {
	if left < 0 || right < 0 || left > math.MaxInt-right {
		return 0
	}
	return left + right
}
