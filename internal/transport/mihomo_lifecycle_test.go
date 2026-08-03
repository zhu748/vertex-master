package transport

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/constant"
)

type lifecycleProxy struct {
	constant.Proxy
	closeCalls  atomic.Int32
	closeOnce   sync.Once
	closeStart  chan struct{}
	closeResume chan struct{}
}

func (p *lifecycleProxy) Close() error {
	p.closeCalls.Add(1)
	if p.closeStart != nil {
		p.closeOnce.Do(func() { close(p.closeStart) })
	}
	if p.closeResume != nil {
		<-p.closeResume
	}
	return nil
}

func TestProxyGCCanStop(t *testing.T) {
	const proxyURI = "test://proxy-gc-lifecycle"
	proxyMutex.Lock()
	proxyMap[proxyURI] = &proxyInfo{lastUsedAt: time.Now().Add(-time.Hour), closed: true}
	proxyMutex.Unlock()
	t.Cleanup(func() {
		proxyMutex.Lock()
		delete(proxyMap, proxyURI)
		proxyMutex.Unlock()
	})

	stop := StartProxyGC(5*time.Millisecond, time.Minute)
	deadline := time.Now().Add(time.Second)
	for {
		proxyMutex.RLock()
		_, exists := proxyMap[proxyURI]
		proxyMutex.RUnlock()
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("proxy GC did not remove idle entry")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	stop() // 幂等

	proxyMutex.Lock()
	proxyMap[proxyURI] = &proxyInfo{lastUsedAt: time.Now().Add(-time.Hour), closed: true}
	proxyMutex.Unlock()
	time.Sleep(20 * time.Millisecond)
	proxyMutex.RLock()
	_, exists := proxyMap[proxyURI]
	proxyMutex.RUnlock()
	if !exists {
		t.Fatal("stopped proxy GC continued running")
	}
}

func TestReuseOrInstallProxyKeepsFirstConcurrentInstance(t *testing.T) {
	const proxyURI = "test://concurrent-install"
	proxyMutex.Lock()
	previous := proxyMap
	proxyMap = make(map[string]*proxyInfo)
	proxyMutex.Unlock()
	t.Cleanup(func() {
		StopAllProxies()
		proxyMutex.Lock()
		proxyMap = previous
		proxyMutex.Unlock()
	})

	const installers = 32
	created := make([]*lifecycleProxy, installers)
	selected := make(chan *proxyInfo, installers)
	var wg sync.WaitGroup
	for index := range installers {
		created[index] = &lifecycleProxy{}
		wg.Add(1)
		go func(proxy *lifecycleProxy) {
			defer wg.Done()
			selected <- reuseOrInstallProxy(proxyURI, "concurrent", proxy)
		}(created[index])
	}
	wg.Wait()
	close(selected)

	var winner *proxyInfo
	for info := range selected {
		if winner == nil {
			winner = info
			continue
		}
		if info != winner {
			t.Fatal("concurrent installers returned different active proxy instances")
		}
	}
	closed := int32(0)
	for _, proxy := range created {
		closed += proxy.closeCalls.Load()
	}
	if closed != installers-1 {
		t.Fatalf("redundant proxy closes = %d, want %d", closed, installers-1)
	}
	proxyMutex.RLock()
	info := proxyMap[proxyURI]
	proxyMutex.RUnlock()
	if info == nil || info != winner || info.closed {
		t.Fatalf("unexpected cached winner: %#v", info)
	}
}

func TestCachedProxyLookupDoesNotRequireGlobalWriteLock(t *testing.T) {
	const proxyURI = "test://read-locked-cache-hit"
	proxyMutex.Lock()
	previous := proxyMap
	proxyMap = map[string]*proxyInfo{
		proxyURI: newProxyInfo(&lifecycleProxy{}, "read-lock-test", time.Now()),
	}
	proxyMutex.Unlock()
	t.Cleanup(func() {
		proxyMutex.Lock()
		proxyMap = previous
		proxyMutex.Unlock()
	})

	// Holding a read lock must not block another cached lookup. This protects
	// the hot path from regressing to a process-wide write lock.
	proxyMutex.RLock()
	result := make(chan error, 1)
	go func() {
		_, err := getOrStartProxyDialer(proxyURI, "read-lock-test", false)
		result <- err
	}()
	select {
	case err := <-result:
		proxyMutex.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		proxyMutex.RUnlock()
		<-result
		t.Fatal("cached proxy lookup blocked behind an unrelated read lock")
	}
}

func TestProxyLifecycleClosesResourcesOutsideCacheLock(t *testing.T) {
	tests := []struct {
		name string
		run  func(string)
	}{
		{name: "remove", run: RemoveProxy},
		{name: "idle cleanup", run: func(string) { cleanupIdleProxies(time.Minute) }},
		{name: "stop all", run: func(string) { StopAllProxies() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uri := "test://close-outside-lock/" + test.name
			proxy := &lifecycleProxy{
				closeStart: make(chan struct{}), closeResume: make(chan struct{}),
			}
			proxyMutex.Lock()
			previous := proxyMap
			proxyMap = map[string]*proxyInfo{
				uri: {
					proxy: proxy, lastUsedAt: time.Now().Add(-time.Hour), label: "lifecycle-test",
				},
			}
			proxyMutex.Unlock()
			t.Cleanup(func() {
				proxyMutex.Lock()
				proxyMap = previous
				proxyMutex.Unlock()
			})

			done := make(chan struct{})
			go func() {
				defer close(done)
				test.run(uri)
			}()
			select {
			case <-proxy.closeStart:
			case <-time.After(time.Second):
				t.Fatal("proxy close did not start")
			}

			lockAcquired := make(chan struct{})
			go func() {
				proxyMutex.Lock()
				close(lockAcquired)
				proxyMutex.Unlock()
			}()
			select {
			case <-lockAcquired:
			case <-time.After(time.Second):
				close(proxy.closeResume)
				<-done
				t.Fatal("proxy cache lock remained held while resource Close blocked")
			}
			close(proxy.closeResume)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("proxy lifecycle operation did not finish")
			}
			if calls := proxy.closeCalls.Load(); calls != 1 {
				t.Fatalf("proxy close calls = %d, want 1", calls)
			}
		})
	}
}

func BenchmarkGetOrStartProxyDialerCached(b *testing.B) {
	const proxyCount = 256
	proxyURIs := make([]string, proxyCount)
	proxies := make(map[string]*proxyInfo, proxyCount)
	for index := range proxyCount {
		uri := fmt.Sprintf("test://cached-dialer-benchmark/%d", index)
		proxyURIs[index] = uri
		proxies[uri] = newProxyInfo(&lifecycleProxy{}, "benchmark", time.Now())
	}
	proxyMutex.Lock()
	previous := proxyMap
	proxyMap = proxies
	proxyMutex.Unlock()
	b.Cleanup(func() {
		proxyMutex.Lock()
		proxyMap = previous
		proxyMutex.Unlock()
	})

	b.ReportAllocs()
	var seed atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		index := int(seed.Add(1))
		for pb.Next() {
			uri := proxyURIs[index&(proxyCount-1)]
			index += 17
			if _, err := getOrStartProxyDialer(uri, "benchmark", false); err != nil {
				b.Fatal(err)
			}
		}
	})
}
