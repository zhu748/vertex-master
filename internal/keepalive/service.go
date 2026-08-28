// Package keepalive periodically calls a configured HTTP(S) URL so deployments
// that provide an external keep-alive target can keep the application active.
package keepalive

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

const (
	requestTimeout      = 30 * time.Second
	maxResponseBodyRead = 1 << 20
)

type Config interface {
	KeepAliveURL() string
	KeepAliveIntervalSeconds() int
}

// Service owns at most one keep-alive loop. Its lifecycle methods are safe to
// call concurrently, which lets the admin API hot-reload it after a save.
type Service struct {
	cfg    Config
	client *http.Client

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

func New(cfg Config) *Service {
	return newService(cfg, &http.Client{Timeout: requestTimeout})
}

func newService(cfg Config, client *http.Client) *Service {
	return &Service{cfg: cfg, client: client} //nolint:exhaustruct
}

// Start starts the loop when a valid URL is configured. The first request is
// sent immediately, matching gcli2api's keep-alive behavior.
func (s *Service) Start() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.done != nil {
		return
	}
	s.startLocked()
}

// Restart atomically replaces the active loop with the latest config values.
func (s *Service) Restart() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stopLocked()
	s.startLocked()
}

// Stop cancels any in-flight request and waits for the loop to exit.
func (s *Service) Stop() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stopLocked()
}

func (s *Service) startLocked() {
	if s.cfg == nil {
		return
	}
	target := strings.TrimSpace(s.cfg.KeepAliveURL())
	if target == "" {
		return
	}
	if err := config.ValidateKeepAliveURL(target); err != nil {
		log.Printf("[KeepAlive] 配置无效，保活未启动: %v", err)
		return
	}
	intervalSeconds := s.cfg.KeepAliveIntervalSeconds()
	if intervalSeconds < config.KeepAliveMinIntervalSeconds ||
		intervalSeconds > config.KeepAliveMaxIntervalSeconds {
		log.Printf("[KeepAlive] 间隔 %d 秒无效，使用默认值 60 秒", intervalSeconds)
		intervalSeconds = 60
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	log.Printf("[KeepAlive] 已启动，目标=%s，间隔=%d 秒", safeURL(target), intervalSeconds)
	go s.run(ctx, done, target, time.Duration(intervalSeconds)*time.Second)
}

func (s *Service) stopLocked() {
	if s.done == nil {
		return
	}
	s.cancel()
	<-s.done
	s.cancel = nil
	s.done = nil
	log.Printf("[KeepAlive] 已停止")
}

func (s *Service) run(ctx context.Context, done chan<- struct{}, target string, interval time.Duration) {
	defer close(done)
	for {
		status, err := s.ping(ctx, target)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[KeepAlive] 请求 %s 失败: %v", safeURL(target), err)
		} else {
			log.Printf("[KeepAlive] 请求 %s 完成，状态码=%d", safeURL(target), status)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (s *Service) ping(ctx context.Context, target string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, fmt.Errorf("创建请求: %w", err)
	}
	req.Header.Set("User-Agent", "vertex-keepalive/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBodyRead))
	closeErr := resp.Body.Close()
	if copyErr != nil {
		return resp.StatusCode, fmt.Errorf("读取响应: %w", copyErr)
	}
	if closeErr != nil {
		return resp.StatusCode, fmt.Errorf("关闭响应: %w", closeErr)
	}
	return resp.StatusCode, nil
}

func safeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String()
}
