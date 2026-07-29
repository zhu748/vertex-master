package vertex

import (
	"encoding/base64"
	"strings"
	"testing"
)

var benchmarkAudioData AudioData   //nolint:gochecknoglobals
var benchmarkImageData []ImageData //nolint:gochecknoglobals
var benchmarkDecodedBase64 []byte  //nolint:gochecknoglobals

func BenchmarkAppendBase64LooseOneMiB(b *testing.B) {
	raw := make([]byte, 1<<20)
	for index := range raw {
		raw[index] = byte(index)
	}
	standard := base64.StdEncoding.EncodeToString(raw)
	urlSafe := base64.RawURLEncoding.EncodeToString(raw)

	for name, input := range map[string]string{
		"standard":          standard,
		"url_safe_unpadded": urlSafe,
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for range b.N {
				var err error
				benchmarkDecodedBase64, err = appendBase64Loose(nil, input)
				if err != nil || len(benchmarkDecodedBase64) != len(raw) {
					b.Fatalf("decoded bytes=%d, err=%v", len(benchmarkDecodedBase64), err)
				}
			}
		})
	}
}

func BenchmarkExtractAudioResponseSixteenSegments(b *testing.B) {
	segment := make([]byte, 64<<10)
	for index := range segment {
		segment[index] = byte(index)
	}
	encoded := base64.StdEncoding.EncodeToString(segment)
	parts := make([]any, 16)
	for index := range parts {
		parts[index] = map[string]any{"inlineData": map[string]any{
			"data": encoded, "mimeType": "audio/L16;rate=24000",
		}}
	}
	result := makeResult(parts)
	b.ReportAllocs()
	b.SetBytes(16 * int64(len(segment)))
	for range b.N {
		benchmarkAudioData = extractAudioResponse(result)
		if len(benchmarkAudioData.Raw) == 0 {
			b.Fatal("audio extraction returned no data")
		}
	}
}

func BenchmarkExtractImageResponse(b *testing.B) {
	b.Run("sixteen_inline_with_text", func(b *testing.B) {
		encoded := strings.Repeat("A", 64<<10)
		text := strings.Repeat("description ", 256)
		parts := make([]any, 0, 32)
		for range 16 {
			parts = append(parts,
				map[string]any{"text": text},
				map[string]any{"inlineData": map[string]any{
					"data": encoded, "mimeType": "image/png",
				}},
			)
		}
		result := makeResult(parts)
		b.ReportAllocs()
		for range b.N {
			benchmarkImageData = extractImageResponse(result)
			if len(benchmarkImageData) != 16 {
				b.Fatalf("image extraction returned %d items", len(benchmarkImageData))
			}
		}
	})

	b.Run("markdown_one_mib_split", func(b *testing.B) {
		markdown := "![Generated Image](data:image/png;base64," + strings.Repeat("A", 1<<20) + ")"
		const segments = 16
		segmentSize := len(markdown) / segments
		parts := make([]any, 0, segments)
		for start := 0; start < len(markdown); start += segmentSize {
			end := min(start+segmentSize, len(markdown))
			parts = append(parts, map[string]any{"text": markdown[start:end]})
		}
		result := makeResult(parts)
		b.ReportAllocs()
		b.SetBytes(int64(len(markdown)))
		for range b.N {
			benchmarkImageData = extractImageResponse(result)
			if len(benchmarkImageData) != 1 {
				b.Fatalf("markdown image extraction returned %d items", len(benchmarkImageData))
			}
		}
	})
}

// makeResult 构造一个最小 Gemini 响应：candidates[0].content.parts = parts。
func makeResult(parts []any) map[string]any {
	return map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{"parts": parts},
			},
		},
	}
}

// ---- extractImageResponse ----

func TestExtractImageResponseInlineData(t *testing.T) {
	result := makeResult([]any{
		map[string]any{"inlineData": map[string]any{"data": "AAAA", "mimeType": "image/png"}},
		map[string]any{"inlineData": map[string]any{"data": "BBBB", "mimeType": "image/jpeg"}},
	})
	imgs := extractImageResponse(result)
	if len(imgs) != 2 {
		t.Fatalf("应抽出 2 张图，实际 %d", len(imgs))
	}
	if imgs[0].B64JSON != "AAAA" || imgs[0].MimeType != "image/png" {
		t.Errorf("图1 不符: %+v", imgs[0])
	}
	if imgs[1].B64JSON != "BBBB" || imgs[1].MimeType != "image/jpeg" {
		t.Errorf("图2 不符: %+v", imgs[1])
	}
}

func TestExtractImageResponseInlineDataDefaultMime(t *testing.T) {
	result := makeResult([]any{
		map[string]any{"inlineData": map[string]any{"data": "CCCC"}}, // 无 mimeType
	})
	imgs := extractImageResponse(result)
	if len(imgs) != 1 || imgs[0].MimeType != "image/png" {
		t.Fatalf("缺 mimeType 应默认 image/png，实际 %+v", imgs)
	}
}

func TestExtractImageResponseInlineDataSkipsEmptyData(t *testing.T) {
	result := makeResult([]any{
		map[string]any{"inlineData": map[string]any{"data": "", "mimeType": "image/png"}},
		map[string]any{"inlineData": map[string]any{"data": "DDDD", "mimeType": "image/png"}},
	})
	imgs := extractImageResponse(result)
	if len(imgs) != 1 || imgs[0].B64JSON != "DDDD" {
		t.Fatalf("空 data 段应跳过，实际 %+v", imgs)
	}
}

func TestExtractImageResponseEmptyInlineDataKeepsPriority(t *testing.T) {
	result := makeResult([]any{
		map[string]any{"text": "![Generated Image](data:image/png;base64,EEEE)"},
		map[string]any{"inlineData": map[string]any{"data": ""}},
	})
	if imgs := extractImageResponse(result); len(imgs) != 0 {
		t.Fatalf("存在 inlineData 时不应退回 markdown 图片，实际 %+v", imgs)
	}
}

func TestExtractImageResponseMarkdownFallback(t *testing.T) {
	// 无 inlineData，全文以 markdown data-URI 开头 → 退化抠取。
	md := "![Generated Image](data:image/png;base64,EEEE)"
	result := makeResult([]any{
		map[string]any{"text": md},
	})
	imgs := extractImageResponse(result)
	if len(imgs) != 1 {
		t.Fatalf("markdown 退化应抽 1 张，实际 %d", len(imgs))
	}
	if imgs[0].B64JSON != "EEEE" {
		t.Errorf("应抠出 EEEE，实际 %q", imgs[0].B64JSON)
	}
}

func TestExtractImageResponseNoImage(t *testing.T) {
	result := makeResult([]any{
		map[string]any{"text": "just plain text, no image"},
	})
	if imgs := extractImageResponse(result); len(imgs) != 0 {
		t.Errorf("无图应返回空，实际 %+v", imgs)
	}
}

func TestExtractImageResponseEmptyCandidates(t *testing.T) {
	if imgs := extractImageResponse(map[string]any{}); len(imgs) != 0 {
		t.Errorf("空响应应返回空，实际 %+v", imgs)
	}
	if imgs := extractImageResponse(map[string]any{"candidates": []any{}}); len(imgs) != 0 {
		t.Errorf("空 candidates 应返回空，实际 %+v", imgs)
	}
}

// ---- extractAudioResponse：多段拼接守护回归（不能只取首段）----

func TestExtractAudioResponseConcatenatesAllSegments(t *testing.T) {
	// 三段 audio/L16，原始字节分别 3 / 4 / 5 字节。
	seg1 := []byte{0x01, 0x02, 0x03}
	seg2 := []byte{0x04, 0x05, 0x06, 0x07}
	seg3 := []byte{0x08, 0x09, 0x0a, 0x0b, 0x0c}
	enc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

	result := makeResult([]any{
		map[string]any{"inlineData": map[string]any{"data": enc(seg1), "mimeType": "audio/L16;rate=24000"}},
		map[string]any{"inlineData": map[string]any{"data": enc(seg2), "mimeType": "audio/L16;rate=24000"}},
		map[string]any{"inlineData": map[string]any{"data": enc(seg3), "mimeType": "audio/L16;rate=24000"}},
	})

	audio := extractAudioResponse(result)
	if len(audio.Raw) == 0 {
		t.Fatalf("应返回拼接音频")
	}
	if audio.Data != "" {
		t.Fatalf("正常提取路径不应重新编码整段 Base64")
	}
	decoded := audio.Raw
	wantLen := len(seg1) + len(seg2) + len(seg3) // 12
	if len(decoded) != wantLen {
		t.Fatalf("🔴 拼接长度应为 3 段之和 %d（守护「只取首段=截断」回归），实际 %d", wantLen, len(decoded))
	}
	// 验证字节序正确拼接。
	want := append(append(append([]byte{}, seg1...), seg2...), seg3...)
	for i := range want {
		if decoded[i] != want[i] {
			t.Fatalf("拼接字节序错误 @%d: got %#x want %#x", i, decoded[i], want[i])
		}
	}
	// mime 取首个有效段。
	if audio.MimeType != "audio/L16;rate=24000" {
		t.Errorf("mime 应取首段，实际 %q", audio.MimeType)
	}
}

func TestExtractAudioResponseMimeFromFirstValid(t *testing.T) {
	enc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	// 首段无 mime（应默认），但因首个有效段无 mime → mime 取默认。
	result := makeResult([]any{
		map[string]any{"inlineData": map[string]any{"data": enc([]byte{1, 2})}}, // 无 mimeType
		map[string]any{"inlineData": map[string]any{"data": enc([]byte{3, 4}), "mimeType": "audio/wav"}},
	})
	audio := extractAudioResponse(result)
	if audio.MimeType != "audio/L16;rate=24000" {
		t.Errorf("首个有效段无 mime 时应默认 audio/L16;rate=24000，实际 %q", audio.MimeType)
	}
	decoded := audio.Raw
	if len(decoded) != 4 {
		t.Errorf("应拼接两段共 4 字节，实际 %d", len(decoded))
	}
}

func TestExtractAudioResponseSkipsNonAudio(t *testing.T) {
	enc := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	result := makeResult([]any{
		map[string]any{"inlineData": map[string]any{"data": enc([]byte{1, 2, 3}), "mimeType": "image/png"}}, // 跳过
		map[string]any{"inlineData": map[string]any{"data": enc([]byte{4, 5}), "mimeType": "audio/L16"}},
	})
	audio := extractAudioResponse(result)
	decoded := audio.Raw
	if len(decoded) != 2 {
		t.Errorf("非 audio/* 段应跳过，应只拼 2 字节，实际 %d", len(decoded))
	}
	if audio.MimeType != "audio/L16" {
		t.Errorf("mime 应取首个 audio 段，实际 %q", audio.MimeType)
	}
}

func TestExtractAudioResponsePreservesLooseBase64Compatibility(t *testing.T) {
	result := makeResult([]any{
		map[string]any{"inlineData": map[string]any{"data": "-_-_", "mimeType": "audio/L16"}},
		map[string]any{"inlineData": map[string]any{"data": "@@@@", "mimeType": "audio/L16"}},
		map[string]any{"inlineData": map[string]any{"data": "aGVsbG8", "mimeType": "audio/L16"}},
	})
	audio := extractAudioResponse(result)
	want := append(mustStdDecode("+/+/"), []byte("hello")...)
	if string(audio.Raw) != string(want) {
		t.Fatalf("宽松 Base64 拼接=%v，期望 %v", audio.Raw, want)
	}
}

func TestExtractAudioResponseEmpty(t *testing.T) {
	result := makeResult([]any{
		map[string]any{"text": "no audio here"},
	})
	audio := extractAudioResponse(result)
	if len(audio.Raw) != 0 || audio.Data != "" || audio.MimeType != "" {
		t.Errorf("无音频应返回空 AudioData，实际 %+v", audio)
	}
}

// ---- decodeBase64Loose ----

func TestDecodeBase64Loose(t *testing.T) {
	std := base64.StdEncoding.EncodeToString([]byte("hello world"))

	cases := []struct {
		name    string
		in      string
		want    []byte
		wantErr bool
	}{
		{"standard base64", std, []byte("hello world"), false},
		// URL-safe: 含 -/_ 字符。bytes 0xfb 0xff 0xbf → std "+/+/" 中含 +、/
		{"url-safe with dash underscore", "-_-_", mustStdDecode("+/+/"), false},
		{"mixed standard and URL-safe alphabet", "+_+/", mustStdDecode("+/+/"), false},
		{"missing padding restored", "aGVsbG8", []byte("hello"), false}, // "hello" std = aGVsbG8= (缺1个=)
		{"line breaks ignored", "aGVs\r\nbG8=", []byte("hello"), false},
		{"empty string", "", []byte{}, false},
		{"invalid chars", "@@@@", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeBase64Loose(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("应返回错误，实际 got=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if string(got) != string(c.want) {
				t.Errorf("decodeBase64Loose(%q)=%v，期望 %v", c.in, got, c.want)
			}
		})
	}
}

func TestAppendBase64LoosePreservesDestinationOnFailure(t *testing.T) {
	prefix := []byte("existing-audio")
	got, err := appendBase64Loose(append([]byte(nil), prefix...), "@@@@")
	if err == nil {
		t.Fatal("invalid Base64 must return an error")
	}
	if string(got) != string(prefix) {
		t.Fatalf("destination changed after failed decode: got %q, want %q", got, prefix)
	}

	got, err = appendBase64Loose(got, "aGVsbG8")
	if err != nil {
		t.Fatalf("append unpadded Base64: %v", err)
	}
	if want := "existing-audiohello"; string(got) != want {
		t.Fatalf("appended bytes=%q, want %q", got, want)
	}
}

func mustStdDecode(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
