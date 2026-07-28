package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type readTrackingCloser struct {
	reads  int
	closes int
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
