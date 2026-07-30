package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeGeminiRequestBodyRejectsDroppedPromptFragments(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "invalid wrapper",
			body: map[string]any{"generateContentRequest": "prompt"},
			want: "generateContentRequest must be an object",
		},
		{
			name: "invalid content item",
			body: map[string]any{"contents": []any{
				map[string]any{"role": "user", "parts": []any{map[string]any{"text": "keep"}}},
				true,
			}},
			want: "contents[1] must be a string or object",
		},
		{
			name: "invalid part item",
			body: map[string]any{"contents": []any{map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"text": "keep"},
					float64(42),
				},
			}}},
			want: "contents[0].parts[1] must be a string or object",
		},
		{
			name: "ambiguous content fields",
			body: map[string]any{"contents": []any{map[string]any{
				"parts":   []any{map[string]any{"text": "one"}},
				"content": "two",
			}}},
			want: "cannot contain both parts and content",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeGeminiRequestBody(test.body); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestNormalizeGeminiRequestBodyUnwrapsValidRequest(t *testing.T) {
	request := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{
			map[string]any{"text": "hello"},
			"legacy text",
		}},
	}}
	body, err := normalizeGeminiRequestBody(map[string]any{"generateContentRequest": request})
	if err != nil {
		t.Fatal(err)
	}
	if body["contents"] == nil {
		t.Fatalf("valid wrapped request was not preserved: %#v", body)
	}
}

func TestReadGeminiBodyRejectsInvalidPromptShape(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1beta/models/gemini-test:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":["keep",false]}]}`),
	)
	if body, ok := (&GeminiHandler{}).readGeminiBody(recorder, request); ok || body != nil {
		t.Fatalf("invalid prompt shape was accepted: %#v", body)
	}
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "contents[0].parts[1]") {
		t.Fatalf("unexpected validation response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
