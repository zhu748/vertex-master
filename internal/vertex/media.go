package vertex

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
)

// ImageData 是一张抽出的图片（base64 + mime）。
type ImageData struct {
	B64JSON  string
	MimeType string
}

// AudioData 是抽出的整段音频（base64 + mime）。
type AudioData struct {
	// Raw 是已按顺序拼接的音频字节，正常提取路径直接填充它，避免 API 层
	// 再执行整段 Base64 编解码。Data 仅保留给旧调用方构造的兼容值。
	Raw      []byte
	Data     string
	MimeType string
}

// CompleteChatImage 走标准非流式请求，再从响应抽取图片数据。
func (c *VertexAIClient) CompleteChatImage(ctx context.Context, model string, geminiPayload map[string]any) ([]ImageData, error) {
	result, err := c.CompleteChat(ctx, model, geminiPayload)
	if err != nil {
		return nil, err
	}
	return extractImageResponse(result), nil
}

// CompleteChatAudio 走标准非流式请求，再从响应抽取（拼接）音频数据。
func (c *VertexAIClient) CompleteChatAudio(ctx context.Context, model string, geminiPayload map[string]any) (AudioData, error) {
	result, err := c.CompleteChat(ctx, model, geminiPayload)
	if err != nil {
		return AudioData{}, err
	}
	return extractAudioResponse(result), nil
}

// extractImageResponse 从完整 Gemini 响应抽取图片。
//
// ① 优先 inlineData：每个带 data 的 inlineData → {b64_json, mime_type}。
// ② 退化：若全文以 "![Generated Image](data:" 开头，则从 markdown 抠出 base64。
// 无图返回空切片（路由层据此返 502）。
func extractImageResponse(result map[string]any) []ImageData {
	allParts := firstCandidateParts(result)

	inlineImageCount := 0
	hasInlineData := false
	fullTextLength := 0
	for _, pRaw := range allParts {
		p, ok := pRaw.(map[string]any)
		if !ok {
			continue
		}
		if text := toStr(p["text"]); fullTextLength >= 0 {
			maximumInt := int(^uint(0) >> 1)
			if len(text) > maximumInt-fullTextLength {
				fullTextLength = -1
			} else {
				fullTextLength += len(text)
			}
		}
		if inline, ok := p["inlineData"].(map[string]any); ok {
			hasInlineData = true
			if toStr(inline["data"]) != "" {
				inlineImageCount++
			}
		}
	}

	// ① inlineData 格式
	if hasInlineData {
		if inlineImageCount == 0 {
			return nil
		}
		out := make([]ImageData, 0, inlineImageCount)
		for _, pRaw := range allParts {
			part, ok := pRaw.(map[string]any)
			if !ok {
				continue
			}
			inline, ok := part["inlineData"].(map[string]any)
			if !ok {
				continue
			}
			data := toStr(inline["data"])
			if data == "" {
				continue
			}
			mime := toStr(inline["mimeType"])
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, ImageData{B64JSON: data, MimeType: mime})
		}
		return out
	}

	// ② markdown 退化格式
	var fullText strings.Builder
	if fullTextLength > 0 {
		fullText.Grow(fullTextLength)
	}
	for _, pRaw := range allParts {
		if part, ok := pRaw.(map[string]any); ok {
			fullText.WriteString(toStr(part["text"]))
		}
	}
	text := fullText.String()
	if strings.HasPrefix(text, "![Generated Image](data:") {
		startIdx := strings.Index(text, "(")
		endIdx := strings.LastIndex(text, ")")
		if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
			dataURL := text[startIdx+1 : endIdx]
			if comma := strings.Index(dataURL, ","); comma != -1 {
				encoded := dataURL[comma+1:]
				if encoded != "" {
					return []ImageData{{B64JSON: encoded}}
				}
			}
		}
	}
	return nil
}

// extractAudioResponse 从完整 Gemini 响应抽取并拼接 TTS 音频。
//
// Gemini TTS 把整段音频切成多段 inlineData（每段一小块 L16 PCM），必须把所有音频段的
// 原始字节按序拼接，否则只取第一段会被截断成几十毫秒（血泪教训）。返回拼接后整段的 base64 + mime。
func extractAudioResponse(result map[string]any) AudioData {
	allParts := firstCandidateParts(result)

	decodedCapacity := 0
	maximumInt := int(^uint(0) >> 1)
	for _, part := range allParts {
		data, _, ok := audioInlineSegment(part)
		if !ok {
			continue
		}
		segmentCapacity := base64.StdEncoding.DecodedLen(len(data))
		if segmentCapacity > maximumInt-decodedCapacity {
			return AudioData{}
		}
		decodedCapacity += segmentCapacity
	}

	raw := make([]byte, 0, decodedCapacity)
	mime := ""
	for _, pRaw := range allParts {
		data, segmentMime, ok := audioInlineSegment(pRaw)
		if !ok {
			continue
		}
		if mime == "" {
			if segmentMime != "" {
				mime = segmentMime
			} else {
				mime = "audio/L16;rate=24000"
			}
		}
		var err error
		raw, err = appendBase64Loose(raw, data)
		if err != nil {
			continue // 单段解码失败跳过
		}
	}

	if len(raw) > 0 {
		if mime == "" {
			mime = "audio/L16;rate=24000"
		}
		return AudioData{Raw: raw, MimeType: mime}
	}
	return AudioData{}
}

func audioInlineSegment(raw any) (data, mime string, ok bool) {
	part, ok := raw.(map[string]any)
	if !ok {
		return "", "", false
	}
	inline, ok := part["inlineData"].(map[string]any)
	if !ok {
		return "", "", false
	}
	data = toStr(inline["data"])
	if data == "" {
		return "", "", false
	}
	mime = toStr(inline["mimeType"])
	// 仅接受 audio/* 或无 mime 的段（按既定条件）。
	if mime != "" && !strings.HasPrefix(mime, "audio/") {
		return "", "", false
	}
	return data, mime, true
}

func appendBase64Loose(dst []byte, encoded string) ([]byte, error) {
	start := len(dst)
	decodedCapacity := base64.StdEncoding.DecodedLen(len(encoded))
	dst = slices.Grow(dst, decodedCapacity)
	dst = dst[:start+decodedCapacity]
	written, err := base64.StdEncoding.Decode(dst[start:], []byte(encoded))
	if err == nil {
		return dst[:start+written], nil
	}
	dst = dst[:start]

	// URL-safe 字符替换 + 补 padding，与 decodeBase64Loose 的兼容语义一致。
	normalized := strings.ReplaceAll(strings.ReplaceAll(encoded, "-", "+"), "_", "/")
	if pad := len(normalized) % 4; pad != 0 {
		normalized += strings.Repeat("=", 4-pad)
	}
	decoded, fallbackErr := base64.StdEncoding.DecodeString(normalized)
	if fallbackErr != nil {
		return dst, fallbackErr //nolint:wrapcheck
	}
	return append(dst, decoded...), nil
}

// firstCandidateParts 取 result.candidates[0].content.parts（无则空切片）。
func firstCandidateParts(result map[string]any) []any {
	cands, ok := result["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return nil
	}
	c, ok := cands[0].(map[string]any)
	if !ok {
		return nil
	}
	content, ok := c["content"].(map[string]any)
	if !ok {
		return nil
	}
	parts, _ := content["parts"].([]any)
	return parts
}

// decodeBase64Loose 容错解码 base64：先 standard、失败再 URL-safe、再补 padding，保持宽松性
// （上游偶有 URL-safe / 缺 padding 的段）。
func decodeBase64Loose(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	// URL-safe 字符替换 + 补 padding
	t := strings.ReplaceAll(strings.ReplaceAll(s, "-", "+"), "_", "/")
	if pad := len(t) % 4; pad != 0 {
		t += strings.Repeat("=", 4-pad)
	}
	return base64.StdEncoding.DecodeString(t) //nolint:wrapcheck
}
