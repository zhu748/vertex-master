package transport

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// ApplyXHTTPExtraOptions converts Xray's XHTTP extra object into the option
// names and value types consumed by Mihomo.
func ApplyXHTTPExtraOptions(options, extra map[string]any) {
	if options == nil || len(extra) == 0 {
		return
	}

	if value, ok := extra["noGRPCHeader"].(bool); ok && value {
		options["no-grpc-header"] = true
	}
	if value, ok := extra["xPaddingObfsMode"].(bool); ok {
		options["x-padding-obfs-mode"] = value
	}
	for _, field := range [][2]string{
		{"xPaddingBytes", "x-padding-bytes"},
		{"xPaddingKey", "x-padding-key"},
		{"xPaddingHeader", "x-padding-header"},
		{"xPaddingPlacement", "x-padding-placement"},
		{"xPaddingMethod", "x-padding-method"},
		{"uplinkHttpMethod", "uplink-http-method"},
		{"sessionIDTable", "session-table"},
		{"seqPlacement", "seq-placement"},
		{"seqKey", "seq-key"},
		{"uplinkDataPlacement", "uplink-data-placement"},
		{"uplinkDataKey", "uplink-data-key"},
	} {
		if value, ok := xhttpStringValue(extra[field[0]]); ok {
			options[field[1]] = value
		}
	}
	for _, field := range []struct {
		target  string
		sources []string
	}{
		{target: "session-placement", sources: []string{"sessionIDPlacement", "sessionPlacement"}},
		{target: "session-key", sources: []string{"sessionIDKey", "sessionKey"}},
	} {
		if value, ok := firstXHTTPString(extra, field.sources...); ok {
			options[field.target] = value
		}
	}
	for _, field := range [][2]string{
		{"sessionIDLength", "session-length"},
		{"uplinkChunkSize", "uplink-chunk-size"},
		{"scMaxEachPostBytes", "sc-max-each-post-bytes"},
		{"scMinPostsIntervalMs", "sc-min-posts-interval-ms"},
	} {
		if value, ok := xhttpScalarString(extra[field[0]]); ok {
			options[field[1]] = value
		}
	}

	if reuse := xhttpReuseSettings(extra["xmux"]); len(reuse) > 0 {
		options["reuse-settings"] = reuse
	}
	if download := xhttpDownloadSettings(extra["downloadSettings"]); len(download) > 0 {
		options["download-settings"] = download
	}
}

func xhttpReuseSettings(value any) map[string]any {
	source, ok := value.(map[string]any)
	if !ok || len(source) == 0 {
		return nil
	}
	reuse := make(map[string]any)
	for _, field := range [][2]string{
		{"maxConnections", "max-connections"},
		{"maxConcurrency", "max-concurrency"},
		{"cMaxReuseTimes", "c-max-reuse-times"},
		{"hMaxRequestTimes", "h-max-request-times"},
		{"hMaxReusableSecs", "h-max-reusable-secs"},
	} {
		if converted, exists := xhttpScalarString(source[field[0]]); exists {
			reuse[field[1]] = converted
		}
	}
	if keepAlive, exists := xhttpIntValue(source["hKeepAlivePeriod"]); exists {
		reuse["h-keep-alive-period"] = keepAlive
	}
	return reuse
}

func xhttpDownloadSettings(value any) map[string]any {
	source, ok := value.(map[string]any)
	if !ok || len(source) == 0 {
		return nil
	}
	download := make(map[string]any)
	if server, exists := xhttpStringValue(source["address"]); exists {
		download["server"] = server
	}
	if port, exists := xhttpIntValue(source["port"]); exists {
		download["port"] = port
	}

	security, _ := xhttpStringValue(source["security"])
	security = strings.ToLower(security)
	if security == "tls" || security == "reality" {
		download["tls"] = true
		if tlsSettings, tlsOK := source["tlsSettings"].(map[string]any); tlsOK {
			if serverName, exists := xhttpStringValue(tlsSettings["serverName"]); exists {
				download["servername"] = serverName
			}
			if fingerprint, exists := xhttpStringValue(tlsSettings["fingerprint"]); exists {
				download["client-fingerprint"] = fingerprint
			}
			if alpn := xhttpStringSlice(tlsSettings["alpn"]); len(alpn) > 0 {
				download["alpn"] = alpn
			}
			if allowInsecure, exists := tlsSettings["allowInsecure"].(bool); exists && allowInsecure {
				download["skip-cert-verify"] = true
			}
		}
		if security == "reality" {
			if reality, realityOK := source["realitySettings"].(map[string]any); realityOK {
				realityOptions := make(map[string]any)
				if publicKey, exists := xhttpStringValue(reality["publicKey"]); exists {
					realityOptions["public-key"] = publicKey
				}
				if shortID, exists := xhttpStringValue(reality["shortId"]); exists {
					realityOptions["short-id"] = shortID
				}
				if len(realityOptions) > 0 {
					download["reality-opts"] = realityOptions
				}
			}
		}
	}

	if xhttpSettings, settingsOK := source["xhttpSettings"].(map[string]any); settingsOK {
		if path, exists := xhttpStringValue(xhttpSettings["path"]); exists {
			download["path"] = path
		}
		if host, exists := xhttpStringValue(xhttpSettings["host"]); exists {
			download["host"] = host
		}
		if headers := xhttpStringMap(xhttpSettings["headers"]); len(headers) > 0 {
			download["headers"] = headers
		}
		if nestedExtra, extraOK := xhttpSettings["extra"].(map[string]any); extraOK {
			if reuse := xhttpReuseSettings(nestedExtra["xmux"]); len(reuse) > 0 {
				download["reuse-settings"] = reuse
			}
		}
	}
	return download
}

func firstXHTTPString(values map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := xhttpStringValue(values[key]); ok {
			return value, true
		}
	}
	return "", false
}

func xhttpStringValue(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok && text != ""
}

func xhttpScalarString(value any) (string, bool) {
	if text, ok := xhttpStringValue(value); ok {
		return text, true
	}
	switch number := value.(type) {
	case json.Number:
		if number == "" {
			return "", false
		}
		return string(number), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return "", false
		}
		return strconv.FormatFloat(number, 'f', -1, 64), true
	case int:
		return strconv.Itoa(number), true
	case int64:
		return strconv.FormatInt(number, 10), true
	case uint64:
		return strconv.FormatUint(number, 10), true
	default:
		return "", false
	}
}

func xhttpIntValue(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		parsed, err := strconv.Atoi(strconv.FormatInt(number, 10))
		return parsed, err == nil
	case uint64:
		parsed, err := strconv.Atoi(strconv.FormatUint(number, 10))
		return parsed, err == nil
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
			return 0, false
		}
		parsed, err := strconv.Atoi(strconv.FormatFloat(number, 'f', 0, 64))
		return parsed, err == nil
	case json.Number:
		parsed, err := strconv.Atoi(string(number))
		return parsed, err == nil
	case string:
		parsed, err := strconv.Atoi(number)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func xhttpStringSlice(value any) []string {
	switch items := value.(type) {
	case []string:
		return append([]string(nil), items...)
	case []any:
		result := make([]string, 0, len(items))
		for _, item := range items {
			if text, ok := xhttpStringValue(item); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func xhttpStringMap(value any) map[string]string {
	switch entries := value.(type) {
	case map[string]string:
		result := make(map[string]string, len(entries))
		for key, entry := range entries {
			result[key] = entry
		}
		return result
	case map[string]any:
		result := make(map[string]string, len(entries))
		for key, entry := range entries {
			if text, ok := entry.(string); ok {
				result[key] = text
			}
		}
		return result
	default:
		return nil
	}
}
