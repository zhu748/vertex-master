package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSESerializationPreservesFramingAndHTML(t *testing.T) {
	payload := map[string]any{"text": "<b>你好</b> & ok"}
	const wantData = "data: {\"text\":\"<b>你好</b> & ok\"}\n\n"
	if got := sseEvent(payload); got != wantData {
		t.Fatalf("sseEvent=%q, want %q", got, wantData)
	}
	gemini := &GeminiHandler{}
	if got := gemini.geminiSSE(payload); got != wantData {
		t.Fatalf("geminiSSE=%q, want %q", got, wantData)
	}
	if got, want := namedSSE("message", payload),
		"event: message\ndata: {\"text\":\"<b>你好</b> & ok\"}\n\n"; got != want {
		t.Fatalf("namedSSE=%q, want %q", got, want)
	}
}

func TestSSESerializationFailureFallback(t *testing.T) {
	bad := map[string]any{"unsupported": func() {}}
	if got, want := sseEvent(bad), "data: {}\n\n"; got != want {
		t.Fatalf("sseEvent fallback=%q, want %q", got, want)
	}
	if got, want := namedSSE("error", bad),
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"serialization failed\"}}\n\n"; got != want {
		t.Fatalf("namedSSE fallback=%q, want %q", got, want)
	}
}

func TestSSEWriterNamedSerializationAndFallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload any
		want    string
	}{
		{
			name:    "valid",
			payload: map[string]any{"text": "<b>你好</b> & ok"},
			want:    "event: message\ndata: {\"text\":\"<b>你好</b> & ok\"}\n\n",
		},
		{
			name:    "serialization failure",
			payload: map[string]any{"unsupported": func() {}},
			want:    "event: message\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"serialization failed\"}}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if ok := newSSEWriter(recorder, "text/event-stream").writeNamed("message", test.payload); !ok {
				t.Fatal("writeNamed returned false")
			}
			if got := recorder.Body.String(); got != test.want {
				t.Fatalf("writeNamed=%q, want %q", got, test.want)
			}
		})
	}
}

func TestSSEWriterDataSerializationAndFallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload any
		want    string
	}{
		{
			name:    "valid",
			payload: map[string]any{"text": "<b>你好</b> & ok"},
			want:    "data: {\"text\":\"<b>你好</b> & ok\"}\n\n",
		},
		{
			name:    "serialization failure",
			payload: map[string]any{"unsupported": func() {}},
			want:    "data: {}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if ok := newSSEWriter(recorder, "text/event-stream").writeData(test.payload); !ok {
				t.Fatal("writeData returned false")
			}
			if got := recorder.Body.String(); got != test.want {
				t.Fatalf("writeData=%q, want %q", got, test.want)
			}
		})
	}
}

func TestTypedProtocolDeltasPreserveFieldsAndHTML(t *testing.T) {
	const text = "<b>你好</b> & ok"

	anthropicRecorder := httptest.NewRecorder()
	anthropic := anthropicStreamState{
		sw:       newSSEWriter(anthropicRecorder, "text/event-stream"),
		openType: "text",
	}
	anthropic.consume(protocolOutput{Text: text})
	anthropicStream := anthropicRecorder.Body.String()
	for _, want := range []string{
		"event: content_block_delta",
		`"type":"content_block_delta"`,
		`"type":"text_delta"`,
		`"text":"<b>你好</b> & ok"`,
	} {
		if !strings.Contains(anthropicStream, want) {
			t.Fatalf("Anthropic typed delta missing %q: %s", want, anthropicStream)
		}
	}

	responsesRecorder := httptest.NewRecorder()
	responses := responsesStreamState{
		sw:       newSSEWriter(responsesRecorder, "text/event-stream"),
		textID:   "msg_test",
		textOpen: true,
	}
	responses.consume(protocolOutput{Text: text})
	responsesStream := responsesRecorder.Body.String()
	for _, want := range []string{
		"event: response.output_text.delta",
		`"type":"response.output_text.delta"`,
		`"sequence_number":1`,
		`"delta":"<b>你好</b> & ok"`,
		`"logprobs":[]`,
	} {
		if !strings.Contains(responsesStream, want) {
			t.Fatalf("Responses typed delta missing %q: %s", want, responsesStream)
		}
	}
}
