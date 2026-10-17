package redact

import (
	"strings"
	"testing"
)

func TestCompatibilityBodiesAndEscapedCredentialNames(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"PRIVATE_PROMPT"}],"api\u005fkey":"PRIVATE_KEY"}`,
		`{"input":"PRIVATE_PROMPT","instructions":"PRIVATE_SYSTEM"}`,
		`{"system":"PRIVATE_SYSTEM","messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"value":"PRIVATE_ARG"}}]}]}`,
		`{"fileData":{"fileUri":"https://storage.example/PRIVATE_MEDIA"}}`,
		`Upstream Error: {"contents":[{"parts":[{"text":"PRIVATE_PROMPT"}]}],"token":"PRIVATE_KEY"}`,
		`["rresp","PRIVATE_RECAPTCHA"]`,
	} {
		if got := Excerpt(body, 2000); strings.Contains(got, "PRIVATE_") {
			t.Fatalf("content leaked: %s", got)
		}
	}
}

func TestProxyURLAndEncodedQueryCredentials(t *testing.T) {
	for _, raw := range []string{
		"dial https://user:PRIVATE_PASSWORD@proxy.example:8443 failed",
		"GET https://example/?%6bey=PRIVATE_KEY&alt=json failed",
		"https://example/?recaptchaToken=PRIVATE_TOKEN",
	} {
		if got := Excerpt(raw, 1000); strings.Contains(got, "PRIVATE_") {
			t.Fatalf("credentials leaked: %s", got)
		}
	}
}

func TestShortBearerCredentialIsMasked(t *testing.T) {
	got := Excerpt("Authorization: Bearer z9 denied", 100)
	if strings.Contains(got, "z9") || !strings.Contains(got, CredentialMarker) {
		t.Fatalf("short credential leaked: %s", got)
	}
}
