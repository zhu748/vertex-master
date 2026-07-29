package vertex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/recaptcha"
)

var benchmarkExtractedRecursiveText string //nolint:gochecknoglobals

func BenchmarkExtractTextRecursiveOneMiB(b *testing.B) {
	text := strings.Repeat("A", 64<<10)
	flat := make([]any, 16)
	for index := range flat {
		flat[index] = map[string]any{"text": text}
	}
	var nested any = flat
	for range 8 {
		nested = []any{nested}
	}

	for name, value := range map[string]any{
		"flat":         flat,
		"eight_levels": nested,
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(1 << 20)
			for range b.N {
				benchmarkExtractedRecursiveText = extractTextRecursive(value, 0)
				if len(benchmarkExtractedRecursiveText) != 1<<20 {
					b.Fatalf("recursive text length=%d", len(benchmarkExtractedRecursiveText))
				}
			}
		})
	}
}

func BenchmarkScanStreamFrames(b *testing.B) {
	const frameCount = 64
	var raw strings.Builder
	for index := range frameCount {
		raw.WriteString(wrap(`{"candidates":[{"content":{"parts":[{"text":"chunk-` +
			fmt.Sprintf("%02d", index) +
			`"}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED"}]}`))
	}
	payload := []byte(raw.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		count := 0
		err := scanStream(bytes.NewReader(payload), func(map[string]any) (bool, error) {
			count++
			return false, nil
		})
		if err != nil || count != frameCount {
			b.Fatalf("scan err=%v count=%d", err, count)
		}
	}
}

func BenchmarkProcessStreamFrames(b *testing.B) {
	const frameCount = 64
	var raw strings.Builder
	for index := range frameCount {
		raw.WriteString(wrap(`{"candidates":[{"content":{"parts":[{"text":"chunk-` +
			fmt.Sprintf("%02d", index) +
			`"}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED"}]}`))
	}
	payload := []byte(raw.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		emitted := 0
		err := scanStreamRaw(bytes.NewReader(payload), func(raw []byte) (bool, error) {
			return processStreamingJSON(raw, func(map[string]any) bool {
				emitted++
				return true
			})
		})
		if err != nil || emitted != frameCount {
			b.Fatalf("process err=%v emitted=%d", err, emitted)
		}
	}
}

func BenchmarkProcessDirtyTextStreamFrames(b *testing.B) {
	const frameCount = 64
	var raw strings.Builder
	for index := range frameCount {
		raw.WriteString(wrap(`{"candidates":[{"content":{"parts":[{"data":"text","text":"chunk-` +
			fmt.Sprintf("%02d", index) +
			`","thought":false,"thoughtSignature":"","fileData":{},"functionCall":{},"functionResponse":{},"inlineData":{}}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED","index":0}]}`))
	}
	payload := []byte(raw.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		emitted := 0
		err := scanStreamRaw(bytes.NewReader(payload), func(raw []byte) (bool, error) {
			return processStreamingJSON(raw, func(map[string]any) bool {
				emitted++
				return true
			})
		})
		if err != nil || emitted != frameCount {
			b.Fatalf("process err=%v emitted=%d", err, emitted)
		}
	}
}

func TestScanStreamLargeSingleReadBatchPreservesOrder(t *testing.T) {
	const frameCount = 1024
	var raw strings.Builder
	for index := range frameCount {
		fmt.Fprintf(&raw, `{"index":%d}`, index)
	}

	seen := 0
	err := scanStream(strings.NewReader(raw.String()), func(object map[string]any) (bool, error) {
		if got := int(object["index"].(float64)); got != seen {
			t.Fatalf("frame %d index=%d", seen, got)
		}
		seen++
		return false, nil
	})
	if err != nil || seen != frameCount {
		t.Fatalf("scan err=%v seen=%d", err, seen)
	}
}

func TestScanStreamReadErrorIsNotTreatedAsEOF(t *testing.T) {
	sentinel := errors.New("connection reset")
	called := false
	err := scanStreamRaw(&terminalErrorReader{err: sentinel}, func([]byte) (bool, error) {
		called = true
		return false, nil
	})
	if called {
		t.Fatal("callback must not run without a complete object")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("scan error=%v, want wrapped connection error", err)
	}
}

func TestScanStreamProcessesFinalBytesBeforeReadError(t *testing.T) {
	sentinel := errors.New("unexpected stream termination")
	seen := 0
	err := scanStreamRaw(&terminalErrorReader{
		data: []byte(`{"index":1}`),
		err:  sentinel,
	}, func(raw []byte) (bool, error) {
		seen++
		if string(raw) != `{"index":1}` {
			t.Fatalf("frame=%q", raw)
		}
		return false, nil
	})
	if seen != 1 {
		t.Fatalf("seen=%d, want 1", seen)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("scan error=%v, want wrapped terminal error", err)
	}
}

func TestScanStreamEOFRemainsSuccessful(t *testing.T) {
	seen := 0
	err := scanStreamRaw(strings.NewReader(`{"index":1}`), func([]byte) (bool, error) {
		seen++
		return false, nil
	})
	if err != nil || seen != 1 {
		t.Fatalf("EOF scan err=%v seen=%d", err, seen)
	}
}

func TestScanStreamTruncatedObjectReturnsUnexpectedEOF(t *testing.T) {
	called := false
	err := scanStreamRaw(strings.NewReader(`prefix {"index":1`), func([]byte) (bool, error) {
		called = true
		return false, nil
	})
	if called {
		t.Fatal("callback must not receive a truncated object")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("scan error=%v, want io.ErrUnexpectedEOF", err)
	}
}

func TestEffectiveMaxRetriesDefaultsTo429RetryInParallelPool(t *testing.T) {
	cfg := config.DefaultConfig()
	if got := effectiveMaxRetries(
		cfg.MaxRetries,
		cfg.ParallelPoolEnabled,
		cfg.ParallelPoolRetryEnabled,
	); got != 1 {
		t.Fatalf("默认并发池应保留 1 次节点内重试，got %d", got)
	}
	if got := effectiveMaxRetries(cfg.MaxRetries, true, false); got != 0 {
		t.Fatalf("显式关闭并发池重试时应为 0，got %d", got)
	}
	if got := effectiveMaxRetries(-1, false, true); got != 0 {
		t.Fatalf("负数重试配置应安全归零，got %d", got)
	}
}

func TestStreamingCancellationStopsTokenFetch(t *testing.T) {
	started := make(chan struct{})
	client := NewVertexAIClient(config.StaticProvider(config.DefaultConfig()))
	client.SetTokenPool(recaptcha.NewTokenPoolCustomContext(
		func(ctx context.Context, _ string) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	yielded := 0
	go func() {
		client.executeStreamingWithRetries(ctx, "gemini-test", map[string]any{}, "", func(StreamChunk) bool {
			yielded++
			return true
		})
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream token fetch did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
	if yielded != 0 {
		t.Fatalf("canceled stream yielded %d chunks", yielded)
	}
}

// collectStream 把生产使用的原始帧扫描跑到底，收集所有 emit 出来的 chunk。
func collectStream(t *testing.T, raw string) (emitted []map[string]any, stopped bool, scanErr error) {
	t.Helper()
	emit := func(ch map[string]any) bool {
		emitted = append(emitted, ch)
		return true
	}
	scanErr = scanStreamRaw(strings.NewReader(raw), func(frame []byte) (bool, error) {
		stop, err := processStreamingJSON(frame, emit)
		if stop {
			stopped = true
		}
		return stop, err
	})
	return
}

func TestScanStreamRejectsOversizedObjects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "complete", raw: `{"value":"` + strings.Repeat("x", 128) + `"}`},
		{name: "incomplete", raw: `{"value":"` + strings.Repeat("x", 128)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			err := scanStreamWithLimit(strings.NewReader(test.raw), 64, func(map[string]any) (bool, error) {
				called = true
				return false, nil
			})
			if !errors.Is(err, errStreamObjectTooLarge) || !strings.Contains(err.Error(), "exceeds 64 byte limit") {
				t.Fatalf("oversized scan error=%v", err)
			}
			if called {
				t.Fatal("oversized object reached callback")
			}
		})
	}
}

func TestScanStreamAcceptsObjectAtLimit(t *testing.T) {
	raw := `{"ok":true}`
	called := false
	err := scanStreamWithLimit(strings.NewReader(raw), len(raw), func(map[string]any) (bool, error) {
		called = true
		return false, nil
	})
	if err != nil || !called {
		t.Fatalf("exact-limit object: called=%v err=%v", called, err)
	}
}

// wrap 把一段 candidates JSON 包成匿名 batchGraphql 的 results.data.ui.streamGenerateContentAnonymous 结构。
func wrap(inner string) string {
	return `{"results":[{"data":{"ui":{"streamGenerateContentAnonymous":` + inner + `}}}]}`
}

func TestParseCanonicalTextStreamChunkMatchesGenericPath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "clean without index",
			raw:  `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED"}]}`,
		},
		{
			name: "clean stop with index",
			raw:  `{"candidates":[{"content":{"parts":[{"text":"done"}],"role":"model"},"finishReason":"STOP","index":0}]}`,
		},
		{
			name: "dirty escaped text",
			raw:  `{"candidates":[{"content":{"parts":[{"data":"text","text":"line\n\"中文\ud83d\ude00","thought":false,"thoughtSignature":"","fileData":{},"functionCall":{},"functionResponse":{},"inlineData":{}}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED","index":0}]}`,
		},
		{
			name: "empty text without finish reason",
			raw:  `{"candidates":[{"content":{"parts":[{"text":""}],"role":"model"}}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			generic := parseJSONObject([]byte(test.raw))
			want := extractChunk(generic)
			got, ok := parseCanonicalTextStreamChunk([]byte(test.raw))
			if !ok {
				t.Fatal("canonical text frame unexpectedly missed fast path")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("fast path=%#v, generic path=%#v", got, want)
			}
		})
	}
}

func TestParseCanonicalTextStreamChunkRejectsExtendedShapes(t *testing.T) {
	tests := []string{
		`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP","index":1}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP","safetyRatings":[]}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}],"role":"model"},"finishReason":"STOP","index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model","unexpected":true},"finishReason":"STOP"}]}`,
	}
	for _, raw := range tests {
		if chunk, ok := parseCanonicalTextStreamChunk([]byte(raw)); ok {
			t.Fatalf("extended frame must use generic path, got %#v for %s", chunk, raw)
		}
	}
}

func TestTakeCanonicalFinishReason(t *testing.T) {
	for _, reason := range canonicalFinishReasons {
		raw := append(append([]byte(nil), reason.encoded...), []byte(`,tail`)...)
		got, rest, ok := takeCanonicalFinishReason(raw)
		if !ok || got != reason.value || string(rest) != `,tail` {
			t.Fatalf("reason %q: got=%q rest=%q ok=%v", reason.value, got, rest, ok)
		}
	}

	got, rest, ok := takeCanonicalFinishReason([]byte(`"NEW_REASON",tail`))
	if !ok || got != "NEW_REASON" || string(rest) != `,tail` {
		t.Fatalf("unknown reason: got=%q rest=%q ok=%v", got, rest, ok)
	}
}

func TestKnownCanonicalFinishReasonDoesNotAllocate(t *testing.T) {
	raw := []byte(`"FINISH_REASON_UNSPECIFIED",tail`)
	if allocations := testing.AllocsPerRun(100, func() {
		value, rest, ok := takeCanonicalFinishReason(raw)
		if !ok || value != "FINISH_REASON_UNSPECIFIED" || len(rest) != len(`,tail`) {
			t.Fatal("known finish reason was not parsed")
		}
	}); allocations != 0 {
		t.Fatalf("known finish reason allocated %.1f times", allocations)
	}
}

func TestScanStream_MultiChunkBraceScan(t *testing.T) {
	// 多个连在一起的对象（模拟上游一个网络 chunk 里塞了多帧），增量花括号扫描要逐个拆开。
	// usageMetadata 可能在 STOP 后单独到达，必须继续读取，不能把统计帧丢掉。
	raw := wrap(`{"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED","index":0}]}`) +
		wrap(`{"candidates":[{"content":{"parts":[{"text":" world"}],"role":"model"},"finishReason":"STOP","index":0}]}`) +
		wrap(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}}`)
	emitted, stopped, err := collectStream(t, raw)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if len(emitted) != 3 {
		t.Fatalf("emitted=%d, want 3", len(emitted))
	}
	if got := firstPartText(emitted[0]); got != "Hello" {
		t.Errorf("chunk0 text=%q, want Hello", got)
	}
	if got := firstPartText(emitted[1]); got != " world" {
		t.Errorf("chunk1 text=%q, want ' world'", got)
	}
	usage, ok := emitted[2]["usageMetadata"].(map[string]any)
	if !ok || usage["totalTokenCount"] != float64(12) {
		t.Fatalf("STOP 后的 usageMetadata 丢失: %#v", emitted[2])
	}
	if stopped {
		t.Error("真实 STOP 后仍应读取统计尾帧，不能提前停止扫描")
	}
}

// 最关键的红线测试：首帧 FINISH_REASON_UNSPECIFIED 绝不能截断。
func TestScanStream_UnspecifiedDoesNotTruncate(t *testing.T) {
	// 5 个内容帧都带 UNSPECIFIED，最后一帧才 STOP —— 必须全部 emit，不能在首帧停。
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		sb.WriteString(wrap(`{"candidates":[{"content":{"parts":[{"text":"x"}],"role":"model"},"finishReason":"FINISH_REASON_UNSPECIFIED"}]}`))
	}
	sb.WriteString(wrap(`{"candidates":[{"content":{"parts":[{"text":"end"}],"role":"model"},"finishReason":"STOP"}]}`))
	emitted, stopped, err := collectStream(t, sb.String())
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if len(emitted) != 6 {
		t.Fatalf("emitted=%d, want 6（UNSPECIFIED 不能截断！血泪教训）", len(emitted))
	}
	if stopped {
		t.Error("STOP 只结束内容生成，不应提前结束上游扫描")
	}
}

// 真实 finishReason 与末段文本同帧到达：该帧仍要 emit，扫描留给 EOF 收尾。
func TestScanStream_FinishWithContentSameFrame(t *testing.T) {
	raw := wrap(`{"candidates":[{"content":{"parts":[{"text":"final text"}],"role":"model"},"finishReason":"MAX_TOKENS"}]}`)
	emitted, stopped, err := collectStream(t, raw)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted=%d, want 1", len(emitted))
	}
	if got := firstPartText(emitted[0]); got != "final text" {
		t.Errorf("text=%q, want 'final text'（finish 同帧文本不能丢）", got)
	}
	if stopped {
		t.Error("MAX_TOKENS 后仍可能有统计尾帧，不应提前停止扫描")
	}
}

// 增量扫描跨网络 chunk：一个 JSON 对象被劈成两半，跨 chunk 续扫不应丢失。
// 用 splitReader 模拟逐字节投喂，验证 O(n) 续扫状态机的正确性。
func TestScanStream_SplitAcrossReads(t *testing.T) {
	raw := wrap(`{"candidates":[{"content":{"parts":[{"text":"split me"}],"role":"model"},"finishReason":"STOP"}]}`)
	// 逐字节投喂（最极端的分片），状态机必须能正确续扫。
	emitted := []map[string]any{}
	err := scanStreamRaw(&splitReader{data: []byte(raw), chunk: 1}, func(frame []byte) (bool, error) {
		stop, err := processStreamingJSON(frame, func(ch map[string]any) bool {
			emitted = append(emitted, ch)
			return true
		})
		return stop, err
	})
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted=%d, want 1（逐字节分片续扫失败）", len(emitted))
	}
	if got := firstPartText(emitted[0]); got != "split me" {
		t.Errorf("text=%q", got)
	}
}

// 字符串里含花括号 / 转义引号，不能被误判为对象边界。
func TestScanStream_BracesInsideString(t *testing.T) {
	raw := wrap(`{"candidates":[{"content":{"parts":[{"text":"a {nested} \"quote\" } brace"}],"role":"model"},"finishReason":"STOP"}]}`)
	emitted, _, err := collectStream(t, raw)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted=%d, want 1（字符串内花括号被误判为边界？）", len(emitted))
	}
	if got := firstPartText(emitted[0]); got != `a {nested} "quote" } brace` {
		t.Errorf("text=%q（转义/字符串内花括号处理错误）", got)
	}
}

func TestProcessStreamingJSONFallbackPreservesErrorsAndAlternateFormatting(t *testing.T) {
	_, err := processStreamingJSON(
		[]byte(`{"results":[{"errors":[{"message":"Failed to verify action"}]}]}`),
		func(map[string]any) bool { return true },
	)
	if ve := asVertexError(err); ve == nil || ve.Kind != "auth" {
		t.Fatalf("fallback error=%v, want auth", err)
	}

	formatted := []byte(` { "results": [ { "data": { "ui": { "streamGenerateContentAnonymous": { "candidates": [ { "content": { "parts": [ { "text": "formatted" } ] } } ] } } } } ] } `)
	var emitted map[string]any
	stop, err := processStreamingJSON(formatted, func(chunk map[string]any) bool {
		emitted = chunk
		return true
	})
	if err != nil || stop || emitted == nil || firstPartText(emitted) != "formatted" {
		t.Fatalf("formatted fallback stop=%v err=%v emitted=%#v", stop, err, emitted)
	}
}

// results 内的 "Failed to verify action" → AuthenticationError（触发同 token 重试）。
func TestProcessStreamingObject_VerifyFailError(t *testing.T) {
	obj := map[string]any{"results": []any{
		map[string]any{"errors": []any{map[string]any{"message": "Failed to verify action"}}},
	}}
	_, err := processStreamingObject(obj, func(map[string]any) bool { return true })
	if err == nil {
		t.Fatal("expected AuthenticationError")
	}
	if ve := asVertexError(err); ve == nil || ve.Kind != "auth" {
		t.Errorf("err=%v, want auth", err)
	}
}

// results 内真实错误（非 verify-fail）→ 结构化 VertexError。
func TestProcessStreamingObject_RealError(t *testing.T) {
	obj := map[string]any{"results": []any{
		map[string]any{"errors": []any{map[string]any{"message": "Resource exhausted", "code": float64(429)}}},
	}}
	_, err := processStreamingObject(obj, func(map[string]any) bool { return true })
	if err == nil {
		t.Fatal("expected error")
	}
	if ve := asVertexError(err); ve == nil {
		t.Errorf("err=%v, want VertexError", err)
	}
}

// _extract_chunk: 无 candidates 但有 metadata → 保留 metadata（对齐 Python：空 candidates 帧传递元数据）。
func TestExtractChunk_NoCandidates(t *testing.T) {
	chunk := extractChunk(map[string]any{"usageMetadata": map[string]any{"totalTokenCount": float64(5)}})
	if chunk == nil {
		t.Fatal("有 usageMetadata 应返回 chunk，不应为 nil")
	}
	if _, ok := chunk["usageMetadata"]; !ok {
		t.Error("usageMetadata 应保留")
	}
	if _, ok := chunk["candidates"]; ok {
		t.Error("不应有 candidates key")
	}
}

// _extract_chunk: candidates 为空列表 → 保留空列表（对齐 Python）。
func TestExtractChunk_EmptyCandidatesList(t *testing.T) {
	chunk := extractChunk(map[string]any{"candidates": []any{}})
	if chunk == nil {
		t.Fatal("空 candidates 列表应返回 chunk，不应为 nil")
	}
	cands, ok := chunk["candidates"].([]any)
	if !ok || len(cands) != 0 {
		t.Errorf("candidates=%v, want empty list", chunk["candidates"])
	}
}

// _extract_chunk: 完全空帧 → nil。
func TestExtractChunk_CompletelyEmpty(t *testing.T) {
	if chunk := extractChunk(map[string]any{}); chunk != nil {
		t.Errorf("空帧应返回 nil, got %v", chunk)
	}
}

// _extract_chunk 附带元数据：usageMetadata/modelVersion 等非空时带上。
func TestExtractChunk_AttachesMetadata(t *testing.T) {
	data := map[string]any{
		"candidates":    []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "hi"}}}}},
		"usageMetadata": map[string]any{"totalTokenCount": float64(3)},
		"modelVersion":  "gemini-3.1-flash",
	}
	chunk := extractChunk(data)
	if chunk == nil {
		t.Fatal("chunk 不应为 nil")
	}
	if _, ok := chunk["usageMetadata"]; !ok {
		t.Error("usageMetadata 未附带")
	}
	if chunk["modelVersion"] != "gemini-3.1-flash" {
		t.Errorf("modelVersion=%v", chunk["modelVersion"])
	}
}

func TestExtractChunkCleansOwnedDataInPlace(t *testing.T) {
	part := map[string]any{
		"data":     "text",
		"text":     "hello",
		"fileData": map[string]any{},
	}
	content := map[string]any{
		"parts":    []any{part},
		"internal": "discard",
	}
	candidate := map[string]any{"content": content, "finishReason": "STOP"}
	data := map[string]any{
		"candidates": []any{candidate},
		"unknown":    "discard",
	}

	chunk := extractChunk(data)
	if chunk == nil {
		t.Fatal("owned data should produce a chunk")
	}
	if _, ok := chunk["unknown"]; ok {
		t.Fatalf("unknown root field leaked into chunk: %#v", chunk)
	}
	if _, ok := content["internal"]; ok {
		t.Fatalf("unknown content field was not pruned: %#v", content)
	}
	if content["role"] != "model" {
		t.Fatalf("default role=%v, want model", content["role"])
	}
	if _, ok := part["data"]; ok {
		t.Fatalf("protobuf oneof marker was not removed: %#v", part)
	}
	if _, ok := part["fileData"]; ok {
		t.Fatalf("empty fileData was not removed: %#v", part)
	}

	// JSON 帧取得唯一所有权后应复用 candidate/content/part，而不是重新复制。
	part["ownershipProbe"] = true
	returnedCandidates := chunk["candidates"].([]any)
	returnedCandidate := returnedCandidates[0].(map[string]any)
	returnedContent := returnedCandidate["content"].(map[string]any)
	returnedParts := returnedContent["parts"].([]any)
	if returnedParts[0].(map[string]any)["ownershipProbe"] != true {
		t.Fatal("cleaned part did not retain transferred ownership")
	}
}

func TestExtractChunkCanonicalFrameDoesNotAllocate(t *testing.T) {
	data := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{
			"role":  "model",
			"parts": []any{map[string]any{"text": "hello"}},
		},
	}}}
	if allocations := testing.AllocsPerRun(100, func() {
		if chunk := extractChunk(data); chunk == nil {
			t.Fatal("canonical frame unexpectedly disappeared")
		}
	}); allocations != 0 {
		t.Fatalf("canonical extractChunk allocated %.1f times", allocations)
	}
}

func TestExtractChunkWritesCompactedSliceHeaders(t *testing.T) {
	content := map[string]any{
		"role": "model",
		"parts": []any{
			map[string]any{"text": "hello"},
			map[string]any{"thought": true},
		},
	}
	data := map[string]any{"candidates": []any{
		map[string]any{"content": content},
		"invalid",
	}}
	chunk := extractChunk(data)
	candidates := chunk["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("candidate header was not compacted: %#v", candidates)
	}
	parts := candidates[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("parts header was not compacted: %#v", parts)
	}
}

func TestCleanStreamCandidatesCompactsMixedValues(t *testing.T) {
	candidate := map[string]any{"finishReason": "STOP"}
	values := []any{"invalid", candidate, float64(3)}
	cleaned := cleanStreamCandidates(values)
	if len(cleaned) != 1 {
		t.Fatalf("cleaned candidates=%#v, want one map", cleaned)
	}
	if cleaned[0].(map[string]any)["finishReason"] != "STOP" {
		t.Fatalf("valid candidate changed: %#v", cleaned[0])
	}
	if values[1] != nil || values[2] != nil {
		t.Fatalf("compacted tail should release references: %#v", values)
	}
}

// _clean_parts: 畸形嵌套 text（list/dict）递归展开为纯字符串。
func TestCleanStreamParts_MalformedNestedText(t *testing.T) {
	parts := []any{
		map[string]any{"text": []any{map[string]any{"text": "nested"}, map[string]any{"text": " text"}}},
	}
	cleaned := cleanStreamParts(parts)
	if len(cleaned) != 1 {
		t.Fatalf("cleaned len=%d, want 1", len(cleaned))
	}
	p := cleaned[0].(map[string]any)
	if p["text"] != "nested text" {
		t.Errorf("text=%q, want 'nested text'", p["text"])
	}
}

// 正常字符串 text 原样保留。
func TestCleanStreamParts_NormalText(t *testing.T) {
	parts := []any{map[string]any{"text": "plain"}}
	cleaned := cleanStreamParts(parts)
	if len(cleaned) != 1 || cleaned[0].(map[string]any)["text"] != "plain" {
		t.Errorf("normal text 被改动: %v", cleaned)
	}
}

func TestCleanPart_EmptyDefaults(t *testing.T) {
	part := map[string]any{
		"data":             "text",
		"fileData":         map[string]any{},
		"functionCall":     map[string]any{},
		"functionResponse": map[string]any{},
		"inlineData":       map[string]any{},
	}
	if got := cleanPart(part); got != nil {
		t.Errorf("empty defaults should return nil, got %v", got)
	}
}

func TestCleanPart_FunctionCallStringArgs(t *testing.T) {
	part := map[string]any{
		"functionCall": map[string]any{
			"name": "search",
			"args": `{"q":"hello"}`,
		},
	}
	got := cleanPart(part)
	if got == nil {
		t.Fatal("expected non-nil part")
	}
	fc, ok := got["functionCall"].(map[string]any)
	if !ok {
		t.Fatal("expected functionCall in cleaned part")
	}
	if fc["name"] != "search" {
		t.Errorf("name=%v, want search", fc["name"])
	}
	args, ok := fc["args"].(map[string]any)
	if !ok {
		t.Fatalf("args should be map after normalization, got %T", fc["args"])
	}
	if args["q"] != "hello" {
		t.Errorf("args.q=%v, want hello", args["q"])
	}
}

func TestCleanPart_FunctionResponseStringResponse(t *testing.T) {
	part := map[string]any{
		"functionResponse": map[string]any{
			"name":     "search",
			"response": "result text",
		},
	}
	got := cleanPart(part)
	if got == nil {
		t.Fatal("expected non-nil part")
	}
	fr, ok := got["functionResponse"].(map[string]any)
	if !ok {
		t.Fatal("expected functionResponse in cleaned part")
	}
	if fr["name"] != "search" {
		t.Errorf("name=%v, want search", fr["name"])
	}
	resp, ok := fr["response"].(map[string]any)
	if !ok {
		t.Fatalf("response should be map after normalization, got %T", fr["response"])
	}
	if resp["result"] != "result text" {
		t.Errorf("response.result=%v, want 'result text'", resp["result"])
	}
}

func TestCleanStreamParts_SkipsEmpty(t *testing.T) {
	parts := []any{
		map[string]any{"data": "text", "fileData": map[string]any{}, "text": "hi"},
		map[string]any{"data": "text", "fileData": map[string]any{}, "functionCall": map[string]any{}, "functionResponse": map[string]any{}},
	}
	cleaned := cleanStreamParts(parts)
	if len(cleaned) != 1 {
		t.Fatalf("cleaned len=%d, want 1 (only first part should survive)", len(cleaned))
	}
	p := cleaned[0].(map[string]any)
	if p["text"] != "hi" {
		t.Errorf("text=%q, want 'hi'", p["text"])
	}
}

// extractTextRecursive 递归提取嵌套 text，并防无限递归（depth>20 截断）。
func TestExtractTextRecursive_DepthGuard(t *testing.T) {
	// 正向：嵌套 text 能逐层递归提取到底。
	if got := extractTextRecursive(map[string]any{"text": map[string]any{"text": "deep"}}, 0); got != "deep" {
		t.Errorf("嵌套 text 提取失败：got %q，want deep", got)
	}
	// 数组：各 text 拼接。
	if got := extractTextRecursive([]any{map[string]any{"text": "a"}, map[string]any{"text": "b"}}, 0); got != "ab" {
		t.Errorf("数组 text 拼接失败：got %q，want ab", got)
	}
	// depth guard：25 层嵌套必须能返回（不无限递归/不栈溢出），完成本身即证明守护生效。
	var deep any = "x"
	for i := 0; i < 25; i++ {
		deep = map[string]any{"text": deep}
	}
	if got := extractTextRecursive(deep, 0); got != "" {
		t.Errorf("超过深度上限的 map 不应继续展开，got %q", got)
	}
	if got := extractTextRecursive(strings.Repeat("x", 600), maximumRecursiveTextDepth+1); len(got) != maximumRecursiveTextLeafBytes {
		t.Errorf("深度上限字符串应截断到 %d 字节，got %d", maximumRecursiveTextLeafBytes, len(got))
	}
	cycle := make([]any, 1)
	cycle[0] = cycle
	if got := extractTextRecursive(cycle, 0); got != "" {
		t.Errorf("自引用数组应由深度上限终止，got %q", got)
	}
}

func TestProcessStreamingObject_ClientDisconnectStops(t *testing.T) {
	obj := parseJSONObject([]byte(wrap(`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)))
	stop, err := processStreamingObject(obj, func(map[string]any) bool { return false })
	if err != nil {
		t.Fatalf("process error: %v", err)
	}
	if !stop {
		t.Error("客户端断开时应立即停止扫描")
	}
}

// ---- 测试小工具 ----

func firstPartText(chunk map[string]any) string {
	cands, _ := chunk["candidates"].([]any)
	if len(cands) == 0 {
		return ""
	}
	c, _ := cands[0].(map[string]any)
	content, _ := c["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) == 0 {
		return ""
	}
	p, _ := parts[0].(map[string]any)
	if s, ok := p["text"].(string); ok {
		return s
	}
	return ""
}

// splitReader 按固定 chunk 大小逐块投喂数据，模拟网络流分片（测增量续扫）。
type splitReader struct {
	data  []byte
	chunk int
	pos   int
}

type terminalErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *terminalErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}

func (r *splitReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	end := r.pos + r.chunk
	if end > len(r.data) {
		end = len(r.data)
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	return n, nil
}
