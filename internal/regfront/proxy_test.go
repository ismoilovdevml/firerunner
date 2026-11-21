package regfront_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

const blobPath = "/v2/library/alpine/blobs/sha256:" + hex64

// pattern is n deterministic bytes that are not all alike.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// TestBlobIsStreamed: the first bytes of a blob reach the client while the
// registry has not sent the rest, and a large blob arrives whole.
func TestBlobIsStreamed(t *testing.T) {
	first := pattern(1024)
	rest := pattern(32 << 20)
	sum := sha256.New()
	sum.Write(first)
	sum.Write(rest)
	want := sum.Sum(nil)

	gotFirst := make(chan struct{})
	addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(first)+len(rest)))
		_, _ = w.Write(first)
		http.NewResponseController(w).Flush()
		// The rest is held back until the client has the first part: a
		// front that buffers the body never gets there.
		select {
		case <-gotFirst:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(rest)
	}), testLimits())

	resp, err := client(t).Get("http://" + addr + blobPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	head := make([]byte, len(first))
	readDone := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, head); readDone <- err }()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first bytes did not arrive before the registry sent the rest: the body is buffered")
	}
	close(gotFirst)
	h := sha256.New()
	h.Write(head)
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		t.Fatalf("after %d bytes: %v", n, err)
	}
	if !bytes.Equal(h.Sum(nil), want) || n != int64(len(rest)) {
		t.Fatalf("blob differs: %d bytes after the first part, want %d", n, len(rest))
	}
}

// TestRange: a resumed download gets the part it asked for.
func TestRange(t *testing.T) {
	content := pattern(1000)
	addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}), testLimits())
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+blobPath, nil)
	req.Header.Set("Range", "bytes=100-199")
	resp, err := client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 100-199/1000" {
		t.Errorf("Content-Range %q", cr)
	}
	if !bytes.Equal(body, content[100:200]) {
		t.Errorf("got %d bytes, not bytes 100-199", len(body))
	}
}

// TestHead: HEAD answers carry the registry's headers (the size and digest a
// client checks) and no body.
func TestHead(t *testing.T) {
	addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("registry asked %s, want HEAD", r.Method)
		}
		w.Header().Set("Content-Length", "123456789")
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex64)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	}), testLimits())
	resp, err := client(t).Head("http://" + addr + "/v2/library/alpine/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 123456789 || len(body) != 0 {
		t.Errorf("status %d, Content-Length %d, %d body bytes; want 200, 123456789, 0", resp.StatusCode, resp.ContentLength, len(body))
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "sha256:"+hex64 {
		t.Errorf("Docker-Content-Digest %q", d)
	}
}

// TestRegistryAnswersPassThrough: redirects and registry errors reach the
// client as the registry sent them.
func TestRegistryAnswersPassThrough(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		body   string
	}{
		{"redirect to storage", http.StatusTemporaryRedirect, map[string]string{"Location": "https://storage.example/blob?sig=abc"}, ""},
		{"manifest unknown", http.StatusNotFound, map[string]string{"Content-Type": "application/json"},
			`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`},
		{"rate limited", http.StatusTooManyRequests, map[string]string{"Retry-After": "60"},
			`{"errors":[{"code":"TOOMANYREQUESTS","message":"slow down"}]}`},
		{"registry failure", http.StatusInternalServerError, nil, `{"errors":[{"code":"UNKNOWN","message":"boom"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), testLimits())
			resp, err := client(t).Get("http://" + addr + blobPath)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || string(body) != tc.body {
				t.Errorf("got %d %q, want %d %q", resp.StatusCode, body, tc.status, tc.body)
			}
			for k, v := range tc.header {
				if got := resp.Header.Get(k); got != v {
					t.Errorf("%s %q, want %q", k, got, v)
				}
			}
		})
	}
}

// TestRegistryDown: a registry that cannot be reached is a 502 with a
// registry error body, which makes Docker pull from the registry itself.
func TestRegistryDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	_ = l.Close()
	addr, _ := startFront(t, dead, testLimits())
	resp, body := rawRequest(t, addr, "GET /v2/ HTTP/1.1\r\nHost: mirror\r\n\r\n")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if code := errorCode(t, body); code != "UNAVAILABLE" {
		t.Errorf("error code %q, want UNAVAILABLE", code)
	}
}

// TestRegistryHeaderTimeout: a registry that never answers is given up after
// UpstreamHeader.
func TestRegistryHeaderTimeout(t *testing.T) {
	lim := testLimits()
	lim.UpstreamHeader = 200 * time.Millisecond
	addr, _ := startFront(t, startUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), lim)
	start := time.Now()
	resp, _ := rawRequest(t, addr, "GET /v2/library/alpine/manifests/latest HTTP/1.1\r\nHost: mirror\r\n\r\n")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("answered after %v, want about 200ms", d)
	}
}

// TestStalledRegistryBodyIsCut: a body the registry stops sending is ended
// after Stall, and the client sees it cut short, not complete.
func TestStalledRegistryBodyIsCut(t *testing.T) {
	lim := testLimits()
	lim.Stall = 200 * time.Millisecond
	gaveUp := make(chan struct{})
	addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(pattern(10))
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(gaveUp)
	}), lim)
	resp, err := client(t).Get("http://" + addr + blobPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil {
		t.Fatalf("the body ended cleanly after %d of 1000000 bytes", n)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the stalled body was cut after %v, want about %v", d, lim.Stall)
	}
	select {
	case <-gaveUp:
	case <-time.After(10 * time.Second):
		t.Fatal("the registry request was not cancelled")
	}
}

// TestSlowBodyIsNotCut: there is no bound on the whole transfer, only on a
// stall: a body that keeps moving for many times Stall arrives whole.
func TestSlowBodyIsNotCut(t *testing.T) {
	lim := testLimits()
	lim.Stall = 300 * time.Millisecond
	const chunks = 12
	addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(chunks*100))
		for range chunks {
			_, _ = w.Write(pattern(100))
			http.NewResponseController(w).Flush()
			select {
			case <-time.After(100 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}), lim)
	resp, err := client(t).Get("http://" + addr + blobPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n != chunks*100 {
		t.Fatalf("got %d bytes (%v), want %d", n, err, chunks*100)
	}
}

// TestPausingBodyIsNotCut: the stall bounds do not restart on every read
// and write, only now and then, yet a body that moves in bursts with pauses
// well over that step (a tenth of Stall at most) but under Stall arrives
// whole, whether the registry or the client pauses.
func TestPausingBodyIsNotCut(t *testing.T) {
	const (
		stall       = 400 * time.Millisecond
		pause       = 250 * time.Millisecond
		bursts      = 6
		clientBurst = 16 << 20
	)
	for _, c := range []struct {
		name string
		// registry sends size bytes; client reads them.
		registry func(w http.ResponseWriter, r *http.Request)
		client   func(body io.Reader) (int64, error)
		size     int
	}{
		{
			name: "registry pauses",
			size: bursts * 100,
			registry: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(bursts*100))
				for range bursts {
					_, _ = w.Write(pattern(100))
					http.NewResponseController(w).Flush()
					select {
					case <-time.After(pause):
					case <-r.Context().Done():
						return
					}
				}
			},
			client: func(body io.Reader) (int64, error) { return io.Copy(io.Discard, body) },
		},
		{
			// The registry sends at once: the socket buffers fill and the
			// front's write to the client waits out each pause of the client.
			// A burst is more than the socket buffers hold (Linux wakes a
			// writer only once half its send buffer is free): smaller ones
			// leave the write waiting through several pauses, a real stall.
			name: "client pauses",
			size: (bursts + 1) * clientBurst,
			registry: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa((bursts+1)*clientBurst))
				chunk := pattern(1 << 20)
				for range (bursts + 1) * clientBurst / len(chunk) {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			},
			client: func(body io.Reader) (int64, error) {
				var n int64
				for range bursts {
					m, err := io.CopyN(io.Discard, body, clientBurst)
					n += m
					if err != nil {
						return n, err
					}
					time.Sleep(pause)
				}
				m, err := io.Copy(io.Discard, body)
				return n + m, err
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			lim := testLimits()
			lim.Stall = stall
			addr, _ := startFront(t, startUpstream(t, c.registry), lim)
			resp, err := client(t).Get("http://" + addr + blobPath)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			n, err := c.client(resp.Body)
			if err != nil || n != int64(c.size) {
				t.Fatalf("got %d bytes (%v), want %d", n, err, c.size)
			}
		})
	}
}

// TestClientGoneCancelsRegistryRequest: a client that hangs up mid-blob
// frees the registry request at once.
func TestClientGoneCancelsRegistryRequest(t *testing.T) {
	cancelled := make(chan struct{})
	addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(pattern(10))
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(cancelled)
	}), testLimits())
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "GET "+blobPath+" HTTP/1.1\r\nHost: mirror\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the registry request was not cancelled after the client hung up")
	}
}

// TestStalledClientIsCut: a client that stops reading is dropped after
// Stall: the front closes its connection, which frees the client's place,
// and ends the registry request.
func TestStalledClientIsCut(t *testing.T) {
	lim := testLimits()
	lim.Stall = 200 * time.Millisecond
	lim.PerClient = 1
	ended := make(chan struct{}, 8)
	addr, _ := startFront(t, startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			select {
			case ended <- struct{}{}:
			default:
			}
		}()
		chunk := pattern(64 << 10)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}), lim)
	stalled, _ := openPull(t, addr)
	defer stalled.Close()
	// Nothing is read from stalled: the socket buffers fill and the front's
	// writes to it stall.
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the registry kept sending to a client that reads nothing")
	}
	// The client's only place is free again once the front has given up
	// writing to the stalled connection.
	eventually(t, "the stalled connection to be dropped", func() bool {
		c, br := openPull(t, addr)
		defer c.Close()
		resp, err := http.ReadResponse(br, nil)
		return err == nil && resp.StatusCode == http.StatusOK
	})
}

// TestRequestHeaders: the registry gets the client's pull headers, its own
// Host, no forwarding headers a client made up and no Accept-Encoding the
// front added (a transparently decompressed blob would not match its digest).
func TestRequestHeaders(t *testing.T) {
	rec := &recorder{}
	addr, _ := startFront(t, startUpstream(t, rec.ServeHTTP), testLimits())
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v2/library/alpine/manifests/latest", nil)
	accept := "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	req.Header.Set("Accept", accept)
	req.Header.Set("Forwarded", "for=1.2.3.4;host=evil")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Host", "evil")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Real-Ip", "1.2.3.4")
	resp, err := client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	got := rec.requests()
	if len(got) != 1 {
		t.Fatalf("registry asked %d times", len(got))
	}
	h := got[0].Header
	if h.Get("Accept") != accept {
		t.Errorf("Accept %q, want %q", h.Get("Accept"), accept)
	}
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Accept-Encoding"} {
		if v := h.Values(name); len(v) > 0 {
			t.Errorf("%s reached the registry: %q", name, v)
		}
	}
	if got[0].Host != addr {
		t.Errorf("Host %q, want the client's %q", got[0].Host, addr)
	}
}

// TestKeepAliveAfterLongPause: a connection reused after a pause longer
// than Stall still works. net/http keeps a write deadline across requests,
// and an answer without a body (HEAD) is sent with no write of the front's
// own that would set a fresh one.
func TestKeepAliveAfterLongPause(t *testing.T) {
	lim := testLimits()
	lim.Stall = 100 * time.Millisecond
	rec := &recorder{}
	addr, _ := startFront(t, startUpstream(t, rec.ServeHTTP), lim)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	cases := []struct {
		method, target string
		status         int
	}{
		{"GET", "/v2/", http.StatusOK},
		{"GET", "/v2/_catalog", http.StatusNotFound},
		{"HEAD", "/v2/", http.StatusOK},
		{"HEAD", "/v2/library/alpine/manifests/latest", http.StatusOK},
	}
	for i, tc := range cases {
		if i > 0 {
			time.Sleep(3 * lim.Stall)
		}
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, tc.method+" "+tc.target+" HTTP/1.1\r\nHost: mirror\r\n\r\n"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp, err := http.ReadResponse(br, &http.Request{Method: tc.method})
		if err != nil {
			t.Fatalf("request %d (%s %s): %v", i, tc.method, tc.target, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("request %d (%s %s): status %d, want %d", i, tc.method, tc.target, resp.StatusCode, tc.status)
		}
	}
}
