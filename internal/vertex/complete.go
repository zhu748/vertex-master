package vertex

import (
	"context"
	"sort"

	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

type candidateResult struct {
	proxyURI string
	resp     map[string]any
	err      error
}

func (c *VertexAIClient) CompleteChat(ctx context.Context, model string, geminiPayload map[string]any) (map[string]any, error) {
	selected, err := RunRacePreferred(
		ctx,
		c.cfg,
		func(candidateCtx context.Context, proxyURI string) (candidateResult, error) {
			copiedPayload := deepCopyAny(geminiPayload).(map[string]any)
			response, err := c.runSingleCandidate(candidateCtx, model, copiedPayload, proxyURI)
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
	var chunks []map[string]any
	var firstErr *VertexError

	c.executeStreamingWithRetries(ctx, model, geminiPayload, proxyURI, func(chunk StreamChunk) bool {
		if chunk.Err != nil {
			if firstErr == nil {
				firstErr = chunk.Err
			}
			return false
		}
		if chunk.Data != nil {
			chunks = append(chunks, chunk.Data)
		}
		return true
	})

	if firstErr != nil {
		return nil, firstErr
	}
	if len(chunks) == 0 {
		return nil, NewEmptyResponseError("Upstream returned no data")
	}

	result := collectChunksToParseResult(chunks)
	resp, err := c.buildCompleteResponse(result)
	if err != nil {
		return nil, err
	}

	if _, hasSafety := geminiPayload["safetySettings"]; candidateFinish(resp) == "SAFETY" && !hasSafety {
		retryPayload := shallowCopy(geminiPayload)
		retryPayload["safetySettings"] = defaultSafetySettings
		return c.runSingleCandidate(ctx, model, retryPayload, proxyURI)
	}

	return resp, nil
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
