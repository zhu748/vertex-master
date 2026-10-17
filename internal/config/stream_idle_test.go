package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamIdleTimeoutDefaultsNormalizationAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("VPROXY_CONFIG", path)
	t.Setenv("VPROXY_STREAM_IDLE_TIMEOUT_SECONDS", "")
	t.Cleanup(InvalidateCache)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{}`, 60}, {`{"stream_idle_timeout_seconds":0}`, 60},
		{`{"stream_idle_timeout_seconds":-1}`, 1}, {`{"stream_idle_timeout_seconds":9999}`, 1800},
		{`{"stream_idle_timeout_seconds":120}`, 120},
	} {
		// Serialize fixture writes with the asynchronous normalization writer,
		// just as production admin updates do through WriteSettings.
		settingsWriteMu.Lock()
		err := os.WriteFile(path, []byte(tc.body), 0600)
		settingsWriteMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		InvalidateCache()
		if got := GetProvider().StreamIdleTimeoutSeconds(); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.body, got, tc.want)
		}
	}
	for _, seconds := range []int{1, 90, 1800} {
		t.Setenv("VPROXY_STREAM_IDLE_TIMEOUT_SECONDS", fmt.Sprint(seconds))
		InvalidateCache()
		if got := GetProvider().StreamIdleTimeoutSeconds(); got != seconds {
			t.Fatalf("env got %d want %d", got, seconds)
		}
	}
}

func TestNormalizationKeepsOriginalConfigPath(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original.json")
	next := filepath.Join(dir, "next.json")
	for _, path := range []string{original, next} {
		if err := os.WriteFile(path, []byte(`{"stream_idle_timeout_seconds":0}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("VPROXY_CONFIG", original)
	t.Setenv("VPROXY_STREAM_IDLE_TIMEOUT_SECONDS", "")
	InvalidateCache()
	t.Cleanup(InvalidateCache)
	settingsWriteMu.Lock()
	_ = Load() // Queue normalization while the writer is blocked.
	t.Setenv("VPROXY_CONFIG", next)
	settingsWriteMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(original)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]int
		if json.Unmarshal(data, &values) == nil && values["stream_idle_timeout_seconds"] == 60 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("normalization did not write to its original path")
		}
		time.Sleep(time.Millisecond)
	}
	data, err := os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"stream_idle_timeout_seconds":0}` {
		t.Fatalf("normalization modified the new configuration: %s", data)
	}
}
