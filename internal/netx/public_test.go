package netx

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func TestIsPublicAddr(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"0.0.0.1", false},
		{"10.0.0.1", false},
		{"100.64.0.1", false},
		{"169.254.169.254", false},
		{"192.168.1.1", false},
		{"198.18.0.1", false},
		{"203.0.113.1", false},
		{"240.0.0.1", false},
		{"::1", false},
		{"fc00::1", false},
		{"fe80::1", false},
		{"2001:db8::1", false},
		{"fec0::1", false},
		{"64:ff9b::127.0.0.1", false},
		{"2001::1", false},
		{"2002:7f00:1::", false},
		{"::ffff:127.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := isPublicAddr(netip.MustParseAddr(tt.raw)); got != tt.want {
				t.Fatalf("isPublicAddr(%s)=%v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestValidateHTTPURLRejectsPrivateAndUnsafeURLs(t *testing.T) {
	ctx := context.Background()
	for _, rawURL := range []string{
		"file:///etc/passwd",
		"http://127.0.0.1/admin",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/",
		"http://localhost/",
	} {
		if _, err := ValidateHTTPURL(ctx, rawURL, false); err == nil {
			t.Fatalf("expected %q to be rejected", rawURL)
		}
	}
}

func TestValidateHTTPURLPrivateOptOutStillChecksScheme(t *testing.T) {
	ctx := context.Background()
	if _, err := ValidateHTTPURL(ctx, "http://127.0.0.1/list.txt", true); err != nil {
		t.Fatalf("private opt-out should allow a valid private HTTP URL: %v", err)
	}
	if _, err := ValidateHTTPURL(ctx, "file:///tmp/list.txt", true); err == nil {
		t.Fatal("private opt-out must not allow non-HTTP schemes")
	}
}

func TestRestrictedClientRejectsPrivateRedirectAndRedirectLoops(t *testing.T) {
	client := NewRestrictedHTTPClient(time.Second, false)
	privateRequest, err := http.NewRequest(http.MethodGet, "http://user@127.0.0.1/private", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(privateRequest, nil); err == nil {
		t.Fatal("redirect to private address should be rejected")
	}

	publicRequest, err := http.NewRequest(http.MethodGet, "https://8.8.8.8/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, maxHTTPRedirects)
	if err := client.CheckRedirect(publicRequest, via); err == nil {
		t.Fatal("redirect limit should be enforced")
	}
}
