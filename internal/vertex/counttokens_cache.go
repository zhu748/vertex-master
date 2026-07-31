package vertex

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
)

const (
	tokenCountCacheTTL      = 5 * time.Minute
	tokenCountCacheMaxItems = 256
	tokenCountCacheMaxBytes = 256 << 10
	maxPooledTokenKeyBytes  = 64 << 10
)

var tokenCountKeyBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any {
		buffer := make([]byte, 0, 512)
		return &buffer
	},
}

type tokenCountCacheKey [sha256.Size]byte

type tokenCountCacheEntry struct {
	count    int
	storedAt time.Time
}

type tokenCountFlight struct {
	done  chan struct{}
	count int
	err   error
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
	bufferPointer := tokenCountKeyBufferPool.Get().(*[]byte)
	buffer := (*bufferPointer)[:0]
	buffer = appendTokenCountHashString(buffer, tokenCountHashModel, model)
	buffer, ok := appendTokenCountHashArray(buffer, contents, &remaining, 0)
	if !ok {
		releaseTokenCountKeyBuffer(bufferPointer, buffer)
		return tokenCountCacheKey{}, false
	}
	key := tokenCountCacheKey(sha256.Sum256(buffer))
	releaseTokenCountKeyBuffer(bufferPointer, buffer)
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

// appendTokenCountHashValue 用类型标签、长度和有序对象键直接构造缓存摘要。
// 与先编码整棵动态 JSON 再哈希相比，它不为每个 map/interface 创建反射临时对象；
// 未知 Go 类型直接绕过缓存，绝不影响上游精确计数。
func appendTokenCountHashValue(
	buffer []byte,
	value any,
	remaining *int,
	depth int,
) ([]byte, bool) {
	if depth > valueBudgetMaxDepth {
		return buffer, false
	}
	(*remaining)--
	if *remaining < 0 {
		return buffer, false
	}

	switch typed := value.(type) {
	case nil:
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashNil, 0)
	case bool:
		if typed {
			buffer = appendTokenCountHashHeader(buffer, tokenCountHashTrue, 0)
		} else {
			buffer = appendTokenCountHashHeader(buffer, tokenCountHashFalse, 0)
		}
	case string:
		*remaining -= len(typed)
		if *remaining < 0 {
			return buffer, false
		}
		buffer = appendTokenCountHashString(buffer, tokenCountHashString, typed)
	case []byte:
		*remaining -= len(typed)
		if *remaining < 0 {
			return buffer, false
		}
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashBytes, uint64(len(typed)))
		buffer = append(buffer, typed...)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return buffer, false
		}
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashFloat, math.Float64bits(typed))
	case float32:
		value64 := float64(typed)
		if math.IsNaN(value64) || math.IsInf(value64, 0) {
			return buffer, false
		}
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashFloat, math.Float64bits(value64))
	case int:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int8:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int16:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int32:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(int64(typed)))
	case int64:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashSigned, uint64(typed))
	case uint:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint8:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint16:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint32:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashUnsigned, uint64(typed))
	case uint64:
		buffer = appendTokenCountHashUint64(buffer, tokenCountHashUnsigned, typed)
	case canonicalTextContent:
		role, text, ok := typed.CanonicalTextContent()
		if !ok || !consumeCanonicalTextContentBudget(remaining, role, text) {
			return buffer, false
		}
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashObject, 2)
		buffer = appendTokenCountHashString(buffer, tokenCountHashString, "parts")
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashArray, 1)
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashObject, 1)
		buffer = appendTokenCountHashString(buffer, tokenCountHashString, "text")
		buffer = appendTokenCountHashString(buffer, tokenCountHashString, text)
		buffer = appendTokenCountHashString(buffer, tokenCountHashString, "role")
		buffer = appendTokenCountHashString(buffer, tokenCountHashString, role)
	case jsonx.CanonicalObjectView:
		fieldCount, ok := typed.CanonicalJSONFieldCount()
		if !ok || fieldCount > *remaining {
			return buffer, false
		}
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashObject, uint64(fieldCount))
		for index := range fieldCount {
			key, item := typed.CanonicalJSONField(index)
			buffer, ok = appendTokenCountHashObjectKey(buffer, key, remaining)
			if !ok {
				return buffer, false
			}
			buffer, ok = appendTokenCountHashValue(buffer, item, remaining, depth+1)
			if !ok {
				return buffer, false
			}
		}
	case jsonx.CanonicalArrayView:
		itemCount, ok := typed.CanonicalJSONItemCount()
		if !ok || itemCount > *remaining {
			return buffer, false
		}
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashArray, uint64(itemCount))
		for index := range itemCount {
			buffer, ok = appendTokenCountHashValue(
				buffer,
				typed.CanonicalJSONItem(index),
				remaining,
				depth+1,
			)
			if !ok {
				return buffer, false
			}
		}
	case []any:
		return appendTokenCountHashArrayItems(buffer, typed, remaining, depth)
	case map[string]any:
		if len(typed) > *remaining {
			return buffer, false
		}
		buffer = appendTokenCountHashHeader(buffer, tokenCountHashObject, uint64(len(typed)))
		var inlineKeys [8]string
		keys := inlineKeys[:0]
		if len(typed) > len(inlineKeys) {
			keys = make([]string, 0, len(typed))
		}
		for key := range typed {
			*remaining -= len(key)
			if *remaining < 0 {
				return buffer, false
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			buffer = appendTokenCountHashString(buffer, tokenCountHashString, key)
			var ok bool
			buffer, ok = appendTokenCountHashValue(buffer, typed[key], remaining, depth+1)
			if !ok {
				return buffer, false
			}
		}
	default:
		return buffer, false
	}
	return buffer, true
}

func appendTokenCountHashObjectKey(
	buffer []byte,
	key string,
	remaining *int,
) ([]byte, bool) {
	*remaining -= len(key)
	if *remaining < 0 {
		return buffer, false
	}
	return appendTokenCountHashString(buffer, tokenCountHashString, key), true
}

func appendTokenCountHashArray(
	buffer []byte,
	values []any,
	remaining *int,
	depth int,
) ([]byte, bool) {
	if depth > valueBudgetMaxDepth {
		return buffer, false
	}
	(*remaining)--
	if *remaining < 0 {
		return buffer, false
	}
	return appendTokenCountHashArrayItems(buffer, values, remaining, depth)
}

func appendTokenCountHashArrayItems(
	buffer []byte,
	values []any,
	remaining *int,
	depth int,
) ([]byte, bool) {
	if len(values) > *remaining {
		return buffer, false
	}
	buffer = appendTokenCountHashHeader(buffer, tokenCountHashArray, uint64(len(values)))
	for _, item := range values {
		var ok bool
		buffer, ok = appendTokenCountHashValue(buffer, item, remaining, depth+1)
		if !ok {
			return buffer, false
		}
	}
	return buffer, true
}

func appendTokenCountHashString(buffer []byte, tag byte, value string) []byte {
	buffer = appendTokenCountHashHeader(buffer, tag, uint64(len(value)))
	return append(buffer, value...)
}

func appendTokenCountHashHeader(buffer []byte, tag byte, length uint64) []byte {
	buffer = append(buffer, tag)
	return binary.AppendUvarint(buffer, length)
}

func appendTokenCountHashUint64(buffer []byte, tag byte, value uint64) []byte {
	return append(
		buffer,
		tag,
		byte(value),
		byte(value>>8),
		byte(value>>16),
		byte(value>>24),
		byte(value>>32),
		byte(value>>40),
		byte(value>>48),
		byte(value>>56),
	)
}

func releaseTokenCountKeyBuffer(bufferPointer *[]byte, buffer []byte) {
	if cap(buffer) > maxPooledTokenKeyBytes {
		return
	}
	*bufferPointer = buffer[:0]
	tokenCountKeyBufferPool.Put(bufferPointer)
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
	err error,
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
	flight.err = err
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
