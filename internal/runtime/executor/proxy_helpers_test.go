package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestNewProxyAwareHTTPClientWrapsTimingForProxyTransport(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ProxyURL:                 "http://127.0.0.1:8080",
			RequestLog:               true,
			RequestLogUpstreamTiming: true,
		},
	}

	client := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	if client == nil {
		t.Fatal("expected client")
	}
	if _, ok := client.Transport.(*timingRoundTripper); !ok {
		t.Fatalf("expected timingRoundTripper, got %T", client.Transport)
	}
}

func TestUpstreamTimingTraceFormatIncludesPhases(t *testing.T) {
	start := time.Unix(100, 0)
	trace := &upstreamTimingTrace{
		start:          start,
		getConn:        start.Add(10 * time.Millisecond),
		gotConn:        start.Add(20 * time.Millisecond),
		dnsStart:       start.Add(11 * time.Millisecond),
		dnsDone:        start.Add(14 * time.Millisecond),
		connectStart:   start.Add(14 * time.Millisecond),
		connectDone:    start.Add(18 * time.Millisecond),
		tlsStart:       start.Add(18 * time.Millisecond),
		tlsDone:        start.Add(24 * time.Millisecond),
		wroteRequest:   start.Add(30 * time.Millisecond),
		firstByte:      start.Add(130 * time.Millisecond),
		end:            start.Add(150 * time.Millisecond),
		reused:         false,
		wasIdle:        false,
		connReusedSet:  true,
		addr:           "api.example.com:443",
		connectNetwork: "tcp",
		connectAddr:    "127.0.0.1:8080",
		dnsAddrs:       []string{"1.1.1.1"},
		statusCode:     200,
	}

	out := trace.format()
	for _, want := range []string{
		"round_trip_complete: 150ms",
		"ttfb: 130ms",
		"dns_lookup: 3ms",
		"tcp_connect: 4ms",
		"tls_handshake: 6ms",
		"wait_first_byte: 100ms",
		"get_conn_target: api.example.com:443",
		"connect_target: tcp://127.0.0.1:8080",
		"dns_result: 1.1.1.1",
		"status: 200",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}
