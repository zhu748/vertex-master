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

	log.Printf("[Transport] 请求ID=%s 触发代理初始化: %s", reqID, nodes.GetNodeName(uri))

	outMap, err := ParseURI(uri)
	if err != nil {
		return nil, fmt.Errorf("parse URI: %w", err)
	}

	proxy, err := adapter.ParseProxy(outMap)
	if err != nil {
		return nil, fmt.Errorf("parse proxy: %w", err)
	}

	proxyMutex.Lock()
	if old, ok := proxyMap[uri]; ok && !old.closed {
		old.closed = true
		if closer, ok := old.proxy.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	proxyMap[uri] = &proxyInfo{proxy: proxy, lastUsedAt: time.Now()} //nolint:exhaustruct
	proxyMutex.Unlock()

	return makeDialer(proxy, debugMode), nil
}

func makeDialer(p constant.Proxy, debugMode bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("error: %w", err)

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
				return nil, fmt.Errorf("error: %w", err)

			}
			if debugMode {
				log.Printf("[Transport] Mihomo 拨号失败 [%s:%d]: %v", host, portInt, err)
			}
			return nil, fmt.Errorf("error: %w", err)

		}

		return conn, nil
	}
}

// RemoveProxy 主动清理代理实例 (响应面板删除节点)
func RemoveProxy(uri string) {
	proxyMutex.Lock()
	if info, ok := proxyMap[uri]; ok {
		if !info.closed {
			info.closed = true
			if closer, ok := info.proxy.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
			log.Printf("[Transport] 代理节点已清理释放: %s", nodes.GetNodeName(uri))
		}
		delete(proxyMap, uri)
	}
	proxyMutex.Unlock()
}

// StartProxyGC 启动后台空闲实例垃圾回收 (每隔 interval 扫描，超时 maxIdle 回收)
func StartProxyGC(interval, maxIdle time.Duration) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("StartProxyGC panic: %v\n%s", r, debug.Stack())
			}
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			cleanupIdleProxies(maxIdle)
		}
	}()
}

func cleanupIdleProxies(maxIdle time.Duration) {
	proxyMutex.Lock()
	defer proxyMutex.Unlock()
	now := time.Now()
	for uri, info := range proxyMap {
		if now.Sub(info.lastUsedAt) > maxIdle {
			if !info.closed {
				info.closed = true
				if closer, ok := info.proxy.(interface{ Close() error }); ok {
					_ = closer.Close()
				}
				log.Printf("[Transport] 空闲代理已清理释放: %s", nodes.GetNodeName(uri))
			}
			delete(proxyMap, uri)
		}
	}
}

// StopAllProxies 程序优雅退出时清理全部实例
func StopAllProxies() {
	proxyMutex.Lock()
	defer proxyMutex.Unlock()
	for _, info := range proxyMap {
		if !info.closed {
			info.closed = true
			if closer, ok := info.proxy.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
		}
	}
	proxyMap = make(map[string]*proxyInfo)
}
