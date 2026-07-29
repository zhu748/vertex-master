package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/netx"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

const (
	minProxyRefreshMinutes    = 1
	maxProxyRefreshMinutes    = 7 * 24 * 60
	maxProxySubscriptionNodes = 50000
	proxyRefreshConcurrency   = 4

	environmentProxySubscriptionKey            = "environment:proxy-subscription"
	defaultEnvironmentProxyType                = "http"
	defaultEnvironmentProxyRefreshIntervalMins = 60
)

type subscriptionRefreshLockEntry struct {
	mutex sync.Mutex
	refs  int
}

var subscriptionRefreshLockRegistry = struct { //nolint:gochecknoglobals
	sync.Mutex
	entries map[int64]*subscriptionRefreshLockEntry
}{entries: make(map[int64]*subscriptionRefreshLockEntry)}

func proxySubscriptionFromEnvironment() (*nodes.ProxySubscription, error) {
	rawURL := strings.TrimSpace(os.Getenv("VPROXY_PROXY_SUBSCRIPTION_URL"))
	if rawURL == "" {
		return nil, nil
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" ||
		(parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return nil, errors.New("VPROXY_PROXY_SUBSCRIPTION_URL 必须是有效的 HTTP/HTTPS URL")
	}

	proxyType := strings.ToLower(strings.TrimSpace(os.Getenv("VPROXY_PROXY_SUBSCRIPTION_TYPE")))
	if proxyType == "" {
		proxyType = defaultEnvironmentProxyType
	}
	if proxyType != "auto" && !isStandardProxyType(proxyType) {
		return nil, errors.New("VPROXY_PROXY_SUBSCRIPTION_TYPE 不支持该代理类型")
	}

	refreshInterval := defaultEnvironmentProxyRefreshIntervalMins
	if rawInterval := strings.TrimSpace(os.Getenv("VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES")); rawInterval != "" {
		refreshInterval, err = strconv.Atoi(rawInterval)
		if err != nil ||
			refreshInterval < minProxyRefreshMinutes ||
			refreshInterval > maxProxyRefreshMinutes {
			return nil, errors.New("VPROXY_PROXY_SUBSCRIPTION_INTERVAL_MINUTES 必须是 1 到 10080 的整数")
		}
	}

	name := "环境变量代理池"
	if hostname := parsedURL.Hostname(); hostname != "" {
		name += " (" + hostname + ")"
	}
	return &nodes.ProxySubscription{
		ManagedKey:             environmentProxySubscriptionKey,
		Name:                   name,
		URL:                    rawURL,
		ProxyType:              proxyType,
		RefreshIntervalMinutes: refreshInterval,
		Enabled:                true,
	}, nil
}

// SyncEnvironmentProxySubscription 将 Render 环境变量同步为一个受托管的代理池订阅。
func SyncEnvironmentProxySubscription() error {
	item, err := proxySubscriptionFromEnvironment()
	if err != nil {
		return err
	}
	if item == nil {
		existing, getErr := nodes.GetManagedProxySubscription(environmentProxySubscriptionKey)
		if errors.Is(getErr, sql.ErrNoRows) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		release := acquireProxySubscriptionLock(existing.ID)
		defer release()
		removed, err := nodes.DeleteProxySubscriptionAndNodes(existing.ID)
		if err != nil {
			return err
		}
		log.Printf("[ProxyPool] 已移除环境变量托管订阅及 %d 个节点", removed)
		return nil
	}

	saved, err := nodes.UpsertManagedProxySubscription(environmentProxySubscriptionKey, *item)
	if err != nil {
		return err
	}
	log.Printf("[ProxyPool] 已同步环境变量代理订阅 %q：%s，类型 %s，每 %d 分钟刷新",
		saved.Name, subscriptionURLForLog(saved.URL), saved.ProxyType, saved.RefreshIntervalMinutes)
	return nil
}

// acquireProxySubscriptionLock 串行化同一订阅的刷新/删除，并在最后一个调用者
// 退出后回收锁条目，避免长期创建删除订阅导致全局注册表无限增长。
func acquireProxySubscriptionLock(id int64) func() {
	subscriptionRefreshLockRegistry.Lock()
	entry := subscriptionRefreshLockRegistry.entries[id]
	if entry == nil {
		entry = &subscriptionRefreshLockEntry{}
		subscriptionRefreshLockRegistry.entries[id] = entry
	}
	entry.refs++
	subscriptionRefreshLockRegistry.Unlock()

	entry.mutex.Lock()
	return func() {
		// 必须先释放条目锁再减引用；否则最后一个调用者删除注册项后，
		// 新调用者可能拿到另一把锁并与旧临界区重叠。
		entry.mutex.Unlock()
		subscriptionRefreshLockRegistry.Lock()
		entry.refs--
		if entry.refs == 0 && subscriptionRefreshLockRegistry.entries[id] == entry {
			delete(subscriptionRefreshLockRegistry.entries, id)
		}
		subscriptionRefreshLockRegistry.Unlock()
	}
}

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
	if err := nodes.MergeNodes([]nodes.Node{node}); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("保存代理失败: "+err.Error()))
		return
	}
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
	if body.ID > 0 {
		current, err := nodes.GetProxySubscription(body.ID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, adminErr("订阅不存在"))
			return
		}
		if current.ManagedKey != "" {
			writeJSON(w, http.StatusConflict, adminErr("该订阅由 Render 环境变量托管，请在 Render 中修改配置"))
			return
		}
	}
	rawURL := strings.TrimSpace(body.URL)
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		writeJSON(w, http.StatusBadRequest, adminErr("订阅地址必须是有效的 HTTP/HTTPS URL"))
		return
	}
	if _, err = netx.ValidateHTTPURL(
		r.Context(),
		rawURL,
		adm.cfg.AllowPrivateSubscriptionURLs(),
	); err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("订阅地址被安全策略拒绝: "+err.Error()))
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
		count, refreshErr := adm.refreshProxySubscription(r.Context(), item)
		if updated, getErr := nodes.GetProxySubscription(item.ID); getErr == nil {
			item = updated
		}
		result["subscription"] = item
		if refreshErr != nil {
			result["refresh_ok"] = false
			result["refresh_error"] = refreshErr.Error()
			result["count"] = item.NodeCount
			writeJSON(w, http.StatusOK, result)
			return
		}
		result["refresh_ok"] = true
		result["count"] = count
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
	item, err := nodes.GetProxySubscription(body.ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, adminErr("订阅不存在"))
		return
	}
	if item.ManagedKey != "" {
		writeJSON(w, http.StatusConflict, adminErr("该订阅由 Render 环境变量托管，请在 Render 中移除配置"))
		return
	}
	release := acquireProxySubscriptionLock(body.ID)
	defer release()
	item, err = nodes.GetProxySubscription(body.ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, adminErr("订阅不存在"))
		return
	}
	if item.ManagedKey != "" {
		writeJSON(w, http.StatusConflict, adminErr("该订阅由 Render 环境变量托管，请在 Render 中移除配置"))
		return
	}
	removed, err := nodes.DeleteProxySubscriptionAndNodes(body.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_count": removed})
}

func (adm *AdminHandler) refreshProxySubscription(ctx context.Context, item nodes.ProxySubscription) (int, error) {
	release := acquireProxySubscriptionLock(item.ID)
	defer release()

	current, err := nodes.GetProxySubscription(item.ID)
	if err != nil {
		return 0, errors.New("订阅不存在或已删除")
	}
	item = current

	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	text, err := adm.fetchSubscriptionText(ctx, item.URL)
	if err != nil {
		_ = nodes.UpdateProxySubscriptionResult(item.ID, item.NodeCount, err)
		return 0, err
	}
	imported := parseProxyListNodes(text, item.ProxyType)
	imported, rejected, err := filterRemoteSubscriptionNodes(
		ctx,
		imported,
		adm.cfg.AllowPrivateSubscriptionURLs(),
		adm.cfg.AllowDomainSubscriptionProxies(),
	)
	if err != nil {
		_ = nodes.UpdateProxySubscriptionResult(item.ID, item.NodeCount, err)
		return 0, err
	}
	if rejected > 0 {
		log.Printf("[ProxyPool] 订阅 %q 已过滤 %d 个非公网或无法解析的代理端点", item.Name, rejected)
	}
	if len(imported) == 0 {
		err = errors.New("订阅内容中没有解析到安全且有效的公网代理")
		_ = nodes.UpdateProxySubscriptionResult(item.ID, item.NodeCount, err)
		return 0, err
	}
	if len(imported) > maxProxySubscriptionNodes {
		err = fmt.Errorf("订阅包含 %d 个代理，超过单订阅上限 %d", len(imported), maxProxySubscriptionNodes)
		_ = nodes.UpdateProxySubscriptionResult(item.ID, item.NodeCount, err)
		return 0, err
	}
	syncResult, err := nodes.SyncSubscriptionNodesAndMarkRefreshed(item.ID, imported)
	if err != nil {
		_ = nodes.UpdateProxySubscriptionResult(item.ID, item.NodeCount, err)
		return 0, err
	}
	log.Printf("[ProxyPool] 订阅 %q 刷新完成：解析 %d，当前 %d，新增 %d，删除 %d",
		item.Name, len(imported), syncResult.Count, syncResult.Added, syncResult.Removed)
	return syncResult.Count, nil
}

// StartProxySubscriptionScheduler 启动持久化代理订阅的自动刷新器。
func StartProxySubscriptionScheduler(vc *vertex.VertexAIClient, cfg config.ConfigProvider) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	adm := &AdminHandler{handler: handler{vc: vc, cfg: cfg}} //nolint:exhaustruct
	refreshDue := func() {
		items, err := nodes.DueProxySubscriptions(time.Now())
		if err != nil {
			log.Printf("[ProxyPool] 读取定时订阅失败: %v", err)
			return
		}
		refreshProxySubscriptions(ctx, items, adm.refreshProxySubscription)
	}
	go func() {
		defer close(done)
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
	return func() {
		cancel()
		<-done
	}
}

func refreshProxySubscriptions(
	ctx context.Context,
	items []nodes.ProxySubscription,
	refresh func(context.Context, nodes.ProxySubscription) (int, error),
) {
	workerCount := min(proxyRefreshConcurrency, len(items))
	if workerCount == 0 {
		return
	}

	jobs := make(chan nodes.ProxySubscription)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for subscription := range jobs {
				if ctx.Err() != nil {
					return
				}
				if _, err := refresh(ctx, subscription); err != nil {
					log.Printf("[ProxyPool] 自动刷新订阅 %q 失败: %v", subscription.Name, err)
				}
			}
		}()
	}

enqueue:
	for _, item := range items {
		select {
		case jobs <- item:
		case <-ctx.Done():
			break enqueue
		}
	}
	close(jobs)
	workers.Wait()
}
