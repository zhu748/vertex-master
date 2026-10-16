package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadLogsSuccessOnlyWhenConfigContentChanges 验证周期性 TTL 重载的
// “成功加载配置文件”日志只在内容真正变化（或首次加载）时打印，
// 避免守护进程每 60 秒刷一条无信息量的日志。
func TestLoadLogsSuccessOnlyWhenConfigContentChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"port_api":2171}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPROXY_CONFIG", path)
	InvalidateCache()
	lastLoadedConfigRaw = nil
	t.Cleanup(func() {
		InvalidateCache()
		lastLoadedConfigRaw = nil
	})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	Load()
	if !strings.Contains(buf.String(), "成功加载配置文件") {
		t.Fatalf("首次加载应打印成功日志, got: %q", buf.String())
	}

	buf.Reset()
	InvalidateCache()
	Load()
	if strings.Contains(buf.String(), "成功加载配置文件") {
		t.Fatalf("内容未变化的周期重载不应重复打印成功日志, got: %q", buf.String())
	}

	if err := os.WriteFile(path, []byte(`{"port_api":2172}`), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	InvalidateCache()
	Load()
	if !strings.Contains(buf.String(), "成功加载配置文件") {
		t.Fatalf("内容变化后的重载应再次打印成功日志, got: %q", buf.String())
	}
}
