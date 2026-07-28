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

const recentProxyHistoryLimit = 20

// RecordProxySuccess records an actual successful API request winner. Health
// probes deliberately do not call this function.
func RecordProxySuccess(rawURI string) {
	RecordProxySuccessForRequest(rawURI, "", 0)
}

// RecordProxySuccessForRequest records safe request correlation and measured
// winner latency. History never contains raw proxy credentials.
func RecordProxySuccessForRequest(rawURI, requestID string, latencyMs float64) {
	recordProxySuccess(rawURI, requestID, latencyMs, nil)
}

// RecordProxySuccessForNode records a winner whose node metadata is already
// available to the caller, avoiding another global node-list lock and scan.
func RecordProxySuccessForNode(node Node, requestID string, latencyMs float64) {
	recordProxySuccess(node.RawURI, requestID, latencyMs, &node)
}

func recordProxySuccess(rawURI, requestID string, latencyMs float64, knownNode *Node) {
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
		if knownNode != nil && strings.TrimSpace(knownNode.RawURI) == rawURI {
			status.Name = SafeNodeLabel(knownNode.Name)
			if strings.TrimSpace(knownNode.Type) != "" {
				status.Type = strings.ToLower(strings.TrimSpace(knownNode.Type))
			}
		} else {
			lockLoadedForRead()
			if node, found := lookupNodeUnsafe(rawURI); found {
				status.Name = SafeNodeLabel(node.Name)
				if strings.TrimSpace(node.Type) != "" {
					status.Type = strings.ToLower(strings.TrimSpace(node.Type))
				}
			}
			mu.RUnlock()
		}
	}

	recentProxyMu.Lock()
	status.Revision = recentProxyStatus.Revision + 1
	recentProxyStatus = status
	event := RecentProxyEvent{
		Direct: status.Direct, Name: status.Name, Type: status.Type, Address: status.Address,
		UsedAt: status.LastUsedAt, RequestID: status.RequestID, LatencyMs: status.LatencyMs,
	}
	if len(recentProxyHistory) < recentProxyHistoryLimit {
		recentProxyHistory = append(recentProxyHistory, RecentProxyEvent{})
	}
	copy(recentProxyHistory[1:], recentProxyHistory[:len(recentProxyHistory)-1])
	recentProxyHistory[0] = event
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
	valid := true
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			valid = false
			break
		}
	}
	if valid {
		return value
	}

	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] >= 0x21 && value[index] <= 0x7e {
			builder.WriteByte(value[index])
		}
	}
	return builder.String()
}

func proxyTypeAndAddress(rawURI string) (string, string) {
	if proxyType, address, ok := standardProxyAuthority(rawURI); ok {
		return proxyType, address
	}
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

// standardProxyAuthority 处理节点列表最常见的 scheme://userinfo@host 形状。
// 只返回 authority 中去除凭据后的 host；遇到不规范 scheme、空 host 或 opaque
// URI 时交回 net/url，确保复杂协议的兼容行为不变。
func standardProxyAuthority(rawURI string) (string, string, bool) {
	separator := strings.Index(rawURI, "://")
	if separator <= 0 || !validProxyScheme(rawURI[:separator]) {
		return "", "", false
	}
	authority := rawURI[separator+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	if authority == "" {
		return "", "", false
	}
	return strings.ToLower(rawURI[:separator]), authority, true
}

func validProxyScheme(scheme string) bool {
	for index := 0; index < len(scheme); index++ {
		value := scheme[index]
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(index > 0 && ((value >= '0' && value <= '9') || value == '+' || value == '-' || value == '.')) {
			continue
		}
		return false
	}
	return scheme != ""
}
