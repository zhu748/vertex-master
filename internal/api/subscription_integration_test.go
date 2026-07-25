package api

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// Run explicitly with VPROXY_TEST_SUBSCRIPTION_URL to verify a real provider
// through the same fetch, size-limit, parser and endpoint-safety path used in
// production. It is skipped during normal offline test runs.
func TestPublicProxySubscriptionIntegration(t *testing.T) {
	rawURL := os.Getenv("VPROXY_TEST_SUBSCRIPTION_URL")
	if rawURL == "" {
		t.Skip("VPROXY_TEST_SUBSCRIPTION_URL is not set")
	}
	cfg := config.DefaultConfig()
	allowPrivate, _ := strconv.ParseBool(os.Getenv("VPROXY_TEST_ALLOW_PRIVATE"))
	cfg.AllowPrivateSubscriptionURLs = allowPrivate
	adm := &AdminHandler{handler: handler{cfg: config.StaticProvider(cfg)}} //nolint:exhaustruct
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	text, err := adm.fetchSubscriptionText(ctx, rawURL)
	if err != nil {
		t.Fatalf("fetch subscription: %v", err)
	}
	imported := parseProxyListNodes(text, "http")
	filtered, rejected, err := filterRemoteSubscriptionNodes(ctx, imported, allowPrivate, false)
	if err != nil {
		t.Fatalf("validate subscription endpoints: %v", err)
	}
	if len(filtered) == 0 {
		t.Fatalf("subscription produced no safe proxy nodes (parsed=%d rejected=%d)", len(imported), rejected)
	}
	t.Logf("parsed=%d safe=%d rejected=%d", len(imported), len(filtered), rejected)
}
