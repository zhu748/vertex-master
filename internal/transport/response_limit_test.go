package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var benchmarkReadAllLimitResult []byte //nolint:gochecknoglobals

func BenchmarkReadAllLimit(b *testing.B) {
	payload := bytes.Repeat([]byte("response-body-"), 1<<14)
	for _, benchmark := range []struct {
		name     string
		sizeHint int64
	}{
		{name: "unknown_length", sizeHint: -1},
		{name: "known_length", sizeHint: int64(len(payload))},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for range b.N {
				data, err := readAllLimitWithHint(
					bytes.NewReader(payload), 4<<20, benchmark.sizeHint,
				)
				if err != nil || len(data) != len(payload) {
					b.Fatalf("len=%d err=%v", len(data), err)
				}
				benchmarkReadAllLimitResult = data
			}
		})
	}
}

type readTrackingCloser struct {
	reads  int
	closes int
}

type dataThenErrorReader struct {
	data []byte
	err  error
}

func (r *dataThenErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *readTrackingCloser) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func (r *readTrackingCloser) Close() error {
	r.closes++
	return nil
}

func TestReadAllLimit(t *testing.T) {
	data, err := ReadAllLimit(strings.NewReader("12345"), 5)
	if err != nil || string(data) != "12345" {
		t.Fatalf("exact limit: data=%q err=%v", data, err)
	}

	data, err = ReadAllLimit(strings.NewReader("123456789"), 5)
	if !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("oversized error=%v", err)
	}
	if string(data) != "12345" {
		t.Fatalf("oversized prefix=%q", data)
	}

	data, err = ReadAllLimit(strings.NewReader("unlimited"), 0)
	if err != nil || string(data) != "unlimited" {
		t.Fatalf("unlimited read: data=%q err=%v", data, err)
	}
}

func TestReadAllLimitWithHintHandlesExactWrongAndOversizedLengths(t *testing.T) {
	for _, hint := range []int64{2, 9, 100} {
		data, err := readAllLimitWithHint(strings.NewReader("123456789"), 16, hint)
		if err != nil || string(data) != "123456789" {
			t.Fatalf("hint=%d data=%q err=%v", hint, data, err)
		}
	}
	data, err := readAllLimitWithHint(strings.NewReader("123456789"), 5, 9)
	if !errors.Is(err, ErrResponseBodyTooLarge) || string(data) != "12345" {
		t.Fatalf("oversized hinted read: data=%q err=%v", data, err)
	}
	sentinel := errors.New("terminal read error")
	data, err = readAllLimitWithHint(&dataThenErrorReader{data: []byte("prefix"), err: sentinel}, 0, 6)
	if !errors.Is(err, sentinel) || string(data) != "prefix" {
		t.Fatalf("unlimited hinted read error: data=%q err=%v", data, err)
	}
	data, err = ReadAllLimit(&dataThenErrorReader{data: []byte("prefix"), err: sentinel}, 16)
	if !errors.Is(err, sentinel) || data != nil {
		t.Fatalf("bounded unknown-length read error: data=%q err=%v", data, err)
	}
}

func TestReadAllLimitUnknownLengthResultDoesNotAliasPool(t *testing.T) {
	first, err := ReadAllLimit(strings.NewReader("first response"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		data, readErr := ReadAllLimit(strings.NewReader("replacement response"), 1024)
		if readErr != nil || string(data) != "replacement response" {
			t.Fatalf("replacement data=%q err=%v", data, readErr)
		}
	}
	if got, want := string(first), "first response"; got != want {
		t.Fatalf("earlier result changed after pool reuse: got %q, want %q", got, want)
	}
}

func TestSessionDoAndReadLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	}))
	defer server.Close()

	network := NewNetworkClient(false)
	session, err := network.CreateSession(5, "", "response-limit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	status, data, err := session.DoAndReadLimit(
		context.Background(), http.MethodGet, server.URL, nil, nil, 16,
	)
	if status != http.StatusOK || !errors.Is(err, ErrResponseBodyTooLarge) || len(data) != 16 {
		t.Fatalf("status=%d len=%d err=%v", status, len(data), err)
	}
}

func TestSessionDoAndReadLimitChunkedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first chunk"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte(" second chunk"))
	}))
	defer server.Close()

	network := NewNetworkClient(false)
	session, err := network.CreateSession(5, "", "chunked-response-limit-test")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	status, data, err := session.DoAndReadLimit(
		context.Background(), http.MethodGet, server.URL, nil, nil, 1024,
	)
	if status != http.StatusOK || err != nil || string(data) != "first chunk second chunk" {
		t.Fatalf("status=%d data=%q err=%v", status, data, err)
	}
}

func TestStreamResponseCloseDoesNotDrainBody(t *testing.T) {
	body := &readTrackingCloser{}
	response := &StreamResponse{StatusCode: http.StatusOK, Body: body}

	response.Close()
	response.Close()

	if body.reads != 0 {
		t.Fatalf("Close drained the streaming body with %d reads", body.reads)
	}
	if body.closes != 1 {
		t.Fatalf("Close called the body closer %d times, want 1", body.closes)
	}
}
