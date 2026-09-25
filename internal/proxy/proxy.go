// Package proxy is the forwarder that lets microVMs use a corporate HTTP proxy
// without ever seeing its address or password. It listens on the bridge
// address (for microVMs) and on 127.0.0.1 (for host services), and passes
// CONNECT tunnels and plain HTTP requests to the upstream proxy, adding the
// upstream's credentials. Destinations the host's firewall keeps from
// microVMs (the host, other microVMs, cloud metadata, network.egress_deny)
// are refused here as well, since proxied traffic leaves from the host.
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	if u.Hostname() == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return Upstream{}, errors.New("expected http://[user:password@]host:port")
	}
	up := Upstream{Addr: u.Host}
	if u.User != nil {
		pass, _ := u.User.Password()
		up.Auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pass))
	}
	return up, nil
}

// LoadUpstream reads the upstream URL from a root-only file: a regular file
// (not a symlink) owned by this process's user and unreadable by others.
func LoadUpstream(file string) (Upstream, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return Upstream{}, fmt.Errorf("upstream proxy: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Upstream{}, fmt.Errorf("upstream proxy: %s is not a regular file", file)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return Upstream{}, fmt.Errorf("upstream proxy: %s must be owned by uid %d", file, os.Geteuid())
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

// Policy decides which destinations may be reached through the forwarder.
type Policy struct {
	// Bridge is the microVM network; other microVMs are never a destination.
	Bridge *net.IPNet
	// Deny holds further networks jobs must not reach (network.egress_deny).
	Deny []*net.IPNet
	// ConnectPorts are the ports CONNECT tunnels may go to.
	ConnectPorts []int
	// Resolve looks up a host name; nil uses the system resolver. A name that
	// does not resolve here is passed on: the upstream resolves it itself.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// LocalAddrs are the host's own addresses; nil lists the interfaces.
	LocalAddrs func() []net.IP
}

// Check returns why host:port may not be reached, or "" when it may.
func (p *Policy) Check(ctx context.Context, host string, port int, connect bool) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "no host"
	}
	if connect && !containsInt(p.ConnectPorts, port) {
		return fmt.Sprintf("CONNECT to port %d is not allowed (proxy.connect_ports)", port)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "localhost"
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		if strings.ContainsAny(host, ":%[]") {
			return "not a host name or IP address"
		}
		// 2130706433, 0x7f000001, 127.1: some resolvers read these as addresses.
		labels := strings.Split(host, ".")
		if last := labels[len(labels)-1]; isNumeric(last) {
			return "IP address in a non-standard form"
		}
		resolve := p.Resolve
		if resolve == nil {
			resolve = func(ctx context.Context, h string) ([]net.IP, error) {
				return net.DefaultResolver.LookupIP(ctx, "ip", h)
			}
		}
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ips, _ = resolve(rctx, host)
		cancel()
	}
	var local []net.IP
	if p.LocalAddrs != nil {
		local = p.LocalAddrs()
	} else {
		local = interfaceAddrs()
	}
	for _, ip := range ips {
		if why := p.deniedIP(ip, local); why != "" {
			return why
		}
	}
	return ""
}

func (p *Policy) deniedIP(ip net.IP, local []net.IP) string {
	switch {
	case ip.IsLoopback():
		return "loopback address"
	case ip.IsUnspecified():
		return "unspecified address"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local address (cloud metadata)"
	case ip.IsMulticast():
		return "multicast address"
	case p.Bridge != nil && p.Bridge.Contains(ip):
		return "the microVM network"
	}
	for _, l := range local {
		if l.Equal(ip) {
			return "an address of this host"
		}
	}
	for _, n := range p.Deny {
		if n.Contains(ip) {
			return "network.egress_deny " + n.String()
		}
	}
	return ""
}

func isNumeric(s string) bool {
	if strings.HasPrefix(s, "0x") {
		s = s[2:]
		return s != "" && strings.Trim(s, "0123456789abcdef") == ""
	}
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func interfaceAddrs() []net.IP {
	addrs, _ := net.InterfaceAddrs()
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out
}

// SelfLoop reports whether upstream (host:port) is the forwarder itself: a
// loopback, unspecified or local address on the port the forwarder listens on.
func SelfLoop(upstream, listenPort string, local []net.IP) bool {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil || port != listenPort {
		return false
	}
	if h := strings.TrimSuffix(strings.ToLower(host), "."); h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	for _, l := range local {
		if l.Equal(ip) {
			return true
		}
	}
	return false
}

// Limits bound what clients can hold open at once.
type Limits struct {
	// Zero means no limit for every field.
	PerClient   int           // concurrent requests and tunnels per microVM address
	Loopback    int           // host services on 127.0.0.1: a budget of their own
	Total       int           // all microVMs together
	TunnelIdle  time.Duration // a tunnel without traffic this long is closed
	ServerIdle  time.Duration // idle keep-alive connections
	HeaderWait  time.Duration // the upstream's answer to a plain request
	RefusedWait time.Duration // after a 407, how long refused credentials are not retried
}

// DefaultLimits suit one host running tens of jobs.
var DefaultLimits = Limits{PerClient: 256, Loopback: 1024, Total: 4096, TunnelIdle: 15 * time.Minute,
	ServerIdle: 90 * time.Second, HeaderWait: 5 * time.Minute, RefusedWait: 5 * time.Minute}

// Forwarder passes requests to the upstream proxy.
type Forwarder struct {
	// Upstream is read for every connection, so a new password or proxy address
	// applies without a restart.
	Upstream func() (Upstream, error)
	// Allowed reports whether a client address may use the forwarder.
	Allowed func(ip net.IP) bool
	// Policy decides the destinations; nil allows every destination.
	Policy *Policy
	Limits Limits
	Log    *slog.Logger
	// DialTimeout bounds connecting to the upstream and its answer to CONNECT.
	DialTimeout time.Duration

	transportOnce sync.Once
	transport     *http.Transport

	mu      sync.Mutex
	active  map[string]int
	total   int
	refused struct {
		key     string // upstream address and credentials the upstream refused
		at      time.Time
		probing bool // after RefusedWait, one request tries them again
	}
}

// hop-by-hop headers are not forwarded (RFC 9110 7.6.1), nor the client's own
// proxy credentials: the upstream only ever sees the forwarder's.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func removeHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

func (f *Forwarder) acquire(client string, loopback bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil {
		f.active = map[string]int{}
	}
	// Host services (containerd, gitlab-runner) have their own budget: microVMs
	// cannot take it, however many addresses a job gives its VM.
	if loopback {
		if f.Limits.Loopback > 0 && f.active[client] >= f.Limits.Loopback {
			return false
		}
		f.active[client]++
		return true
	}
	if (f.Limits.Total > 0 && f.total >= f.Limits.Total) || (f.Limits.PerClient > 0 && f.active[client] >= f.Limits.PerClient) {
		return false
	}
	f.active[client]++
	f.total++
	return true
}

func (f *Forwarder) release(client string, loopback bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active[client]--; f.active[client] <= 0 {
		delete(f.active, client)
	}
	if !loopback {
		f.total--
	}
}

// knownRefused reports whether these credentials must not be sent: the
// upstream refused them within Limits.RefusedWait, or after it one request is
// already trying them again (probe). A changed password must not lock the
// corporate account through a burst of failed logins. The caller of a probe
// calls endProbe when it is done.
func (f *Forwarder) knownRefused(up Upstream) (refused, probe bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refused.key != up.Addr+"\x00"+up.Auth {
		return false, false
	}
	if time.Since(f.refused.at) < f.Limits.RefusedWait || f.refused.probing {
		return true, false
	}
	f.refused.probing = true
	return false, true
}

func (f *Forwarder) endProbe() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused.probing = false
}

func (f *Forwarder) setRefused(up Upstream, refused bool) {
	key := up.Addr + "\x00" + up.Auth
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case refused && f.refused.key != key:
		f.refused.key, f.refused.at, f.refused.probing = key, time.Now(), false
		f.Log.Error("upstream proxy refused the credentials; not retrying them for a while (fix the upstream file)",
			"upstream", up.Addr, "with_credentials", up.Auth != "", "retry_after", f.Limits.RefusedWait.String())
	case refused:
		f.refused.at, f.refused.probing = time.Now(), false
	case f.refused.key == key:
		f.refused.key = ""
	}
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil || !f.Allowed(ip) {
		http.Error(w, "firerunner proxy: not allowed from "+host, http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect && (!r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https")) {
		http.Error(w, "firerunner proxy: send absolute http(s):// requests or CONNECT host:port", http.StatusBadRequest)
		return
	}
	target := r.Host
	if r.Method != http.MethodConnect {
		target = r.URL.Host
	}
	dhost, dport, err := targetHostPort(target, r.Method == http.MethodConnect, r.URL.Scheme)
	if err != nil {
		http.Error(w, "firerunner proxy: "+err.Error(), http.StatusBadRequest)
		return
	}
	if f.Policy != nil {
		if why := f.Policy.Check(r.Context(), dhost, dport, r.Method == http.MethodConnect); why != "" {
			f.Log.Warn("proxy request refused", "client", host, "target", target, "reason", why)
			http.Error(w, "firerunner proxy: destination not allowed: "+why, http.StatusForbidden)
			return
		}
	}
	if !f.acquire(host, ip.IsLoopback()) {
		f.Log.Warn("proxy request refused: too many connections", "client", host)
		http.Error(w, "firerunner proxy: too many connections from this client", http.StatusServiceUnavailable)
		return
	}
	defer f.release(host, ip.IsLoopback())
	up, err := f.Upstream()
	if err != nil {
		f.Log.Error("proxy request refused: no upstream", "err", err)
		http.Error(w, "firerunner proxy: upstream proxy not configured (see journalctl -u firerunner-proxy)", http.StatusBadGateway)
		return
	}
	refused, probe := f.knownRefused(up)
	if refused {
		http.Error(w, "firerunner proxy: the upstream proxy refused this host's credentials", http.StatusBadGateway)
		return
	}
	if probe {
		defer f.endProbe()
	}
	var status int
	var bytes int64
	switch {
	case r.Method == http.MethodConnect:
		status, bytes = f.tunnel(w, r, up, target)
	case r.URL.Scheme == "https":
		// Some clients (busybox wget) ask the proxy for https:// URLs instead of
		// opening a CONNECT tunnel; corporate proxies fetch those themselves.
		status, bytes = f.passThrough(w, r, up)
	default:
		status, bytes = f.forward(w, r, up)
	}
	f.Log.Info("proxied", "client", host, "method", r.Method, "target", target, "status", status,
		"bytes", bytes, "seconds", time.Since(start).Round(time.Millisecond).Seconds())
}

// targetHostPort splits a CONNECT target (host:port) or an absolute URL's
// host, with the scheme's default port.
func targetHostPort(target string, connect bool, scheme string) (string, int, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		if connect {
			return "", 0, errors.New("CONNECT needs host:port")
		}
		host, port = target, "80"
		if scheme == "https" {
			port = "443"
		}
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", 0, errors.New("bad port")
	}
	return host, p, nil
}

// tunnel opens CONNECT through the upstream and splices the two connections.
func (f *Forwarder) tunnel(w http.ResponseWriter, _ *http.Request, up Upstream, target string) (int, int64) {
	upConn, err := net.DialTimeout("tcp", up.Addr, f.DialTimeout)
	if err != nil {
		f.Log.Warn("upstream proxy unreachable", "upstream", up.Addr, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return http.StatusBadGateway, 0
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
		return http.StatusBadGateway, 0
	}
	if resp.StatusCode != http.StatusOK {
		_ = upConn.Close()
		return f.upstreamRefused(w, up, target, resp.StatusCode), 0
	}
	f.setRefused(up, false)
	_ = upConn.SetDeadline(time.Time{})
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upConn.Close()
		http.Error(w, "firerunner proxy: cannot tunnel", http.StatusInternalServerError)
		return http.StatusInternalServerError, 0
	}
	client, cbuf, err := hj.Hijack()
	if err != nil {
		_ = upConn.Close()
		return http.StatusInternalServerError, 0
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upConn.Close()
		return http.StatusOK, 0
	}
	return http.StatusOK, splice(client, cbuf.Reader, upConn, br, f.Limits.TunnelIdle)
}

// splice copies both ways until either side closes or, with idle > 0, no byte
// has moved for idle; bytes already buffered on either side are included. It
// returns the bytes copied.
func splice(client net.Conn, clientBuf io.Reader, upstream net.Conn, upstreamBuf io.Reader, idle time.Duration) int64 {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var total atomic.Int64
	done := make(chan struct{}, 2)
	cp := func(dst net.Conn, src io.Reader) {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				total.Add(int64(n))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(upstream, clientBuf)
	go cp(client, upstreamBuf)
	stop := make(chan struct{})
	if idle > 0 {
		go func() {
			t := time.NewTicker(idle / 4)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					if time.Since(time.Unix(0, last.Load())) > idle {
						_ = client.Close()
						_ = upstream.Close()
						return
					}
				}
			}
		}()
	}
	<-done
	<-done
	close(stop)
	_ = client.Close()
	_ = upstream.Close()
	return total.Load()
}

// forward sends a plain http:// request through the upstream proxy.
func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request, up Upstream) (int, int64) {
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
			ResponseHeaderTimeout: f.Limits.HeaderWait,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
		}
	})
	// Not the request's context: a client that half-closes (busybox) cancels it.
	// The answer is abandoned instead when no byte comes for HeaderWait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := r.Clone(ctx)
	out.RequestURI = ""
	removeHopHeaders(out.Header)
	if up.Auth != "" {
		out.Header.Set("Proxy-Authorization", up.Auth)
	}
	resp, err := f.transport.RoundTrip(out)
	if err != nil {
		f.Log.Warn("upstream proxy request failed", "upstream", up.Addr, "url", r.URL.Host, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return http.StatusBadGateway, 0
	}
	defer resp.Body.Close()
	resp.Body = idleBody(resp.Body, f.Limits.HeaderWait, cancel)
	return f.relay(w, resp, up, r.URL.Host)
}

// idleBody gives up on a body that sends nothing for idle (0: never), by
// calling stop, which ends the request.
func idleBody(body io.ReadCloser, idle time.Duration, stop func()) io.ReadCloser {
	if idle <= 0 {
		return body
	}
	return &idleReader{ReadCloser: body, t: time.AfterFunc(idle, stop), idle: idle}
}

type idleReader struct {
	io.ReadCloser
	t    *time.Timer
	idle time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	r.t.Reset(r.idle)
	n, err := r.ReadCloser.Read(p)
	r.t.Reset(r.idle)
	return n, err
}

func (r *idleReader) Close() error {
	r.t.Stop()
	return r.ReadCloser.Close()
}

// passThrough sends an absolute-form request to the upstream as it came,
// with the upstream's credentials, over a connection of its own. A stalled
// upstream is given up after HeaderWait. The request's context is not used:
// clients like busybox wget half-close after the request, which cancels it.
func (f *Forwarder) passThrough(w http.ResponseWriter, r *http.Request, up Upstream) (int, int64) {
	conn, err := net.DialTimeout("tcp", up.Addr, f.DialTimeout)
	if err != nil {
		f.Log.Warn("upstream proxy unreachable", "upstream", up.Addr, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return http.StatusBadGateway, 0
	}
	defer conn.Close()
	out := r.Clone(context.Background())
	removeHopHeaders(out.Header)
	if up.Auth != "" {
		out.Header.Set("Proxy-Authorization", up.Auth)
	}
	out.Close = true
	_ = conn.SetDeadline(time.Now().Add(f.DialTimeout))
	if err := out.WriteProxy(conn); err != nil {
		http.Error(w, "firerunner proxy: upstream proxy unreachable", http.StatusBadGateway)
		return http.StatusBadGateway, 0
	}
	if f.Limits.HeaderWait > 0 {
		_ = conn.SetDeadline(time.Now().Add(f.Limits.HeaderWait))
	} else {
		_ = conn.SetDeadline(time.Time{})
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), out)
	if err != nil {
		f.Log.Warn("upstream proxy did not answer", "upstream", up.Addr, "url", r.URL.Host, "err", err)
		http.Error(w, "firerunner proxy: upstream proxy did not answer", http.StatusBadGateway)
		return http.StatusBadGateway, 0
	}
	defer resp.Body.Close()
	_ = conn.SetDeadline(time.Time{})
	resp.Body = idleBody(resp.Body, f.Limits.HeaderWait, func() { _ = conn.Close() })
	return f.relay(w, resp, up, r.URL.Host)
}

// relay passes the upstream's answer to the client.
func (f *Forwarder) relay(w http.ResponseWriter, resp *http.Response, up Upstream, target string) (int, int64) {
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return f.upstreamRefused(w, up, target, resp.StatusCode), 0
	}
	f.setRefused(up, false)
	removeHopHeaders(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	return resp.StatusCode, n
}

// upstreamRefused answers a request the upstream turned down. A 407 is not
// passed on: the client has no credentials to offer, and the fix is on the host.
func (f *Forwarder) upstreamRefused(w http.ResponseWriter, up Upstream, target string, code int) int {
	if code == http.StatusProxyAuthRequired {
		f.setRefused(up, true)
		http.Error(w, "firerunner proxy: the upstream proxy refused this host's credentials", http.StatusBadGateway)
		return http.StatusBadGateway
	}
	f.Log.Warn("upstream proxy refused", "upstream", up.Addr, "target", target, "status", code)
	http.Error(w, fmt.Sprintf("firerunner proxy: upstream proxy answered %d", code), http.StatusBadGateway)
	return http.StatusBadGateway
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
		s := &http.Server{Handler: f, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: f.Limits.ServerIdle}
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
