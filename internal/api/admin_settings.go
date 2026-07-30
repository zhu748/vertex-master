package api

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"reflect"
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
var adminSettingEnvironmentVariables = map[string]string{
	"admin_password":                      "VPROXY_ADMIN_PASSWORD",
	"max_concurrent_requests":             "VPROXY_MAX_CONCURRENT_REQUESTS",
	"proxy_failover_max_attempts":         "VPROXY_PROXY_FAILOVER_MAX_ATTEMPTS",
	"proxy_health_check_enabled":          "VPROXY_PROXY_HEALTH_CHECK_ENABLED",
	"proxy_health_check_interval_minutes": "VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES",
	"proxy_health_check_batch_size":       "VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE",
	"proxy_health_check_concurrency":      "VPROXY_PROXY_HEALTH_CHECK_CONCURRENCY",
	"proxy_health_check_timeout_seconds":  "VPROXY_PROXY_HEALTH_CHECK_TIMEOUT_SECONDS",
}

func environmentManagedAdminSettings() map[string]string {
	managed := make(map[string]string)
	for setting, environmentVariable := range adminSettingEnvironmentVariables {
		if strings.TrimSpace(os.Getenv(environmentVariable)) != "" {
			managed[setting] = environmentVariable
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
		"managed_fields": environmentManagedAdminSettings(),
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
			"proxy_health_check_concurrency", "proxy_health_check_timeout_seconds":
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
			"claude_prompt_replace_to":
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
		case "proxy_url", "active_node_uri", "background_image", "font_size",
			"font_color_type", "font_color":
			if _, ok := v.(string); !ok {
				writeJSON(w, http.StatusBadRequest, adminErr(k+" 必须是字符串"))
				return
			}
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
	promptPolicyChanged := !reflect.DeepEqual(currentClaudeReplacementRules, claudeReplacementRules)
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
	if number > float64(math.MaxInt) || number < float64(math.MinInt) {
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
	writeJSON(w, http.StatusOK, metricsBody(adm.vc))
}

func (adm *AdminHandler) adminResetStats(w http.ResponseWriter, _ *http.Request) {
	adm.vc.ResetCountTokenStats()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
