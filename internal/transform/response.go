package transform

import (
	"strings"
	"time"
)

// GeminiJSONToOAIJSON 把 Gemini 非流式响应转为 OpenAI ChatCompletion JSON。
func GeminiJSONToOAIJSON(geminiResp map[string]any, model string) map[string]any {
	candidate := firstCandidate(geminiResp)
	parts := candidateParts(candidate)
	finish, _ := candidate["finishReason"].(string)

	text, toolCalls, reasoning := ExtractParts(parts, false)

	var oaiFinish string
	if finish != "" {
		oaiFinish = MapFinishReason(finish, len(toolCalls) > 0)
	} else if len(toolCalls) > 0 {
		oaiFinish = "tool_calls"
	} else {
		oaiFinish = "stop"
	}

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
		text, toolCalls, reasoning := ExtractParts(parts, false)

		var oaiFinish string
		if finish != "" {
			oaiFinish = MapFinishReason(finish, len(toolCalls) > 0)
		} else if len(toolCalls) > 0 {
			oaiFinish = "tool_calls"
		} else {
			oaiFinish = "stop"
		}

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
			u := ConvertUsageForCandidate(usageMeta, candidate)
			totalPrompt += numOf(u["prompt_tokens"])
			totalCompletion += numOf(u["completion_tokens"])
			totalTokens += numOf(u["total_tokens"])
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

// ConvertUsageForCandidate 把 Gemini usageMetadata 转成 OpenAI usage，并利用候选项
// tokenCount 补齐部分匿名/预览模型只返回 totalTokenCount 时缺失的输出分项。
func ConvertUsageForCandidate(meta, candidate map[string]any) map[string]any {
	promptDetailList := usageDetailList(meta, "promptTokensDetails", "prompt_tokens_details")
	toolDetailList := usageDetailList(meta, "toolUsePromptTokensDetails", "tool_use_prompt_tokens_details")
	candidateDetailList := usageDetailList(meta, "candidatesTokensDetails", "candidates_tokens_details")

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

	prompt := promptBase + toolPrompt
	completion := candidateTokens + thoughts
	total := usageCount(meta, "totalTokenCount", "total_token_count", "total_tokens")
	if total == 0 {
		total = prompt + completion
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
	result := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}

	promptDetails := map[string]any{}
	if c := usageCount(meta, "cachedContentTokenCount", "cached_content_token_count"); c > 0 {
		promptDetails["cached_tokens"] = c
	}
	for _, d := range promptDetailList {
		count := usageDetailCount(d)
		switch strings.ToUpper(toString(d["modality"])) {
		case "AUDIO":
			promptDetails["audio_tokens"] = numOf(promptDetails["audio_tokens"]) + count
		case "TEXT":
			promptDetails["text_tokens"] = numOf(promptDetails["text_tokens"]) + count
		}
	}
	if len(promptDetails) > 0 {
		result["prompt_tokens_details"] = promptDetails
	}

	completionDetails := map[string]any{}
	if t := thoughts; t > 0 {
		completionDetails["reasoning_tokens"] = t
	}
	for _, d := range candidateDetailList {
		count := usageDetailCount(d)
		switch strings.ToUpper(toString(d["modality"])) {
		case "IMAGE":
			completionDetails["image_tokens"] = numOf(completionDetails["image_tokens"]) + count
		case "AUDIO":
			completionDetails["audio_tokens"] = numOf(completionDetails["audio_tokens"]) + count
		case "TEXT":
			completionDetails["text_tokens"] = numOf(completionDetails["text_tokens"]) + count
		}
	}
	if len(completionDetails) > 0 {
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

func usageDetailList(meta map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		if details := asMapSlice(meta[key]); len(details) > 0 {
			return details
		}
	}
	return nil
}

func usageDetailCount(detail map[string]any) int {
	return usageCount(detail, "tokenCount", "tokens", "token_count")
}

func sumUsageDetails(details []map[string]any) int {
	total := 0
	for _, detail := range details {
		total += usageDetailCount(detail)
	}
	return total
}
