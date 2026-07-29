package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"strings"
	"sync"

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

const (
	maxResponseReadPreallocateBytes     int64 = 4 << 20
	maxPooledResponseReadBufferCapacity       = 1 << 20
)

var responseReadBufferPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return new(bytes.Buffer) },
}

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
	data, readErr := readAllLimitWithHint(resp.Body, maxBytes, resp.ContentLength)
	if readErr != nil {
		return resp.StatusCode, data, fmt.Errorf("读取响应体: %w", readErr)
	}
	return resp.StatusCode, data, nil
}

// ReadAllLimit 读取 reader。maxBytes <= 0 表示不限；超限时返回限制内的前缀和
// ErrResponseBodyTooLarge，便于错误日志保留有界的上游详情。
func ReadAllLimit(reader io.Reader, maxBytes int64) ([]byte, error) {
	return readAllLimitWithHint(reader, maxBytes, -1)
}

func readAllLimitWithHint(reader io.Reader, maxBytes, sizeHint int64) ([]byte, error) {
	if maxBytes <= 0 {
		if sizeHint > 0 {
			return readAllWithCapacity(reader, min(sizeHint, maxResponseReadPreallocateBytes))
		}
		return io.ReadAll(reader)
	}
	if maxBytes == int64(^uint64(0)>>1) {
		if sizeHint > 0 {
			return readAllWithCapacity(reader, min(sizeHint, maxResponseReadPreallocateBytes))
		}
		return io.ReadAll(reader)
	}
	limited := io.LimitReader(reader, maxBytes+1)
	var data []byte
	var err error
	if sizeHint > 0 {
		capacity := min(sizeHint, maxBytes+1, maxResponseReadPreallocateBytes)
		data, err = readAllWithCapacity(limited, capacity)
	} else {
		data, err = readAllWithPooledBuffer(limited)
	}
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], fmt.Errorf("%w: limit %d bytes", ErrResponseBodyTooLarge, maxBytes)
	}
	return data, nil
}

// readAllWithPooledBuffer amortizes the geometric growth needed when an HTTP
// response omits Content-Length. Returned bytes are detached before a buffer is
// reused; unusually large backing arrays are transferred to the caller and not
// retained by the pool.
func readAllWithPooledBuffer(reader io.Reader) ([]byte, error) {
	buffer := responseReadBufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	if _, err := buffer.ReadFrom(reader); err != nil {
		releaseResponseReadBuffer(buffer)
		return nil, err
	}

	view := buffer.Bytes()
	if cap(view) > maxPooledResponseReadBufferCapacity {
		// The buffer object becomes unreachable, while its backing array remains
		// owned by the returned slice. Avoid a second large full-body copy.
		return view, nil
	}
	data := bytes.Clone(view)
	releaseResponseReadBuffer(buffer)
	return data, nil
}

func releaseResponseReadBuffer(buffer *bytes.Buffer) {
	if buffer.Cap() > maxPooledResponseReadBufferCapacity {
		return
	}
	buffer.Reset()
	responseReadBufferPool.Put(buffer)
}

func readAllWithCapacity(reader io.Reader, capacity int64) ([]byte, error) {
	var buffer bytes.Buffer
	if capacity > 0 {
		// bytes.Buffer.ReadFrom reserves bytes.MinRead before each read. Include
		// that spare capacity so an exact Content-Length does not trigger a
		// second allocation solely for the final EOF probe.
		buffer.Grow(int(capacity) + bytes.MinRead)
	}
	if _, err := buffer.ReadFrom(reader); err != nil {
		return buffer.Bytes(), err
	}
	return buffer.Bytes(), nil
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
