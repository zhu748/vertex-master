package vertex

import (
	"context"
	"fmt"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

const (
	maxStreamRaceMetadataChunks = 64
	maxStreamRaceMetadataBytes  = 1 << 20
)

type streamRaceResult struct {
	pending   []StreamChunk
	source    <-chan StreamChunk
	preferred bool
}

func RunParallel[T any](ctx context.Context, cfg config.ConfigProvider, run func(context.Context, string) (T, error)) (T, error) {
	return RunRace(ctx, cfg, run)
}

func StreamParallel(ctx context.Context, cfg config.ConfigProvider,
	op func(context.Context, string) <-chan StreamChunk,
	yield func(StreamChunk) bool,
) {
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	wrappedOp := func(ctx context.Context, uri string) (streamRaceResult, error) {
		ch := op(ctx, uri)
		pending := make([]StreamChunk, 0, 4)
		remainingBytes := maxStreamRaceMetadataBytes
		for {
			select {
			case chunk, open := <-ch:
				if !open {
					if len(pending) == 0 {
						return streamRaceResult{}, fmt.Errorf(
							"stream: %s closed immediately",
							nodes.GetNodeName(uri),
						)
					}
					// Metadata-only completion remains a valid soft fallback.
					// This preserves native Gemini pass-through behavior when
					// every candidate returns only prompt/usage metadata.
					return streamRaceResult{pending: pending}, nil
				}
				if chunk.Err != nil {
					return streamRaceResult{}, chunk.Err
				}
				if streamChunkHasPayload(chunk) {
					pending = append(pending, chunk)
					return streamRaceResult{
						pending: pending, source: ch, preferred: true,
					}, nil
				}
				if len(pending) >= maxStreamRaceMetadataChunks ||
					!valueFitsBudget(chunk.Data, &remainingBytes) {
					return streamRaceResult{}, NewUnavailableError(
						"upstream stream metadata lookahead exceeded safe limit",
					)
				}
				pending = append(pending, chunk)
			case <-ctx.Done():
				return streamRaceResult{}, ctx.Err()
			}
		}
	}

	winner, err := RunRacePreferred(
		streamCtx,
		cfg,
		wrappedOp,
		func(result streamRaceResult) bool { return result.preferred },
		chooseStreamRaceFallback,
		WithNoCancelOnSuccess(),
	)
	if err != nil {
		if vertexErr := asVertexError(err); vertexErr != nil {
			yield(StreamChunk{Err: vertexErr})
		} else {
			yield(StreamChunk{Err: NewInternalError(err.Error())})
		}
		return
	}

	for _, chunk := range winner.pending {
		select {
		case <-streamCtx.Done():
			return
		default:
		}
		if !yield(chunk) {
			return
		}
	}
	if winner.source == nil {
		return
	}
	for {
		select {
		case <-streamCtx.Done():
			return
		case chunk, open := <-winner.source:
			if !open || !yield(chunk) {
				return
			}
		}
	}
}

func chooseStreamRaceFallback(results []streamRaceResult) (streamRaceResult, error) {
	if len(results) == 0 {
		return streamRaceResult{}, fmt.Errorf("no stream fallback results")
	}
	// Prefer the richest metadata-only response. Ties retain completion order,
	// matching the old first-success behavior as closely as possible.
	best := results[0]
	for _, candidate := range results[1:] {
		if len(candidate.pending) > len(best.pending) {
			best = candidate
		}
	}
	return best, nil
}

func streamChunkHasPayload(chunk StreamChunk) bool {
	if chunk.Err != nil || chunk.Data == nil {
		return false
	}
	data := chunk.Data

	if reason := promptFeedbackBlockReason(data); reason != "" && !isUnspecifiedBlockReason(reason) {
		return true
	}

	candidates, _ := data["candidates"].([]any)
	for _, rawCandidate := range candidates {
		candidate, ok := rawCandidate.(map[string]any)
		if !ok {
			continue
		}
		finishReason := strings.ToUpper(strings.TrimSpace(toStr(candidate["finishReason"])))
		if finishReason != "" && !strings.Contains(finishReason, "UNSPECIFIED") {
			return true
		}
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && text != "" {
				return true
			}
			for _, key := range []string{
				"functionCall",
				"functionResponse",
				"inlineData",
				"fileData",
				"executableCode",
				"codeExecutionResult",
			} {
				if isTruthyAny(part[key]) {
					return true
				}
			}
		}
	}
	return false
}
