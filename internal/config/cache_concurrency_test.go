package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestLoadIsRaceFreeUnderHotReload 覆盖配置缓存的读写并发：
// ConfigProvider 的每个 accessor 都会调 Load()，请求路径上并发读取极频繁，
// 同时 SIGHUP / 面板改设置会随时失效缓存。配合 go test -race 防回归。
func TestLoadIsRaceFreeUnderHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"port_api":2156,"max_retries":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPROXY_CONFIG", path)
	InvalidateCache()
	t.Cleanup(InvalidateCache)

	const (
		readers = 8
		loops   = 200
	)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range loops {
				cfg := Load()
				// 读到的必须是自洽的快照，不能是撕裂的中间态。
				if cfg.PortAPI != 2156 || cfg.MaxRetries != 3 {
					t.Errorf("读到不一致的配置快照: port=%d retries=%d", cfg.PortAPI, cfg.MaxRetries)
					return
				}
				if err := GetProvider().ClaudePromptPolicySnapshot().ValidationError(); err != nil {
					t.Errorf("读到无效的提示词策略快照: %v", err)
					return
				}
			}
		}()
	}
	// 并发失效缓存，模拟 SIGHUP 热重载与面板写设置。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range loops {
			InvalidateCache()
		}
	}()
	wg.Wait()
}

// TestLoadModelsFileIsRaceFreeUnderHotReload 同上，覆盖 models.json 缓存。
// ResolveModelName 在每次请求中都会被调用。
func TestLoadModelsFileIsRaceFreeUnderHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(
		path,
		[]byte(`{"models":["gemini-alpha"],"alias_map":{"a":"gemini-alpha"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPROXY_MODELS", path)
	InvalidateModelsCache()
	t.Cleanup(InvalidateModelsCache)

	const (
		readers = 8
		loops   = 200
	)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range loops {
				if got := ResolveModelName("a"); got != "gemini-alpha" {
					t.Errorf("别名解析结果异常: %q", got)
					return
				}
				// 返回值必须是副本：调用方改动不能污染共享快照。
				models := BaseModels()
				if len(models) > 0 {
					models[0] = "mutated-by-caller"
				}
				aliases := AliasMap()
				aliases["injected"] = "by-caller"
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range loops {
			InvalidateModelsCache()
		}
	}()
	wg.Wait()

	// 调用方的改动不得回流到共享缓存。
	if got := BaseModels(); contains(got, "mutated-by-caller") {
		t.Fatalf("BaseModels 必须返回副本，共享快照被污染: %v", got)
	}
	if _, injected := AliasMap()["injected"]; injected {
		t.Fatal("AliasMap 必须返回副本，共享快照被污染")
	}
}
