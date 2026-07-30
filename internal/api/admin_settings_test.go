package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/spool"
)

func TestAdminPutSettingsRejectsInvalidProxySettings(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			name:        "fractional integer",
			body:        `{"settings":{"proxy_health_check_batch_size":12.5}}`,
			wantMessage: "proxy_health_check_batch_size 必须是整数",
		},
		{
			name:        "wrong integer type",
			body:        `{"settings":{"proxy_failover_max_attempts":"30"}}`,
			wantMessage: "proxy_failover_max_attempts 必须是整数",
		},
		{
			name:        "wrong bool type",
			body:        `{"settings":{"proxy_health_check_enabled":"true"}}`,
			wantMessage: "proxy_health_check_enabled 必须是布尔值",
		},
		{
			name:        "wrong string type",
			body:        `{"settings":{"proxy_url":123}}`,
			wantMessage: "proxy_url 必须是字符串",
		},
		{
			name:        "interval below minimum",
			body:        `{"settings":{"proxy_health_check_interval_minutes":0}}`,
			wantMessage: "proxy_health_check_interval_minutes 必须在 1 到 1440 之间",
		},
		{
			name:        "batch above maximum",
			body:        `{"settings":{"proxy_health_check_batch_size":501}}`,
			wantMessage: "proxy_health_check_batch_size 必须在 1 到 500 之间",
		},
		{
			name:        "timeout below minimum",
			body:        `{"settings":{"proxy_health_check_timeout_seconds":1}}`,
			wantMessage: "proxy_health_check_timeout_seconds 必须在 2 到 60 之间",
		},
		{
			name:        "retry amplification",
			body:        `{"settings":{"max_retries":11}}`,
			wantMessage: "max_retries 必须在 0 到 10 之间",
		},
		{
			name:        "unbounded memory spool",
			body:        `{"settings":{"max_spill_mb":0}}`,
			wantMessage: "max_spill_mb 必须在 1 到 8192 之间",
		},
		{
			name:        "candidate amplification",
			body:        `{"settings":{"max_n":33}}`,
			wantMessage: "max_n 必须在 1 到 32 之间",
		},
		{
			name:        "invalid Claude injection position",
			body:        `{"settings":{"claude_prompt_injection_position":"middle"}}`,
			wantMessage: "claude_prompt_injection_position 必须是 prepend 或 append",
		},
		{
			name:        "Claude replacement missing search text",
			body:        `{"settings":{"claude_prompt_replacement_enabled":true}}`,
			wantMessage: "至少需要一条规则",
		},
		{
			name:        "Claude promotion removal wrong type",
			body:        `{"settings":{"claude_prompt_strip_claude_code_promotions":"true"}}`,
			wantMessage: "claude_prompt_strip_claude_code_promotions 必须是布尔值",
		},
		{
			name:        "Claude security preamble replacement wrong type",
			body:        `{"settings":{"claude_prompt_replace_security_preamble":"true"}}`,
			wantMessage: "claude_prompt_replace_security_preamble 必须是布尔值",
		},
		{
			name:        "Claude replacement rule missing search text",
			body:        `{"settings":{"claude_prompt_replacements":[{"from":"","to":"value"}]}}`,
			wantMessage: "查找内容不能为空",
		},
		{
			name:        "duplicate Claude replacement rule",
			body:        `{"settings":{"claude_prompt_replacements":[{"from":"same","to":"one"},{"from":"same","to":"two"}]}}`,
			wantMessage: "查找内容重复",
		},
		{
			name:        "Claude replacement models wrong type",
			body:        `{"settings":{"claude_prompt_replacements":[{"from":"same","to":"one","models":"fake-model"}]}}`,
			wantMessage: "models 必须是字符串数组",
		},
		{
			name:        "Claude replacement disabled wrong type",
			body:        `{"settings":{"claude_prompt_replacements":[{"from":"same","to":"one","disabled":"false"}]}}`,
			wantMessage: "disabled 必须是布尔值",
		},
		{
			name:        "Claude replacement duplicate model",
			body:        `{"settings":{"claude_prompt_replacements":[{"from":"same","to":"one","models":["Fake-Model","fake-model"]}]}}`,
			wantMessage: "包含重复模型",
		},
		{
			name:        "Claude replacement all disabled",
			body:        `{"settings":{"claude_prompt_replacement_enabled":true,"claude_prompt_replacements":[{"from":"same","to":"one","disabled":true}]}}`,
			wantMessage: "该规则已启用",
		},
		{
			name:        "Claude injection missing prompt",
			body:        `{"settings":{"claude_prompt_injection_enabled":true,"claude_prompt_injection_text":"  "}}`,
			wantMessage: "注入内容不能为空",
		},
		{
			name:        "failover below concurrency",
			body:        `{"settings":{"parallel_pool_size":8,"proxy_failover_max_attempts":7}}`,
			wantMessage: "单请求最多尝试代理不能小于最大同时并发",
		},
		{
			name:        "health workload exceeds interval",
			body:        `{"settings":{"proxy_health_check_interval_minutes":1,"proxy_health_check_batch_size":500,"proxy_health_check_concurrency":1,"proxy_health_check_timeout_seconds":60}}`,
			wantMessage: "单轮节点数最多为 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			t.Setenv("VPROXY_CONFIG", path)
			config.InvalidateCache()
			t.Cleanup(config.InvalidateCache)
			adm := newAdminSettingsTestHandler()
			req := httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()

			adm.adminPutSettings(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应拒绝非法设置，status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantMessage) {
				t.Fatalf("错误信息不匹配：body=%s want=%q", rec.Body.String(), tt.wantMessage)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("非法设置不应写配置文件，stat err=%v", err)
			}
		})
	}
}

func TestAdminSettingIntRejectsPlatformOverflow(t *testing.T) {
	if value, ok := adminSettingInt(float64(math.MaxInt) + 1); ok || value != 0 {
		t.Fatalf("overflowing admin integer=(%d,%v), want (0,false)", value, ok)
	}
}

func TestAdminPutSettingsAcceptsValidProxySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	adm := newAdminSettingsTestHandler()
	body := `{"settings":{
		"proxy_url":"http://127.0.0.1:8080",
		"parallel_pool_enabled":true,
		"parallel_pool_size":6,
		"parallel_pool_delay_dynamic":false,
		"parallel_pool_delay_ms":1250,
		"proxy_failover_max_attempts":24,
		"proxy_health_check_enabled":true,
		"proxy_health_check_interval_minutes":30,
		"proxy_health_check_batch_size":100,
		"proxy_health_check_concurrency":8,
		"proxy_health_check_timeout_seconds":12,
		"sticky_node_priority":true,
		"parallel_pool_retry_enabled":true,
		"claude_prompt_injection_enabled":true,
		"claude_prompt_injection_position":"prepend",
		"claude_prompt_injection_text":"injected policy",
		"claude_prompt_strip_claude_code_promotions":false,
		"claude_prompt_replace_security_preamble":false,
		"claude_prompt_replacement_enabled":true,
		"claude_prompt_replacements":[
			{"from":"old policy","to":"new policy","models":["fake-gemini-3.6-flash"]},
			{"from":"second section","to":"updated section","disabled":true}
		]
	}}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	adm.adminPutSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("合法代理设置应被接受，status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"proxy_url":                                  "http://127.0.0.1:8080",
		"parallel_pool_enabled":                      true,
		"parallel_pool_size":                         float64(6),
		"parallel_pool_delay_dynamic":                false,
		"parallel_pool_delay_ms":                     float64(1250),
		"proxy_failover_max_attempts":                float64(24),
		"proxy_health_check_enabled":                 true,
		"proxy_health_check_interval_minutes":        float64(30),
		"proxy_health_check_batch_size":              float64(100),
		"proxy_health_check_concurrency":             float64(8),
		"proxy_health_check_timeout_seconds":         float64(12),
		"sticky_node_priority":                       true,
		"parallel_pool_retry_enabled":                true,
		"claude_prompt_injection_enabled":            true,
		"claude_prompt_injection_position":           "prepend",
		"claude_prompt_injection_text":               "injected policy",
		"claude_prompt_strip_claude_code_promotions": false,
		"claude_prompt_replace_security_preamble":    false,
		"claude_prompt_replacement_enabled":          true,
		"claude_prompt_replace_from":                 "",
		"claude_prompt_replace_to":                   "",
	}
	for key, want := range expected {
		if got := raw[key]; got != want {
			t.Errorf("%s 写盘值错误：got=%v want=%v", key, got, want)
		}
	}
	rawRules, ok := raw["claude_prompt_replacements"].([]any)
	if !ok || len(rawRules) != 2 {
		t.Fatalf("Claude replacement rules were not persisted: %#v", raw["claude_prompt_replacements"])
	}
	firstRule, _ := rawRules[0].(map[string]any)
	secondRule, _ := rawRules[1].(map[string]any)
	if firstRule["from"] != "old policy" || firstRule["to"] != "new policy" ||
		secondRule["from"] != "second section" || secondRule["to"] != "updated section" ||
		secondRule["disabled"] != true {
		t.Fatalf("persisted Claude replacement rules are invalid: %#v", rawRules)
	}
	firstModels, _ := firstRule["models"].([]any)
	if len(firstModels) != 1 || firstModels[0] != "fake-gemini-3.6-flash" {
		t.Fatalf("persisted Claude replacement model scope is invalid: %#v", firstRule)
	}
}

func TestAdminPutSettingsClearsStaleLegacyClaudePromptRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	if err := os.WriteFile(path, []byte(`{
		"claude_prompt_replace_from":"stale source",
		"claude_prompt_replace_to":"stale target"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	adm := newAdminSettingsTestHandler()
	recorder := httptest.NewRecorder()
	adm.adminPutSettings(
		recorder,
		httptest.NewRequest(
			http.MethodPut,
			"/api/admin/settings",
			bytes.NewBufferString(`{"settings":{"claude_prompt_replacements":[]}}`),
		),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear rules status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var raw map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["claude_prompt_replace_from"] != "" || raw["claude_prompt_replace_to"] != "" {
		t.Fatalf("stale legacy fields were not cleared: %#v", raw)
	}
}

func TestAdminPutSettingsSynchronizesLegacyClaudePromptRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	if err := os.WriteFile(path, []byte(`{
		"claude_prompt_replace_from":"stale source",
		"claude_prompt_replace_to":"stale target"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	adm := newAdminSettingsTestHandler()
	recorder := httptest.NewRecorder()
	adm.adminPutSettings(
		recorder,
		httptest.NewRequest(
			http.MethodPut,
			"/api/admin/settings",
			bytes.NewBufferString(`{"settings":{"claude_prompt_replacements":[{"from":"current source","to":"current target"}]}}`),
		),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save rules status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var raw map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["claude_prompt_replace_from"] != "current source" ||
		raw["claude_prompt_replace_to"] != "current target" {
		t.Fatalf("legacy fields were not synchronized: %#v", raw)
	}
}

func TestAdminPutSettingsClearsRecentPromptsOnlyWhenPolicyChanges(t *testing.T) {
	for _, test := range []struct {
		name        string
		to          string
		wantCleared bool
	}{
		{name: "unchanged", to: "current target", wantCleared: false},
		{name: "changed", to: "new target", wantCleared: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			t.Setenv("VPROXY_CONFIG", path)
			config.InvalidateCache()
			t.Cleanup(config.InvalidateCache)
			cfg := config.DefaultConfig()
			cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{{
				From: "current source",
				To:   "current target",
			}}
			store := &claudePromptStore{}
			store.Record("model", "messages", claudePromptPolicyResult{OriginalPrompt: "generate"})
			store.Record("model", "count_tokens", claudePromptPolicyResult{OriginalPrompt: "count"})
			adm := &AdminHandler{
				handler:       handler{cfg: config.StaticProvider(cfg)},
				claudePrompts: store,
			} //nolint:exhaustruct
			body := fmt.Sprintf(
				`{"settings":{"claude_prompt_replacements":[{"from":"current source","to":%q}]}}`,
				test.to,
			)
			recorder := httptest.NewRecorder()
			adm.adminPutSettings(
				recorder,
				httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewBufferString(body)),
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("save status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			_, messagesAvailable := store.Latest("messages")
			_, countAvailable := store.Latest("count_tokens")
			wantAvailable := !test.wantCleared
			if messagesAvailable != wantAvailable || countAvailable != wantAvailable {
				t.Fatalf("unexpected records after save: messages=%v count=%v wantCleared=%v",
					messagesAvailable, countAvailable, test.wantCleared)
			}
		})
	}
}

func TestAdminPutSettingsMigratesLegacyClaudePromptRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	adm := newAdminSettingsTestHandler()
	rec := httptest.NewRecorder()

	adm.adminPutSettings(
		rec,
		httptest.NewRequest(
			http.MethodPut,
			"/api/admin/settings",
			bytes.NewBufferString(`{"settings":{"claude_prompt_replacement_enabled":true,"claude_prompt_replace_from":"legacy","claude_prompt_replace_to":"modern"}}`),
		),
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("legacy Claude rule status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"claude_prompt_replacements"`) ||
		!strings.Contains(string(data), `"from": "legacy"`) {
		t.Fatalf("legacy rule was not migrated to the rule array: %s", data)
	}
}

func TestAdminPutSettingsLegacyEditPreservesAdditionalClaudeRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	cfg := config.DefaultConfig()
	cfg.ClaudePromptReplacementEnabled = true
	cfg.ClaudePromptReplacements = []config.ClaudePromptReplacementRule{
		{From: "first", To: "old", Disabled: true, Models: []string{"fake-model"}},
		{From: "second", To: "keep"},
	}
	adm := &AdminHandler{handler: handler{cfg: config.StaticProvider(cfg)}} //nolint:exhaustruct
	rec := httptest.NewRecorder()

	adm.adminPutSettings(
		rec,
		httptest.NewRequest(
			http.MethodPut,
			"/api/admin/settings",
			bytes.NewBufferString(`{"settings":{"claude_prompt_replace_from":"first","claude_prompt_replace_to":"updated"}}`),
		),
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("legacy first-rule edit status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var rules []config.ClaudePromptReplacementRule
	if err := json.Unmarshal(raw["claude_prompt_replacements"], &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].To != "updated" || !rules[0].Disabled ||
		len(rules[0].Models) != 1 || rules[0].Models[0] != "fake-model" ||
		rules[1].From != "second" {
		t.Fatalf("legacy edit discarded multi-rule config: %#v", rules)
	}
}

func TestAdminPutSettingsRejectsOversizedClaudePromptRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	adm := newAdminSettingsTestHandler()
	body, err := json.Marshal(map[string]any{"settings": map[string]any{
		"claude_prompt_replacements": []map[string]any{{
			"from": strings.Repeat("x", maxClaudePromptSettingBytes),
			"to":   "y",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()

	adm.adminPutSettings(
		rec,
		httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(body)),
	)

	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "不能超过") {
		t.Fatalf("oversized Claude prompt status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized rule must not be persisted: %v", err)
	}
}

func TestAdminPutSettingsRejectsTooManyClaudePromptRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	adm := newAdminSettingsTestHandler()
	rules := make([]map[string]any, maxClaudePromptReplacementRules+1)
	for index := range rules {
		rules[index] = map[string]any{"from": fmt.Sprintf("from-%d", index), "to": "value"}
	}
	body, err := json.Marshal(map[string]any{"settings": map[string]any{
		"claude_prompt_replacements": rules,
	}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()

	adm.adminPutSettings(
		rec,
		httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(body)),
	)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不能超过") {
		t.Fatalf("too many Claude rules status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminPutSettingsUpdatesSpoolThresholdImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	original := spool.MaxSpillBytes()
	t.Cleanup(func() { spool.SetMaxSpillBytes(original) })

	adm := newAdminSettingsTestHandler()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/admin/settings",
		bytes.NewBufferString(`{"settings":{"max_spill_mb":123}}`),
	)
	rec := httptest.NewRecorder()
	adm.adminPutSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("更新 max_spill_mb status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := spool.MaxSpillBytes(); got != 123<<20 {
		t.Fatalf("spool threshold=%d, want %d", got, 123<<20)
	}
}

func TestAdminSettingsExposeAndRejectEnvironmentManagedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES", "45")
	config.InvalidateCache()
	t.Cleanup(config.InvalidateCache)
	adm := newAdminSettingsTestHandler()

	getRec := httptest.NewRecorder()
	adm.adminGetSettings(getRec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("get settings status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var response struct {
		ManagedFields map[string]string `json:"managed_fields"`
		Settings      map[string]any    `json:"settings"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.ManagedFields["proxy_health_check_interval_minutes"]; got != "VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES" {
		t.Fatalf("managed field metadata = %q", got)
	}
	if response.Settings["claude_prompt_injection_position"] != "append" ||
		response.Settings["claude_prompt_injection_enabled"] != false ||
		response.Settings["claude_prompt_strip_claude_code_promotions"] != true ||
		response.Settings["claude_prompt_replace_security_preamble"] != true ||
		response.Settings["claude_prompt_replacement_enabled"] != false {
		t.Fatalf("Claude prompt settings missing from admin response: %#v", response.Settings)
	}
	if rules, ok := response.Settings["claude_prompt_replacements"].([]any); !ok || len(rules) != 0 {
		t.Fatalf("default Claude prompt rules missing from admin response: %#v", response.Settings)
	}

	putRec := httptest.NewRecorder()
	adm.adminPutSettings(
		putRec,
		httptest.NewRequest(
			http.MethodPut,
			"/api/admin/settings",
			bytes.NewBufferString(`{"settings":{"proxy_health_check_interval_minutes":30}}`),
		),
	)
	if putRec.Code != http.StatusConflict ||
		!strings.Contains(putRec.Body.String(), "VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES") {
		t.Fatalf("managed update status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("managed setting must not be written locally: stat err=%v", err)
	}
}

func newAdminSettingsTestHandler() *AdminHandler {
	cfg := config.DefaultConfig()
	return &AdminHandler{handler: handler{cfg: config.StaticProvider(cfg)}} //nolint:exhaustruct
}
