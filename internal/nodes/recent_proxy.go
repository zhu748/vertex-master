package nodes

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

// RecentProxyStatus describes the proxy that most recently produced a
// successful user-facing upstream response. A race can have multiple active
// candidates, so this intentionally represents the latest winner, not an
// exclusive process-wide connection.
type RecentProxyStatus struct {
	Available  bool    `json:"available"`
	Direct     bool    `json:"direct"`
	RawURI     string  `json:"raw_uri,omitempty"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Address    string  `json:"address"`
	LastUsedAt int64   `json:"last_used_at"`
	Revision   uint64  `json:"revision"`
	RequestID  string  `json:"request_id,omitempty"`
	LatencyMs  float64 `json:"latency_ms,omitempty"`
}

type RecentProxyEvent struct {
	Direct    bool    `json:"direct"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Address   string  `json:"address"`
	UsedAt    int64   `json:"used_at"`
	RequestID string  `json:"request_id,omitempty"`
	LatencyMs float64 `json:"latency_ms,omitempty"`
}

var (
	recentProxyMu      sync.RWMutex       //nolint:gochecknoglobals
	recentProxyStatus  RecentProxyStatus  //nolint:gochecknoglobals
	recentProxyHistory []RecentProxyEvent //nolint:gochecknoglobals
)

// RecordProxySuccess records an actual successful API request winner. Health
// probes deliberately do not call this function.
func RecordProxySuccess(rawURI string) {
	RecordProxySuccessForRequest(rawURI, "", 0)
}

// RecordProxySuccessForRequest records safe request correlation and measured
// winner latency. History never contains raw proxy credentials.
func RecordProxySuccessForRequest(rawURI, requestID string, latencyMs float64) {
	rawURI = strings.TrimSpace(rawURI)
	requestID = sanitizeRecentRequestID(requestID)
	status := RecentProxyStatus{
		Available:  true,
		RawURI:     rawURI,
		LastUsedAt: time.Now().Unix(),
		RequestID:  requestID,
		LatencyMs:  max(0, latencyMs),
	}
	if rawURI == "" {
		status.Direct = true
		status.Name = "直连"
		status.Type = "direct"
		status.Address = "未使用代理"
	} else {
		status.Type, status.Address = proxyTypeAndAddress(rawURI)
		status.Name = status.Address

		mu.Lock()
		ensureLoaded()
		for _, node := range nodeList {
			if node.RawURI != rawURI {
				continue
			}
			status.Name = SafeNodeLabel(node.Name)
			if strings.TrimSpace(node.Type) != "" {
				status.Type = strings.ToLower(strings.TrimSpace(node.Type))
			}
			break
		}
		mu.Unlock()
	}

	recentProxyMu.Lock()
	status.Revision = recentProxyStatus.Revision + 1
	recentProxyStatus = status
	event := RecentProxyEvent{
		Direct: status.Direct, Name: status.Name, Type: status.Type, Address: status.Address,
		UsedAt: status.LastUsedAt, RequestID: status.RequestID, LatencyMs: status.LatencyMs,
	}
	recentProxyHistory = append([]RecentProxyEvent{event}, recentProxyHistory...)
	if len(recentProxyHistory) > 20 {
		recentProxyHistory = recentProxyHistory[:20]
	}
	recentProxyMu.Unlock()
}

func GetRecentProxyStatus() RecentProxyStatus {
	recentProxyMu.RLock()
	defer recentProxyMu.RUnlock()
	return recentProxyStatus
}

func GetRecentProxyHistory(limit int) []RecentProxyEvent {
	recentProxyMu.RLock()
	defer recentProxyMu.RUnlock()
	if limit <= 0 || limit > len(recentProxyHistory) {
		limit = len(recentProxyHistory)
	}
	out := make([]RecentProxyEvent, limit)
	copy(out, recentProxyHistory[:limit])
	return out
}

func sanitizeRecentRequestID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		value = value[:64]
	}
	var builder strings.Builder
	for _, char := range value {
		if char >= 0x21 && char <= 0x7e {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func proxyTypeAndAddress(rawURI string) (string, string) {
	parsed, err := url.Parse(rawURI)
	if err == nil {
		proxyType := strings.ToLower(strings.TrimSpace(parsed.Scheme))
		if proxyType == "" {
			proxyType = "proxy"
		}
		if parsed.Host != "" {
			return proxyType, parsed.Host
		}
		if parsed.Opaque != "" {
			return proxyType, "复杂协议节点"
		}
	}
	return "proxy", "代理节点"
}
