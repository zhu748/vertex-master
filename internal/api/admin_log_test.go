package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

type adminLogTestConfig struct {
	config.ConfigProvider
	configDir string
}

func (c adminLogTestConfig) ConfigDir() string { return c.configDir }

func newAdminLogTestHandler(t testing.TB, content string) (*AdminHandler, string) {
	t.Helper()
	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "logs_latest.log")
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	provider := adminLogTestConfig{
		ConfigProvider: config.StaticProvider(cfg),
		configDir:      filepath.Join(root, "config"),
	}
	return &AdminHandler{handler: handler{cfg: provider}}, logPath //nolint:exhaustruct
}

func TestAdminGetLogReturnsContent(t *testing.T) {
	adm, _ := newAdminLogTestHandler(t, "first\nsecond\n")
	recorder := httptest.NewRecorder()
	adm.adminGetLog(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/log", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusOK)
	}
	var body struct {
		OK      bool   `json:"ok"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Content != "first\nsecond" {
		t.Fatalf("unexpected response: %+v", body)
	}
	if got := recorder.Header().Get("X-Vertex-Auto-Refresh-Logs"); got != "true" {
		t.Fatalf("auto-refresh header=%q, want true", got)
	}
}

func TestAdminGetLogReturnsNotModifiedUntilFileChanges(t *testing.T) {
	adm, logPath := newAdminLogTestHandler(t, "first\n")
	first := httptest.NewRecorder()
	adm.adminGetLog(first, httptest.NewRequest(http.MethodGet, "/api/admin/log", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("initial response is missing ETag")
	}

	unchangedRequest := httptest.NewRequest(http.MethodGet, "/api/admin/log", nil)
	unchangedRequest.Header.Set("If-None-Match", etag)
	unchanged := httptest.NewRecorder()
	adm.adminGetLog(unchanged, unchangedRequest)
	if unchanged.Code != http.StatusNotModified || unchanged.Body.Len() != 0 {
		t.Fatalf("unchanged response: status=%d body=%q", unchanged.Code, unchanged.Body.String())
	}

	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("second\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	changedRequest := httptest.NewRequest(http.MethodGet, "/api/admin/log", nil)
	changedRequest.Header.Set("If-None-Match", etag)
	changed := httptest.NewRecorder()
	adm.adminGetLog(changed, changedRequest)
	if changed.Code != http.StatusOK || !strings.Contains(changed.Body.String(), "second") {
		t.Fatalf("changed response: status=%d body=%q", changed.Code, changed.Body.String())
	}
	if changed.Header().Get("ETag") == etag {
		t.Fatal("ETag did not change after appending to the log")
	}
}

func TestReadAdminLogTailBoundsLargeFileAndKeepsLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs_latest.log")
	var content strings.Builder
	content.Grow(maxAdminLogTailBytes + 32*1024)
	content.WriteString(strings.Repeat("discarded-old-line\n", maxAdminLogTailBytes/19+100))
	for index := range 250 {
		_, _ = fmt.Fprintf(&content, "tail-%03d\n", index)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readAdminLogTail(path, maxAdminLogTailBytes, maxAdminLogTailLines)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != maxAdminLogTailLines {
		t.Fatalf("got %d lines, want %d", len(lines), maxAdminLogTailLines)
	}
	if lines[0] != "tail-050" || lines[len(lines)-1] != "tail-249" {
		t.Fatalf("unexpected bounded tail: first=%q last=%q", lines[0], lines[len(lines)-1])
	}
	if len(got) > maxAdminLogTailBytes {
		t.Fatalf("tail response is %d bytes, limit is %d", len(got), maxAdminLogTailBytes)
	}
}

func TestReadAdminLogTailKeepsBoundedSuffixOfOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs_latest.log")
	content := strings.Repeat("x", maxAdminLogTailBytes+128)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readAdminLogTail(path, maxAdminLogTailBytes, maxAdminLogTailLines)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxAdminLogTailBytes || got != content[len(content)-maxAdminLogTailBytes:] {
		t.Fatalf("oversized single-line tail length=%d", len(got))
	}
}

func TestReadAdminLogTailOversizedFinalLineKeepsPreviousCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs_latest.log")
	longFinal := strings.Repeat("尾", adminLogReadBlockSize)
	content := "discard\nkeep\n" + longFinal
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readAdminLogTail(path, int64(len(content)), 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := "keep\n" + longFinal; got != want {
		t.Fatalf("oversized final-line tail length=%d, want %d", len(got), len(want))
	}
}

func TestReadAdminLogTailOversizedBlankLineIsFiltered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs_latest.log")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxAdminLogTailBytes+128)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readAdminLogTail(path, maxAdminLogTailBytes, maxAdminLogTailLines)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("oversized blank log line was not filtered: len=%d", len(got))
	}
}

func TestReadAdminLogTailFiltersBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs_latest.log")
	if err := os.WriteFile(path, []byte("first\n \nsecond\n\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readAdminLogTail(path, maxAdminLogTailBytes, maxAdminLogTailLines)
	if err != nil {
		t.Fatal(err)
	}
	if got != "first\nsecond\nthird" {
		t.Fatalf("filtered log tail=%q", got)
	}
}

func TestReadAdminLogTailJoinsLineAcrossReadBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs_latest.log")
	longLine := strings.Repeat("跨块", 16*1024)
	content := "old\n" + longLine + "\nlast"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readAdminLogTail(path, int64(len(content)), 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := longLine + "\nlast"; got != want {
		t.Fatalf("cross-block tail length=%d, want %d", len(got), len(want))
	}
}

func TestReadAdminLogTailWindowBoundary(t *testing.T) {
	for _, test := range []struct {
		name     string
		content  string
		maxBytes int64
		want     string
	}{
		{
			name:     "drops partial first line",
			content:  "0123456789\none\ntwo\n",
			maxBytes: int64(len("3456789\none\ntwo\n")),
			want:     "one\ntwo",
		},
		{
			name:     "keeps complete first line",
			content:  "skip\none\ntwo",
			maxBytes: int64(len("one\ntwo")),
			want:     "one\ntwo",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logs_latest.log")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readAdminLogTail(path, test.maxBytes, maxAdminLogTailLines)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("tail=%q, want %q", got, test.want)
			}
		})
	}
}

func BenchmarkReadAdminLogTail(b *testing.B) {
	tail := []byte(strings.Repeat("benchmark log line\n", maxAdminLogTailBytes/19+1))
	for _, size := range []int64{2 << 20, 64 << 20} {
		path := filepath.Join(b.TempDir(), fmt.Sprintf("log-%d", size))
		file, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := file.Truncate(size); err != nil {
			_ = file.Close()
			b.Fatal(err)
		}
		if _, err := file.WriteAt(tail, size-int64(len(tail))); err != nil {
			_ = file.Close()
			b.Fatal(err)
		}
		if err := file.Close(); err != nil {
			b.Fatal(err)
		}

		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := readAdminLogTail(path, maxAdminLogTailBytes, maxAdminLogTailLines); err != nil {
					b.Fatal(err)
				}
			}
		})
	}

	b.Run("oversized-single-line", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "oversized-line.log")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", maxAdminLogTailBytes+128)), 0o600); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for range b.N {
			if _, err := readAdminLogTail(path, maxAdminLogTailBytes, maxAdminLogTailLines); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkAdminGetLogUnchanged(b *testing.B) {
	adm, _ := newAdminLogTestHandler(b, strings.Repeat("benchmark log line\n", maxAdminLogTailBytes/19+1))
	initial := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/log", nil)
	adm.adminGetLog(initial, request)
	request.Header.Set("If-None-Match", initial.Header().Get("ETag"))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		recorder := httptest.NewRecorder()
		adm.adminGetLog(recorder, request)
		if recorder.Code != http.StatusNotModified {
			b.Fatalf("status=%d", recorder.Code)
		}
	}
}
