package vertex

import "github.com/bsfdsagfadg/vertex/internal/transform"

// chunkCollector 增量提取流式 chunk 的 parts 与元数据。非流式生成无需再保留
// candidates/content 等外层 map，只保留最终合并确实需要的 part。
type chunkCollector struct {
	result *ParseResult
	parts  *transform.ContentBlockMerger
	count  int
}

func newChunkCollector() *chunkCollector {
	return &chunkCollector{
		result: &ParseResult{
			PromptFeedback: map[string]any{},
			UsageMetadata:  map[string]any{},
		},
		parts: transform.NewContentBlockMerger(4),
	}
}

func (c *chunkCollector) Add(chunk map[string]any) {
	c.count++
	result := c.result
	if candidates, ok := chunk["candidates"].([]any); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			if value := candidate["finishReason"]; isTruthyAny(value) {
				result.FinishReason = toStr(value)
			}
			if value, ok := candidate["finishMessage"]; ok {
				result.FinishMessage = value
			}
			if value := candidate["safetyRatings"]; isTruthyAny(value) {
				result.SafetyRatings = value
			}
			if value := candidate["citationMetadata"]; isTruthyAny(value) {
				result.CitationMetadata = value
			}
			if value := candidate["groundingMetadata"]; isTruthyAny(value) {
				result.GroundingMetadata = value
			}
			if value, ok := candidate["tokenCount"]; ok {
				result.TokenCount = value
			}
			if value, ok := candidate["avgLogprobs"]; ok {
				result.AvgLogprobs = value
			}
			if value, ok := candidate["logprobsResult"]; ok {
				result.LogprobsResult = value
			}
			if value := candidate["index"]; value != nil {
				result.CandidateIndex = toInt(value, 0)
			}

			if content, ok := candidate["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok {
					for _, rawPart := range parts {
						if part, ok := rawPart.(map[string]any); ok {
							c.parts.Add(part)
						}
					}
				}
			}
		}
	}

	if feedback, ok := chunk["promptFeedback"].(map[string]any); ok &&
		len(feedback) > 0 && len(result.PromptFeedback) == 0 {
		result.PromptFeedback = feedback
	}
	if usage, ok := chunk["usageMetadata"]; ok {
		if normalized := toMap(usage); len(normalized) > 0 {
			result.UsageMetadata = normalized
		}
	}
	if value, ok := chunk["createTime"]; ok {
		result.CreateTime = value
	}
	if value, ok := chunk["modelVersion"]; ok {
		result.ModelVersion = value
	}
	if value, ok := chunk["responseId"]; ok {
		result.ResponseID = value
	}
}

func (c *chunkCollector) Len() int { return c.count }

func (c *chunkCollector) Result() *ParseResult {
	c.result.Parts = c.parts.Result()
	return c.result
}
