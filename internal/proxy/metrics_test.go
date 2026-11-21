package proxy

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// startMetered runs a Forwarder whose upstream comes from a 0600 file and
// returns it with its address and the file, so a test can read its metrics.
func startMetered(t *testing.T, upstreamURL string, p *Policy, lim Limits) (*Forwarder, string, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "upstream")
	if err := os.WriteFile(file, []byte(upstreamURL), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &Forwarder{Upstream: func() (Upstream, error) { return LoadUpstream(file) }, Allowed: AllowNets(),
		Policy: p, Limits: lim, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DialTimeout: 2 * time.Second}
	return f, serveForwarder(t, f), file
}

// series reads one series of f's metrics the way the daemon gets them: a
// counter's or gauge's value, a histogram's sample count. found is false
// when no series of that name has exactly these labels.
func series(t *testing.T, f *Forwarder, name string, labels map[string]string) (value float64, found bool) {
	t.Helper()
	mfs, err := f.Metrics().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] != l.GetValue() {
					continue next
				}
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}

// eventually waits until cond holds: a handler counts after it has answered.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requestCounts are the non-zero firerunner_proxy_requests_total series, as
// "kind/outcome" -> count.
func requestCounts(t *testing.T, f *Forwarder) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, kind := range []string{"tunnel", "request"} {
		for _, o := range []string{"allowed", "policy_denied", "limit_refused", "upstream_auth_failed", "upstream_error",
			"upstream_refused", "bad_request", "client_gone", "internal_error", "stopping"} {
			v, found := series(t, f, "firerunner_proxy_requests_total", map[string]string{"kind": kind, "outcome": o})
			if !found {
				t.Fatalf("firerunner_proxy_requests_total{kind=%q,outcome=%q} missing: every combination is created at start", kind, o)
			}
			if v != 0 {
				out[kind+"/"+o] = v
			}
		}
	}
	return out
}

func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// raw sends one request line as written and waits for the answer.
func raw(req string) func(t *testing.T, fwd string) {
	return func(t *testing.T, fwd string) {
		c, err := net.Dial("tcp", fwd)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, req)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
}

// brokenUpload announces a 100-byte body, sends 3 bytes and closes its side:
// the forwarder cannot read the body, which is the client's failure.
func brokenUpload(target string) func(t *testing.T, fwd string) {
	return func(t *testing.T, fwd string) {
		c, err := net.Dial("tcp", fwd)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		u, _ := url.Parse(target)
		fmt.Fprintf(c, "POST %s/up HTTP/1.1\r\nHost: %s\r\nContent-Length: 100\r\n\r\nabc", target, u.Host)
		_ = c.(*net.TCPConn).CloseWrite()
		_, _ = io.Copy(io.Discard, c) // whatever answer, until the forwarder closes
	}
}

// Every request is counted once, by kind and by what became of it.
func TestRequestOutcomesCounted(t *testing.T) {
	right := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:right"))
	open, locked := startUpstream(t, ""), startUpstream(t, right)
	echo, web, dead := echoServer(t), origin(t), deadAddr(t)
	tunnelTo := func(target string) func(*testing.T, string) {
		return func(t *testing.T, fwd string) {
			c, _, _ := connect(t, fwd, target)
			c.Close()
		}
	}
	get := func(t *testing.T, fwd string) {
		resp, err := client(fwd).Get(web.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	for _, c := range []struct {
		name     string
		upstream string
		policy   *Policy
		do       func(*testing.T, string)
		want     string // kind/outcome
	}{
		{"tunnel opened", "http://" + open.addr, nil, tunnelTo(echo), "tunnel/allowed"},
		{"request answered", "http://" + open.addr, nil, get, "request/allowed"},
		{"destination refused", "http://" + open.addr, testPolicy(), tunnelTo("169.254.169.254:443"), "tunnel/policy_denied"},
		{"request to a refused destination", "http://" + open.addr, testPolicy(), raw("GET http://169.254.169.254/ HTTP/1.1\r\nHost: 169.254.169.254\r\n\r\n"), "request/policy_denied"},
		{"credentials refused on CONNECT", "http://bob:wrong@" + locked.addr, nil, tunnelTo(echo), "tunnel/upstream_auth_failed"},
		{"credentials refused on a request", "http://bob:wrong@" + locked.addr, nil, get, "request/upstream_auth_failed"},
		{"upstream unreachable for CONNECT", "http://" + dead, nil, tunnelTo(echo), "tunnel/upstream_error"},
		{"upstream unreachable for a request", "http://" + dead, nil, get, "request/upstream_error"},
		{"upstream file unusable", "ftp://corp:21", nil, get, "request/upstream_error"},
		{"upstream cannot reach the destination", "http://" + open.addr, nil, tunnelTo(dead), "tunnel/upstream_refused"},
		{"origin-form request", "http://" + open.addr, nil, raw("GET /x HTTP/1.1\r\nHost: x\r\n\r\n"), "request/bad_request"},
		{"CONNECT without a port", "http://" + open.addr, nil, raw("CONNECT example.com HTTP/1.1\r\nHost: example.com\r\n\r\n"), "tunnel/bad_request"},
		{"client breaks off its request body", "http://" + open.addr, nil, brokenUpload(web.URL), "request/client_gone"},
		{"client breaks off an https:// upload", "http://" + open.addr, nil, brokenUpload("https://" + strings.TrimPrefix(web.URL, "http://")), "request/client_gone"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, fwd, _ := startMetered(t, c.upstream, c.policy, Limits{})
			c.do(t, fwd)
			eventually(t, c.want, func() bool { return len(requestCounts(t, f)) > 0 })
			if got := requestCounts(t, f); len(got) != 1 || got[c.want] != 1 {
				t.Fatalf("counted %v, want only %s once", got, c.want)
			}
		})
	}
}

// A client at its limit is refused and counted; the gauges show how close the
// busiest client and the host services are to their limits.
func TestLimitRefusalsAndConnectionsCounted(t *testing.T) {
	up := startUpstream(t, "")
	echo := echoServer(t)
	f, fwd, _ := startMetered(t, "http://"+up.addr, nil, Limits{Loopback: 1, PerClient: 2, Total: 3})
	for scope, want := range map[string]float64{"microvms": 3, "busiest_microvm": 2, "host": 1} {
		if v, ok := series(t, f, "firerunner_proxy_connections_limit", map[string]string{"scope": scope}); !ok || v != want {
			t.Errorf("limit %s = %v (%v), want %v", scope, v, ok, want)
		}
	}
	c1, _, code := connect(t, fwd, echo)
	defer c1.Close()
	if code != 200 {
		t.Fatalf("first tunnel: %d", code)
	}
	eventually(t, "the tunnel counted against the host's budget", func() bool {
		v, _ := series(t, f, "firerunner_proxy_connections", map[string]string{"scope": "host"})
		return v == 1
	})
	for _, scope := range []string{"microvms", "busiest_microvm"} {
		if v, ok := series(t, f, "firerunner_proxy_connections", map[string]string{"scope": scope}); !ok || v != 0 {
			t.Errorf("%s = %v (%v): a host service's tunnel is not a microVM's", scope, v, ok)
		}
	}
	c2, _, code := connect(t, fwd, echo)
	c2.Close()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("second tunnel: %d, want 503", code)
	}
	eventually(t, "the refusal counted", func() bool { return requestCounts(t, f)["tunnel/limit_refused"] == 1 })
}

// Bytes are counted as they move, both ways, for tunnels and requests; an
// open tunnel shows in the active gauge until it closes.
func TestTrafficAndActiveCounted(t *testing.T) {
	up := startUpstream(t, "")
	echo, web := echoServer(t), origin(t)
	f, fwd, _ := startMetered(t, "http://"+up.addr, nil, Limits{})
	bytes := func(dir string) float64 {
		v, _ := series(t, f, "firerunner_proxy_bytes_total", map[string]string{"direction": dir})
		return v
	}
	active := func(kind string) float64 {
		v, _ := series(t, f, "firerunner_proxy_active", map[string]string{"kind": kind})
		return v
	}

	c, br, code := connect(t, fwd, echo)
	if code != 200 {
		t.Fatal(code)
	}
	eventually(t, "an active tunnel", func() bool { return active("tunnel") == 1 })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "hello world")
	if _, err := io.ReadFull(br, make([]byte, 11)); err != nil {
		t.Fatal(err)
	}
	// Counted while the tunnel is still open, not when it ends.
	eventually(t, "tunnel bytes", func() bool { return bytes("up") == 11 && bytes("down") == 11 })
	c.Close()
	eventually(t, "the tunnel to end", func() bool { return active("tunnel") == 0 })

	resp, err := client(fwd).Post(web.URL+"/up", "text/plain", strings.NewReader("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	eventually(t, "request bytes", func() bool { return bytes("up") == 11+6 && bytes("down") == float64(11+len(body)) })
	eventually(t, "the request to end", func() bool { return active("request") == 0 })
}

// Connecting to the upstream is timed: the TCP dial for every kind, and the
// CONNECT exchange for tunnels.
func TestUpstreamLatencyObserved(t *testing.T) {
	up := startUpstream(t, "")
	echo, web := echoServer(t), origin(t)
	f, fwd, _ := startMetered(t, "http://"+up.addr, nil, Limits{})
	count := func(step string) float64 {
		v, found := series(t, f, "firerunner_proxy_upstream_connect_seconds", map[string]string{"step": step})
		if !found {
			t.Fatalf("no %s histogram", step)
		}
		return v
	}
	if count("dial") != 0 || count("handshake") != 0 {
		t.Fatal("observed before any request")
	}
	c, _, _ := connect(t, fwd, echo)
	c.Close()
	eventually(t, "tunnel timings", func() bool { return count("dial") == 1 && count("handshake") == 1 })
	resp, err := client(fwd).Get(web.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if count("dial") != 2 || count("handshake") != 1 {
		t.Fatalf("after a plain request: dial %v, handshake %v; want 2, 1", count("dial"), count("handshake"))
	}
}

// A change of the upstream file is counted once when it takes effect, and an
// unusable file once until it is fixed, however many requests meet it.
func TestUpstreamReloadsCounted(t *testing.T) {
	up := startUpstream(t, "")
	web := origin(t)
	f, fwd, file := startMetered(t, "http://"+up.addr, nil, Limits{})
	reloads := func() (ok, failed float64) {
		ok, _ = series(t, f, "firerunner_proxy_upstream_reloads_total", map[string]string{"result": "ok"})
		failed, _ = series(t, f, "firerunner_proxy_upstream_reloads_total", map[string]string{"result": "failed"})
		return ok, failed
	}
	get := func() {
		resp, err := client(fwd).Get(web.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	write := func(s string) {
		if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []struct {
		name       string
		file       string // "" keeps the file
		ok, failed float64
	}{
		{"first read is not a reload", "", 0, 0},
		{"same file again", "", 0, 0},
		{"new credentials", "http://bob:new@" + up.addr, 1, 0},
		{"file broken", "ftp://" + up.addr, 1, 1},
		{"still broken", "", 1, 1},
		{"fixed", "http://" + up.addr, 2, 1},
	} {
		if step.file != "" {
			write(step.file)
		}
		get()
		eventually(t, step.name, func() bool {
			ok, failed := reloads()
			return ok == step.ok && failed == step.failed
		})
	}
}

// The upstream refusing the host's credentials shows in a gauge until it
// accepts them again.
func TestUpstreamAuthFailingGauge(t *testing.T) {
	up := startUpstream(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:right")))
	echo := echoServer(t)
	f, fwd, file := startMetered(t, "http://bob:wrong@"+up.addr, nil, Limits{RefusedWait: time.Hour})
	failing := func() float64 {
		v, found := series(t, f, "firerunner_proxy_upstream_auth_failing", nil)
		if !found {
			t.Fatal("no firerunner_proxy_upstream_auth_failing")
		}
		return v
	}
	if failing() != 0 {
		t.Fatal("failing before any request")
	}
	c, _, code := connect(t, fwd, echo)
	c.Close()
	if code != http.StatusBadGateway || failing() != 1 {
		t.Fatalf("after a 407: status %d, gauge %v", code, failing())
	}
	if err := os.WriteFile(file, []byte("http://bob:right@"+up.addr), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, code = connect(t, fwd, echo)
	c.Close()
	if code != 200 || failing() != 0 {
		t.Fatalf("after fixing the password: status %d, gauge %v", code, failing())
	}
}

// A tunnel the forwarder cannot take over from the HTTP server is its own
// failure, not the client's: internal_error, 500.
func TestTunnelWithoutHijackIsInternalError(t *testing.T) {
	up := startUpstream(t, "")
	f, _, _ := startMetered(t, "http://"+up.addr, nil, Limits{})
	f.Allowed = func(net.IP) bool { return true } // httptest's client is 192.0.2.1
	rec := httptest.NewRecorder()                 // not an http.Hijacker
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodConnect, echoServer(t), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if got := requestCounts(t, f); len(got) != 1 || got["tunnel/internal_error"] != 1 {
		t.Fatalf("counted %v, want tunnel/internal_error once", got)
	}
}

// Requests refused because the forwarder has cut everything on a stop count
// as stopping, not as a connection limit.
func TestRequestsAfterTheCutCountAsStopping(t *testing.T) {
	up := startUpstream(t, "")
	f := forwarderTo(t, "http://"+up.addr, 0)
	_, stop, done := stoppable(t, f)
	stop()
	<-done
	f.Allowed = func(net.IP) bool { return true }
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodConnect, echoServer(t), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := requestCounts(t, f); len(got) != 1 || got["tunnel/stopping"] != 1 {
		t.Fatalf("counted %v, want tunnel/stopping once", got)
	}
}

// Requests the forwarder refuses itself because the upstream refused these
// credentials a moment ago count as upstream_auth_failed too: sustained
// traffic keeps the count rising though the upstream saw only one 407, which
// FireRunnerProxyUpstreamAuthFailing relies on.
func TestKnownRefusedCredentialsCounted(t *testing.T) {
	up := startUpstream(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:right")))
	f, fwd, _ := startMetered(t, "http://bob:wrong@"+up.addr, nil, Limits{RefusedWait: time.Hour})
	for i := 0; i < 3; i++ {
		c, _, code := connect(t, fwd, "example.com:443")
		c.Close()
		if code != http.StatusBadGateway {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if n := len(up.requests()); n != 1 {
		t.Fatalf("upstream asked %d times, want once", n)
	}
	eventually(t, "three refusals", func() bool { return requestCounts(t, f)["tunnel/upstream_auth_failed"] == 3 })
}

// Requests already on their way when the first 407 comes each get a 407 of
// their own; they are one refusal of the credentials, not several, or one
// transient refusal under load would look like a refused retry.
func TestConcurrentRefusalsCountOnce(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	asked := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		<-release
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	t.Cleanup(up.Close)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f, fwd, _ := startMetered(t, "http://bob:wrong@"+up.Listener.Addr().String(), nil, Limits{RefusedWait: time.Hour})
	const n = 3
	codes := make(chan int, n)
	for range n {
		go func() {
			c, err := net.Dial("tcp", fwd)
			if err != nil {
				codes <- 0
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprint(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
			if err != nil {
				codes <- 0
				return
			}
			codes <- resp.StatusCode
		}()
	}
	eventually(t, "all CONNECTs at the upstream", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return asked == n
	})
	once.Do(func() { close(release) })
	for range n {
		if code := <-codes; code != http.StatusBadGateway {
			t.Fatalf("refused CONNECT: %d, want 502", code)
		}
	}
	if v, _ := series(t, f, "firerunner_proxy_upstream_auth_refusals_total", nil); v != 1 {
		t.Fatalf("refusals after %d concurrent 407s: %v, want 1", n, v)
	}
	if got := requestCounts(t, f)["tunnel/upstream_auth_failed"]; got != n {
		t.Fatalf("upstream_auth_failed %v, want %d", got, n)
	}
}

// firerunner_proxy_upstream_auth_refusals_total counts refusals of the
// credentials, not the requests the forwarder held back after one: two of
// them are at least Limits.RefusedWait apart, the retry refused again, which
// FireRunnerProxyUpstreamAuthFailing pages on. Plain requests count too.
func TestUpstreamAuthRefusalsCounted(t *testing.T) {
	up := startUpstream(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("bob:right")))
	web := origin(t)
	const wait = 300 * time.Millisecond
	f, fwd, _ := startMetered(t, "http://bob:wrong@"+up.addr, nil, Limits{RefusedWait: wait})
	refusals := func() float64 {
		v, found := series(t, f, "firerunner_proxy_upstream_auth_refusals_total", nil)
		if !found {
			t.Fatal("no firerunner_proxy_upstream_auth_refusals_total")
		}
		return v
	}
	if refusals() != 0 {
		t.Fatal("refusals before any request")
	}
	tunnel := func() {
		c, _, code := connect(t, fwd, "example.com:443")
		c.Close()
		if code != http.StatusBadGateway {
			t.Fatalf("CONNECT with refused credentials: %d", code)
		}
	}
	tunnel() // the upstream's 407
	tunnel() // held back: not sent
	tunnel()
	if r := refusals(); r != 1 {
		t.Fatalf("refusals after one 407 and two held back: %v, want 1", r)
	}
	time.Sleep(wait + 100*time.Millisecond)
	resp, err := client(fwd).Get(web.URL) // the retry, as a plain request
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if r, n := refusals(), len(up.requests()); r != 2 || n != 2 {
		t.Fatalf("after the retry was refused: %v refusals, upstream asked %d times; want 2, 2", r, n)
	}
}

// Labels have a handful of fixed values: never a client address or a
// destination, which would make a series per VM or per site.
func TestMetricsLabelsAreBounded(t *testing.T) {
	up := startUpstream(t, "")
	echo, web := echoServer(t), origin(t)
	f, fwd, _ := startMetered(t, "http://"+up.addr, testPolicy(), Limits{})
	c, _, _ := connect(t, fwd, echo)
	c.Close()
	raw("GET http://169.254.169.254/ HTTP/1.1\r\nHost: 169.254.169.254\r\n\r\n")(t, fwd)
	if resp, err := client(fwd).Get(web.URL); err == nil {
		resp.Body.Close()
	}
	allowed := map[string]bool{"kind": true, "outcome": true, "direction": true, "step": true, "result": true, "scope": true}
	mfs, err := f.Metrics().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mf := range mfs {
		names = append(names, mf.GetName())
		if !strings.HasPrefix(mf.GetName(), "firerunner_proxy_") {
			t.Errorf("%s: the daemon re-exports only firerunner_proxy_* metrics", mf.GetName())
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s has label %s", mf.GetName(), l.GetName())
				}
				if strings.ContainsAny(l.GetValue(), ".:") {
					t.Errorf("%s{%s=%q} looks like an address", mf.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}
	sort.Strings(names)
	// MetricNames is the daemon's allow-list: it must name every family.
	want := MetricNames()
	sort.Strings(want)
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("metrics %v, MetricNames %v", names, want)
	}
}
