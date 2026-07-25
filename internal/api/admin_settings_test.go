package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
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
		"parallel_pool_retry_enabled":true
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
		"proxy_url":                           "http://127.0.0.1:8080",
		"parallel_pool_enabled":               true,
		"parallel_pool_size":                  float64(6),
		"parallel_pool_delay_dynamic":         false,
		"parallel_pool_delay_ms":              float64(1250),
		"proxy_failover_max_attempts":         float64(24),
		"proxy_health_check_enabled":          true,
		"proxy_health_check_interval_minutes": float64(30),
		"proxy_health_check_batch_size":       float64(100),
		"proxy_health_check_concurrency":      float64(8),
		"proxy_health_check_timeout_seconds":  float64(12),
		"sticky_node_priority":                true,
		"parallel_pool_retry_enabled":         true,
	}
	for key, want := range expected {
		if got := raw[key]; got != want {
			t.Errorf("%s 写盘值错误：got=%v want=%v", key, got, want)
		}
	}
}

func newAdminSettingsTestHandler() *AdminHandler {
	cfg := config.DefaultConfig()
	return &AdminHandler{handler: handler{cfg: config.StaticProvider(cfg)}} //nolint:exhaustruct
}
