package proxy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Kinds of request, the kind label: a CONNECT tunnel, or a plain request
// (http://, and https:// URLs asked for without CONNECT).
const (
	kindTunnel  = "tunnel"
	kindRequest = "request"
)

// Outcomes of a request, the outcome label of firerunner_proxy_requests_total.
const (
	// The upstream opened the tunnel, or answered the plain request (its
	// answer is passed on whatever its status).
	outcomeAllowed = "allowed"
	// 403: a client address or a destination the policy refuses.
	outcomePolicyDenied = "policy_denied"
	// 503: the client, all microVMs or the host services hold their limit.
	outcomeLimitRefused = "limit_refused"
	// The forwarder is stopping: a request refused after the cut (503), or
	// one the cut ended. Apart from the outcomes above so a restart is not
	// read as a limit or an upstream failure.
	outcomeStopping = "stopping"
	// 502: the upstream answered 407, or refused these credentials within
	// Limits.RefusedWait so they were not sent again.
	outcomeAuthFailed = "upstream_auth_failed"
	// 502: no usable upstream file, the upstream unreachable, or no answer.
	outcomeUpstreamError = "upstream_error"
	// 502: the upstream answered CONNECT with another status than 200 or
	// 407: it blocks or cannot reach that destination. Apart from
	// upstream_error so a blocked site does not look like a broken upstream.
	outcomeUpstreamRefused = "upstream_refused"
	// 400: not a proxy request or a bad target.
	outcomeBadRequest = "bad_request"
	// The client broke off the body of its request before the forwarder had
	// sent it on: the upstream is not to blame.
	outcomeClientGone = "client_gone"
	// 500: the forwarder could not take the connection over for a tunnel.
	outcomeInternalError = "internal_error"
)

var outcomes = [...]string{outcomeAllowed, outcomePolicyDenied, outcomeLimitRefused, outcomeAuthFailed,
	outcomeUpstreamError, outcomeUpstreamRefused, outcomeBadRequest, outcomeClientGone, outcomeInternalError,
	outcomeStopping}

// MetricNames are the names of every family Metrics gathers. The daemon
// re-exports these and no others.
func MetricNames() []string {
	return []string{"firerunner_proxy_active", "firerunner_proxy_requests_total", "firerunner_proxy_bytes_total",
		"firerunner_proxy_upstream_connect_seconds", "firerunner_proxy_upstream_reloads_total",
		"firerunner_proxy_upstream_auth_failing", "firerunner_proxy_upstream_auth_refusals_total",
		"firerunner_proxy_connections", "firerunner_proxy_connections_limit"}
}

// metrics are one Forwarder's. They have a registry of their own, without
// the Go runtime collectors: the daemon re-exports every family under its
// own endpoint, which already has go_* and process_* of its own.
type metrics struct {
	reg         *prometheus.Registry
	active      *prometheus.GaugeVec
	requests    *prometheus.CounterVec
	bytesUp     prometheus.Counter
	bytesDown   prometheus.Counter
	dial        prometheus.Observer
	handshake   prometheus.Observer
	reloads     *prometheus.CounterVec
	authFailing prometheus.Gauge
	// authRefusals counts the upstream's refusals of the credentials: a 407
	// that starts a hold-back (setRefused). Not the requests held back
	// during one, nor the other 407s of requests that were already on their
	// way: two of these are at least Limits.RefusedWait apart, so two in a
	// while mean the retry was refused again rather than one transient
	// refusal. The hold-back lives in memory: a restart right after a 407
	// lets the next request try the credentials again, which can count a
	// second time.
	authRefusals prometheus.Counter
}

func newMetrics(f *Forwarder) *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "firerunner_proxy_active",
			Help: "Requests and tunnels the forwarder is handling now, by kind (tunnel: CONNECT, request: plain http:// or https:// URL)."}, []string{"kind"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_proxy_requests_total",
			Help: "Requests to the forwarder, by kind (tunnel, request) and outcome: allowed (the upstream opened the tunnel or answered), " +
				"policy_denied (403), limit_refused (503), upstream_auth_failed (the upstream refused the credentials: 407), " +
				"upstream_error (502: no usable upstream file, upstream unreachable or silent), " +
				"upstream_refused (502: the upstream answered CONNECT with another status, it blocks or cannot reach that site), bad_request (400), " +
				"client_gone (the client broke off its request body), internal_error (500), stopping (refused or cut by a stop of the forwarder)."},
			[]string{"kind", "outcome"}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_proxy_upstream_reloads_total",
			Help: "Changes of the upstream file the forwarder took up (it reads the file for every request), by result: " +
				"ok (another address or credentials in use), failed (the file became unusable: unreadable, wrong owner or mode, not a proxy URL, or the forwarder itself)."},
			[]string{"result"}),
		authFailing: prometheus.NewGauge(prometheus.GaugeOpts{Name: "firerunner_proxy_upstream_auth_failing",
			Help: "1 while the upstream's last answer refused the credentials (407): requests fail until the upstream file is fixed or, after a while, one request tries them again."}),
		authRefusals: prometheus.NewCounter(prometheus.CounterOpts{Name: "firerunner_proxy_upstream_auth_refusals_total",
			Help: "Refusals of the forwarder's credentials by the upstream: 407s that start a hold-back (new credentials, a refused retry, or the last hold-back over). Requests held back until the forwarder tries the credentials again, and the 407s of requests already on their way, are not counted again."}),
	}
	bytes := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "firerunner_proxy_bytes_total",
		Help: "Bytes carried, counted as they move, by direction: up (client to upstream), down (upstream to client). Tunnels' bytes and requests' and answers' bodies, without headers."},
		[]string{"direction"})
	m.bytesUp, m.bytesDown = bytes.WithLabelValues("up"), bytes.WithLabelValues("down")
	// Dials end within DialTimeout (15 s in firerunner-proxy): 5 ms tells a
	// LAN upstream from one across a WAN link; 1 s and over is a struggling
	// upstream or a dropped SYN being resent.
	upstream := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "firerunner_proxy_upstream_connect_seconds",
		Help:    "Time to reach the upstream, by step: dial (TCP connect, every kind; failed dials are not observed), handshake (the upstream's answer to CONNECT, tunnels only).",
		Buckets: []float64{0.005, 0.025, 0.1, 0.25, 1, 5, 15}}, []string{"step"})
	m.dial, m.handshake = upstream.WithLabelValues("dial"), upstream.WithLabelValues("handshake")
	// Every series exists from the start, so dashboards and rate() see 0
	// rather than nothing before the first request.
	for _, k := range []string{kindTunnel, kindRequest} {
		m.active.WithLabelValues(k)
		for _, o := range outcomes {
			m.requests.WithLabelValues(k, o)
		}
	}
	m.reloads.WithLabelValues("ok")
	m.reloads.WithLabelValues("failed")
	m.reg.MustRegister(m.active, m.requests, bytes, upstream, m.reloads, m.authFailing, m.authRefusals, connCollector{f: f,
		conns: prometheus.NewDesc("firerunner_proxy_connections",
			"Requests and tunnels holding a slot, by scope: microvms (all microVMs together), busiest_microvm (the microVM holding most), host (host services on 127.0.0.1).",
			[]string{"scope"}, nil),
		limit: prometheus.NewDesc("firerunner_proxy_connections_limit",
			"The limit of each scope of firerunner_proxy_connections; 0 is no limit. Past it, requests are refused with 503.",
			[]string{"scope"}, nil),
	})
	return m
}

// connCollector reads the slots in use when scraped, rather than keeping a
// gauge per client: the busiest client is what nears a limit, and its address
// must not become a label.
type connCollector struct {
	f            *Forwarder
	conns, limit *prometheus.Desc
}

func (c connCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.conns
	ch <- c.limit
}

func (c connCollector) Collect(ch chan<- prometheus.Metric) {
	f := c.f
	f.mu.Lock()
	total, busiest, host := f.total, 0, 0
	for client, n := range f.active {
		if ip := net.ParseIP(client); ip != nil && ip.IsLoopback() {
			host = max(host, n)
		} else {
			busiest = max(busiest, n)
		}
	}
	f.mu.Unlock()
	for _, s := range []struct {
		scope    string
		n, limit int
	}{{"microvms", total, f.Limits.Total}, {"busiest_microvm", busiest, f.Limits.PerClient}, {"host", host, f.Limits.Loopback}} {
		ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(s.n), s.scope)
		ch <- prometheus.MustNewConstMetric(c.limit, prometheus.GaugeValue, float64(s.limit), s.scope)
	}
}

// Metrics are the forwarder's Prometheus metrics, all named firerunner_proxy_*.
// No label holds a client address or a destination.
func (f *Forwarder) Metrics() prometheus.Gatherer {
	f.setup()
	return f.m.reg
}

// outcome records what became of one request, once: at the moment it is
// decided, before the client is answered, so a tunnel counts when it opens
// rather than when it ends.
type outcome struct {
	m    *metrics
	kind string
	done bool
}

func (o *outcome) set(name string) {
	if o.done {
		return
	}
	o.done = true
	o.m.requests.WithLabelValues(o.kind, name).Inc()
}

// noteUpstream counts changes of the upstream file: a new address or
// credentials once when first used, a broken file once until it is fixed.
// The first read is not a change.
func (f *Forwarder) noteUpstream(up Upstream, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case err != nil:
		if f.upstreamState != upstreamFailed {
			f.m.reloads.WithLabelValues("failed").Inc()
		}
		f.upstreamState = upstreamFailed
	default:
		key := up.Addr + "\x00" + up.Auth
		if f.upstreamState == upstreamFailed || (f.upstreamState == upstreamOK && key != f.upstreamSeen) {
			f.m.reloads.WithLabelValues("ok").Inc()
		}
		f.upstreamState, f.upstreamSeen = upstreamOK, key
	}
}

const (
	upstreamUnread = iota
	upstreamOK
	upstreamFailed
)

// countingReader adds what passes through it to a counter as it is read, so
// a long transfer shows in the byte rate while it runs.
type countingReader struct {
	io.Reader
	c prometheus.Counter
}

func (r countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.c.Add(float64(n))
	}
	return n, err
}

// clientBody is a request body on its way to the upstream. What is read from
// the client is counted as it moves, and a failed read is kept: a request
// that fails because the client broke off its body is not the upstream's
// failure. The transport reads it on a goroutine of its own.
type clientBody struct {
	r io.ReadCloser
	c prometheus.Counter

	mu  sync.Mutex
	err error
}

func (b *clientBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.c.Add(float64(n))
	}
	// A read after the transport closed the body (it does on a failed
	// request) is not the client's doing.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, http.ErrBodyReadAfterClose) {
		b.mu.Lock()
		if b.err == nil {
			b.err = err
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *clientBody) Close() error { return b.r.Close() }

func (b *clientBody) failed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err != nil
}

// sendBody wraps a request body for the upstream (see clientBody) and returns
// the func that tells whether reading it from the client failed. A request
// without a body keeps http.NoBody, so it is still sent with no body rather
// than chunked.
func (f *Forwarder) sendBody(b io.ReadCloser) (io.ReadCloser, func() bool) {
	if b == nil || b == http.NoBody {
		return b, func() bool { return false }
	}
	cb := &clientBody{r: b, c: f.m.bytesUp}
	return cb, cb.failed
}

// failedOutcome tells why a request to the upstream failed: the client broke
// off its body, the forwarder is cutting everything on a stop, or else the
// upstream. The client's request context is not asked: a client that
// half-closes after its request (busybox wget) cancels it and still waits
// for the answer.
func (f *Forwarder) failedOutcome(clientFailed func() bool) string {
	switch {
	case clientFailed != nil && clientFailed():
		return outcomeClientGone
	case f.upstreamCtx.Err() != nil:
		return outcomeStopping
	}
	return outcomeUpstreamError
}
