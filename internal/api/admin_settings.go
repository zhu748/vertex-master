package api

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/spool"
)

//nolint:gochecknoglobals // Constant-like map of allowed settings
var adminAllowedSettings = map[string]bool{
	"max_retries": true, "max_spill_mb": true,
	"max_request_mb": true, "max_concurrent_requests": true, "max_n": true, "aggregate_stream": true,
	"drop_max_tokens": true, "proxy_url": true,
	"claude_prompt_injection_enabled":            true,
	"claude_prompt_injection_position":           true,
	"claude_prompt_injection_text":               true,
	"claude_prompt_strip_claude_code_promotions": true,
	"claude_prompt_replace_security_preamble":    true,
	"claude_prompt_promotion_targets":            true,
	"claude_prompt_security_preamble_from":       true,
	"claude_prompt_security_preamble_to":         true,
	"claude_prompt_replacement_enabled":          true,
	"claude_prompt_replacements":                 true,
	"claude_prompt_replace_from":                 true,
	"claude_prompt_replace_to":                   true,
	"request_timeout":                            true,
	"parallel_pool_enabled":                      true, "parallel_pool_size": true,
	"parallel_pool_delay_dynamic":         true,
	"parallel_pool_delay_ms":              true,
	"proxy_failover_max_attempts":         true,
	"proxy_health_check_enabled":          true,
	"proxy_health_check_interval_minutes": true,
	"proxy_health_check_batch_size":       true,
	"proxy_health_check_concurrency":      true,
	"proxy_health_check_timeout_seconds":  true,
	"keepalive_url":                       true,
	"keepalive_interval":                  true,
	"gemini_api_mode":                     true,
	"gemini_api_key":                      true,
	"gemini_api_base_url":                 true,
	"active_node_uri":                     true,
	"sticky_node_priority":                true,
	"parallel_pool_retry_enabled":         true,
	"background_image":                    true,
	"font_size":                           true,
	"font_color_type":                     true,
	"font_color":                          true,
	"custom_bg_presets":                   true,
	"debug_mode":                          true,
	"auto_refresh_logs":                   true,
}

//nolint:gochecknoglobals // Stable mapping used to explain Render-managed settings to the UI.
var adminSettingEnvironmentVariables = map[string][]string{
	"admin_password":                      {"VPROXY_ADMIN_PASSWORD"},
	"max_concurrent_requests":             {"VPROXY_MAX_CONCURRENT_REQUESTS"},
	"proxy_failover_max_attempts":         {"VPROXY_PROXY_FAILOVER_MAX_ATTEMPTS"},
	"proxy_health_check_enabled":          {"VPROXY_PROXY_HEALTH_CHECK_ENABLED"},
	"proxy_health_check_interval_minutes": {"VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES"},
	"proxy_health_check_batch_size":       {"VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE"},
	"proxy_health_check_concurrency":      {"VPROXY_PROXY_HEALTH_CHECK_CONCURRENCY"},
	"proxy_health_check_timeout_seconds":  {"VPROXY_PROXY_HEALTH_CHECK_TIMEOUT_SECONDS"},
	"keepalive_url":                       {"VPROXY_KEEPALIVE_URL", "KEEPALIVE_URL"},
	"keepalive_interval": {
		"VPROXY_KEEPALIVE_INTERVAL_SECONDS",
		"VPROXY_KEEPALIVE_INTERVAL",
		"KEEPALIVE_INTERVAL",
	},
	"gemini_api_mode": {"VPROXY_GEMINI_API_MODE"},
	"gemini_api_key":  {"VPROXY_GEMINI_API_KEY"},
}

func environmentManagedAdminSettings() map[string]string {
	managed := make(map[string]string)
	for setting, environmentVariables := range adminSettingEnvironmentVariables {
		for _, environmentVariable := range environmentVariables {
			if _, present := os.LookupEnv(environmentVariable); present &&
				(setting == "keepalive_url" || strings.TrimSpace(os.Getenv(environmentVariable)) != "") {
				managed[setting] = environmentVariable
				break
			}
		}
	}
	return managed
}

func (adm *AdminHandler) adminGetSettings(w http.ResponseWriter, _ *http.Request) {
	policy := adm.cfg.ClaudePromptPolicy()
	rules := policy.ReplacementRules
	legacyFrom := ""
	legacyTo := ""
	if len(rules) > 0 {
		legacyFrom = rules[0].From
		legacyTo = rules[0].To
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"managed_fields":                 environmentManagedAdminSettings(),
		"claude_prompt_builtin_defaults": claudePromptBuiltinDefaults(),
		"settings": map[string]any{
			"max_retries":                                adm.cfg.MaxRetries(),
			"max_spill_mb":                               adm.cfg.MaxSpillMB(),
			"max_request_mb":                             adm.cfg.MaxRequestMB(),
			"max_concurrent_requests":                    adm.cfg.MaxConcurrentRequests(),
			"max_n":                                      adm.cfg.MaxN(),
			"aggregate_stream":                           adm.cfg.AggregateStream(),
			"drop_max_tokens":                            adm.cfg.DropMaxTokens(),
			"claude_prompt_injection_enabled":            adm.cfg.ClaudePromptInjectionEnabled(),
			"claude_prompt_injection_position":           adm.cfg.ClaudePromptInjectionPosition(),
			"claude_prompt_injection_text":               adm.cfg.ClaudePromptInjectionText(),
			"claude_prompt_strip_claude_code_promotions": policy.StripPromotions,
			"claude_prompt_replace_security_preamble":    policy.ReplaceSecurity,
			"claude_prompt_promotion_targets":            policy.PromotionTargets,
			"claude_prompt_security_preamble_from":       policy.SecurityPreambleFrom,
			"claude_prompt_security_preamble_to":         policy.SecurityPreambleTo,
			"claude_prompt_replacement_enabled":          adm.cfg.ClaudePromptReplacementEnabled(),
			"claude_prompt_replacements":                 rules,
			"claude_prompt_replace_from":                 legacyFrom,
			"claude_prompt_replace_to":                   legacyTo,
			"request_timeout":                            adm.cfg.RequestTimeout(),
			"proxy_url":                                  adm.cfg.ProxyURL(), "parallel_pool_enabled": adm.cfg.ParallelPoolEnabled(), "parallel_pool_size": adm.cfg.ParallelPoolSize(), "active_node_uri": adm.cfg.ActiveNodeURI(),
			"parallel_pool_delay_dynamic":         adm.cfg.ParallelPoolDelayDynamic(),
			"parallel_pool_delay_ms":              adm.cfg.ParallelPoolDelayMs(),
			"proxy_failover_max_attempts":         adm.cfg.ProxyFailoverMaxAttempts(),
			"proxy_health_check_enabled":          adm.cfg.ProxyHealthCheckEnabled(),
			"proxy_health_check_interval_minutes": adm.cfg.ProxyHealthCheckIntervalMinutes(),
			"proxy_health_check_batch_size":       adm.cfg.ProxyHealthCheckBatchSize(),
			"proxy_health_check_concurrency":      adm.cfg.ProxyHealthCheckConcurrency(),
			"proxy_health_check_timeout_seconds":  adm.cfg.ProxyHealthCheckTimeoutSeconds(),
			"keepalive_url":                       adm.cfg.KeepAliveURL(),
			"keepalive_interval":                  adm.cfg.KeepAliveIntervalSeconds(),
			"gemini_api_mode":                     config.NormalizeGeminiAPIMode(adm.cfg.GeminiAPIMode()),
			"gemini_api_key":                      maskAPIKey(config.ResolveGeminiAPIKey(adm.cfg.GeminiAPIMode(), adm.cfg.GeminiAPIKey())),
			"gemini_api_key_builtin":              config.UsingBuiltinGeminiAPIKey(adm.cfg.GeminiAPIMode(), adm.cfg.GeminiAPIKey()),
			"gemini_api_base_url":                 adm.cfg.GeminiAPIBaseURL(),
			"sticky_node_priority":                adm.cfg.StickyNodePriority(),
			"parallel_pool_retry_enabled":         adm.cfg.ParallelPoolRetryEnabled(),
			"background_image":                    adm.cfg.BackgroundImage(),
			"font_size":                           adm.cfg.FontSize(),
			"font_color_type":                     adm.cfg.FontColorType(),
			"font_color":                          adm.cfg.FontColor(),
			"custom_bg_presets":                   adm.cfg.CustomBgPresets(),
			"debug_mode":                          adm.cfg.DebugMode(),
			"auto_refresh_logs":                   adm.cfg.AutoRefreshLogs(),
		},
	})
}

func (adm *AdminHandler) adminPutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	managed := environmentManagedAdminSettings()
	for key := range body.Settings {
		if environmentVariable, ok := managed[key]; ok {
			writeJSON(
				w,
				http.StatusConflict,
				adminErr(fmt.Sprintf("%s 由环境变量 %s 托管，请在 Render 中修改", key, environmentVariable)),
			)
			return
		}
	}
	updates := map[string]any{}

	// 面板依赖校验：禁用并发池时强制禁用粘性池
	if ppEnabled, ok := body.Settings["parallel_pool_enabled"].(bool); ok && !ppEnabled {
		body.Settings["sticky_node_priority"] = false
	}

	for k, v := range body.Settings {
		if !adminAllowedSettings[k] {
			continue
		}
		switch k {
		case "max_retries", "max_spill_mb", "max_request_mb", "max_concurrent_requests", "max_n", "parallel_pool_size",
			"parallel_pool_delay_ms", "request_timeout", "proxy_failover_max_attempts",
			"proxy_health_check_interval_minutes", "proxy_health_check_batch_size",
			"proxy_health_check_concurrency", "proxy_health_check_timeout_seconds",
			"keepalive_interval":
			val, ok := adminSettingInt(v)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是整数"))
				return
			}
			if err := validateAdminProxySetting(k, val); err != nil {
				writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
				return
			}
			updates[k] = val
			continue
		case "aggregate_stream", "drop_max_tokens",
			"parallel_pool_enabled", "parallel_pool_delay_dynamic",
			"proxy_health_check_enabled", "sticky_node_priority",
			"parallel_pool_retry_enabled", "debug_mode", "auto_refresh_logs",
			"claude_prompt_injection_enabled", "claude_prompt_replacement_enabled",
			"claude_prompt_strip_claude_code_promotions",
			"claude_prompt_replace_security_preamble":
			if _, ok := v.(bool); !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是布尔值"))
				return
			}
		case "claude_prompt_injection_position":
			position, ok := v.(string)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
			if position != "prepend" && position != "append" {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是 prepend 或 append"))
				return
			}
		case "claude_prompt_injection_text", "claude_prompt_replace_from",
			"claude_prompt_replace_to", "claude_prompt_security_preamble_from",
			"claude_prompt_security_preamble_to":
			text, ok := v.(string)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
			if len(text) > maxClaudePromptSettingBytes {
				writeJSON(
					w,
					http.StatusBadRequest,
					adminErr(fmt.Sprintf("%s 不能超过 %d 字节", k, maxClaudePromptSettingBytes)),
				)
				return
			}
		case "claude_prompt_replacements":
			rules, err := parseAdminClaudePromptReplacementRules(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
				return
			}
			updates[k] = rules
			continue
		case "claude_prompt_promotion_targets":
			targets, err := parseAdminClaudePromptPromotionTargets(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
				return
			}
			updates[k] = targets
			continue
		case "keepalive_url":
			value, ok := v.(string)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
			value = strings.TrimSpace(value)
			if err := config.ValidateKeepAliveURL(value); err != nil {
				writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
				return
			}
			updates[k] = value
			continue
		case "proxy_url", "active_node_uri", "background_image", "font_size",
			"font_color_type", "font_color":
			if _, ok := v.(string); !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
		case "gemini_api_base_url":
			baseURL, ok := v.(string)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
			baseURL = strings.TrimSpace(baseURL)
			if baseURL != "" && !strings.HasPrefix(baseURL, "http://") &&
				!strings.HasPrefix(baseURL, "https://") {
				writeJSON(w, http.StatusBadRequest, adminErr("gemini_api_base_url 必须以 http:// 或 https:// 开头"))
				return
			}
			updates[k] = baseURL
			continue
		case "gemini_api_mode":
			// 通道开关：anon（默认，无头 Vertex 匿名端点）/ official（官方直连）。
			// 显式拒绝未知取值，避免拼写错误被静默归一化为 anon。
			value, ok := v.(string)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
			normalized := strings.ToLower(strings.TrimSpace(value))
			if normalized != "" && normalized != config.GeminiAPIModeAnon && normalized != config.GeminiAPIModeOfficial {
				writeJSON(w, http.StatusBadRequest, adminErr("gemini_api_mode 仅支持 anon（匿名端点）或 official（官方直连）"))
				return
			}
			if normalized == "" {
				normalized = config.GeminiAPIModeAnon
			}
			updates[k] = normalized
			continue
		case "gemini_api_key":
			// GET 返回脱敏值；保存时若值仍是掩码（含 '*'）视为未修改，
			// 避免把掩码当成新密钥写回。API Key 不含 '*'，不会误判。
			value, ok := v.(string)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
			value = strings.TrimSpace(value)
			if strings.Contains(value, "*") {
				continue
			}
			if len(value) > maxGeminiAPIKeyBytes {
				writeJSON(w, http.StatusBadRequest, adminErr(
					fmt.Sprintf("gemini_api_key 不能超过 %d 字节", maxGeminiAPIKeyBytes),
				))
				return
			}
			updates[k] = value
			continue
		case "custom_bg_presets":
			values, ok := v.([]any)
			if !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串数组"))
				return
			}
			converted := make([]string, 0, len(values))
			for _, item := range values {
				value, ok := item.(string)
				if !ok {
					writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串数组"))
					return
				}
				converted = append(converted, value)
			}
			updates[k] = converted
			continue
		}
		updates[k] = v
	}
	currentClaudePolicy := adm.cfg.ClaudePromptPolicy()
	claudePromotionTargets := append([]string(nil), currentClaudePolicy.PromotionTargets...)
	if value, updated := updates["claude_prompt_promotion_targets"]; updated {
		if value == nil {
			claudePromotionTargets = nil
		} else {
			claudePromotionTargets = value.([]string)
		}
	}
	claudeSecurityPreambleFrom := currentClaudePolicy.SecurityPreambleFrom
	if value, updated := updates["claude_prompt_security_preamble_from"].(string); updated {
		claudeSecurityPreambleFrom = value
	}
	claudeSecurityPreambleTo := currentClaudePolicy.SecurityPreambleTo
	if value, updated := updates["claude_prompt_security_preamble_to"].(string); updated {
		claudeSecurityPreambleTo = value
	}
	claudeReplacementEnabled := currentClaudePolicy.ReplacementEnabled
	if value, ok := updates["claude_prompt_replacement_enabled"].(bool); ok {
		claudeReplacementEnabled = value
	}
	currentClaudeReplacementRules := currentClaudePolicy.ReplacementRules
	claudeReplacementRules := append(
		[]config.ClaudePromptReplacementRule(nil),
		currentClaudeReplacementRules...,
	)
	claudeReplacementRulesUpdated := false
	if value, ok := updates["claude_prompt_replacements"].([]config.ClaudePromptReplacementRule); ok {
		claudeReplacementRules = value
		claudeReplacementRulesUpdated = true
	} else {
		legacyFrom, fromUpdated := updates["claude_prompt_replace_from"].(string)
		legacyTo, toUpdated := updates["claude_prompt_replace_to"].(string)
		if fromUpdated || toUpdated {
			claudeReplacementRulesUpdated = true
			if len(claudeReplacementRules) > 0 {
				if !fromUpdated {
					legacyFrom = claudeReplacementRules[0].From
				}
				if !toUpdated {
					legacyTo = claudeReplacementRules[0].To
				}
				if legacyFrom == "" {
					claudeReplacementRules = claudeReplacementRules[1:]
				} else {
					updatedRule := claudeReplacementRules[0]
					updatedRule.From = legacyFrom
					updatedRule.To = legacyTo
					claudeReplacementRules[0] = updatedRule
				}
			} else if legacyFrom == "" {
				claudeReplacementRules = []config.ClaudePromptReplacementRule{}
			} else {
				claudeReplacementRules = []config.ClaudePromptReplacementRule{{
					From: legacyFrom,
					To:   legacyTo,
				}}
			}
			updates["claude_prompt_replacements"] = claudeReplacementRules
		}
	}
	if claudeReplacementRulesUpdated {
		if err := validateAdminClaudePromptReplacementRules(claudeReplacementRules); err != nil {
			writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
			return
		}
		updates["claude_prompt_replacements"] = claudeReplacementRules
		if len(claudeReplacementRules) == 0 || claudeReplacementRules[0].Disabled ||
			len(claudeReplacementRules[0].Models) > 0 {
			updates["claude_prompt_replace_from"] = ""
			updates["claude_prompt_replace_to"] = ""
		} else {
			updates["claude_prompt_replace_from"] = claudeReplacementRules[0].From
			updates["claude_prompt_replace_to"] = claudeReplacementRules[0].To
		}
	}
	activeClaudeReplacementRules := 0
	for _, rule := range claudeReplacementRules {
		if !rule.Disabled {
			activeClaudeReplacementRules++
		}
	}
	if claudeReplacementEnabled && activeClaudeReplacementRules == 0 {
		writeJSON(
			w,
			http.StatusBadRequest,
			adminErr("启用 Claude 提示词替换时，至少需要一条规则且该规则已启用"),
		)
		return
	}
	claudeInjectionEnabled := currentClaudePolicy.InjectionEnabled
	if value, ok := updates["claude_prompt_injection_enabled"].(bool); ok {
		claudeInjectionEnabled = value
	}
	claudeInjectionText := currentClaudePolicy.InjectionText
	if value, ok := updates["claude_prompt_injection_text"].(string); ok {
		claudeInjectionText = value
	}
	if claudeInjectionEnabled && strings.TrimSpace(claudeInjectionText) == "" {
		writeJSON(
			w,
			http.StatusBadRequest,
			adminErr("启用 Claude 系统提示词注入时，注入内容不能为空"),
		)
		return
	}
	nextClaudePolicy := currentClaudePolicy
	nextClaudePolicy.PromotionTargets = claudePromotionTargets
	nextClaudePolicy.SecurityPreambleFrom = claudeSecurityPreambleFrom
	nextClaudePolicy.SecurityPreambleTo = claudeSecurityPreambleTo
	nextClaudePolicy.ReplacementEnabled = claudeReplacementEnabled
	nextClaudePolicy.ReplacementRules = claudeReplacementRules
	nextClaudePolicy.InjectionEnabled = claudeInjectionEnabled
	nextClaudePolicy.InjectionText = claudeInjectionText
	if value, updated := updates["claude_prompt_injection_position"].(string); updated {
		nextClaudePolicy.InjectionPosition = value
	}
	if err := config.ValidateClaudePromptPolicyConfig(nextClaudePolicy); err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
		return
	}
	poolSize := adm.cfg.ParallelPoolSize()
	if value, ok := updates["parallel_pool_size"].(int); ok {
		poolSize = value
	}
	failoverAttempts := adm.cfg.ProxyFailoverMaxAttempts()
	if value, ok := updates["proxy_failover_max_attempts"].(int); ok {
		failoverAttempts = value
	}
	if failoverAttempts < poolSize {
		writeJSON(
			w,
			http.StatusBadRequest,
			adminErr("单请求最多尝试代理不能小于最大同时并发"),
		)
		return
	}
	healthInterval := adm.cfg.ProxyHealthCheckIntervalMinutes()
	if value, ok := updates["proxy_health_check_interval_minutes"].(int); ok {
		healthInterval = value
	}
	healthBatch := adm.cfg.ProxyHealthCheckBatchSize()
	if value, ok := updates["proxy_health_check_batch_size"].(int); ok {
		healthBatch = value
	}
	healthConcurrency := adm.cfg.ProxyHealthCheckConcurrency()
	if value, ok := updates["proxy_health_check_concurrency"].(int); ok {
		healthConcurrency = value
	}
	healthTimeout := adm.cfg.ProxyHealthCheckTimeoutSeconds()
	if value, ok := updates["proxy_health_check_timeout_seconds"].(int); ok {
		healthTimeout = value
	}
	maximumHealthBatch := (healthInterval * 60 / healthTimeout) * healthConcurrency
	if maximumHealthBatch < 1 {
		maximumHealthBatch = 1
	}
	maximumHealthBatch = min(maximumHealthBatch, 500)
	if healthBatch > maximumHealthBatch {
		writeJSON(
			w,
			http.StatusBadRequest,
			adminErr(fmt.Sprintf(
				"当前巡检间隔、并发和超时下，单轮节点数最多为 %d",
				maximumHealthBatch,
			)),
		)
		return
	}
	promptPolicyChanged := !reflect.DeepEqual(currentClaudeReplacementRules, claudeReplacementRules) ||
		!reflect.DeepEqual(currentClaudePolicy.PromotionTargets, claudePromotionTargets) ||
		currentClaudePolicy.SecurityPreambleFrom != claudeSecurityPreambleFrom ||
		currentClaudePolicy.SecurityPreambleTo != claudeSecurityPreambleTo
	if value, ok := updates["claude_prompt_replacement_enabled"].(bool); ok {
		promptPolicyChanged = promptPolicyChanged || value != currentClaudePolicy.ReplacementEnabled
	}
	if value, ok := updates["claude_prompt_strip_claude_code_promotions"].(bool); ok {
		promptPolicyChanged = promptPolicyChanged || value != currentClaudePolicy.StripPromotions
	}
	if value, ok := updates["claude_prompt_replace_security_preamble"].(bool); ok {
		promptPolicyChanged = promptPolicyChanged || value != currentClaudePolicy.ReplaceSecurity
	}
	if value, ok := updates["claude_prompt_injection_enabled"].(bool); ok {
		promptPolicyChanged = promptPolicyChanged || value != currentClaudePolicy.InjectionEnabled
	}
	if value, ok := updates["claude_prompt_injection_position"].(string); ok {
		promptPolicyChanged = promptPolicyChanged || value != currentClaudePolicy.InjectionPosition
	}
	if value, ok := updates["claude_prompt_injection_text"].(string); ok {
		promptPolicyChanged = promptPolicyChanged || value != currentClaudePolicy.InjectionText
	}
	if err := config.WriteSettings(updates); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("写入配置失败 (failed to write config)"))
		return
	}
	if maxSpillMB, ok := updates["max_spill_mb"].(int); ok {
		spool.SetMaxSpillBytes(int64(maxSpillMB) << 20)
	}
	if promptPolicyChanged {
		adm.claudePrompts.Clear("all")
	}
	if adm.keepAliveReload != nil {
		_, urlChanged := updates["keepalive_url"]
		_, intervalChanged := updates["keepalive_interval"]
		if urlChanged || intervalChanged {
			adm.keepAliveReload()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func parseAdminClaudePromptReplacementRules(
	value any,
) ([]config.ClaudePromptReplacementRule, error) {
	rawRules, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("claude_prompt_replacements 必须是规则数组")
	}
	rules := make([]config.ClaudePromptReplacementRule, 0, len(rawRules))
	for index, raw := range rawRules {
		rule, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("第 %d 条 Claude 提示词替换规则必须是对象", index+1)
		}
		from, fromOK := rule["from"].(string)
		to, toOK := rule["to"].(string)
		if !fromOK || !toOK {
			return nil, fmt.Errorf("第 %d 条 Claude 提示词替换规则的 from/to 必须是字符串", index+1)
		}
		disabled := false
		if rawDisabled, present := rule["disabled"]; present {
			var disabledOK bool
			disabled, disabledOK = rawDisabled.(bool)
			if !disabledOK {
				return nil, fmt.Errorf("第 %d 条 Claude 提示词替换规则的 disabled 必须是布尔值", index+1)
			}
		}
		models := []string(nil)
		if rawModels, present := rule["models"]; present {
			items, modelsOK := rawModels.([]any)
			if !modelsOK {
				return nil, fmt.Errorf("第 %d 条 Claude 提示词替换规则的 models 必须是字符串数组", index+1)
			}
			if len(items) > 0 {
				models = make([]string, 0, len(items))
			}
			for _, item := range items {
				model, modelOK := item.(string)
				if !modelOK {
					return nil, fmt.Errorf("第 %d 条 Claude 提示词替换规则的 models 必须是字符串数组", index+1)
				}
				models = append(models, strings.TrimSpace(model))
			}
		}
		rules = append(rules, config.ClaudePromptReplacementRule{
			From: from, To: to, Disabled: disabled, Models: models,
		})
	}
	return rules, validateAdminClaudePromptReplacementRules(rules)
}

// parseAdminClaudePromptPromotionTargets accepts nil to restore the maintained
// built-in signatures. A non-nil list is an explicit exact-delete override.
func parseAdminClaudePromptPromotionTargets(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	rawTargets, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("claude_prompt_promotion_targets 必须是字符串数组或 null")
	}
	if len(rawTargets) == 0 {
		return nil, fmt.Errorf("至少需要一条 Claude Code 推广提示精确匹配内容")
	}
	if len(rawTargets) > config.MaxClaudePromptPromotionTargets {
		return nil, fmt.Errorf(
			"内置 Claude Code 推广提示不能超过 %d 条",
			config.MaxClaudePromptPromotionTargets,
		)
	}
	targets := make([]string, 0, len(rawTargets))
	seen := make(map[string]struct{}, len(rawTargets))
	totalBytes := 0
	for index, rawTarget := range rawTargets {
		target, ok := rawTarget.(string)
		if !ok {
			return nil, fmt.Errorf("第 %d 条 Claude Code 推广提示必须是字符串", index+1)
		}
		if target == "" {
			return nil, fmt.Errorf("第 %d 条 Claude Code 推广提示不能为空", index+1)
		}
		if _, duplicate := seen[target]; duplicate {
			return nil, fmt.Errorf("第 %d 条 Claude Code 推广提示重复", index+1)
		}
		seen[target] = struct{}{}
		totalBytes += len(target)
		if totalBytes > maxClaudePromptSettingBytes {
			return nil, fmt.Errorf("内置 Claude Code 推广提示文本总计不能超过 1 MiB")
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func validateAdminClaudePromptReplacementRules(
	rules []config.ClaudePromptReplacementRule,
) error {
	if len(rules) > maxClaudePromptReplacementRules {
		return fmt.Errorf(
			"提示词替换规则不能超过 %d 条",
			maxClaudePromptReplacementRules,
		)
	}
	seen := make(map[string]struct{}, len(rules))
	totalBytes := 0
	for index, rule := range rules {
		if rule.From == "" {
			return fmt.Errorf("第 %d 条 Claude 提示词替换规则的查找内容不能为空", index+1)
		}
		if _, duplicate := seen[rule.From]; duplicate {
			return fmt.Errorf("第 %d 条 Claude 提示词替换规则的查找内容重复", index+1)
		}
		seen[rule.From] = struct{}{}
		totalBytes += len(rule.From) + len(rule.To)
		if len(rule.Models) > maxClaudePromptRuleModels {
			return fmt.Errorf("第 %d 条 Claude 提示词替换规则的模型不能超过 %d 个", index+1, maxClaudePromptRuleModels)
		}
		seenModels := make(map[string]struct{}, len(rule.Models))
		for _, model := range rule.Models {
			trimmedModel := strings.TrimSpace(model)
			if trimmedModel == "" {
				return fmt.Errorf("第 %d 条 Claude 提示词替换规则包含空模型名", index+1)
			}
			normalized := strings.ToLower(trimmedModel)
			if _, duplicate := seenModels[normalized]; duplicate {
				return fmt.Errorf("第 %d 条 Claude 提示词替换规则包含重复模型", index+1)
			}
			seenModels[normalized] = struct{}{}
			totalBytes += len(trimmedModel)
		}
		if totalBytes > maxClaudePromptSettingBytes {
			return fmt.Errorf("提示词替换规则文本总计不能超过 1 MiB")
		}
	}
	return nil
}

func adminSettingInt(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return 0, false
	}
	limit := float64(uint64(1) << (strconv.IntSize - 1))
	if number < -limit || number >= limit {
		return 0, false
	}
	return int(number), true
}

func validateAdminProxySetting(key string, value int) error {
	bounds := map[string][2]int{
		"max_retries":                         {0, 10},
		"max_spill_mb":                        {1, 8192},
		"max_n":                               {1, 32},
		"parallel_pool_size":                  {1, 20},
		"parallel_pool_delay_ms":              {100, 10000},
		"max_request_mb":                      {1, 1024},
		"max_concurrent_requests":             {1, 1000},
		"request_timeout":                     {1, 1800},
		"proxy_failover_max_attempts":         {1, 100},
		"proxy_health_check_interval_minutes": {1, 1440},
		"proxy_health_check_batch_size":       {1, 500},
		"proxy_health_check_concurrency":      {1, 20},
		"proxy_health_check_timeout_seconds":  {2, 60},
		"keepalive_interval":                  {config.KeepAliveMinIntervalSeconds, config.KeepAliveMaxIntervalSeconds},
	}
	bound, constrained := bounds[key]
	if !constrained {
		return nil
	}
	if value < bound[0] || value > bound[1] {
		return fmt.Errorf("%s 必须在 %d 到 %d 之间", key, bound[0], bound[1])
	}
	return nil
}

func (adm *AdminHandler) adminGetStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, metricsBody(adm.vc, adm.requests))
}

func (adm *AdminHandler) adminResetStats(w http.ResponseWriter, _ *http.Request) {
	adm.vc.ResetCountTokenStats()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// maxGeminiAPIKeyBytes 限制单 API Key 的最大长度（密钥远小于该值，防误粘大段文本）。
const maxGeminiAPIKeyBytes = 512

// maskAPIKey 对单 API Key 脱敏展示：保留前 5 后 4 字符，中间以 **** 代替。
// 过短或为空的密钥原样/按规则处理，保证 GET 返回值永远不含完整密钥。
func maskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= 9 {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:5]) + "****" + string(runes[len(runes)-4:])
}
