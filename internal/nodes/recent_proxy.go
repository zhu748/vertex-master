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
	Available  bool   `json:"available"`
	Direct     bool   `json:"direct"`
	RawURI     string `json:"raw_uri,omitempty"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Address    string `json:"address"`
	LastUsedAt int64  `json:"last_used_at"`
	Revision   uint64 `json:"revision"`
}

var (
	recentProxyMu     sync.RWMutex      //nolint:gochecknoglobals
	recentProxyStatus RecentProxyStatus //nolint:gochecknoglobals
)

// RecordProxySuccess records an actual successful API request winner. Health
// probes deliberately do not call this function.
func RecordProxySuccess(rawURI string) {
	rawURI = strings.TrimSpace(rawURI)
	status := RecentProxyStatus{
		Available:  true,
		RawURI:     rawURI,
		LastUsedAt: time.Now().Unix(),
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
	recentProxyMu.Unlock()
}

func GetRecentProxyStatus() RecentProxyStatus {
	recentProxyMu.RLock()
	defer recentProxyMu.RUnlock()
	return recentProxyStatus
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
