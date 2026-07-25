package vertex

import "testing"

// ---- parseCountTokensResponse：三种 unwrap 形态 + errors 跳过 + 字符串/数字 totalTokens ----

func TestParseCountTokensResponse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "data.ui.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`,
			want: 42,
		},
		{
			name: "data.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":100}}}]}]`,
			want: 100,
		},
		{
			name: "data.countTokens (number)",
			raw:  `[{"results":[{"data":{"countTokens":{"totalTokens":7}}}]}]`,
			want: 7,
		},
		{
			name: "totalTokens as string",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":"256"}}}}]}]`,
			want: 256,
		},
		{
			name: "single object (not array)",
			raw:  `{"results":[{"data":{"countTokensV2":{"totalTokens":15}}}]}`,
			want: 15,
		},
		{
			name: "entry-level errors skipped",
			raw:  `[{"errors":[{"message":"boom"}]},{"results":[{"data":{"countTokensV2":{"totalTokens":9}}}]}]`,
			want: 9,
		},
		{
			name: "result-level errors skipped",
			raw:  `[{"results":[{"errors":[{"x":1}]},{"data":{"countTokensV2":{"totalTokens":11}}}]}]`,
			want: 11,
		},
		{
			name: "ui preferred over flat",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":1}},"countTokensV2":{"totalTokens":999}}}]}]`,
			want: 1,
		},
		{
			name: "no countData returns 0",
			raw:  `[{"results":[{"data":{"somethingElse":{}}}]}]`,
			want: 0,
		},
		{
			name: "missing totalTokens returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{}}}]}]`,
			want: 0,
		},
		{
			name: "empty results returns 0",
			raw:  `[{"results":[]}]`,
			want: 0,
		},
		{
			name: "invalid json returns 0",
			raw:  `not json{`,
			want: 0,
		},
		{
			name: "json primitive returns 0",
			raw:  `12345`,
			want: 0,
		},
		{
			name: "empty array returns 0",
			raw:  `[]`,
			want: 0,
		},
		{
			name: "totalTokens non-numeric string returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":"abc"}}}]}]`,
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCountTokensResponse([]byte(c.raw)); got != c.want {
				t.Errorf("parseCountTokensResponse(%s)=%d，期望 %d", c.raw, got, c.want)
			}
		})
	}
}

// ---- coerceTokenCount ----

func TestCoerceTokenCount(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64", float64(42), 42},
		{"float64 truncates", float64(42.9), 42},
		{"int", 7, 7},
		{"numeric string", "123", 123},
		{"trimmed not supported (atoi strict)", " 5 ", 0}, // Atoi 不 trim
		{"non-numeric string", "abc", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
		{"zero float", float64(0), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coerceTokenCount(c.in); got != c.want {
				t.Errorf("coerceTokenCount(%v)=%d，期望 %d", c.in, got, c.want)
			}
		})
	}
}
func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name     string
		contents []any
		want     int
	}{
		{
			name:     "nil contents",
			contents: nil,
			want:     0,
		},
		{
			name:     "empty contents",
			contents: []any{},
			want:     0,
		},
		{
			name: "single string content",
			contents: []any{
				"Hello World",
			},
			want: 3, // "Hello World" -> 11 chars * 0.25 = 2.75 -> 3
		},
		{
			name: "single content with ascii text part",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{"text": "Hello, world!"}, // 13 chars * 0.25 = 3.25 -> 4
					},
				},
			},
			want: 4,
		},
		{
			name: "single content with chinese text part",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{"text": "你好，世界"}, // 5 chars * 1.5 = 7.5 -> 8
					},
				},
			},
			want: 8,
		},
		{
			name: "mixed ascii and chinese text part",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{"text": "Hello 世界"}, // 6 ascii (1.5) + 2 chinese (3.0) = 4.5 -> 5
					},
				},
			},
			want: 5,
		},
		{
			name: "OpenAI style image_url part",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{
							"image_url": map[string]any{"url": "data:image/jpeg;base64,abc"},
						},
					},
				},
			},
			want: 1024,
		},
		{
			name: "Gemini style inlineData image part",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{
							"inlineData": map[string]any{
								"mimeType": "image/png",
								"data":     "base64data",
							},
						},
					},
				},
			},
			want: 1024,
		},
		{
			name: "Gemini style fileData image part",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{
							"fileData": map[string]any{
								"mimeType": "image/webp",
								"fileUri":  "gs://bucket/img.webp",
							},
						},
					},
				},
			},
			want: 1024,
		},
		{
			name: "Gemini style inline_data snake_case image part",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{
							"inline_data": map[string]any{
								"mime_type": "image/gif",
								"data":      "base64data",
							},
						},
					},
				},
			},
			want: 1024,
		},
		{
			name: "mixed text and image parts",
			contents: []any{
				map[string]any{
					"parts": []any{
						map[string]any{"text": "Analyze this image:"}, // 20 chars * 0.25 = 5
						map[string]any{
							"inlineData": map[string]any{
								"mimeType": "image/jpeg",
								"data":     "xyz",
							},
						},
						map[string]any{"text": "谢谢"}, // 2 * 1.5 = 3
					},
				},
			},
			want: 1032, // 5 + 1024 + 3 = 1032
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := estimateTokens(c.contents); got != c.want {
				t.Errorf("estimateTokens(%v) = %d, want %d", c.name, got, c.want)
			}
		})
	}
}
