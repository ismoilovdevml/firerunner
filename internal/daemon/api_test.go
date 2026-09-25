package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// failingWriter is a response writer whose client is gone.
type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) FlushError() error         { return errors.New("broken pipe") }

// A claim the executor never received (it gave up after its 3 s timeout, or
// the answer could not be written) puts the VM back at the head of the pool
// instead of leaving it claimed for nobody.
func TestClaimNotDeliveredGoesBack(t *testing.T) {
	cases := []struct {
		name      string
		gone      bool // the client's request context is done
		failWrite bool
		delivered bool
	}{
		{"delivered", false, false, true},
		{"client gave up", true, false, false},
		{"answer not written", false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, _ := newTestDaemon(t)
			fp := fingerprint(d.cfg)
			d.ready = []*pooled{pooledVM("a", fp, 0), pooledVM("b", fp, 0)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.gone {
				cancel()
			}
			req := httptest.NewRequest(http.MethodPost, "/claim?job=job-123", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			var w http.ResponseWriter = rec
			if c.failWrite {
				w = &failingWriter{header: http.Header{}}
			}
			d.apiHandler().ServeHTTP(w, req)

			d.mu.Lock()
			defer d.mu.Unlock()
			_, claimed := d.claimed["a"]
			if c.delivered {
				var got struct{ UID string }
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.UID != "a" || !claimed || len(d.ready) != 1 {
					t.Fatalf("delivered claim: body %q, claimed %v, ready %d", rec.Body, claimed, len(d.ready))
				}
				return
			}
			if claimed || len(d.ready) != 2 || d.ready[0].inst.UID != "a" {
				var ready []string
				for _, p := range d.ready {
					ready = append(ready, p.inst.UID)
				}
				t.Fatalf("undelivered claim: claimed %v, ready %v; want a back at the head", claimed, ready)
			}
		})
	}
}

// The claim log line names the job by its GitLab id, like the job events.
func TestClaimLogsTheBareJobID(t *testing.T) {
	d, _ := newTestDaemon(t)
	logs := captureLog(d)
	d.ready = []*pooled{pooledVM("a", fingerprint(d.cfg), 0)}
	req := httptest.NewRequest(http.MethodPost, "/claim?job=job-123", nil)
	d.apiHandler().ServeHTTP(httptest.NewRecorder(), req)
	if out := logs.String(); !strings.Contains(out, "vm=pool-a job=123") {
		t.Fatalf("claim log line: %s", out)
	}
}

// Over the real socket: the executor's client gives up while the claim waits
// for the daemon's lock; the VM it never got goes back to the pool.
func TestClaimAfterClientGaveUpOverTheSocket(t *testing.T) {
	d, _ := newTestDaemon(t)
	logs := captureLog(d)
	d.ready = []*pooled{pooledVM("a", fingerprint(d.cfg), 0)}
	c := serveAPI(t, d)
	c.http.Timeout = 200 * time.Millisecond
	d.mu.Lock() // a slow pass holds the lock
	if inst, err := c.Claim("job-1"); err == nil {
		d.mu.Unlock()
		t.Fatalf("Claim = %v, want the client's timeout", inst)
	}
	d.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "msg=\"pool VM claim") { // the handler got the lock and answered
		if time.Now().After(deadline) {
			t.Fatalf("claim handler never finished:\n%s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.mu.Lock()
	back := len(d.ready) == 1 && len(d.claimed) == 0
	d.mu.Unlock()
	if !back {
		t.Fatalf("the VM the client never received stayed claimed:\n%s", logs)
	}
	if inst, err := NewClient(d.cfg.Daemon.Socket).Claim("job-2"); err != nil || inst == nil || inst.UID != "a" {
		t.Fatalf("next Claim = %v, %v; want the VM back", inst, err)
	}
}
