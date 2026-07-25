package admin

import (
	"strings"
	"testing"
)

func TestProxyBatchControlsAreEmbedded(t *testing.T) {
	htmlBytes, err := Assets.ReadFile("assets/admin.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, id := range []string{
		"batchTestStartBtn",
		"batchTestMaxNodes",
		"batchTestConcurrency",
		"batchTestTimeoutSeconds",
		"batchTestIncludeDisabled",
		"batchTestRecoverDisabled",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("proxy batch control %q is missing from admin.html", id)
		}
	}

	scriptBytes, err := Assets.ReadFile("assets/page-nodes.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, field := range []string{
		"include_disabled",
		"recover_disabled",
		"max_nodes",
		"concurrency",
		"timeout_seconds",
	} {
		if !strings.Contains(script, field+":") {
			t.Errorf("proxy batch request field %q is missing from page-nodes.js", field)
		}
	}
}

func TestProxySettingsFieldsAreEmbedded(t *testing.T) {
	scriptBytes, err := Assets.ReadFile("assets/page-settings.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, key := range []string{
		"proxy_failover_max_attempts",
		"proxy_health_check_enabled",
		"proxy_health_check_interval_minutes",
		"proxy_health_check_batch_size",
		"proxy_health_check_concurrency",
		"proxy_health_check_timeout_seconds",
	} {
		if !strings.Contains(script, "k: '"+key+"'") {
			t.Errorf("proxy setting %q is missing from page-settings.js", key)
		}
	}
}
