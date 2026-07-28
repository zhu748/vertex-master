package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWriteSettingsSerializesConcurrentMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			errs <- WriteSettings(map[string]any{fmt.Sprintf("concurrent_%02d", index): index})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < writers; i++ {
		if _, ok := raw[fmt.Sprintf("concurrent_%02d", i)]; !ok {
			t.Fatalf("concurrent update %d was lost", i)
		}
	}
}

// TestWriteSettingsMergesAndPreservesUnknown 验证 WriteSettings：
// 合并已知字段、保留未提及字段（含 AppConfig 之外的额外字段）、写后缓存失效立即生效。
func TestWriteSettingsMergesAndPreservesUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("VPROXY_CONFIG", path)

	// 预置一份含额外字段 max_concurrent（admin 不认识、但不应丢）。
	initial := `{"port_api":2156,"max_retries":2,"max_concurrent":40}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()

	if err := WriteSettings(map[string]any{"max_retries": 5, "aggregate_stream": true}); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	// 强类型读取：已知字段已更新。
	cfg := Load()
	if cfg.MaxRetries != 5 {
		t.Fatalf("max_retries 应为 5，got %d", cfg.MaxRetries)
	}
	if !cfg.AggregateStream {
		t.Fatalf("aggregate_stream 应为 true")
	}

	// 原始 map 读取：未提及字段 + 额外字段都应保留。
	raw := map[string]any{}
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["max_concurrent"] != float64(40) {
		t.Fatalf("额外字段 max_concurrent 应被保留为 40，got %v", raw["max_concurrent"])
	}
	if raw["port_api"] != float64(2156) {
		t.Fatalf("未提及字段 port_api 应保留为 2156，got %v", raw["port_api"])
	}

	InvalidateCache() // 清理，避免影响其它测试
}

func TestWriteSettingsIfUnchangedPreservesNewerAdminValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	initial := map[string]any{
		"parallel_pool_size": 0,
		"request_timeout":    0,
		"custom_setting":     "keep-me",
	}
	if err := writeJSONFile(path, initial); err != nil {
		t.Fatal(err)
	}

	// 模拟 Load 读取旧值后、异步归一化落盘前管理员更新了同一字段。
	newer := map[string]any{
		"parallel_pool_size": 10,
		"request_timeout":    0,
		"max_request_mb":     2048,
		"custom_setting":     "keep-me",
	}
	if err := writeJSONFile(path, newer); err != nil {
		t.Fatal(err)
	}

	err := writeSettingsIfUnchanged(
		map[string]any{"parallel_pool_size": 0, "request_timeout": 0},
		map[string]any{"parallel_pool_size": 5, "request_timeout": 180},
	)
	if err != nil {
		t.Fatalf("writeSettingsIfUnchanged: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["parallel_pool_size"] != float64(10) {
		t.Fatalf("管理员的新值不应被旧归一化任务覆盖，got %v", raw["parallel_pool_size"])
	}
	if raw["request_timeout"] != float64(180) {
		t.Fatalf("未变化的旧值仍应完成归一化，got %v", raw["request_timeout"])
	}
	if raw["max_request_mb"] != float64(2048) {
		t.Fatalf("后台归一化不应顺带修改未跟踪的字段，got %v", raw["max_request_mb"])
	}
	if raw["custom_setting"] != "keep-me" {
		t.Fatalf("未知字段应保留，got %v", raw["custom_setting"])
	}
}

// TestWriteModelsRoundTrip 验证 WriteModels：写盘 + 热重载，BaseModels/AliasMap 立即读到新值。
func TestWriteModelsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	t.Setenv("VPROXY_MODELS", path)
	InvalidateModelsCache()

	models := []string{"gemini-x", "gemini-y"}
	alias := map[string]string{"fast": "gemini-x"}
	if err := WriteModels(models, alias); err != nil {
		t.Fatalf("WriteModels: %v", err)
	}

	if got := BaseModels(); !contains(got, "gemini-x") || !contains(got, "gemini-y") {
		t.Fatalf("BaseModels 应含写入的模型，got %v", got)
	}
	if got := AliasMap(); got["fast"] != "gemini-x" {
		t.Fatalf("AliasMap 应含 fast→gemini-x，got %v", got)
	}
	if ResolveModelName("fast") != "gemini-x" {
		t.Fatalf("别名 fast 应解析为 gemini-x")
	}

	// nil aliasMap 应写空表、不报错。
	if err := WriteModels([]string{"only-one"}, nil); err != nil {
		t.Fatalf("WriteModels nil alias: %v", err)
	}
	if got := AliasMap(); len(got) != 0 {
		t.Fatalf("nil aliasMap 应写空表，got %v", got)
	}

	InvalidateModelsCache() // 清理
}

func TestWriteModelsSerializesConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	t.Setenv("VPROXY_MODELS", path)
	InvalidateModelsCache()
	t.Cleanup(InvalidateModelsCache)

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			model := fmt.Sprintf("gemini-%d", index)
			errs <- WriteModels([]string{model}, map[string]string{"current": model})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发写模型配置失败: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed modelsFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("并发写入后 models.json 必须保持有效: %v", err)
	}
	if len(parsed.Models) != 1 || parsed.AliasMap["current"] != parsed.Models[0] {
		t.Fatalf("模型和别名应来自同一次原子写入: %+v", parsed)
	}
}

func TestWriteJSONFileMarshalFailurePreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const original = `{"keep":true}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeJSONFile(path, map[string]any{"unsupported": make(chan int)}); err == nil {
		t.Fatal("不可序列化的数据应返回错误")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("序列化失败不应破坏原文件，got %q", data)
	}
}

func TestApplyEnvironmentOverrides(t *testing.T) {
	t.Setenv("PORT", "10000")
	t.Setenv("VPROXY_ADMIN_PASSWORD", "render-admin-password")
	t.Setenv("VPROXY_ALLOW_PRIVATE_SUBSCRIPTION_URLS", "true")
	t.Setenv("VPROXY_ALLOW_DOMAIN_SUBSCRIPTION_PROXIES", "true")
	t.Setenv("VPROXY_PROXY_SUBSCRIPTION_ALLOW_PROXY_FALLBACK", "true")

	cfg := DefaultConfig()
	applyEnvironmentOverrides(&cfg)

	if cfg.PortAPI != 10000 {
		t.Fatalf("PORT 环境变量未生效，got %d", cfg.PortAPI)
	}
	if cfg.AdminPassword != "render-admin-password" {
		t.Fatalf("VPROXY_ADMIN_PASSWORD 环境变量未生效")
	}
	if !cfg.AllowPrivateSubscriptionURLs {
		t.Fatalf("VPROXY_ALLOW_PRIVATE_SUBSCRIPTION_URLS 环境变量未生效")
	}
	if !cfg.AllowDomainSubscriptionProxies {
		t.Fatalf("VPROXY_ALLOW_DOMAIN_SUBSCRIPTION_PROXIES 环境变量未生效")
	}
	if !cfg.ProxySubscriptionAllowProxyFallback {
		t.Fatalf("VPROXY_PROXY_SUBSCRIPTION_ALLOW_PROXY_FALLBACK 环境变量未生效")
	}
}

func TestApplyEnvironmentOverridesIgnoresInvalidPort(t *testing.T) {
	t.Setenv("PORT", "70000")

	cfg := DefaultConfig()
	applyEnvironmentOverrides(&cfg)

	if cfg.PortAPI != 2156 {
		t.Fatalf("无效 PORT 不应覆盖默认端口，got %d", cfg.PortAPI)
	}
}

func TestApplyEnvironmentOverridesProxyHealthAndFailover(t *testing.T) {
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_ENABLED", "false")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES", "45")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE", "120")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_CONCURRENCY", "9")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_TIMEOUT_SECONDS", "17")
	t.Setenv("VPROXY_PROXY_FAILOVER_MAX_ATTEMPTS", "37")

	cfg := DefaultConfig()
	applyEnvironmentOverrides(&cfg)

	if cfg.ProxyHealthCheckEnabled {
		t.Fatal("VPROXY_PROXY_HEALTH_CHECK_ENABLED=false 未生效")
	}
	if cfg.ProxyHealthCheckIntervalMinutes != 45 {
		t.Fatalf("健康巡检间隔应为 45，got %d", cfg.ProxyHealthCheckIntervalMinutes)
	}
	if cfg.ProxyHealthCheckBatchSize != 120 {
		t.Fatalf("健康巡检批量应为 120，got %d", cfg.ProxyHealthCheckBatchSize)
	}
	if cfg.ProxyHealthCheckConcurrency != 9 {
		t.Fatalf("健康巡检并发应为 9，got %d", cfg.ProxyHealthCheckConcurrency)
	}
	if cfg.ProxyHealthCheckTimeoutSeconds != 17 {
		t.Fatalf("健康巡检超时应为 17，got %d", cfg.ProxyHealthCheckTimeoutSeconds)
	}
	if cfg.ProxyFailoverMaxAttempts != 37 {
		t.Fatalf("代理接力尝试数应为 37，got %d", cfg.ProxyFailoverMaxAttempts)
	}
}

func TestApplyEnvironmentOverridesIgnoresInvalidProxyValues(t *testing.T) {
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_ENABLED", "sometimes")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES", "0")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE", "501")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_CONCURRENCY", "not-a-number")
	t.Setenv("VPROXY_PROXY_HEALTH_CHECK_TIMEOUT_SECONDS", "1")
	t.Setenv("VPROXY_PROXY_FAILOVER_MAX_ATTEMPTS", "101")

	cfg := DefaultConfig()
	cfg.ProxyHealthCheckEnabled = false
	cfg.ProxyHealthCheckIntervalMinutes = 33
	cfg.ProxyHealthCheckBatchSize = 77
	cfg.ProxyHealthCheckConcurrency = 6
	cfg.ProxyHealthCheckTimeoutSeconds = 12
	cfg.ProxyFailoverMaxAttempts = 19
	applyEnvironmentOverrides(&cfg)

	if cfg.ProxyHealthCheckEnabled {
		t.Fatal("非法布尔环境变量不应覆盖原值")
	}
	if cfg.ProxyHealthCheckIntervalMinutes != 33 ||
		cfg.ProxyHealthCheckBatchSize != 77 ||
		cfg.ProxyHealthCheckConcurrency != 6 ||
		cfg.ProxyHealthCheckTimeoutSeconds != 12 ||
		cfg.ProxyFailoverMaxAttempts != 19 {
		t.Fatalf("非法代理环境变量不应覆盖原配置，got %+v", cfg)
	}
}

func TestLoadLegacyConfigGetsNewProxyDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	clearProxyEnvironment(t)
	if err := os.WriteFile(path, []byte(`{"parallel_pool_enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	cfg := Load()
	defaults := DefaultConfig()
	if cfg.ProxyFailoverMaxAttempts != defaults.ProxyFailoverMaxAttempts ||
		cfg.ParallelPoolRetryEnabled != defaults.ParallelPoolRetryEnabled ||
		cfg.ProxyHealthCheckEnabled != defaults.ProxyHealthCheckEnabled ||
		cfg.ProxyHealthCheckIntervalMinutes != defaults.ProxyHealthCheckIntervalMinutes ||
		cfg.ProxyHealthCheckBatchSize != defaults.ProxyHealthCheckBatchSize ||
		cfg.ProxyHealthCheckConcurrency != defaults.ProxyHealthCheckConcurrency ||
		cfg.ProxyHealthCheckTimeoutSeconds != defaults.ProxyHealthCheckTimeoutSeconds {
		t.Fatalf("旧配置应获得新增代理默认值，got %+v", cfg)
	}
}

func TestDefaultConfigEnablesParallelPoolRetry(t *testing.T) {
	if !DefaultConfig().ParallelPoolRetryEnabled {
		t.Fatal("默认配置应开启并发池节点重试，以便 429 可在节点内自动重试")
	}
}

func TestLoadNormalizesOutOfRangeProxySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	clearProxyEnvironment(t)
	initial := `{
		"parallel_pool_size": 0,
		"parallel_pool_delay_ms": 999999,
		"proxy_failover_max_attempts": 1,
		"proxy_health_check_interval_minutes": 0,
		"proxy_health_check_batch_size": 999,
		"proxy_health_check_concurrency": -3,
		"proxy_health_check_timeout_seconds": 1
	}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	cfg := Load()
	if cfg.ParallelPoolSize != 5 {
		t.Fatalf("parallel_pool_size=0 应回退为 5，got %d", cfg.ParallelPoolSize)
	}
	if cfg.ParallelPoolDelayMs != 10000 {
		t.Fatalf("过大的接力延迟应限制为 10000，got %d", cfg.ParallelPoolDelayMs)
	}
	if cfg.ProxyFailoverMaxAttempts != 5 {
		t.Fatalf("尝试数应至少等于并发数 5，got %d", cfg.ProxyFailoverMaxAttempts)
	}
	if cfg.ProxyHealthCheckIntervalMinutes != 15 {
		t.Fatalf("巡检间隔 0 应回退为 15，got %d", cfg.ProxyHealthCheckIntervalMinutes)
	}
	if cfg.ProxyHealthCheckBatchSize != 450 {
		t.Fatalf("巡检批量应受单轮周期预算限制为 450，got %d", cfg.ProxyHealthCheckBatchSize)
	}
	if cfg.ProxyHealthCheckConcurrency != 1 {
		t.Fatalf("巡检并发下限应为 1，got %d", cfg.ProxyHealthCheckConcurrency)
	}
	if cfg.ProxyHealthCheckTimeoutSeconds != 2 {
		t.Fatalf("巡检超时下限应为 2，got %d", cfg.ProxyHealthCheckTimeoutSeconds)
	}

	waitForNormalizedConfig(t, path, map[string]int{
		"parallel_pool_size":                  5,
		"parallel_pool_delay_ms":              10000,
		"proxy_failover_max_attempts":         5,
		"proxy_health_check_interval_minutes": 15,
		"proxy_health_check_batch_size":       450,
		"proxy_health_check_concurrency":      1,
		"proxy_health_check_timeout_seconds":  2,
	})
}

func clearProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PORT",
		"VPROXY_ADMIN_PASSWORD",
		"VPROXY_ALLOW_PRIVATE_SUBSCRIPTION_URLS",
		"VPROXY_ALLOW_DOMAIN_SUBSCRIPTION_PROXIES",
		"VPROXY_PROXY_SUBSCRIPTION_ALLOW_PROXY_FALLBACK",
		"VPROXY_PROXY_HEALTH_CHECK_ENABLED",
		"VPROXY_PROXY_HEALTH_CHECK_INTERVAL_MINUTES",
		"VPROXY_PROXY_HEALTH_CHECK_BATCH_SIZE",
		"VPROXY_PROXY_HEALTH_CHECK_CONCURRENCY",
		"VPROXY_PROXY_HEALTH_CHECK_TIMEOUT_SECONDS",
		"VPROXY_PROXY_FAILOVER_MAX_ATTEMPTS",
	} {
		t.Setenv(key, "")
	}
}

func waitForNormalizedConfig(t *testing.T, path string, expected map[string]int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			raw := map[string]any{}
			if json.Unmarshal(data, &raw) == nil {
				matches := true
				for key, value := range expected {
					if raw[key] != float64(value) {
						matches = false
						break
					}
				}
				if matches {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待归一化配置回写超时，path=%s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
