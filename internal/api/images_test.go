package api

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func multipartImageHeader(t testing.TB, payload []byte) *multipart.FileHeader {
	t.Helper()
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("image", "benchmark.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(form.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if err = request.ParseMultipartForm(int64(len(payload)) + 1024); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = request.MultipartForm.RemoveAll() })
	return request.MultipartForm.File["image"][0]
}

func TestUploadToInlineImageHandlesKnownAndOverflowSizes(t *testing.T) {
	payload := []byte("image payload 中文")
	for _, test := range []struct {
		name string
		size int64
	}{
		{name: "known size", size: int64(len(payload))},
		{name: "overflow size skips preallocation", size: int64(^uint64(0) >> 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := multipartImageHeader(t, payload)
			header.Size = test.size
			header.Header.Del("Content-Type")
			image, err := uploadToInlineImage(header)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(image.Data)
			if err != nil || !bytes.Equal(decoded, payload) || image.MimeType != "image/png" {
				t.Fatalf("image=%#v decoded=%q err=%v", image, decoded, err)
			}
		})
	}
}

func TestUploadToInlineImageRejectsEmptyFile(t *testing.T) {
	header := multipartImageHeader(t, nil)
	if _, err := uploadToInlineImage(header); err == nil {
		t.Fatal("empty upload was accepted")
	}
}

func TestFormUploadsUsesStableFieldOrder(t *testing.T) {
	header := func(name string) *multipart.FileHeader { return &multipart.FileHeader{Filename: name} }
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.MultipartForm = &multipart.Form{File: map[string][]*multipart.FileHeader{
		"image[10]": {header("index-10")},
		"image[]":   {header("array")},
		"image[2]":  {header("index-2")},
		"image":     {header("direct")},
		"mask":      {header("ignored")},
	}}
	uploads := formUploads(request, "image")
	want := []string{"direct", "array", "index-2", "index-10"}
	if len(uploads) != len(want) {
		t.Fatalf("uploads=%#v", uploads)
	}
	for index, expected := range want {
		if uploads[index].Filename != expected {
			t.Fatalf("upload %d=%q, want %q", index, uploads[index].Filename, expected)
		}
	}
}

func TestFormUploadsSingleFieldDoesNotAllocate(t *testing.T) {
	headers := []*multipart.FileHeader{{Filename: "single.png"}}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.MultipartForm = &multipart.Form{File: map[string][]*multipart.FileHeader{"image": headers}}
	if allocations := testing.AllocsPerRun(100, func() {
		uploads := formUploads(request, "image")
		if len(uploads) != 1 || uploads[0] != headers[0] {
			t.Fatal("single upload changed")
		}
	}); allocations != 0 {
		t.Fatalf("single-field formUploads allocated %.1f times", allocations)
	}
}

func BenchmarkUploadToInlineImageOneMiB(b *testing.B) {
	payload := bytes.Repeat([]byte{0x5a}, 1<<20)
	header := multipartImageHeader(b, payload)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		image, err := uploadToInlineImage(header)
		if err != nil || len(image.Data) == 0 {
			b.Fatal("image encoding failed", err)
		}
	}
}

func BenchmarkImageResponseJSONOneMiB(b *testing.B) {
	encoded := strings.Repeat("A", 1<<20)
	legacyBody := map[string]any{
		"created": int64(123),
		"data":    []any{map[string]any{"b64_json": encoded}},
	}
	typedItems := []imageResponseItem{{B64JSON: encoded}}
	b.Run("direct_typed", func(b *testing.B) {
		writer := &audioBenchmarkWriter{header: make(http.Header)}
		b.ReportAllocs()
		b.SetBytes(int64(len(encoded)))
		for range b.N {
			writeImageResponse(writer, 123, typedItems)
		}
	})
	b.Run("generic_buffered", func(b *testing.B) {
		writer := &audioBenchmarkWriter{header: make(http.Header)}
		b.ReportAllocs()
		b.SetBytes(int64(len(encoded)))
		for range b.N {
			writeJSON(writer, http.StatusOK, legacyBody)
		}
	})
}

func TestTypedImageResponseMatchesGenericJSON(t *testing.T) {
	created := int64(123)
	typedItems := []imageResponseItem{
		{B64JSON: "QUJD"},
		{URL: "data:image/svg+xml;base64,<>&中文"},
	}
	typedRecorder := httptest.NewRecorder()
	writeImageResponse(typedRecorder, created, typedItems)

	genericRecorder := httptest.NewRecorder()
	writeJSON(genericRecorder, http.StatusOK, map[string]any{
		"created": created,
		"data": []any{
			map[string]any{"b64_json": "QUJD"},
			map[string]any{"url": "data:image/svg+xml;base64,<>&中文"},
		},
	})
	if typedRecorder.Code != genericRecorder.Code ||
		typedRecorder.Header().Get("Content-Type") != genericRecorder.Header().Get("Content-Type") ||
		!bytes.Equal(typedRecorder.Body.Bytes(), genericRecorder.Body.Bytes()) {
		t.Fatalf("typed=%q generic=%q", typedRecorder.Body.Bytes(), genericRecorder.Body.Bytes())
	}
}

// ---- coerceOAIN：clamp [1,8]，非法 → 1 ----

func TestCoerceOAIN(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"valid mid", "3", 3},
		{"min", "1", 1},
		{"max", "8", 8},
		{"below clamps to 1", "0", 1},
		{"negative clamps to 1", "-5", 1},
		{"above clamps to 8", "9", 8},
		{"far above clamps to 8", "1000", 8},
		{"empty to 1", "", 1},
		{"non-numeric to 1", "abc", 1},
		{"whitespace trimmed", "  4  ", 4},
		{"float string to 1 (atoi fails)", "2.5", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coerceOAIN(c.in); got != c.want {
				t.Errorf("coerceOAIN(%q)=%d，期望 %d", c.in, got, c.want)
			}
		})
	}
}

// ---- getStr：缺失/非字符串 → default，存在字符串（含空串）→ 原样 ----

func TestGetStr(t *testing.T) {
	body := map[string]any{
		"present":   "value",
		"empty":     "",
		"number":    42,
		"boolean":   true,
		"nilval":    nil,
		"nestedmap": map[string]any{"x": 1},
	}
	cases := []struct {
		name string
		key  string
		def  string
		want string
	}{
		{"present string returned", "present", "DEF", "value"},
		{"empty string returned as-is", "empty", "DEF", ""},
		{"missing returns default", "missing", "DEF", "DEF"},
		{"non-string number returns default", "number", "DEF", "DEF"},
		{"non-string bool returns default", "boolean", "DEF", "DEF"},
		{"nil value returns default", "nilval", "DEF", "DEF"},
		{"map value returns default", "nestedmap", "DEF", "DEF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := getStr(body, c.key, c.def); got != c.want {
				t.Errorf("getStr(%q, %q)=%q，期望 %q", c.key, c.def, got, c.want)
			}
		})
	}
}

// ---- firstNonEmptyStr ----

func TestFirstNonEmptyStr(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want string
	}{
		{"a non-empty wins", "first", "second", "first"},
		{"a empty falls to b", "", "second", "second"},
		{"a whitespace falls to b", "   ", "second", "second"},
		{"both empty returns b", "", "", ""},
		{"a wins even if b empty", "first", "", "first"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstNonEmptyStr(c.a, c.b); got != c.want {
				t.Errorf("firstNonEmptyStr(%q,%q)=%q，期望 %q", c.a, c.b, got, c.want)
			}
		})
	}
}

// ---- hasImageSize ----

func TestHasImageSize(t *testing.T) {
	cases := []struct { //nolint:govet
		name    string
		payload map[string]any
		want    bool
	}{
		{
			name: "set non-empty",
			payload: map[string]any{
				"generationConfig": map[string]any{
					"imageConfig": map[string]any{"imageSize": "1K"},
				},
			},
			want: true,
		},
		{
			name: "imageSize empty string",
			payload: map[string]any{
				"generationConfig": map[string]any{
					"imageConfig": map[string]any{"imageSize": ""},
				},
			},
			want: false,
		},
		{
			name: "imageSize missing key",
			payload: map[string]any{
				"generationConfig": map[string]any{
					"imageConfig": map[string]any{},
				},
			},
			want: false,
		},
		{
			name: "no imageConfig",
			payload: map[string]any{
				"generationConfig": map[string]any{},
			},
			want: false,
		},
		{
			name:    "no generationConfig",
			payload: map[string]any{},
			want:    false,
		},
		{
			name: "imageSize non-string value",
			payload: map[string]any{
				"generationConfig": map[string]any{
					"imageConfig": map[string]any{"imageSize": 1024},
				},
			},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasImageSize(c.payload); got != c.want {
				t.Errorf("hasImageSize=%v，期望 %v", got, c.want)
			}
		})
	}
}
