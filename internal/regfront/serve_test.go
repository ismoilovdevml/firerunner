package regfront_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/regfront"
)

// heldRegistry answers blob requests with a header and holds the body until
// release is closed (or the request is cancelled); started counts requests
// that got that far.
func heldRegistry(t *testing.T) (url string, started chan struct{}, release chan struct{}) {
	t.Helper()
	started = make(chan struct{}, 64)
	release = make(chan struct{})
	url = startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		_, _ = io.WriteString(w, "da")
		http.NewResponseController(w).Flush()
		started <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, "ta")
		case <-r.Context().Done():
		}
	})
	return url, started, release
}

// openPull sends a blob GET on a connection of its own and returns it with
// a reader of the answer.
func openPull(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(c, "GET "+blobPath+" HTTP/1.1\r\nHost: mirror\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

// refused reports whether the front dropped a new connection without an
// answer.
func refused(t *testing.T, addr string) bool {
	t.Helper()
	c, br := openPull(t, addr)
	defer c.Close()
	_, err := http.ReadResponse(br, nil)
	return err != nil
}

func waitStarted(t *testing.T, started chan struct{}, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d requests reached the registry", i, n)
		}
	}
}

// TestConnectionLimits: a client address may hold PerClient connections,
// all clients together Total; a connection over either is dropped, and a
// closed one frees its place.
func TestConnectionLimits(t *testing.T) {
	cases := []struct {
		name             string
		perClient, total int
		open             int // connections that are served
	}{
		{"per client", 2, 10, 2},
		{"total", 10, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, started, release := heldRegistry(t)
			defer close(release)
			lim := testLimits()
			lim.PerClient, lim.Total = tc.perClient, tc.total
			addr, _ := startFront(t, upstream, lim)
			var conns []net.Conn
			for range tc.open {
				c, _ := openPull(t, addr)
				conns = append(conns, c)
			}
			waitStarted(t, started, tc.open)
			if !refused(t, addr) {
				t.Fatalf("connection %d was served, want it dropped", tc.open+1)
			}
			_ = conns[0].Close()
			eventually(t, "a closed connection to free its place", func() bool {
				c, br := openPull(t, addr)
				defer c.Close()
				resp, err := http.ReadResponse(br, nil)
				return err == nil && resp.StatusCode == http.StatusOK
			})
		})
	}
}

// TestShutdownLetsPullsFinish: once stopped, the front takes no new
// connection, and a pull under way still completes.
func TestShutdownLetsPullsFinish(t *testing.T) {
	upstream, started, release := heldRegistry(t)
	lim := testLimits()
	lim.ShutdownWait = 20 * time.Second
	addr, stop := startFront(t, upstream, lim)
	_, br := openPull(t, addr)
	waitStarted(t, started, 1)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	eventually(t, "the listener to close", func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	})
	select {
	case err := <-stopped:
		t.Fatalf("Serve returned (%v) while a pull was under way", err)
	default:
	}
	close(release)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "data" {
		t.Fatalf("pull got %q (%v), want %q", body, err, "data")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the last pull ended")
	}
}

// TestShutdownIsBounded: a pull still running ShutdownWait after the stop is
// cut, and its registry request cancelled.
func TestShutdownIsBounded(t *testing.T) {
	upstream, started, release := heldRegistry(t)
	defer close(release)
	lim := testLimits()
	lim.ShutdownWait = 300 * time.Millisecond
	lim.Stall = 0 // only ShutdownWait may end the held pull
	addr, stop := startFront(t, upstream, lim)
	_, br := openPull(t, addr)
	waitStarted(t, started, 1)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Serve returned %v after the stop, want about %v", d, lim.ShutdownWait)
	}
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Errorf("the cut pull ended cleanly with %q", body)
	}
}

// TestServeListenerError: Serve reports a listener that fails.
func TestServeListenerError(t *testing.T) {
	f, err := regfront.New("http://127.0.0.1:5000", testLimits(), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if err := f.Serve(t.Context(), l); err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}

// TestNewUpstream: the registry address is http://host:port and nothing else.
func TestNewUpstream(t *testing.T) {
	cases := []struct {
		upstream string
		ok       bool
	}{
		{"http://127.0.0.1:5000", true},
		{"http://127.0.0.1:5000/", true},
		{"http://[::1]:5001", true},
		{"http://localhost:5002", true},
		{"", false},
		{"127.0.0.1:5000", false},
		{"https://127.0.0.1:5000", false},
		{"ftp://127.0.0.1:5000", false},
		{"http://127.0.0.1", false},
		{"http://:5000", false},
		{"http://127.0.0.1:5000/v2/", false},
		{"http://user:pass@127.0.0.1:5000", false},
		{"http://127.0.0.1:5000?x=1", false},
		{"http://127.0.0.1:5000#x", false},
		{"http://127.0.0.1:notaport", false},
	}
	for _, tc := range cases {
		t.Run(tc.upstream, func(t *testing.T) {
			_, err := regfront.New(tc.upstream, testLimits(), discardLog())
			if (err == nil) != tc.ok {
				t.Errorf("New(%q) error %v, want ok=%v", tc.upstream, err, tc.ok)
			}
		})
	}
}
