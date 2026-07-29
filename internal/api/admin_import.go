package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/netx"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

const subscriptionFetchUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func parseImportedNodes(text string) []nodes.Node {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	normalized := maybeDecodeSubscriptionText(text)
	if looksLikeJSONDocument(normalized) {
		if imported := parseJSONImportedNodes(normalized); len(imported) > 0 {
			return imported
		}
		if imported := parseClashYAMLToNodes(normalized); len(imported) > 0 {
			return imported
		}
		return parseImportedNodeLines(normalized)
	}
	if looksLikeNodeLineList(normalized) {
		return parseImportedNodeLines(normalized)
	}
	if imported := parseClashYAMLToNodes(normalized); len(imported) > 0 {
		return imported
	}
	if imported := parseJSONImportedNodes(normalized); len(imported) > 0 {
		return imported
	}
	return parseImportedNodeLines(normalized)
}

func parseImportedNodeLines(text string) []nodes.Node {
	capacity := min(strings.Count(text, "\n")+1, 4096)
	imported := make([]nodes.Node, 0, capacity)
	for line := range strings.SplitSeq(text, "\n") {
		if node, ok := parseFlexibleImportedNodeLine(line); ok {
			imported = append(imported, node)
		}
	}
	return imported
}

func maybeDecodeSubscriptionText(text string) string {
	b, err := decodeSubBase64(text)
	if err != nil {
		return text
	}

	decodedBytes := bytes.TrimSpace(b)
	if len(decodedBytes) == 0 {
		return text
	}
	decoded := string(decodedBytes)
	if strings.Contains(decoded, "proxies:") || looksLikeNodeLineList(decoded) || json.Valid(decodedBytes) {
		return decoded
	}
	return text
}

func looksLikeJSONDocument(text string) bool {
	if text == "" {
		return false
	}
	return text[0] == '{' || text[0] == '['
}

func looksLikeNodeLineList(text string) bool {
	foundNodeURI := false
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if hasLikelyURIScheme(line) {
			foundNodeURI = true
			continue
		}
		if looksLikeStructuredImportLine(line) {
			return false
		}
	}
	return foundNodeURI
}

func hasLikelyURIScheme(text string) bool {
	separator := strings.Index(text, "://")
	if separator < 1 {
		return false
	}
	for index := range separator {
		char := text[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') {
			continue
		}
		if index > 0 && ((char >= '0' && char <= '9') || char == '+' || char == '-' || char == '.') {
			continue
		}
		return false
	}
	return true
}

func looksLikeStructuredImportLine(line string) bool {
	if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "-") {
		return true
	}
	separator := strings.IndexByte(line, ':')
	return separator >= 0 && (separator == len(line)-1 || line[separator+1] == ' ' || line[separator+1] == '\t')
}

func parseImportedNodeLine(line string) (nodes.Node, bool) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return nodes.Node{}, false
	}

	out, err := transport.ParseURI(raw)
	if err != nil {
		return nodes.Node{}, false
	}

	nodeType := strings.TrimSpace(valueToString(out["type"]))
	if nodeType == "" {
		return nodes.Node{}, false
	}

	nodeName := extractImportedNodeName(raw, out)
	if nodeName == "" {
		nodeName = importedNodeFallbackName(nodeType, out)
	}
	nodeName = nodes.SafeNodeLabel(nodeName)
	return nodes.Node{Type: nodeType, Name: nodeName, RawURI: raw}, true
}

func importedNodeFallbackName(nodeType string, out map[string]any) string {
	server := strings.TrimSpace(valueToString(out["server"]))
	port := intValue(out["port"])
	switch {
	case server != "" && port > 0:
		return nodeType + "-" + net.JoinHostPort(server, strconv.Itoa(port))
	case server != "":
		return nodeType + "-" + server
	default:
		return nodeType + "-node"
	}
}

func extractImportedNodeName(raw string, out map[string]any) string {
	if name := strings.TrimSpace(valueToString(out["name"])); name != "" {
		return name
	}

	if strings.HasPrefix(raw, "vmess://") {
		b64Str := raw[8:]
		if idx := strings.Index(b64Str, "?"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		if idx := strings.Index(b64Str, "#"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		b64Str = strings.ReplaceAll(strings.ReplaceAll(b64Str, "-", "+"), "_", "/")
		if pad := len(b64Str) % 4; pad != 0 {
			b64Str += strings.Repeat("=", 4-pad)
		}
		if b, err := base64.StdEncoding.DecodeString(b64Str); err == nil {
			var d map[string]any
			if errUnm := json.Unmarshal(b, &d); errUnm == nil {
				if ps, ok := d["ps"].(string); ok {
					return strings.TrimSpace(ps)
				}
			}
		}
	}

	if idx := strings.Index(raw, "#"); idx != -1 {
		escapedName := raw[idx+1:]
		if dec, err := url.QueryUnescape(escapedName); err == nil {
			return strings.TrimSpace(dec)
		}
		return strings.TrimSpace(escapedName)
	}

	return ""
}

func parseFlexibleImportedNodeLine(line string) (nodes.Node, bool) {
	if node, ok := parseImportedNodeLine(line); ok {
		return node, true
	}
	return parseV2RayNNodeLine(line)
}

func parseProxyListNodes(text, defaultType string) []nodes.Node {
	defaultType = strings.ToLower(strings.TrimSpace(defaultType))
	if defaultType == "" || defaultType == "auto" {
		return parseImportedNodes(text)
	}

	imported := make([]nodes.Node, 0)
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		raw, ok := normalizeProxyListLine(line, defaultType)
		if !ok || seen[raw] {
			continue
		}
		node, ok := parseImportedNodeLine(raw)
		if !ok {
			continue
		}
		seen[raw] = true
		imported = append(imported, node)
	}
	return imported
}

func normalizeProxyListLine(line, defaultType string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
		return "", false
	}
	if fields := strings.Fields(line); len(fields) > 0 {
		line = fields[0]
	}

	if parsed, err := url.Parse(line); err == nil && strings.Contains(line, "://") {
		scheme := strings.ToLower(parsed.Scheme)
		if !isStandardProxyType(scheme) || parsed.Hostname() == "" {
			return "", false
		}
		parsed.Scheme = scheme
		return parsed.String(), true
	}
	if !isStandardProxyType(defaultType) {
		return "", false
	}

	// 常见纯文本格式：host:port:user:pass。
	parts := strings.Split(line, ":")
	if len(parts) >= 4 && !strings.Contains(parts[0], "[") {
		port, err := strconv.Atoi(parts[1])
		if err == nil && port > 0 && port <= 65535 {
			u := &url.URL{
				Scheme: defaultType,
				User:   url.UserPassword(parts[2], strings.Join(parts[3:], ":")),
				Host:   net.JoinHostPort(parts[0], parts[1]),
			}
			return u.String(), true
		}
	}

	u, err := url.Parse(defaultType + "://" + line)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", false
	}
	return u.String(), true
}

func isStandardProxyType(proxyType string) bool {
	switch strings.ToLower(strings.TrimSpace(proxyType)) {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
		return true
	default:
		return false
	}
}

// filterRemoteSubscriptionNodes prevents a remote list from turning the
// health checker or request router into a connection primitive for local
// services. Manual imports remain unrestricted for legitimate LAN proxies.
func filterRemoteSubscriptionNodes(
	ctx context.Context,
	imported []nodes.Node,
	allowPrivate bool,
	allowDomains bool,
) ([]nodes.Node, int, error) {
	if allowPrivate {
		return imported, 0, nil
	}
	allowedHosts := make(map[string]bool)
	filtered := make([]nodes.Node, 0, len(imported))
	rejected := 0
	for _, node := range imported {
		if err := ctx.Err(); err != nil {
			return nil, rejected, err
		}
		parsed, err := transport.ParseURI(node.RawURI)
		if err != nil {
			rejected++
			continue
		}
		host := strings.TrimSpace(valueToString(parsed["server"]))
		if _, parseErr := netip.ParseAddr(strings.Trim(host, "[]")); parseErr != nil && !allowDomains {
			rejected++
			continue
		}
		cacheKey := strings.ToLower(strings.TrimSuffix(host, "."))
		allowed, cached := allowedHosts[cacheKey]
		if !cached {
			hostCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			validateErr := netx.ValidatePublicHost(hostCtx, host)
			cancel()
			allowed = validateErr == nil
			allowedHosts[cacheKey] = allowed
		}
		if !allowed {
			rejected++
			continue
		}
		filtered = append(filtered, node)
	}
	return filtered, rejected, nil
}

func (adm *AdminHandler) adminImportNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string `json:"text"`
		Replace bool   `json:"replace"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [ImportNodes] 收到优选节点文件导入请求, 替换模式: %v", body.Replace)

	newNodes := parseImportedNodes(strings.TrimSpace(body.Text))
	if len(newNodes) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("导入内容中没有有效节点，未修改现有代理"))
		return
	}
	if body.Replace {
		log.Printf("[Admin] [ImportNodes] 替换模式，正在原子替换手动节点")
		if err := nodes.ReplaceManualNodes(newNodes); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("替换节点失败: "+err.Error()))
			return
		}
	} else {
		log.Printf("[Admin] [ImportNodes] 正在合并导入的新节点数量: %d", len(newNodes))
		if err := nodes.MergeNodes(newNodes); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("保存节点失败: "+err.Error()))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(newNodes)})
}

func (adm *AdminHandler) adminImportNodesJson(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string `json:"text"`
		Replace bool   `json:"replace"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [ImportNodesJson] 收到旧版 nodes.json 导入请求, 替换模式: %v", body.Replace)

	var d struct {
		Nodes []nodes.Node `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body.Text), &d); err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("JSON 解析失败: "+err.Error()))
		return
	}
	validNodes := make([]nodes.Node, 0, len(d.Nodes))
	for _, node := range d.Nodes {
		if strings.TrimSpace(node.RawURI) != "" {
			validNodes = append(validNodes, node)
		}
	}
	if len(validNodes) == 0 {
		writeJSON(w, http.StatusBadRequest, adminErr("JSON 中没有有效节点，未修改现有代理"))
		return
	}

	if body.Replace {
		log.Printf("[Admin] [ImportNodesJson] 替换模式，正在原子替换手动节点")
		if err := nodes.ReplaceManualNodes(validNodes); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("替换节点失败: "+err.Error()))
			return
		}
	} else {
		log.Printf("[Admin] [ImportNodesJson] 正在合并导入的新节点数量: %d", len(validNodes))
		if err := nodes.MergeNodes(validNodes); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("保存节点失败: "+err.Error()))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(validNodes)})
}
