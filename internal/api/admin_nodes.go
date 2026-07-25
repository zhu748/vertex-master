package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/netx"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

const maxSubscriptionResponseBytes int64 = 10 << 20

func (adm *AdminHandler) adminGetNodes(w http.ResponseWriter, r *http.Request) {
	list := nodes.LoadNodes()
	health := nodes.LoadHealth()
	var enabledCount, disabledCount int
	for _, n := range list {
		if n.Disabled {
			disabledCount++
		} else {
			enabledCount++
		}
	}

	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	nodeType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	filtered := make([]nodes.Node, 0, len(list))
	for _, node := range list {
		if query != "" && !strings.Contains(strings.ToLower(node.Name), query) &&
			!strings.Contains(strings.ToLower(node.RawURI), query) &&
			!strings.Contains(strings.ToLower(node.Type), query) {
			continue
		}
		if nodeType != "" && nodeType != "all" && !strings.EqualFold(node.Type, nodeType) {
			continue
		}
		if source == "manual" && node.SourceID != 0 {
			continue
		}
		if source == "subscription" && node.SourceID == 0 {
			continue
		}
		if !nodeMatchesAdminStatus(node, health[node.RawURI], status) {
			continue
		}
		filtered = append(filtered, node)
	}

	if r.URL.Query().Get("uris_only") == "true" {
		uris := make([]string, 0, len(filtered))
		for _, node := range filtered {
			uris = append(uris, node.RawURI)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"uris":  uris,
			"total": len(filtered),
		})
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = len(filtered)
		if pageSize == 0 {
			pageSize = 1
		}
	} else if pageSize > 200 {
		pageSize = 200
	}
	totalPages := max(1, (len(filtered)+pageSize-1)/pageSize)
	if page > totalPages {
		page = totalPages
	}
	start := min((page-1)*pageSize, len(filtered))
	end := min(start+pageSize, len(filtered))
	pageNodes := filtered[start:end]
	pageHealth := make(map[string]*nodes.NodeHealth, len(pageNodes))
	for _, node := range pageNodes {
		if item := health[node.RawURI]; item != nil {
			pageHealth[node.RawURI] = item
		}
	}

	sp := nodes.GetStickyPool()
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":                 pageNodes,
		"health":                pageHealth,
		"total":                 len(filtered),
		"overall_total":         len(list),
		"page":                  page,
		"page_size":             pageSize,
		"total_pages":           totalPages,
		"enabled_count":         enabledCount,
		"disabled_count":        disabledCount,
		"sticky_pool_available": sp.AvailableCount(),
		"sticky_pool_in_use":    sp.StaleCount(),
		"sticky_node_priority":  adm.cfg.StickyNodePriority(),
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
	log.Printf("[Admin] [FetchSub] 开始拉取订阅 URL: %s", subscriptionURLForLog(body.URL))
	text, err := adm.fetchSubscriptionText(r.Context(), body.URL)
	if err != nil {
		log.Printf("[Admin] [FetchSub] 拉取失败: %v", err)
		writeJSON(w, http.StatusBadRequest, adminErr("拉取失败: "+err.Error()))
		return
	}

	newNodes := parseImportedNodes(text)
	nodes.MergeNodes(newNodes)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(newNodes)})
}

func (adm *AdminHandler) adminTestAll(w http.ResponseWriter, _ *http.Request) {
	log.Printf("[Admin] [TestAll] 开始触发全局并发测速（基于 recaptchaToken 耗时）")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		list := nodes.LoadNodes()
		var enabledNodes []nodes.Node
		for _, n := range list {
			if !n.Disabled {
				enabledNodes = append(enabledNodes, n)
			}
		}
		log.Printf("[Admin] [TestAll] 加载到待测启用节点数: %d / %d", len(enabledNodes), len(list))
		nodes.StartTestProgress(len(enabledNodes))

		var wg sync.WaitGroup
		sem := make(chan struct{}, 10)

		for _, n := range enabledNodes {
			wg.Add(1)
			go func(node nodes.Node) {
				defer wg.Done()
				if nodes.CheckTestControl() {
					return
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				if nodes.CheckTestControl() {
					return
				}

				start := time.Now()
				log.Printf("[Admin] [TestAll] 开始测试节点: %s (%s)", node.Name, node.Type)

				sess, err := adm.vc.Net().CreateSession(15, node.RawURI, "admin-test-all")
				var testErr error
				if err == nil {
					testErr = fetchRecaptchaTokenWithSess(ctx, sess)
					sess.Close()
				} else {
					testErr = err
				}

				duration := float64(time.Since(start).Milliseconds())
				if testErr != nil {
					log.Printf("[Admin] [TestAll] 节点 %s 测试失败: %v, 耗时: %.0fms", node.Name, testErr, duration)
				} else {
					log.Printf("[Admin] [TestAll] 节点 %s 测试成功, recaptcha 耗时: %.0fms", node.Name, duration)
				}
				success := testErr == nil
				nodes.RecordTest(node.RawURI, success, duration, errToStr(testErr))
				if !success {
					nodes.BatchUpdateNodesDisabled([]string{node.RawURI}, true)
				}
				nodes.UpdateTestProgress(node.Name, success)
			}(n)
		}
		wg.Wait()
		nodes.FinishTestProgress()
		log.Printf("[Admin] [TestAll] 全局节点测试全部结束")
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	sess, err := adm.vc.Net().CreateSession(15, body.RawURI, "admin-test-node")
	var testErr error
	if err == nil {
		testErr = fetchRecaptchaTokenWithSess(ctx, sess)
		sess.Close()
	} else {
		testErr = err
	}
	elapsed := float64(time.Since(start).Milliseconds())

	errStr := ""
	ok := testErr == nil
	if testErr != nil {
		if ctx.Err() != nil || errors.Is(testErr, context.DeadlineExceeded) {
			errStr = "timeout"
		} else {
			errStr = testErr.Error()
		}
	}

	disabled := false
	if body.AutoDisable {
		nodes.UpdateNodeTestResult(body.RawURI, ok, elapsed, errStr)
		disabled = !ok
		if !ok {
			nodes.BatchUpdateNodesDisabled([]string{body.RawURI}, true)
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

func (adm *AdminHandler) adminEnableNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	ok := nodes.EnableNode(body.RawURI)
	log.Printf("[Admin] [EnableNode] 启用节点 %s: %v", nodes.GetNodeName(body.RawURI), ok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
}

func fetchRecaptchaTokenWithSess(ctx context.Context, sess *transport.Session) error {
	const (
		recaptchaBase = "https://www.google.com"
		siteKey       = "6LdCjtspAAAAAMcV4TGdWLJqRTEk1TfpdLqEnKdj"
		recaptchaCo   = "aHR0cHM6Ly9jb25zb2xlLmNsb3VkLmdvb2dsZS5jb206NDQz"
		recaptchaHl   = "zh-CN"
		recaptchaV    = "jdMmXeCQEkPbnFDy9T04NbgJ"
		recaptchaVh   = "6581054572"
		randomCharset = "abcdefghijklmnopqrstuvwxyz0123456789"
	)
	var (
		tokenRe = regexp.MustCompile(`id="recaptcha-token"[^>]*value="([^"]+)"`)
		rrespRe = regexp.MustCompile(`rresp","(.*?)"`)
	)

	b := make([]byte, 10)
	for i := range b {
		b[i] = randomCharset[time.Now().UnixNano()%int64(len(randomCharset))]
	}
	cb := string(b)

	anchorURL := fmt.Sprintf(
		"%s/recaptcha/enterprise/anchor?ar=1&k=%s&co=%s&hl=%s&v=%s&size=invisible&anchor-ms=20000&execute-ms=15000&cb=%s",
		recaptchaBase, siteKey, recaptchaCo, recaptchaHl, recaptchaV, cb,
	)

	_, anchorBody, err := sess.DoAndRead(ctx, "GET", anchorURL, transport.AnchorHeaders(), nil)
	if err != nil {
		return fmt.Errorf("GET anchor 失败: %w", err)
	}
	m := tokenRe.FindSubmatch(anchorBody)
	if m == nil {
		return fmt.Errorf("从 anchor HTML 解析 recaptcha-token 失败")
	}
	baseToken := string(m[1])

	form := url.Values{
		"v":      {recaptchaV},
		"reason": {"q"},
		"k":      {siteKey},
		"c":      {baseToken},
		"co":     {recaptchaCo},
		"hl":     {recaptchaHl},
		"size":   {"invisible"},
		"vh":     {recaptchaVh},
		"chr":    {""},
		"bg":     {""},
	}
	reloadURL := recaptchaBase + "/recaptcha/enterprise/reload?k=" + siteKey
	header := transport.XHRHeaders(
		"application/x-www-form-urlencoded;charset=UTF-8", "*/*",
		recaptchaBase, anchorURL, "same-origin",
	)

	_, reloadBody, err := sess.DoAndRead(ctx, "POST", reloadURL, header, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("POST reload 失败: %w", err)
	}
	rm := rrespRe.FindSubmatch(reloadBody)
	if rm == nil {
		return fmt.Errorf("从 reload 响应解析 rresp 失败")
	}
	return nil
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
		_ = config.WriteSettings(map[string]any{"active_node_uri": "", "parallel_pool_enabled": true})
	} else {
		_ = config.WriteSettings(map[string]any{"active_node_uri": body.RawURI, "parallel_pool_enabled": false})
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
	nodes.DeleteNode(body.RawURI)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminBatchDisableNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchDisable] 批量禁用 %d 个节点", len(body.URIs))
	nodes.BatchUpdateNodesDisabled(body.URIs, true)
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
	nodes.BatchUpdateNodesDisabled(body.URIs, false)
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
	nodes.BatchDeleteNodes(body.URIs)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) fetchSubscriptionText(ctx context.Context, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", errors.New("subscription url is empty")
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" ||
		(parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return "", errors.New("subscription url must be a valid HTTP/HTTPS URL")
	}

	data, err := fetchSubscriptionDataDirect(ctx, rawURL)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}

	proxyURI := firstNonEmpty(adm.cfg.ActiveNodeURI(), adm.cfg.ProxyURL())
	if proxyURI == "" || adm.vc == nil || adm.vc.Net() == nil {
		return "", err
	}

	log.Printf("[Admin] [FetchSub] direct fetch failed, retry via proxy: %v", err)
	data, proxyErr := fetchSubscriptionDataViaProxy(ctx, adm.vc.Net(), rawURL, proxyURI)
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
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func fetchSubscriptionDataDirect(ctx context.Context, rawURL string) ([]byte, error) {
	client := netx.NewHTTPClient(30 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	req.Header.Set("User-Agent", subscriptionFetchUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
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

func fetchSubscriptionDataViaProxy(ctx context.Context, netClient *transport.NetworkClient, rawURL string, proxyURI string) ([]byte, error) {
	if netClient == nil {
		return nil, errors.New("network client unavailable")
	}

	sess, err := netClient.CreateSession(30, proxyURI, "admin-fetch-sub")
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	defer sess.Close()

	header := transport.Header{
		"user-agent": {subscriptionFetchUserAgent},
		"accept":     {"*/*"},
	}
	resp, err := sess.Do(ctx, http.MethodGet, rawURL, header, nil)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	if resp == nil {
		return nil, errors.New("nil response received")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code %d", resp.StatusCode)
	}
	return readLimitedSubscriptionBody(resp.Body, resp.ContentLength)
}

func readLimitedSubscriptionBody(body io.Reader, contentLength int64) ([]byte, error) {
	if contentLength > maxSubscriptionResponseBytes {
		return nil, fmt.Errorf("subscription response exceeds %d MiB limit", maxSubscriptionResponseBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxSubscriptionResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	if int64(len(data)) > maxSubscriptionResponseBytes {
		return nil, fmt.Errorf("subscription response exceeds %d MiB limit", maxSubscriptionResponseBytes>>20)
	}
	return data, nil
}
