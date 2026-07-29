package jsonx

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var benchmarkMarshalBytes []byte   //nolint:gochecknoglobals
var benchmarkMarshalString string  //nolint:gochecknoglobals
var benchmarkMarshalViewLength int //nolint:gochecknoglobals

type escapedHTMLMarshaler struct{}

func (escapedHTMLMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(`"\u003c"`), nil
}

func BenchmarkMarshal(b *testing.B) {
	for _, benchmark := range []struct {
		name  string
		value any
	}{
		{name: "tool_arguments", value: map[string]any{
			"query": "weather", "limit": 10, "nested": map[string]any{"enabled": true},
		}},
		{name: "html", value: map[string]any{
			"text": strings.Repeat("<tag>&value</tag>", 8),
		}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				encoded, err := Marshal(benchmark.value)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkMarshalBytes = encoded
			}
		})
	}
}

func BenchmarkMarshalString(b *testing.B) {
	value := map[string]any{
		"query": "weather", "limit": 10, "nested": map[string]any{"enabled": true},
	}
	for _, benchmark := range []struct {
		name string
		run  func() (string, error)
	}{
		{name: "direct", run: func() (string, error) { return MarshalString(value) }},
		{name: "via_bytes", run: func() (string, error) {
			encoded, err := Marshal(value)
			return string(encoded), err
		}},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				encoded, err := benchmark.run()
				if err != nil {
					b.Fatal(err)
				}
				benchmarkMarshalString = encoded
			}
		})
	}
}

func BenchmarkMarshalView(b *testing.B) {
	value := map[string]any{
		"text": strings.Repeat("<tag>你好&value</tag>", 1024),
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(value["text"].(string))))
	for range b.N {
		if err := MarshalView(value, func(encoded []byte) {
			benchmarkMarshalViewLength = len(encoded)
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMarshal(t *testing.T) {
	tests := []struct {
		name    string
		v       any
		want    []byte
		wantErr bool
	}{
		{ //nolint:exhaustruct
			name: "no html escape",
			v:    map[string]string{"html": "<script>alert(1)</script> & foo"},
			want: []byte(`{"html":"<script>alert(1)</script> & foo"}`),
		},
		{ //nolint:exhaustruct
			name: "unicode preserved",
			v:    map[string]string{"text": "你好世界"},
			want: []byte(`{"text":"你好世界"}`),
		},
		{ //nolint:exhaustruct
			name: "simple string",
			v:    "test",
			want: []byte(`"test"`),
		},
		{
			name: "nil",
			v:    nil,
			want: []byte(`null`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Marshal(tt.v)
			if (err != nil) != tt.wantErr {
				t.Errorf("Marshal() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("Marshal() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestEncodeWritesUnescapedJSONWithTrailingNewline(t *testing.T) {
	var output bytes.Buffer
	if err := Encode(&output, map[string]string{"html": "<b>你好</b> & ok"}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "{\"html\":\"<b>你好</b> & ok\"}\n"; got != want {
		t.Fatalf("Encode()=%q, want %q", got, want)
	}
}

func TestEncodeNoTrailingNewlineMatchesMarshal(t *testing.T) {
	value := map[string]any{
		"html":   "<b>你好</b> & ok",
		"nested": []any{true, nil, map[string]any{"value": "😀"}},
	}
	want, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := EncodeNoTrailingNewline(&output, value); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("EncodeNoTrailingNewline()=%q, Marshal()=%q", output.Bytes(), want)
	}
}

func TestTrailingNewlineWriterHandlesMultipleWrites(t *testing.T) {
	var output bytes.Buffer
	writer := trailingNewlineWriter{writer: &output}
	for _, chunk := range [][]byte{[]byte("first"), []byte(" second"), []byte("\n")} {
		if written, err := writer.Write(chunk); err != nil || written != len(chunk) {
			t.Fatalf("write %q: written=%d err=%v", chunk, written, err)
		}
	}
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "first second"; got != want {
		t.Fatalf("trimmed output=%q, want %q", got, want)
	}
}

func TestEncodeNoTrailingNewlineDoesNotWriteOnMarshalError(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	var output bytes.Buffer
	if err := EncodeNoTrailingNewline(&output, cyclic); err == nil {
		t.Fatal("cyclic value error = nil")
	}
	if output.Len() != 0 {
		t.Fatalf("partial output after marshal error: %q", output.Bytes())
	}
}

func TestMarshalMatchesUnescapedEncoder(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "nested standard JSON", value: map[string]any{
			"query": "weather", "limit": float64(10),
			"nested": []any{true, nil, map[string]any{"city": "上海"}},
		}},
		{name: "HTML in value", value: map[string]any{"text": "<tag>&value</tag>"}},
		{name: "HTML in key", value: map[string]any{"<key>": "value"}},
		{name: "literal escaped text", value: map[string]any{"text": `\u003c`}},
		{name: "raw message", value: json.RawMessage(`"\u003c"`)},
		{name: "custom marshaler", value: escapedHTMLMarshaler{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var reference bytes.Buffer
			if err := Encode(&reference, test.value); err != nil {
				t.Fatal(err)
			}
			want := bytes.TrimSuffix(reference.Bytes(), []byte{'\n'})
			got, err := Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("Marshal()=%q, Encode() without newline=%q", got, want)
			}
			gotString, err := MarshalString(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if gotString != string(want) {
				t.Fatalf("MarshalString()=%q, Encode() without newline=%q", gotString, want)
			}
		})
	}
}

func TestMarshalCyclicValueReturnsError(t *testing.T) {
	value := map[string]any{}
	value["self"] = value
	if _, err := Marshal(value); err == nil {
		t.Fatal("Marshal() cyclic value error = nil")
	}
}

func TestMarshalViewMatchesMarshalAndSkipsConsumerOnError(t *testing.T) {
	value := map[string]any{"html": "<b>你好</b> & ok"}
	want, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := MarshalView(value, func(view []byte) {
		got = append(got, view...)
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalView()=%q, Marshal()=%q", got, want)
	}

	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	called := false
	if err := MarshalView(cyclic, func([]byte) { called = true }); err == nil {
		t.Fatal("MarshalView() cyclic value error = nil")
	}
	if called {
		t.Fatal("MarshalView consumer ran after serialization failure")
	}
}

func TestMarshalResultDoesNotAliasPooledBuffer(t *testing.T) {
	first, err := Marshal(map[string]any{"value": "first"})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := Marshal(map[string]any{"value": "replacement"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := string(first), `{"value":"first"}`; got != want {
		t.Fatalf("earlier Marshal result changed after pool reuse: got %q, want %q", got, want)
	}
}

func TestTruthy(t *testing.T) {
	tests := []struct { //nolint:govet
		name string
		v    any
		want bool
	}{
		{"nil", nil, false},
		{"bool true", true, true},
		{"bool false", false, false},
		{"string empty", "", false},
		{"string non-empty", "hello", true},
		{"float64 zero", 0.0, false},
		{"float64 non-zero", 1.5, true},
		{"float64 negative", -1.5, true},
		{"int zero (default true for unhandled)", 0, true}, // Truthy function doesn't specifically handle int
		{"slice empty", []any{}, false},
		{"slice non-empty", []any{1}, true},
		{"map empty", map[string]any{}, false},
		{"map non-empty", map[string]any{"a": 1}, true},
		{"custom struct", struct{}{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Truthy(tt.v); got != tt.want {
				t.Errorf("Truthy(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}
