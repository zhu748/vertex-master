package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRealCLICompatibility 是显式启用的真实客户端冒烟测试。默认测试套件不依赖
// 外部 CLI；设置 VPROXY_RUN_CLI_COMPAT=1 后，会让已安装的 Codex CLI 和
// Claude Code 直接调用本测试的 mock Vertex 上游，验证完整协议握手与流结束。
func TestRealCLICompatibility(t *testing.T) {
	if os.Getenv("VPROXY_RUN_CLI_COMPAT") != "1" {
		t.Skip("set VPROXY_RUN_CLI_COMPAT=1 to run installed CLI compatibility checks")
	}

	t.Run("codex_cli", func(t *testing.T) {
		binary := lookupCLI(t, "codex")
		fx := newTestServer(t)
		home := t.TempDir()
		configText := fmt.Sprintf(`model = "gemini-2.5-flash"
model_provider = "vertex_proxy_test"
model_reasoning_summary = "none"
model_supports_reasoning_summaries = false

[model_providers.vertex_proxy_test]
name = "Vertex Proxy Test"
base_url = %q
env_key = "VPROXY_CLI_TEST_KEY"
wire_api = "responses"
supports_websockets = false
`, fx.server.URL+"/v1")
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(
			ctx, binary, "exec", "--skip-git-repo-check", "--color", "never", "Reply briefly.",
		)
		cmd.Dir = t.TempDir()
		cmd.Env = replaceEnv(os.Environ(), map[string]string{
			"CODEX_HOME": home, "VPROXY_CLI_TEST_KEY": "sk-test-key",
		})
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Codex CLI compatibility failed: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "Hello") {
			t.Fatalf("Codex CLI did not consume the mock response: %s", output)
		}
	})

	t.Run("claude_code", func(t *testing.T) {
		binary := lookupCLI(t, "claude")
		fx := newTestServer(t)
		claudeConfig := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(
			ctx, binary, "-p", "Reply briefly.", "--model", "gemini-2.5-flash", "--max-turns", "1",
			"--output-format", "text", "--dangerously-skip-permissions",
		)
		cmd.Dir = t.TempDir()
		cmd.Stdin = strings.NewReader("")
		cmd.Env = replaceEnv(os.Environ(), map[string]string{
			"ANTHROPIC_BASE_URL":                       fx.server.URL,
			"ANTHROPIC_AUTH_TOKEN":                     "sk-test-key",
			"ANTHROPIC_API_KEY":                        "sk-test-key",
			"DISABLE_PROMPT_CACHING":                   "1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			"CLAUDE_CONFIG_DIR":                        claudeConfig,
			"CLAUDECODE":                               "",
		})
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Claude Code compatibility failed: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "Hello") {
			t.Fatalf("Claude Code did not consume the mock response: %s", output)
		}
	})
}

func lookupCLI(t *testing.T, name string) string {
	t.Helper()
	candidates := []string{name}
	if runtime.GOOS == "windows" {
		candidates = []string{name + ".cmd", name + ".exe", name}
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skipf("%s is not installed", name)
	return ""
}

func replaceEnv(current []string, updates map[string]string) []string {
	result := make([]string, 0, len(current)+len(updates))
	for _, entry := range current {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := updates[strings.ToUpper(key)]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range updates {
		result = append(result, key+"="+value)
	}
	return result
}
