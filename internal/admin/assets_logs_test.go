package admin

import (
	"strings"
	"testing"
)

func TestLogPageUsesConditionalPollingWithoutSettingsWaterfall(t *testing.T) {
	scriptBytes, err := Assets.ReadFile("assets/page-logs.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	if strings.Contains(script, "API.settings.get()") {
		t.Fatal("log polling still fetches the full settings document")
	}
	for _, required := range []string{
		"If-None-Match",
		"res.status === 304",
		"logsLoadPromise",
		"stopLogsAutoRefresh",
		"visibilitychange",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("conditional log polling marker %q is missing", required)
		}
	}
}

func TestNavigationStopsLogPollingWhenPageIsHidden(t *testing.T) {
	scriptBytes, err := Assets.ReadFile("assets/admin.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	if !strings.Contains(script, "curPage === 'logs' && page !== 'logs'") ||
		!strings.Contains(script, "leaveLogsPage()") {
		t.Fatal("navigation does not stop log polling when leaving the page")
	}
}

func TestLogPageGroupsMultiLineErrorsAndClassifiesLevels(t *testing.T) {
	scriptBytes, err := Assets.ReadFile("assets/page-logs.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		"parseLogEntries",  // 多行报错体 / panic 堆栈归组为单条
		"classifyLogLevel", // 级别判定
		`error=""`,         // 成功路径的空错误占位不误判为报错
		"filterLogLevel",   // 级别筛选
		"searchLogs",       // 关键词搜索
		"copyLogEntry",     // 单条复制
		"toggleLogExpand",  // 超长报错折叠展开
		"logFollowTail",    // 智能滚动跟随
		"jumpToLogBottom",  // 离开底部时的"回到底部"入口
	} {
		if !strings.Contains(script, required) {
			t.Errorf("error-log display marker %q is missing", required)
		}
	}
}

func TestLogPageMarkupExposesErrorFilters(t *testing.T) {
	htmlBytes, err := Assets.ReadFile("assets/admin.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(htmlBytes)
	for _, required := range []string{
		`id="logLevelChips"`,
		`data-level="error"`,
		`id="logSearchInput"`,
		`id="logEmptyHint"`,
		`id="logJumpBtn"`,
		`id="logsHint"`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("log page markup marker %q is missing", required)
		}
	}
}
