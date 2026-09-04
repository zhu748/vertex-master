package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGeminiAPIModeDefaultBehavior 验证上游通道模式的默认语义：
//   - 默认（config.json 不存在 / 无 gemini_api_mode 字段）为 anon，
//     即项目原有的无头 Vertex 匿名端点，不注入任何默认密钥；
//   - official 模式下未显式配置 Key 且未构建时注入时，解析结果为空（调用方报配置错误）；
//   - 显式配置的 gemini_api_key 始终优先于构建时注入的内置密钥。
func TestGeminiAPIModeDefaultBehavior(t *testing.T) {
	cases := []struct {
		name       string
		content    string // 空串表示不创建配置文件
		wantMode   string // NormalizeGeminiAPIMode 归一化后的期望值
		wantKey    string // Load().GeminiAPIKey 原始值
		wantEffect string // ResolveGeminiAPIKey 解析后的实际生效密钥
	}{
		{
			name:       "missing config file stays anonymous",
			content:    "",
			wantMode:   GeminiAPIModeAnon,
			wantKey:    "",
			wantEffect: "",
		},
		{
			name:       "legacy config without fields stays anonymous",
			content:    `{"port_api":2156}`,
			wantMode:   GeminiAPIModeAnon,
			wantKey:    "",
			wantEffect: "",
		},
		{
			name:       "official mode without key resolves empty (no builtin injected)",
			content:    `{"port_api":2156,"gemini_api_mode":"official"}`,
			wantMode:   GeminiAPIModeOfficial,
			wantKey:    "",
			wantEffect: "",
		},
		{
			name:       "official mode with explicit key wins",
			content:    `{"port_api":2156,"gemini_api_mode":"official","gemini_api_key":"K"}`,
			wantMode:   GeminiAPIModeOfficial,
			wantKey:    "K",
			wantEffect: "K",
		},
		{
			name:       "anon mode keeps saved key for later switch",
			content:    `{"port_api":2156,"gemini_api_mode":"anon","gemini_api_key":"K"}`,
			wantMode:   GeminiAPIModeAnon,
			wantKey:    "K",
			wantEffect: "K", // anon 不发起直连请求；保留展示供切换回 official 时使用
		},
		{
			name:       "unknown mode value treated as anon",
			content:    `{"port_api":2156,"gemini_api_mode":"whatever"}`,
			wantMode:   GeminiAPIModeAnon,
			wantKey:    "",
			wantEffect: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("VPROXY_CONFIG", path)
			t.Setenv("VPROXY_GEMINI_API_KEY", "") // 排除环境变量干扰
			t.Setenv("VPROXY_GEMINI_API_MODE", "")
			InvalidateCache()
			t.Cleanup(InvalidateCache)

			cfg := Load()
			if got := NormalizeGeminiAPIMode(cfg.GeminiAPIMode); got != tc.wantMode {
				t.Fatalf("mode = %q, want %q", got, tc.wantMode)
			}
			if got := cfg.GeminiAPIKey; got != tc.wantKey {
				t.Fatalf("GeminiAPIKey = %q, want %q", got, tc.wantKey)
			}
			if got := ResolveGeminiAPIKey(cfg.GeminiAPIMode, cfg.GeminiAPIKey); got != tc.wantEffect {
				t.Fatalf("effective key = %q, want %q", got, tc.wantEffect)
			}
		})
	}
}

// TestEnvGeminiModeOverrides 验证环境变量对通道模式的控制：
//   - VPROXY_GEMINI_API_MODE 显式指定时优先级最高；
//   - 只设置 VPROXY_GEMINI_API_KEY（未显式选择模式）时视为要直连；
//   - 配置文件显式 anon + 环境变量 Key：文件声明优先，保持匿名。
func TestEnvGeminiModeOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"port_api":2156}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPROXY_CONFIG", path)
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	t.Run("env mode wins", func(t *testing.T) {
		t.Setenv("VPROXY_GEMINI_API_MODE", "OFFICIAL")
		t.Setenv("VPROXY_GEMINI_API_KEY", "")
		InvalidateCache()
		if got := NormalizeGeminiAPIMode(Load().GeminiAPIMode); got != GeminiAPIModeOfficial {
			t.Fatalf("env mode = %q, want official", got)
		}
	})
	t.Run("env key alone implies official", func(t *testing.T) {
		t.Setenv("VPROXY_GEMINI_API_MODE", "")
		t.Setenv("VPROXY_GEMINI_API_KEY", "ENVKEY")
		InvalidateCache()
		cfg := Load()
		if got := NormalizeGeminiAPIMode(cfg.GeminiAPIMode); got != GeminiAPIModeOfficial {
			t.Fatalf("env key alone: mode = %q, want official", got)
		}
		if got := ResolveGeminiAPIKey(cfg.GeminiAPIMode, cfg.GeminiAPIKey); got != "ENVKEY" {
			t.Fatalf("env key alone: effective key = %q, want ENVKEY", got)
		}
	})
	t.Run("file anon beats env key", func(t *testing.T) {
		if err := os.WriteFile(path, []byte(`{"port_api":2156,"gemini_api_mode":"anon"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VPROXY_GEMINI_API_MODE", "")
		t.Setenv("VPROXY_GEMINI_API_KEY", "ENVKEY")
		InvalidateCache()
		if got := NormalizeGeminiAPIMode(Load().GeminiAPIMode); got != GeminiAPIModeAnon {
			t.Fatalf("file anon must beat env key, got %q", got)
		}
	})
}

// TestBuiltinGeminiAPIKeyInjection 验证构建时注入（ldflags）的内置密钥语义：
// 仅当 official 模式 + 未显式配置 Key 时回退生效，且显式 Key 始终优先。
func TestBuiltinGeminiAPIKeyInjection(t *testing.T) {
	restore := builtinGeminiAPIKey
	t.Cleanup(func() { builtinGeminiAPIKey = restore })
	builtinGeminiAPIKey = "AQ.BuiltinKeyFromLdflags12345"

	if got := ResolveGeminiAPIKey(GeminiAPIModeOfficial, ""); got != builtinGeminiAPIKey {
		t.Fatalf("official+empty must fall back to injected builtin, got %q", got)
	}
	if got := ResolveGeminiAPIKey(GeminiAPIModeOfficial, "K"); got != "K" {
		t.Fatalf("explicit key must win over builtin, got %q", got)
	}
	if got := ResolveGeminiAPIKey(GeminiAPIModeAnon, ""); got != "" {
		t.Fatalf("anon must never use builtin key, got %q", got)
	}
	if !UsingBuiltinGeminiAPIKey(GeminiAPIModeOfficial, "") {
		t.Fatal("UsingBuiltinGeminiAPIKey must be true for official+empty with injected builtin")
	}
	if UsingBuiltinGeminiAPIKey(GeminiAPIModeOfficial, "K") {
		t.Fatal("UsingBuiltinGeminiAPIKey must be false when explicit key set")
	}

	// 未注入（空内置密钥）时：official+空 Key 解析为空，且不算使用内置密钥。
	builtinGeminiAPIKey = ""
	if got := ResolveGeminiAPIKey(GeminiAPIModeOfficial, ""); got != "" {
		t.Fatalf("empty builtin must resolve empty, got %q", got)
	}
	if UsingBuiltinGeminiAPIKey(GeminiAPIModeOfficial, "") {
		t.Fatal("UsingBuiltinGeminiAPIKey must be false when builtin empty")
	}
}

// TestUsingBuiltinGeminiAPIKey 验证密钥来源判定（面板展示用）。
func TestUsingBuiltinGeminiAPIKey(t *testing.T) {
	restore := builtinGeminiAPIKey
	t.Cleanup(func() { builtinGeminiAPIKey = restore })
	builtinGeminiAPIKey = "AQ.BuiltinKeyFromLdflags12345"

	cases := []struct {
		mode, key string
		want      bool
	}{
		{mode: GeminiAPIModeOfficial, key: "", want: true},
		{mode: "OFFICIAL", key: "  ", want: true},
		{mode: GeminiAPIModeOfficial, key: "K", want: false},
		{mode: GeminiAPIModeAnon, key: "", want: false},
		{mode: "", key: "K", want: false},
	}
	for _, tc := range cases {
		if got := UsingBuiltinGeminiAPIKey(tc.mode, tc.key); got != tc.want {
			t.Fatalf("UsingBuiltinGeminiAPIKey(%q, %q) = %v, want %v", tc.mode, tc.key, got, tc.want)
		}
	}
}

// TestDefaultConfigKeepsGeminiDefaultsEmpty 确保 DefaultConfig 不携带真实密钥
// 也不预置直连模式，避免测试夹具（marshal 完整结构体）意外写进 config.json。
func TestDefaultConfigKeepsGeminiDefaultsEmpty(t *testing.T) {
	if DefaultConfig().GeminiAPIKey != "" {
		t.Fatal("DefaultConfig must not prefill gemini_api_key")
	}
	if DefaultConfig().GeminiAPIMode != "" {
		t.Fatal("DefaultConfig must not prefill gemini_api_mode")
	}
	if got := NormalizeGeminiAPIMode(DefaultConfig().GeminiAPIMode); got != GeminiAPIModeAnon {
		t.Fatalf("default mode must normalize to anon, got %q", got)
	}
}
