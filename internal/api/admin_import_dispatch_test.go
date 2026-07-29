package api

import (
	"encoding/base64"
	"testing"
)

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
