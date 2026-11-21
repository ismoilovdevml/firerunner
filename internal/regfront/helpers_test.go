package regfront_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/regfront"
)

// testLimits are short enough for a test to see every bound fire.
func testLimits() regfront.Limits {
	return regfront.Limits{
		PerClient:      64,
		Total:          128,
		HeaderRead:     5 * time.Second,
		Idle:           5 * time.Second,
		Dial:           2 * time.Second,
		UpstreamHeader: 5 * time.Second,
		Stall:          5 * time.Second,
		ShutdownWait:   2 * time.Second,
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startUpstream runs h as the registry and returns its URL.
func startUpstream(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	return up.URL
}

// startFront runs a front for upstream on a loopback port through Serve,
// as the command does, and stops it when the test ends. stop cancels it
// early and returns what Serve returned.
func startFront(t *testing.T, upstream string, lim regfront.Limits) (addr string, stop func() error) {
	t.Helper()
	return startFrontLog(t, upstream, lim, discardLog())
}

// startFrontLog is startFront logging to log.
func startFrontLog(t *testing.T, upstream string, lim regfront.Limits, log *slog.Logger) (addr string, stop func() error) {
	t.Helper()
	f, err := regfront.New(upstream, lim, log)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, l) }()
	var once sync.Once
	var serveErr error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case serveErr = <-done:
			case <-time.After(30 * time.Second):
				serveErr = errors.New("serve did not return 30s after its context was cancelled")
			}
		})
		return serveErr
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	return l.Addr().String(), stop
}

// client is an HTTP client that never follows redirects and drops its
// connections when the test ends.
func client(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableCompression: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{
		Transport:     tr,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// rawRequest sends req byte for byte (no client normalises the path) and
// reads one response.
func rawRequest(t *testing.T, addr, req string) (*http.Response, []byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	method, _, _ := strings.Cut(req, " ")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%q: %v", req, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%q: reading the body: %v", req, err)
	}
	return resp, body
}

// errorCode is the first code of a registry error body.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err != nil || len(e.Errors) == 0 {
		t.Fatalf("not a registry error body: %q (%v)", body, err)
	}
	return e.Errors[0].Code
}

// recorder is a registry that answers 200 "ok" and keeps what it was asked.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, r.Clone(context.Background()))
	rec.mu.Unlock()
	_, _ = io.WriteString(w, "ok")
}

func (rec *recorder) requests() []*http.Request {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]*http.Request(nil), rec.reqs...)
}

func (rec *recorder) reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reqs = nil
}

// eventually waits up to 10s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
