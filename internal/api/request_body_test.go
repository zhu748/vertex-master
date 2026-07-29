package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func TestPublicJSONHandlersRejectTrailingValues(t *testing.T) {
	tests := []struct {
		name string
		run  func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "chat completions",
			run: func(w http.ResponseWriter, r *http.Request) {
				(&ChatHandler{}).handleChatCompletions(w, r)
			},
		},
		{
			name: "Gemini",
			run: func(w http.ResponseWriter, r *http.Request) {
				(&GeminiHandler{}).readGeminiBody(w, r)
			},
		},
		{
			name: "audio speech",
			run: func(w http.ResponseWriter, r *http.Request) {
				(&AudioHandler{}).handleAudioSpeech(w, r)
			},
		},
		{
			name: "image JSON",
			run: func(w http.ResponseWriter, r *http.Request) {
				(&ImageHandler{}).readJSONObject(w, r)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodPost,
				"/",
				strings.NewReader(`{"model":"gemini-test"} {"unexpected":true}`),
			)
			test.run(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDecodeJSONObjectAcceptsTrailingWhitespace(t *testing.T) {
	body, err := decodeJSONObject(strings.NewReader(" {\"model\":\"gemini-test\"} \r\n\t"))
	if err != nil {
		t.Fatal(err)
	}
	if body["model"] != "gemini-test" {
		t.Fatalf("decoded body=%#v", body)
	}
}

func TestDecodeJSONObjectRejectsInvalidUTF8(t *testing.T) {
	tests := []struct {
		name  string
		value []byte
	}{
		{name: "invalid leading byte", value: []byte{0xff}},
		{name: "unexpected continuation", value: []byte{0x80}},
		{name: "overlong two byte", value: []byte{0xc0, 0x80}},
		{name: "overlong three byte", value: []byte{0xe0, 0x80, 0x80}},
		{name: "surrogate", value: []byte{0xed, 0xa0, 0x80}},
		{name: "above unicode maximum", value: []byte{0xf4, 0x90, 0x80, 0x80}},
		{name: "truncated sequence", value: []byte{0xe4, 0xb8}},
	}
	for _, test := range tests {
		raw := append([]byte(`{"text":"`), test.value...)
		raw = append(raw, []byte(`"}`)...)
		for _, reader := range []struct {
			name string
			body io.Reader
		}{
			{name: "single read", body: bytes.NewReader(raw)},
			{name: "one byte reads", body: iotest.OneByteReader(bytes.NewReader(raw))},
		} {
			t.Run(test.name+"/"+reader.name, func(t *testing.T) {
				if body, err := decodeJSONObject(reader.body); err == nil {
					t.Fatalf("invalid UTF-8 decoded as %#v", body)
				}
			})
		}
	}
}

func TestDecodeJSONObjectAcceptsSplitUTF8Sequences(t *testing.T) {
	raw := strings.NewReader(`{"text":"中文😀"}`)
	body, err := decodeJSONObject(iotest.OneByteReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if body["text"] != "中文😀" {
		t.Fatalf("decoded body=%#v", body)
	}
}

func TestChatAndGeminiReportInvalidUTF8(t *testing.T) {
	tests := []struct {
		name string
		run  func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "chat completions",
			run: func(w http.ResponseWriter, r *http.Request) {
				(&ChatHandler{}).handleChatCompletions(w, r)
			},
		},
		{
			name: "Gemini",
			run: func(w http.ResponseWriter, r *http.Request) {
				(&GeminiHandler{}).readGeminiBody(w, r)
			},
		},
	}
	raw := []byte{'{', '"', 't', 'e', 'x', 't', '"', ':', '"', 0xff, '"', '}'}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
			test.run(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "UTF-8") {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDecodeAdminBodyUsesStrictTypedDecoder(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		wantMessage string
	}{
		{
			name:        "trailing value",
			body:        []byte(`{"name":"first"} {"name":"second"}`),
			wantMessage: "只能包含一个 JSON 值",
		},
		{
			name:        "invalid UTF-8",
			body:        []byte{'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', '}'},
			wantMessage: "invalid JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/admin/test", bytes.NewReader(test.body))
			var target struct {
				Name string `json:"name"`
			}
			if (&handler{}).decodeAdminBody(recorder, request, &target) { //nolint:exhaustruct
				t.Fatal("strict admin decoder accepted invalid body")
			}
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.wantMessage) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/test",
		io.NopCloser(iotest.OneByteReader(strings.NewReader(`{"name":"中文😀"}`))),
	)
	recorder := httptest.NewRecorder()
	var target struct {
		Name string `json:"name"`
	}
	if !(&handler{}).decodeAdminBody(recorder, request, &target) { //nolint:exhaustruct
		t.Fatalf("valid split UTF-8 rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if target.Name != "中文😀" {
		t.Fatalf("decoded target=%#v", target)
	}
}

func BenchmarkDecodeJSONObjectUTF8Validation(b *testing.B) {
	payload := []byte(`{"text":"` + strings.Repeat("中", 1<<15) + `"}`)
	b.Run("validated", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(payload)))
		for range b.N {
			body, err := decodeJSONObject(bytes.NewReader(payload))
			if err != nil || len(body["text"].(string)) == 0 {
				b.Fatal("validated decode failed", err)
			}
		}
	})
	b.Run("standard_library", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(payload)))
		for range b.N {
			var body map[string]any
			decoder := json.NewDecoder(bytes.NewReader(payload))
			if err := decoder.Decode(&body); err != nil {
				b.Fatal(err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF || len(body["text"].(string)) == 0 {
				b.Fatal("standard decode failed", err)
			}
		}
	})
}
