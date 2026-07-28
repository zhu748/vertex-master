package transport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/constant"
)

type proxyInfo struct {
	proxy      constant.Proxy
	lastUsedAt time.Time
	closed     bool
	label      string
}

var (
	//nolint:gochecknoglobals // Internal proxy connection cache
	proxyMap = make(map[string]*proxyInfo)
	//nolint:gochecknoglobals // Internal proxy connection cache
	proxyMutex sync.RWMutex
)

func getOrStartProxyDialer(uri string, reqID string, debugMode bool) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	proxyMutex.Lock()
	if info, ok := proxyMap[uri]; ok && !info.closed {
		info.lastUsedAt = time.Now()
		p := info.proxy
		proxyMutex.Unlock()
		return makeDialer(p, debugMode), nil
	}
	proxyMutex.Unlock()

	nodeName := nodes.GetNodeName(uri)
	log.Printf("[Transport] 请求ID=%s 触发代理初始化: %s", reqID, nodeName)

	outMap, err := ParseURI(uri)
	if err != nil {
		return nil, fmt.Errorf("parse URI: %w", err)
	}

	proxy, err := adapter.ParseProxy(outMap)
	if err != nil {
		return nil, fmt.Errorf("parse proxy: %w", err)
	}

	proxy = reuseOrInstallProxy(uri, nodeName, proxy)
	return makeDialer(proxy, debugMode), nil
}

func reuseOrInstallProxy(uri, label string, created constant.Proxy) constant.Proxy {
	proxyMutex.Lock()
	if existing, ok := proxyMap[uri]; ok && !existing.closed {
		existing.lastUsedAt = time.Now()
		selected := existing.proxy
		proxyMutex.Unlock()
		closeProxy(created)
		return selected
	}
	proxyMap[uri] = &proxyInfo{
		proxy: created, lastUsedAt: time.Now(), label: label,
	} //nolint:exhaustruct
	proxyMutex.Unlock()
	return created
}

func closeProxy(proxy constant.Proxy) {
	if proxy == nil {
		return
	}
	if closer, ok := proxy.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

func cachedProxyLabel(uri, label string) string {
	if label != "" {
		return label
	}
	return nodes.GetNodeName(uri)
}

func makeDialer(p constant.Proxy, debugMode bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("拆分目标地址 %q: %w", addr, err)
		}

		portInt, _ := strconv.Atoi(port)

		metadata := &constant.Metadata{ //nolint:exhaustruct
			NetWork: constant.TCP,
			Type:    constant.HTTP,
			Host:    host,
			DstPort: uint16(portInt),
		}

		conn, err := p.DialContext(ctx, metadata)
		if err != nil {
			// 若是因为上下文取消导致拨号中止，属于并发竞速中的正常现象，直接退出，不打印误报
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil, fmt.Errorf("mihomo 拨号 %s:%d 被取消: %w", host, portInt, err)
			}
			if debugMode {
				log.Printf("[Transport] Mihomo 拨号失败 [%s:%d]: %v", host, portInt, err)
			}
			return nil, fmt.Errorf("mihomo 拨号 %s:%d: %w", host, portInt, err)
		}

		return conn, nil
	}
}

// RemoveProxy 主动清理代理实例 (响应面板删除节点)
func RemoveProxy(uri string) {
	var removed *proxyInfo
	proxyMutex.Lock()
	if info, ok := proxyMap[uri]; ok {
		if !info.closed {
			info.closed = true
			removed = info
		}
		delete(proxyMap, uri)
	}
	proxyMutex.Unlock()
	if removed != nil {
		closeProxy(removed.proxy)
		log.Printf("[Transport] 代理节点已清理释放: %s", cachedProxyLabel(uri, removed.label))
	}
}

// StartProxyGC 启动后台空闲实例垃圾回收，并返回幂等停止函数。
func StartProxyGC(interval, maxIdle time.Duration) func() {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("StartProxyGC panic: %v\n%s", r, debug.Stack())
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cleanupIdleProxies(maxIdle)
			case <-stop:
				return
			}
		}
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			close(stop)
			<-done
		})
	}
}

func cleanupIdleProxies(maxIdle time.Duration) {
	type expiredProxy struct {
		uri   string
		label string
		proxy constant.Proxy
	}
	var expired []expiredProxy
	proxyMutex.Lock()
	now := time.Now()
	for uri, info := range proxyMap {
		if now.Sub(info.lastUsedAt) > maxIdle {
			if !info.closed {
				info.closed = true
				expired = append(expired, expiredProxy{uri: uri, label: info.label, proxy: info.proxy})
			}
			delete(proxyMap, uri)
		}
	}
	proxyMutex.Unlock()
	for _, info := range expired {
		closeProxy(info.proxy)
		log.Printf("[Transport] 空闲代理已清理释放: %s", cachedProxyLabel(info.uri, info.label))
	}
}

// StopAllProxies 程序优雅退出时清理全部实例
func StopAllProxies() {
	var active []constant.Proxy
	proxyMutex.Lock()
	for _, info := range proxyMap {
		if !info.closed {
			info.closed = true
			active = append(active, info.proxy)
		}
	}
	proxyMap = make(map[string]*proxyInfo)
	proxyMutex.Unlock()
	for _, proxy := range active {
		closeProxy(proxy)
	}
}
