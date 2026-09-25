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
			splice(c, buf.Reader, dst, dst)
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
