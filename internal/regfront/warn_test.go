package regfront_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"
)

// logBuffer keeps the front's JSON log lines; safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) logger() *slog.Logger { return slog.New(slog.NewJSONHandler(b, nil)) }

// lines are the Warn lines with msg (and reason, when not empty).
func (b *logBuffer) lines(t *testing.T, msg, reason string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	data := append([]byte(nil), b.buf.Bytes()...)
	b.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var l map[string]any
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		if l["level"] == "WARN" && l["msg"] == msg && (reason == "" || l["reason"] == reason) {
			out = append(out, l)
		}
	}
	return out
}

// accounted is how many occurrences lines stand for: one per line, plus the
// repeats a summary line counts.
func accounted(lines []map[string]any) int {
	n := 0
	for _, l := range lines {
		if r, ok := l["repeats"].(float64); ok {
			n += int(r)
		} else {
			n++
		}
	}
	return n
}

// TestRefusalsAreRateLimited: a client repeating a refused request gets one
// line at once and then one line counting the repeats, per reason, not one
// line per request.
func TestRefusalsAreRateLimited(t *testing.T) {
	logs := &logBuffer{}
	lim := testLimits()
	lim.WarnEvery = time.Second
	addr, _ := startFrontLog(t, startUpstream(t, (&recorder{}).ServeHTTP), lim, logs.logger())
	const n = 30
	for range n {
		rawRequest(t, addr, "GET /v2/_catalog HTTP/1.1\r\nHost: mirror\r\n\r\n")
	}
	rawRequest(t, addr, "DELETE /v2/x/manifests/latest HTTP/1.1\r\nHost: mirror\r\n\r\n")

	if got := len(logs.lines(t, "registry request refused", "method")); got != 1 {
		t.Errorf("%d lines for the one refused DELETE, want 1", got)
	}
	eventually(t, "the repeats to be logged", func() bool {
		return accounted(logs.lines(t, "registry request refused", "not a pull")) == n
	})
	if got := len(logs.lines(t, "registry request refused", "not a pull")); got > 3 {
		t.Errorf("%d lines for %d refused requests, want the first and a count", got, n)
	}
}

// TestDroppedConnectionsAreRateLimited: a client reconnecting at its limit
// is logged once and then counted.
func TestDroppedConnectionsAreRateLimited(t *testing.T) {
	logs := &logBuffer{}
	upstream, started, release := heldRegistry(t)
	defer close(release)
	lim := testLimits()
	lim.PerClient = 1
	lim.WarnEvery = time.Second
	addr, _ := startFrontLog(t, upstream, lim, logs.logger())
	openPull(t, addr)
	waitStarted(t, started, 1)
	const n = 20
	for i := range n {
		if !refused(t, addr) {
			t.Fatalf("connection %d over the limit was served", i+2)
		}
	}
	const msg = "registry connection refused: too many connections"
	eventually(t, "the dropped connections to be logged", func() bool {
		return accounted(logs.lines(t, msg, "")) == n
	})
	if got := len(logs.lines(t, msg, "")); got > 3 {
		t.Errorf("%d lines for %d dropped connections, want the first and a count", got, n)
	}
}

// TestRepeatsLoggedAtStop: repeats not yet logged when the front stops are
// logged before Serve returns.
func TestRepeatsLoggedAtStop(t *testing.T) {
	logs := &logBuffer{}
	lim := testLimits()
	lim.WarnEvery = time.Hour
	addr, stop := startFrontLog(t, startUpstream(t, (&recorder{}).ServeHTTP), lim, logs.logger())
	const n = 5
	for range n {
		rawRequest(t, addr, "GET /v2/_catalog HTTP/1.1\r\nHost: mirror\r\n\r\n")
	}
	if got := len(logs.lines(t, "registry request refused", "")); got != 1 {
		t.Fatalf("%d lines before the stop, want 1", got)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	lines := logs.lines(t, "registry request refused", "")
	if len(lines) != 2 || accounted(lines) != n {
		t.Errorf("after the stop: %d lines for %d occurrences, want 2 for %d", len(lines), accounted(lines), n)
	}
}

// TestWarnEveryZeroLogsEach: WarnEvery 0 logs every refusal.
func TestWarnEveryZeroLogsEach(t *testing.T) {
	logs := &logBuffer{}
	lim := testLimits()
	lim.WarnEvery = 0
	addr, _ := startFrontLog(t, startUpstream(t, (&recorder{}).ServeHTTP), lim, logs.logger())
	for range 5 {
		resp, _ := rawRequest(t, addr, "GET /v2/_catalog HTTP/1.1\r\nHost: mirror\r\n\r\n")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if got := len(logs.lines(t, "registry request refused", "")); got != 5 {
		t.Errorf("%d lines, want 5", got)
	}
}
