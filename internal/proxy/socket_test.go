package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir is a temporary directory with a path short enough for a unix
// socket (t.TempDir can pass the 108-byte limit).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "frp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// The metrics are served on a root-only unix socket for the daemon, which
// goes away with the forwarder.
func TestMetricsServedOnSocket(t *testing.T) {
	up := startUpstream(t, "")
	f := forwarderTo(t, "http://"+up.addr, 0)
	sock := filepath.Join(shortDir(t), "metrics.sock")
	// A socket left by a previous run is replaced.
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	f.MetricsSocket = sock
	_, stop, done := stoppable(t, f)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	var body []byte
	eventually(t, "metrics on the socket", func() bool {
		resp, err := hc.Get("http://proxy/metrics")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		return resp.StatusCode == 200
	})
	if !strings.Contains(string(body), `firerunner_proxy_requests_total{kind="tunnel",outcome="allowed"} 0`) {
		t.Fatalf("metrics:\n%s", body)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v, want mode 0600", fi, err)
	}
	hc.CloseIdleConnections()
	stop()
	<-done
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left after stop: %v", err)
	}
}

// A socket that cannot be made (the unit has no runtime directory) costs the
// metrics, not the forwarding.
func TestServesWithoutMetricsSocket(t *testing.T) {
	up := startUpstream(t, "")
	f := forwarderTo(t, "http://"+up.addr, 0)
	f.MetricsSocket = filepath.Join(shortDir(t), "missing", "metrics.sock")
	addr, _, _ := stoppable(t, f)
	c, _, code := connect(t, addr, echoServer(t))
	c.Close()
	if code != 200 {
		t.Fatalf("CONNECT without a metrics socket: %d", code)
	}
}

// Something else at the socket's path is not removed to make room.
func TestMetricsSocketKeepsOtherFiles(t *testing.T) {
	up := startUpstream(t, "")
	f := forwarderTo(t, "http://"+up.addr, 0)
	path := filepath.Join(shortDir(t), "metrics.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.MetricsSocket = path
	addr, stop, done := stoppable(t, f)
	c, _, code := connect(t, addr, echoServer(t))
	c.Close()
	stop()
	<-done
	if b, err := os.ReadFile(path); code != 200 || err != nil || string(b) != "not a socket" {
		t.Fatalf("CONNECT %d; file now %q %v", code, b, err)
	}
}
