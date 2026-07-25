package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const maxHTTPRedirects = 5

// ValidateHTTPURL validates an HTTP(S) URL and, unless explicitly allowed,
// requires every address returned for its host to be publicly routable.
func ValidateHTTPURL(ctx context.Context, rawURL string, allowPrivate bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return nil, errors.New("URL must be a valid HTTP/HTTPS URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("URL must use HTTP or HTTPS")
	}
	if allowPrivate {
		return parsed, nil
	}
	if err := ValidatePublicHost(ctx, parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}

// ValidatePublicHost resolves host and rejects loopback, private, link-local,
// multicast, documentation and other non-public address ranges.
func ValidatePublicHost(ctx context.Context, host string) error {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return errors.New("host is empty")
	}
	lowerHost := strings.ToLower(strings.TrimSuffix(host, "."))
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") ||
		strings.HasSuffix(lowerHost, ".local") {
		return fmt.Errorf("host %q is not publicly routable", host)
	}

	addrs, err := lookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("host %q has no addresses", host)
	}
	for _, addr := range addrs {
		if !isPublicAddr(addr) {
			return fmt.Errorf("host %q resolves to non-public address %s", host, addr)
		}
	}
	return nil
}

// NewRestrictedHTTPClient returns a direct HTTP client with bounded redirects.
// In public-only mode DNS is resolved and checked again at dial time, and the
// socket is opened against the validated literal IP to prevent DNS rebinding.
func NewRestrictedHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	transport := newHTTPTransport()
	// Subscription fetching has an explicit proxy fallback. Do not implicitly
	// inherit HTTP_PROXY here because that would bypass the dial-time checks.
	transport.Proxy = nil
	if !allowPrivate {
		transport.DialContext = publicDialContext
	}

	return &http.Client{ //nolint:exhaustruct
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxHTTPRedirects {
				return errors.New("too many redirects")
			}
			_, err := ValidateHTTPURL(req.Context(), req.URL.String(), allowPrivate)
			return err
		},
	}
}

func lookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	resolver := net.DefaultResolver
	if termuxResolver := newTermuxResolver(); termuxResolver != nil {
		resolver = termuxResolver
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, ip.Unmap())
	}
	return addrs, nil
}

func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid dial address: %w", err)
	}
	addrs, err := lookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("host %q has no addresses", host)
	}
	for _, addr := range addrs {
		if !isPublicAddr(addr) {
			return nil, fmt.Errorf("host %q resolves to non-public address %s", host, addr)
		}
	}

	dialer := &net.Dialer{ //nolint:exhaustruct
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	var lastErr error
	for _, addr := range addrs {
		if network == "tcp4" && !addr.Is4() {
			continue
		}
		if network == "tcp6" && !addr.Is6() {
			continue
		}
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("no compatible public address")
	}
	return nil, lastErr
}

func isPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() ||
		addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() ||
		addr.IsUnspecified() {
		return false
	}
	// Go's IsGlobalUnicast deliberately includes several special-purpose
	// networks. Use a conservative allowlist for IPv6 and explicitly remove
	// non-routable/special IPv4 ranges that can otherwise bypass SSRF checks.
	blocked := []string{}
	if addr.Is4() {
		blocked = []string{
			"0.0.0.0/8",
			"100.64.0.0/10",
			"192.0.0.0/24",
			"192.0.2.0/24",
			"192.88.99.0/24",
			"198.18.0.0/15",
			"198.51.100.0/24",
			"203.0.113.0/24",
			"240.0.0.0/4",
		}
	} else {
		if !netip.MustParsePrefix("2000::/3").Contains(addr) {
			return false
		}
		blocked = []string{
			"2001::/23",
			"2001:db8::/32",
			"2002::/16",
			"3ffe::/16",
			"3fff::/20",
			"5f00::/16",
		}
	}
	for _, rawPrefix := range blocked {
		if netip.MustParsePrefix(rawPrefix).Contains(addr) {
			return false
		}
	}
	return true
}
