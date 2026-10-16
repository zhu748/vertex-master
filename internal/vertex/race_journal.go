package vertex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// race_journal 把单次请求竞速过程中所有候选节点的结果收集成一份"账本"，
// 在竞速结束（胜出 / 全败 / 回退）时输出唯一一条紧凑汇总行。
//
// 背景：并发竞速开启时（默认 16 并发、failover 60），一次请求会并行探测
// 大量候选节点，每个候选的拨号、认证、429、超时都会产生日志。逐条打印
// 会让日志面板被同一请求的几十条报错淹没，且看不出"这次请求整体表现如何"。
// 账本把逐节点明细降级到 debug 模式，非 debug 模式下每个请求只留下一条
// 可读的结论行，例如：
//
//	[Racing] 请求ID=abc123 竞速胜出: 东京-01 (832ms) | 候选 9, 落选 3 (限流×2, 认证×1), 对冲中止 5, 总耗时 3.4s
//	[Racing] [ERROR] 请求ID=abc123 竞速失败: 候选 12 个全部失败 (限流×5(东京-01,大阪-02,+3), 认证×4, 不可用×3), 总耗时 9.1s | 最后错误: [ratelimit] 服务器繁忙，请求过于频繁…
//
// 汇总行的用词刻意避开前端日志分级的关键词（失败/错误/超时/异常/429 等）：
// 胜出行保持 info 级；全败行显式携带 [ERROR] 标记，保证被识别为 error 级。
const (
	// journalMaxStoredNamesPerKind 每类失败最多记录的节点名数量（防内存与行宽失控）。
	journalMaxStoredNamesPerKind = 8
	// journalMaxShownNamesPerKind 汇总行里每类失败最多展示的节点名数量。
	journalMaxShownNamesPerKind = 3
	// journalMaxLastErrChars 全败汇总里"最后错误"消息的最大字符数。
	journalMaxLastErrChars = 160
	// journalMaxVariablesChars 400 诊断行里 Variables payload 的最大字符数。
	journalMaxVariablesChars = 400
)

// raceJournal 记录一次 RunRace 的全部候选结果。
//
// 所有方法都只在竞速主循环 goroutine 内调用（结果统一经 resCh 串行处理），
// 因此无需加锁；launchNode 在进入循环前对首个候选的调用同样处于同一 goroutine。
type raceJournal struct {
	reqID     string
	startedAt time.Time
	launched  int
	successes int
	failures  int
	// failCounts/failNames 按错误类别聚合计数与节点名；failOrder 保留类别
	// 首次出现的顺序，渲染时再按计数降序排列，保证输出稳定可读。
	failCounts map[string]int
	failNames  map[string][]string
	failOrder  []string
	winner     string
	winElapsed time.Duration
}

func newRaceJournal(reqID string) *raceJournal {
	return &raceJournal{
		reqID:      reqID,
		startedAt:  time.Now(),
		failCounts: make(map[string]int),
		failNames:  make(map[string][]string),
	}
}

func (j *raceJournal) recordLaunch() {
	j.launched++
}

// recordSuccess 记录一个成功但未被选为最终结果的候选（非首选回退候选）。
func (j *raceJournal) recordSuccess() {
	j.successes++
}

func (j *raceJournal) recordWinner(name string, elapsed time.Duration) {
	j.winner = name
	j.winElapsed = elapsed
}

// recordFailure 记录一个真实失败的候选。上下文取消（对冲竞速中被取消的
// 落后者）不算失败，由 launched - successes - failures - 1 推导为"对冲中止"。
func (j *raceJournal) recordFailure(name string, err error) {
	j.failures++
	label := journalKindLabel(err)
	if _, seen := j.failCounts[label]; !seen {
		j.failOrder = append(j.failOrder, label)
	}
	j.failCounts[label]++
	if names := j.failNames[label]; len(names) < journalMaxStoredNamesPerKind {
		j.failNames[label] = append(names, name)
	}
}

// hedgeCanceled 返回被对冲取消（仍在飞行中被胜者取消）的候选数。
func (j *raceJournal) hedgeCanceled() int {
	winners := 0
	if j.winner != "" {
		winners = 1
	}
	canceled := j.launched - j.failures - j.successes - winners
	if canceled < 0 {
		return 0
	}
	return canceled
}

// logWinSummary 输出胜出汇总（info 级用词，避免被前端降/升级）。
func (j *raceJournal) logWinSummary() {
	var b strings.Builder
	fmt.Fprintf(&b, "[Racing] 请求ID=%s 竞速胜出: %s (%s)", j.reqID, j.winner, journalDuration(j.winElapsed))
	fmt.Fprintf(&b, " | 候选 %d", j.launched)
	if j.failures > 0 {
		fmt.Fprintf(&b, ", 落选 %d (%s)", j.failures, j.failureBreakdown(false))
	}
	if c := j.hedgeCanceled(); c > 0 {
		fmt.Fprintf(&b, ", 对冲中止 %d", c)
	}
	fmt.Fprintf(&b, ", 总耗时 %s", journalDuration(time.Since(j.startedAt)))
	log.Print(b.String())
}

// logFallbackSummary 输出"无首选结果、采用回退候选"的软完成汇总。
func (j *raceJournal) logFallbackSummary() {
	var b strings.Builder
	fmt.Fprintf(&b, "[Racing] 请求ID=%s 竞速完成: 无首选结果，采用回退候选 | 候选 %d", j.reqID, j.launched)
	if j.failures > 0 {
		fmt.Fprintf(&b, ", 落选 %d (%s)", j.failures, j.failureBreakdown(false))
	}
	if c := j.hedgeCanceled(); c > 0 {
		fmt.Fprintf(&b, ", 对冲中止 %d", c)
	}
	fmt.Fprintf(&b, ", 总耗时 %s", journalDuration(time.Since(j.startedAt)))
	log.Print(b.String())
}

// logFailSummary 输出全败汇总（显式 [ERROR] 标记，前端据此归入错误级）。
// lastErr 为空时（防御性终止路径）使用通用兜底文案。
func (j *raceJournal) logFailSummary(lastErr error) {
	kind, message := "unknown", "all nodes failed"
	if lastErr != nil {
		kind = journalKindLabel(lastErr)
		if ve := asVertexError(lastErr); ve != nil {
			kind = ve.Kind
		}
		message = lastErr.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Racing] [ERROR] 请求ID=%s 竞速失败: 候选 %d 个全部落败", j.reqID, j.launched)
	if j.failures > 0 {
		fmt.Fprintf(&b, " (%s)", j.failureBreakdown(true))
	}
	fmt.Fprintf(&b, ", 总耗时 %s", journalDuration(time.Since(j.startedAt)))
	fmt.Fprintf(&b, " | 最后错误: [%s] %s", kind, compactLogMessage(message, journalMaxLastErrChars))
	log.Print(b.String())
}

// logSingleWin / logSingleFail 覆盖并发池关闭（或无候选）时的单节点直连路径，
// 保证任何模式下每个请求都有且仅有一条结论日志。
func (j *raceJournal) logSingleWin(name string, elapsed time.Duration) {
	log.Printf("[Racing] 请求ID=%s 单节点直连成功: %s (%s)", j.reqID, name, journalDuration(elapsed))
}

func (j *raceJournal) logSingleFail(name string, err error, elapsed time.Duration) {
	kind := journalKindLabel(err)
	if ve := asVertexError(err); ve != nil {
		kind = ve.Kind
	}
	log.Printf(
		"[Racing] [ERROR] 请求ID=%s 单节点请求失败: %s (%s) | 错误: [%s] %s",
		j.reqID, name, journalDuration(elapsed), kind, compactLogMessage(err.Error(), journalMaxLastErrChars),
	)
}

// failureBreakdown 渲染失败类别聚合，如 "限流×2, 认证×1"。
// withNames 为 true 时（全败场景）附带每类节点名（封顶 3 个，超出 +N）。
func (j *raceJournal) failureBreakdown(withNames bool) string {
	kinds := make([]string, len(j.failOrder))
	copy(kinds, j.failOrder)
	sort.SliceStable(kinds, func(a, b int) bool {
		return j.failCounts[kinds[a]] > j.failCounts[kinds[b]]
	})

	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		count := j.failCounts[kind]
		part := fmt.Sprintf("%s×%d", kind, count)
		if withNames {
			if names := j.failNames[kind]; len(names) > 0 {
				part += "(" + journalShowNames(names) + ")"
			}
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "未知"
	}
	return strings.Join(parts, ", ")
}

func journalShowNames(names []string) string {
	if len(names) <= journalMaxShownNamesPerKind {
		return strings.Join(names, ",")
	}
	return strings.Join(names[:journalMaxShownNamesPerKind], ",") +
		fmt.Sprintf(",+%d", len(names)-journalMaxShownNamesPerKind)
}

// journalKindLabel 把候选错误映射为简短中文类别标签。
// 标签刻意避开 失败/错误/超时/异常/429 等词，防止胜出汇总被前端日志分级
// 误判为报错级别（全败汇总显式带 [ERROR] 标记，不受此约束影响）。
func journalKindLabel(err error) string {
	if errors.Is(err, context.Canceled) {
		return "取消"
	}
	if ve := asVertexError(err); ve != nil {
		switch ve.Kind {
		case "ratelimit":
			return "限流"
		case "auth":
			return "认证"
		case "permission":
			return "权限"
		case "invalid":
			return "参数"
		case "notfound":
			return "404"
		case "empty":
			return "空响应"
		case "unavailable":
			return "不可用"
		case "internal":
			return "内部"
		case "server":
			return "服务端"
		case "client":
			return "客户端"
		}
		if ve.Code == 429 {
			return "限流"
		}
	}
	return "网络"
}

// journalDuration 把耗时渲染成紧凑形式（832ms / 3.4s / 1m05s）。
func journalDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// compactLogMessage 把可能包含多行 JSON / 堆栈的错误消息压成单行并截断。
// 上游错误 body（如 429 响应体）经常是带换行的 JSON，直接进日志会一条
// 变十几行；账本汇总与 400 诊断行统一经此压缩，保证一条日志就是一条。
func compactLogMessage(msg string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = journalMaxLastErrChars
	}
	compact := strings.Join(strings.Fields(msg), " ")
	if compact == "" {
		return ""
	}
	runes := []rune(compact)
	if len(runes) <= maxChars {
		return compact
	}
	return string(runes[:maxChars]) + "…"
}
