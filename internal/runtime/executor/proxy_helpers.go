package executor

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		disableKeepAlive := cfg != nil && cfg.ProxyDisableKeepAlive
		transport := buildProxyTransport(proxyURL, disableKeepAlive)
		if transport != nil {
			httpClient.Transport = transport
		} else {
			// If proxy setup failed, log and fall through to context RoundTripper
			log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
		}
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if httpClient.Transport == nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			httpClient.Transport = rt
		}
	}

	if cfg != nil && cfg.RequestLog && cfg.RequestLogUpstreamTiming {
		httpClient.Transport = newTimingRoundTripper(ctx, cfg, httpClient.Transport)
	}

	return httpClient
}

type timingRoundTripper struct {
	ctx  context.Context
	cfg  *config.Config
	base http.RoundTripper
}

func newTimingRoundTripper(ctx context.Context, cfg *config.Config, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &timingRoundTripper{
		ctx:  ctx,
		cfg:  cfg,
		base: base,
	}
}

func (t *timingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return t.base.RoundTrip(req)
	}

	trace := newUpstreamTimingTrace()
	req = req.Clone(withTimingTrace(req.Context(), trace))

	resp, err := t.base.RoundTrip(req)
	trace.finish(resp, err)
	recordAPIResponseTiming(t.ctx, t.cfg, trace.format())
	return resp, err
}

type upstreamTimingTrace struct {
	mu sync.Mutex

	start time.Time
	end   time.Time

	getConn  time.Time
	gotConn  time.Time
	dnsStart time.Time
	dnsDone  time.Time

	connectStart time.Time
	connectDone  time.Time

	tlsStart time.Time
	tlsDone  time.Time

	wroteRequest   time.Time
	firstByte      time.Time
	reused         bool
	wasIdle        bool
	idleTime       time.Duration
	network        string
	addr           string
	connectNetwork string
	connectAddr    string
	errText        string
	dnsAddrs       []string
	statusCode     int
	connReusedSet  bool
}

func newUpstreamTimingTrace() *upstreamTimingTrace {
	return &upstreamTimingTrace{start: time.Now()}
}

func withTimingTrace(ctx context.Context, trace *upstreamTimingTrace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if trace == nil {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			trace.mu.Lock()
			if trace.getConn.IsZero() {
				trace.getConn = time.Now()
				trace.addr = hostPort
			}
			trace.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			trace.mu.Lock()
			if trace.gotConn.IsZero() {
				trace.gotConn = time.Now()
			}
			trace.reused = info.Reused
			trace.wasIdle = info.WasIdle
			trace.idleTime = info.IdleTime
			trace.connReusedSet = true
			trace.mu.Unlock()
		},
		DNSStart: func(info httptrace.DNSStartInfo) {
			trace.mu.Lock()
			if trace.dnsStart.IsZero() {
				trace.dnsStart = time.Now()
			}
			trace.mu.Unlock()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			trace.mu.Lock()
			if trace.dnsDone.IsZero() {
				trace.dnsDone = time.Now()
			}
			if len(info.Addrs) > 0 {
				trace.dnsAddrs = trace.dnsAddrs[:0]
				for _, addr := range info.Addrs {
					trace.dnsAddrs = append(trace.dnsAddrs, addr.String())
				}
			}
			if info.Err != nil && trace.errText == "" {
				trace.errText = info.Err.Error()
			}
			trace.mu.Unlock()
		},
		ConnectStart: func(network, addr string) {
			trace.mu.Lock()
			if trace.connectStart.IsZero() {
				trace.connectStart = time.Now()
				trace.connectNetwork = network
				trace.connectAddr = addr
			}
			trace.mu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			trace.mu.Lock()
			if trace.connectDone.IsZero() {
				trace.connectDone = time.Now()
			}
			trace.connectNetwork = network
			trace.connectAddr = addr
			if err != nil && trace.errText == "" {
				trace.errText = err.Error()
			}
			trace.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			trace.mu.Lock()
			if trace.tlsStart.IsZero() {
				trace.tlsStart = time.Now()
			}
			trace.mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			trace.mu.Lock()
			if trace.tlsDone.IsZero() {
				trace.tlsDone = time.Now()
			}
			if err != nil && trace.errText == "" {
				trace.errText = err.Error()
			}
			trace.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			trace.mu.Lock()
			if trace.wroteRequest.IsZero() {
				trace.wroteRequest = time.Now()
			}
			if info.Err != nil && trace.errText == "" {
				trace.errText = info.Err.Error()
			}
			trace.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			trace.mu.Lock()
			if trace.firstByte.IsZero() {
				trace.firstByte = time.Now()
			}
			trace.mu.Unlock()
		},
	})
}

func (t *upstreamTimingTrace) finish(resp *http.Response, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.end.IsZero() {
		t.end = time.Now()
	}
	if resp != nil {
		t.statusCode = resp.StatusCode
	}
	if err != nil && t.errText == "" {
		t.errText = err.Error()
	}
}

func (t *upstreamTimingTrace) format() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var lines []string
	lines = append(lines, fmt.Sprintf("trace_start: %s", t.start.Format(time.RFC3339Nano)))
	if !t.end.IsZero() {
		lines = append(lines, fmt.Sprintf("round_trip_complete: %s", durationString(t.end.Sub(t.start))))
	}
	if !t.firstByte.IsZero() {
		lines = append(lines, fmt.Sprintf("ttfb: %s", durationString(t.firstByte.Sub(t.start))))
	}
	if !t.getConn.IsZero() && !t.gotConn.IsZero() {
		lines = append(lines, fmt.Sprintf("get_conn_to_got_conn: %s", durationString(t.gotConn.Sub(t.getConn))))
	}
	if !t.dnsStart.IsZero() && !t.dnsDone.IsZero() {
		lines = append(lines, fmt.Sprintf("dns_lookup: %s", durationString(t.dnsDone.Sub(t.dnsStart))))
	}
	if !t.connectStart.IsZero() && !t.connectDone.IsZero() {
		lines = append(lines, fmt.Sprintf("tcp_connect: %s", durationString(t.connectDone.Sub(t.connectStart))))
	}
	if !t.tlsStart.IsZero() && !t.tlsDone.IsZero() {
		lines = append(lines, fmt.Sprintf("tls_handshake: %s", durationString(t.tlsDone.Sub(t.tlsStart))))
	}
	if !t.gotConn.IsZero() && !t.wroteRequest.IsZero() {
		lines = append(lines, fmt.Sprintf("write_request: %s", durationString(t.wroteRequest.Sub(t.gotConn))))
	}
	if !t.wroteRequest.IsZero() && !t.firstByte.IsZero() {
		lines = append(lines, fmt.Sprintf("wait_first_byte: %s", durationString(t.firstByte.Sub(t.wroteRequest))))
	}
	if !t.getConn.IsZero() {
		lines = append(lines, fmt.Sprintf("get_conn_at: %s", durationString(t.getConn.Sub(t.start))))
	}
	if !t.gotConn.IsZero() {
		lines = append(lines, fmt.Sprintf("got_conn_at: %s", durationString(t.gotConn.Sub(t.start))))
	}
	if !t.wroteRequest.IsZero() {
		lines = append(lines, fmt.Sprintf("wrote_request_at: %s", durationString(t.wroteRequest.Sub(t.start))))
	}
	if !t.firstByte.IsZero() {
		lines = append(lines, fmt.Sprintf("first_byte_at: %s", durationString(t.firstByte.Sub(t.start))))
	}
	if t.connReusedSet {
		lines = append(lines, fmt.Sprintf("conn_reused: %t", t.reused))
		lines = append(lines, fmt.Sprintf("conn_was_idle: %t", t.wasIdle))
		if t.wasIdle {
			lines = append(lines, fmt.Sprintf("conn_idle_for: %s", durationString(t.idleTime)))
		}
	}
	if t.addr != "" {
		lines = append(lines, fmt.Sprintf("get_conn_target: %s", t.addr))
	}
	if t.connectAddr != "" {
		target := t.connectAddr
		if t.connectNetwork != "" {
			target = t.connectNetwork + "://" + target
		}
		lines = append(lines, fmt.Sprintf("connect_target: %s", target))
	}
	if len(t.dnsAddrs) > 0 {
		lines = append(lines, fmt.Sprintf("dns_result: %s", strings.Join(t.dnsAddrs, ", ")))
	}
	if t.statusCode > 0 {
		lines = append(lines, fmt.Sprintf("status: %d", t.statusCode))
	}
	if t.errText != "" {
		lines = append(lines, fmt.Sprintf("trace_error: %s", t.errText))
	}
	return strings.Join(lines, "\n")
}

func durationString(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Microsecond).String()
}

func logProxyDiagnostics(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, provider string) {
	if cfg == nil || !cfg.ProxyDiagnostics {
		return
	}

	proxyURL, source := resolveProxyURL(cfg, auth, ctx)
	disableKeepAlive := cfg.ProxyDisableKeepAlive
	logger := logWithRequestID(ctx)
	if proxyURL == "" {
		if source == "roundtripper" {
			logger.Debugf("proxy diag: provider=%s proxy=roundtripper disable_keepalive=%t", provider, disableKeepAlive)
		} else {
			logger.Debugf("proxy diag: provider=%s proxy=direct disable_keepalive=%t", provider, disableKeepAlive)
		}
	} else {
		logger.Debugf("proxy diag: provider=%s proxy=%s source=%s disable_keepalive=%t", provider, redactProxyURL(proxyURL), source, disableKeepAlive)
	}

	diagURL := strings.TrimSpace(cfg.ProxyDiagnosticsURL)
	if diagURL == "" {
		return
	}

	client := newProxyAwareHTTPClient(ctx, cfg, auth, 2*time.Second)
	if client == nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, diagURL, nil)
	if err != nil {
		logger.Debugf("proxy diag: provider=%s exit_ip_error=%v", provider, err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		logger.Debugf("proxy diag: provider=%s exit_ip_error=%v", provider, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	exitIP := strings.TrimSpace(string(body))
	if exitIP == "" {
		exitIP = "<empty>"
	}
	logger.Debugf("proxy diag: provider=%s exit_ip=%s status=%d", provider, exitIP, resp.StatusCode)
}

func resolveProxyURL(cfg *config.Config, auth *cliproxyauth.Auth, ctx context.Context) (string, string) {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL, "auth"
		}
	}
	if cfg != nil {
		if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
			return proxyURL, "config"
		}
	}
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			return "", "roundtripper"
		}
	}
	return "", "direct"
}

func redactProxyURL(proxyURL string) string {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed == nil {
		return proxyURL
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		if username == "" {
			parsed.User = url.UserPassword("user", "redacted")
		} else {
			parsed.User = url.UserPassword(username, "redacted")
		}
	}
	return parsed.String()
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//   - disableKeepAlive: When true, disables keep-alive so each request uses a new TCP connection.
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string, disableKeepAlive bool) *http.Transport {
	if proxyURL == "" {
		return nil
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("parse proxy URL failed: %v", errParse)
		return nil
	}

	var transport *http.Transport

	// Handle different proxy schemes
	if parsedURL.Scheme == "socks5" {
		// Configure SOCKS5 proxy with optional authentication
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
			return nil
		}
		// Set up a custom transport using the SOCKS5 dialer
		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}
	} else if parsedURL.Scheme == "http" || parsedURL.Scheme == "https" {
		// Configure HTTP or HTTPS proxy
		transport = &http.Transport{Proxy: http.ProxyURL(parsedURL)}
	} else {
		log.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
		return nil
	}

	if disableKeepAlive && transport != nil {
		transport.DisableKeepAlives = true
		transport.MaxIdleConns = 0
		transport.MaxIdleConnsPerHost = 0
	}
	return transport
}
