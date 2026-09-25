// Package proxy is the forwarder that lets microVMs use a corporate HTTP proxy
// without ever seeing its address or password. It listens on the bridge
// address (for microVMs) and on 127.0.0.1 (for host services), and passes
// CONNECT tunnels and plain HTTP requests to the upstream proxy, adding the
// upstream's credentials.
package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Upstream is the corporate proxy.
type Upstream struct {
	Addr string // host:port
	Auth string // Proxy-Authorization value, empty without credentials
}

// ParseUpstream reads http://[user:password@]host:port. The user and password
// are URL-decoded, so a password with special characters is written %-encoded.
func ParseUpstream(raw string) (Upstream, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return Upstream{}, errors.New("not a URL: expected http://[user:password@]host:port")
	}
	if u.Scheme != "http" {
		return Upstream{}, fmt.Errorf("scheme %q: only http:// proxies are supported (traffic to https:// sites is still end-to-end TLS)", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return Upstream{}, errors.New("expected http://[user:password@]host:port")
	}
	up := Upstream{Addr: u.Host}
	if u.User != nil {
		pass, _ := u.User.Password()
		up.Auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pass))
	}
	return up, nil
}

// LoadUpstream reads the upstream URL from a root-only file.
func LoadUpstream(file string) (Upstream, error) {
	info, err := os.Stat(file)
	if err != nil {
		return Upstream{}, fmt.Errorf("upstream proxy: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Upstream{}, fmt.Errorf("upstream proxy: %s is readable by other users (it may hold a password): chmod 600 it", file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Upstream{}, fmt.Errorf("upstream proxy: %w", err)
	}
	up, err := ParseUpstream(string(data))
	if err != nil {
		return Upstream{}, fmt.Errorf("upstream proxy in %s: %w", file, err)
	}
	return up, nil
}

// Forwarder passes requests to the upstream proxy.
type Forwarder struct {
	// Upstream is read for every connection, so a new password or proxy address
	// applies without a restart.
	Upstream func() (Upstream, error)
	// Allowed reports whether a client address may use the forwarder.
	Allowed func(ip net.IP) bool
	Log     *slog.Logger
	// DialTimeout bounds connecting to the upstream and its answer to CONNECT.
	DialTimeout time.Duration

	transportOnce sync.Once
	transport     *http.Transport
}

// hop-by-hop headers are not forwarded (RFC 9110 7.6.1), nor the client's own
// proxy credentials: the upstream only ever sees the forwarder's.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !f.Allowed(ip) {
		http.Error(w, "firerunner proxy: not allowed from "+host, http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect && (!r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https")) {
		http.Error(w, "firerunner proxy: send absolute http(s):// requests or CONNECT host:port", http.StatusBadRequest)
		return
	}
	up, err := f.Upstream()
	if err != nil {
		f.Log.Error("proxy request refused: no upstream", "err", err)
		http.Error(w, "firerunner proxy: upstream proxy not configured (see journalctl -u firerunner-proxy)", http.StatusBadGateway)
		return
	}
	switch {
	case r.Method == http.MethodConnect:
		f.tunnel(w, r, up)
	case r.URL.Scheme == "https":
		// Some clients (busybox wget) ask the proxy for https:// URLs instead of
		// opening a CONNECT tunnel; corporate proxies fetch those themselves.
		f.passThrough(w, r, up)
	default:
		f.forward(w, r, up)
	}
}

// tunnel opens CONNECT through the upstream and splices the two connections.
func (f *Forwarder) tunnel(w http.ResponseWriter, r *http.Request, up Upstream) {
	target := r.Host
	if _, port, err := net.SplitHostPort(target); err != nil || port == "" {
		http.Error(w, "firerunner proxy: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	upConn, err := net.DialTimeout("tcp", up.Addr, f.DialTimeout)
	if err != nil {
		f.Log.Warn("upstream proxy unreachable", "upstream", up.Addr, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return
	}
	_ = upConn.SetDeadline(time.Now().Add(f.DialTimeout))
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if up.Auth != "" {
		req += "Proxy-Authorization: " + up.Auth + "\r\n"
	}
	br := bufio.NewReader(upConn)
	resp, err := func() (*http.Response, error) {
		if _, err := io.WriteString(upConn, req+"\r\n"); err != nil {
			return nil, err
		}
		return http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	}()
	if err != nil {
		_ = upConn.Close()
		f.Log.Warn("upstream proxy did not answer CONNECT", "upstream", up.Addr, "target", target, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy did not answer", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusOK {
		_ = upConn.Close()
		f.refused(w, up, target, resp.StatusCode)
		return
	}
	_ = upConn.SetDeadline(time.Time{})
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upConn.Close()
		http.Error(w, "firerunner proxy: cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, cbuf, err := hj.Hijack()
	if err != nil {
		_ = upConn.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upConn.Close()
		return
	}
	splice(client, cbuf.Reader, upConn, br)
}

// splice copies both ways until either side closes, including bytes already
// buffered on either side.
func splice(client net.Conn, clientBuf io.Reader, upstream net.Conn, upstreamBuf io.Reader) {
	done := make(chan struct{}, 2)
	cp := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(upstream, clientBuf)
	go cp(client, upstreamBuf)
	<-done
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

// forward sends a plain http:// request through the upstream proxy.
func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request, up Upstream) {
	f.transportOnce.Do(func() {
		f.transport = &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) {
				u, err := f.Upstream()
				if err != nil {
					return nil, err
				}
				return &url.URL{Scheme: "http", Host: u.Addr}, nil
			},
			DialContext:           (&net.Dialer{Timeout: f.DialTimeout}).DialContext,
			ResponseHeaderTimeout: 5 * time.Minute,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
		}
	})
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	if up.Auth != "" {
		out.Header.Set("Proxy-Authorization", up.Auth)
	}
	resp, err := f.transport.RoundTrip(out)
	if err != nil {
		f.Log.Warn("upstream proxy request failed", "upstream", up.Addr, "url", r.URL.Host, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		f.refused(w, up, r.URL.Host, resp.StatusCode)
		return
	}
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// passThrough sends an absolute-form request to the upstream as it came,
// with the upstream's credentials, over a connection of its own.
func (f *Forwarder) passThrough(w http.ResponseWriter, r *http.Request, up Upstream) {
	conn, err := net.DialTimeout("tcp", up.Addr, f.DialTimeout)
	if err != nil {
		f.Log.Warn("upstream proxy unreachable", "upstream", up.Addr, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return
	}
	defer conn.Close()
	out := r.Clone(r.Context())
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	if up.Auth != "" {
		out.Header.Set("Proxy-Authorization", up.Auth)
	}
	out.Close = true
	if err := out.WriteProxy(conn); err != nil {
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), out)
	if err != nil {
		f.Log.Warn("upstream proxy did not answer", "upstream", up.Addr, "url", r.URL.Host, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy did not answer", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		f.refused(w, up, r.URL.Host, resp.StatusCode)
		return
	}
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// refused answers a request the upstream turned down. A 407 is not passed on:
// the client has no credentials to offer, and the fix is on the host.
func (f *Forwarder) refused(w http.ResponseWriter, up Upstream, target string, code int) {
	if code == http.StatusProxyAuthRequired {
		f.Log.Error("upstream proxy refused the credentials", "upstream", up.Addr, "target", target,
			"with_credentials", up.Auth != "")
		http.Error(w, "firerunner proxy: the upstream proxy refused this host's credentials", http.StatusBadGateway)
		return
	}
	f.Log.Warn("upstream proxy refused", "upstream", up.Addr, "target", target, "status", code)
	http.Error(w, fmt.Sprintf("firerunner proxy: upstream proxy answered %d", code), http.StatusBadGateway)
}

// AllowNets returns an Allowed func for loopback and the given networks.
func AllowNets(nets ...*net.IPNet) func(net.IP) bool {
	return func(ip net.IP) bool {
		if ip.IsLoopback() {
			return true
		}
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
}

// Serve runs the forwarder on each address until ctx is done.
func Serve(ctx context.Context, f *Forwarder, addrs ...string) error {
	var servers []*http.Server
	errs := make(chan error, len(addrs))
	for _, addr := range addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			for _, s := range servers {
				_ = s.Close()
			}
			return err
		}
		s := &http.Server{Handler: f, ReadHeaderTimeout: 30 * time.Second}
		servers = append(servers, s)
		go func() { errs <- s.Serve(l) }()
		f.Log.Info("proxy forwarder listening", "addr", addr)
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			for _, s := range servers {
				_ = s.Close()
			}
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return nil
}
