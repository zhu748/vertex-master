package api

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// APIKeyManager 简化版 API 密钥管理器。
type APIKeyManager struct { //nolint:govet
	mu       sync.RWMutex
	keys     map[string]string // api_key -> name
	keysFile string
}

const (
	environmentAPIKeyName = "environment"
	examplePlaceholderKey = "sk-your-key-here"
)

// NewAPIKeyManager 构造管理器（密钥文件路径同 config 的解析策略）。
func NewAPIKeyManager() *APIKeyManager {
	return &APIKeyManager{keys: map[string]string{}, keysFile: keysFilePath()} //nolint:exhaustruct
}

func keysFilePath() string {
	if p := os.Getenv("VPROXY_API_KEYS"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config", "api_keys.txt")
		if _, errStat := os.Stat(p); errStat == nil { //nolint:govet
			return p
		}
	}
	return filepath.Join("config", "api_keys.txt")
}

// LoadKeys 从 config/api_keys.txt 和环境变量加载密钥。
// 文件格式：name:api_key:description（每行），api_key 可为任意非空字符串。
func (m *APIKeyManager) LoadKeys() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = map[string]string{}

	loaded := false
	f, err := os.Open(m.keysFile)
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	if err == nil {
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 3)
			if len(parts) < 2 {
				continue
			}
			name := strings.TrimSpace(parts[0])
			key := strings.TrimSpace(parts[1])
			if key == "" || isPlaceholderAPIKey(key) {
				continue
			}
			m.keys[key] = name
		}
		if errScan := sc.Err(); errScan != nil { //nolint:govet
			return false
		}
		loaded = true
	}
	if envKey := strings.TrimSpace(os.Getenv("VPROXY_API_KEY")); envKey != "" {
		m.keys[envKey] = "environment"
		loaded = true
	}
	return loaded
}

// ValidateKey 校验密钥是否有效。
func (m *APIKeyManager) ValidateKey(key string) bool {
	if key == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.keys[strings.TrimSpace(key)]
	return ok
}

// GetKeyName 返回密钥对应的显示名。
func (m *APIKeyManager) GetKeyName(key string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n, ok := m.keys[strings.TrimSpace(key)]; ok {
		return n
	}
	return "unknown"
}

// Count 返回已加载的密钥数。
func (m *APIKeyManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys)
}

// extractAPIKey 从请求提取 API key：
// Bearer 头 > x-goog-api-key 头 > query 参数 key。
func extractAPIKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" && strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if g := r.Header.Get("x-goog-api-key"); g != "" {
		return strings.TrimSpace(g)
	}
	if k := r.URL.Query().Get("key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

// ---- admin 后台的密钥读写 ----
//
// 这些方法直接操作 api_keys.txt 文件（保留每行的 name:key:description 三段格式与顺序），
// 而非改内存 map——文件是真相之源，改完调 LoadKeys 重载内存。

// apiKeyEntry 是密钥文件里的一行（name、key、可选描述）。
type apiKeyEntry struct {
	Name        string
	Key         string
	Description string
	Source      string
	ReadOnly    bool
}

func isPlaceholderAPIKey(key string) bool {
	return strings.EqualFold(strings.TrimSpace(key), examplePlaceholderKey)
}

// readEntries 解析 api_keys.txt 为有序条目列表（跳过空行/注释/字段不足的行）。
func (m *APIKeyManager) readEntries() ([]apiKeyEntry, error) {
	f, err := os.Open(m.keysFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 文件不存在视为空列表
		}
		return nil, fmt.Errorf("error: %w", err)

	}
	defer func() { _ = f.Close() }()

	var out []apiKeyEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 {
			continue
		}
		e := apiKeyEntry{
			Name:   strings.TrimSpace(parts[0]),
			Key:    strings.TrimSpace(parts[1]),
			Source: "file",
		} //nolint:exhaustruct
		if e.Key == "" || isPlaceholderAPIKey(e.Key) {
			continue
		}
		if len(parts) >= 3 {
			e.Description = strings.TrimSpace(parts[2])
		}
		out = append(out, e)
	}
	return out, sc.Err() //nolint:wrapcheck
}

// writeEntries 原子写回 api_keys.txt（先写 .tmp 再 rename），保留三段格式与表头。
func (m *APIKeyManager) writeEntries(entries []apiKeyEntry) error {
	if dir := filepath.Dir(m.keysFile); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("error: %w", err)

		}
	}
	var b strings.Builder
	b.WriteString("# 格式: name:key:description （由管理面板维护）\n")
	for _, e := range entries {
		if e.Name == "" || e.Key == "" || e.ReadOnly || e.Source == "environment" ||
			isPlaceholderAPIKey(e.Key) {
			continue
		}
		b.WriteString(e.Name)
		b.WriteByte(':')
		b.WriteString(e.Key)
		if e.Description != "" {
			b.WriteByte(':')
			b.WriteString(e.Description)
		}
		b.WriteByte('\n')
	}
	tmp := m.keysFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("error: %w", err)

	}
	return os.Rename(tmp, m.keysFile) //nolint:wrapcheck
}

// List 返回文件密钥与环境变量托管密钥。环境变量密钥只用于生成服务端脱敏值，
// 管理 API 不应将其明文返回给浏览器。
func (m *APIKeyManager) List() ([]apiKeyEntry, error) {
	entries, err := m.readEntries()
	if err != nil {
		return nil, err
	}
	envKey := strings.TrimSpace(os.Getenv("VPROXY_API_KEY"))
	if envKey == "" {
		return entries, nil
	}
	kept := make([]apiKeyEntry, 0, len(entries)+1)
	for _, entry := range entries {
		if entry.Key != envKey {
			kept = append(kept, entry)
		}
	}
	kept = append(kept, apiKeyEntry{
		Name:        environmentAPIKeyName,
		Key:         envKey,
		Description: "由 VPROXY_API_KEY 环境变量托管",
		Source:      "environment",
		ReadOnly:    true,
	})
	return kept, nil
}

// Add 新增（或按 name 覆盖）一个密钥，写回文件并重载内存。
// 先剔除同名旧条目，再追加，最后 load_keys。
func (m *APIKeyManager) Add(name, key, description string) error {
	if isPlaceholderAPIKey(key) {
		return fmt.Errorf("示例密钥不能作为有效 API Key")
	}
	entries, err := m.readEntries()
	if err != nil {
		return err
	}
	kept := entries[:0:0] // 新切片，不复用底层数组
	for _, e := range entries {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	kept = append(kept, apiKeyEntry{Name: name, Key: key, Description: description})
	if errW := m.writeEntries(kept); errW != nil { //nolint:govet
		return err
	}
	m.LoadKeys()
	return nil
}

// Delete 按 name 删除一个密钥，写回文件并重载内存。返回 false 表示未找到该名称。
// 未找到时调用方返回 404。
func (m *APIKeyManager) Delete(name string) (bool, error) {
	entries, err := m.readEntries()
	if err != nil {
		return false, err
	}
	kept := entries[:0:0]
	for _, e := range entries {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return false, nil // 没删掉任何条目
	}
	if errW := m.writeEntries(kept); errW != nil { //nolint:govet
		return false, err
	}
	m.LoadKeys()
	return true, nil
}
