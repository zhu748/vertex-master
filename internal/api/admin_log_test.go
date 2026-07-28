package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
