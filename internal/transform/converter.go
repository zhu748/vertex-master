package transform

import "github.com/bsfdsagfadg/vertex/internal/config"

type RequestConverter interface {
	Convert(body map[string]any, cfg config.ConfigProvider) (model string, geminiPayload map[string]any, err error)
}

type ResponseConverter interface {
	ToOAI(geminiResp map[string]any, model string) map[string]any
	StreamToSSE(chunk map[string]any, model, requestID string, isFirst bool) []string
	AggregateN(responses []map[string]any, model string) map[string]any
}

// StreamEventEncoder 把单个上游 chunk 转为一个或多个可直接 JSON 序列化的
// OpenAI 流事件。emit 必须在返回前同步消费 payload，不能保留其引用。
type StreamEventEncoder interface {
	Emit(chunk map[string]any, isFirst bool, emit func(payload any) bool) (StreamEventResult, bool)
}

// TextStreamEventEncoder 是严格单文本帧的可选零 map 快速路径。
type TextStreamEventEncoder interface {
	EmitText(
		text, finishReason string,
		isFirst bool,
		emit func(payload any) bool,
	) (StreamEventResult, bool)
}

type StreamEventResult struct {
	HasContent bool
	HasFinish  bool
}

// StreamingResponseConverter 是 ResponseConverter 的可选快速路径。自定义转换器
// 无需实现；调用方会继续使用 StreamToSSE 兼容接口。
type StreamingResponseConverter interface {
	NewStreamEventEncoder(model, requestID string) StreamEventEncoder
}

type defaultRequestConverter struct{}

func (defaultRequestConverter) Convert(body map[string]any, cfg config.ConfigProvider) (string, map[string]any, error) {
	return ConvertChatRequest(body, cfg)
}

func DefaultRequestConverter() RequestConverter { return defaultRequestConverter{} }

type defaultResponseConverter struct{}

func (defaultResponseConverter) ToOAI(geminiResp map[string]any, model string) map[string]any {
	return GeminiJSONToOAIJSON(geminiResp, model)
}

func (defaultResponseConverter) StreamToSSE(chunk map[string]any, model, requestID string, isFirst bool) []string {
	return ConvertRealtimeChunk(chunk, model, requestID, isFirst)
}

func (defaultResponseConverter) NewStreamEventEncoder(model, requestID string) StreamEventEncoder {
	return NewOpenAIStreamEncoder(model, requestID)
}

func (defaultResponseConverter) AggregateN(responses []map[string]any, model string) map[string]any {
	return GeminiResponsesToOAIJSON(responses, model)
}

func DefaultResponseConverter() ResponseConverter { return defaultResponseConverter{} }
