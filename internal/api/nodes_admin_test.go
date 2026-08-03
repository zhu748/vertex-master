package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

var benchmarkAdminNodesResponseSize int //nolint:gochecknoglobals

func BenchmarkAdminGetNodesLargePoolPage(b *testing.B) {
	benchmarkAdminGetNodesLargePool(b, "admin-page-benchmark", "&page=1&page_size=50")
}

func BenchmarkAdminGetNodesLargePoolPageNoMatch(b *testing.B) {
	benchmarkAdminGetNodesLargePool(b, "admin-page-no-match", "-missing&page=1&page_size=50")
}

func BenchmarkAdminGetNodesLargePoolURIsOnly(b *testing.B) {
	benchmarkAdminGetNodesLargePool(b, "admin-uris-benchmark", "&status=healthy&uris_only=true")
}

func TestAdminNodeQueryMatcherMatchesLowercaseContains(t *testing.T) {
	tests := []struct {
		name  string
		value string
		query string
	}{
		{name: "empty query", value: "anything", query: ""},
		{name: "lowercase match", value: "http://proxy.example", query: "proxy"},
		{name: "lowercase miss", value: "http://proxy.example", query: "missing"},
		{name: "uppercase ASCII", value: "HTTP://Proxy.Example", query: "proxy"},
		{name: "mixed ASCII miss", value: "HTTP://Proxy.Example", query: "missing"},
		{name: "unicode value", value: "节点-Ä-Kelvin", query: "kelvin"},
		{name: "unicode query", value: "节点-Ä-Kelvin", query: "ä"},
		{name: "invalid UTF-8", value: string([]byte{'n', 'o', 'd', 'e', 0xff, 'X'}), query: "x"},
		{name: "longer query", value: "short", query: "a much longer query"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lowerQuery := strings.ToLower(test.query)
			matcher := newAdminNodeQueryMatcher(lowerQuery)
			got := matcher.Contains(test.value)
			want := strings.Contains(strings.ToLower(test.value), lowerQuery)
			if got != want {
				t.Fatalf(
					"matcher.Contains(%q, %q)=%v, want %v",
					test.value,
					test.query,
					got,
					want,
				)
			}
		})
	}
}

func benchmarkAdminGetNodesLargePool(b *testing.B, prefix, querySuffix string) {
	b.Helper()
	const nodeCount = 5000
	list := make([]nodes.Node, nodeCount)
	uris := make([]string, nodeCount)
	for index := range list {
		uri := fmt.Sprintf("http://%s-%d.invalid:8080", prefix, index)
		uris[index] = uri
		list[index] = nodes.Node{
			Type: "http", Name: fmt.Sprintf("%s-%d", prefix, index), RawURI: uri,
		}
	}
	if err := nodes.MergeNodes(list); err != nil {
		b.Fatal(err)
	}
	for _, uri := range uris {
		nodes.RecordTest(uri, true, 25, "")
	}
	b.Cleanup(func() {
		if err := nodes.BatchDeleteNodes(uris); err != nil {
			b.Errorf("cleanup benchmark nodes: %v", err)
		}
	})

	cfg := config.StaticProvider(config.DefaultConfig())
	adm := &AdminHandler{handler: handler{cfg: cfg}} //nolint:exhaustruct
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/admin/nodes?query="+prefix+querySuffix,
		nil,
	)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		recorder := httptest.NewRecorder()
		adm.adminGetNodes(recorder, request)
		if recorder.Code != http.StatusOK {
			b.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		benchmarkAdminNodesResponseSize = recorder.Body.Len()
	}
}

func TestWriteAdminNodeURIsJSONMatchesGenericEncoding(t *testing.T) {
	uris := []string{
		"http://user:pass@example.com:8080/path?x=<&>#中文",
		"line\nbreak",
		"unicode\u2028separator",
		string([]byte{'i', 'n', 'v', 'a', 'l', 'i', 'd', '-', 0xff}),
	}
	direct := httptest.NewRecorder()
	writeAdminNodeURIsJSON(direct, uris)
	generic := httptest.NewRecorder()
	writeJSON(generic, http.StatusOK, map[string]any{
		"total": len(uris),
		"uris":  uris,
	})

	if direct.Code != generic.Code ||
		direct.Header().Get("Content-Type") != generic.Header().Get("Content-Type") ||
		direct.Body.String() != generic.Body.String() {
		t.Fatalf(
			"direct URI response differs:\ndirect=%d %q %q\ngeneric=%d %q %q",
			direct.Code,
			direct.Header().Get("Content-Type"),
			direct.Body.String(),
			generic.Code,
			generic.Header().Get("Content-Type"),
			generic.Body.String(),
		)
	}
}

func TestAdminNodesPageResponseMatchesGenericEncoding(t *testing.T) {
	health := map[string]nodes.NodeHealth{
		"http://user:pass@example.com:8080/path?x=<&>#中文": {
			SuccessCount:  1,
			LastTestMs:    12.5,
			LastTestError: "line\nbreak\u2028",
		},
	}
	typedHealth := make(map[string]*nodes.NodeHealth, len(health))
	for rawURI, nodeHealth := range health {
		typedHealth[rawURI] = &nodeHealth
	}
	pageNodes := []nodes.Node{{
		Type:   "http",
		Name:   "node <&> 中文",
		RawURI: "http://user:pass@example.com:8080/path?x=<&>#中文",
	}}
	poolStats := nodes.NodePoolStats{
		Total: 2, Enabled: 1, Disabled: 1, Healthy: 1, Unhealthy: 1,
	}
	scheduler := ProxyHealthSchedulerStatus{
		Enabled: true, Running: true, Checked: 2, Succeeded: 1, Failed: 1,
	}
	recent := nodes.RecentProxyStatus{
		Available: true, Name: "node <&> 中文", Type: "http", Address: "example.com:8080",
	}
	history := []nodes.RecentProxyEvent{{
		Name: "node <&> 中文", Type: "http", Address: "example.com:8080",
	}}
	typed := httptest.NewRecorder()
	writeJSON(typed, http.StatusOK, adminNodesPageResponse{
		ActiveNodeURI:              pageNodes[0].RawURI,
		DisabledCount:              1,
		EnabledCount:               1,
		Health:                     typedHealth,
		HealthCycleEstimateMinutes: 30,
		HealthScheduler:            scheduler,
		Nodes:                      pageNodes,
		OverallTotal:               2,
		Page:                       1,
		PageSize:                   50,
		PoolStats:                  poolStats,
		ProxyURL:                   "http://proxy.example:8080",
		RecentProxy:                recent,
		RecentProxyHistory:         history,
		StickyNodePriority:         true,
		StickyPoolAvailable:        3,
		StickyPoolInUse:            2,
		Total:                      1,
		TotalPages:                 1,
	})
	generic := httptest.NewRecorder()
	writeJSON(generic, http.StatusOK, map[string]any{
		"active_node_uri":               pageNodes[0].RawURI,
		"disabled_count":                1,
		"enabled_count":                 1,
		"health":                        health,
		"health_cycle_estimate_minutes": 30,
		"health_scheduler":              scheduler,
		"nodes":                         pageNodes,
		"overall_total":                 2,
		"page":                          1,
		"page_size":                     50,
		"pool_stats":                    poolStats,
		"proxy_url":                     "http://proxy.example:8080",
		"recent_proxy":                  recent,
		"recent_proxy_history":          history,
		"sticky_node_priority":          true,
		"sticky_pool_available":         3,
		"sticky_pool_in_use":            2,
		"total":                         1,
		"total_pages":                   1,
	})

	if typed.Code != generic.Code ||
		typed.Header().Get("Content-Type") != generic.Header().Get("Content-Type") ||
		typed.Body.String() != generic.Body.String() {
		t.Fatalf(
			"typed page response differs:\ntyped=%d %q %q\ngeneric=%d %q %q",
			typed.Code,
			typed.Header().Get("Content-Type"),
			typed.Body.String(),
			generic.Code,
			generic.Header().Get("Content-Type"),
			generic.Body.String(),
		)
	}
}

func TestReadLimitedSubscriptionBody(t *testing.T) {
	data, err := readLimitedSubscriptionBody(strings.NewReader("proxy-list"), -1)
	if err != nil || string(data) != "proxy-list" {
		t.Fatalf("small subscription body failed: data=%q err=%v", data, err)
	}
	if _, err := readLimitedSubscriptionBody(strings.NewReader("ignored"), maxSubscriptionResponseBytes+1); err == nil {
		t.Fatal("known oversized subscription body should be rejected")
	}
	oversized := strings.NewReader(strings.Repeat("x", int(maxSubscriptionResponseBytes)+1))
	if _, err := readLimitedSubscriptionBody(oversized, -1); err == nil {
		t.Fatal("streamed oversized subscription body should be rejected")
	}
}

func TestMaxBatchTestNodesFitsTaskBudget(t *testing.T) {
	if got := maxBatchTestNodes(20, 60); got != 1000 {
		t.Fatalf("high-concurrency cap=%d, want 1000", got)
	}
	if got := maxBatchTestNodes(1, 60); got != 59 {
		t.Fatalf("single-worker task capacity=%d, want 59", got)
	}
}

func TestSubscriptionURLForLogRemovesCredentialsAndQuery(t *testing.T) {
	got := subscriptionURLForLog("https://alice:secret@example.com/list.txt?token=sensitive#fragment")
	if got != "https://example.com" {
		t.Fatalf("sensitive subscription URL parts leaked: %q", got)
	}
}

func TestFilterRemoteSubscriptionNodesRejectsPrivateEndpoints(t *testing.T) {
	imported := parseImportedNodes(strings.Join([]string{
		"http://127.0.0.1:8080",
		"socks5://169.254.169.254:1080",
		"http://proxy.example.com:3128",
		"http://8.8.8.8:8080",
	}, "\n"))
	filtered, rejected, err := filterRemoteSubscriptionNodes(
		context.Background(),
		imported,
		false,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if rejected != 3 || len(filtered) != 1 || !strings.Contains(filtered[0].RawURI, "8.8.8.8") {
		t.Fatalf("unexpected filtering result: rejected=%d filtered=%#v", rejected, filtered)
	}

	filtered, rejected, err = filterRemoteSubscriptionNodes(
		context.Background(),
		imported,
		true,
		false,
	)
	if err != nil || rejected != 0 || len(filtered) != len(imported) {
		t.Fatalf("private opt-out should preserve endpoints: rejected=%d filtered=%#v err=%v", rejected, filtered, err)
	}
}

func TestReplaceImportRejectsEmptyParseWithoutDeletingExistingNode(t *testing.T) {
	existingURI := "http://8.8.8.8:39871"
	if err := nodes.MergeNodes([]nodes.Node{{
		Type: "http", Name: "must survive", RawURI: existingURI,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nodes.DeleteNode(existingURI) })

	adm := &AdminHandler{} //nolint:exhaustruct
	req := httptest.NewRequest(
		"POST",
		"/api/admin/nodes/import",
		strings.NewReader(`{"text":"not a proxy list","replace":true}`),
	)
	rec := httptest.NewRecorder()
	adm.adminImportNodes(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty parsed replacement status=%d body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, node := range nodes.LoadNodes() {
		if node.RawURI == existingURI {
			found = true
		}
	}
	if !found {
		t.Fatal("invalid replacement deleted the existing node")
	}
}

func TestNodeMatchesAdminStatus(t *testing.T) {
	enabled := nodes.Node{RawURI: "http://127.0.0.1:8080"}                  //nolint:exhaustruct
	disabled := nodes.Node{RawURI: "http://127.0.0.1:8081", Disabled: true} //nolint:exhaustruct
	healthy := &nodes.NodeHealth{LastSuccessAt: 1}                          //nolint:exhaustruct
	unhealthy := &nodes.NodeHealth{LastFailAt: 1, ConsecutiveFailures: 1}   //nolint:exhaustruct

	if !nodeMatchesAdminStatus(enabled, nil, "enabled") ||
		nodeMatchesAdminStatus(disabled, nil, "enabled") {
		t.Fatal("enabled status filter mismatch")
	}
	if !nodeMatchesAdminStatus(disabled, nil, "disabled") {
		t.Fatal("disabled status filter mismatch")
	}
	if !nodeMatchesAdminStatus(enabled, healthy, "healthy") ||
		nodeMatchesAdminStatus(enabled, unhealthy, "healthy") {
		t.Fatal("healthy status filter mismatch")
	}
	if !nodeMatchesAdminStatus(enabled, unhealthy, "unhealthy") ||
		!nodeMatchesAdminStatus(enabled, nil, "untested") {
		t.Fatal("health failure/untested filter mismatch")
	}
}

func TestAdminGetRecentProxy(t *testing.T) {
	rawURI := "http://user:secret@8.8.4.4:3128"
	nodes.RecordProxySuccess(rawURI)
	adm := &AdminHandler{} //nolint:exhaustruct
	rec := httptest.NewRecorder()

	adm.adminGetRecentProxy(rec, httptest.NewRequest(
		http.MethodGet,
		"/api/admin/nodes/current",
		nil,
	))

	var response struct {
		Recent  nodes.RecentProxyStatus  `json:"recent_proxy"`
		History []nodes.RecentProxyEvent `json:"recent_proxy_history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Recent.Available ||
		response.Recent.Address != "8.8.4.4:3128" ||
		response.Recent.Type != "http" {
		t.Fatalf("unexpected recent proxy response: %#v", response.Recent)
	}
	if strings.Contains(response.Recent.Address, "secret") {
		t.Fatalf("proxy credentials leaked: %q", response.Recent.Address)
	}
	if len(response.History) == 0 || response.History[0].Address != "8.8.4.4:3128" {
		t.Fatalf("unexpected recent proxy history: %#v", response.History)
	}
}

func TestAdminGetNodesPaginationAndFilters(t *testing.T) {
	const prefix = "admin-pagination-filter-test"
	firstURI := "http://127.0.0.1:49101#" + prefix + "-first"
	secondURI := "socks5://127.0.0.1:49102#" + prefix + "-second"
	if err := nodes.MergeNodes([]nodes.Node{
		{Type: "http", Name: prefix + "-first", RawURI: firstURI},                     //nolint:exhaustruct
		{Type: "socks5", Name: prefix + "-second", RawURI: secondURI, Disabled: true}, //nolint:exhaustruct
	}); err != nil {
		t.Fatalf("MergeNodes() error = %v", err)
	}
	t.Cleanup(func() {
		nodes.DeleteNode(firstURI)
		nodes.DeleteNode(secondURI)
	})

	appConfig := config.DefaultConfig()
	appConfig.ActiveNodeURI = firstURI
	appConfig.ProxyURL = "http://127.0.0.1:7890"
	cfg := config.StaticProvider(appConfig)
	adm := &AdminHandler{handler: handler{cfg: cfg}} //nolint:exhaustruct
	req := httptest.NewRequest("GET",
		"/api/admin/nodes?query="+prefix+"&page=2&page_size=1", nil)
	rec := httptest.NewRecorder()
	adm.adminGetNodes(rec, req)

	var page struct {
		ActiveNodeURI string       `json:"active_node_uri"`
		Nodes         []nodes.Node `json:"nodes"`
		Page          int          `json:"page"`
		PageSize      int          `json:"page_size"`
		ProxyURL      string       `json:"proxy_url"`
		Total         int          `json:"total"`
		TotalPages    int          `json:"total_pages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Page != 2 || page.PageSize != 1 ||
		page.TotalPages != 2 || len(page.Nodes) != 1 ||
		page.ActiveNodeURI != firstURI || page.ProxyURL != appConfig.ProxyURL {
		t.Fatalf("unexpected paginated response: %#v", page)
	}

	req = httptest.NewRequest("GET",
		"/api/admin/nodes?query="+prefix+"&status=disabled&uris_only=true", nil)
	rec = httptest.NewRecorder()
	adm.adminGetNodes(rec, req)
	var filtered struct {
		URIs  []string `json:"uris"`
		Total int      `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 1 || len(filtered.URIs) != 1 || filtered.URIs[0] != secondURI {
		t.Fatalf("unexpected filtered URI response: %#v", filtered)
	}
}

func TestAdminGetSettingsIncludesProxyPoolControls(t *testing.T) {
	cfg := config.StaticProvider(config.DefaultConfig())
	adm := &AdminHandler{handler: handler{cfg: cfg}} //nolint:exhaustruct
	rec := httptest.NewRecorder()
	adm.adminGetSettings(rec, httptest.NewRequest("GET", "/api/admin/settings", nil))

	var response struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}

	expected := map[string]any{
		"max_retries":                         float64(10),
		"parallel_pool_size":                  float64(10),
		"proxy_failover_max_attempts":         float64(30),
		"proxy_health_check_enabled":          true,
		"proxy_health_check_interval_minutes": float64(15),
		"proxy_health_check_batch_size":       float64(50),
		"proxy_health_check_concurrency":      float64(5),
		"proxy_health_check_timeout_seconds":  float64(8),
	}
	for key, want := range expected {
		if got := response.Settings[key]; got != want {
			t.Errorf("setting %q = %#v, want %#v", key, got, want)
		}
		if !adminAllowedSettings[key] {
			t.Errorf("setting %q is returned but cannot be saved", key)
		}
	}
}

func TestParseInlineYamlAttrsKeepsNestedObjects(t *testing.T) {
	attrs := parseInlineYamlAttrs("name: demo, type: vless, ws-opts: { path: /ws, headers: { Host: edge.example.com } }, reality-opts: { public-key: pubkey, short-id: abcd }")

	if got := attrs["ws-opts"]; got != "{ path: /ws, headers: { Host: edge.example.com } }" {
		t.Fatalf("ws-opts was split unexpectedly: %q", got)
	}
	if got := attrs["reality-opts"]; got != "{ public-key: pubkey, short-id: abcd }" {
		t.Fatalf("reality-opts was split unexpectedly: %q", got)
	}
}

func TestClashProxyToURIPreservesVlessWSAndReality(t *testing.T) {
	raw := clashProxyToURI(map[string]string{
		"type":               "vless",
		"name":               "demo",
		"server":             "cf.example.com",
		"port":               "443",
		"uuid":               "12345678-1234-1234-1234-123456789012",
		"tls":                "true",
		"servername":         "edge.example.com",
		"client-fingerprint": "chrome",
		"flow":               "xtls-rprx-vision",
		"network":            "ws",
		"ws-opts":            "{ path: /ws, headers: { Host: edge.example.com } }",
		"reality-opts":       "{ public-key: pubkey, short-id: abcd }",
	})

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	q := u.Query()

	if u.Scheme != "vless" {
		t.Fatalf("unexpected scheme: %s", u.Scheme)
	}
	if q.Get("security") != "reality" {
		t.Fatalf("security not preserved: %q", q.Get("security"))
	}
	if q.Get("pbk") != "pubkey" || q.Get("sid") != "abcd" {
		t.Fatalf("reality opts not preserved: pbk=%q sid=%q", q.Get("pbk"), q.Get("sid"))
	}
	if q.Get("type") != "ws" || q.Get("path") != "/ws" || q.Get("host") != "edge.example.com" {
		t.Fatalf("ws params not preserved: type=%q path=%q host=%q", q.Get("type"), q.Get("path"), q.Get("host"))
	}
	if q.Get("sni") != "edge.example.com" || q.Get("fp") != "chrome" || q.Get("flow") != "xtls-rprx-vision" {
		t.Fatalf("tls params not preserved: sni=%q fp=%q flow=%q", q.Get("sni"), q.Get("fp"), q.Get("flow"))
	}
}

func TestClashProxyToURIBuildsHy2WithPortRange(t *testing.T) {
	raw := clashProxyToURI(map[string]string{
		"type":             "hysteria2",
		"name":             "demo",
		"server":           "203.10.99.51",
		"port":             "20000",
		"ports":            "20000-55000",
		"password":         "secret",
		"sni":              "www.bing.com",
		"skip-cert-verify": "true",
	})

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	q := u.Query()

	if u.Scheme != "hy2" {
		t.Fatalf("unexpected scheme: %s", u.Scheme)
	}
	if q.Get("ports") != "20000-55000" {
		t.Fatalf("ports not preserved: %q", q.Get("ports"))
	}
	if q.Get("sni") != "www.bing.com" || q.Get("insecure") != "1" {
		t.Fatalf("hy2 tls params not preserved: sni=%q insecure=%q", q.Get("sni"), q.Get("insecure"))
	}
}

func TestParseClashYAMLToNodesPreservesSSPluginOpts(t *testing.T) {
	yamlText := `
proxies:
  - { name: 'HK Demo', type: ss, server: example.com, port: 12022, cipher: aes-128-gcm, password: secret, plugin: obfs, plugin-opts: { mode: http, host: edge.example.com }, udp: true }
`

	imported := parseClashYAMLToNodes(yamlText)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}
	if imported[0].Type != "ss" || imported[0].Name != "HK Demo" {
		t.Fatalf("unexpected imported node metadata: %#v", imported[0])
	}

	out, err := transport.ParseURI(imported[0].RawURI)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	if got := out["plugin"]; got != "obfs" {
		t.Fatalf("plugin not preserved: %#v", got)
	}
	opts, ok := out["plugin-opts"].(map[string]any)
	if !ok {
		t.Fatalf("plugin-opts missing or wrong type: %#v", out["plugin-opts"])
	}
	if opts["mode"] != "http" || opts["host"] != "edge.example.com" {
		t.Fatalf("plugin-opts not preserved: %#v", opts)
	}
	if got := out["udp"]; got != true {
		t.Fatalf("udp not preserved: %#v", got)
	}
}

func TestParseClashYAMLToNodesSkipsInvalidProxyObjects(t *testing.T) {
	yamlText := `
proxies:
  - { name: bad missing endpoint, type: ss }
  - { name: group-ish, type: select }
`

	imported := parseClashYAMLToNodes(yamlText)
	if len(imported) != 0 {
		t.Fatalf("expected invalid proxy objects to be skipped, got %#v", imported)
	}
}

func TestParseImportedNodesSupportsSingleTopLevelProxyObject(t *testing.T) {
	text := `{ name: 'HK Demo', type: ss, server: example.com, port: 12022, cipher: aes-128-gcm, password: secret, plugin: obfs, plugin-opts: { mode: http, host: edge.example.com } }`

	imported := parseImportedNodes(text)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}

	out, err := transport.ParseURI(imported[0].RawURI)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	if out["type"] != "ss" || out["server"] != "example.com" {
		t.Fatalf("unexpected imported node: %#v", out)
	}
}

func TestParseImportedNodesSupportsV2RayNInnerURI(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"ConfigType":     5,
		"Remarks":        "demo",
		"Address":        "cf.example.com",
		"Port":           443,
		"Password":       "12345678-1234-1234-1234-123456789012",
		"StreamSecurity": "tls",
		"Sni":            "edge.example.com",
		"Fingerprint":    "chrome",
		"Network":        "ws",
		"ProtoExtraObj":  map[string]any{"VlessEncryption": "none"},
		"TransportExtraObj": map[string]any{
			"Path": "/ws",
			"Host": "edge.example.com",
		},
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	text := "v2rayn://vless/" + base64.RawURLEncoding.EncodeToString(payload)
	imported := parseImportedNodes(text)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}

	out, err := transport.ParseURI(imported[0].RawURI)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	if out["type"] != "vless" || out["servername"] != "edge.example.com" {
		t.Fatalf("unexpected imported node: %#v", out)
	}
	wsOpts, ok := out["ws-opts"].(map[string]any)
	if !ok || wsOpts["path"] != "/ws" {
		t.Fatalf("ws-opts not preserved: %#v", out["ws-opts"])
	}
}

func TestParseImportedNodesConvertsV2RayNXHTTPExtra(t *testing.T) {
	extraJSON, err := json.Marshal(map[string]any{
		"noGRPCHeader":    true,
		"sessionIDLength": 8,
		"xmux": map[string]any{
			"maxConnections":   "1-4",
			"hKeepAlivePeriod": 30,
		},
	})
	if err != nil {
		t.Fatalf("Marshal XHTTP extra: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"ConfigType":     5,
		"Remarks":        "xhttp demo",
		"Address":        "xhttp.example.com",
		"Port":           443,
		"Password":       "12345678-1234-1234-1234-123456789012",
		"StreamSecurity": "tls",
		"Sni":            "edge.example.com",
		"Network":        "xhttp",
		"ProtoExtraObj":  map[string]any{"VlessEncryption": "none"},
		"TransportExtraObj": map[string]any{
			"Path":       "/api",
			"Host":       "cdn.example.com",
			"XhttpMode":  "stream-one",
			"XhttpExtra": string(extraJSON),
		},
	})
	if err != nil {
		t.Fatalf("Marshal V2RayN profile: %v", err)
	}

	imported := parseImportedNodes(
		"v2rayn://vless/" + base64.RawURLEncoding.EncodeToString(payload),
	)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}
	out, err := transport.ParseURI(imported[0].RawURI)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	xhttpOptions, ok := out["xhttp-opts"].(map[string]any)
	if !ok || xhttpOptions["path"] != "/api" ||
		xhttpOptions["host"] != "cdn.example.com" ||
		xhttpOptions["mode"] != "stream-one" ||
		xhttpOptions["no-grpc-header"] != true ||
		xhttpOptions["session-length"] != "8" {
		t.Fatalf("V2RayN XHTTP options not converted: %#v", out["xhttp-opts"])
	}
	if _, stale := xhttpOptions["extra"]; stale {
		t.Fatalf("raw XHTTP extra leaked into Mihomo options: %#v", xhttpOptions)
	}
	reuse, ok := xhttpOptions["reuse-settings"].(map[string]any)
	if !ok || reuse["max-connections"] != "1-4" ||
		reuse["h-keep-alive-period"] != float64(30) {
		t.Fatalf("V2RayN XHTTP reuse settings not converted: %#v", reuse)
	}

	directProxy := map[string]any{}
	applyTransportExtras(
		directProxy,
		map[string]any{"Network": "xhttp"},
		map[string]any{
			"XhttpExtra": map[string]any{
				"noGRPCHeader": true,
				"xmux":         map[string]any{"maxConnections": 2},
			},
		},
	)
	directOptions, ok := directProxy["xhttp-opts"].(map[string]any)
	if !ok || directOptions["no-grpc-header"] != true {
		t.Fatalf("object XHTTP extra not converted: %#v", directProxy)
	}
	directReuse, ok := directOptions["reuse-settings"].(map[string]any)
	if !ok || directReuse["max-connections"] != "2" {
		t.Fatalf("object XHTTP reuse settings not converted: %#v", directReuse)
	}
}

func TestParseImportedNodesSupportsSIP008(t *testing.T) {
	text := `{"servers":[{"remarks":"ss demo","server":"1.2.3.4","server_port":8388,"method":"aes-128-gcm","password":"secret"}]}`

	imported := parseImportedNodes(text)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}

	out, err := transport.ParseURI(imported[0].RawURI)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	if out["type"] != "ss" || intValue(out["port"]) != 8388 {
		t.Fatalf("unexpected imported node: %#v", out)
	}
}

func TestImportedNodeIntegersRejectFractionsAndOverflow(t *testing.T) {
	for _, value := range []any{
		float32(443.5),
		float64(443.5),
		math.NaN(),
		math.Inf(1),
		float64(math.MaxInt) + 1,
		^uint64(0),
	} {
		if got := intValue(value); got != 0 {
			t.Errorf("intValue(%v)=%d, want 0", value, got)
		}
	}

	text := `{"servers":[{"server":"1.2.3.4","server_port":8388.5,"method":"aes-128-gcm","password":"secret"}]}`
	if imported := parseImportedNodes(text); len(imported) != 0 {
		t.Fatalf("fractional SIP008 port was imported: %#v", imported)
	}
}

func TestParseProxyListNodes(t *testing.T) {
	text := `
# comment
1.2.3.4:8080
user:pass@proxy.example.com:3128
5.6.7.8:1080:alice:secret
socks5://bob:pwd@9.9.9.9:1080
invalid
1.2.3.4:8080
`
	imported := parseProxyListNodes(text, "http")
	if len(imported) != 4 {
		t.Fatalf("expected 4 unique proxies, got %d: %#v", len(imported), imported)
	}
	if imported[0].Type != "http" || imported[3].Type != "socks5" {
		t.Fatalf("proxy types were not normalized correctly: %#v", imported)
	}
	out, err := transport.ParseURI(imported[2].RawURI)
	if err != nil {
		t.Fatal(err)
	}
	if out["username"] != "alice" || out["password"] != "secret" {
		t.Fatalf("host:port:user:pass credentials lost: %#v", out)
	}
}

func TestParseImportedNodesSupportsStandardProxyURI(t *testing.T) {
	imported := parseImportedNodes("socks4://127.0.0.1:1080\nhttp://user:pass@example.com:8080")
	if len(imported) != 2 {
		t.Fatalf("expected 2 standard proxies, got %d", len(imported))
	}
	if imported[0].Type != "socks4" || imported[1].Type != "http" {
		t.Fatalf("unexpected types: %#v", imported)
	}
}

func TestImportedFallbackNamesNeverContainCredentials(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"vless", "vless://11111111-2222-3333-4444-555555555555@8.8.8.8:443?security=tls"},
		{"trojan", "trojan://super-secret-password@8.8.4.4:443"},
		{"hysteria2", "hysteria2://another-secret@1.1.1.1:443"},
		{"tuic", "tuic://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:secret@9.9.9.9:443"},
		{"shadowsocks", "ss://YWVzLTEyOC1nY206c2VjcmV0@1.0.0.1:8388"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, ok := parseImportedNodeLine(tt.raw)
			if !ok {
				t.Fatalf("failed to parse %s", tt.raw)
			}
			for _, secret := range []string{
				"11111111-2222-3333-4444-555555555555",
				"super-secret-password",
				"another-secret",
				"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
				"secret",
			} {
				if strings.Contains(node.Name, secret) {
					t.Fatalf("credential leaked in fallback name %q", node.Name)
				}
			}
			if !strings.HasPrefix(node.Name, node.Type+"-") {
				t.Fatalf("unexpected safe fallback name %q", node.Name)
			}
		})
	}
}

func TestParseImportedNodesSupportsV2RayOutbounds(t *testing.T) {
	text := `{
  "outbounds": [
    {
      "tag": "demo",
      "protocol": "vmess",
      "settings": {
        "vnext": [
          {
            "address": "v2ray.cool",
            "port": 443,
            "users": [
              {
                "id": "a3482e88-686a-4a58-8126-99c9df64b7bf",
                "security": "auto",
                "alterId": 0
              }
            ]
          }
        ]
      },
      "streamSettings": {
        "network": "ws",
        "security": "tls",
        "tlsSettings": {
          "serverName": "edge.example.com",
          "fingerprint": "chrome",
          "allowInsecure": true,
          "alpn": "h2"
        },
        "wsSettings": {
          "path": "/ws",
          "headers": {
            "Host": "edge.example.com"
          }
        }
      }
    }
  ]
}`

	imported := parseImportedNodes(text)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}

	out, err := transport.ParseURI(imported[0].RawURI)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	if out["type"] != "vmess" || out["servername"] != "edge.example.com" {
		t.Fatalf("unexpected imported node: %#v", out)
	}
	wsOpts, ok := out["ws-opts"].(map[string]any)
	if !ok {
		t.Fatalf("ws-opts missing: %#v", out["ws-opts"])
	}
	headers, ok := wsOpts["headers"].(map[string]any)
	if !ok || headers["Host"] != "edge.example.com" {
		t.Fatalf("unexpected ws headers: %#v", wsOpts)
	}
}
