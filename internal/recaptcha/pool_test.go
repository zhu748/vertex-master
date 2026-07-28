package recaptcha

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// TestTokenPoolRealtime 验证每次 GetToken 都实时获取，且 Start/Stop 不阻塞、Stats 返回 0,0。
func TestTokenPoolRealtime(t *testing.T) {
	var calls int32
	p := NewTokenPoolCustom(func(_ string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		return fmt.Sprintf("tok-%d", n), nil
	})

	p.Start()
	if size, fill := p.Stats(); size != 0 || fill != 0 {
		t.Fatalf("Stats 应为 0,0，got %d,%d", size, fill)
	}

	for i := 1; i <= 3; i++ {
		tok, err := p.GetToken()
		if err != nil || tok == "" {
			t.Fatalf("第 %d 次 GetToken 失败：tok=%q err=%v", i, tok, err)
		}
		if int(atomic.LoadInt32(&calls)) != i {
			t.Fatalf("应每次实时获取，期望 %d 次，实际 %d", i, calls)
		}
	}

	p.Stop() // 不应阻塞
}

func TestTokenPoolContextCancellation(t *testing.T) {
	started := make(chan struct{})
	proxySeen := make(chan string, 1)
	p := NewTokenPoolCustomContext(func(ctx context.Context, proxyURI string) (string, error) {
		proxySeen <- proxyURI
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.GetTokenWithProxyContext(ctx, "http://proxy.example:8080")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("context-aware fetcher did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("token fetch did not stop after cancellation")
	}
	if proxyURI := <-proxySeen; proxyURI != "http://proxy.example:8080" {
		t.Fatalf("proxy URI=%q", proxyURI)
	}
}
