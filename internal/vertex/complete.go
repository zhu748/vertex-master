package vertex

import (
	"context"
	"sort"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

type candidateResult struct {
	proxyURI string
	resp     map[string]any
	err      error
}

func (c *VertexAIClient) CompleteChat(ctx context.Context, model string, geminiPayload map[string]any) (map[string]any, error) {
	return c.completeChatPrepared(
		ctx,
		model,
		geminiPayload,
		buildRequestVariables(model, geminiPayload, c.cfg),
	)
}

func (c *VertexAIClient) completeChatPrepared(
	ctx context.Context,
	model string,
	geminiPayload map[string]any,
	preparedVariables map[string]any,
) (map[string]any, error) {
	selected, err := RunRacePreferred(
		ctx,
		c.cfg,
		func(candidateCtx context.Context, proxyURI string) (candidateResult, error) {
			candidatePayload := payloadForCandidate(geminiPayload)
			response, err := c.runSingleCandidatePrepared(
				candidateCtx, model, candidatePayload, preparedVariables, proxyURI,
			)
			return candidateResult{proxyURI: proxyURI, resp: response, err: err}, err
		},
		func(result candidateResult) bool {
			return candidateFinish(result.resp) == "STOP"
		},
		pickBestResult,
	)
	if err != nil {
		return nil, err
	}
	// Preferred STOP results are recorded by the race engine. If all candidates
	// are soft fallbacks, record the exact fallback selected here.
	if candidateFinish(selected.resp) != "STOP" {
		nodes.RecordProxySuccessForRequest(selected.proxyURI, RequestIDFromContext(ctx), 0)
	}
	return selected.resp, nil
}

func (c *VertexAIClient) runSingleCandidate(ctx context.Context, model string, geminiPayload map[string]any, proxyURI string) (map[string]any, error) {
	return c.runSingleCandidatePrepared(
		ctx,
		model,
		geminiPayload,
		buildRequestVariables(model, geminiPayload, c.cfg),
		proxyURI,
	)
}

func (c *VertexAIClient) runSingleCandidatePrepared(
	ctx context.Context,
	model string,
	geminiPayload map[string]any,
	preparedVariables map[string]any,
	proxyURI string,
) (map[string]any, error) {
	collector := newChunkCollector()
	var firstErr *VertexError

	c.executeStreamingWithPreparedVariables(ctx, model, preparedVariables, proxyURI, func(chunk StreamChunk) bool {
		if chunk.Err != nil {
			if firstErr == nil {
				firstErr = chunk.Err
			}
			return false
		}
		if chunk.Data != nil {
			collector.Add(chunk.Data)
		}
		return true
	})

	if firstErr != nil {
		return nil, firstErr
	}
	if collector.Len() == 0 {
		return nil, NewEmptyResponseError("Upstream returned no data")
	}

	result := collector.Result()
	resp, err := c.buildCompleteResponse(result)
	if err != nil {
		return nil, err
	}

	// 与 c6f6b65 行为对齐：仅在 candidateFinish == "SAFETY" 时按需补 safetySettings 重试一次。
	// 不再因 promptFeedback.blockReason == BLOCKED_REASON_UNSPECIFIED 触发重试 ——
	// 匿名 Gemini 上游经常在正常响应里附带该字段，把它当作真正拦截会导致流被提前 abort、客户端拿不到内容。
	if _, hasSafety := geminiPayload["safetySettings"]; candidateFinish(resp) == "SAFETY" && !hasSafety {
		retryPayload := shallowCopy(geminiPayload)
		retryPayload["safetySettings"] = defaultSafetySettings
		return c.runSingleCandidate(ctx, model, retryPayload, proxyURI)
	}

	return resp, nil
}

// promptFeedbackBlockReason 提取 Gemini 响应里的 promptFeedback.blockReason。
// 保留这个 helper 给 stream.go 与测试使用，但生产路径不再依据它做语义重试。
func promptFeedbackBlockReason(resp map[string]any) string {
	feedback, _ := resp["promptFeedback"].(map[string]any)
	return strings.TrimSpace(toStr(feedback["blockReason"]))
}

// isUnspecifiedBlockReason 仅作语义判断 helper 保留。
func isUnspecifiedBlockReason(reason string) bool {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "BLOCKED_REASON_UNSPECIFIED", "BLOCK_REASON_UNSPECIFIED":
		return true
	default:
		return false
	}
}

func pickBestResult(results []candidateResult) (candidateResult, error) {
	sort.Slice(results, func(i, j int) bool {
		fi := candidateFinish(results[i].resp)
		fj := candidateFinish(results[j].resp)
		if fi == "MAX_TOKENS" && fj != "MAX_TOKENS" {
			return true
		}
		if fj == "MAX_TOKENS" && fi != "MAX_TOKENS" {
			return false
		}
		return responseContentLength(results[i].resp) > responseContentLength(results[j].resp)
	})
	return results[0], nil
}

func responseContentLength(resp map[string]any) int {
	cands, ok := resp["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return 0
	}
	c, ok := cands[0].(map[string]any)
	if !ok {
		return 0
	}
	content, ok := c["content"].(map[string]any)
	if !ok {
		return 0
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return 0
	}
	total := 0
	for _, pRaw := range parts {
		p, ok := pRaw.(map[string]any)
		if !ok {
			continue
		}
		total += len(toStr(p["text"]))
	}
	return total
}
