package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 假流式分片必须按完整字符切分。多字节 UTF-8 字符（中文、emoji 等）若在字节边界被
// 截断，半个字符经 JSON 序列化会变成 U+FFFD（）乱码，客户端无法还原。
func TestSplitIntoRuneChunks(t *testing.T) {
	cases := []string{
		"你好，世界！这是一段中文文本。",
		"Hello 世界 mixed ASCII 和中文",
		"emoji 🦊🎉 与 multibyte 字符",
		strings.Repeat("中", 100), // chunkSize 远大于 1，验证每个分片仍是完整字符
		"a",
		"中",
	}
	for _, text := range cases {
		chunks := splitIntoRuneChunks(text)
		// 每个分片都必须是合法 UTF-8（不含被截断的多字节序列）。
		for i, c := range chunks {
			if !utf8.ValidString(c) {
				t.Errorf("文本 %q 第 %d 个分片 %q 不是合法 UTF-8", text, i, c)
			}
		}
		// 所有分片拼接后必须逐字节等于原文。
		if got := strings.Join(chunks, ""); got != text {
			t.Errorf("分片拼接 = %q，want %q", got, text)
		}
	}
}

// 空文本不产生分片。
func TestSplitIntoRuneChunksEmpty(t *testing.T) {
	if chunks := splitIntoRuneChunks(""); chunks != nil {
		t.Errorf("空文本应返回 nil，got %v", chunks)
	}
}
