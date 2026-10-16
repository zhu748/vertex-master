package vertex

import (
	"context"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// TestRunRaceAllFailEmitsExactlyOneSummaryLine 验证非 debug 模式下全败竞速
// 只输出一条带 [ERROR] 的账本汇总行，不再逐节点刷失败明细。
func TestRunRaceAllFailEmitsExactlyOneSummaryLine(t *testing.T) {
	installRaceTestNodes(t, 4)
	cfg := raceTestConfig(2, 4, 50)

	buf := captureLogs(t)
	_, err := RunRace(context.Background(), cfg, func(_ context.Context, _ string) (string, error) {
		return "", NewUnavailableError("retryable test failure")
	})
	if err == nil {
		t.Fatal("RunRace() error = nil, want all-fail error")
	}

	out := buf.String()
	if got := countLogLinesContaining(out, "[Racing]"); got != 1 {
		t.Fatalf("非 debug 全败竞速应只输出 1 条 [Racing] 汇总, got %d:\n%s", got, out)
	}
	for _, want := range []string{"[ERROR]", "竞速失败", "候选 4 个全部落败", "不可用×4"} {
		if !strings.Contains(out, want) {
			t.Errorf("全败汇总缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "失败: retryable test failure") {
		t.Errorf("非 debug 模式不应出现逐节点失败明细:\n%s", out)
	}
}

// TestRunRaceWinEmitsExactlyOneSummaryLine 验证胜出路径同样只留一条 info 级汇总。
func TestRunRaceWinEmitsExactlyOneSummaryLine(t *testing.T) {
	installRaceTestNodes(t, 3)
	cfg := raceTestConfig(2, 3, 50)

	buf := captureLogs(t)
	value, err := RunRace(context.Background(), cfg, func(_ context.Context, _ string) (string, error) {
		return "winner", nil
	})
	if err != nil || value != "winner" {
		t.Fatalf("RunRace() = %q, %v", value, err)
	}

	out := buf.String()
	if got := countLogLinesContaining(out, "[Racing]"); got != 1 {
		t.Fatalf("非 debug 竞速胜出应只输出 1 条 [Racing] 汇总, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "竞速胜出") {
		t.Errorf("胜出汇总应包含竞速胜出:\n%s", out)
	}
	if strings.Contains(out, "[ERROR]") {
		t.Errorf("胜出汇总不应带 [ERROR]:\n%s", out)
	}
	assertNoFrontendErrorKeywords(t, out)
}

// TestRunRaceDebugModeKeepsPerNodeDetail 验证 debug 模式仍保留逐节点明细，
// 排障能力不受账本降噪影响。
func TestRunRaceDebugModeKeepsPerNodeDetail(t *testing.T) {
	installRaceTestNodes(t, 3)
	base := config.DefaultConfig()
	base.ParallelPoolEnabled = true
	base.StickyNodePriority = false
	base.ParallelPoolSize = 2
	base.ParallelNodeTopK = 3
	base.ParallelPoolDelayDynamic = false
	base.ParallelPoolDelayMs = 50
	base.ProxyFailoverMaxAttempts = 3
	base.DebugMode = true
	cfg := config.StaticProvider(base)

	buf := captureLogs(t)
	_, err := RunRace(context.Background(), cfg, func(_ context.Context, _ string) (string, error) {
		return "", NewUnavailableError("retryable test failure")
	})
	if err == nil {
		t.Fatal("RunRace() error = nil, want all-fail error")
	}

	out := buf.String()
	if !strings.Contains(out, "失败: retryable test failure") {
		t.Errorf("debug 模式应保留逐节点失败明细:\n%s", out)
	}
	if !strings.Contains(out, "竞速失败") {
		t.Errorf("debug 模式也应输出账本汇总:\n%s", out)
	}
}

// TestRunRaceSingleNodeFailureEmitsConclusionLine 覆盖并发池关闭的单节点
// 直连路径：失败时输出单条 [ERROR] 结论行。
func TestRunRaceSingleNodeFailureEmitsConclusionLine(t *testing.T) {
	base := config.DefaultConfig()
	base.ActiveNodeURI = "http://serial.invalid:8080"
	cfg := serialRaceConfig{ConfigProvider: config.StaticProvider(base)}

	buf := captureLogs(t)
	_, err := RunRace(context.Background(), cfg, func(_ context.Context, _ string) (string, error) {
		return "", NewInvalidArgumentError("bad payload")
	})
	if err == nil {
		t.Fatal("RunRace() error = nil, want invalid argument error")
	}

	out := buf.String()
	if !strings.Contains(out, "[Racing] [ERROR] 请求ID= 单节点请求失败") {
		t.Errorf("单节点失败应输出 [ERROR] 结论行:\n%s", out)
	}
	if !strings.Contains(out, "错误: [invalid] bad payload") {
		t.Errorf("单节点失败结论应携带压缩后的错误:\n%s", out)
	}
}
