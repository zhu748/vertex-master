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

func TestNodePageLoadsIndependentDataWithoutRequestWaterfall(t *testing.T) {
	scriptBytes, err := Assets.ReadFile("assets/page-nodes.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	loadStart := strings.Index(script, "async function loadNodes(options)")
	loadEnd := strings.Index(script, "async function addStandardProxy()")
	if loadStart < 0 || loadEnd <= loadStart {
		t.Fatal("loadNodes function boundaries are missing from page-nodes.js")
	}
	loadBody := script[loadStart:loadEnd]
	for _, token := range []string{
		"refreshNodeTestProgress(loadSequence)",
		"loadProxySubscriptions(!options.refreshSubscriptions)",
		"d = await API.nodes.list(",
		"curSettings.active_node_uri = d.active_node_uri",
		"curSettings.proxy_url = d.proxy_url",
	} {
		if !strings.Contains(loadBody, token) {
			t.Errorf("concurrent node loader token %q is missing", token)
		}
	}
	if strings.Contains(script, "API.settings.get()") ||
		strings.Contains(loadBody, "await API.nodes.testProgress()") {
		t.Fatal("node page reintroduced a settings request or sequential progress request")
	}
	renderIndex := strings.Index(loadBody, "tbody.appendChild(frag)")
	auxiliaryWaitIndex := strings.LastIndex(
		loadBody,
		"await Promise.all([progressRequest, subscriptionsRequest])",
	)
	if renderIndex < 0 || auxiliaryWaitIndex < 0 || renderIndex >= auxiliaryWaitIndex {
		t.Fatal("node table rendering must finish before waiting for auxiliary data")
	}

	for _, token := range []string{
		"if (useCache && proxySubscriptionsLoaded) return Promise.resolve()",
		"loadNodes({ refreshSubscriptions: true })",
	} {
		if !strings.Contains(script, token) {
			t.Errorf("node page cache/refresh token %q is missing", token)
		}
	}
	functionBody := func(signature string) string {
		t.Helper()
		start := strings.Index(script, signature)
		if start < 0 {
			t.Fatalf("node page function %q is missing", signature)
		}
		endOffset := strings.Index(script[start:], "\n}\n")
		if endOffset < 0 {
			t.Fatalf("node page function %q has no closing boundary", signature)
		}
		return script[start : start+endOffset]
	}
	for _, signature := range []string{
		"async function delNode(uri, button)",
		"async function batchDeleteSelectedNodes()",
		"async function saveProxySubscription()",
		"async function refreshProxySubscription(id, button)",
		"async function deleteProxySubscription(id, button)",
	} {
		if !strings.Contains(functionBody(signature), "refreshSubscriptions: true") {
			t.Errorf("%s must refresh subscription counts", signature)
		}
	}
	for _, signature := range []string{
		"async function testSingleNode(uri, button)",
		"async function enableNode(uri, button)",
		"async function useNode(uri, button)",
		"async function unuseNode(uri, button)",
	} {
		if strings.Contains(functionBody(signature), "refreshSubscriptions: true") {
			t.Errorf("%s must reuse the subscription cache", signature)
		}
	}
	for _, token := range []string{
		"async function dedupNodes() { await API.nodes.dedup(); await loadNodes({ refreshSubscriptions: true });",
		"async function deleteDisabledNodes() { await API.nodes.deleteDisabled(); await loadNodes({ refreshSubscriptions: true });",
	} {
		if !strings.Contains(script, token) {
			t.Errorf("subscription-count-changing action %q is not refreshing the cache", token)
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
		"set_claude_prompt_rule_from_",
		"set_claude_prompt_rule_to_",
		"set_claude_prompt_rule_action_",
		"set_claude_prompt_rule_models_",
		"addClaudeReplacementRule",
		"moveClaudeReplacementRule",
		"removeClaudeReplacementRule",
		"rule_match_counts",
		"set_claude_prompt_strip_claude_code_promotions",
		"set_claude_prompt_replace_security_preamble",
		"security_preamble_replacement_count",
		"set_claude_prompt_injection_enabled",
		"set_claude_prompt_injection_position",
		"set_claude_prompt_injection_text",
		"useLatestClaudePromptAsFind",
		"useLatestClaudeModelForRule",
		"previewClaudePrompt",
		"claudePromptLatestEndpoint",
		"loadLatestClaudePrompt",
	} {
		if !strings.Contains(script, token) {
			t.Errorf("Claude prompt settings token %q is missing", token)
		}
	}
	useLatestStart := strings.Index(script, "function useLatestClaudePromptAsFind()")
	useLatestEnd := strings.Index(script, "function useLatestClaudeModelForRule(")
	if useLatestStart < 0 || useLatestEnd <= useLatestStart {
		t.Fatal("Claude latest-prompt rule creation function is missing")
	}
	useLatestBlock := script[useLatestStart:useLatestEnd]
	if !strings.Contains(useLatestBlock, "models: []") ||
		strings.Contains(useLatestBlock, "[latestClaudePrompt.model]") {
		t.Fatal("one-click Claude replacement rules must default to all models")
	}

	apiScript, err := Assets.ReadFile("assets/api.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		"/api/admin/claude-prompt/latest",
		"/api/admin/claude-prompt/preview",
		"claudePrompt",
	} {
		if !strings.Contains(string(apiScript), token) {
			t.Errorf("Claude prompt API token %q is missing", token)
		}
	}
}
