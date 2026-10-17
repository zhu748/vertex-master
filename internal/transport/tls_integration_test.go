package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	tls_client "github.com/bogdanfinn/tls-client"
)

// Exercise the patched TLS libraries over real, locally trusted HTTPS rather
// than relying only on HTTP mocks. Force P-384 to cover curve negotiation.
func TestBrowserProfilesTLSAndHTTP2(t *testing.T) {
	for _, version := range []struct {
		name  string
		value uint16
	}{{"TLS12", tls.VersionTLS12}, {"TLS13", tls.VersionTLS13}} {
		t.Run(version.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 || r.TLS == nil || r.TLS.Version != version.value {
					http.Error(w, "unexpected TLS/HTTP protocol", 500)
					return
				}
				w.Header().Set("X-Stream-Test", "secure")
				_, _ = io.WriteString(w, "data: secure\n\n")
			}))
			server.EnableHTTP2 = true
			server.TLS = &tls.Config{MinVersion: version.value, MaxVersion: version.value, CurvePreferences: []tls.CurveID{tls.CurveP384}}
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			for _, profile := range browserProfiles {
				t.Run(profile.GetClientHelloStr(), func(t *testing.T) {
					client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
						tls_client.WithClientProfile(profile), tls_client.WithTimeoutSeconds(5),
						tls_client.WithTransportOptions(&tls_client.TransportOptions{RootCAs: roots}))
					if err != nil {
						t.Fatal(err)
					}
					session := &Session{client: client}
					defer session.Close()
					response, err := session.DoStream(context.Background(), "GET", server.URL, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Close()
					body, err := ReadAllLimit(response.Body, 1024)
					if err != nil || response.StatusCode != 200 || response.Header.Get("X-Stream-Test") != "secure" || string(body) != "data: secure\n\n" {
						t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, err)
					}
				})
			}
		})
	}
}
