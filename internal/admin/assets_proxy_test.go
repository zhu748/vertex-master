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

func TestRecentProxyAndEnvironmentKeyUIAreEmbedded(t *testing.T) {
	htmlBytes, err := Assets.ReadFile("assets/admin.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, id := range []string{
		"recentProxyCard",
		"recentProxyName",
		"recentProxyAddress",
		"recentProxyType",
		"recentProxyUpdated",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("recent proxy field %q is missing", id)
		}
	}

	nodesScript, err := Assets.ReadFile("assets/page-nodes.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		"loadRecentProxyStatus",
		"startRecentProxyPolling",
		"recent-proxy-badge",
	} {
		if !strings.Contains(string(nodesScript), token) {
			t.Errorf("recent proxy UI token %q is missing", token)
		}
	}

	keysScript, err := Assets.ReadFile("assets/page-keys.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"key_masked", "read_only", "environment"} {
		if !strings.Contains(string(keysScript), token) {
			t.Errorf("environment key UI token %q is missing", token)
		}
	}
}

func TestClaudePromptSettingsUIIsEmbedded(t *testing.T) {
	settingsScript, err := Assets.ReadFile("assets/page-settings.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(settingsScript)
	for _, token := range []string{
		"set_claude_prompt_replacement_enabled",
		"set_claude_prompt_replace_from",
		"set_claude_prompt_replace_to",
		"set_claude_prompt_injection_enabled",
		"set_claude_prompt_injection_position",
		"set_claude_prompt_injection_text",
		"useLatestClaudePromptAsFind",
		"loadLatestClaudePrompt",
	} {
		if !strings.Contains(script, token) {
			t.Errorf("Claude prompt settings token %q is missing", token)
		}
	}

	apiScript, err := Assets.ReadFile("assets/api.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		"/api/admin/claude-prompt/latest",
		"claudePrompt",
	} {
		if !strings.Contains(string(apiScript), token) {
			t.Errorf("Claude prompt API token %q is missing", token)
		}
	}
}
