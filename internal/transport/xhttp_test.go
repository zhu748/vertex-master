package transport

import "testing"

func TestApplyXHTTPExtraOptions(t *testing.T) {
	options := map[string]any{}
	ApplyXHTTPExtraOptions(options, map[string]any{
		"noGRPCHeader":         true,
		"xPaddingBytes":        "100-1000",
		"xPaddingObfsMode":     true,
		"uplinkHttpMethod":     "PUT",
		"sessionIDPlacement":   "header",
		"sessionIDKey":         "X-Session",
		"sessionIDLength":      float64(8),
		"uplinkChunkSize":      float64(65536),
		"scMaxEachPostBytes":   "1000000",
		"scMinPostsIntervalMs": float64(20),
		"xmux": map[string]any{
			"maxConnections":   "1-4",
			"maxConcurrency":   float64(8),
			"hKeepAlivePeriod": float64(30),
		},
		"downloadSettings": map[string]any{
			"address":  "download.example.com",
			"port":     float64(8443),
			"security": "reality",
			"tlsSettings": map[string]any{
				"serverName":    "edge.example.com",
				"fingerprint":   "chrome",
				"alpn":          []any{"h2", "http/1.1"},
				"allowInsecure": true,
			},
			"realitySettings": map[string]any{
				"publicKey": "public-key",
				"shortId":   "abcd",
			},
			"xhttpSettings": map[string]any{
				"path":    "/download",
				"host":    "cdn.example.com",
				"headers": map[string]any{"X-Test": "value", "Ignored": true},
				"extra": map[string]any{
					"xmux": map[string]any{"maxConnections": float64(2)},
				},
			},
		},
	})

	if options["no-grpc-header"] != true ||
		options["x-padding-bytes"] != "100-1000" ||
		options["x-padding-obfs-mode"] != true ||
		options["uplink-http-method"] != "PUT" ||
		options["session-placement"] != "header" ||
		options["session-key"] != "X-Session" ||
		options["session-length"] != "8" ||
		options["uplink-chunk-size"] != "65536" ||
		options["sc-max-each-post-bytes"] != "1000000" ||
		options["sc-min-posts-interval-ms"] != "20" {
		t.Fatalf("root XHTTP extra options not converted: %#v", options)
	}
	reuse, ok := options["reuse-settings"].(map[string]any)
	if !ok || reuse["max-connections"] != "1-4" ||
		reuse["max-concurrency"] != "8" || reuse["h-keep-alive-period"] != 30 {
		t.Fatalf("XHTTP reuse settings not converted: %#v", options["reuse-settings"])
	}
	download, ok := options["download-settings"].(map[string]any)
	if !ok || download["server"] != "download.example.com" ||
		download["port"] != 8443 || download["tls"] != true ||
		download["servername"] != "edge.example.com" ||
		download["client-fingerprint"] != "chrome" ||
		download["skip-cert-verify"] != true ||
		download["path"] != "/download" || download["host"] != "cdn.example.com" {
		t.Fatalf("XHTTP download settings not converted: %#v", options["download-settings"])
	}
	headers, ok := download["headers"].(map[string]string)
	if !ok || len(headers) != 1 || headers["X-Test"] != "value" {
		t.Fatalf("XHTTP download headers not converted safely: %#v", download["headers"])
	}
	reality, ok := download["reality-opts"].(map[string]any)
	if !ok || reality["public-key"] != "public-key" || reality["short-id"] != "abcd" {
		t.Fatalf("XHTTP download REALITY settings not converted: %#v", reality)
	}
	downloadReuse, ok := download["reuse-settings"].(map[string]any)
	if !ok || downloadReuse["max-connections"] != "2" {
		t.Fatalf("XHTTP download reuse settings not converted: %#v", downloadReuse)
	}
}

func TestApplyXHTTPExtraOptionsIgnoresInvalidTypes(t *testing.T) {
	options := map[string]any{"path": "/stable"}
	ApplyXHTTPExtraOptions(options, map[string]any{
		"noGRPCHeader":    "true",
		"sessionIDLength": true,
		"xmux":            []any{"invalid"},
		"downloadSettings": map[string]any{
			"port":    1.5,
			"headers": []any{"invalid"},
		},
	})
	if len(options) != 1 || options["path"] != "/stable" {
		t.Fatalf("invalid XHTTP extra mutated options: %#v", options)
	}
}
