package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/ismoilovdevml/firerunner/internal/proxy"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"google.golang.org/protobuf/proto"
)

// The forwarder (firerunner proxy) is a process of its own, sandboxed by its
// unit. It serves its metrics on a root-only unix socket and the daemon
// re-exports them under its /metrics, so a host stays one scrape target
// behind the one firewall rule install.sh writes for :9477.

// proxyMetricsTimeout bounds reading the forwarder's metrics within one
// scrape: well inside Prometheus' default scrape timeout of 10 s, so a hung
// forwarder costs firerunner_proxy_metrics_up 0, not the daemon's scrape.
const proxyMetricsTimeout = 2 * time.Second

// maxProxyMetricsBytes caps what is read from the socket; the forwarder's
// metrics are a few kB.
const maxProxyMetricsBytes = 1 << 20

const proxyMetricsUp = "firerunner_proxy_metrics_up"

// maxProxySeriesPerFamily caps the series of one re-exported family. The
// forwarder's largest has 20 (2 kinds by 10 outcomes); a forwarder sending
// more is broken, and its output must not grow the daemon's scrape.
const maxProxySeriesPerFamily = 64

// proxyGatherer gathers the forwarder's metrics within ctx (the scrape's)
// when proxy.enabled: its firerunner_proxy_* families, and
// firerunner_proxy_metrics_up saying whether they could be read. It never
// fails: the forwarder's trouble must not fail the daemon's own metrics.
func (d *Daemon) proxyGatherer(ctx context.Context) prometheus.Gatherer {
	return prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		if !d.cfgSnapshot().Proxy.Enabled {
			return nil, nil
		}
		fams, err := readProxyMetrics(ctx, d.proxyMetricsSocket)
		up := 1.0
		if err != nil {
			d.warnLimited("proxy-metrics", "cannot read the forwarder's metrics: firerunner_proxy_* missing from /metrics",
				"socket", d.proxyMetricsSocket, "err", err)
			fams, up = nil, 0
		}
		return append(fams, &dto.MetricFamily{
			Name:   proto.String(proxyMetricsUp),
			Help:   proto.String("1 if the daemon read the forwarder's metrics (firerunner_proxy_*) for this scrape; only with proxy.enabled."),
			Type:   dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(up)}}},
		}), nil
	})
}

// readProxyMetrics fetches the forwarder's metric families from its socket.
// Only the families proxy.MetricNames lists are kept, so none can clash with
// the daemon's; each may have at most maxProxySeriesPerFamily series, and
// they must be consistent (no series twice) on their own.
func readProxyMetrics(ctx context.Context, socket string) ([]*dto.MetricFamily, error) {
	ctx, cancel := context.WithTimeout(ctx, proxyMetricsTimeout)
	defer cancel()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://firerunner-proxy/metrics", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", string(expfmt.NewFormat(expfmt.TypeProtoDelim)))
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProxyMetricsBytes))
		return nil, fmt.Errorf("forwarder metrics: %s", resp.Status)
	}
	dec := expfmt.NewDecoder(io.LimitReader(resp.Body, maxProxyMetricsBytes), expfmt.ResponseFormat(resp.Header))
	known := map[string]bool{}
	for _, name := range proxy.MetricNames() {
		known[name] = true
	}
	var fams []*dto.MetricFamily
	for {
		mf := &dto.MetricFamily{}
		if err := dec.Decode(mf); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("forwarder metrics: %w", err)
		}
		if !known[mf.GetName()] {
			continue
		}
		if n := len(mf.GetMetric()); n > maxProxySeriesPerFamily {
			return nil, fmt.Errorf("forwarder metrics: %s has %d series, more than the %d it can have", mf.GetName(), n, maxProxySeriesPerFamily)
		}
		fams = append(fams, mf)
	}
	// Gatherers checks what it merges; an inconsistent family would fail the
	// whole scrape there, so it is checked here, alone.
	if _, err := (prometheus.Gatherers{prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) { return fams, nil })}).Gather(); err != nil {
		return nil, fmt.Errorf("forwarder metrics: %w", err)
	}
	return fams, nil
}
