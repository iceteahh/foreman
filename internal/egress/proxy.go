// Package egress is the worker network allowlist (design §4.1 "network egress
// restricted to an allowlist", plan Step 14). Workers run on an `--internal`
// docker network whose only way out is this HTTP CONNECT proxy; it tunnels
// TLS to allowlisted hosts and answers 403 to everything else. It never
// decrypts traffic and keeps no request bodies: the audit trail is a log line
// per decision.
package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAllow is what a code_fix worker needs: the API, GitHub, and the Go
// and npm registries. Operators extend it per deployment.
var DefaultAllow = []string{
	"api.anthropic.com",
	"github.com", "*.github.com", "*.githubusercontent.com",
	"proxy.golang.org", "sum.golang.org", "storage.googleapis.com",
	"registry.npmjs.org",
}

// Allowlist matches host names: exact entries and "*.suffix" wildcards
// (which also match the bare suffix's subdomains only, never the suffix itself).
type Allowlist struct {
	exact  map[string]bool
	suffix []string
	ports  map[int]bool
}

// NewAllowlist builds a matcher. ports restricts destination ports (default 443 and 80).
func NewAllowlist(hosts []string, ports ...int) *Allowlist {
	a := &Allowlist{exact: map[string]bool{}, ports: map[int]bool{}}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, "*.") {
			a.suffix = append(a.suffix, h[1:]) // ".github.com"
		} else {
			a.exact[h] = true
		}
	}
	if len(ports) == 0 {
		ports = []int{443, 80}
	}
	for _, p := range ports {
		a.ports[p] = true
	}
	return a
}

// Allows reports whether host:port may be reached.
func (a *Allowlist) Allows(host string, port int) bool {
	if a == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !a.ports[port] {
		return false
	}
	if a.exact[host] {
		return true
	}
	for _, s := range a.suffix {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
}

// Proxy is the CONNECT server.
type Proxy struct {
	Allow *Allowlist
	// Dial opens the upstream connection (default net.Dialer with 10s timeout).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// IdleTimeout closes tunnels idle in both directions (default 10 minutes).
	IdleTimeout time.Duration
	Logger      *slog.Logger

	mu      sync.Mutex
	allowed int
	denied  int
}

// Stats returns the decision counters.
func (p *Proxy) Stats() (allowed, denied int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allowed, p.denied
}

func (p *Proxy) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

func (p *Proxy) dial(ctx context.Context, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, "tcp", addr)
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// ServeHTTP handles CONNECT (tunnel) and plain HTTP (forwarded only to
// allowlisted hosts on port 80; useful for `go env GOPROXY` style probes).
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL == nil || !r.URL.IsAbs() {
		http.Error(w, "egress proxy: absolute-form request required", http.StatusBadRequest)
		return
	}
	host, port := splitHostPort(r.URL.Host, 80)
	if !p.Allow.Allows(host, port) {
		p.deny(host, port, r.RemoteAddr)
		http.Error(w, fmt.Sprintf("egress proxy: %s:%d is not allowlisted", host, port), http.StatusForbidden)
		return
	}
	p.permit(host, port, r.RemoteAddr)
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Connection")
	resp, err := (&http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return p.dial(ctx, addr) }, Proxy: nil}).RoundTrip(out)
	if err != nil {
		http.Error(w, "egress proxy: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port := splitHostPort(r.Host, 443)
	if !p.Allow.Allows(host, port) {
		p.deny(host, port, r.RemoteAddr)
		http.Error(w, fmt.Sprintf("egress proxy: %s:%d is not allowlisted", host, port), http.StatusForbidden)
		return
	}
	upstream, err := p.dial(r.Context(), net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		http.Error(w, "egress proxy: dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "egress proxy: hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	p.permit(host, port, r.RemoteAddr)
	_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = buf.Flush()
	p.tunnel(client, buf.Reader, upstream)
}

// tunnel copies both ways until either side closes or the idle timeout hits.
func (p *Proxy) tunnel(client net.Conn, clientBuf *bufio.Reader, upstream net.Conn) {
	idle := p.IdleTimeout
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	var wg sync.WaitGroup
	wg.Add(2)
	// The deadline has to sit on the connection being *read*: that is the call
	// that blocks. Setting it on the write side leaves an idle tunnel parked
	// forever, holding a worker's slot and a file descriptor with nothing
	// flowing through it.
	cp := func(dst net.Conn, src io.Reader, srcConn net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			_ = srcConn.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(buf)
			if n > 0 {
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	// clientBuf wraps client, so the client connection carries its deadline.
	go cp(upstream, clientBuf, client)
	go cp(client, upstream, upstream)
	wg.Wait()
	_ = client.Close()
	_ = upstream.Close()
}

func (p *Proxy) permit(host string, port int, from string) {
	p.mu.Lock()
	p.allowed++
	p.mu.Unlock()
	p.log().Info("egress allowed", "host", host, "port", port, "from", from)
}

func (p *Proxy) deny(host string, port int, from string) {
	p.mu.Lock()
	p.denied++
	p.mu.Unlock()
	p.log().Warn("egress denied", "host", host, "port", port, "from", from)
}

func splitHostPort(hostport string, def int) (string, int) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.TrimSuffix(hostport, "."), def
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return host, def
	}
	return host, port
}

// Serve listens on addr until ctx ends.
func Serve(ctx context.Context, addr string, p *Proxy) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		p.log().Info("egress proxy listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shCtx)
	case err := <-errc:
		return err
	}
}
