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
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestParseUpstream(t *testing.T) {
	basic := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		in, addr, auth string
		bad            bool
	}{
		{in: "http://proxy.corp:3128", addr: "proxy.corp:3128"},
		{in: " http://proxy.corp:3128/\n", addr: "proxy.corp:3128"},
		{in: "http://bob:s3cret@10.0.0.5:8080", addr: "10.0.0.5:8080", auth: basic("bob:s3cret")},
		// A password with @ : / % is written %-encoded.
		{in: "http://DOMAIN%5Cbob:p%40ss%3Aw%2Fo%25rd@proxy:3128", addr: "proxy:3128", auth: basic(`DOMAIN\bob:p@ss:w/o%rd`)},
		{in: "https://proxy:3128", bad: true},
		{in: "socks5://proxy:1080", bad: true},
		{in: "http://proxy", bad: true}, // no port
		{in: "proxy:3128", bad: true},
		{in: "http://proxy:3128/path", bad: true},
		{in: "", bad: true},
	} {
		up, err := ParseUpstream(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseUpstream(%q) accepted", tc.in)
			}
			continue
		}
		if err != nil || up.Addr != tc.addr || up.Auth != tc.auth {
			t.Errorf("ParseUpstream(%q) = %+v, %v; want %s %q", tc.in, up, err, tc.addr, tc.auth)
		}
	}
}

func TestLoadUpstreamRefusesReadableFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "upstream")
	if err := os.WriteFile(file, []byte("http://u:p@proxy:3128\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUpstream(file); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable file with a password: %v", err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if up, err := LoadUpstream(file); err != nil || up.Addr != "proxy:3128" {
		t.Fatalf("0600 file: %+v, %v", up, err)
	}
	if _, err := LoadUpstream(file + ".missing"); err == nil {
		t.Fatal("missing file accepted")
	}
}

// fakeUpstream is a corporate proxy that requires user:pass, logs what it
// saw, and forwards CONNECT and plain HTTP like a real one.
type fakeUpstream struct {
	addr string
	mu   sync.Mutex
	seen []string // "CONNECT host:port auth" / "GET url auth"
	want string   // expected Proxy-Authorization; "" = none required
}

func startUpstream(t *testing.T, want string) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{want: want}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Proxy-Authorization")
		u.mu.Lock()
		u.seen = append(u.seen, r.Method+" "+r.Host+" "+auth)
		u.mu.Unlock()
		if u.want != "" && auth != u.want {
			w.Header().Set("Proxy-Authenticate", `Basic realm="corp"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if r.Method == http.MethodConnect {
			dst, err := net.Dial("tcp", r.Host)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
			c, buf, _ := w.(http.Hijacker).Hijack()
			splice(c, buf.Reader, dst, dst, 0)
			return
		}
		out := r.Clone(context.Background())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("X-Via", "corp")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	u.addr = srv.Listener.Addr().String()
	return u
}

func (u *fakeUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

// startForwarder runs a Forwarder whose upstream comes from a 0600 file.
func startForwarder(t *testing.T, upstreamURL string, allowed func(net.IP) bool) (addr, file string) {
	t.Helper()
	file = filepath.Join(t.TempDir(), "upstream")
	if err := os.WriteFile(file, []byte(upstreamURL), 0o600); err != nil {
		t.Fatal(err)
	}
	if allowed == nil {
		allowed = AllowNets()
	}
	f := &Forwarder{Upstream: func() (Upstream, error) { return LoadUpstream(file) }, Allowed: allowed,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DialTimeout: 2 * time.Second}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String(), file
}

func client(proxyAddr string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: proxyAddr})}}
}

func origin(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s, proxy-auth=%q", r.URL.Path, r.Header.Get("Proxy-Authorization"))
	}))
	t.Cleanup(s.Close)
	return s
}

// Plain HTTP and HTTPS (CONNECT) go through the upstream with its password;
// the client sends none, and a client's own Proxy-Authorization is not passed on.
func TestForwarderAddsUpstreamCredentials(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:p@ss"))
	up := startUpstream(t, want)
	fwd, _ := startForwarder(t, "http://bob:p%40ss@"+up.addr, nil)
	web := origin(t)
	tlsWeb := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secure") }))
	t.Cleanup(tlsWeb.Close)

	req, _ := http.NewRequest(http.MethodGet, web.URL+"/x", nil)
	req.Header.Set("Proxy-Authorization", "Basic amFjazpndWVzcw==") // the client's own guess
	resp, err := client(fwd).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `hello /x, proxy-auth=""` || resp.Header.Get("X-Via") != "corp" {
		t.Fatalf("http: %d %q via %q", resp.StatusCode, body, resp.Header.Get("X-Via"))
	}

	c := client(fwd)
	c.Transport.(*http.Transport).TLSClientConfig = tlsWeb.Client().Transport.(*http.Transport).TLSClientConfig
	resp, err = c.Get(tlsWeb.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secure" {
		t.Fatalf("https through CONNECT: %q", body)
	}
	for _, r := range up.requests() {
		if !strings.HasSuffix(r, want) {
			t.Fatalf("upstream saw a request without the host's credentials: %q", r)
		}
	}
	if n := len(up.requests()); n != 2 {
		t.Fatalf("upstream saw %d requests, want 2: %v", n, up.requests())
	}
}

// Wrong credentials, an unreachable upstream, a missing upstream file and
// requests that are not proxy requests all fail with a clear status, and the
// client never gets a 407 it could answer with its own credentials.
func TestForwarderFailures(t *testing.T) {
	up := startUpstream(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:right")))
	web := origin(t)

	fwd, file := startForwarder(t, "http://bob:wrong@"+up.addr, nil)
	for _, target := range []string{web.URL, "https://" + strings.TrimPrefix(web.URL, "http://")} {
		resp, err := client(fwd).Get(target)
		if err == nil {
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("%s with wrong credentials: %d, want 502", target, resp.StatusCode)
			}
			resp.Body.Close()
		} else if !strings.Contains(err.Error(), "Bad Gateway") {
			t.Fatalf("%s with wrong credentials: %v, want 502", target, err)
		}
	}

	// The password is fixed in the file: the next request uses it, no restart.
	if err := os.WriteFile(file, []byte("http://bob:right@"+up.addr), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := client(fwd).Get(web.URL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("after fixing the password: %v %v", resp, err)
	}
	resp.Body.Close()

	// Upstream down.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	if err := os.WriteFile(file, []byte("http://"+deadAddr), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, err := client(fwd).Get(web.URL); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream down: %v %v, want 502", resp, err)
	}

	// Upstream file unusable.
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if resp, err := client(fwd).Get(web.URL); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("world-readable upstream file: %v %v, want 502", resp, err)
	}

	// Not a proxy request.
	if resp, err := http.Get("http://" + fwd + "/"); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("origin-form request: %v %v, want 400", resp, err)
	}
}

// Only the bridge network and loopback may use the forwarder.
func TestForwarderRefusesOtherNetworks(t *testing.T) {
	up := startUpstream(t, "")
	_, bridge, _ := net.ParseCIDR("10.200.0.0/24")
	allowed := AllowNets(bridge)
	if !allowed(net.ParseIP("10.200.0.44")) || !allowed(net.ParseIP("127.0.0.1")) || allowed(net.ParseIP("192.168.1.9")) {
		t.Fatal("AllowNets")
	}
	fwd, _ := startForwarder(t, "http://"+up.addr, func(net.IP) bool { return false })
	resp, err := client(fwd).Get(origin(t).URL)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client outside the allowed networks: %v %v, want 403", resp, err)
	}
	if len(up.requests()) != 0 {
		t.Fatal("a refused client reached the upstream")
	}
}

// A CONNECT tunnel carries data both ways, including bytes the client sends
// right after the CONNECT line without waiting for the answer.
func TestTunnelCarriesPipelinedBytes(t *testing.T) {
	up := startUpstream(t, "")
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	fwd, _ := startForwarder(t, "http://"+up.addr, nil)
	c, err := net.Dial("tcp", fwd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nping", echo.Addr(), echo.Addr())
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q %v", buf, err)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{Upstream: func() (Upstream, error) { return Upstream{}, errors.New("none") }, Allowed: AllowNets(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DialTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, f, "127.0.0.1:0", "127.0.0.1:0") }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
	// A port already in use is reported, not ignored.
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	if err := Serve(context.Background(), f, busy.Addr().String()); err == nil {
		t.Fatal("Serve on a busy port returned nil")
	}
}

// busybox wget asks the proxy for https:// URLs with GET instead of CONNECT;
// the request goes to the upstream as it came, with the host's credentials.
func TestForwarderPassesAbsoluteHTTPS(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:pw"))
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.String() + " " + r.Header.Get("Proxy-Authorization")
		if r.Header.Get("Proxy-Authorization") != want {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		_, _ = io.WriteString(w, "fetched by the corporate proxy")
	}))
	t.Cleanup(up.Close)
	fwd, file := startForwarder(t, "http://bob:pw@"+up.Listener.Addr().String(), nil)
	c, err := net.Dial("tcp", fwd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET https://example.com/x HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "fetched by the corporate proxy" || seen != "GET https://example.com/x "+want {
		t.Fatalf("%d %q, upstream saw %q", resp.StatusCode, body, seen)
	}
	// Wrong credentials: 502 to the client, never the upstream's 407.
	if err := os.WriteFile(file, []byte("http://bob:bad@"+up.Listener.Addr().String()), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, _ := net.Dial("tcp", fwd)
	defer c2.Close()
	fmt.Fprint(c2, "GET https://example.com/x HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	if resp, err := http.ReadResponse(bufio.NewReader(c2), nil); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("wrong credentials: %v %v, want 502", resp, err)
	}
}

func testPolicy() *Policy {
	_, bridge, _ := net.ParseCIDR("10.200.0.0/24")
	_, deny, _ := net.ParseCIDR("192.168.0.0/16")
	return &Policy{Bridge: bridge, Deny: []*net.IPNet{deny}, ConnectPorts: []int{443},
		Resolve: func(_ context.Context, h string) ([]net.IP, error) {
			switch h {
			case "evil.example":
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			case "internal.example":
				return []net.IP{net.ParseIP("192.168.5.5")}, nil
			case "github.com":
				return []net.IP{net.ParseIP("140.82.121.4")}, nil
			}
			return nil, errors.New("no such host")
		},
		LocalAddrs: func() []net.IP { return []net.IP{net.ParseIP("172.17.4.40")} }}
}

// Proxied traffic leaves from the host, so the forwarder refuses what the
// host's firewall keeps from microVMs.
func TestPolicy(t *testing.T) {
	p := testPolicy()
	for _, tc := range []struct {
		host    string
		port    int
		connect bool
		denied  bool
	}{
		{"github.com", 443, true, false},
		{"unresolvable.corp", 443, true, false}, // the upstream resolves it
		{"140.82.121.4", 80, false, false},
		{"github.com", 22, true, true}, // CONNECT only to proxy.connect_ports
		{"github.com", 8080, false, false},
		{"localhost", 443, true, true},
		{"db.localhost", 443, true, true},
		{"LOCALHOST.", 443, true, true},
		{"127.0.0.1", 443, true, true},
		{"::1", 443, true, true},
		{"0.0.0.0", 443, true, true},
		{"169.254.169.254", 80, false, true},
		{"fe80::1", 443, true, true},
		{"fe80::1%eth0", 443, true, true},
		{"224.0.0.1", 443, true, true},
		{"10.200.0.12", 443, true, true},  // another microVM
		{"10.200.0.1", 443, true, true},   // the bridge address
		{"172.17.4.40", 443, true, true},  // the host itself
		{"192.168.1.10", 443, true, true}, // network.egress_deny
		{"evil.example", 443, true, true}, // a name that resolves to loopback
		{"internal.example", 80, false, true},
		{"2130706433", 443, true, true}, // 127.0.0.1 as one number
		{"0x7f000001", 443, true, true},
		{"127.1", 443, true, true},
		{"", 443, true, true},
	} {
		why := p.Check(context.Background(), tc.host, tc.port, tc.connect)
		if (why != "") != tc.denied {
			t.Errorf("Check(%q, %d, connect=%v) = %q, want denied=%v", tc.host, tc.port, tc.connect, why, tc.denied)
		}
	}
}

func TestSelfLoop(t *testing.T) {
	local := []net.IP{net.ParseIP("10.200.0.1"), net.ParseIP("172.17.4.40")}
	for up, want := range map[string]bool{
		"127.0.0.1:3128": true, "localhost:3128": true, "[::1]:3128": true, "0.0.0.0:3128": true,
		"10.200.0.1:3128": true, "172.17.4.40:3128": true,
		"localhost.:3128": true, "db.localhost:3128": true,
		"127.0.0.1:3129": false, "proxy.corp:3128": false, "10.0.0.5:3128": false,
	} {
		if got := SelfLoop(up, "3128", local); got != want {
			t.Errorf("SelfLoop(%s) = %v, want %v", up, got, want)
		}
	}
}

// startForwarderWith runs a Forwarder with a policy and limits.
func startForwarderWith(t *testing.T, upstreamURL string, p *Policy, lim Limits) (string, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "upstream")
	if err := os.WriteFile(file, []byte(upstreamURL), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &Forwarder{Upstream: func() (Upstream, error) { return LoadUpstream(file) }, Allowed: AllowNets(),
		Policy: p, Limits: lim, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DialTimeout: 2 * time.Second}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String(), file
}

func connect(t *testing.T, fwd, target string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	c, err := net.Dial("tcp", fwd)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Time{})
	return c, br, resp.StatusCode
}

// A refused destination never reaches the upstream.
func TestForwarderEnforcesPolicy(t *testing.T) {
	up := startUpstream(t, "")
	fwd, _ := startForwarderWith(t, "http://"+up.addr, testPolicy(), Limits{})
	for _, target := range []string{"127.0.0.1:9090", "169.254.169.254:443", "10.200.0.12:443", "192.168.1.10:443", "localhost:443", "github.com:22"} {
		c, _, code := connect(t, fwd, target)
		c.Close()
		if code != http.StatusForbidden {
			t.Errorf("CONNECT %s: %d, want 403", target, code)
		}
	}
	resp, err := client(fwd).Get("http://169.254.169.254/latest/meta-data/")
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET metadata: %v %v, want 403", resp, err)
	}
	resp.Body.Close()
	if n := len(up.requests()); n != 0 {
		t.Fatalf("upstream saw %d refused requests: %v", n, up.requests())
	}
}

// One client cannot hold more than its share; the slot comes back when its
// tunnel closes.
func TestForwarderLimitsConnections(t *testing.T) {
	up := startUpstream(t, "")
	echo := echoServer(t)
	fwd, _ := startForwarderWith(t, "http://"+up.addr, nil, Limits{Loopback: 1, PerClient: 1})
	c1, _, code := connect(t, fwd, echo)
	if code != 200 {
		t.Fatalf("first tunnel: %d", code)
	}
	c2, _, code := connect(t, fwd, echo)
	c2.Close()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("second tunnel from the same client: %d, want 503", code)
	}
	c1.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c3, _, code := connect(t, fwd, echo)
		c3.Close()
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot not released after the tunnel closed: %d", code)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().String()
}

// A tunnel without traffic is closed after TunnelIdle; one with traffic is not.
func TestTunnelIdleTimeout(t *testing.T) {
	up := startUpstream(t, "")
	echo := echoServer(t)
	fwd, _ := startForwarderWith(t, "http://"+up.addr, nil, Limits{TunnelIdle: 400 * time.Millisecond})
	c, br, code := connect(t, fwd, echo)
	if code != 200 {
		t.Fatal(code)
	}
	defer c.Close()
	for i := 0; i < 5; i++ { // 1 s of traffic every 200 ms: stays open
		fmt.Fprint(c, "x")
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := br.ReadByte(); err != nil {
			t.Fatalf("active tunnel closed: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("idle tunnel not closed by the forwarder: %v", err)
	}
}

// busybox-style https requests: a stalled upstream is given up after
// HeaderWait, and a client that half-closes after its request (busybox wget
// does) still gets the answer.
func TestPassThroughStalledUpstreamAndHalfClose(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	closed := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, c) // never answers; returns when the forwarder closes
		close(closed)
	}()
	fwd, _ := startForwarderWith(t, "http://"+l.Addr().String(), nil, Limits{HeaderWait: 500 * time.Millisecond})
	c, err := net.Dial("tcp", fwd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("stalled upstream connection kept past HeaderWait")
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:pw"))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != want {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		time.Sleep(200 * time.Millisecond) // the half-close reaches the forwarder first
		_, _ = io.WriteString(w, "fetched")
	}))
	defer up.Close()
	fwd2, _ := startForwarderWith(t, "http://bob:pw@"+up.Listener.Addr().String(), nil, Limits{HeaderWait: time.Minute})
	c2, err := net.Dial("tcp", fwd2)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	fmt.Fprint(c2, "GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	_ = c2.(*net.TCPConn).CloseWrite()
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "fetched" {
		t.Fatalf("half-closed client got %d %q", resp.StatusCode, body)
	}
}

// Refused credentials are not sent again until the file changes or
// RefusedWait passes: a rotated password must not lock the corporate account.
func TestRefusedCredentialsNotRetried(t *testing.T) {
	up := startUpstream(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:right")))
	fwd, file := startForwarderWith(t, "http://bob:wrong@"+up.addr, nil, Limits{RefusedWait: time.Hour})
	for i := 0; i < 5; i++ {
		c, _, code := connect(t, fwd, "example.com:443")
		c.Close()
		if code != http.StatusBadGateway {
			t.Fatalf("attempt %d: %d, want 502", i, code)
		}
	}
	if n := len(up.requests()); n != 1 {
		t.Fatalf("refused password sent %d times, want once", n)
	}
	if err := os.WriteFile(file, []byte("http://bob:right@"+up.addr), 0o600); err != nil {
		t.Fatal(err)
	}
	echo := echoServer(t)
	c, _, code := connect(t, fwd, echo)
	c.Close()
	if code != 200 {
		t.Fatalf("after fixing the password: %d, want 200 at once", code)
	}
}

// Headers the client lists in Connection are hop-by-hop too (RFC 9110 7.6.1).
func TestConnectionListedHeadersRemoved(t *testing.T) {
	var got http.Header
	web := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer web.Close()
	up := startUpstream(t, "")
	fwd, _ := startForwarderWith(t, "http://"+up.addr, nil, Limits{})
	req, _ := http.NewRequest(http.MethodGet, web.URL, nil)
	req.Header.Set("Connection", "X-Hop, keep-alive")
	req.Header.Set("X-Hop", "secret")
	req.Header.Set("X-Keep", "kept")
	resp, err := client(fwd).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("X-Hop") != "" || got.Get("X-Keep") != "kept" {
		t.Fatalf("origin saw X-Hop=%q X-Keep=%q", got.Get("X-Hop"), got.Get("X-Keep"))
	}
}

func TestLoadUpstreamRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("http://proxy:3128"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "upstream")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUpstream(link); err == nil {
		t.Fatal("symlink accepted")
	}
	// A FIFO with safe permissions: reading it would block the forwarder.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadUpstream(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LoadUpstream blocked on a FIFO")
	}
}

// Host services keep a budget of their own: microVMs, however many addresses
// they use, cannot take it.
func TestLoopbackBudgetNotStarvedByMicroVMs(t *testing.T) {
	f := &Forwarder{Limits: Limits{PerClient: 1, Total: 2, Loopback: 2}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if !f.acquire("10.200.0.2", false) || !f.acquire("10.200.0.3", false) {
		t.Fatal("microVMs within limits refused")
	}
	if f.acquire("10.200.0.4", false) {
		t.Fatal("microVM total exceeded")
	}
	for i := 0; i < 2; i++ {
		if !f.acquire("127.0.0.1", true) {
			t.Fatal("host services refused while microVMs hold the total")
		}
	}
	if f.acquire("127.0.0.1", true) {
		t.Fatal("host services exceeded their own budget")
	}
	f.release("127.0.0.1", true)
	f.release("10.200.0.2", false)
	if !f.acquire("10.200.0.4", false) {
		t.Fatal("released microVM slot not reusable")
	}
}

// After RefusedWait, one request tries refused credentials again; requests
// arriving meanwhile do not, so a burst cannot lock the corporate account.
func TestRefusedCredentialsProbeOnce(t *testing.T) {
	up := startUpstream(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:right")))
	fwd, _ := startForwarderWith(t, "http://bob:wrong@"+up.addr, nil, Limits{RefusedWait: 200 * time.Millisecond})
	c, _, _ := connect(t, fwd, "example.com:443")
	c.Close()
	time.Sleep(300 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", fwd)
			if err != nil {
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprint(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
			_, _ = http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
		}()
	}
	wg.Wait()
	if n := len(up.requests()); n > 2 {
		t.Fatalf("refused password sent %d times; want the first attempt and one probe", n)
	}
}

// An upstream that stops in the middle of a body is given up after
// HeaderWait without data, also when the client has left.
func TestPassThroughMidBodyStall(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	closed := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		_, _ = http.ReadRequest(br)
		fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\npartial")
		_, _ = io.Copy(io.Discard, br) // stalls; returns when the forwarder closes
		close(closed)
	}()
	fwd, _ := startForwarderWith(t, "http://"+l.Addr().String(), nil, Limits{HeaderWait: 500 * time.Millisecond})
	c, err := net.Dial("tcp", fwd)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(c, "GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	time.Sleep(200 * time.Millisecond)
	c.Close()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("stalled body kept the upstream connection open")
	}
}

// Plain http:// requests from a client that half-closes still get the answer.
func TestForwardHalfClose(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "plain")
	}))
	defer web.Close()
	up := startUpstream(t, "")
	fwd, _ := startForwarderWith(t, "http://"+up.addr, nil, Limits{HeaderWait: time.Minute})
	c, err := net.Dial("tcp", fwd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET %s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", web.URL, web.Listener.Addr())
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "plain" {
		t.Fatalf("half-closed client got %d %q", resp.StatusCode, body)
	}
}
