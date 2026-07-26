package spool

import (
	"bytes"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
)

// TestEncodeJSONMatchesJsonx 验证 EncodeJSON 与 jsonx.Marshal 逐字节一致
// （关 HTML 转义 + 去尾换行），保证发往上游的请求体不变。
func TestEncodeJSONMatchesJsonx(t *testing.T) {
	cases := []any{
		map[string]any{"a": float64(1), "b": "x<y>&z"}, // 含 < > & 验证不转义
		map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "你好"}}}}},
		"plain string",
		[]any{float64(1), float64(2), float64(3)},
	}
	for i, v := range cases {
		buf, err := EncodeJSON(v)
		if err != nil {
			t.Fatalf("case %d EncodeJSON: %v", i, err)
		}
		r, _ := buf.Reader()
		got, _ := io.ReadAll(r)
		want, _ := jsonx.Marshal(v)
		if string(got) != string(want) {
			t.Fatalf("case %d 不一致:\n got=%q\nwant=%q", i, got, want)
		}
		_ = buf.Close()
	}
}

// TestBufferMemOnly 验证内存缓冲：写入、读回完整、Len 正确、不落盘。
func TestBufferMemOnly(t *testing.T) {
	if SpilledBytes() != 0 {
		t.Fatal("SpilledBytes 应为 0")
	}
	SetMaxSpillBytes(123) // 不溢出磁盘，调用不应改变行为

	b := New()
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 15 {
		t.Fatalf("Len 应为 15，got %d", b.Len())
	}
	r, err := b.Reader()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "hello0123456789" {
		t.Fatalf("读回内容错: %q", got)
	}
	if SpilledBytes() != 0 {
		t.Fatal("写入后 SpilledBytes 仍应为 0（不落盘）")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestBufferSpillRoundTrip 验证超过阈值后落盘：内容完整读回、Len 计入全部字节、
// SpilledBytes 累加，且 Close 后临时文件被删除。
//
// 注意：本用例会把 SpilledBytes 计数器抬高，必须排在 TestBufferMemOnly 之后
// （同文件内 go test 按源码顺序执行）。
func TestBufferSpillRoundTrip(t *testing.T) {
	SetMaxSpillBytes(8)
	defer SetMaxSpillBytes(0)

	before := SpilledBytes()
	b := New()
	head := []byte("head")                // 4 字节，先留在内存
	tail := bytes.Repeat([]byte("x"), 32) // 触发溢出
	if _, err := b.Write(head); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(tail); err != nil {
		t.Fatal(err)
	}

	want := append(append([]byte(nil), head...), tail...)
	if b.Len() != int64(len(want)) {
		t.Fatalf("Len 应为 %d，got %d", len(want), b.Len())
	}
	r, err := b.Reader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("落盘后读回内容不一致:\n got=%q\nwant=%q", got, want)
	}
	if delta := SpilledBytes() - before; delta != int64(len(want)) {
		t.Fatalf("SpilledBytes 增量应为 %d，got %d", len(want), delta)
	}

	path := b.filePath
	if path == "" {
		t.Fatal("溢出后 filePath 不应为空")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Close 后临时文件应被删除，stat err=%v", err)
	}
}

// TestConcurrentSpillCounter 覆盖多请求并发落盘与 /metrics 并发读取计数器的场景。
// 该场景曾触发 spilledBytes 的数据竞争，需配合 go test -race 防回归。
func TestConcurrentSpillCounter(t *testing.T) {
	SetMaxSpillBytes(16)
	defer SetMaxSpillBytes(0)

	const (
		writers   = 8
		blobSize  = 64
		readLoops = 200
	)

	before := SpilledBytes()
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := New()
			defer func() { _ = b.Close() }()
			if _, err := b.Write(bytes.Repeat([]byte("y"), blobSize)); err != nil {
				t.Errorf("并发写入失败: %v", err)
			}
		}()
	}
	// 模拟 /metrics 在写入过程中读取计数器。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range readLoops {
			_ = SpilledBytes()
		}
	}()
	wg.Wait()

	if delta := SpilledBytes() - before; delta != writers*blobSize {
		t.Fatalf("并发落盘计数应为 %d，got %d", writers*blobSize, delta)
	}
}
