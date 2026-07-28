package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Header 是 fhttp.Header 的别名，让 recaptcha/vertex 能构造请求头而不直接 import fhttp。
type Header = http.Header

// Response 是 fhttp.Response 的别名。
type Response = http.Response

// ErrResponseBodyTooLarge 表示受限读取超过调用方允许的响应体大小。
var ErrResponseBodyTooLarge = errors.New("response body too large")

// Session 封装一个独立的 tls-client，服务于单次逻辑请求。
type Session struct {
	client   tls_client.HttpClient
	ProxyURI string
}

func (s *Session) Do(ctx context.Context, method, url string, header http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("构造 %s %s 请求: %w", method, url, err)
	}
	req = req.WithContext(ctx)
	if header != nil {
		req.Header = header
	}
	return s.client.Do(req) //nolint:wrapcheck
}

func (s *Session) DoAndRead(ctx context.Context, method, url string, header http.Header, body io.Reader) (int, []byte, error) {
	return s.DoAndReadLimit(ctx, method, url, header, body, 0)
}

// DoAndReadLimit 发出请求并读取响应体；maxBytes 大于 0 时限制内存读取大小。
func (s *Session) DoAndReadLimit(
	ctx context.Context,
	method string,
	url string,
	header http.Header,
	body io.Reader,
	maxBytes int64,
) (int, []byte, error) {
	resp, err := s.Do(ctx, method, url, header, body)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := ReadAllLimit(resp.Body, maxBytes)
	if readErr != nil {
		return resp.StatusCode, data, fmt.Errorf("读取响应体: %w", readErr)
	}
	return resp.StatusCode, data, nil
}

// ReadAllLimit 读取 reader。maxBytes <= 0 表示不限；超限时返回限制内的前缀和
// ErrResponseBodyTooLarge，便于错误日志保留有界的上游详情。
func ReadAllLimit(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(reader)
	}
	if maxBytes == int64(^uint64(0)>>1) {
		return io.ReadAll(reader)
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], fmt.Errorf("%w: limit %d bytes", ErrResponseBodyTooLarge, maxBytes)
	}
	return data, nil
}

type StreamResponse struct { //nolint:govet
	StatusCode int
	Body       io.ReadCloser
}

func (sr *StreamResponse) Close() {
	body := sr.Body
	if body == nil {
		return
	}
	sr.Body = nil
	_ = body.Close()
}

func (s *Session) DoStream(ctx context.Context, method, url string, header http.Header, body io.Reader) (*StreamResponse, error) {
	resp, err := s.Do(ctx, method, url, header, body)
	if err != nil {
		return nil, err
	}
	return &StreamResponse{StatusCode: resp.StatusCode, Body: resp.Body}, nil
}

func (s *Session) Close() {
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
}

// SetFollowRedirect controls automatic redirects for callers that must
// validate every redirect target before sending the next request.
func (s *Session) SetFollowRedirect(follow bool) {
	if s.client != nil {
		s.client.SetFollowRedirect(follow)
	}
}

type NetworkClient struct {
	debugMode bool
}

func NewNetworkClient(debugMode bool) *NetworkClient { return &NetworkClient{debugMode: debugMode} }

//nolint:gochecknoglobals // Read-only list of browser profiles
var browserProfiles = []profiles.ClientProfile{
	profiles.Chrome_144, profiles.Chrome_146,
}

func pickProfile() profiles.ClientProfile {
	return browserProfiles[rand.Intn(len(browserProfiles))]
}

// injectProxy 统一处理网络代理挂载，如果代理初始化失败，返回 error
func injectProxy(opts []tls_client.HttpClientOption, proxyURI string, reqID string, debugMode bool) ([]tls_client.HttpClientOption, error) {
	if proxyURI == "" {
		return opts, nil
	}
	// 用户自定义的外部标准代理，直接使用 URL
	lowerProxyURI := strings.ToLower(proxyURI)
	if strings.HasPrefix(lowerProxyURI, "http://") ||
		strings.HasPrefix(lowerProxyURI, "https://") ||
		strings.HasPrefix(lowerProxyURI, "socks4://") ||
		strings.HasPrefix(lowerProxyURI, "socks4a://") ||
		strings.HasPrefix(lowerProxyURI, "socks5://") ||
		strings.HasPrefix(lowerProxyURI, "socks5h://") {
		return append(opts, tls_client.WithProxyUrl(proxyURI)), nil
	}

	// 订阅节点，获取并挂载内部 Dialer
	dialCtx, err := getOrStartProxyDialer(proxyURI, reqID, debugMode)
	if err != nil {
		return nil, fmt.Errorf("节点内部 Dialer 启动失败: %w", err)
	}

	opts = append(opts, tls_client.WithDialContext(dialCtx))
	return opts, nil
}

// CreateSession 创建一个新 Session：随机 Chrome 指纹 + 可选代理 + 独立 cookie jar。
func (c *NetworkClient) CreateSession(timeoutSec int, proxyURI string, reqID string) (*Session, error) {
	prof := pickProfile()
	log.Printf("[Transport] reqID: %s, Assigned TLS Profile: %s", reqID, prof.GetClientHelloStr())

	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(timeoutSec),
		tls_client.WithClientProfile(prof),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}

	// 使用 injectProxy 挂载代理，失败则直接熔断，坚决不走静默直连！
	var err error
	opts, err = injectProxy(opts, proxyURI, reqID, c.debugMode)
	if err != nil {
		return nil, err
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("创建 TLS 客户端: %w", err)
	}
	return &Session{client: client, ProxyURI: proxyURI}, nil
}
