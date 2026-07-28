package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAPIKeyManagerSerializesConcurrentAdds(t *testing.T) {
	keysFile := filepath.Join(t.TempDir(), "api_keys.txt")
	t.Setenv("VPROXY_API_KEYS", keysFile)
	t.Setenv("VPROXY_API_KEY", "")
	manager := NewAPIKeyManager()

	const writers = 16
	start := make(chan struct{})
	errorsSeen := make(chan error, writers)
	var wg sync.WaitGroup
	for index := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("writer-%02d", index)
			errorsSeen <- manager.Add(name, "key-"+name, "")
		}()
	}
	close(start)
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent Add failed: %v", err)
		}
	}

	entries, err := manager.List()
	if err != nil || len(entries) != writers {
		t.Fatalf("entries=%d err=%v, want %d", len(entries), err, writers)
	}
	for index := range writers {
		name := fmt.Sprintf("writer-%02d", index)
		if !manager.ValidateKey("key-" + name) {
			t.Fatalf("concurrent update lost %q", name)
		}
	}
}

func TestAPIKeyManagerFailedReloadKeepsLastCompleteSnapshot(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "api_keys.txt")
	if err := os.WriteFile(keysFile, []byte("stable:stable-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPROXY_API_KEYS", keysFile)
	t.Setenv("VPROXY_API_KEY", "")
	manager := NewAPIKeyManager()
	if !manager.LoadKeys() || !manager.ValidateKey("stable-key") {
		t.Fatal("initial key snapshot did not load")
	}

	manager.keysFile = dir // 扫描目录必然失败，模拟瞬时读取错误。
	if manager.LoadKeys() {
		t.Fatal("directory reload unexpectedly succeeded")
	}
	if !manager.ValidateKey("stable-key") || manager.Count() != 1 {
		t.Fatal("failed reload replaced the last complete key snapshot")
	}
}

func TestAPIKeySnapshotReadersDuringUpdates(t *testing.T) {
	keysFile := filepath.Join(t.TempDir(), "api_keys.txt")
	t.Setenv("VPROXY_API_KEYS", keysFile)
	t.Setenv("VPROXY_API_KEY", "stable-environment-key")
	manager := NewAPIKeyManager()
	if !manager.LoadKeys() {
		t.Fatal("environment key did not load")
	}

	const readers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 500 {
				if !manager.ValidateKey("stable-environment-key") || manager.Count() < 1 {
					t.Errorf("reader observed an incomplete key snapshot")
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for index := range 20 {
			name := fmt.Sprintf("rotating-%d", index)
			if err := manager.Add(name, "key-"+name, ""); err != nil {
				t.Errorf("Add(%s): %v", name, err)
				return
			}
			if _, err := manager.Delete(name); err != nil {
				t.Errorf("Delete(%s): %v", name, err)
				return
			}
		}
	}()
	close(start)
	wg.Wait()
}

func BenchmarkAPIKeyValidateSnapshot(b *testing.B) {
	manager := NewAPIKeyManager()
	manager.snapshot.Store(&apiKeySnapshot{keys: map[string]string{"benchmark-key": "bench"}})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !manager.ValidateKey("benchmark-key") {
				b.Fatal("key disappeared")
			}
		}
	})
}
