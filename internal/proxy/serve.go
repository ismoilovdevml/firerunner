package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// cutGrace bounds the wait for handlers after a stop has cut their
// connections and cancelled their upstream calls. What can still hold one
// is a destination check's DNS lookup, which Policy.Check bounds to 3 s, or
// a read of the upstream file.
const cutGrace = 5 * time.Second

// Serve runs the forwarder on each address until ctx is done, then stops as
// ServeListeners does.
func Serve(ctx context.Context, f *Forwarder, addrs ...string) error {
	var ls []net.Listener
	for _, addr := range addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			for _, l := range ls {
				_ = l.Close()
			}
			return err
		}
		ls = append(ls, l)
	}
	return ServeListeners(ctx, f, ls...)
}

// ServeListeners runs the forwarder on each listener, which it closes, until
// ctx is done. It then refuses new connections at once, gives the requests
// and tunnels already open up to f.ShutdownWait to finish, cuts what is left
// and returns when every handler has ended (or cutGrace after the cut).
// When a listener fails, everything open is cut at once and the error is
// returned.
func ServeListeners(ctx context.Context, f *Forwarder, ls ...net.Listener) error {
	f.setup()
	var wg sync.WaitGroup
	defer wg.Wait()
	errs := make(chan error, len(ls))
	servers := make([]*http.Server, 0, len(ls))
	for _, l := range ls {
		s := &http.Server{Handler: f, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: f.Limits.ServerIdle}
		servers = append(servers, s)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Serve(l)
		}()
		f.Log.Info("proxy forwarder listening", "addr", l.Addr().String())
	}
	// Last to stop: a scrape during the stop still sees what is open.
	defer f.serveMetrics(&wg)()
	var err error
	wait := f.ShutdownWait
	select {
	case <-ctx.Done():
	case err = <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		wait = 0
	}
	f.stop(servers, wait)
	return err
}

// DefaultMetricsSocket is where firerunner-proxy serves its metrics and the
// daemon reads them: in the unit's RuntimeDirectory, readable by root only.
const DefaultMetricsSocket = "/run/firerunner-proxy/metrics.sock"

// serveMetrics serves the metrics on f.MetricsSocket, under wg, and returns
// the func that stops it. A socket it cannot make costs the metrics only:
// the forwarder runs on.
func (f *Forwarder) serveMetrics(wg *sync.WaitGroup) (stop func()) {
	if f.MetricsSocket == "" {
		return func() {}
	}
	l, err := listenSocket(f.MetricsSocket)
	if err != nil {
		f.Log.Warn("proxy metrics not exported: cannot listen on the metrics socket (firerunner-proxy.service needs RuntimeDirectory=firerunner-proxy: re-run install.sh)",
			"socket", f.MetricsSocket, "err", err)
		return func() {}
	}
	s := &http.Server{Handler: promhttp.HandlerFor(f.m.reg, promhttp.HandlerOpts{}), ReadHeaderTimeout: 5 * time.Second}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Serve(l)
	}()
	return func() { _ = s.Close() }
}

// listenSocket listens on a unix socket at path for root only. A socket a
// previous run left is replaced; any other file there is not touched.
func listenSocket(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The runtime directory is 0700 already; the socket is not left to the umask.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// stop shuts the servers down, waits up to wait for open handlers, then cuts.
func (f *Forwarder) stop(servers []*http.Server, wait time.Duration) {
	start := time.Now()
	f.Log.Info("proxy stopping: no new connections, open requests and tunnels may finish",
		"open", f.openHandlers(), "shutdown_timeout", wait.String())
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	// Shutdown closes the listeners and idle connections at once and waits
	// for plain requests; hijacked tunnels are only in f.handlers.
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Shutdown(ctx)
		}()
	}
	wg.Wait()
	defer f.transport.CloseIdleConnections()
	if f.waitHandlers(ctx) {
		// Nothing is open: the cut only marks the forwarder stopped, so a
		// request that still gets through is refused as stopping.
		f.cut()
		f.Log.Info("proxy stopped", "seconds", time.Since(start).Round(time.Millisecond).Seconds())
		return
	}
	f.Log.Warn("proxy stopping: cutting requests and tunnels still open after proxy.shutdown_timeout",
		"open", f.openHandlers(), "shutdown_timeout", wait.String())
	for _, s := range servers {
		_ = s.Close()
	}
	f.cut()
	gctx, gcancel := context.WithTimeout(context.Background(), cutGrace)
	defer gcancel()
	if !f.waitHandlers(gctx) {
		f.Log.Warn("proxy stopped with handlers still running", "open", f.openHandlers())
		return
	}
	f.Log.Info("proxy stopped", "seconds", time.Since(start).Round(time.Millisecond).Seconds())
}

// enter counts a handler in, unless the forwarder has cut everything.
func (f *Forwarder) enter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cutting {
		return false
	}
	f.handlers++
	return true
}

func (f *Forwarder) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers--
}

func (f *Forwarder) openHandlers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handlers
}

// waitHandlers reports whether every handler ended before ctx was done. It
// polls: handlers end once each, and a stop is rare enough that 25 ms of
// latency costs nothing, where a condition variable cannot honour ctx.
func (f *Forwarder) waitHandlers(ctx context.Context) bool {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		if f.openHandlers() == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return f.openHandlers() == 0
		case <-t.C:
		}
	}
}

// track registers closeFn to be called if the forwarder cuts what is open,
// and returns the func that unregisters it. After a cut it calls closeFn at
// once: a connection made in the meantime must not outlive the stop.
func (f *Forwarder) track(closeFn func()) (untrack func()) {
	f.mu.Lock()
	if f.cutting {
		f.mu.Unlock()
		closeFn()
		return func() {}
	}
	if f.open == nil {
		f.open = map[uint64]func(){}
	}
	f.openSeq++
	id := f.openSeq
	f.open[id] = closeFn
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.open, id)
	}
}

// cut closes every tracked connection and cancels the upstream calls.
func (f *Forwarder) cut() {
	f.mu.Lock()
	f.cutting = true
	open := f.open
	f.open = nil
	f.mu.Unlock()
	f.cancelAll()
	for _, closeFn := range open {
		closeFn()
	}
}
