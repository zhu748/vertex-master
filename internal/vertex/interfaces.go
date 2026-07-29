package vertex

import (
	"context"
)

type ChatCompleter interface {
	CompleteChat(ctx context.Context, model string, geminiPayload map[string]any) (map[string]any, error)
	CompleteChatN(ctx context.Context, model string, geminiPayload map[string]any, n int) ([]map[string]any, error)
	StreamChat(ctx context.Context, model string, geminiPayload map[string]any, yield func(StreamChunk) bool)
}

type ImageGenerator interface {
	CompleteChatImage(ctx context.Context, model string, geminiPayload map[string]any) ([]ImageData, error)
}

type AudioGenerator interface {
	CompleteChatAudio(ctx context.Context, model string, geminiPayload map[string]any) (AudioData, error)
}

var (
	_ ChatCompleter  = (*VertexAIClient)(nil)
	_ ImageGenerator = (*VertexAIClient)(nil)
	_ AudioGenerator = (*VertexAIClient)(nil)
)
