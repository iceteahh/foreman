package egress

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

func TestAllowlist(t *testing.T) {
	a := NewAllowlist([]string{"api.anthropic.com", "*.github.com", " Registry.NPMJS.org "})
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"api.anthropic.com", 443, true},
		{"API.ANTHROPIC.COM.", 443, true},
		{"api.anthropic.com", 22, false},
		{"evil.api.anthropic.com", 443, false},
		{"github.com", 443, false}, // wildcard does not cover the bare suffix
		{"api.github.com", 443, true},
		{"deep.api.github.com", 80, true},
		{"notgithub.com", 443, false},
		{"registry.npmjs.org", 443, true},
		{"example.com", 443, false},
	}
	for _, c := range cases {
		if got := a.Allows(c.host, c.port); got != c.want {
			t.Errorf("Allows(%s,%d)=%v want %v", c.host, c.port, got, c.want)
		}
	}
	if (*Allowlist)(nil).Allows("api.anthropic.com", 443) {
		t.Error("nil allowlist must deny")
	}
}

// TestConnectTunnelsAllowedAndDeniesOthers runs a TLS origin, points the
// proxy's dialer at it for the allowlisted name, and drives an http.Client
// through the proxy with CONNECT.
func TestConnectTunnelsAllowedAndDeniesOthers(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from "+r.Host)
	}))
	defer origin.Close()
	originAddr := origin.Listener.Addr().String()

	p := &Proxy{Allow: NewAllowlist([]string{"allowed.test"}), Logger: quiet,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Every allowlisted name resolves to the fake origin.
			return (&net.Dialer{}).DialContext(ctx, network, originAddr)
		}}
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test origin
	}}
	resp, err := client.Get("https://allowed.test/x")
	if err != nil {
		t.Fatalf("allowed request through proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from allowed.test" {
		t.Errorf("status %d body %q", resp.StatusCode, body)
	}

	_, err = client.Get("https://example.com/")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("denied host should fail with 403 from the proxy, got %v", err)
	}
	if allowed, denied := p.Stats(); allowed != 1 || denied != 1 {
		t.Errorf("stats allowed=%d denied=%d", allowed, denied)
	}
}

func TestPlainHTTPForwardsAllowlistedOnly(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Origin", "yes")
		_, _ = io.WriteString(w, "plain "+r.URL.Path)
	}))
	defer origin.Close()
	p := &Proxy{Allow: NewAllowlist([]string{"plain.test"}), Logger: quiet,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
		}}
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get("http://plain.test/a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "plain /a" || resp.Header.Get("X-Origin") != "yes" {
		t.Errorf("status %d body %q hdr %q", resp.StatusCode, body, resp.Header.Get("X-Origin"))
	}
	resp, err = client.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied plain request status %d", resp.StatusCode)
	}
}

func TestServeStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, "127.0.0.1:0", &Proxy{Allow: NewAllowlist(nil), Logger: quiet}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
