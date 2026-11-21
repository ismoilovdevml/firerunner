// Package regfront is the front of a pull-through registry mirror: a reverse
// proxy between job microVMs and a Docker registry (distribution) that
// listens on loopback. It passes only what a mirror pull needs, GET and HEAD
// of /v2/, manifests and blobs, and answers everything else with a 404
// itself. Above all /v2/_catalog, which would list every repository any
// project pulled through the mirror: distribution cannot turn it off.
package regfront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"
)

// Limits bound what clients can hold and how long the front waits. Zero
// means no limit for every field.
type Limits struct {
	PerClient int // open connections from one client address
	Total     int // open connections from all clients together

	HeaderRead time.Duration // a client's request line and headers
	Idle       time.Duration // a keep-alive connection between requests, both sides
	Dial       time.Duration // connecting to the registry
	// UpstreamHeader is the registry's answer up to its headers. On a cache
	// miss the registry fetches from the remote registry before it answers.
	UpstreamHeader time.Duration
	// Stall is how long a response body may move no byte, from the registry
	// or to the client. A body that keeps moving has no bound: a large layer
	// on a slow link takes as long as it takes.
	Stall time.Duration
	// ShutdownWait is how long pulls under way may finish after a stop.
	ShutdownWait time.Duration
	// WarnEvery is how often a problem that repeats (a refused request or
	// connection from one client, a registry that does not answer) is logged:
	// the first time at once, then one line with the count per WarnEvery.
	WarnEvery time.Duration
}

// DefaultLimits suit one host running tens of jobs. PerClient and Total are
// those of the proxy forwarder (internal/proxy): connections here are only
// ever pulls, which BuildKit and parallel `docker pull`s run many of.
// UpstreamHeader and Stall are the forwarder's wait for an upstream answer,
// since a cache miss goes out the same corporate links. ShutdownWait stays
// under systemd's default stop timeout (90s), so a stop never ends in a kill.
// WarnEvery keeps a looping client to one line a minute per problem.
func DefaultLimits() Limits {
	return Limits{
		PerClient:      256,
		Total:          4096,
		HeaderRead:     30 * time.Second,
		Idle:           90 * time.Second,
		Dial:           10 * time.Second,
		UpstreamHeader: 5 * time.Minute,
		Stall:          5 * time.Minute,
		ShutdownWait:   60 * time.Second,
		WarnEvery:      time.Minute,
	}
}

// maxHeaderBytes bounds a request's headers. A pull's are a few hundred
// bytes, a bearer token a few KB; the server default (1 MB) times Total
// connections would be gigabytes held by clients that never finish one.
const maxHeaderBytes = 64 << 10

// idleConnsToRegistry keeps that many connections to the registry open
// between requests, so parallel layer pulls reuse them instead of opening
// one per request (the transport default keeps 2).
const idleConnsToRegistry = 64

// Front is the reverse proxy in front of one registry.
type Front struct {
	upstream  *url.URL
	limits    Limits
	log       *slog.Logger
	warns     *warnLimiter
	transport *http.Transport
	proxy     *httputil.ReverseProxy
}

// New returns a front for the registry at upstream, http://host:port.
func New(upstream string, limits Limits, log *slog.Logger) (*Front, error) {
	up, err := parseUpstream(upstream)
	if err != nil {
		return nil, err
	}
	f := &Front{upstream: up, limits: limits, log: log, warns: newWarnLimiter(log, limits.WarnEvery)}
	f.transport = &http.Transport{
		// Proxy is nil: the registry is local, never reached through
		// HTTP_PROXY from the environment.
		DialContext:           (&net.Dialer{Timeout: limits.Dial}).DialContext,
		ResponseHeaderTimeout: limits.UpstreamHeader,
		MaxIdleConnsPerHost:   idleConnsToRegistry,
		IdleConnTimeout:       limits.Idle,
		// Without it the transport asks for gzip and inflates the answer: the
		// client would get other bytes and another length than the registry
		// sent, and a blob that does not match its digest.
		DisableCompression: true,
	}
	f.proxy = &httputil.ReverseProxy{
		Rewrite:        f.rewrite,
		Transport:      f.transport,
		FlushInterval:  -1, // every read from the registry goes out at once
		ModifyResponse: f.watchBody,
		ErrorHandler:   f.registryFailed,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return f, nil
}

// parseUpstream reads http://host:port, with no path, credentials, query or
// fragment.
func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("upstream %q: %w", raw, err)
	}
	if u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Hostname() == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("upstream %q: expected http://host:port", raw)
	}
	if p, err := strconv.Atoi(u.Port()); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("upstream %q: expected http://host:port", raw)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// cancelKey carries a request's cancel func to watchBody, which ends the
// request when its body stalls.
type cancelKey struct{}

// ServeHTTP passes a pull to the registry and refuses anything else.
func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w = newStallWriter(w, f.limits.Stall)
	if why := refusal(r); why != "" {
		client := hostOf(r.RemoteAddr)
		f.warns.warn(why, client, "registry request refused", "client", client, "method", clip(r.Method),
			"target", clip(r.RequestURI), "reason", why)
		writeError(w, http.StatusNotFound, "UNSUPPORTED", "this registry mirror serves image pulls only")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	f.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, cancelKey{}, cancel)))
}

// rewrite sends the request to the registry with the path refusal checked,
// as written, without the query: the pull API takes none (containerd's ns=
// is ignored by distribution).
func (f *Front) rewrite(pr *httputil.ProxyRequest) {
	pr.Out.URL.Scheme = f.upstream.Scheme
	pr.Out.URL.Host = f.upstream.Host
	pr.Out.URL.Path = pr.In.URL.Path
	pr.Out.URL.RawPath = ""
	pr.Out.URL.RawQuery = ""
	pr.Out.URL.ForceQuery = false
	// The client's Host: an absolute URL the registry builds must lead the
	// client back here, not to loopback.
	pr.Out.Host = pr.In.Host
	// ReverseProxy already dropped the client's Forwarded and X-Forwarded-For,
	// -Host and -Proto (distribution builds URLs from them). X-Real-Ip, the
	// client address to proxies, is not the client's to set either.
	pr.Out.Header.Del("X-Real-Ip")
}

// watchBody ends a request whose body moves no byte for Limits.Stall.
func (f *Front) watchBody(resp *http.Response) error {
	cancel, ok := resp.Request.Context().Value(cancelKey{}).(context.CancelFunc)
	if ok && f.limits.Stall > 0 {
		resp.Body = newStallReader(resp.Body, f.limits.Stall, cancel)
	}
	return nil
}

// registryFailed answers a request the registry gave no answer to.
func (f *Front) registryFailed(w http.ResponseWriter, r *http.Request, err error) {
	// A client that hung up is not the registry's failure.
	if r.Context().Err() == nil {
		f.warns.warn("registry did not answer", "", "registry did not answer", "upstream", f.upstream.Host,
			"method", r.Method, "path", clip(r.URL.Path), "err", err)
	}
	writeError(w, http.StatusBadGateway, "UNAVAILABLE", "the registry behind this mirror did not answer")
}

// writeError answers in the registry's error format, which Docker reads and,
// from a mirror, takes as a reason to pull from the registry itself.
func writeError(w http.ResponseWriter, status int, code, message string) {
	type regError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	body, _ := json.Marshal(struct {
		Errors []regError `json:"errors"`
	}{[]regError{{code, message}}})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Serve runs the front on l until ctx is done, then stops taking
// connections and lets the requests under way finish, for at most
// Limits.ShutdownWait, before it cuts them. It returns nil after a stop, and
// the error of a listener that failed.
func (f *Front) Serve(ctx context.Context, l net.Listener) error {
	// Requests do not live off ctx: its end must not cut the pulls a
	// graceful stop lets finish. hardStop cuts them.
	base, hardStop := context.WithCancel(context.WithoutCancel(ctx))
	defer hardStop()
	srv := &http.Server{
		Handler: f,
		// No ReadTimeout or WriteTimeout: they bound the whole request, and a
		// blob takes as long as it keeps moving (Stall bounds a stall). An
		// expired ReadTimeout would also cancel a request mid-body.
		ReadHeaderTimeout: f.limits.HeaderRead,
		IdleTimeout:       f.limits.Idle,
		MaxHeaderBytes:    maxHeaderBytes,
		// OPTIONS * is refused like any other request, not answered by net/http.
		DisableGeneralOptionsHandler: true,
		BaseContext:                  func(net.Listener) context.Context { return base },
		ErrorLog:                     slog.NewLogLogger(f.log.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(newLimitListener(l, f.limits.PerClient, f.limits.Total, f.warns)) }()
	defer f.transport.CloseIdleConnections()
	// The repeats counted since the last line are logged before Serve returns.
	stopWarns, warnsDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(warnsDone); f.warns.run(stopWarns) }()
	defer func() { close(stopWarns); <-warnsDone; f.warns.flush(true) }()
	f.log.Info("registry front listening", "addr", l.Addr().String(), "upstream", f.upstream.String())

	select {
	case err := <-served:
		return fmt.Errorf("registry front on %s: %w", l.Addr(), err)
	case <-ctx.Done():
	}
	wait := context.Background()
	if f.limits.ShutdownWait > 0 {
		var cancel context.CancelFunc
		wait, cancel = context.WithTimeout(wait, f.limits.ShutdownWait)
		defer cancel()
	}
	if err := srv.Shutdown(wait); err != nil {
		f.log.Warn("pulls still under way at shutdown were cut", "waited", f.limits.ShutdownWait.String())
		hardStop()
		_ = srv.Close()
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("registry front on %s: %w", l.Addr(), err)
	}
	return nil
}

// hostOf is the address in host:port, or all of it when it has no port.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// clip shortens what a client sent for a log line: a request target may be
// as long as the header limit allows.
func clip(s string) string {
	const limit = 256
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
