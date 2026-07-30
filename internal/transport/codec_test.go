package transport

import (
	"encoding/base64"
	"testing"

	"github.com/metacubex/mihomo/adapter"
)

var benchmarkParsedURI map[string]any //nolint:gochecknoglobals

func BenchmarkParseURIBase64Variants(b *testing.B) {
	payload := []byte(`{"v":"2","ps":"demo","add":"vmess.example.com","port":"443","id":"12345678-1234-1234-1234-123456789012","aid":"0","net":"ws","host":"edge.example.com","path":"/ws","tls":"tls"}`)
	credentials := []byte("aes-256-gcm:benchmark-password")
	for name, uri := range map[string]string{
		"vmess_standard": "vmess://" + base64.StdEncoding.EncodeToString(payload),
		"vmess_url_raw":  "vmess://" + base64.RawURLEncoding.EncodeToString(payload),
		"ss_standard": "ss://" + base64.StdEncoding.EncodeToString(credentials) +
			"@proxy.example.com:8388#demo",
		"ss_url_raw": "ss://" + base64.RawURLEncoding.EncodeToString(credentials) +
			"@proxy.example.com:8388#demo",
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				var err error
				benchmarkParsedURI, err = ParseURI(uri)
				if err != nil || benchmarkParsedURI["type"] == nil {
					b.Fatalf("parsed=%#v, err=%v", benchmarkParsedURI, err)
				}
			}
		})
	}
}

func TestParseURIShadowsocksKeepsPortAndPlugin(t *testing.T) {
	raw := "ss://YWVzLTEyOC1nY206aGFNTE1YaXJCeW42ckdWaA@example.com:20111/?plugin=simple-obfs%3Bobfs%3Dhttp%3Bobfs-host%3Dcdn.example.com#demo"

	out, err := ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}

	if got := out["port"]; got != 20111 {
		t.Fatalf("expected port 20111, got %#v", got)
	}
	if got := out["plugin"]; got != "obfs" {
		t.Fatalf("expected plugin obfs, got %#v", got)
	}
	opts, ok := out["plugin-opts"].(map[string]any)
	if !ok {
		t.Fatalf("plugin-opts missing or wrong type: %#v", out["plugin-opts"])
	}
	if opts["mode"] != "http" || opts["host"] != "cdn.example.com" {
		t.Fatalf("unexpected plugin opts: %#v", opts)
	}
}

func TestParseURIVlessKeepsRealityAndWS(t *testing.T) {
	raw := "vless://12345678-1234-1234-1234-123456789012@cf.example.com:443?security=reality&sni=edge.example.com&fp=chrome&pbk=pubkey&sid=abcd&type=ws&host=edge.example.com&path=%2Fws&flow=xtls-rprx-vision#demo"

	out, err := ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}

	if got := out["servername"]; got != "edge.example.com" {
		t.Fatalf("expected servername edge.example.com, got %#v", got)
	}
	if got := out["client-fingerprint"]; got != "chrome" {
		t.Fatalf("expected client-fingerprint chrome, got %#v", got)
	}
	if got := out["flow"]; got != "xtls-rprx-vision" {
		t.Fatalf("expected flow preserved, got %#v", got)
	}
	realityOpts, ok := out["reality-opts"].(map[string]any)
	if !ok {
		t.Fatalf("reality-opts missing or wrong type: %#v", out["reality-opts"])
	}
	if realityOpts["public-key"] != "pubkey" || realityOpts["short-id"] != "abcd" {
		t.Fatalf("unexpected reality opts: %#v", realityOpts)
	}
	if got := out["network"]; got != "ws" {
		t.Fatalf("expected network ws, got %#v", got)
	}
	wsOpts, ok := out["ws-opts"].(map[string]any)
	if !ok {
		t.Fatalf("ws-opts missing or wrong type: %#v", out["ws-opts"])
	}
	headers, ok := wsOpts["headers"].(map[string]any)
	if !ok {
		t.Fatalf("ws headers missing or wrong type: %#v", wsOpts["headers"])
	}
	if wsOpts["path"] != "/ws" || headers["Host"] != "edge.example.com" {
		t.Fatalf("unexpected ws opts: %#v", wsOpts)
	}
}

func TestParseURIHy2KeepsPortRange(t *testing.T) {
	raw := "hy2://secret@203.10.99.51:20000?sni=www.bing.com&insecure=1&ports=20000-55000#demo"

	out, err := ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}

	if got := out["ports"]; got != "20000-55000" {
		t.Fatalf("expected ports preserved, got %#v", got)
	}
	if got := out["sni"]; got != "www.bing.com" {
		t.Fatalf("expected sni preserved, got %#v", got)
	}
	if got := out["skip-cert-verify"]; got != true {
		t.Fatalf("expected skip-cert-verify=true, got %#v", got)
	}
}

func TestParseURIHy2KeepsAuthorityPortHoppingAndFullAuth(t *testing.T) {
	raw := "hysteria2://user:p%40ss@hy2.example.com:20000-55000" +
		"?sni=edge.example.com&up=20%20mbps&down=100%20mbps" +
		"&hop_interval=10&pinSHA256=certificate-pin#demo"

	out, err := ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	if out["password"] != "user:p@ss" || out["port"] != 20000 ||
		out["ports"] != "20000-55000" {
		t.Fatalf("Hysteria2 auth or port hopping not preserved: %#v", out)
	}
	if out["up"] != "20 mbps" || out["down"] != "100 mbps" ||
		out["hop-interval"] != "10" || out["fingerprint"] != "certificate-pin" {
		t.Fatalf("Hysteria2 transport options not preserved: %#v", out)
	}

	delete(out, "fingerprint")
	proxy, err := adapter.ParseProxy(out)
	if err != nil {
		t.Fatalf("Mihomo rejected parsed Hysteria2 options: %v", err)
	}
	closeProxy(proxy)
}

func TestParseURISimpleRejectsInvalidEndpoint(t *testing.T) {
	for _, raw := range []string{
		"vless://uuid@:443",
		"trojan://secret@example.com:0",
		"tuic://token@example.com:65536",
	} {
		t.Run(raw, func(t *testing.T) {
			if out, err := ParseURI(raw); err == nil {
				t.Fatalf("ParseURI(%q) returned success with %#v", raw, out)
			}
		})
	}
}

func TestParseURITUICKeepsCredentialsAndTransportOptions(t *testing.T) {
	raw := "tuic://12345678-1234-1234-1234-123456789012:p%40ss@tuic.example.com:443" +
		"?sni=edge.example.com&allowInsecure=1&congestion_control=bbr" +
		"&alpn=h3%2Chq-29&disable_sni=1&udp_relay_mode=native#demo"

	out, err := ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}
	if out["uuid"] != "12345678-1234-1234-1234-123456789012" ||
		out["password"] != "p@ss" {
		t.Fatalf("TUIC v5 credentials not preserved: %#v", out)
	}
	if out["udp"] != true || out["congestion-controller"] != "bbr" ||
		out["disable-sni"] != true || out["udp-relay-mode"] != "native" {
		t.Fatalf("TUIC transport options not preserved: %#v", out)
	}
	if out["sni"] != "edge.example.com" || out["skip-cert-verify"] != true {
		t.Fatalf("TUIC TLS options not preserved: %#v", out)
	}
	alpn, ok := out["alpn"].([]string)
	if !ok || len(alpn) != 2 || alpn[0] != "h3" || alpn[1] != "hq-29" {
		t.Fatalf("TUIC ALPN not preserved: %#v", out["alpn"])
	}
	proxy, err := adapter.ParseProxy(out)
	if err != nil {
		t.Fatalf("Mihomo rejected parsed TUIC v5 options: %v", err)
	}
	closeProxy(proxy)

	legacy, err := ParseURI("tuic://legacy-token@tuic.example.com:443#legacy")
	if err != nil {
		t.Fatalf("ParseURI(TUIC v4) returned error: %v", err)
	}
	if legacy["token"] != "legacy-token" || legacy["uuid"] != nil ||
		legacy["password"] != nil {
		t.Fatalf("TUIC v4 token not preserved: %#v", legacy)
	}
	proxy, err = adapter.ParseProxy(legacy)
	if err != nil {
		t.Fatalf("Mihomo rejected parsed TUIC v4 options: %v", err)
	}
	closeProxy(proxy)
}

func TestParseURIVmessKeepsSNIAndFingerprint(t *testing.T) {
	rawJSON := `{"v":"2","ps":"demo","add":"vmess.example.com","port":"443","id":"12345678-1234-1234-1234-123456789012","aid":"0","net":"ws","host":"edge.example.com","path":"/ws","tls":"tls","sni":"edge.example.com","fp":"chrome","alpn":"h2,http/1.1","allowInsecure":"1"}`
	raw := "vmess://" + base64.StdEncoding.EncodeToString([]byte(rawJSON))

	out, err := ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI returned error: %v", err)
	}

	if out["servername"] != "edge.example.com" || out["client-fingerprint"] != "chrome" {
		t.Fatalf("tls metadata not preserved: %#v", out)
	}
	alpn, ok := out["alpn"].([]string)
	if !ok || len(alpn) != 2 || alpn[0] != "h2" {
		t.Fatalf("alpn not preserved: %#v", out["alpn"])
	}
}

func TestParseURIClashValidatesEncodedObject(t *testing.T) {
	validJSON := `{"name":"demo","type":"socks5","server":"proxy.example.com","port":1080}`
	valid := "clash://" + base64.StdEncoding.EncodeToString([]byte(validJSON))
	out, err := ParseURI(valid)
	if err != nil {
		t.Fatalf("ParseURI(valid clash URI): %v", err)
	}
	if out["type"] != "socks5" || out["server"] != "proxy.example.com" ||
		out["port"] != float64(1080) {
		t.Fatalf("unexpected parsed clash proxy: %#v", out)
	}

	tests := []struct {
		name string
		raw  string
	}{
		{name: "invalid base64", raw: "clash://%%%"},
		{
			name: "invalid JSON",
			raw:  "clash://" + base64.StdEncoding.EncodeToString([]byte("not-json")),
		},
		{
			name: "null JSON",
			raw:  "clash://" + base64.StdEncoding.EncodeToString([]byte("null")),
		},
		{
			name: "empty object",
			raw:  "clash://" + base64.StdEncoding.EncodeToString([]byte("{}")),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, err := ParseURI(test.raw)
			if err == nil {
				t.Fatalf("ParseURI(%q) returned success with %#v", test.raw, out)
			}
			if out != nil {
				t.Fatalf("ParseURI(%q) returned data on error: %#v", test.raw, out)
			}
		})
	}
}

func TestParseURIStandardProxies(t *testing.T) {
	tests := []struct {
		raw      string
		wantType string
		wantPort int
		wantUser string
	}{
		{"http://127.0.0.1:8080", "http", 8080, ""},
		{"https://user:pass@example.com:8443#secure", "https", 8443, "user"},
		{"socks4://127.0.0.1:1080", "socks4", 1080, ""},
		{"socks4a://name@proxy.example:1080", "socks4a", 1080, "name"},
		{"socks5://user:pass@127.0.0.1:1080", "socks5", 1080, "user"},
		{"socks5h://proxy.example:1080", "socks5h", 1080, ""},
	}
	for _, tt := range tests {
		t.Run(tt.wantType, func(t *testing.T) {
			out, err := ParseURI(tt.raw)
			if err != nil {
				t.Fatalf("ParseURI(%q): %v", tt.raw, err)
			}
			if out["type"] != tt.wantType || out["port"] != tt.wantPort {
				t.Fatalf("unexpected parsed proxy: %#v", out)
			}
			if tt.wantUser != "" && out["username"] != tt.wantUser {
				t.Fatalf("username=%#v, want %q", out["username"], tt.wantUser)
			}
		})
	}
}
