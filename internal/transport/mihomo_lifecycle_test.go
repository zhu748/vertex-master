package transport

import (
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
	selected := make(chan constant.Proxy, installers)
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

	var winner constant.Proxy
	for proxy := range selected {
		if winner == nil {
			winner = proxy
			continue
		}
		if proxy != winner {
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
	if info == nil || info.proxy != winner || info.closed {
		t.Fatalf("unexpected cached winner: %#v", info)
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
				proxyMutex.Unlock()
				close(lockAcquired)
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
