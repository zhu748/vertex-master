package main

import (
	"path/filepath"
	"testing"
)

func TestRenderDocumentedRulesHash(t *testing.T) {
	const documentedHash = "36800adeec862126"
	if got := rulesHash(); got != documentedHash {
		t.Fatalf("Render 文档中的规则哈希需更新：got %q, want %q", got, documentedHash)
	}
}

func TestCheckRulesAgreedDockerFromEnvironment(t *testing.T) {
	t.Setenv("VPROXY_RULES_HASH", "test-rules-hash")

	if !checkRulesAgreedDocker("test-rules-hash") {
		t.Fatal("未通过 VPROXY_RULES_HASH 确认容器使用规则")
	}
}

func TestRenderEnvironmentIsNonInteractiveContainer(t *testing.T) {
	t.Setenv("RENDER", "true")

	if !inDocker() {
		t.Fatal("Render 环境应使用无交互容器启动流程")
	}
}

func TestRulesAgreementUsesConfigDirectory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config", "config.json")
	t.Setenv("VPROXY_CONFIG", configPath)

	want := filepath.Join(filepath.Dir(configPath), "state", "agreed-rules-docker.txt")
	if got := rulesAgreedDockerPath(); got != want {
		t.Fatalf("规则文件路径错误：got %q, want %q", got, want)
	}
}
