package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stoppable runs f with ServeListeners on a loopback listener until stop is
// called; done gets what ServeListeners returned.
func stoppable(t *testing.T, f *Forwarder) (addr string, stop func(), done <-chan error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, finished := make(chan error, 1), make(chan struct{})
	go func() {
		ch <- ServeListeners(ctx, f, l)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("ServeListeners did not return")
		}
	})
	return l.Addr().String(), cancel, ch
}

func forwarderTo(t *testing.T, upstreamURL string, wait time.Duration) *Forwarder {
	t.Helper()
	file := filepath.Join(t.TempDir(), "upstream")
	if err := os.WriteFile(file, []byte(upstreamURL), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Forwarder{Upstream: func() (Upstream, error) { return LoadUpstream(file) }, Allowed: AllowNets(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DialTimeout: 2 * time.Second, ShutdownWait: wait}
}

// On stop the forwarder takes no new connections, but a tunnel already open
// keeps working and the forwarder returns as soon as it ends.
func TestStopLetsOpenTunnelsFinish(t *testing.T) {
	up := startUpstream(t, "")
	echo := echoServer(t)
	addr, stop, done := stoppable(t, forwarderTo(t, "http://"+up.addr, 10*time.Second))
	c, br, code := connect(t, addr, echo)
	if code != 200 {
		t.Fatal(code)
	}
	defer c.Close()

	stop()
	eventually(t, "the listener to close", func() bool {
		n, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			n.Close()
		}
		return err != nil
	})
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 3; i++ {
		fmt.Fprint(c, "x")
		if b, err := br.ReadByte(); err != nil || b != 'x' {
			t.Fatalf("open tunnel after stop: %q %v", b, err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("returned with a tunnel open: %v", err)
	default:
	}
	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still running after the last tunnel closed")
	}
}

// A plain request under way when the stop comes gets its answer.
func TestStopLetsOpenRequestsFinish(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, "late answer")
	}))
	t.Cleanup(slow.Close)
	addr, stop, done := stoppable(t, forwarderTo(t, "http://"+slow.Listener.Addr().String(), 10*time.Second))

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := client(addr).Get("http://example.com/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()
	time.Sleep(200 * time.Millisecond) // the request reaches the upstream
	stop()
	time.Sleep(200 * time.Millisecond)
	close(release)
	if r := <-got; r.err != nil || r.body != "late answer" {
		t.Fatalf("request under way at stop: %q %v", r.body, r.err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still running after the request ended")
	}
}

// What is still open when ShutdownWait runs out is cut: a quiet tunnel and a
// request whose upstream never answers do not hold the stop.
func TestStopCutsWhatOutlastsTheWait(t *testing.T) {
	stuck := make(chan struct{})
	// CONNECT opens a tunnel to the echo server; any other request hangs.
	echo := echoServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			<-stuck
			return
		}
		dst, err := net.Dial("tcp", echo)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		c, buf, _ := w.(http.Hijacker).Hijack()
		splice(c, buf.Reader, dst, dst, 0, nil, nil)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(stuck) }) // runs first: lets upstream.Close return

	addr, stop, done := stoppable(t, forwarderTo(t, "http://"+upstream.Listener.Addr().String(), 300*time.Millisecond))
	c, br, code := connect(t, addr, echo)
	if code != 200 {
		t.Fatal(code)
	}
	defer c.Close()
	// The upstream never answers: the request can only end by being cut.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		if resp, err := client(addr).Get("http://example.com/"); err == nil {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(200 * time.Millisecond) // both are under way

	start := time.Now()
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open connections held the stop past ShutdownWait")
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Fatalf("stopped after %v, before ShutdownWait", took)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("tunnel still open after the stop")
	}
	select {
	case <-reqDone:
	case <-time.After(3 * time.Second):
		t.Fatal("request to a silent upstream not cut")
	}
}

// With no wait, a stop cuts open tunnels at once, as before.
func TestStopWithoutWaitCutsAtOnce(t *testing.T) {
	up := startUpstream(t, "")
	echo := echoServer(t)
	addr, stop, done := stoppable(t, forwarderTo(t, "http://"+up.addr, 0))
	c, _, code := connect(t, addr, echo)
	if code != 200 {
		t.Fatal(code)
	}
	defer c.Close()
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("did not stop at once")
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := bufio.NewReader(c).ReadByte(); err == nil {
		t.Fatal("tunnel still open")
	}
}
