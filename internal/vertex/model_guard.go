package vertex

import "github.com/bsfdsagfadg/vertex/internal/config"

func (c *VertexAIClient) generationModelError(model string) *VertexError {
	if config.IsVideoModel(c.cfg.ResolveModelName(model)) {
		return &VertexError{Code: 501, Status: "UNIMPLEMENTED", Kind: "unsupported",
			Message: "视频模型目前仅供目录展示，匿名生成端点不支持视频 (video generation is not supported)"}
	}
	return nil
}
