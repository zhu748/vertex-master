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
