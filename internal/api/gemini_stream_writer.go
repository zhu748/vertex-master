package api

// geminiTextStreamEncoder 将最常见的单文本 Gemini 增量映射到可复用类型，
// 避免每帧通过 map[string]any 做反射编码。只有字段集合完全已知时才命中；
// metadata、工具调用、安全信息和多候选等形状继续交给通用写出器。
type geminiTextStreamEncoder struct {
	event      geminiTextStreamEvent
	candidates [1]geminiTextStreamCandidate
	parts      [2]geminiTextStreamPart
	index      int
	thought    bool
	signature  string
}

type geminiTextStreamEvent struct {
	Candidates []geminiTextStreamCandidate `json:"candidates"`
}

type geminiTextStreamCandidate struct {
	Content      geminiTextStreamContent `json:"content"`
	FinishReason string                  `json:"finishReason,omitempty"`
	Index        *int                    `json:"index,omitempty"`
}

type geminiTextStreamContent struct {
	Parts []geminiTextStreamPart `json:"parts"`
	Role  string                 `json:"role"`
}

type geminiTextStreamPart struct {
	Text             string  `json:"text"`
	Thought          *bool   `json:"thought,omitempty"`
	ThoughtSignature *string `json:"thoughtSignature,omitempty"`
}

func (e *geminiTextStreamEncoder) init() {
	e.event.Candidates = e.candidates[:]
	e.candidates[0].Content.Parts = e.parts[:]
	e.candidates[0].Content.Role = "model"
}

func (e *geminiTextStreamEncoder) writeData(sw *sseWriter, data map[string]any) bool {
	if e.prepare(data) {
		return sw.writeData(&e.event)
	}
	return sw.writeData(data)
}

func (e *geminiTextStreamEncoder) writeCanonical(
	sw *sseWriter,
	text, tail, finishReason string,
	hasIndex, dirty bool,
) bool {
	if e == nil || sw == nil {
		return false
	}
	e.reset(text)
	e.candidates[0].FinishReason = finishReason
	if hasIndex {
		e.candidates[0].Index = &e.index
	}
	if dirty {
		e.parts[0].Thought = &e.thought
		e.parts[0].ThoughtSignature = &e.signature
	}
	if tail != "" {
		e.parts[1].Text = tail
		e.candidates[0].Content.Parts = e.parts[:2]
	}
	return sw.writeData(&e.event)
}

func (e *geminiTextStreamEncoder) prepare(data map[string]any) bool {
	if e == nil || len(data) != 1 {
		return false
	}
	candidates, ok := data["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		return false
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok || len(candidate) < 1 || len(candidate) > 3 {
		return false
	}
	for key := range candidate {
		if key != "content" && key != "finishReason" && key != "index" {
			return false
		}
	}

	content, ok := candidate["content"].(map[string]any)
	if !ok || len(content) != 2 || content["role"] != "model" {
		return false
	}
	for key := range content {
		if key != "parts" && key != "role" {
			return false
		}
	}
	parts, ok := content["parts"].([]any)
	if !ok || len(parts) != 1 {
		return false
	}
	part, ok := parts[0].(map[string]any)
	if !ok || len(part) < 1 || len(part) > 3 {
		return false
	}
	for key := range part {
		if key != "text" && key != "thought" && key != "thoughtSignature" {
			return false
		}
	}
	text, ok := part["text"].(string)
	if !ok {
		return false
	}

	e.reset(text)

	if rawFinish, exists := candidate["finishReason"]; exists {
		finishReason, ok := rawFinish.(string)
		if !ok || finishReason == "" {
			return false
		}
		e.candidates[0].FinishReason = finishReason
	}
	if rawIndex, exists := candidate["index"]; exists {
		if !isZeroGeminiCandidateIndex(rawIndex) {
			return false
		}
		e.candidates[0].Index = &e.index
	}
	if rawThought, exists := part["thought"]; exists {
		thought, ok := rawThought.(bool)
		if !ok {
			return false
		}
		e.thought = thought
		e.parts[0].Thought = &e.thought
	}
	if rawSignature, exists := part["thoughtSignature"]; exists {
		signature, ok := rawSignature.(string)
		if !ok {
			return false
		}
		e.signature = signature
		e.parts[0].ThoughtSignature = &e.signature
	}
	return true
}

func (e *geminiTextStreamEncoder) reset(text string) {
	e.index = 0
	e.thought = false
	e.signature = ""
	e.candidates[0].FinishReason = ""
	e.candidates[0].Index = nil
	e.candidates[0].Content.Parts = e.parts[:1]
	e.parts[0].Text = text
	e.parts[0].Thought = nil
	e.parts[0].ThoughtSignature = nil
	e.parts[1] = geminiTextStreamPart{}
}

func isZeroGeminiCandidateIndex(value any) bool {
	switch index := value.(type) {
	case float64:
		return index == 0
	case int:
		return index == 0
	default:
		return false
	}
}
