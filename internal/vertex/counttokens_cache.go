package vertex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	tokenCountCacheTTL      = 5 * time.Minute
	tokenCountCacheMaxItems = 256
	tokenCountCacheMaxBytes = 256 << 10
	maxPooledTokenKeyBytes  = 64 << 10
)

var tokenCountKeyBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return new(bytes.Buffer) },
}

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
	if remaining < 0 {
		return tokenCountCacheKey{}, false
	}
	buffer := tokenCountKeyBufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	writeTokenCountHashString(buffer, tokenCountHashModel, model)
	if !writeTokenCountHashValue(buffer, contents, &remaining, 0) {
		releaseTokenCountKeyBuffer(buffer)
		return tokenCountCacheKey{}, false
	}
	key := tokenCountCacheKey(sha256.Sum256(buffer.Bytes()))
	releaseTokenCountKeyBuffer(buffer)
	return key, true
}

const (
	tokenCountHashNil byte = iota
	tokenCountHashFalse
	tokenCountHashTrue
	tokenCountHashString
	tokenCountHashBytes
	tokenCountHashFloat
	tokenCountHashSigned
	tokenCountHashUnsigned
	tokenCountHashArray
	tokenCountHashObject
	tokenCountHashModel
)

// writeTokenCountHashValue 用类型标签、长度和有序对象键直接构造缓存摘要。
// 与先编码整棵动态 JSON 再哈希相比，它不为每个 map/interface 创建反射临时对象；
// 未知 Go 类型直接绕过缓存，绝不影响上游精确计数。
func writeTokenCountHashValue(
	buffer *bytes.Buffer,
	value any,
	remaining *int,
	depth int,
) bool {
	if depth > valueBudgetMaxDepth {
		return false
	}
	(*remaining)--
	if *remaining < 0 {
		return false
	}

	switch typed := value.(type) {
	case nil:
		writeTokenCountHashHeader(buffer, tokenCountHashNil, 0)
	case bool:
		if typed {
			writeTokenCountHashHeader(buffer, tokenCountHashTrue, 0)
		} else {
			writeTokenCountHashHeader(buffer, tokenCountHashFalse, 0)
		}
	case string:
		*remaining -= len(typed)
		if *remaining < 0 {
			return false
		}
		writeTokenCountHashString(buffer, tokenCountHashString, typed)
	case []byte:
		*remaining -= len(typed)
		if *remaining < 0 {
			return false
		}
		writeTokenCountHashHeader(buffer, tokenCountHashBytes, uint64(len(typed)))
		_, _ = buffer.Write(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return false
		}
		writeTokenCountHashUint64(buffer, tokenCountHashFloat, math.Float64bits(typed))
	case float32:
		value64 := float64(typed)
		if math.IsNaN(value64) || math.IsInf(value64, 0) {
			return false
		}
		writeTokenCountHashUint64(buffer, tokenCountHashFloat, math.Float64bits(value64))
	case int:
		writeTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int8:
		writeTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int16:
		writeTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int32:
		writeTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int64:
		writeTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(typed))
	case uint:
		writeTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint8:
		writeTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint16:
		writeTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint32:
		writeTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint64:
		writeTokenCountHashUint64(buffer, tokenCountHashUnsigned, typed)
	case []any:
		if len(typed) > *remaining {
			return false
		}
		writeTokenCountHashHeader(buffer, tokenCountHashArray, uint64(len(typed)))
		for _, item := range typed {
			if !writeTokenCountHashValue(buffer, item, remaining, depth+1) {
				return false
			}
		}
	case map[string]any:
		if len(typed) > *remaining {
			return false
		}
		writeTokenCountHashHeader(buffer, tokenCountHashObject, uint64(len(typed)))
		var inlineKeys [8]string
		keys := inlineKeys[:0]
		if len(typed) > len(inlineKeys) {
			keys = make([]string, 0, len(typed))
		}
		for key := range typed {
			*remaining -= len(key)
			if *remaining < 0 {
				return false
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writeTokenCountHashString(buffer, tokenCountHashString, key)
			if !writeTokenCountHashValue(buffer, typed[key], remaining, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	return true
}

func writeTokenCountHashString(buffer *bytes.Buffer, tag byte, value string) {
	writeTokenCountHashHeader(buffer, tag, uint64(len(value)))
	_, _ = buffer.WriteString(value)
}

func writeTokenCountHashHeader(buffer *bytes.Buffer, tag byte, length uint64) {
	_ = buffer.WriteByte(tag)
	var encoded [binary.MaxVarintLen64]byte
	count := binary.PutUvarint(encoded[:], length)
	_, _ = buffer.Write(encoded[:count])
}

func writeTokenCountHashUint64(buffer *bytes.Buffer, tag byte, value uint64) {
	_ = buffer.WriteByte(tag)
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	_, _ = buffer.Write(encoded[:])
}

func releaseTokenCountKeyBuffer(buffer *bytes.Buffer) {
	if buffer.Cap() > maxPooledTokenKeyBytes {
		return
	}
	buffer.Reset()
	tokenCountKeyBufferPool.Put(buffer)
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
