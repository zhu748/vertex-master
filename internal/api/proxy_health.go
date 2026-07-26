package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/transport"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

const proxyHealthCheckURL = "https://www.google.com/generate_204"

type ProxyHealthSchedulerStatus struct {
	Enabled   bool  `json:"enabled"`
	Running   bool  `json:"running"`
	LastRunAt int64 `json:"last_run_at"`
	NextRunAt int64 `json:"next_run_at"`
	Checked   int   `json:"checked"`
	Succeeded int   `json:"succeeded"`
	Failed    int   `json:"failed"`
}

var (
	proxyHealthStatusMu sync.RWMutex //nolint:gochecknoglobals
	proxyHealthStatus   ProxyHealthSchedulerStatus
)

func GetProxyHealthSchedulerStatus() ProxyHealthSchedulerStatus {
	proxyHealthStatusMu.RLock()
	defer proxyHealthStatusMu.RUnlock()
	return proxyHealthStatus
}

func setProxyHealthSchedulerStatus(update func(*ProxyHealthSchedulerStatus)) {
	proxyHealthStatusMu.Lock()
	update(&proxyHealthStatus)
	proxyHealthStatusMu.Unlock()
}

func checkProxyConnectivity(
	ctx context.Context,
	netClient *transport.NetworkClient,
	node nodes.Node,
	timeoutSeconds int,
) error {
	if netClient == nil {
		return errors.New("network client unavailable")
	}
	session, err := netClient.CreateSession(timeoutSeconds, node.RawURI, "proxy-health")
	if err != nil {
		return err
	}
	defer session.Close()

	resp, err := session.Do(ctx, http.MethodGet, proxyHealthCheckURL, transport.Header{
		"user-agent": {"Mozilla/5.0 (proxy health checker)"},
		"accept":     {"*/*"},
	}, nil)
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("nil health response")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("health endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func runProxyHealthBatch(ctx context.Context, vc *vertex.VertexAIClient, cfg config.ConfigProvider) {
	if vc == nil || vc.Net() == nil {
		log.Printf("[ProxyHealth] 跳过巡检：网络客户端不可用")
		setProxyHealthSchedulerStatus(func(status *ProxyHealthSchedulerStatus) {
			status.Running = false
			status.LastRunAt = time.Now().Unix()
			status.Checked = 0
			status.Succeeded = 0
			status.Failed = 0
		})
		return
	}

	interval := time.Duration(cfg.ProxyHealthCheckIntervalMinutes()) * time.Minute
	batch := nodes.SelectNodesForHealthCheck(cfg.ProxyHealthCheckBatchSize(), interval, time.Now())
	if len(batch) == 0 {
		setProxyHealthSchedulerStatus(func(status *ProxyHealthSchedulerStatus) {
			status.Running = false
			status.LastRunAt = time.Now().Unix()
			status.Checked = 0
			status.Succeeded = 0
			status.Failed = 0
		})
		return
	}

	setProxyHealthSchedulerStatus(func(status *ProxyHealthSchedulerStatus) {
		status.Running = true
		status.LastRunAt = time.Now().Unix()
		status.Checked = 0
		status.Succeeded = 0
		status.Failed = 0
	})

	var succeeded atomic.Int64
	var failed atomic.Int64
	var checked atomic.Int64
	defer func() {
		setProxyHealthSchedulerStatus(func(status *ProxyHealthSchedulerStatus) {
			status.Running = false
			status.Checked = int(checked.Load())
			status.Succeeded = int(succeeded.Load())
			status.Failed = int(failed.Load())
		})
	}()

	timeoutSeconds := cfg.ProxyHealthCheckTimeoutSeconds()
	concurrency := min(max(1, cfg.ProxyHealthCheckConcurrency()), len(batch))
	jobs := make(chan nodes.Node)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for node := range jobs {
				if ctx.Err() != nil {
					return
				}
				start := time.Now()
				nodeCtx, cancel := context.WithTimeout(
					ctx,
					time.Duration(timeoutSeconds)*time.Second,
				)
				err := checkProxyConnectivity(
					nodeCtx,
					vc.Net(),
					node,
					timeoutSeconds,
				)
				cancel()
				if ctx.Err() != nil {
					return
				}
				elapsed := float64(time.Since(start).Milliseconds())
				if err != nil {
					failed.Add(1)
					nodes.RecordTest(node.RawURI, false, elapsed, safeProxyTestError(err))
				} else {
					succeeded.Add(1)
					nodes.RecordTest(node.RawURI, true, elapsed, "")
				}
				checked.Add(1)
			}
		}()
	}
	for _, node := range batch {
		select {
		case jobs <- node:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()

	log.Printf("[ProxyHealth] 巡检完成：检查 %d，可用 %d，失败 %d",
		checked.Load(), succeeded.Load(), failed.Load())
}

// StartProxyHealthScheduler 启动低频、限量的代理健康巡检，不会永久禁用失败节点。
func StartProxyHealthScheduler(vc *vertex.VertexAIClient, cfg config.ConfigProvider) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		const configPollInterval = 10 * time.Second
		nextRun := time.Now().Add(10 * time.Second)
		var previousInterval time.Duration
		ticker := time.NewTicker(configPollInterval)
		defer ticker.Stop()
		for {
			enabled := cfg.ProxyHealthCheckEnabled()
			interval := time.Duration(cfg.ProxyHealthCheckIntervalMinutes()) * time.Minute
			if interval < time.Minute {
				interval = time.Minute
			}
			now := time.Now()
			status := GetProxyHealthSchedulerStatus()
			if !enabled {
				nextRun = time.Time{}
			} else {
				switch {
				case nextRun.IsZero():
					nextRun = now.Add(10 * time.Second)
				case previousInterval > 0 && interval != previousInterval && status.LastRunAt > 0:
					nextRun = time.Unix(status.LastRunAt, 0).Add(interval)
					if nextRun.Before(now) {
						nextRun = now
					}
				}
			}
			previousInterval = interval
			setProxyHealthSchedulerStatus(func(status *ProxyHealthSchedulerStatus) {
				status.Enabled = enabled
				if nextRun.IsZero() {
					status.NextRunAt = 0
				} else {
					status.NextRunAt = nextRun.Unix()
				}
			})

			if enabled && !now.Before(nextRun) {
				runProxyHealthBatch(ctx, vc, cfg)
				nextRun = time.Now().Add(interval)
				setProxyHealthSchedulerStatus(func(status *ProxyHealthSchedulerStatus) {
					status.NextRunAt = nextRun.Unix()
				})
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				setProxyHealthSchedulerStatus(func(status *ProxyHealthSchedulerStatus) {
					status.Running = false
					status.NextRunAt = 0
				})
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
