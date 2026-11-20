package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// End to end with the real forwarder: every family it exports reaches the
// daemon's /metrics, histograms and the connection gauges included.
func TestMetricsCarryTheRealForwarders(t *testing.T) {
	d, _ := newTestDaemon(t)
	dir, err := os.MkdirTemp("", "frpm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &proxy.Forwarder{Upstream: func() (proxy.Upstream, error) { return proxy.Upstream{}, errors.New("none") },
		Allowed: proxy.AllowNets(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Limits: proxy.DefaultLimits,
		MetricsSocket: filepath.Join(dir, "metrics.sock")}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- proxy.ServeListeners(ctx, f, l) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	d.mu.Lock()
	d.cfg.Proxy.Enabled = true
	d.mu.Unlock()
	d.proxyMetricsSocket = f.MetricsSocket

	var body string
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(body, "firerunner_proxy_metrics_up 1") {
		if time.Now().After(deadline) {
			t.Fatalf("forwarder metrics never read:\n%s", body)
		}
		time.Sleep(20 * time.Millisecond)
		rec := httptest.NewRecorder()
		d.metricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body = rec.Body.String()
	}
	mfs, err := f.Metrics().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if !strings.Contains(body, "# TYPE "+mf.GetName()+" ") {
			t.Errorf("%s not re-exported", mf.GetName())
		}
	}
	for _, s := range []string{`firerunner_proxy_connections_limit{scope="busiest_microvm"} 256`,
		`firerunner_proxy_upstream_connect_seconds_bucket{step="dial",le="15"} 0`} {
		if !strings.Contains(body, s) {
			t.Errorf("missing %s", s)
		}
	}
}

// serveSocket serves h on a unix socket until the test ends.
func serveSocket(t *testing.T, path string, h http.Handler) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// The daemon's /metrics carries the forwarder's metrics, so Prometheus
// scrapes one target per host. Proxy off: none of them; on, a gauge says
// whether they could be read, and the daemon's own metrics never depend on it.
func TestMetricsIncludeTheForwarders(t *testing.T) {
	d, _ := newTestDaemon(t)
	dir, err := os.MkdirTemp("", "frpm") // short: holds unix sockets
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		d.metricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("scrape: %d %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "firerunner_pool_target ") {
			t.Fatalf("the daemon's own metrics are missing:\n%s", rec.Body)
		}
		return rec.Body.String()
	}
	setProxy := func(on bool, socket string) {
		d.mu.Lock()
		d.cfg.Proxy.Enabled = on
		d.mu.Unlock()
		d.proxyMetricsSocket = socket
	}

	// What the forwarder serves: its own families, and families the daemon
	// has too (go_*), which must not be taken.
	reg := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_proxy_requests_total", Help: "h"}, []string{"kind", "outcome"})
	requests.WithLabelValues("tunnel", "allowed").Add(3)
	reg.MustRegister(requests, collectors.NewGoCollector())
	forwarder := filepath.Join(dir, "fwd.sock")
	serveSocket(t, forwarder, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	// One that accepts and never answers.
	hung := filepath.Join(dir, "hung.sock")
	hl, err := net.Listen("unix", hung)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hl.Close() })
	// One whose output is inconsistent: the same series twice.
	broken := filepath.Join(dir, "broken.sock")
	serveSocket(t, broken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, "# TYPE firerunner_proxy_x counter\nfirerunner_proxy_x 1\nfirerunner_proxy_x 2\n")
	}))

	for _, c := range []struct {
		name     string
		on       bool
		socket   string
		want     []string
		wantNot  []string
		maxTaken time.Duration
	}{
		{"proxy off", false, forwarder, nil, []string{"firerunner_proxy_"}, time.Second},
		{"forwarder serving", true, forwarder,
			[]string{`firerunner_proxy_requests_total{kind="tunnel",outcome="allowed"} 3`, "firerunner_proxy_metrics_up 1"}, nil, time.Second},
		{"forwarder not running", true, filepath.Join(dir, "missing.sock"),
			[]string{"firerunner_proxy_metrics_up 0"}, []string{"firerunner_proxy_requests_total"}, time.Second},
		{"forwarder hangs", true, hung,
			[]string{"firerunner_proxy_metrics_up 0"}, nil, proxyMetricsTimeout + time.Second},
		{"forwarder output inconsistent", true, broken,
			[]string{"firerunner_proxy_metrics_up 0"}, []string{"firerunner_proxy_x"}, time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			setProxy(c.on, c.socket)
			start := time.Now()
			body := scrape()
			if took := time.Since(start); took > c.maxTaken {
				t.Errorf("scrape took %v", took)
			}
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(body, w) {
					t.Errorf("unexpected %q", w)
				}
			}
			if n := strings.Count(body, "# TYPE go_goroutines "); n != 1 {
				t.Errorf("go_goroutines %d times: the forwarder's go_* must not be taken", n)
			}
		})
	}
}
