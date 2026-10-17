package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAnonAPIKey          = "AIzaSyCI-zsRP85UVOi0DjtiCwWBwQ1djDy741g"
	defaultCountTokensQuerySig = "2/mENOSldfC+HZM+tGhVuJLrl8M6gEyK3HRjUKuA5AM58="
	defaultMaxRetries          = 10
	defaultParallelPoolSize    = 16
	defaultKeepAliveInterval   = 60

	// KeepAliveMinIntervalSeconds and KeepAliveMaxIntervalSeconds define the
	// supported admin/config range for the outbound keep-alive request loop.
	KeepAliveMinIntervalSeconds = 5
	KeepAliveMaxIntervalSeconds = 86400
	MaxKeepAliveURLBytes        = 4096

	// MaxClaudePromptSettingBytes bounds one configured prompt policy payload.
	MaxClaudePromptSettingBytes = 1 << 20
	// MaxClaudePromptReplacementRules bounds ordered literal rewrite work.
	MaxClaudePromptReplacementRules = 32
	// MaxClaudePromptRuleModels bounds model filters attached to one rule.
	MaxClaudePromptRuleModels = 16
	// MaxClaudePromptPromotionTargets bounds exact built-in promotion signatures.
	MaxClaudePromptPromotionTargets = 16
)

type AppConfig struct { //nolint:govet
	PortAPI                   int               `json:"port_api"`
	MaxRetries                int               `json:"max_retries"`
	AdminPassword             string            `json:"admin_password"`
	ProxyURL                  string            `json:"proxy_url"`
	AggregateStream           bool              `json:"aggregate_stream"`
	DropMaxTokens             bool              `json:"drop_max_tokens"`
	SafetySettings            map[string]string `json:"safety_settings"`
	VertexAPIKey              string            `json:"vertex_api_key"`
	CountTokensQuerySignature string            `json:"count_tokens_query_signature"`
	MaxN                      int               `json:"max_n"`
	MaxSpillMB                int               `json:"max_spill_mb"`
	MaxRequestMB              int               `json:"max_request_mb"`
	MaxConcurrentRequests     int               `json:"max_concurrent_requests"`
	StreamIdleTimeoutSeconds  int               `json:"stream_idle_timeout_seconds"`
	RequestTimeout            int               `json:"request_timeout"`
	KeepAliveURL              string            `json:"keepalive_url"`
	KeepAliveIntervalSeconds  int               `json:"keepalive_interval"`

	// Claude Messages 顶层及中途 system 提示词处理
	ClaudePromptInjectionEnabled  bool   `json:"claude_prompt_injection_enabled"`
	ClaudePromptInjectionPosition string `json:"claude_prompt_injection_position"`
	ClaudePromptInjectionText     string `json:"claude_prompt_injection_text"`
	ClaudePromptStripPromotions   bool   `json:"claude_prompt_strip_claude_code_promotions"`
	ClaudePromptReplaceSecurity   bool   `json:"claude_prompt_replace_security_preamble"`
	// Optional overrides for the two built-in Claude Code prompt rules. A nil
	// promotion target slice and empty security values keep the maintained
	// built-in signatures active, so older config files retain safe defaults.
	ClaudePromptPromotionTargets     []string                      `json:"claude_prompt_promotion_targets"`
	ClaudePromptSecurityPreambleFrom string                        `json:"claude_prompt_security_preamble_from"`
	ClaudePromptSecurityPreambleTo   string                        `json:"claude_prompt_security_preamble_to"`
	ClaudePromptReplacementEnabled   bool                          `json:"claude_prompt_replacement_enabled"`
	ClaudePromptReplacements         []ClaudePromptReplacementRule `json:"claude_prompt_replacements"`
	// Deprecated single-rule fields are retained for config-file compatibility.
	ClaudePromptReplaceFrom string `json:"claude_prompt_replace_from"`
	ClaudePromptReplaceTo   string `json:"claude_prompt_replace_to"`

	// 并发池与节点锁定配置
	ActiveNodeURI            string `json:"active_node_uri"`
	ParallelPoolEnabled      bool   `json:"parallel_pool_enabled"`
	StickyNodePriority       bool   `json:"sticky_node_priority"`
	ParallelPoolRetryEnabled bool   `json:"parallel_pool_retry_enabled"`
	ParallelPoolSize         int    `json:"parallel_pool_size"`
	DebugPprof               bool   `json:"debug_pprof"`
	ParallelNodeTopK         int    `json:"parallel_node_top_k"`
	DebugMode                bool   `json:"debug_mode"`
	ParallelPoolDelayDynamic bool   `json:"parallel_pool_delay_dynamic"`
	ParallelPoolDelayMs      int    `json:"parallel_pool_delay_ms"`
	ProxyFailoverMaxAttempts int    `json:"proxy_failover_max_attempts"`

	// 代理池后台健康巡检
	ProxyHealthCheckEnabled         bool `json:"proxy_health_check_enabled"`
	ProxyHealthCheckIntervalMinutes int  `json:"proxy_health_check_interval_minutes"`
	ProxyHealthCheckBatchSize       int  `json:"proxy_health_check_batch_size"`
	ProxyHealthCheckConcurrency     int  `json:"proxy_health_check_concurrency"`
	ProxyHealthCheckTimeoutSeconds  int  `json:"proxy_health_check_timeout_seconds"`
	// Environment-only safety escape hatch for trusted LAN subscriptions.
	AllowPrivateSubscriptionURLs        bool `json:"-"`
	AllowDomainSubscriptionProxies      bool `json:"-"`
	ProxySubscriptionAllowProxyFallback bool `json:"-"`

	// 外观配置
	BackgroundImage string   `json:"background_image"`
	FontSize        string   `json:"font_size"`
	FontColorType   string   `json:"font_color_type"`
	FontColor       string   `json:"font_color"`
	CustomBgPresets []string `json:"custom_bg_presets"`
	AutoRefreshLogs *bool    `json:"auto_refresh_logs,omitempty"`
}

type ClaudePromptReplacementRule struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Disabled bool     `json:"disabled,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// ClaudePromptPolicyConfig is an immutable per-request snapshot. Keeping the
// related values together prevents a live config reload from mixing fields
// from different revisions while a request is being converted.
type ClaudePromptPolicyConfig struct {
	InjectionEnabled     bool
	InjectionPosition    string
	InjectionText        string
	StripPromotions      bool
	ReplaceSecurity      bool
	PromotionTargets     []string
	SecurityPreambleFrom string
	SecurityPreambleTo   string
	ReplacementEnabled   bool
	ReplacementRules     []ClaudePromptReplacementRule
	MaxRequestMB         int
	validationErr        error
}

func (c AppConfig) ClaudePromptPolicy() ClaudePromptPolicyConfig {
	policy := c.ClaudePromptPolicySnapshot()
	policy.ReplacementRules = cloneClaudePromptReplacementRules(policy.ReplacementRules)
	policy.PromotionTargets = append([]string(nil), policy.PromotionTargets...)
	return policy
}

// ClaudePromptPolicySnapshot returns a read-only view backed by the immutable
// configuration snapshot. Callers must not mutate ReplacementRules or their
// nested Models slices. Use ClaudePromptPolicy when a detached copy is needed.
func (c AppConfig) ClaudePromptPolicySnapshot() ClaudePromptPolicyConfig {
	policy := ClaudePromptPolicyConfig{
		InjectionEnabled:     c.ClaudePromptInjectionEnabled,
		InjectionPosition:    normalizeClaudePromptInjectionPosition(c.ClaudePromptInjectionPosition),
		InjectionText:        c.ClaudePromptInjectionText,
		StripPromotions:      c.ClaudePromptStripPromotions,
		ReplaceSecurity:      c.ClaudePromptReplaceSecurity,
		PromotionTargets:     c.ClaudePromptPromotionTargets,
		SecurityPreambleFrom: c.ClaudePromptSecurityPreambleFrom,
		SecurityPreambleTo:   c.ClaudePromptSecurityPreambleTo,
		ReplacementEnabled:   c.ClaudePromptReplacementEnabled,
		ReplacementRules:     c.effectiveClaudePromptReplacementRulesSnapshot(),
		MaxRequestMB:         c.MaxRequestMB,
	}
	policy.validationErr = ValidateClaudePromptPolicyConfig(policy)
	return policy
}

// ValidationError returns the validation result captured with this snapshot.
func (p ClaudePromptPolicyConfig) ValidationError() error {
	return p.validationErr
}

// EffectiveClaudePromptReplacementRules returns a detached replacement rule
// slice. An explicitly configured multi-rule array takes precedence; old
// config files that only contain the legacy from/to fields keep working.
func (c AppConfig) EffectiveClaudePromptReplacementRules() []ClaudePromptReplacementRule {
	return cloneClaudePromptReplacementRules(
		c.effectiveClaudePromptReplacementRulesSnapshot(),
	)
}

func (c AppConfig) effectiveClaudePromptReplacementRulesSnapshot() []ClaudePromptReplacementRule {
	if c.ClaudePromptReplacements != nil {
		return c.ClaudePromptReplacements
	}
	if c.ClaudePromptReplaceFrom == "" {
		return []ClaudePromptReplacementRule{}
	}
	return []ClaudePromptReplacementRule{{
		From: c.ClaudePromptReplaceFrom,
		To:   c.ClaudePromptReplaceTo,
	}}
}

func cloneClaudePromptReplacementRules(
	rules []ClaudePromptReplacementRule,
) []ClaudePromptReplacementRule {
	cloned := make([]ClaudePromptReplacementRule, len(rules))
	for index := range rules {
		cloned[index] = rules[index]
		cloned[index].Models = append([]string(nil), rules[index].Models...)
	}
	return cloned
}

// ValidateClaudePromptPolicyConfig validates a complete prompt policy.
func ValidateClaudePromptPolicyConfig(policy ClaudePromptPolicyConfig) error {
	if policy.PromotionTargets != nil {
		if len(policy.PromotionTargets) == 0 {
			return fmt.Errorf("configured Claude promotion targets are empty")
		}
		if len(policy.PromotionTargets) > MaxClaudePromptPromotionTargets {
			return fmt.Errorf(
				"configured Claude promotion targets exceed the limit of %d",
				MaxClaudePromptPromotionTargets,
			)
		}
		seenPromotionTargets := make(map[string]struct{}, len(policy.PromotionTargets))
		totalPromotionBytes := 0
		for index, target := range policy.PromotionTargets {
			if target == "" {
				return fmt.Errorf("configured Claude promotion target %d is empty", index+1)
			}
			if _, duplicate := seenPromotionTargets[target]; duplicate {
				return fmt.Errorf("configured Claude promotion target %d is duplicated", index+1)
			}
			seenPromotionTargets[target] = struct{}{}
			totalPromotionBytes += len(target)
			if totalPromotionBytes > MaxClaudePromptSettingBytes {
				return fmt.Errorf("configured Claude promotion targets exceed 1 MiB")
			}
		}
	}
	if policy.SecurityPreambleFrom != "" || policy.SecurityPreambleTo != "" {
		if policy.SecurityPreambleFrom == "" || policy.SecurityPreambleTo == "" {
			return fmt.Errorf("configured Claude security preamble source and replacement must both be set")
		}
		if len(policy.SecurityPreambleFrom)+len(policy.SecurityPreambleTo) >
			MaxClaudePromptSettingBytes {
			return fmt.Errorf("configured Claude security preamble exceeds 1 MiB")
		}
	}
	if policy.ReplacementEnabled {
		if err := ValidateClaudePromptReplacementRules(policy.ReplacementRules); err != nil {
			return err
		}
		activeRules := 0
		for _, rule := range policy.ReplacementRules {
			if !rule.Disabled {
				activeRules++
			}
		}
		if activeRules == 0 {
			return fmt.Errorf("claude prompt replacement is enabled without an active rule")
		}
	}
	if policy.InjectionEnabled {
		if strings.TrimSpace(policy.InjectionText) == "" {
			return fmt.Errorf("claude prompt injection is enabled without content")
		}
		if len(policy.InjectionText) > MaxClaudePromptSettingBytes {
			return fmt.Errorf("configured Claude prompt injection exceeds 1 MiB")
		}
	}
	return nil
}

// ValidateClaudePromptReplacementRules validates ordered literal rewrite rules.
func ValidateClaudePromptReplacementRules(rules []ClaudePromptReplacementRule) error {
	if len(rules) > MaxClaudePromptReplacementRules {
		return fmt.Errorf(
			"configured Claude prompt replacement rules exceed the limit of %d",
			MaxClaudePromptReplacementRules,
		)
	}
	seen := make(map[string]struct{}, len(rules))
	totalBytes := 0
	for index, rule := range rules {
		if rule.From == "" {
			return fmt.Errorf("claude prompt replacement rule %d has an empty source", index+1)
		}
		if _, duplicate := seen[rule.From]; duplicate {
			return fmt.Errorf("claude prompt replacement rule %d has a duplicate source", index+1)
		}
		seen[rule.From] = struct{}{}
		totalBytes += len(rule.From) + len(rule.To)
		if len(rule.Models) > MaxClaudePromptRuleModels {
			return fmt.Errorf(
				"claude prompt replacement rule %d has more than %d models",
				index+1,
				MaxClaudePromptRuleModels,
			)
		}
		seenModels := make(map[string]struct{}, len(rule.Models))
		for _, model := range rule.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				return fmt.Errorf("claude prompt replacement rule %d has an empty model", index+1)
			}
			normalized := strings.ToLower(model)
			if _, duplicate := seenModels[normalized]; duplicate {
				return fmt.Errorf("claude prompt replacement rule %d has a duplicate model", index+1)
			}
			seenModels[normalized] = struct{}{}
			totalBytes += len(model)
		}
		if totalBytes > MaxClaudePromptSettingBytes {
			return fmt.Errorf("configured Claude prompt replacement rules exceed 1 MiB")
		}
	}
	return nil
}

func DefaultConfig() AppConfig {
	return AppConfig{ //nolint:exhaustruct
		PortAPI:                         2156,
		MaxRetries:                      defaultMaxRetries,
		VertexAPIKey:                    defaultAnonAPIKey,
		CountTokensQuerySignature:       defaultCountTokensQuerySig,
		MaxN:                            8,
		MaxSpillMB:                      2048,
		MaxRequestMB:                    64,
		MaxConcurrentRequests:           16,
		RequestTimeout:                  180,
		StreamIdleTimeoutSeconds:        60,
		KeepAliveIntervalSeconds:        defaultKeepAliveInterval,
		ClaudePromptInjectionPosition:   "append",
		ClaudePromptStripPromotions:     true,
		ClaudePromptReplaceSecurity:     true,
		ParallelPoolEnabled:             true,
		StickyNodePriority:              true,
		ParallelPoolRetryEnabled:        true,
		ParallelPoolSize:                defaultParallelPoolSize,
		ParallelNodeTopK:                80,
		ParallelPoolDelayDynamic:        false, // 建议默认关闭动态对冲，改为稳定的秒级接力
		ParallelPoolDelayMs:             500,   // 每 0.5 秒启动一个后备节点，高负载下更快填满并发池
		ProxyFailoverMaxAttempts:        60,
		ProxyHealthCheckEnabled:         true,
		ProxyHealthCheckIntervalMinutes: 15,
		ProxyHealthCheckBatchSize:       50,
		ProxyHealthCheckConcurrency:     5,
		ProxyHealthCheckTimeoutSeconds:  8,
		BackgroundImage:                 "url('background.jpg')",
		FontSize:                        "14px",
		FontColorType:                   "adaptive",
		FontColor:                       "#f6f1e9",
		CustomBgPresets:                 []string{},
	}
}

// cacheEntry 是一份不可变的配置快照。发布后其字段不再修改，
// 因此读路径可以无锁取用（见 Load）。
type cacheEntry struct {
	cfg                AppConfig
	claudePromptPolicy ClaudePromptPolicyConfig
	loadedAt           time.Time
}

var (
	//nolint:gochecknoglobals // Global configuration cache
	//
	// 读路径无锁：ConfigProvider 的每个 accessor 都会调 Load()，单次请求可达
	// 数十次；用 atomic.Pointer 发布快照可避免这些高频读互相争抢互斥锁。
	cached atomic.Pointer[cacheEntry]
	//nolint:gochecknoglobals // Serializes cache refills so a burst of misses reads the file once.
	reloadMu sync.Mutex
	//nolint:gochecknoglobals // Serializes atomic config read-modify-write cycles.
	settingsWriteMu sync.Mutex
	//nolint:gochecknoglobals // 上一次成功加载的 config.json 原始内容；
	// 用于把周期性 TTL 重载的“成功加载”日志压缩到内容真正变化时才打印。
	// 仅在 reloadMu 持有期间读写。
	lastLoadedConfigRaw []byte
)

const cacheTTL = 60 * time.Second

func configPath() string {
	if p := os.Getenv("VPROXY_CONFIG"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config", "config.json")
		if _, errStat := os.Stat(p); errStat == nil { //nolint:govet
			return p
		}
	}
	return filepath.Join("config", "config.json")
}

func ConfigPath() string { return configPath() }

func ConfigDir() string { return filepath.Dir(configPath()) }

func WriteSettings(updates map[string]any) error {
	settingsWriteMu.Lock()
	defer settingsWriteMu.Unlock()
	path := configPath()
	raw := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("解析现有配置 %s: %w", path, err)
		}
		if raw == nil {
			return fmt.Errorf("解析现有配置 %s: 顶层必须是 JSON 对象", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取现有配置 %s: %w", path, err)
	}
	for k, v := range updates {
		raw[k] = v
	}

	clampIntSetting(raw, "parallel_pool_size", 1, 40)
	clampIntSetting(raw, "parallel_pool_delay_ms", 50, 10000)
	clampIntSetting(raw, "max_retries", 0, 20)
	clampIntSetting(raw, "max_spill_mb", 1, 8192)
	clampIntSetting(raw, "max_n", 1, 32)
	clampIntSetting(raw, "max_request_mb", 1, 1024)
	clampIntSetting(raw, "max_concurrent_requests", 1, 1000)
	clampIntSetting(raw, "proxy_failover_max_attempts", 1, 200)
	clampIntSetting(raw, "proxy_health_check_interval_minutes", 1, 1440)
	clampIntSetting(raw, "proxy_health_check_batch_size", 1, 500)
	clampIntSetting(raw, "proxy_health_check_concurrency", 1, 20)
	clampIntSetting(raw, "proxy_health_check_timeout_seconds", 2, 60)
	clampIntSetting(
		raw,
		"keepalive_interval",
		KeepAliveMinIntervalSeconds,
		KeepAliveMaxIntervalSeconds,
	)
	clampHealthCheckWorkload(raw)

	if err := writeJSONFile(path, raw); err != nil {
		return err
	}
	InvalidateCache()
	return nil
}

// writeSettingsIfUnchanged applies normalization results only while the
// corresponding on-disk values still match the snapshot Load observed. This
// prevents a delayed normalization goroutine from overwriting a newer admin
// update to the same setting.
func writeSettingsIfUnchanged(expected, updates map[string]any) error {
	return writeSettingsIfUnchangedAt(configPath(), expected, updates)
}

func writeSettingsIfUnchangedAt(path string, expected, updates map[string]any) error {
	settingsWriteMu.Lock()
	defer settingsWriteMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取待归一化配置 %s: %w", path, err)
	}
	raw := map[string]any{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("解析待归一化配置 %s: %w", path, err)
	}

	changed := false
	for key, value := range updates {
		original, tracked := expected[key]
		current, exists := raw[key]
		if !tracked || !exists || !settingValuesEqual(current, original) {
			continue
		}
		raw[key] = value
		changed = true
	}
	if !changed {
		return nil
	}

	if err := writeJSONFile(path, raw); err != nil {
		return err
	}
	InvalidateCache()
	return nil
}

func settingValuesEqual(left, right any) bool {
	leftNumber, leftIsNumber := settingNumber(left)
	rightNumber, rightIsNumber := settingNumber(right)
	if leftIsNumber && rightIsNumber {
		return leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}

func settingNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		number, err := v.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func clampIntSetting(raw map[string]any, key string, minimum, maximum int) {
	var value int
	switch v := raw[key].(type) {
	case float64:
		switch {
		case math.IsNaN(v), math.IsInf(v, 0):
			return
		case v <= float64(minimum):
			value = minimum
		case v >= float64(maximum):
			value = maximum
		default:
			value = int(v)
		}
	case int:
		value = v
	default:
		return
	}
	if value < minimum {
		value = minimum
	}
	if value > maximum {
		value = maximum
	}
	raw[key] = value
}

func writeJSONFile(path string, v any) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入配置临时文件 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("提交配置文件 %s: %w", path, err)
	}
	return nil
}

func Load() AppConfig {
	return loadCacheEntry().cfg
}

func loadCacheEntry() *cacheEntry {
	if entry := cached.Load(); entry != nil && time.Since(entry.loadedAt) < cacheTTL {
		return entry
	}

	// 缓存过期：加锁重填，避免一批并发读同时去读盘。
	reloadMu.Lock()
	defer reloadMu.Unlock()
	// 双重检查——可能已有其它 goroutine 在等锁期间填好了缓存。
	if entry := cached.Load(); entry != nil && time.Since(entry.loadedAt) < cacheTTL {
		return entry
	}

	cfg := DefaultConfig()
	path := configPath()
	data, err := os.ReadFile(path)
	if err == nil {
		if errUnm := json.Unmarshal(data, &cfg); errUnm != nil { //nolint:govet
			log.Printf("[Config] 解析 config.json 失败: %v", errUnm)
		} else {
			normalized := map[string]any{}
			expected := map[string]any{}
			recordNormalization := func(key string, original, value int) {
				if _, recorded := expected[key]; !recorded {
					expected[key] = original
				}
				normalized[key] = value
			}
			// 自动补偿 RequestTimeout 默认值
			if cfg.RequestTimeout <= 0 {
				original := cfg.RequestTimeout
				cfg.RequestTimeout = 180
				recordNormalization("request_timeout", original, cfg.RequestTimeout)
			} else if cfg.RequestTimeout > 1800 {
				log.Printf("[Config] 警告: 请求超时配置过高 (%d)，已限制为上限 1800", cfg.RequestTimeout)
				original := cfg.RequestTimeout
				cfg.RequestTimeout = 1800
				recordNormalization("request_timeout", original, cfg.RequestTimeout)
			}
			normalize := func(key string, target *int, minimum, maximum, fallback int) {
				original := *target
				value := clampInt(original, minimum, maximum, fallback)
				if value != original {
					*target = value
					recordNormalization(key, original, value)
				}
			}
			normalize("stream_idle_timeout_seconds", &cfg.StreamIdleTimeoutSeconds, 1, 1800, 60)
			normalize(
				"parallel_pool_size",
				&cfg.ParallelPoolSize,
				1,
				40,
				defaultParallelPoolSize,
			)
			normalize("parallel_pool_delay_ms", &cfg.ParallelPoolDelayMs, 50, 10000, 500)
			if cfg.MaxRetries < 0 || cfg.MaxRetries > 20 {
				original := cfg.MaxRetries
				cfg.MaxRetries = min(max(cfg.MaxRetries, 0), 20)
				recordNormalization("max_retries", original, cfg.MaxRetries)
			}
			normalize("max_spill_mb", &cfg.MaxSpillMB, 1, 8192, 2048)
			normalize("max_n", &cfg.MaxN, 1, 32, 8)
			normalize("max_request_mb", &cfg.MaxRequestMB, 1, 1024, 64)
			normalize("max_concurrent_requests", &cfg.MaxConcurrentRequests, 1, 1000, 16)
			normalize("proxy_failover_max_attempts", &cfg.ProxyFailoverMaxAttempts, 1, 200, 60)
			normalize(
				"proxy_health_check_interval_minutes",
				&cfg.ProxyHealthCheckIntervalMinutes,
				1,
				1440,
				15,
			)
			normalize("proxy_health_check_batch_size", &cfg.ProxyHealthCheckBatchSize, 1, 500, 50)
			normalize("proxy_health_check_concurrency", &cfg.ProxyHealthCheckConcurrency, 1, 20, 5)
			normalize(
				"proxy_health_check_timeout_seconds",
				&cfg.ProxyHealthCheckTimeoutSeconds,
				2,
				60,
				8,
			)
			normalize(
				"keepalive_interval",
				&cfg.KeepAliveIntervalSeconds,
				KeepAliveMinIntervalSeconds,
				KeepAliveMaxIntervalSeconds,
				defaultKeepAliveInterval,
			)
			if maximum := maxHealthCheckBatch(
				cfg.ProxyHealthCheckIntervalMinutes,
				cfg.ProxyHealthCheckConcurrency,
				cfg.ProxyHealthCheckTimeoutSeconds,
			); cfg.ProxyHealthCheckBatchSize > maximum {
				original := cfg.ProxyHealthCheckBatchSize
				cfg.ProxyHealthCheckBatchSize = maximum
				recordNormalization("proxy_health_check_batch_size", original, maximum)
			}
			if cfg.ProxyFailoverMaxAttempts < cfg.ParallelPoolSize {
				original := cfg.ProxyFailoverMaxAttempts
				cfg.ProxyFailoverMaxAttempts = cfg.ParallelPoolSize
				recordNormalization(
					"proxy_failover_max_attempts",
					original,
					cfg.ProxyFailoverMaxAttempts,
				)
			}
			if len(normalized) > 0 {
				// 异步回写，避免阻塞加载；仅修改仍与本次读取一致的值，
				// 防止覆盖管理员在此期间提交的新配置。
				go func(path string, expectedValues, updates map[string]any) {
					if err := writeSettingsIfUnchangedAt(path, expectedValues, updates); err != nil {
						log.Printf("[Config] 自动回写归一化配置失败: %v", err)
					}
				}(path, expected, normalized)
			}
			if !bytes.Equal(data, lastLoadedConfigRaw) {
				log.Printf("[Config] 成功加载配置文件 config.json")
			}
			lastLoadedConfigRaw = append([]byte(nil), data...)
		}
	} else if os.IsNotExist(err) {
		// 全新部署（尚无 config.json）：保持默认匿名模式（无头 Vertex）。
	} else if !os.IsNotExist(err) {
		log.Printf("[Config] 读取 config.json 失败: %v", err)
	}
	applyEnvironmentOverrides(&cfg)
	entry := &cacheEntry{
		cfg:                cfg,
		claudePromptPolicy: cfg.ClaudePromptPolicySnapshot(),
		loadedAt:           time.Now(),
	}
	cached.Store(entry)
	return entry
}

func applyEnvironmentOverrides(cfg *AppConfig) {
	if rawPort := strings.TrimSpace(os.Getenv("PORT")); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			log.Printf("[Config] 忽略无效的 PORT=%q，继续使用端口 %d", rawPort, cfg.PortAPI)
		} else {
			cfg.PortAPI = port
		}
	}

	if password := strings.TrimSpace(os.Getenv("VPROXY_ADMIN_PASSWORD")); password != "" {
		cfg.AdminPassword = password
	}

	applyEnvBool("VPROXY_PROXY_HEALTH_CHECK_ENABLED", &cfg.ProxyHealthCheckEnabled)
	applyEnvBool("VPROXY_ALLOW_PRIVATE_SUBSCRIPTION_URLS", &cfg.AllowPrivateSubscriptionURLs)
	applyEnvBool("VPROXY_ALLOW_DOMAIN_SUBSCRIPTION_PROXIES", &cfg.AllowDomainSubscriptionProxies)
	applyEnvBool(
		"VPROXY_PROXY_SUBSCRIPTION_ALLOW_PROXY_FALLBACK",
		&cfg.ProxySubscriptionAllowProxyFallback,
	)
	applyEnvInt("VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES", &cfg.ProxyHealthCheckIntervalMinutes, 1, 1440)
	applyEnvInt("VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE", &cfg.ProxyHealthCheckBatchSize, 1, 500)
	applyEnvInt("VPROXY_PROXY_HEALTH_CHECK_CONCURRENCY", &cfg.ProxyHealthCheckConcurrency, 1, 20)
	applyEnvInt("VPROXY_PROXY_HEALTH_CHECK_TIMEOUT_SECONDS", &cfg.ProxyHealthCheckTimeoutSeconds, 2, 60)
	applyEnvInt("VPROXY_PROXY_FAILOVER_MAX_ATTEMPTS", &cfg.ProxyFailoverMaxAttempts, 1, 200)
	applyEnvInt("VPROXY_MAX_CONCURRENT_REQUESTS", &cfg.MaxConcurrentRequests, 1, 1000)
	applyEnvInt("VPROXY_STREAM_IDLE_TIMEOUT_SECONDS", &cfg.StreamIdleTimeoutSeconds, 1, 1800)
	applyKeepAliveEnvironmentOverrides(cfg)
	if cfg.ProxyFailoverMaxAttempts < cfg.ParallelPoolSize {
		log.Printf(
			"[Config] 接力尝试数 %d 小于最大并发 %d，已提升为 %d",
			cfg.ProxyFailoverMaxAttempts,
			cfg.ParallelPoolSize,
			cfg.ParallelPoolSize,
		)
		cfg.ProxyFailoverMaxAttempts = cfg.ParallelPoolSize
	}
	if maximum := maxHealthCheckBatch(
		cfg.ProxyHealthCheckIntervalMinutes,
		cfg.ProxyHealthCheckConcurrency,
		cfg.ProxyHealthCheckTimeoutSeconds,
	); cfg.ProxyHealthCheckBatchSize > maximum {
		log.Printf(
			"[Config] 健康巡检批量 %d 超出当前周期预算，已限制为 %d",
			cfg.ProxyHealthCheckBatchSize,
			maximum,
		)
		cfg.ProxyHealthCheckBatchSize = maximum
	}
}

func applyKeepAliveEnvironmentOverrides(cfg *AppConfig) {
	for _, key := range []string{"VPROXY_KEEPALIVE_URL", "KEEPALIVE_URL"} {
		raw, present := os.LookupEnv(key)
		if !present {
			continue
		}
		value := strings.TrimSpace(raw)
		if err := ValidateKeepAliveURL(value); err != nil {
			log.Printf("[Config] 忽略无效的 %s: %v", key, err)
		} else {
			cfg.KeepAliveURL = value
		}
		break
	}
	for _, key := range []string{
		"VPROXY_KEEPALIVE_INTERVAL_SECONDS",
		"VPROXY_KEEPALIVE_INTERVAL",
		"KEEPALIVE_INTERVAL",
	} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			continue
		}
		applyEnvInt(
			key,
			&cfg.KeepAliveIntervalSeconds,
			KeepAliveMinIntervalSeconds,
			KeepAliveMaxIntervalSeconds,
		)
		break
	}
}

// ValidateKeepAliveURL accepts an empty value (which disables keep-alive) or
// an absolute HTTP(S) URL. Credentials are rejected so secrets cannot be
// accidentally persisted in the admin-visible configuration.
func ValidateKeepAliveURL(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	if len(value) > MaxKeepAliveURLBytes {
		return fmt.Errorf("保活 URL 不能超过 %d 字节", MaxKeepAliveURLBytes)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("保活 URL 必须是完整的 http 或 https 地址")
	}
	if parsed.User != nil {
		return fmt.Errorf("保活 URL 不能包含用户名或密码")
	}
	return nil
}

func maxHealthCheckBatch(intervalMinutes, concurrency, timeoutSeconds int) int {
	if intervalMinutes < 1 {
		intervalMinutes = 1
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	batches := (intervalMinutes * 60) / timeoutSeconds
	if batches < 1 {
		batches = 1
	}
	return min(500, batches*concurrency)
}

func clampHealthCheckWorkload(raw map[string]any) {
	cfg := DefaultConfig()
	data, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return
	}
	maximum := maxHealthCheckBatch(
		cfg.ProxyHealthCheckIntervalMinutes,
		cfg.ProxyHealthCheckConcurrency,
		cfg.ProxyHealthCheckTimeoutSeconds,
	)
	if cfg.ProxyHealthCheckBatchSize > maximum {
		raw["proxy_health_check_batch_size"] = maximum
	}
}

func clampInt(value, minimum, maximum, fallback int) int {
	if value == 0 {
		return fallback
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func applyEnvInt(key string, target *int, minimum, maximum int) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		log.Printf("[Config] 忽略无效的 %s=%q", key, raw)
		return
	}
	*target = value
}

func applyEnvBool(key string, target *bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("[Config] 忽略无效的 %s=%q", key, raw)
		return
	}
	*target = value
}

func InvalidateCache() {
	cached.Store(nil)
}

func (c AppConfig) ConfigDir() string  { return ConfigDir() }
func (c AppConfig) ConfigPath() string { return ConfigPath() }

func (c *AppConfig) WriteSettings(updates map[string]any) error { return WriteSettings(updates) }
func (c *AppConfig) WriteModels(models []string, aliasMap map[string]string) error {
	return WriteModels(models, aliasMap)
}

func (c AppConfig) GetAutoRefreshLogs() bool {
	if c.AutoRefreshLogs == nil {
		return true
	}
	return *c.AutoRefreshLogs
}
