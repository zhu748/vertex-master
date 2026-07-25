package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

const (
	minProxyRefreshMinutes = 1
	maxProxyRefreshMinutes = 7 * 24 * 60
)

var subscriptionRefreshMu sync.Mutex //nolint:gochecknoglobals

func (adm *AdminHandler) adminAddStandardProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type     string `json:"type"`
		Address  string `json:"address"`
		Username string `json:"username"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	proxyType := strings.ToLower(strings.TrimSpace(body.Type))
	if !isStandardProxyType(proxyType) {
		writeJSON(w, http.StatusBadRequest, adminErr("不支持的代理类型"))
		return
	}
	raw, ok := normalizeProxyListLine(body.Address, proxyType)
	if !ok {
		writeJSON(w, http.StatusBadRequest, adminErr("代理地址格式无效，请填写 host:port 或完整代理 URI"))
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("代理地址格式无效"))
		return
	}
	if strings.TrimSpace(body.Username) != "" {
		u.User = url.UserPassword(strings.TrimSpace(body.Username), body.Password)
	}
	if name := strings.TrimSpace(body.Name); name != "" {
		u.Fragment = name
	}
	node, ok := parseImportedNodeLine(u.String())
	if !ok {
		writeJSON(w, http.StatusBadRequest, adminErr("无法解析代理地址"))
		return
	}
	nodes.MergeNodes([]nodes.Node{node})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": node})
}

func (adm *AdminHandler) adminListProxySubscriptions(w http.ResponseWriter, _ *http.Request) {
	items, err := nodes.ListProxySubscriptions()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": items})
}

func (adm *AdminHandler) adminSaveProxySubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID                     int64  `json:"id"`
		Name                   string `json:"name"`
		URL                    string `json:"url"`
		ProxyType              string `json:"proxy_type"`
		RefreshIntervalMinutes int    `json:"refresh_interval_minutes"`
		Enabled                *bool  `json:"enabled"`
		RefreshNow             bool   `json:"refresh_now"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	rawURL := strings.TrimSpace(body.URL)
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		writeJSON(w, http.StatusBadRequest, adminErr("订阅地址必须是有效的 HTTP/HTTPS URL"))
		return
	}
	proxyType := strings.ToLower(strings.TrimSpace(body.ProxyType))
	if proxyType == "" {
		proxyType = "auto"
	}
	if proxyType != "auto" && !isStandardProxyType(proxyType) {
		writeJSON(w, http.StatusBadRequest, adminErr("不支持的代理类型"))
		return
	}
	if body.RefreshIntervalMinutes < minProxyRefreshMinutes ||
		body.RefreshIntervalMinutes > maxProxyRefreshMinutes {
		writeJSON(w, http.StatusBadRequest, adminErr("刷新间隔必须在 1 到 10080 分钟之间"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = parsedURL.Host
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	item, err := nodes.SaveProxySubscription(nodes.ProxySubscription{
		ID:                     body.ID,
		Name:                   name,
		URL:                    rawURL,
		ProxyType:              proxyType,
		RefreshIntervalMinutes: body.RefreshIntervalMinutes,
		Enabled:                enabled,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr(err.Error()))
		return
	}

	result := map[string]any{"ok": true, "subscription": item}
	if body.RefreshNow {
		added, refreshErr := adm.refreshProxySubscription(r.Context(), item)
		if refreshErr != nil {
			writeJSON(w, http.StatusBadRequest, adminErr("订阅已保存，但首次刷新失败: "+refreshErr.Error()))
			return
		}
		result["count"] = added
		item, _ = nodes.GetProxySubscription(item.ID)
		result["subscription"] = item
	}
	writeJSON(w, http.StatusOK, result)
}

func (adm *AdminHandler) adminRefreshProxySubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int64 `json:"id"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	item, err := nodes.GetProxySubscription(body.ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, adminErr("订阅不存在"))
		return
	}
	count, err := adm.refreshProxySubscription(r.Context(), item)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": count})
}

func (adm *AdminHandler) adminDeleteProxySubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int64 `json:"id"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if _, err := nodes.GetProxySubscription(body.ID); err != nil {
		writeJSON(w, http.StatusNotFound, adminErr("订阅不存在"))
		return
	}
	removed := nodes.DeleteSubscriptionNodes(body.ID)
	if err := nodes.DeleteProxySubscription(body.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_count": removed})
}

func (adm *AdminHandler) refreshProxySubscription(ctx context.Context, item nodes.ProxySubscription) (int, error) {
	subscriptionRefreshMu.Lock()
	defer subscriptionRefreshMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	text, err := adm.fetchSubscriptionText(ctx, item.URL)
	if err != nil {
		_ = nodes.UpdateProxySubscriptionResult(item.ID, item.NodeCount, err)
		return 0, err
	}
	imported := parseProxyListNodes(text, item.ProxyType)
	if len(imported) == 0 {
		err = errors.New("订阅内容中没有解析到有效代理")
		_ = nodes.UpdateProxySubscriptionResult(item.ID, item.NodeCount, err)
		return 0, err
	}
	added, removed := nodes.ReplaceSubscriptionNodes(item.ID, imported)
	if err := nodes.UpdateProxySubscriptionResult(item.ID, added, nil); err != nil {
		return 0, err
	}
	log.Printf("[ProxyPool] 订阅 %q 刷新完成：解析 %d，加入 %d，替换旧节点 %d",
		item.Name, len(imported), added, removed)
	return added, nil
}

// StartProxySubscriptionScheduler 启动持久化代理订阅的自动刷新器。
func StartProxySubscriptionScheduler(vc *vertex.VertexAIClient, cfg config.ConfigProvider) func() {
	ctx, cancel := context.WithCancel(context.Background())
	adm := &AdminHandler{handler: handler{vc: vc, cfg: cfg}} //nolint:exhaustruct
	refreshDue := func() {
		items, err := nodes.DueProxySubscriptions(time.Now())
		if err != nil {
			log.Printf("[ProxyPool] 读取定时订阅失败: %v", err)
			return
		}
		for _, item := range items {
			if ctx.Err() != nil {
				return
			}
			if _, err := adm.refreshProxySubscription(ctx, item); err != nil {
				log.Printf("[ProxyPool] 自动刷新订阅 %q 失败: %v", item.Name, err)
			}
		}
	}
	go func() {
		refreshDue()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refreshDue()
			case <-ctx.Done():
				return
			}
		}
	}()
	return cancel
}
