package vertex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// captureLogs 把标准 log 重定向到 buffer（测试内串行运行，无并发竞争），
// 用于断言竞速账本输出的日志形态与数量。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func countLogLinesContaining(haystack, needle string) int {
	count := 0
	for _, line := range strings.Split(haystack, "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

// frontendErrorKeywords 与管理面板 page-logs.js 的错误分级关键词保持同步。
// 胜出/回退汇总行必须避开这些词，才能在前端保持 info 级不被误报为错误。
var frontendErrorKeywords = []string{
	"失败", "错误", "报错", "超时", "异常", "429", "警告",
	"error", "fail", "failed", "failure", "timeout", "timed out",
	"refused", "unreachable", "denied",
}

func assertNoFrontendErrorKeywords(t *testing.T, output string) {
	t.Helper()
	lowered := strings.ToLower(output)
	for _, keyword := range frontendErrorKeywords {
		marker := strings.ToLower(keyword)
		if keyword == "429" {
			if strings.Contains(output, "429") {
				t.Errorf("胜出汇总不应包含前端报错关键词 %q: %q", keyword, output)
			}
			continue
		}
		if strings.Contains(lowered, marker) {
			t.Errorf("胜出汇总不应包含前端报错关键词 %q: %q", keyword, output)
		}
	}
}

func TestRaceJournalWinSummaryIsSingleInfoSafeLine(t *testing.T) {
	j := newRaceJournal("req-win")
	for range 9 {
		j.recordLaunch()
	}
	j.recordFailure("节点A", NewRateLimitError("rate limited upstream", 0))
	j.recordFailure("节点B", NewRateLimitError("rate limited upstream", 0))
	j.recordFailure("节点C", NewAuthenticationError("recaptcha expired"))

	j.recordWinner("东京-01", 832*time.Millisecond)

	buf := captureLogs(t)
	j.logWinSummary()
	out := buf.String()

	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("胜出汇总应为单行, got %d newlines: %q", got, out)
	}
	for _, want := range []string{
		"[Racing] 请求ID=req-win 竞速胜出: 东京-01 (832ms)",
		"候选 9",
		"落选 3 (限流×2, 认证×1)",
		"对冲中止 5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("胜出汇总缺少 %q: %q", want, out)
		}
	}
	assertNoFrontendErrorKeywords(t, out)
}

func TestRaceJournalWinSummaryWithoutFailuresOmitsLoserSegment(t *testing.T) {
	j := newRaceJournal("req-clean")
	j.recordLaunch()
	j.recordWinner("大阪-02", 45*time.Millisecond)

	buf := captureLogs(t)
	j.logWinSummary()
	out := buf.String()

	if !strings.Contains(out, "候选 1, 总耗时") {
		t.Errorf("无落败时应省略落选/中止段: %q", out)
	}
	if strings.Contains(out, "落选") || strings.Contains(out, "中止") {
		t.Errorf("无落败时不应出现落选/中止字段: %q", out)
	}
	assertNoFrontendErrorKeywords(t, out)
}

func TestRaceJournalFallbackSummaryIsInfoSafe(t *testing.T) {
	j := newRaceJournal("req-soft")
	for range 6 {
		j.recordLaunch()
	}
	j.recordSuccess()
	j.recordSuccess()
	j.recordFailure("节点A", NewEmptyResponseError("no data"))
	j.recordFailure("节点B", NewEmptyResponseError("no data"))

	buf := captureLogs(t)
	j.logFallbackSummary()
	out := buf.String()

	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("回退汇总应为单行, got %d newlines: %q", got, out)
	}
	for _, want := range []string{"竞速完成", "无首选结果，采用回退候选", "候选 6", "落选 2 (空响应×2)", "对冲中止 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("回退汇总缺少 %q: %q", want, out)
		}
	}
	if strings.Contains(out, "[ERROR]") {
		t.Errorf("回退汇总不应带 [ERROR] 标记: %q", out)
	}
	assertNoFrontendErrorKeywords(t, out)
}

func TestRaceJournalFailSummaryAggregatesKindsAndCapsNames(t *testing.T) {
	j := newRaceJournal("req-fail")
	for range 12 {
		j.recordLaunch()
	}
	for i := range 5 {
		j.recordFailure(fmt.Sprintf("节点%d", i), NewRateLimitError("busy", 0))
	}
	for i := range 4 {
		j.recordFailure(fmt.Sprintf("认证%d", i), NewAuthenticationError("verify failed"))
	}
	for i := range 3 {
		j.recordFailure(fmt.Sprintf("断连%d", i), NewUnavailableError("upstream request: connection refused"))
	}

	buf := captureLogs(t)
	j.logFailSummary(NewRateLimitError("服务器繁忙，请求过于频繁，请稍后重试 (rate limited)", 0))
	out := buf.String()

	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("全败汇总应为单行, got %d newlines: %q", got, out)
	}
	for _, want := range []string{
		"[Racing] [ERROR] 请求ID=req-fail 竞速失败",
		"候选 12 个全部落败",
		"限流×5(节点0,节点1,节点2,+2)",
		"认证×4",
		"不可用×3",
		"最后错误: [ratelimit] 服务器繁忙，请求过于频繁，请稍后重试 (rate limited)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("全败汇总缺少 %q: %q", want, out)
		}
	}
}

func TestRaceJournalFailSummaryCompactsMultilineUpstreamBody(t *testing.T) {
	j := newRaceJournal("req-ml")
	j.recordLaunch()
	j.recordFailure("节点A", NewUnavailableError("dial failed"))
	multiline := "upstream error:\n{\n  \"error\": {\n    \"code\": 500\n  }\n}\n"

	buf := captureLogs(t)
	j.logFailSummary(NewInternalError(multiline))
	out := buf.String()

	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("多行错误体应压缩为单行, got %d newlines: %q", got, out)
	}
	if !strings.Contains(out, `upstream error: { "error": { "code": 500 } }`) {
		t.Errorf("多行错误体未正确折叠: %q", out)
	}
}

func TestRaceJournalSingleNodeSummaries(t *testing.T) {
	buf := captureLogs(t)
	win := newRaceJournal("req-single-win")
	win.logSingleWin("香港-03", 1200*time.Millisecond)
	out := buf.String()
	if !strings.Contains(out, "[Racing] 请求ID=req-single-win 单节点直连成功: 香港-03 (1.2s)") {
		t.Errorf("单节点成功汇总格式不符: %q", out)
	}
	assertNoFrontendErrorKeywords(t, out)

	buf.Reset()
	fail := newRaceJournal("req-single-fail")
	fail.logSingleFail("香港-03", NewInvalidArgumentError("bad payload"), 300*time.Millisecond)
	out = buf.String()
	for _, want := range []string{
		"[Racing] [ERROR] 请求ID=req-single-fail 单节点请求失败: 香港-03 (300ms)",
		"错误: [invalid] bad payload",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("单节点失败汇总缺少 %q: %q", want, out)
		}
	}
}

func TestRaceJournalHedgeCanceledNeverNegative(t *testing.T) {
	j := newRaceJournal("req-edge")
	j.recordLaunch()
	j.recordWinner("唯一", time.Second)
	if got := j.hedgeCanceled(); got != 0 {
		t.Errorf("hedgeCanceled() = %d, want 0", got)
	}
	// 防御性：结果统计出现异常倒挂时不应输出负数。
	j.recordFailure("迟到失败", errors.New("late"))
	if got := j.hedgeCanceled(); got != 0 {
		t.Errorf("hedgeCanceled() = %d, want 0 (clamped)", got)
	}
}

func TestJournalKindLabelMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ratelimit", NewRateLimitError("x", 0), "限流"},
		{"auth", NewAuthenticationError("x"), "认证"},
		{"permission", NewPermissionDeniedError("x"), "权限"},
		{"invalid", NewInvalidArgumentError("x"), "参数"},
		{"notfound", NewNotFoundError("x"), "404"},
		{"empty", NewEmptyResponseError("x"), "空响应"},
		{"unavailable", NewUnavailableError("x"), "不可用"},
		{"internal", NewInternalError("x"), "内部"},
		{"plain-error", errors.New("dial tcp: refused"), "网络"},
		{"wrapped", fmt.Errorf("wrap: %w", NewRateLimitError("x", 0)), "限流"},
	}
	for _, tc := range cases {
		if got := journalKindLabel(tc.err); got != tc.want {
			t.Errorf("journalKindLabel(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := journalKindLabel(context.Canceled); got != "取消" {
		t.Errorf("journalKindLabel(canceled) = %q, want 取消", got)
	}
}

func TestCompactLogMessage(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		max     int
		want    string
		wantSub bool
	}{
		{"collapses-newlines", "line1\nline2\n\nline3", 100, "line1 line2 line3", false},
		{"collapses-tabs", "a\t\tb", 100, "a b", false},
		{"truncates-with-ellipsis", "0123456789", 5, "01234…", false},
		{"runes-not-bytes", "你好世界朋友", 4, "你好世界…", false},
		{"empty", " \n\t ", 100, "", false},
	}
	for _, tc := range cases {
		got := compactLogMessage(tc.input, tc.max)
		if got != tc.want {
			t.Errorf("compactLogMessage(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := compactLogMessage("x", 0); got != "x" {
		t.Errorf("compactLogMessage with max=0 should use default limit, got %q", got)
	}
	long := strings.Repeat("字", journalMaxLastErrChars+10)
	if got := compactLogMessage(long, 0); len([]rune(got)) != journalMaxLastErrChars+1 {
		t.Errorf("default limit truncation length = %d, want %d", len([]rune(got)), journalMaxLastErrChars+1)
	}
}

func TestJournalShowNamesCapsAtThree(t *testing.T) {
	if got := journalShowNames([]string{"a", "b"}); got != "a,b" {
		t.Errorf("journalShowNames(2) = %q", got)
	}
	if got := journalShowNames([]string{"a", "b", "c", "d", "e"}); got != "a,b,c,+2" {
		t.Errorf("journalShowNames(5) = %q, want a,b,c,+2", got)
	}
}
