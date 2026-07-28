package vertex

import (
	"crypto/sha256"
	"encoding/json"
	"sync/atomic"
	"time"
)

const (
	tokenCountCacheTTL      = 5 * time.Minute
	tokenCountCacheMaxItems = 256
	tokenCountCacheMaxBytes = 256 << 10
)

type tokenCountCacheKey [sha256.Size]byte

type tokenCountCacheEntry struct {
	count    int
	storedAt time.Time
}

type tokenCountFlight struct {
	done  chan struct{}
	count int
}

type tokenCountMetrics struct {
	cacheHits       atomic.Uint64
	cacheMisses     atomic.Uint64
	sharedWaits     atomic.Uint64
	cacheBypasses   atomic.Uint64
	upstreamQueries atomic.Uint64
	httpRequests    atomic.Uint64
	failures        atomic.Uint64
}

// TokenCountStats 描述 CountTokens 缓存和上游查询状态。
type TokenCountStats struct {
	CacheHits       uint64 `json:"cache_hits"`
	CacheMisses     uint64 `json:"cache_misses"`
	SharedWaits     uint64 `json:"shared_waits"`
	CacheBypasses   uint64 `json:"cache_bypasses"`
	UpstreamQueries uint64 `json:"upstream_queries"`
	HTTPRequests    uint64 `json:"http_requests"`
	Failures        uint64 `json:"failures"`
	CacheEntries    int    `json:"cache_entries"`
	InFlight        int    `json:"in_flight"`
}

func makeTokenCountCacheKey(model string, contents []any) (tokenCountCacheKey, bool) {
	remaining := tokenCountCacheMaxBytes - len(model)
	if !valueFitsBudget(contents, &remaining) {
		return tokenCountCacheKey{}, false
	}
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(struct {
		Model    string `json:"model"`
		Contents []any  `json:"contents"`
	}{Model: model, Contents: contents}); err != nil {
		return tokenCountCacheKey{}, false
	}
	var key tokenCountCacheKey
	copy(key[:], hash.Sum(nil))
	return key, true
}

func (c *VertexAIClient) loadTokenCountCache(key tokenCountCacheKey) (int, bool) {
	c.countCacheMu.Lock()
	defer c.countCacheMu.Unlock()
	return c.loadTokenCountCacheLocked(key, time.Now())
}

func (c *VertexAIClient) loadTokenCountCacheLocked(key tokenCountCacheKey, now time.Time) (int, bool) {
	entry, ok := c.countCache[key]
	if !ok {
		return 0, false
	}
	if now.Sub(entry.storedAt) >= tokenCountCacheTTL {
		delete(c.countCache, key)
		return 0, false
	}
	return entry.count, entry.count > 0
}

func (c *VertexAIClient) lookupOrClaimTokenCount(
	key tokenCountCacheKey,
) (count int, hit bool, flight *tokenCountFlight, owner bool) {
	c.countCacheMu.Lock()
	defer c.countCacheMu.Unlock()
	if count, hit := c.loadTokenCountCacheLocked(key, time.Now()); hit {
		c.countMetrics.cacheHits.Add(1)
		return count, true, nil, false
	}
	c.countMetrics.cacheMisses.Add(1)
	if existing := c.countFlights[key]; existing != nil {
		c.countMetrics.sharedWaits.Add(1)
		return 0, false, existing, false
	}
	if c.countFlights == nil {
		c.countFlights = make(map[tokenCountCacheKey]*tokenCountFlight)
	}
	flight = &tokenCountFlight{done: make(chan struct{})}
	c.countFlights[key] = flight
	return 0, false, flight, true
}

func (c *VertexAIClient) completeTokenCountFlight(
	key tokenCountCacheKey,
	flight *tokenCountFlight,
	count int,
) {
	c.countCacheMu.Lock()
	defer c.countCacheMu.Unlock()
	if current := c.countFlights[key]; current != flight {
		return
	}
	if count > 0 {
		c.storeTokenCountCacheLocked(key, count, time.Now())
	}
	flight.count = count
	delete(c.countFlights, key)
	close(flight.done)
}

func (c *VertexAIClient) storeTokenCountCache(key tokenCountCacheKey, count int) {
	if count <= 0 {
		return
	}
	now := time.Now()
	c.countCacheMu.Lock()
	defer c.countCacheMu.Unlock()
	c.storeTokenCountCacheLocked(key, count, now)
}

func (c *VertexAIClient) storeTokenCountCacheLocked(key tokenCountCacheKey, count int, now time.Time) {
	if c.countCache == nil {
		c.countCache = make(map[tokenCountCacheKey]tokenCountCacheEntry)
	}
	_, replacing := c.countCache[key]
	if !replacing && len(c.countCache) >= tokenCountCacheMaxItems {
		c.pruneExpiredTokenCountCacheLocked(now)
	}
	if !replacing && len(c.countCache) >= tokenCountCacheMaxItems {
		var oldestKey tokenCountCacheKey
		var oldestAt time.Time
		oldestFound := false
		for cachedKey, entry := range c.countCache {
			if !oldestFound || entry.storedAt.Before(oldestAt) {
				oldestKey, oldestAt = cachedKey, entry.storedAt
				oldestFound = true
			}
		}
		if oldestFound {
			delete(c.countCache, oldestKey)
		}
	}
	c.countCache[key] = tokenCountCacheEntry{count: count, storedAt: now}
}

func (c *VertexAIClient) pruneExpiredTokenCountCacheLocked(now time.Time) {
	for key, entry := range c.countCache {
		if now.Sub(entry.storedAt) >= tokenCountCacheTTL {
			delete(c.countCache, key)
		}
	}
}

// CountTokenStats 返回只读统计快照。
func (c *VertexAIClient) CountTokenStats() TokenCountStats {
	if c == nil {
		return TokenCountStats{}
	}
	c.countCacheMu.Lock()
	c.pruneExpiredTokenCountCacheLocked(time.Now())
	entries, inFlight := len(c.countCache), len(c.countFlights)
	c.countCacheMu.Unlock()
	return TokenCountStats{
		CacheHits:       c.countMetrics.cacheHits.Load(),
		CacheMisses:     c.countMetrics.cacheMisses.Load(),
		SharedWaits:     c.countMetrics.sharedWaits.Load(),
		CacheBypasses:   c.countMetrics.cacheBypasses.Load(),
		UpstreamQueries: c.countMetrics.upstreamQueries.Load(),
		HTTPRequests:    c.countMetrics.httpRequests.Load(),
		Failures:        c.countMetrics.failures.Load(),
		CacheEntries:    entries,
		InFlight:        inFlight,
	}
}

// ResetCountTokenStats 清零累计计数；缓存内容和进行中的请求不受影响。
func (c *VertexAIClient) ResetCountTokenStats() {
	if c == nil {
		return
	}
	c.countMetrics.cacheHits.Store(0)
	c.countMetrics.cacheMisses.Store(0)
	c.countMetrics.sharedWaits.Store(0)
	c.countMetrics.cacheBypasses.Store(0)
	c.countMetrics.upstreamQueries.Store(0)
	c.countMetrics.httpRequests.Store(0)
	c.countMetrics.failures.Store(0)
}
