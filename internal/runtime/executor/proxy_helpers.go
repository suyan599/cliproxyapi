package executor

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	return httpClient
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
