package api

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestEncodeClashProxyURIMatchesCanonicalEncoding(t *testing.T) {
	proxy := map[string]any{
		"name":    "node <one>",
		"type":    "http",
		"server":  "proxy.example.com",
		"port":    float64(8080),
		"headers": map[string]any{"X-Test": "a&b"},
	}
	body, err := json.Marshal(proxy)
	if err != nil {
		t.Fatal(err)
	}
	want := "clash://" + base64.StdEncoding.EncodeToString(body)
	if got := buildClashURI(proxy); got != want {
		t.Fatalf("buildClashURI()=%q, want %q", got, want)
	}
	if got := clashProxyObjectToURI(proxy); got != want {
		t.Fatalf("clashProxyObjectToURI()=%q, want %q", got, want)
	}
}

func TestParseImportedNodesDecodesJSONSubscription(t *testing.T) {
	payload := `[{"type":"http","name":"encoded","server":"proxy.example.com","port":8080}]`
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))

	imported := parseImportedNodes(encoded)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}
	if imported[0].Type != "http" || imported[0].Name != "encoded" {
		t.Fatalf("unexpected imported node: %#v", imported[0])
	}
}

func TestParseImportedNodesJSONMetadataMatchesEncodedClashNode(t *testing.T) {
	imported := parseImportedNodes(`[
		{"type":"HTTP","server":"proxy.example.com","port":8080},
		{"type":"socks5","name":"unsafe\n\u200bname","server":"socks.example.com","port":1080}
	]`)
	if len(imported) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(imported))
	}

	for index, node := range imported {
		reparsed, ok := parseImportedNodeLine(node.RawURI)
		if !ok {
			t.Fatalf("node %d has an invalid encoded Clash URI: %#v", index, node)
		}
		if node.Type != reparsed.Type || node.Name != reparsed.Name {
			t.Fatalf("node %d metadata differs from encoded URI: direct=%#v reparsed=%#v", index, node, reparsed)
		}
	}
	if imported[0].Type != "HTTP" || imported[0].Name != "HTTP-proxy.example.com:8080" {
		t.Fatalf("fallback metadata changed: %#v", imported[0])
	}
	if imported[1].Name != "unsafename" {
		t.Fatalf("unsafe imported label was not normalized: %q", imported[1].Name)
	}
}

func TestParseImportedNodesURIListAllowsNonStructuredNoise(t *testing.T) {
	text := `# generated subscription
this line is ignored
http://user:pass@proxy.example.com:8080#first
not a node either
socks5://user:pass@proxy.example.com:1080#second`

	imported := parseImportedNodes(text)
	if len(imported) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(imported))
	}
	if imported[0].Name != "first" || imported[1].Name != "second" {
		t.Fatalf("unexpected imported nodes: %#v", imported)
	}
}

func TestParseImportedNodesDoesNotTreatYAMLURLAsNodeList(t *testing.T) {
	text := `proxy-providers:
  example:
    url: https://provider.example.com/subscription
proxies:
  - { name: 'yaml-node', type: http, server: proxy.example.com, port: 8080, username: user, password: pass }`

	imported := parseImportedNodes(text)
	if len(imported) != 1 {
		t.Fatalf("expected 1 node, got %d", len(imported))
	}
	if imported[0].Type != "http" || imported[0].Name != "yaml-node" {
		t.Fatalf("unexpected imported node: %#v", imported[0])
	}
}

func TestLooksLikeNodeLineListRejectsStructuredDocuments(t *testing.T) {
	if !looksLikeNodeLineList("ignored\nss://method:password@example.com:443#node") {
		t.Fatal("expected URI list to be detected")
	}
	if looksLikeNodeLineList("source: https://example.com/subscription") {
		t.Fatal("YAML mapping must not be detected as a URI list")
	}
}
