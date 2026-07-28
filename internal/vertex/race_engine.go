package vertex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

type RaceOption func(*raceConfig)

type raceConfig struct {
	noCancelOnSuccess bool
}

func WithNoCancelOnSuccess() RaceOption {
	return func(cfg *raceConfig) {
		cfg.noCancelOnSuccess = true
	}
}

func safeResetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

type raceResult[T any] struct {
	node    nodes.Node
	val     T
	err     error
	elapsed time.Duration
}

func proxyAttemptMilliseconds(elapsed time.Duration) float64 {
	return float64(elapsed) / float64(time.Millisecond)
}

// recordProxyAttempt keeps proxy health separate from request semantics.
// Invalid arguments, missing models, and other non-retryable upstream errors
// describe the request rather than the proxy, so they must not cool down an
// otherwise healthy node. The return value reports whether sticky state should
// be evicted for this attempt.
func recordProxyAttempt(uri string, err error, elapsed time.Duration) bool {
	if uri == "" {
		return false
	}
	if err == nil {
		nodes.RecordTest(uri, true, proxyAttemptMilliseconds(elapsed), "")
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}

	ve := asVertexError(err)
	if ve != nil && ve.Kind == "ratelimit" {
		nodes.RecordRateLimit(uri, 30)
		return true
	}
	if ve != nil && !ve.IsRetryable() {
		return false
	}

	nodes.RecordTest(uri, false, proxyAttemptMilliseconds(elapsed), err.Error())
	return true
}

// RunRace runs a hedge race across multiple candidate nodes.
//
// It handles:
//   - sticky pool acquisition (when enabled)
//   - node selection via SelectForParallel
//   - sticky pool filtering (enabled: exclude sticky URIs; disabled: prepend sticky URIs as priority)
//   - fallback to single node when pool is disabled or no candidates
//   - hedge timer with static/dynamic delay
//   - result collection: first success wins immediately
//   - cancellation of losing candidates as soon as a winner is selected
//   - error classification: 429 → rate-limit cooldown; retryable connection/upstream
//     errors → failed health; non-retryable request errors do not penalize the proxy
//   - hard error (non-retryable) terminates the race early
//   - context.Canceled errors are not counted as failures
func RunRace[T any](ctx context.Context, cfg config.ConfigProvider,
	run func(ctx context.Context, proxyURI string) (T, error),
	opts ...RaceOption,
) (T, error) {
	return runRacePreferred(ctx, cfg, run, nil, nil, opts...)
}

// RunRacePreferred waits for a preferred successful result while retaining
// other successful results as fallbacks. This is used by non-streaming calls,
// where a fast truncated response must not cancel a slightly slower complete
// response.
func RunRacePreferred[T any](
	ctx context.Context,
	cfg config.ConfigProvider,
	run func(ctx context.Context, proxyURI string) (T, error),
	preferred func(T) bool,
	chooseFallback func([]T) (T, error),
	opts ...RaceOption,
) (T, error) {
	return runRacePreferred(ctx, cfg, run, preferred, chooseFallback, opts...)
}

func runRacePreferred[T any](
	ctx context.Context,
	cfg config.ConfigProvider,
	run func(ctx context.Context, proxyURI string) (T, error),
	preferred func(T) bool,
	chooseFallback func([]T) (T, error),
	opts ...RaceOption,
) (T, error) {
	var rc raceConfig
	for _, o := range opts {
		o(&rc)
	}

	stickyPool := nodes.GetStickyPool()

	cands := nodes.SelectForParallel(
		cfg.ProxyFailoverMaxAttempts(),
		cfg.ParallelNodeTopK(),
		cfg.DebugMode(),
		cfg.StickyNodePriority(),
	)

	if !cfg.ParallelPoolEnabled() || len(cands) == 0 {
		proxy := cfg.ActiveNodeURI()
		if proxy == "" {
			proxy = cfg.ProxyURL()
		}
		log.Printf("[Vertex] [RunParallel] 降级为单节点运行: %s", nodes.GetNodeName(proxy))
		startedAt := time.Now()
		value, err := run(ctx, proxy)
		if err == nil {
			recordProxyAttempt(proxy, nil, time.Since(startedAt))
			nodes.RecordProxySuccessForRequest(
				proxy,
				RequestIDFromContext(ctx),
				proxyAttemptMilliseconds(time.Since(startedAt)),
			)
		} else {
			recordProxyAttempt(proxy, err, time.Since(startedAt))
		}
		return value, err
	}

	if cfg.DebugMode() {
		log.Printf("[Vertex] [RunParallel] 开启对冲延迟竞速, %d 个节点参与", len(cands))
		for _, c := range cands {
			log.Printf("[Vertex] [RunParallel] 参与节点: %s", nodes.SafeNodeLabel(c.Name))
		}
	}

	cli.UpdateReqState(RequestIDFromContext(ctx), "⚡ 并发竞速", "\033[33m", fmt.Sprintf("并行节点: %d", len(cands)))

	ctxRace, cancel := context.WithCancel(ctx) //nolint:govet // cancel called on error paths; win path relies on parent ctx
	var returnedOnWinPath bool
	defer func() {
		if !returnedOnWinPath || !rc.noCancelOnSuccess {
			cancel()
		}
	}()

	resCh := make(chan raceResult[T], min(len(cands)+20, 30))
	active := 0
	activeKeys := make(map[string]bool)
	activeCancels := make(map[string]context.CancelFunc)

	launchNode := func(candidate nodes.Node) bool {
		uri := candidate.RawURI
		if activeKeys[uri] {
			return false
		}
		activeKeys[uri] = true
		nodeCtx, nodeCancel := context.WithCancel(ctxRace)
		activeCancels[uri] = nodeCancel

		nodes.RecordSelection(uri)
		active++
		go func(node nodes.Node) {
			startedAt := time.Now()
			v, err := run(nodeCtx, node.RawURI)
			select {
			case resCh <- raceResult[T]{node: node, val: v, err: err, elapsed: time.Since(startedAt)}:
			case <-ctxRace.Done():
			}
		}(candidate)
		return true
	}

	cancelCandidate := func(uri string) {
		if candidateCancel := activeCancels[uri]; candidateCancel != nil {
			candidateCancel()
			delete(activeCancels, uri)
		}
	}

	cancelOtherCandidates := func(winner string) {
		for uri, candidateCancel := range activeCancels {
			if uri == winner {
				continue
			}
			candidateCancel()
			delete(activeCancels, uri)
		}
	}

	launchNode(cands[0])

	delay := time.Duration(cfg.ParallelPoolDelayMs()) * time.Millisecond
	if cfg.ParallelPoolDelayDynamic() {
		delay = time.Duration(nodes.GetAverageLatency()) * time.Millisecond
	}
	if delay < 100*time.Millisecond {
		delay = 100 * time.Millisecond
	} else if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	maxConcurrent := max(1, cfg.ParallelPoolSize())

	timer := time.NewTimer(delay)
	defer timer.Stop()

	nextIdx := 1
	var zero T
	var fallbackResults []T
	finishWithFallback := func(lastErr error) (T, error) {
		if len(fallbackResults) > 0 && chooseFallback != nil {
			return chooseFallback(fallbackResults)
		}
		if lastErr != nil {
			return zero, lastErr
		}
		return zero, fmt.Errorf("all nodes failed")
	}
	launchNext := func() bool {
		for nextIdx < len(cands) && active < maxConcurrent {
			candidate := cands[nextIdx]
			nextIdx++
			if launchNode(candidate) {
				return true
			}
		}
		return false
	}

	for {
		select {
		case <-ctx.Done():
			cancel()
			return zero, ctx.Err()

		case <-timer.C:
			if launchNext() {
				if cfg.DebugMode() {
					log.Printf("[Racing] 对冲延迟唤醒，已启动第 %d 个候选节点", nextIdx)
				}
			}
			if nextIdx < len(cands) && active < maxConcurrent {
				timer.Reset(delay)
			}

		case res := <-resCh:
			active--
			uri := res.node.RawURI
			name := nodes.SafeNodeLabel(res.node.Name)

			if res.err == nil {
				recordProxyAttempt(uri, nil, res.elapsed)
				stickyPool.Add(uri)
				if preferred != nil && !preferred(res.val) {
					fallbackResults = append(fallbackResults, res.val)
					cancelCandidate(uri)
					if launchNext() {
						safeResetTimer(timer, delay)
					}
					if active == 0 && nextIdx >= len(cands) {
						cancel()
						return finishWithFallback(nil)
					}
					continue
				}
				nodes.RecordProxySuccessForNode(
					res.node,
					RequestIDFromContext(ctx),
					proxyAttemptMilliseconds(res.elapsed),
				)
				log.Printf("[Racing] 竞速胜出节点: %s", name)
				cli.UpdateReqWinner(RequestIDFromContext(ctx), name)
				cli.UpdateReqState(RequestIDFromContext(ctx), "🟢 数据传输", "\033[32m", "已建立连接")

				returnedOnWinPath = true
				if rc.noCancelOnSuccess {
					cancelOtherCandidates(uri)
				} else {
					cancel()
				}
				return res.val, nil
			}
			cancelCandidate(uri)

			if !errors.Is(res.err, context.Canceled) {
				if cfg.DebugMode() {
					log.Printf("[Racing] 节点 %s 失败: %s", name, res.err.Error())
				}

				ve := asVertexError(res.err)
				if ve != nil && ve.Kind == "ratelimit" {
					if cfg.DebugMode() {
						log.Printf("[Racing] 节点 %s 触发 429 API 限制，进入 30 秒短时歇息", name)
					}
				}
				if recordProxyAttempt(uri, res.err, res.elapsed) {
					stickyPool.Evict(uri)
				}

				if ve != nil && !ve.IsRetryable() {
					if cfg.DebugMode() {
						log.Printf("[Racing] 节点 %s 触发不可重试的硬性错误，终止竞速", name)
					}
					cancel()
					return finishWithFallback(res.err)
				}

				if launchNext() {
					if cfg.DebugMode() {
						log.Printf("[Racing] 竞速失败触发极速对冲接力...")
					}
					safeResetTimer(timer, delay)
				}
			} else {
				if cfg.DebugMode() {
					log.Printf("[Racing] 节点 %s 拨号取消", name)
				}
				if ctxRace.Err() == nil && launchNext() {
					safeResetTimer(timer, delay)
				}
			}

			if active == 0 && nextIdx < len(cands) && launchNext() {
				safeResetTimer(timer, delay)
			}
			if active == 0 && nextIdx >= len(cands) {
				cancel()
				return finishWithFallback(res.err)
			}
		}
	}
}
