package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/netx"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/recaptcha"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

const maxSubscriptionResponseBytes int64 = 10 << 20
const proxyBatchTestBudget = time.Hour

var (
	proxyTestTaskMu     sync.Mutex         //nolint:gochecknoglobals
	proxyTestTaskCancel context.CancelFunc //nolint:gochecknoglobals
)

func (adm *AdminHandler) adminGetNodes(w http.ResponseWriter, r *http.Request) {
	queryValues := r.URL.Query()
	query := strings.ToLower(strings.TrimSpace(queryValues.Get("query")))
	nodeType := strings.ToLower(strings.TrimSpace(queryValues.Get("type")))
	status := strings.ToLower(strings.TrimSpace(queryValues.Get("status")))
	source := strings.ToLower(strings.TrimSpace(queryValues.Get("source")))
	urisOnly := queryValues.Get("uris_only") == "true"
	queryMatcher := newAdminNodeQueryMatcher(query)
	match := func(node nodes.Node, nodeHealth *nodes.NodeHealth) bool {
		if query != "" && !queryMatcher.Contains(node.Name) &&
			!queryMatcher.Contains(node.RawURI) &&
			!queryMatcher.Contains(node.Type) {
			return false
		}
		if nodeType != "" && nodeType != "all" && !strings.EqualFold(node.Type, nodeType) {
			return false
		}
		if source == "manual" && node.SourceID != 0 {
			return false
		}
		if source == "subscription" && node.SubscriptionSourceCount == 0 {
			return false
		}
		return nodeMatchesAdminStatus(node, nodeHealth, status)
	}

	if urisOnly {
		filteredURIs := nodes.LoadFilteredNodeURIs(match)
		writeAdminNodeURIsJSON(w, filteredURIs)
		return
	}

	page, _ := strconv.Atoi(queryValues.Get("page"))
	pageSize, _ := strconv.Atoi(queryValues.Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize > 200 {
		pageSize = 200
	}
	snapshot := nodes.LoadNodePoolPageSnapshot(time.Now(), page, pageSize, match)
	pageNodes := snapshot.Nodes
	pageHealth := make(map[string]*nodes.NodeHealth, len(pageNodes))
	for index, node := range pageNodes {
		if snapshot.HasHealth[index] {
			pageHealth[node.RawURI] = &snapshot.Health[index]
		}
	}

	sp := nodes.GetStickyPool()
	poolStats := snapshot.Stats
	healthCycleEstimateMinutes := 0
	if adm.cfg.ProxyHealthCheckEnabled() && poolStats.Enabled > 0 {
		batchSize := max(1, adm.cfg.ProxyHealthCheckBatchSize())
		rounds := (poolStats.Enabled + batchSize - 1) / batchSize
		healthCycleEstimateMinutes = rounds * max(1, adm.cfg.ProxyHealthCheckIntervalMinutes())
	}
	// Keep fields in the same lexicographic order used by encoding/json for
	// string-keyed maps so the response remains byte-for-byte compatible.
	writeJSON(w, http.StatusOK, adminNodesPageResponse{
		DisabledCount:              poolStats.Disabled,
		EnabledCount:               poolStats.Enabled,
		Health:                     pageHealth,
		HealthCycleEstimateMinutes: healthCycleEstimateMinutes,
		HealthScheduler:            GetProxyHealthSchedulerStatus(),
		Nodes:                      pageNodes,
		OverallTotal:               poolStats.Total,
		Page:                       snapshot.Page,
		PageSize:                   snapshot.PageSize,
		PoolStats:                  poolStats,
		RecentProxy:                nodes.GetRecentProxyStatus(),
		RecentProxyHistory:         nodes.GetRecentProxyHistory(10),
		StickyNodePriority:         adm.cfg.StickyNodePriority(),
		StickyPoolAvailable:        sp.AvailableCount(),
		StickyPoolInUse:            sp.StaleCount(),
		Total:                      snapshot.TotalMatches,
		TotalPages:                 snapshot.TotalPages,
	})
}

type adminNodeQueryMatcher struct {
	query string
	ascii bool
}

func newAdminNodeQueryMatcher(lowerQuery string) adminNodeQueryMatcher {
	for index := range len(lowerQuery) {
		if lowerQuery[index] >= 0x80 {
			return adminNodeQueryMatcher{query: lowerQuery}
		}
	}
	return adminNodeQueryMatcher{query: lowerQuery, ascii: true}
}

func (matcher adminNodeQueryMatcher) Contains(value string) bool {
	if matcher.query == "" {
		return true
	}
	if !matcher.ascii {
		return strings.Contains(strings.ToLower(value), matcher.query)
	}

	hasUpper := false
	for index := range len(value) {
		current := value[index]
		if current >= 0x80 {
			return strings.Contains(strings.ToLower(value), matcher.query)
		}
		if current >= 'A' && current <= 'Z' {
			hasUpper = true
		}
	}
	if !hasUpper {
		return strings.Contains(value, matcher.query)
	}
	return containsFoldedASCII(value, matcher.query)
}

func containsFoldedASCII(value, lowerQuery string) bool {
	if len(lowerQuery) > len(value) {
		return false
	}
	for start := 0; start <= len(value)-len(lowerQuery); start++ {
		matched := true
		for offset := range len(lowerQuery) {
			current := value[start+offset]
			if current >= 'A' && current <= 'Z' {
				current += 'a' - 'A'
			}
			if current != lowerQuery[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

type adminNodesPageResponse struct {
	DisabledCount              int                          `json:"disabled_count"`
	EnabledCount               int                          `json:"enabled_count"`
	Health                     map[string]*nodes.NodeHealth `json:"health"`
	HealthCycleEstimateMinutes int                          `json:"health_cycle_estimate_minutes"`
	HealthScheduler            ProxyHealthSchedulerStatus   `json:"health_scheduler"`
	Nodes                      []nodes.Node                 `json:"nodes"`
	OverallTotal               int                          `json:"overall_total"`
	Page                       int                          `json:"page"`
	PageSize                   int                          `json:"page_size"`
	PoolStats                  nodes.NodePoolStats          `json:"pool_stats"`
	RecentProxy                nodes.RecentProxyStatus      `json:"recent_proxy"`
	RecentProxyHistory         []nodes.RecentProxyEvent     `json:"recent_proxy_history"`
	StickyNodePriority         bool                         `json:"sticky_node_priority"`
	StickyPoolAvailable        int                          `json:"sticky_pool_available"`
	StickyPoolInUse            int                          `json:"sticky_pool_in_use"`
	Total                      int                          `json:"total"`
	TotalPages                 int                          `json:"total_pages"`
}

type adminNodeURIsResponse struct {
	Total int      `json:"total"`
	URIs  []string `json:"uris"`
}

func writeAdminNodeURIsJSON(w http.ResponseWriter, uris []string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// This closed wire type contains only an integer and strings, so JSON type
	// serialization cannot fail. Write it directly to avoid retaining a second
	// complete encoding buffer for large node pools.
	_ = jsonx.EncodeNoTrailingNewline(w, adminNodeURIsResponse{
		Total: len(uris),
		URIs:  uris,
	})
}

func (adm *AdminHandler) adminGetRecentProxy(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"recent_proxy":         nodes.GetRecentProxyStatus(),
		"recent_proxy_history": nodes.GetRecentProxyHistory(10),
	})
}

func nodeMatchesAdminStatus(node nodes.Node, health *nodes.NodeHealth, status string) bool {
	switch status {
	case "", "all":
		return true
	case "enabled":
		return !node.Disabled
	case "disabled":
		return node.Disabled
	case "healthy":
		return health != nil && health.LastSuccessAt > 0 && health.ConsecutiveFailures == 0
	case "unhealthy":
		return health != nil && health.ConsecutiveFailures > 0
	case "untested":
		return health == nil || (health.LastSuccessAt == 0 && health.LastFailAt == 0)
	default:
		return true
	}
}

func (adm *AdminHandler) adminGetTestProgress(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, nodes.GetTestProgress())
}

func (adm *AdminHandler) adminFetchSub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	log.Printf("[Admin] [FetchSub] 开始拉取订阅 URL: %s", subscriptionURLForLog(body.URL))
	text, err := adm.fetchSubscriptionText(ctx, body.URL)
	if err != nil {
		log.Printf("[Admin] [FetchSub] 拉取失败: %v", err)
		writeJSON(w, http.StatusBadRequest, adminErr("拉取失败: "+err.Error()))
		return
	}

	newNodes := parseImportedNodes(text)
	if len(newNodes) > maxProxySubscriptionNodes {
		writeJSON(
			w,
			http.StatusBadRequest,
			adminErr(fmt.Sprintf(
				"订阅包含 %d 个代理，超过单订阅上限 %d",
				len(newNodes),
				maxProxySubscriptionNodes,
			)),
		)
		return
	}
	newNodes, rejected, err := filterRemoteSubscriptionNodes(
		ctx,
		newNodes,
		adm.cfg.AllowPrivateSubscriptionURLs(),
		adm.cfg.AllowDomainSubscriptionProxies(),
	)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("代理端点校验失败: "+err.Error()))
		return
	}
	if rejected > 0 {
		log.Printf("[Admin] [FetchSub] 已过滤 %d 个非公网或无法解析的代理端点", rejected)
	}
	if len(newNodes) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("订阅中没有安全且有效的公网代理"))
		return
	}
	if err := nodes.MergeNodes(newNodes); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("保存节点失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(newNodes)})
}

func (adm *AdminHandler) adminTestAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	options := struct {
		IncludeDisabled bool `json:"include_disabled"`
		RecoverDisabled bool `json:"recover_disabled"`
		MaxNodes        int  `json:"max_nodes"`
		Concurrency     int  `json:"concurrency"`
		TimeoutSeconds  int  `json:"timeout_seconds"`
	}{
		MaxNodes:       500,
		Concurrency:    10,
		TimeoutSeconds: 15,
	}
	if r.ContentLength != 0 && !adm.decodeAdminBody(w, r, &options) {
		return
	}
	options.Concurrency = min(max(options.Concurrency, 1), 20)
	options.TimeoutSeconds = min(max(options.TimeoutSeconds, 3), 60)
	options.MaxNodes = min(
		max(options.MaxNodes, 1),
		maxBatchTestNodes(options.Concurrency, options.TimeoutSeconds),
	)

	list := nodes.LoadNodes()
	testNodes := make([]nodes.Node, 0, min(len(list), options.MaxNodes))
	for _, node := range list {
		if node.Disabled && !options.IncludeDisabled {
			continue
		}
		testNodes = append(testNodes, node)
		if len(testNodes) >= options.MaxNodes {
			break
		}
	}
	if len(testNodes) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("没有符合条件的节点可测试"))
		return
	}
	if !nodes.TryStartTestProgress(len(testNodes)) {
		writeJSON(w, http.StatusConflict, adminErr("已有批量测速任务正在运行"))
		return
	}

	log.Printf("[Admin] [TestAll] 开始触发全局并发测速（基于 recaptchaToken 耗时）")
	taskCtx, cancel := context.WithTimeout(context.Background(), proxyBatchTestBudget)
	var netClient *transport.NetworkClient
	if adm.vc != nil {
		netClient = adm.vc.Net()
	}
	proxyTestTaskMu.Lock()
	proxyTestTaskCancel = cancel
	proxyTestTaskMu.Unlock()

	go func() {
		defer func() {
			cancel()
			proxyTestTaskMu.Lock()
			proxyTestTaskCancel = nil
			proxyTestTaskMu.Unlock()
			nodes.FinishTestProgress()
		}()

		jobs := make(chan nodes.Node)
		var wg sync.WaitGroup
		for range options.Concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for node := range jobs {
					if nodes.CheckTestControl() || taskCtx.Err() != nil {
						return
					}
					nodeCtx, nodeCancel := context.WithTimeout(
						taskCtx,
						time.Duration(options.TimeoutSeconds)*time.Second,
					)
					start := time.Now()
					testErr := testProxyNodeWithRecaptcha(
						nodeCtx,
						netClient,
						node,
						options.TimeoutSeconds,
						"admin-test-all",
					)
					nodeCancel()
					duration := float64(time.Since(start).Milliseconds())
					success := testErr == nil
					nodes.RecordTest(node.RawURI, success, duration, safeProxyTestError(testErr))
					if success && node.Disabled && options.RecoverDisabled {
						nodes.EnableNode(node.RawURI)
					}
					nodes.UpdateTestProgress(node.Name, success)
				}
			}()
		}
	sendJobs:
		for _, node := range testNodes {
			if nodes.CheckTestControl() {
				break
			}
			select {
			case jobs <- node:
			case <-taskCtx.Done():
				break sendJobs
			}
		}
		close(jobs)
		wg.Wait()
		log.Printf("[Admin] [TestAll] 全局节点测试全部结束")
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(testNodes)})
}

func maxBatchTestNodes(concurrency, timeoutSeconds int) int {
	concurrency = min(max(concurrency, 1), 20)
	timeoutSeconds = min(max(timeoutSeconds, 3), 60)
	usableSeconds := int((proxyBatchTestBudget - time.Minute) / time.Second)
	return min(1000, (usableSeconds/timeoutSeconds)*concurrency)
}

func (adm *AdminHandler) adminTestPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	nodes.PauseTestProgress()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminTestResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	nodes.ResumeTestProgress()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminTestTerminate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	nodes.TerminateTestProgress()
	proxyTestTaskMu.Lock()
	if proxyTestTaskCancel != nil {
		proxyTestTaskCancel()
	}
	proxyTestTaskMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminTestNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI         string  `json:"raw_uri"`
		AutoDisable    bool    `json:"auto_disable"`
		TimeoutSeconds float64 `json:"timeout_seconds"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.TimeoutSeconds <= 0 {
		body.TimeoutSeconds = 25
	}
	timeout := time.Duration(body.TimeoutSeconds * float64(time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	var netClient *transport.NetworkClient
	if adm.vc != nil {
		netClient = adm.vc.Net()
	}
	testErr := testProxyNodeWithRecaptcha(
		ctx,
		netClient,
		nodes.Node{RawURI: body.RawURI},
		int(body.TimeoutSeconds),
		"admin-test-node",
	)
	elapsed := float64(time.Since(start).Milliseconds())

	errStr := ""
	ok := testErr == nil
	if testErr != nil {
		if ctx.Err() != nil || errors.Is(testErr, context.DeadlineExceeded) {
			errStr = "timeout"
		} else {
			errStr = safeProxyTestError(testErr)
		}
	}

	disabled := false
	nodes.UpdateNodeTestResult(body.RawURI, ok, elapsed, errStr)
	if body.AutoDisable {
		disabled = !ok
		if !ok {
			if err := nodes.BatchUpdateNodesDisabled([]string{body.RawURI}, true); err != nil {
				writeJSON(w, http.StatusInternalServerError, adminErr("自动禁用节点失败: "+err.Error()))
				return
			}
		}
	}

	log.Printf("[Admin] [TestNode] 节点测试 %s: ok=%v elapsed=%.0fms error=%q disabled=%v", nodes.GetNodeName(body.RawURI), ok, elapsed, errStr, disabled)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         ok,
		"elapsed_ms": elapsed,
		"error":      errStr,
		"disabled":   disabled,
	})
}

func testProxyNodeWithRecaptcha(
	ctx context.Context,
	netClient *transport.NetworkClient,
	node nodes.Node,
	timeoutSeconds int,
	reqID string,
) error {
	if netClient == nil {
		return errors.New("network client unavailable")
	}
	session, err := netClient.CreateSession(timeoutSeconds, node.RawURI, reqID)
	if err != nil {
		return err
	}
	defer session.Close()
	_, err = recaptcha.FetchTokenWithSession(ctx, session)
	return err
}

func (adm *AdminHandler) adminEnableNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	ok, err := nodes.EnableNodeWithError(body.RawURI)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("启用节点失败: "+err.Error()))
		return
	}
	log.Printf("[Admin] [EnableNode] 启用节点 %s: %v", nodes.GetNodeName(body.RawURI), ok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
}

func (adm *AdminHandler) adminDedupNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_count": nodes.DedupNodes()})
}

func (adm *AdminHandler) adminDeleteDisabledNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted_count": nodes.DeleteDisabled()})
}

func (adm *AdminHandler) adminUseNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.RawURI == "" {
		if err := config.WriteSettings(map[string]any{
			"active_node_uri": "", "parallel_pool_enabled": true,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("切换并发池失败: "+err.Error()))
			return
		}
	} else {
		if err := config.WriteSettings(map[string]any{
			"active_node_uri": body.RawURI, "parallel_pool_enabled": false,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("切换活动节点失败: "+err.Error()))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminSortNodesByLatency(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Desc bool `json:"desc"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.Desc {
		nodes.SortNodesByLatencyDesc()
	} else {
		nodes.SortNodesByLatency()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminDeleteNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	deleted, err := nodes.DeleteNodeWithError(body.RawURI)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("删除节点失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": deleted})
}

func (adm *AdminHandler) adminBatchDisableNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchDisable] 批量禁用 %d 个节点", len(body.URIs))
	if err := nodes.BatchUpdateNodesDisabled(body.URIs, true); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("批量禁用节点失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminBatchEnableNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchEnable] 批量启用 %d 个节点", len(body.URIs))
	if err := nodes.BatchUpdateNodesDisabled(body.URIs, false); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("批量启用节点失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminBatchDeleteNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchDelete] 批量删除 %d 个节点", len(body.URIs))
	if err := nodes.BatchDeleteNodes(body.URIs); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("批量删除节点失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) fetchSubscriptionText(ctx context.Context, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", errors.New("subscription url is empty")
	}
	allowPrivate := adm.cfg.AllowPrivateSubscriptionURLs()
	if _, err := netx.ValidateHTTPURL(ctx, rawURL, allowPrivate); err != nil {
		return "", fmt.Errorf("subscription URL rejected: %w", err)
	}

	data, err := fetchSubscriptionDataDirect(ctx, rawURL, allowPrivate)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}

	proxyURI := firstNonEmpty(adm.cfg.ActiveNodeURI(), adm.cfg.ProxyURL())
	if !adm.cfg.ProxySubscriptionAllowProxyFallback() ||
		proxyURI == "" || adm.vc == nil || adm.vc.Net() == nil {
		return "", err
	}

	log.Printf("[Admin] [FetchSub] direct fetch failed, retry via proxy: %v", err)
	data, proxyErr := fetchSubscriptionDataViaProxy(
		ctx,
		adm.vc.Net(),
		rawURL,
		proxyURI,
		allowPrivate,
	)
	if proxyErr != nil {
		return "", fmt.Errorf("direct fetch failed: %v; proxy retry failed: %w", err, proxyErr)
	}

	log.Printf("[Admin] [FetchSub] proxy retry succeeded")
	return strings.TrimSpace(string(data)), nil
}

func subscriptionURLForLog(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return "<invalid subscription URL>"
	}
	parsed.User = nil
	return parsed.Scheme + "://" + parsed.Host
}

func fetchSubscriptionDataDirect(ctx context.Context, rawURL string, allowPrivate bool) ([]byte, error) {
	client := netx.NewRestrictedHTTPClient(30*time.Second, allowPrivate)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造订阅拉取请求: %w", err)
	}
	req.Header.Set("User-Agent", subscriptionFetchUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, safeSubscriptionRequestError(err)
	}
	if resp == nil {
		return nil, fmt.Errorf("nil response received")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code %d", resp.StatusCode)
	}
	return readLimitedSubscriptionBody(resp.Body, resp.ContentLength)
}

func fetchSubscriptionDataViaProxy(
	ctx context.Context,
	netClient *transport.NetworkClient,
	rawURL string,
	proxyURI string,
	allowPrivate bool,
) ([]byte, error) {
	if netClient == nil {
		return nil, errors.New("network client unavailable")
	}

	sess, err := netClient.CreateSession(30, proxyURI, "admin-fetch-sub")
	if err != nil {
		return nil, errors.New("proxy session initialization failed")
	}
	defer sess.Close()
	sess.SetFollowRedirect(false)

	header := transport.Header{
		"user-agent": {subscriptionFetchUserAgent},
		"accept":     {"*/*"},
	}
	currentURL := rawURL
	for redirectCount := 0; redirectCount <= 5; redirectCount++ {
		current, validateErr := netx.ValidateHTTPURL(ctx, currentURL, allowPrivate)
		if validateErr != nil {
			return nil, fmt.Errorf("redirect target rejected: %w", validateErr)
		}
		resp, requestErr := sess.Do(ctx, http.MethodGet, current.String(), header, nil)
		if requestErr != nil {
			return nil, safeSubscriptionRequestError(requestErr)
		}
		if resp == nil {
			return nil, errors.New("nil response received")
		}
		if resp.StatusCode == http.StatusOK {
			data, readErr := readLimitedSubscriptionBody(resp.Body, resp.ContentLength)
			_ = resp.Body.Close()
			return data, readErr
		}
		if resp.StatusCode < 300 || resp.StatusCode > 399 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("status code %d", resp.StatusCode)
		}
		location := strings.TrimSpace(resp.Header.Get("Location"))
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		_ = resp.Body.Close()
		if location == "" {
			return nil, errors.New("redirect response is missing Location")
		}
		target, parseErr := url.Parse(location)
		if parseErr != nil {
			return nil, errors.New("invalid redirect target")
		}
		currentURL = current.ResolveReference(target).String()
	}
	return nil, errors.New("too many redirects")
}

func safeSubscriptionRequestError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return errors.New("subscription request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("subscription request timed out")
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return errors.New("subscription request timed out")
	}
	return errors.New("subscription network request failed")
}

func safeProxyTestError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "proxy check canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "proxy check timed out"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return "proxy check timed out"
	}
	return "proxy connectivity check failed"
}

func readLimitedSubscriptionBody(body io.Reader, contentLength int64) ([]byte, error) {
	if contentLength > maxSubscriptionResponseBytes {
		return nil, fmt.Errorf("subscription response exceeds %d MiB limit", maxSubscriptionResponseBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxSubscriptionResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取订阅响应体: %w", err)
	}
	if int64(len(data)) > maxSubscriptionResponseBytes {
		return nil, fmt.Errorf("subscription response exceeds %d MiB limit", maxSubscriptionResponseBytes>>20)
	}
	return data, nil
}
